// Package mcp serves the project's RAG store to MCP clients (ADR-030): one
// tool, search_<store>, behind POST /mcp.
//
// The server is stateless and speaks both protocol eras on the one endpoint.
// The era is worked out from every request on its own (see resolveEra), so
// replicas share nothing and no session is ever minted:
//
//   - modern (2026-07-28): server/discover, tools/list and tools/call, with
//     the Mcp-Method and Mcp-Name headers checked against the body;
//   - legacy (2025-11-25, 2025-06-18, 2025-03-26): initialize, ping,
//     tools/list and tools/call, answered without an Mcp-Session-Id.
//
// The protocol is written by hand. The official SDK is a test-only
// dependency here: it is the client the conformance test connects with.
// Authentication, limits and the request log are the gateway's own (the
// gateway.MCP* entry points); this package adds no copy of them.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ragmux/ragmux/internal/gateway"
)

// Protocol versions the server answers (ADR-030).
const (
	versionModern = "2026-07-28"
	versionLatest = "2025-11-25" // newest legacy version, offered to initialize

	// metaVersionKey is where a modern request carries its protocol version.
	metaVersionKey = "io.modelcontextprotocol/protocolVersion"
	// metaServerInfoKey is where a modern server/discover result names the server.
	metaServerInfoKey = "io.modelcontextprotocol/serverInfo"

	instructions = "Search the project's knowledge base with the search tool."
)

// supportedVersions is the list -32022 answers with, newest first.
var supportedVersions = []string{versionModern, "2025-11-25", "2025-06-18", "2025-03-26"}

func isLegacyVersion(v string) bool {
	switch v {
	case "2025-11-25", "2025-06-18", "2025-03-26":
		return true
	}
	return false
}

// JSON-RPC error codes. The -3200x ones are MCP's own (-32020 header
// mismatch, -32022 unsupported version) or, for -32001, Ragmux's: the
// application-specific code of a refused request.
const (
	codeParseError      = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeInternalError   = -32603
	codeRefused         = -32001
	codeHeaderMismatch  = -32020
	codeUnsupportedVers = -32022
)

const (
	// maxToolNameLen bounds a tool name, "search_" included, so the MCP
	// client's own "mcp__<server>__" prefix still fits a 64 character name.
	maxToolNameLen = 48
	toolPrefix     = "search_"
	maxTopK        = 50

	defaultMaxBodyBytes = 1 << 20
)

// Backend is what the server needs of the gateway. *gateway.Gateway
// implements it.
type Backend interface {
	// MCPAuth authenticates every request: key, retrieve scope, project.
	MCPAuth(refuse gateway.MCPRefuseFunc) func(http.Handler) http.Handler
	// MCPStoreFor is the RAG store of the request's project, nil if it has none.
	MCPStoreFor(ctx context.Context) (*gateway.MCPStore, error)
	// MCPSearch is one tools/call, with the gateway's limits and accounting.
	MCPSearch(w http.ResponseWriter, r *http.Request, args gateway.MCPSearchArgs) (*gateway.MCPSearchResult, *gateway.MCPFailure)
}

// Config configures a Server.
type Config struct {
	Backend Backend
	Log     *slog.Logger
	// Version is the Ragmux version reported as the server's.
	Version string
	// AllowedOrigins is CORS_ORIGINS. A request that carries an Origin not in
	// it is refused with 403; "*" lets every Origin through; an empty list
	// refuses every Origin. A request without one always passes.
	AllowedOrigins []string
	// MaxBodyBytes bounds a request body; zero means 1 MiB.
	MaxBodyBytes int64
}

// Server is the /mcp handler.
type Server struct {
	cfg  Config
	log  *slog.Logger
	h    http.Handler
	orig map[string]bool
	all  bool
}

