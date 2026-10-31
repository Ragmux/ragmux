package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// POST /v1/messages (ADR-028): the Anthropic Messages API, served by the same
// pipeline as chat completions. A request is read once; each attempt then takes
// the path its target allows. An Anthropic connection gets the client's body
// with the few changes ADR-028 lists and answers in its own bytes (native). Any
// other connection gets the request translated to the canonical form and its
// answer translated back.

// messagesOperation is POST /v1/messages. It is chat as far as scopes, limits
// and metrics go; the endpoint column tells the rows apart.
var messagesOperation = operation{
	name:  "chat",
	span:  "gateway.messages",
	scope: store.ScopeChat,
	what:  "chat completions",

	endpoint: "messages",
	// Claude Code names its session in x-claude-code-session-id (ADR-032).
	claudeSession: true,
}

// isMessagesPath reports a request for the Messages endpoint or anything under
// it. The router sees the full path, whatever prefix it is mounted at.
func isMessagesPath(path string) bool {
	return strings.Contains(path+"/", "/v1/messages/")
}

// anthropicEnvelope is the router middleware that gives the Messages endpoint
// its error shape. The pipeline writes errors in OpenAI's envelope from some
// forty places (ADR-028 rejected changing each of them); this wrapper holds
// back an error response on the endpoint's paths and rewrites it into
// Anthropic's: {"type":"error","error":{type,message,code},"request_id"}. It
// also sets the request-id header the Anthropic SDKs read an error's id from.
func (g *Gateway) anthropicEnvelope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMessagesPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		id := requestIDFrom(r.Context())
		if id != "" {
			w.Header().Set("request-id", id)
		}
		ew := &envelopeWriter{ResponseWriter: w, requestID: id}
		next.ServeHTTP(ew, r)
		ew.finish()
	})
}

// envelopeWriter holds back a JSON error response until the handler is done
// and writes it again in Anthropic's envelope. Every other response, an error
// that is not JSON included, passes through as it is.
type envelopeWriter struct {
	http.ResponseWriter
	requestID string
	wrote     bool
	holding   bool
	// raw says the handler is relaying the upstream's own error body, which
	// must reach the client unchanged (keepErrorBody).
	raw    bool
	status int
	buf    bytes.Buffer
}

// keepErrorBody tells the envelope writer, if w is one, that the error about
// to be written is already in the client's format.
func keepErrorBody(w http.ResponseWriter) {
	if ew, ok := w.(*envelopeWriter); ok {
		ew.raw = true
	}
}

