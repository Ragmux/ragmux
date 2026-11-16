package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/guardrail"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/webhook"
)

// Guardrails (ADR-034). A project's rules are applied at three stages of a
// request: pre_request (what the client sent), pre_retrieval (the query that
// goes to the search) and post_response (what the model answered). This file
// holds the state a request carries through them and the two stages that look
// at the request; guardrail_walk.go finds the texts of a request,
// guardrail_response.go does the response side.
//
// Nothing here logs, records or labels the text that matched (PRD rule 10):
// the row, the metrics and the log lines carry counters, stage names and rule
// ids.

// Codes of the errors the guardrails answer with.
const (
	codeGuardrailBlocked     = "guardrail_blocked"
	codeGuardrailUnavailable = "guardrail_unavailable"
)

// guardBlockedMessage is the whole explanation a blocked client gets. The rule
// that matched is not part of it (it would let a client probe the rules); it is
// on the request log row.
func guardBlockedMessage(stage string) string {
	return "request blocked by project guardrail (stage: " + stage + ")"
}

// guardUnavailableMessage is what a client gets when the guardrails could not
// be consulted and the project fails closed.
const guardUnavailableMessage = "guardrail check unavailable"

// webhookReason is what the webhook may say about its verdict.
var webhookReason = regexp.MustCompile(`^[a-z0-9_.-]{1,32}$`)

// Webhook limits (ADR-034, G8).
const (
	// guardWebhookMaxBody is the largest request body sent to a guardrail
	// webhook; a request that needs more is not sent and counts as a failure.
	guardWebhookMaxBody = 1 << 20
	// guardWebhookMaxReply bounds what is read back: the same texts, possibly
	// redacted, plus the envelope.
	guardWebhookMaxReply = guardWebhookMaxBody + 64<<10
	guardWebhookVersion  = "2026-11-01"
)

// guardState is what the guardrails of one request know and have found. It
// exists only for a project that has guardrails; a nil *guardState is a request
// they do not touch.
type guardState struct {
	set *guardrail.Set

	redactions int
	flags      int
	rules      []string
	// blockedStage is the stage a rule (or the webhook) blocked the request at.
	blockedStage string
	// webhook is the worst outcome of the webhook calls of the request, as the
	// column spells it; empty when it was not asked.
	webhook string
}

// absorb adds what a scan found to the request's totals.
func (gs *guardState) absorb(sc *guardrail.Scan) {
	gs.redactions += sc.Redactions()
	gs.flags += sc.Flags()
	for _, id := range sc.Rules() {
		gs.noteRule(id)
	}
}

func (gs *guardState) noteRule(id string) {
	if len(gs.rules) >= store.MaxGuardrailRules {
		return
	}
	for _, have := range gs.rules {
		if have == id {
			return
		}
	}
	gs.rules = append(gs.rules, id)
}

// webhookRank orders the webhook outcomes by how much an operator should care.
var webhookRank = map[string]int{
	obs.GuardrailWebhookOK: 1, obs.GuardrailWebhookSkippedFailOpen: 2,
	obs.GuardrailWebhookTimeout: 3, obs.GuardrailWebhookError: 4,
}

func (gs *guardState) noteWebhook(outcome string) {
	if webhookRank[outcome] > webhookRank[gs.webhook] {
		gs.webhook = outcome
	}
}

// apply writes the findings onto the request log row.
func (gs *guardState) apply(rec *store.RequestLog) {
	if gs == nil {
		return
	}
	rec.GuardrailBlockedStage = gs.blockedStage
	rec.GuardrailRedactions = gs.redactions
	rec.GuardrailFlags = gs.flags
	rec.GuardrailRules = gs.rules
	rec.GuardrailWebhook = gs.webhook
}

// guardCache holds the compiled guardrails of each project. It is shared by
// every Gateway of the process and keyed by the stored configuration itself, so
// two gateways that read the same value share a compilation and one that reads
// a different value compiles its own.
var guardCache guardrail.Cache