// New builds the handler for POST /mcp.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, log: cfg.Log, orig: map[string]bool{}}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.cfg.MaxBodyBytes <= 0 {
		s.cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			s.all = true
		}
		s.orig[o] = true
	}
	s.h = cfg.Backend.MCPAuth(refuse)(http.HandlerFunc(s.serve))
	return s
}

// ServeHTTP checks the Origin, then the method, then authenticates, then
// speaks JSON-RPC. The cheap refusals come first; nothing reaches the
// database before the credential is known.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" && !s.all && !s.orig[o] {
		writeRPCError(w, http.StatusForbidden, nil, codeRefused, "origin not allowed",
			map[string]any{"code": "origin_not_allowed"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeRPCError(w, http.StatusMethodNotAllowed, nil, codeInvalidRequest, "this endpoint only accepts POST", nil)
		return
	}
	s.h.ServeHTTP(w, r)
}

// refuse renders a refusal the gateway wrote (ADR-030): an id-less JSON-RPC
// error whose data.code is the Ragmux code. Server-side failures keep the
// JSON-RPC internal-error code.
func refuse(w http.ResponseWriter, status int, code, message string) {
	rpc := codeRefused
	if status >= http.StatusInternalServerError {
		rpc = codeInternalError
	}
	writeRPCError(w, status, nil, rpc, message, map[string]any{"code": code})
}

type era int

const (
	eraLegacy era = iota
	eraModern
)

// rpcRequest is a JSON-RPC message from the client. A message without an id
// is a notification; one without a method is the client's answer to a request
// the server never makes.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > s.cfg.MaxBodyBytes {
		writeRPCError(w, http.StatusRequestEntityTooLarge, nil, codeInvalidRequest, "request body too large", nil)
		return
	}
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, "batch requests are not supported", nil)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, codeParseError, "parse error", nil)
		return
	}
	if req.Method == "" {
		if req.Result != nil || req.Error != nil {
			w.WriteHeader(http.StatusAccepted) // the client's answer to nothing we asked
			return
		}
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, "method is required", nil)
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, `jsonrpc must be "2.0"`, nil)
		return
	}
	if req.ID == nil { // a notification: notifications/initialized and the like
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !validID(req.ID) {
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, "id must be a string or a number", nil)
		return
	}

	e, perr := resolveEra(r.Header, &req)
	if perr != nil {
		perr.write(w, req.ID)
		return
	}
	if e == eraModern {
		if perr := checkModernHeaders(r.Header, &req); perr != nil {
			perr.write(w, req.ID)
			return
		}
	}
	s.dispatch(w, r, e, &req)
}

