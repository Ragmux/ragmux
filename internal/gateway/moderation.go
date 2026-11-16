package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/guardrail"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/store"
)

// POST /v1/moderations (ADR-034): the OpenAI moderation call, answered by the
// project's own guardrails. It reads each input with the rules bound to the
// pre_request stage, and the guardrail webhook if the project has one, and
// reports what they found; it applies nothing. No model is called, so it costs
// no tokens, but it counts as a request: it takes a slot of the project's
// request limit and leaves a request log row (endpoint moderations).

// moderationsOperation is POST /v1/moderations. It is chat as far as scopes go
// (a key that may chat may ask whether a text would pass).
var moderationsOperation = operation{
	name:  "moderation",
	span:  "gateway.moderation",
	scope: store.ScopeChat,
	what:  "moderations",

	endpoint: "moderations",
}

const (
	// moderationModel is the model the answer names: the guardrails, whatever
	// the request said.
	moderationModel = "ragmux-guardrails"
	// maxModerationInputs bounds one call.
	maxModerationInputs = 32
)

// moderationConn stands in for the connection the call does not use: the
// request log row and the metrics name a model and a provider.
var moderationConn = &store.ModelConnection{Name: moderationModel, ProviderType: "ragmux", ModelName: moderationModel}

// moderationResult is one entry of the answer.
type moderationResult struct {
	Flagged        bool               `json:"flagged"`
	Categories     map[string]bool    `json:"categories"`
	CategoryScores map[string]float64 `json:"category_scores"`
	Ragmux         moderationRagmux   `json:"ragmux"`
}

// moderationRagmux says what the project's rules would do with the input.
type moderationRagmux struct {
	Action string `json:"action"`
}

type moderationResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Results []moderationResult `json:"results"`
}

func (g *Gateway) moderations(w http.ResponseWriter, r *http.Request) {
	c, ok := g.authenticate(w, r, moderationsOperation)
	if !ok {
		return
	}
	defer c.span.End()

	inputs, ok := g.decodeModeration(c)
	if !ok {
		return
	}
	if !acceptTargets(c, []*store.ModelConnection{moderationConn}) {
		return
	}
	if !g.checkLimits(c, 0, false) {
		return
	}
	if !g.guardBegin(c, false) {
		return
	}
	c.free = true
	g.openRecord(c, false, nil)
	defer g.record(c)
	// Nothing goes upstream, so there is no attempt to count.
	c.rec.StatusCode, c.rec.Attempt = http.StatusOK, 0

	results := make([]moderationResult, len(inputs))
	for i, in := range inputs {
		results[i] = g.moderateInput(c, in)
	}
	if enabled, timeoutMS, failOpen := c.guardSettings(); enabled {
		texts := make([]webhookText, len(inputs))
		for i, in := range inputs {
			texts[i] = webhookText{Index: i, Role: "user", Text: in}
		}
		reply, outcome := g.callGuardWebhook(c, guardrail.StagePreRequest, texts, time.Duration(timeoutMS)*time.Millisecond)
		if outcome != obs.GuardrailWebhookOK && failOpen {
			outcome = obs.GuardrailWebhookSkippedFailOpen
		}
		c.guard.noteWebhook(outcome)
		g.Metrics.RecordGuardrailWebhook(c.projectID, outcome)
		switch {
		case outcome == obs.GuardrailWebhookSkippedFailOpen:
		case outcome != obs.GuardrailWebhookOK:
			(&guardRefusal{status: http.StatusServiceUnavailable, typ: "server_error", code: codeGuardrailUnavailable,
				msg: guardUnavailableMessage, rowError: codeGuardrailUnavailable}).apply(c)
			return
		default:
			mergeWebhookReport(c, results, inputs, reply)
		}
	}

	c.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(c.w).Encode(moderationResponse{ID: newModerationID(), Model: moderationModel, Results: results})
}

// guardSettings reports the webhook settings of the call's project; the
// webhook is off for a project without guardrails.
func (c *call) guardSettings() (enabled bool, timeoutMS int, failOpen bool) {
	if c.guard == nil {
		return false, 0, false
	}
	return c.guard.set.WebhookSettings()
}

// decodeModeration reads the request body: {"input": string | [string], "model"?}.
// model is ignored, but a body that is not JSON, or an input that is not text,
// is refused.
func (g *Gateway) decodeModeration(c *call) ([]string, bool) {
	body, ok := g.readBody(c)
	if !ok {
		return nil, false
	}
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return nil, false
	}
	var inputs []string
	switch firstByte(req.Input) {
	case '"':
		var s string
		if json.Unmarshal(req.Input, &s) != nil {
			break
		}
		inputs = []string{s}
	case '[':
		// *string, so a null entry is told from an empty string.
		var items []*string
		if json.Unmarshal(req.Input, &items) != nil || slices.Contains(items, nil) {
			writeError(c.w, http.StatusBadRequest, "invalid_request_error", "input must be a string or an array of strings")
			return nil, false
		}
		for _, it := range items {
			inputs = append(inputs, *it)
		}
	case 0:
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return nil, false
	default:
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "input must be a string or an array of strings")
		return nil, false
	}
	switch {
	case len(inputs) == 0:
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return nil, false
	case len(inputs) > maxModerationInputs:
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "input has more than 32 entries")
		return nil, false
	}
	return inputs, true
}

// actionRank orders the actions by how much they would do to the text.
var actionRank = map[string]int{"none": 0, guardrail.ActionFlag: 1, guardrail.ActionRedact: 2, guardrail.ActionBlock: 3}

// moderateInput checks one input with the project's pre_request rules. Every
// rule of the stage is a category; the action is the strongest of the rules
// that matched. The request row learns which rules matched, not what.
func (g *Gateway) moderateInput(c *call, input string) moderationResult {
	res := moderationResult{Categories: map[string]bool{}, CategoryScores: map[string]float64{},
		Ragmux: moderationRagmux{Action: "none"}}
	if c.guard == nil {
		return res
	}
	for _, hit := range c.guard.set.Report(guardrail.StagePreRequest, input) {
		matched := hit.Count > 0
		res.Categories[hit.ID] = matched
		res.CategoryScores[hit.ID] = 0
		if !matched {
			continue
		}
		res.CategoryScores[hit.ID] = 1
		res.Flagged = true
		c.guard.noteRule(hit.ID)
		if actionRank[hit.Action] > actionRank[res.Ragmux.Action] {
			res.Ragmux.Action = hit.Action
		}
	}
	return res
}

// mergeWebhookReport adds the webhook's verdict to the results. The webhook
// answers once for the whole call, so a block or a flag cannot be pinned on one
// input and marks them all; a redact marks the inputs it changed.
func mergeWebhookReport(c *call, results []moderationResult, inputs []string, reply guardReply) {
	if reply.Action == "allow" {
		return
	}
	reason := "webhook"
	if reply.Reason != "" {
		reason = "webhook:" + reply.Reason
	}
	c.guard.noteRule(reason)
	for i := range results {
		if reply.Action == "redact" && reply.Texts[i].Text == inputs[i] {
			continue
		}
		results[i].Flagged = true
		results[i].Categories["webhook"] = true
		results[i].CategoryScores["webhook"] = 1
		if actionRank[reply.Action] > actionRank[results[i].Ragmux.Action] {
			results[i].Ragmux.Action = reply.Action
		}
	}
}

// newModerationID is the id of an answer: "modr-" and 24 hex characters.
func newModerationID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "modr-" + strings.Repeat("0", 24)
	}
	return "modr-" + hex.EncodeToString(b[:])
}