// guardrails returns the project's compiled configuration. A project without
// guardrails yields nil; a stored configuration this build cannot use is an
// error, which the caller answers closed (a security control that silently
// stops working is worse than one that says so).
func (g *Gateway) guardrails(c *call) (*guardrail.Set, error) {
	return guardCache.For(c.project.ID, c.project.Guardrails)
}

// guardBegin attaches the request's guardrail state to c. It answers the client
// and records the request, and returns false, when the project's configuration
// cannot be used. streamed is only what the request log row says.
func (g *Gateway) guardBegin(c *call, streamed bool) bool {
	set, err := g.guardrails(c)
	if err != nil {
		c.log.Error("guardrail configuration unusable; refusing the request", "err", err)
		g.refuseGuard(c, streamed, http.StatusServiceUnavailable, "server_error", codeGuardrailUnavailable,
			guardUnavailableMessage, codeGuardrailUnavailable)
		return false
	}
	if set != nil {
		c.guard = &guardState{set: set}
	}
	return true
}

// refuseGuard answers a request the guardrails stopped and writes its row. The
// request has passed the limit check, so it counts against the project's
// request limit like any other (record charges it). Nothing went upstream.
func (g *Gateway) refuseGuard(c *call, streamed bool, status int, typ, code, msg, rowError string) {
	writeErrorCode(c.w, status, typ, code, msg)
	g.openRecord(c, streamed, nil)
	c.rec.StatusCode, c.rec.Error = status, rowError
	c.rec.Attempt = 0
	g.record(c)
}

// countEvents adds the findings of one scan to the metrics.
func (g *Gateway) countEvents(c *call, stage string, sc *guardrail.Scan, blockedNow bool) {
	g.Metrics.RecordGuardrailEvents(c.projectID, stage, obs.GuardrailRedact, sc.Redactions())
	g.Metrics.RecordGuardrailEvents(c.projectID, stage, obs.GuardrailFlag, sc.Flags())
	if blockedNow {
		g.Metrics.RecordGuardrailEvents(c.projectID, stage, obs.GuardrailBlock, 1)
	}
}

// guardRefusal is an answer the guardrails decided on at a point where the
// request log row is not open yet. The handler applies it once it is.
type guardRefusal struct {
	status                   int
	typ, code, msg, rowError string
}

// apply answers the client and marks the row. The row is open (openRecord) and
// the handler's deferred record writes it.
func (r *guardRefusal) apply(c *call) {
	writeErrorCode(c.w, r.status, r.typ, r.code, r.msg)
	c.rec.StatusCode, c.rec.Error = r.status, r.rowError
	c.rec.Attempt = 0
}

// guardVerdict is the outcome of a stage over the texts of a request.
type guardVerdict struct {
	blocked     bool
	unavailable bool
	// redactions is how many matches this stage replaced, for the Warning.
	redactions int
}