func validID(id json.RawMessage) bool {
	switch id[0] {
	case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

// protoError is a request the protocol layer refuses: the HTTP status, the
// JSON-RPC error and its data.
type protoError struct {
	status  int
	code    int
	message string
	data    any
}

func (p *protoError) write(w http.ResponseWriter, id json.RawMessage) {
	writeRPCError(w, p.status, id, p.code, p.message, p.data)
}

func unsupportedVersion(requested string) *protoError {
	return &protoError{http.StatusBadRequest, codeUnsupportedVers, "unsupported protocol version",
		map[string]any{"requested": requested, "supported": supportedVersions}}
}

// resolveEra settles which protocol era one request speaks (ADR-030). Nothing
// is remembered between requests, so every request is read on its own:
//
//  1. initialize is legacy, whatever else it carries;
//  2. a request whose params._meta names a protocol version is modern; the
//     MCP-Protocol-Version header has to name the same one;
//  3. otherwise the header decides: a legacy version is legacy, a modern one
//     without _meta is a header mismatch, anything else is unsupported;
//  4. no header at all is 2025-03-26, which clients before 2025-06-18 send.
func resolveEra(h http.Header, req *rpcRequest) (era, *protoError) {
	header := strings.TrimSpace(h.Get("MCP-Protocol-Version"))
	if req.Method != "initialize" {
		if v := metaVersion(req.Params); v != "" {
			if v != versionModern {
				return 0, unsupportedVersion(v)
			}
			if header != v {
				return 0, &protoError{http.StatusBadRequest, codeHeaderMismatch,
					"MCP-Protocol-Version header must match params._meta[\"" + metaVersionKey + "\"]",
					map[string]any{"header": "MCP-Protocol-Version", "expected": v, "received": header}}
			}
			return eraModern, nil
		}
	}
	switch {
	case header == "":
		return eraLegacy, nil
	case isLegacyVersion(header):
		return eraLegacy, nil
	case header == versionModern:
		return 0, &protoError{http.StatusBadRequest, codeHeaderMismatch,
			"a " + versionModern + " request carries params._meta[\"" + metaVersionKey + "\"]",
			map[string]any{"header": "MCP-Protocol-Version", "received": header}}
	}
	return 0, unsupportedVersion(header)
}

// metaVersion is the protocol version a modern request names in its params.
func metaVersion(params json.RawMessage) string {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return ""
	}
	var v string
	if raw, ok := p.Meta[metaVersionKey]; ok && json.Unmarshal(raw, &v) == nil {
		return v
	}
	return ""
}

// checkModernHeaders is the header-against-body validation of a modern
// request: Mcp-Method names the method, and for tools/call Mcp-Name names the
// tool. A missing header is as wrong as a different one.
func checkModernHeaders(h http.Header, req *rpcRequest) *protoError {
	check := func(name, want string, mayBeEncoded bool) *protoError {
		got := h.Get(name)
		// A tool name that is not plain ASCII is sent as =?base64?<data>?= and
		// has to be decoded before it is compared with the body. One that does
		// not decode matches nothing. The method name is never encoded.
		if dec, encoded, ok := decodeHeaderValue(got); mayBeEncoded && encoded {
			if !ok {
				return &protoError{http.StatusBadRequest, codeHeaderMismatch,
					name + " header is not valid base64", map[string]any{"header": name}}
			}
			got = dec
		}
		if got == want {
			return nil
		}
		return &protoError{http.StatusBadRequest, codeHeaderMismatch,
			name + " header does not match the request body",
			map[string]any{"header": name, "expected": want, "received": got}}
	}
	if perr := check("Mcp-Method", req.Method, false); perr != nil {
		return perr
	}
	if req.Method == "tools/call" {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return check("Mcp-Name", p.Name, true)
	}
	return nil
}

// decodeHeaderValue decodes the =?base64?<data>?= form a header value takes
// when it is not plain ASCII. encoded reports that the value had that form;
// ok that its data decoded.
func decodeHeaderValue(v string) (decoded string, encoded, ok bool) {
	const pre, suf = "=?base64?", "?="
	if len(v) < len(pre)+len(suf) || !strings.HasPrefix(v, pre) || !strings.HasSuffix(v, suf) {
		return "", false, false
	}
	b, err := base64.StdEncoding.DecodeString(v[len(pre) : len(v)-len(suf)])
	if err != nil || !utf8.Valid(b) {
		return "", true, false
	}
	return string(b), true, true
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, e era, req *rpcRequest) {
	switch {
	case e == eraModern && req.Method == "server/discover":
		// The server's identity rides in _meta in this revision, not in a
		// serverInfo member as initialize has it.
		writeResult(w, e, req.ID, map[string]any{
			"supportedVersions": supportedVersions,
			"capabilities":      capabilities(),
			"instructions":      instructions,
			"_meta":             map[string]any{metaServerInfoKey: s.serverInfo()},
		})
	case e == eraLegacy && req.Method == "initialize":
		writeResult(w, e, req.ID, s.initialize(req.Params))
	case e == eraLegacy && req.Method == "ping":
		writeResult(w, e, req.ID, map[string]any{})
	case req.Method == "tools/list":
		s.toolsList(w, r, e, req)
	case req.Method == "tools/call":
		s.toolsCall(w, r, e, req)
	default:
		// A modern client is told with the HTTP status too; a legacy one
		// reads the JSON-RPC error out of a 200, as its transport expects.
		status := http.StatusOK
		if e == eraModern {
			status = http.StatusNotFound
		}
		writeRPCError(w, status, req.ID, codeMethodNotFound, "method not found: "+clip(req.Method), nil)
	}
}

func capabilities() map[string]any {
	return map[string]any{"tools": map[string]any{"listChanged": false}}
}

func (s *Server) serverInfo() map[string]any {
	v := s.cfg.Version
	if v == "" {
		v = "dev"
	}
	return map[string]any{"name": "ragmux", "version": v}
}

// initialize answers the legacy handshake. It mints no session: nothing is
// kept, so there is nothing for an Mcp-Session-Id to name.
func (s *Server) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	v := versionLatest
	if isLegacyVersion(p.ProtocolVersion) {
		v = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": v,
		"capabilities":    capabilities(),
		"serverInfo":      s.serverInfo(),
		"instructions":    instructions,
	}
}