func (w *envelopeWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	ct := w.Header().Get("Content-Type")
	if code >= http.StatusBadRequest && !w.raw && strings.HasPrefix(ct, "application/json") {
		w.holding, w.status = true, code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *envelopeWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.holding {
		return w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *envelopeWriter) Flush() {
	if w.holding {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection.
func (w *envelopeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish writes the held error in the Anthropic envelope.
func (w *envelopeWriter) finish() {
	if !w.holding {
		return
	}
	var in struct {
		Error struct {
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	msg, code := "request failed", ""
	if json.Unmarshal(w.buf.Bytes(), &in) == nil && in.Error.Message != "" {
		msg = in.Error.Message
		// Only a code the 401 body already carries: an unknown key has none
		// (PRD rule 4).
		_ = json.Unmarshal(in.Error.Code, &code)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(provider.MessagesError(w.status, code, msg, w.requestID))
}

// messagesRead is a Messages request read and ready to route.
type messagesRead struct {
	body *provider.MessagesBody
	// req is the canonical request of the translated path.
	req  provider.ChatRequest
	opts ragmuxRequest
	// warnings are the texts for what the translation dropped.
	warnings []string
}

// readMessages decodes the request, applies the project's tool policy to it
// and translates it, in this order: the policy must see the client's tools
// before the translation drops any, and the tool turn is read from the
// original message. It has written the answer when ok is false.
func (g *Gateway) readMessages(c *call) (messagesRead, bool) {
	body, model, opts, ok := g.decodeMessages(c)
	if !ok {
		return messagesRead{}, false
	}
	c.requestedModel = model
	if !g.applyMessagesToolPolicy(c, body, &opts) {
		return messagesRead{}, false
	}
	tr, err := provider.MessagesToChat(body)
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) {
			writeError(c.w, http.StatusBadRequest, "invalid_request_error", pe.Message)
		} else {
			writeError(c.w, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return messagesRead{}, false
	}
	opts.ToolTurn = tr.ToolTurn
	return messagesRead{body: body, req: tr.Request, opts: opts, warnings: tr.Warnings}, true
}

// messages is POST /v1/messages.
func (g *Gateway) messages(w http.ResponseWriter, r *http.Request) {
	c, ok := g.authenticate(w, r, messagesOperation)
	if !ok {
		return
	}
	defer c.span.End()

	m, ok := g.readMessages(c)
	if !ok {
		return
	}
	body, req, opts := m.body, m.req, m.opts
	if !g.resolveRoute(c, req.Model) {
		return
	}
	prov, ok, buildErr := g.chatProvider(c)
	if !ok {
		return
	}
	if req.Model == "" {
		req.Model = c.conn.ModelName
	}

	estimate := (len(c.project.SystemPrompt) + messageChars(req.Messages) + 3) / 4
	if !g.checkLimits(c, estimate, req.Stream) {
		return
	}

	prep := g.prepareContext(c, prov, &req, opts)
	g.openRecord(c, req.Stream, prep.apply)
	defer g.record(c)
	if !g.checkContextLimits(c, estimate, prep.contextBlock, prep.contextLanguage) {
		return
	}

	inv := g.newMessagesInvocation(c, r, body, req, m.warnings, prep, prov, buildErr)
	g.invoke(c, inv)
}

// newMessagesInvocation gathers what every attempt of the request needs: the
// canonical request for the translated path, and for the native one the
// client's body with what the project adds to it. prov is the first target's
// adapter, or nil with buildErr when it could not be built.
func (g *Gateway) newMessagesInvocation(c *call, r *http.Request, body *provider.MessagesBody, req provider.ChatRequest,
	translated []string, prep chatPrep, prov provider.Provider, buildErr error) *messagesInvocation {
	inv := &messagesInvocation{gw: g, req: req, body: body, translated: translated,
		version: r.Header.Get("anthropic-version"), beta: strings.Join(r.Header.Values("anthropic-beta"), ", "),
		promptChars: messageChars(req.Messages), opts: prep.opts, sources: prep.sources, contextBlock: prep.contextBlock}
	if c.project.SystemPrompt != "" {
		inv.systemAppend = append(inv.systemAppend, c.project.SystemPrompt)
	}
	if prep.contextBlock != "" {
		if c.project.ContextPlacement == store.ContextPlacementBeforeLastUser {
			inv.beforeLastUser = prep.contextBlock
		} else {
			inv.systemAppend = append(inv.systemAppend, prep.contextBlock)
		}
	}
	if prov != nil {
		inv.prov, inv.provConn = prov, c.conn
	} else {
		inv.failedConn, inv.failedErr = c.conn, buildErr
	}
	return inv
}

// decodeMessages reads the request body into its fields and the ragmux
// extension out of it, answering the client itself when it cannot.
func (g *Gateway) decodeMessages(c *call) (*provider.MessagesBody, string, ragmuxRequest, bool) {
	var opts ragmuxRequest
	raw, ok := g.readBody(c)
	if !ok {
		return nil, "", opts, false
	}
	body, err := provider.ParseMessagesBody(raw)
	if err != nil {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return nil, "", opts, false
	}
	var model string
	if v, ok := body.Get("model"); ok {
		if err := json.Unmarshal(v, &model); err != nil && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			writeError(c.w, http.StatusBadRequest, "invalid_request_error", "model must be a string")
			return nil, "", opts, false
		}
	}
	var msgs []json.RawMessage
	if v, ok := body.Get("messages"); !ok || json.Unmarshal(v, &msgs) != nil || len(msgs) == 0 {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return nil, "", opts, false
	}
	// The ragmux request object is ours and never reaches an upstream, which
	// Anthropic would refuse for an unknown field. Same grammar as chat.
	if v, ok := body.Get("ragmux"); ok {
		opts, err = parseRagmux(map[string]json.RawMessage{"ragmux": v})
		if err != nil {
			writeErrorCode(c.w, http.StatusBadRequest, "invalid_request_error", errInvalidRagmux, err.Error())
			return nil, "", opts, false
		}
		body.Delete("ragmux")
	}
	return body, model, opts, true
}

// applyMessagesToolPolicy runs the project's tool lists over the request's
// tools and tool_choice, which governTools reads in the Messages shape as well
// (ADR-032), and writes the outcome back into the body. The native path sends
// that body as it stands and the canonical path translates it, so both see the
// same tools.
func (g *Gateway) applyMessagesToolPolicy(c *call, body *provider.MessagesBody, opts *ragmuxRequest) bool {
	tools, hasTools := body.Get("tools")
	choice, hasChoice := body.Get("tool_choice")
	shadow := &provider.ChatRequest{Extra: map[string]json.RawMessage{}}
	if hasTools {
		shadow.Tools = tools
	}
	if hasChoice {
		shadow.ToolChoice = choice
	}
	if !g.applyToolPolicy(c, shadow, opts) {
		return false
	}
	if hasTools && !bytes.Equal(shadow.Tools, tools) {
		if len(shadow.Tools) == 0 {
			body.Delete("tools")
		} else {
			body.Set("tools", shadow.Tools)
		}
	}
	if hasChoice && !bytes.Equal(shadow.ToolChoice, choice) {
		if len(shadow.ToolChoice) == 0 {
			body.Delete("tool_choice")
		} else {
			body.Set("tool_choice", shadow.ToolChoice)
		}
	}
	return true
}

// messagesInvocation is the Messages endpoint's upstreamCall.
type messagesInvocation struct {
	gw *Gateway
	// prov, provConn, failedConn and failedErr are chatInvocation's: the
	// adapter of the target tried last, and the target whose adapter could not
	// be built.
	prov       provider.Provider
	provConn   *store.ModelConnection
	failedConn *store.ModelConnection
	failedErr  error

	// req is the canonical request, after the project prompt and the RAG
	// context were put into it. req.Model is the client's model string.
	req provider.ChatRequest
	// body is the client's request with tool governance applied, for the
	// native path; systemAppend and beforeLastUser are what it adds to it.
	body           *provider.MessagesBody
	systemAppend   []string
	beforeLastUser string
	// version and beta are the client's anthropic-version and anthropic-beta.
	version, beta string
	// translated are the Warning texts of the canonical translation.
	translated []string

	promptChars  int
	opts         ragmuxRequest
	sources      []ragSource
	contextBlock string
}

// providerFor is chatInvocation.providerFor.
func (in *messagesInvocation) providerFor(c *call, conn *store.ModelConnection) (provider.Provider, error) {
	if conn == in.provConn {
		return in.prov, nil
	}
	if conn == in.failedConn && in.failedErr != nil {
		return nil, in.failedErr
	}
	prov, err := in.gw.Providers(conn)
	if err != nil {
		c.log.Error("provider setup", "connection", conn.ID, "err", err)
		return nil, newTargetConfigError()
	}
	in.prov, in.provConn = prov, conn
	return prov, nil
}

// attempt is one Messages call. Like chatInvocation.attempt it returns an
// error only while the client is untouched.
func (in *messagesInvocation) attempt(c *call, conn *store.ModelConnection) error {
	prov, err := in.providerFor(c, conn)
	if err != nil {
		return err
	}
	inline, err := in.opts.inlineField(in.sources, in.contextBlock)
	if err != nil {
		c.log.Error("ragmux response field", "err", err)
		inline = nil
	}
	// Each attempt says what its own path did to the request; the ones of an
	// earlier attempt do not apply to this one.
	h := c.w.Header()
	h.Del("Warning")
	addRagmuxWarnings(h, in.opts.Warnings)
	if nat, ok := prov.(provider.MessagesPassthrough); ok {
		return in.native(c, conn, nat, inline)
	}
	return in.canonical(c, conn, prov, inline)
}

// headerSafe returns v when it may be echoed in a Warning text.
func headerSafe(v string) string {
	if len(v) > 64 || !safeToolName.MatchString(v) {
		return ""
	}
	return v
}

// canonicalWarnings are the Warning texts of the translated path: what the
// translation dropped, what the Messages headers asked for that it cannot
// give, and what the adapter itself ignores.
func (in *messagesInvocation) canonicalWarnings(prov provider.Provider) []string {
	out := append([]string(nil), in.translated...)
	if v := in.version; v != "" && v != "2023-06-01" {
		text := "anthropic-version is not applied by this connection"
		if s := headerSafe(v); s != "" {
			text = "anthropic-version " + s + " is not applied by this connection"
		}
		out = append(out, text)
	}
	if in.beta != "" {
		list := in.beta
		if len(list) > 256 {
			list = list[:256] + "..."
		}
		out = append(out, "anthropic-beta values are not applied by this connection: "+safeBetaList(list))
	}
	if rw, ok := prov.(provider.RequestWarner); ok {
		out = append(out, rw.RequestWarnings(in.req)...)
	}
	return out
}

// safeBetaList keeps the characters a beta name is made of and replaces the
// rest, so a header value cannot put anything but a name into a Warning.
func safeBetaList(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ',', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// canonical is the path of every target that does not speak Messages: the
// request goes through the adapter as a chat request and the answer is written
// as a Messages answer.
func (in *messagesInvocation) canonical(c *call, conn *store.ModelConnection, prov provider.Provider, inline []byte) error {
	addWarnings(c.w.Header(), in.canonicalWarnings(prov))
	req := in.req
	if req.Stream {
		enc := provider.NewMessagesEncoder(req.Model, inline)
		return in.gw.stream(c.w, c.r, c.budgetLeft(), prov, conn, req, c.rec, c.obs, in.promptChars, withMessagesEncoder(enc))
	}

	ctx, cancel := c.attemptContext()
	defer cancel()
	upstreamStart := time.Now()
	resp, err := prov.Chat(ctx, req)
	c.obs.upstream = time.Since(upstreamStart)
	if err != nil {
		return err
	}
	c.obs.noteResponseModel(resp.Model, req.Model, conn.ModelName)
	var called toolNameSet
	for i := range resp.Choices {
		c.obs.noteFinishReason(resp.Choices[i].FinishReason)
		called.add(resp.Choices[i].Message.ToolCalls)
	}
	c.rec.ToolNames = called.names()
	fillUsage(c.rec, resp.Usage, in.promptChars, completionChars(resp))
	c.rec.StatusCode = http.StatusOK
	c.w.Header().Set("Content-Type", "application/json")
	out, err := json.Marshal(provider.ChatToMessages(resp, req.Model, inline))
	if err != nil {
		// Marshalling strings and raw JSON that decoded is not expected to fail.
		c.log.Error("encode messages answer", "err", err)
		c.rec.StatusCode = http.StatusInternalServerError
		writeError(c.w, http.StatusInternalServerError, "server_error", "response could not be encoded")
		return nil
	}
	_, _ = c.w.Write(append(out, '\n'))
	return nil
}

// native is the path of an Anthropic target.
func (in *messagesInvocation) native(c *call, conn *store.ModelConnection, nat provider.MessagesPassthrough, inline []byte) error {
	prep, err := nat.PrepareMessages(provider.MessagesInput{Body: in.body, SystemAppend: in.systemAppend,
		BeforeLastUser: in.beforeLastUser, RAGActive: in.req.RAGActive, Version: in.version, Beta: in.beta, Inline: inline,
		EchoModel: in.req.Model})
	if err != nil {
		c.log.Error("prepare messages request", "err", err)
		return &provider.Error{Status: http.StatusInternalServerError, Type: "server_error", Message: "request could not be prepared"}
	}
	addWarnings(c.w.Header(), prep.Warnings)
	if prep.Stream {
		return in.nativeStream(c, conn, nat, prep)
	}

	ctx, cancel := c.attemptContext()
	defer cancel()
	upstreamStart := time.Now()
	reply, err := nat.MessagesCall(ctx, prep)
	c.obs.upstream = time.Since(upstreamStart)
	if err != nil {
		return err
	}
	c.obs.noteResponseModel(reply.Model, in.req.Model, conn.ModelName)
	c.obs.noteFinishReason(reply.Finish)
	var called toolNameSet
	for _, n := range reply.ToolNames {
		called.addName(n)
	}
	c.rec.ToolNames = called.names()
	fillUsage(c.rec, reply.Usage, in.promptChars, reply.TextChars)
	c.rec.StatusCode = http.StatusOK
	c.w.Header().Set("Content-Type", "application/json")
	_, _ = c.w.Write(reply.Body) //nolint:gosec // G705: the upstream's JSON message, relayed unchanged as application/json
	return nil
}

// nativeStream relays an Anthropic stream. Like stream() it returns an error
// only when the upstream failed before its first event, so the walk may go on
// to another target; after that every outcome is settled in the log row.
func (in *messagesInvocation) nativeStream(c *call, conn *store.ModelConnection, nat provider.MessagesPassthrough, prep provider.MessagesPrepared) error {
	w := c.w
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported by server")
		return nil
	}
	ctx, cancel := context.WithCancel(c.r.Context())
	defer cancel()
	var budgetHit atomic.Bool
	var budgetTimer *time.Timer
	if firstByte := c.budgetLeft(); firstByte > 0 {
		budgetTimer = time.AfterFunc(firstByte, func() {
			budgetHit.Store(true)
			cancel()
		})
		defer budgetTimer.Stop()
	}
	g := in.gw
	sm := g.Metrics.StreamStarted(conn.ProviderType)
	defer sm.Ended()
	upstreamStart := time.Now()

	st, err := nat.MessagesStream(ctx, prep)
	if err != nil {
		c.obs.upstream = time.Since(upstreamStart)
		if budgetHit.Load() {
			err = &provider.Error{Status: http.StatusGatewayTimeout, Type: "timeout", Message: "upstream request cancelled or timed out"}
		}
		return err
	}
	defer func() { _ = st.Close() }()

	started := false
	begin := func() {
		started = true
		if budgetTimer != nil {
			budgetTimer.Stop()
		}
		c.obs.ttft = time.Since(upstreamStart)
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}
	sum, err := st.Relay(w, flusher.Flush, begin, sm.Chunk)
	c.obs.upstream = time.Since(upstreamStart)

	var called toolNameSet
	for _, n := range sum.ToolNames {
		called.addName(n)
	}
	if names := called.names(); len(names) > 0 {
		c.rec.ToolNames = names
	}
	if started {
		c.obs.noteResponseModel(sum.Model, in.req.Model, conn.ModelName)
		c.obs.noteFinishReason(sum.Finish)
	}

	if errors.Is(err, provider.ErrClientWrite) || (started && c.ctx().Err() != nil) {
		c.rec.StatusCode, c.rec.Error = statusClientClosed, errClientClosed
		fillUsage(c.rec, sum.Usage, in.promptChars, sum.TextChars)
		return nil
	}
	if budgetHit.Load() && !started {
		err = &provider.Error{Status: http.StatusGatewayTimeout, Type: "timeout", Message: "upstream request cancelled or timed out"}
	}
	if err != nil && !started {
		return err
	}
	switch {
	case err != nil:
		// The upstream broke off after the client had its first event.
		_, pe := providerError(err)
		c.rec.StatusCode, c.rec.Error, c.obs.errType = http.StatusBadGateway, pe.Message, pe.Type
		g.Log.Warn("upstream stream failed", "project", c.rec.ProjectID, "msg", pe.Message)
		_, _ = w.Write(provider.MessagesErrorEvent(pe.Status, pe.Message))
		flusher.Flush()
	case sum.FailedType != "":
		// An error event the upstream sent; the client already has it.
		c.rec.StatusCode, c.rec.Error, c.obs.errType = http.StatusBadGateway, sum.FailedMessage, sum.FailedType
		g.Log.Warn("upstream stream failed", "project", c.rec.ProjectID, "msg", sum.FailedMessage)
	default:
		c.rec.StatusCode = http.StatusOK
	}
	fillUsage(c.rec, sum.Usage, in.promptChars, sum.TextChars)
	return nil
}

// addName adds one tool name, as add does for a tool_calls value.
func (s *toolNameSet) addName(name string) {
	if name == "" {
		return
	}
	if s.seen == nil {
		s.seen = map[string]struct{}{}
	}
	if len(s.seen) >= maxCollectedTools {
		return
	}
	s.seen[cutName(name)] = struct{}{}
}
