package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/ragmux/ragmux/internal/httpx"
	"github.com/ragmux/ragmux/internal/store"
)

// POST /v1/feedback (ADR-042): a score and an optional label for a request
// the project already made, found by its request_id. It takes no content, so
// it is not a pipeline call: it spends no slot of the project's request or
// token limits, opens no request log row and has no span or metric of its own
// (PRD rule 10).

const (
	// codeRequestNotFound answers an id the project has no request log for, and
	// an id that belongs to another project: one answer for both, so a key
	// holder cannot probe other projects' ids.
	codeRequestNotFound = "request_not_found"
	codeInvalidFeedback = "invalid_feedback"

	// maxFeedbackBody bounds the body: an id, a score and a 64 byte label.
	maxFeedbackBody = 4 << 10
)

// feedbackLabelPattern is what a label may look like.
var feedbackLabelPattern = regexp.MustCompile(`^[a-z0-9_.:-]{1,64}$`)

// feedbackRequest is the body of POST /v1/feedback. Pointers tell a missing
// field from a zero value.
type feedbackRequest struct {
	RequestID *string `json:"request_id"`
	Score     *int    `json:"score"`
	Label     *string `json:"label"`
}

// feedback records one score for (project, request_id). A key needs the chat or
// the retrieve scope, the two scopes whose calls leave a request_id to rate; no
// scope of its own exists (ADR-019 stands). Any key of the project may rate any
// request of the project: the backend makes the call, a front end with a key of
// its own rates it. A repeated submission replaces the earlier one.
func (g *Gateway) feedback(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r.Context())
	if pr != nil && pr.key != nil && !pr.key.HasScope(store.ScopeChat) && !pr.key.HasScope(store.ScopeRetrieve) {
		writeErrorCode(w, http.StatusForbidden, "insufficient_scope", "insufficient_scope",
			"api key is not authorised for feedback (it needs the chat or retrieve scope)")
		return
	}
	if !checkProject(w, pr) {
		return
	}
	p := projectFrom(r.Context())
	if p == nil {
		writeError(w, http.StatusInternalServerError, "server_error", "project lookup failed")
		return
	}

	body, ok := readFeedbackBody(w, r)
	if !ok {
		return
	}
	in, msg := parseFeedback(body)
	if msg != "" {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request_error", codeInvalidFeedback, msg)
		return
	}

	var keyID *int64
	if pr != nil && pr.key != nil {
		keyID = &pr.key.ID
	}
	err := g.Store.UpsertFeedback(r.Context(), p.ID, *in.RequestID, *in.Score, in.Label, keyID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "not_found_error", codeRequestNotFound,
			"no request with that request_id in this project")
		return
	case err != nil:
		g.Log.Error("record feedback", "project", p.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "feedback could not be recorded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"request_id": *in.RequestID, "score": *in.Score, "label": in.Label})
}

// readFeedbackBody reads the body up to maxFeedbackBody. It answers the client
// itself on failure.
func readFeedbackBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	httpx.Deadline(w, bodyReadDeadline)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxFeedbackBody+1))
	httpx.Deadline(w, 0)
	if err != nil || len(body) > maxFeedbackBody {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return nil, false
	}
	return body, true
}

// parseFeedback decodes and checks a body. Any field the endpoint does not
// know is refused, free text included: the endpoint takes no content. msg is
// empty when the body is valid.
func parseFeedback(body []byte) (in feedbackRequest, msg string) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, "request body must be a JSON object with request_id, score and an optional label: " + err.Error()
	}
	if dec.More() {
		return in, "request body must be a single JSON object"
	}
	switch {
	case in.RequestID == nil || !validRequestID(*in.RequestID):
		return in, "request_id must be 1 to 128 characters of letters, digits and . _ : -"
	case in.Score == nil || (*in.Score != 1 && *in.Score != -1):
		return in, "score must be 1 or -1"
	case in.Label != nil && !feedbackLabelPattern.MatchString(*in.Label):
		return in, "label must match ^[a-z0-9_.:-]{1,64}$"
	}
	return in, ""
}