// tool is the project's search tool as tools/list describes it.
func tool(st *gateway.MCPStore) map[string]any {
	name := cleanText(st.Name, 100)
	return map[string]any{
		"name": ToolName(st.Name, st.ID),
		"description": fmt.Sprintf("Search the %q knowledge base (%d documents). Returns the most relevant passages "+
			"with their source file, section and relevance score.", name, st.DocumentCount),
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "minLength": 1,
					"description": "What to look for, in natural language."},
				"top_k": map[string]any{"type": "integer", "minimum": 1, "maximum": maxTopK,
					"description": "How many passages to return. Omit to use the store's own setting."},
				"filter": map[string]any{"type": "object", "additionalProperties": true,
					"description": `Only search documents whose metadata matches. A key matches by equality ` +
						`({"lang": "en"}); {"in": [...]} matches any of several values ({"team": {"in": ["a", "b"]}}).`},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
		"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true,
			"openWorldHint": false},
	}
}

func (s *Server) toolsList(w http.ResponseWriter, r *http.Request, e era, req *rpcRequest) {
	st, err := s.cfg.Backend.MCPStoreFor(r.Context())
	if err != nil {
		s.log.Error("mcp: load store", "err", err)
		writeRPCError(w, http.StatusInternalServerError, req.ID, codeInternalError, "store lookup failed", nil)
		return
	}
	tools := []map[string]any{} // a project without a store lists nothing, which is not an error
	if st != nil {
		tools = append(tools, tool(st))
	}
	writeResult(w, e, req.ID, map[string]any{"tools": tools})
}

// toolArgs are the arguments of tools/call for the search tool.
type toolArgs struct {
	Query  string          `json:"query"`
	TopK   *int            `json:"top_k"`
	Filter json.RawMessage `json:"filter"`
}