// guardTexts runs a request stage -- pre_request or pre_retrieval -- over the
// texts walk offers: first the project's own rules, then the webhook with what
// is left. walk calls visit once per text, in a fixed order, and puts back what
// visit returns. The webhook body names the call's own endpoint.
//
// It records the findings on c.guard and counts them; what to do with a block
// or an outage is the caller's, because it differs by endpoint and stage.
func (g *Gateway) guardTexts(c *call, stage string, walk textWalker) guardVerdict {
	gs := c.guard
	var v guardVerdict
	if gs == nil {
		return v
	}
	if gs.set.HasStage(stage) {
		sc := gs.set.Scan(stage, false)
		walk(func(role, text string) string {
			if strings.HasPrefix(role, rawRolePrefix) {
				sc.Probe(text)
				return text
			}
			if sc.Blocked() {
				return text
			}
			if strings.HasPrefix(role, nameRolePrefix) {
				return sc.ApplyName(text)
			}
			return sc.Apply(text)
		})
		gs.absorb(sc)
		g.countEvents(c, stage, sc, sc.Blocked())
		v.redactions += sc.Redactions()
		if sc.Blocked() {
			gs.blockedStage = stage
			v.blocked = true
			return v
		}
	}
	enabled, timeoutMS, failOpen := gs.set.WebhookSettings()
	if !enabled {
		return v
	}
	var texts []webhookText
	walk(func(role, text string) string {
		if !skipsWebhook(role) {
			texts = append(texts, webhookText{Index: len(texts), Role: webhookRole(role), Text: text})
		}
		return text
	})
	if len(texts) == 0 {
		return v
	}
	reply, outcome := g.callGuardWebhook(c, stage, texts, time.Duration(timeoutMS)*time.Millisecond)
	if outcome != obs.GuardrailWebhookOK {
		if failOpen {
			outcome = obs.GuardrailWebhookSkippedFailOpen
		} else {
			v.unavailable = true
		}
	}
	gs.noteWebhook(outcome)
	g.Metrics.RecordGuardrailWebhook(c.projectID, outcome)
	if v.unavailable || outcome == obs.GuardrailWebhookSkippedFailOpen {
		return v
	}
	reason := "webhook"
	if reply.Reason != "" {
		reason = "webhook:" + reply.Reason
	}
	switch reply.Action {
	case "block":
		gs.noteRule(reason)
		gs.blockedStage = stage
		g.Metrics.RecordGuardrailEvents(c.projectID, stage, obs.GuardrailBlock, 1)
		v.blocked = true
	case "redact":
		// The reply has one entry per text, in the order they were sent
		// (parseGuardReply checked it) and walk visits them in that order.
		i := 0
		walk(func(role, text string) string {
			if skipsWebhook(role) {
				return text
			}
			out := reply.Texts[i].Text
			i++
			return out
		})
		n := 0
		for i, t := range reply.Texts {
			if t.Text != texts[i].Text {
				n++
			}
		}
		if n > 0 {
			gs.redactions += n
			gs.noteRule(reason)
			g.Metrics.RecordGuardrailEvents(c.projectID, stage, obs.GuardrailRedact, n)
			v.redactions += n
		}
	case "flag":
		gs.flags++
		gs.noteRule(reason)
		g.Metrics.RecordGuardrailEvents(c.projectID, stage, obs.GuardrailFlag, 1)
	}
	return v
}

// webhookText is one text in the body of a guardrail webhook call.
type webhookText struct {
	Index int    `json:"index"`
	Role  string `json:"role,omitempty"`
	Text  string `json:"text"`
}

// webhookRequest is the body sent to the guardrail webhook (ADR-034, G8).
type webhookRequest struct {
	Version   string        `json:"version"`
	Stage     string        `json:"stage"`
	RequestID string        `json:"request_id"`
	ProjectID int64         `json:"project_id"`
	Endpoint  string        `json:"endpoint"`
	Texts     []webhookText `json:"texts"`
}

// webhookReply is what the receiver answers.
type webhookReply struct {
	Action string `json:"action"`
	Texts  []struct {
		Index *int    `json:"index"`
		Text  *string `json:"text"`
	} `json:"texts"`
	Reason string `json:"reason"`
}

// guardReply is a webhookReply that passed the checks, texts flattened.
type guardReply struct {
	Action string
	Texts  []webhookText
	Reason string
}

