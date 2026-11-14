package admin

import (
	"net/http"
	"strconv"
)

// feedbackSummary answers GET /api/projects/{id}/feedback/summary?days=30
// (ADR-042): the project's feedback over the last days days -- positive,
// negative, the share of positive, the labelled breakdown and the daily
// series. It counts; it shows no request or response content, which the
// gateway never received. The caller needs the same access as to the
// project's request logs (the read group).
func (a *Admin) feedbackSummary(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	out, err := a.Store.FeedbackSummary(r.Context(), p.ID, days)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