func (s *Server) toolsCall(w http.ResponseWriter, r *http.Request, e era, req *rpcRequest) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "params must name the tool to call", nil)
		return
	}
	st, err := s.cfg.Backend.MCPStoreFor(r.Context())
	if err != nil {
		s.log.Error("mcp: load store", "err", err)
		writeRPCError(w, http.StatusInternalServerError, req.ID, codeInternalError, "store lookup failed", nil)
		return
	}
	if st == nil || p.Name != ToolName(st.Name, st.ID) {
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "unknown tool: "+clip(p.Name), nil)
		return
	}

	// A wrong argument is the tool's answer, not a protocol error: the model
	// reads it and corrects the next call.
	var a toolArgs
	if len(p.Arguments) > 0 && !bytes.Equal(bytes.TrimSpace(p.Arguments), []byte("null")) {
		// The schema closes the argument set, so a name outside it is told back:
		// the model learns the argument it invented does nothing.
		dec := json.NewDecoder(bytes.NewReader(p.Arguments))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			writeToolError(w, e, req.ID, "invalid arguments: "+err.Error())
			return
		}
	}
	args := gateway.MCPSearchArgs{Query: a.Query}
	if a.TopK != nil {
		if *a.TopK < 1 || *a.TopK > maxTopK {
			writeToolError(w, e, req.ID, "top_k must be between 1 and 50")
			return
		}
		args.TopK = *a.TopK
	}
	// Models often send null for an optional argument; that is no filter.
	if f := bytes.TrimSpace(a.Filter); len(f) > 0 && !bytes.Equal(f, []byte("null")) {
		args.Filter = a.Filter
	}

	res, fail := s.cfg.Backend.MCPSearch(w, r, args)
	switch {
	case fail != nil && fail.Tool:
		// The answer is a 200 for the model to read; a retry hint the failed
		// upstream call left on the headers is not about this response.
		w.Header().Del("Retry-After")
		writeToolError(w, e, req.ID, fail.Message)
	case fail != nil:
		// A refusal: limits, project, server configuration. The status
		// stands, with the headers the gateway already set (Retry-After).
		rpc := codeRefused
		if fail.Status >= http.StatusInternalServerError {
			rpc = codeInternalError
		}
		writeRPCError(w, fail.Status, req.ID, rpc, fail.Message, map[string]any{"code": fail.Code})
	default:
		text, err := json.Marshal(res)
		if err != nil {
			s.log.Error("mcp: encode hits", "err", err)
			writeRPCError(w, http.StatusInternalServerError, req.ID, codeInternalError, "encoding the result failed", nil)
			return
		}
		writeResult(w, e, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}})
	}
}

// ToolName is the search tool's name for a store: "search_" and the store
// name as a slug, at most 48 characters. A name with nothing left of it
// after slugging -- one of only non-ASCII characters, say -- falls back to
// search_store_<id>.
func ToolName(storeName string, storeID int64) string {
	var b strings.Builder
	under := true // the slug does not start with an underscore, and runs of them are one
	for _, r := range storeName {
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
			fallthrough
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			under = false
		default: // '_' and every other character, ASCII or not
			if !under {
				b.WriteByte('_')
				under = true
			}
		}
	}
	slug := strings.TrimRight(b.String(), "_")
	if len(slug) > maxToolNameLen-len(toolPrefix) {
		slug = strings.TrimRight(slug[:maxToolNameLen-len(toolPrefix)], "_")
	}
	if slug == "" {
		return toolPrefix + "store_" + strconv.FormatInt(storeID, 10)
	}
	return toolPrefix + slug
}

// cleanText makes an operator-chosen name safe to put into a description:
// control characters become spaces and it is cut at max characters.
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return strings.TrimSpace(s)
}

// clip shortens client-chosen text before it is echoed back in an error.
func clip(s string) string { return cleanText(s, 64) }

// --- responses ---

// writeResult writes a successful result. Every result of the modern era
// states its resultType (this server never asks the client for more input, so
// it is always "complete"); a legacy result is left as that era shapes it.
func writeResult(w http.ResponseWriter, e era, id json.RawMessage, result map[string]any) {
	if e == eraModern {
		result["resultType"] = "complete"
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// writeToolError is a failed tool call: a normal result the model can read.
func writeToolError(w http.ResponseWriter, e era, id json.RawMessage, message string) {
	writeResult(w, e, id, map[string]any{"isError": true,
		"content": []map[string]any{{"type": "text", "text": message}}})
}

// writeRPCError writes a JSON-RPC error. A nil id leaves "id" out: the
// refusals of ADR-030 are answered before the request is read, so they have
// none to name.
func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	e := map[string]any{"code": code, "message": message}
	if data != nil {
		e["data"] = data
	}
	body := map[string]any{"jsonrpc": "2.0", "error": e}
	if id != nil {
		body["id"] = id
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is out; a failed write leaves nothing to tell the client.
	_ = json.NewEncoder(w).Encode(v)
}