// callGuardWebhook sends the texts to the project's guardrail webhook and
// reads the verdict. The outcome is obs.GuardrailWebhookOK, or Timeout or
// Error when the call failed in any way the design counts as a failure: a
// missing or disabled endpoint, a body over 1 MB (not sent), a transport
// error, a status outside 2xx, a reply that is not the documented shape, or
// texts that do not line up. There is no retry.
func (g *Gateway) callGuardWebhook(c *call, stage string, texts []webhookText, timeout time.Duration) (guardReply, string) {
	fail := func(outcome, why string, args ...any) (guardReply, string) {
		c.log.Warn("guardrail webhook failed: "+why, append([]any{"stage", stage, "outcome", outcome}, args...)...)
		return guardReply{}, outcome
	}
	if g.Webhooks == nil {
		return fail(obs.GuardrailWebhookError, "no webhook client is configured")
	}
	ep, err := g.Store.GetWebhookEndpoint(c.ctx(), c.project.ID, store.WebhookPurposeGuardrail)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fail(obs.GuardrailWebhookError, "the project has no guardrail webhook endpoint")
	case err != nil:
		return fail(obs.GuardrailWebhookError, "the endpoint could not be read", "err", err)
	case ep.DisabledAt != nil:
		return fail(obs.GuardrailWebhookError, "the endpoint is disabled")
	}
	body, err := json.Marshal(webhookRequest{Version: guardWebhookVersion, Stage: stage, RequestID: c.requestID,
		ProjectID: c.project.ID, Endpoint: c.op.endpoint, Texts: texts})
	if err != nil {
		return fail(obs.GuardrailWebhookError, "the request could not be encoded", "err", err)
	}
	if len(body) > guardWebhookMaxBody {
		return fail(obs.GuardrailWebhookError, "the request is over the 1 MB webhook limit", "bytes", len(body))
	}
	resp, err := g.Webhooks.Call(c.ctx(), webhook.Request{
		Purpose: obs.WebhookPurposeGuardrail, URL: ep.URL, EventID: newWebhookEventID(), Body: body,
		Secrets: webhook.ActiveSecrets(ep, time.Now()), Timeout: timeout, MaxResponseBytes: guardWebhookMaxReply,
	})
	if err != nil {
		var ce *webhook.CallError
		if errors.As(err, &ce) {
			outcome := obs.GuardrailWebhookError
			if ce.Class == store.DeliveryErrTimeout {
				outcome = obs.GuardrailWebhookTimeout
			}
			// The error's class and status are fixed values; its text is not
			// logged because a transport error can quote the URL.
			return fail(outcome, "the call failed", "class", ce.Class, "status", ce.StatusCode)
		}
		return fail(obs.GuardrailWebhookError, "the call failed")
	}
	reply, ok := parseGuardReply(resp.Body, texts)
	if !ok {
		return fail(obs.GuardrailWebhookError, "the reply is not the documented shape", "status", resp.StatusCode)
	}
	return reply, obs.GuardrailWebhookOK
}

// parseGuardReply checks a webhook reply against the documented shape and the
// texts that were sent.
func parseGuardReply(raw []byte, sent []webhookText) (guardReply, bool) {
	var in webhookReply
	if err := json.Unmarshal(raw, &in); err != nil {
		return guardReply{}, false
	}
	out := guardReply{Action: in.Action}
	switch in.Action {
	case "allow", "block", "flag":
	case "redact":
		if len(in.Texts) != len(sent) {
			return guardReply{}, false
		}
		for i, t := range in.Texts {
			if t.Index == nil || *t.Index != i || t.Text == nil {
				return guardReply{}, false
			}
			out.Texts = append(out.Texts, webhookText{Index: i, Role: sent[i].Role, Text: *t.Text})
		}
	default:
		return guardReply{}, false
	}
	if in.Reason != "" {
		if !webhookReason.MatchString(in.Reason) {
			return guardReply{}, false
		}
		out.Reason = in.Reason
	}
	return out, true
}

// newWebhookEventID is the webhook-id of one synchronous call: fresh every
// time, unlike an alert's, which names the event it repeats.
func newWebhookEventID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "evt_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "evt_" + hex.EncodeToString(b[:])
}

// guardRedactWarning is the Warning for a request stage that changed what the
// client sent (PRD rule 12).
func guardRedactWarning(n int, what string) string {
	return "guardrail redacted " + strconv.Itoa(n) + " match(es)" + what
}

// guardPreRequest runs the pre_request stage of a request over the texts walk
// offers. It answers the client and records the request, and returns false,
// when a rule blocks it or the guardrails are unavailable. Otherwise it returns
// the Warning to send for what was redacted, empty when nothing was.
func (g *Gateway) guardPreRequest(c *call, walk textWalker, streamed bool) (warning string, ok bool) {
	v := g.guardTexts(c, guardrail.StagePreRequest, walk)
	switch {
	case v.blocked:
		g.refuseGuard(c, streamed, http.StatusBadRequest, "invalid_request_error", codeGuardrailBlocked,
			guardBlockedMessage(guardrail.StagePreRequest), codeGuardrailBlocked)
		return "", false
	case v.unavailable:
		g.refuseGuard(c, streamed, http.StatusServiceUnavailable, "server_error", codeGuardrailUnavailable,
			guardUnavailableMessage, codeGuardrailUnavailable)
		return "", false
	case v.redactions > 0:
		return guardRedactWarning(v.redactions, ""), true
	}
	return "", true
}

// guardQueryResult is the outcome of the pre_retrieval stage over a search
// query.
type guardQueryResult struct {
	query string
	guardVerdict
}

// guardQuery runs the pre_retrieval stage over a retrieval query. The caller
// decides what a block means: chat skips retrieval, /v1/retrieve and the MCP
// tool refuse.
func (g *Gateway) guardQuery(c *call, query string) guardQueryResult {
	res := guardQueryResult{query: query}
	if c.guard == nil {
		return res
	}
	res.guardVerdict = g.guardTexts(c, guardrail.StagePreRetrieval,
		func(visit func(role, text string) string) { res.query = visit("query", res.query) })
	return res
}

// guardRetrieve is the pre_retrieval stage of the endpoints that are a
// retrieval (/v1/retrieve, the MCP search tools). It returns the query to
// search with. When the query is blocked, or the guardrails are unavailable, it
// answers the client, records the request and returns false.
func (g *Gateway) guardRetrieve(c *call, query string) (string, bool) {
	if !g.guardBegin(c, false) {
		return "", false
	}
	if c.guard == nil {
		return query, true
	}
	res := g.guardQuery(c, query)
	switch {
	case res.blocked:
		g.refuseGuard(c, false, http.StatusBadRequest, "invalid_request_error", codeGuardrailBlocked,
			guardBlockedMessage(guardrail.StagePreRetrieval), codeGuardrailBlocked)
		return "", false
	case res.unavailable:
		g.refuseGuard(c, false, http.StatusServiceUnavailable, "server_error", codeGuardrailUnavailable,
			guardUnavailableMessage, codeGuardrailUnavailable)
		return "", false
	case res.redactions > 0:
		addWarnings(c.w.Header(), []string{guardRedactWarning(res.redactions, " in the retrieval query")})
	}
	return res.query, true
}

// guardLogprobs takes the token log-probabilities out of a request whose
// project blocks or redacts in the answer (a stage of flag rules only counts
// what it sees and hides nothing, so it has nothing to protect them from). They spell the answer out a token at a time, a
// token cannot be redacted, and a window cannot hold them; so the upstream is
// not asked for them, and the client is told. Without such a rule the request is
// untouched.
func (g *Gateway) guardLogprobs(c *call, req *provider.ChatRequest) string {
	if c.guard == nil || !c.guard.set.Rewrites(guardrail.StagePostResponse) {
		return ""
	}
	asked := false
	for _, key := range []string{"logprobs", "top_logprobs"} {
		v, ok := req.Extra[key]
		if !ok {
			continue
		}
		delete(req.Extra, key)
		switch s := strings.TrimSpace(string(v)); s {
		case "", "null", "false", "0":
		default:
			asked = true
		}
	}
	if !asked {
		return ""
	}
	return "logprobs was dropped: this project's guardrails read the answer, which token probabilities would spell out"
}
