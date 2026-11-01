package admin

import (
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/store"
)

// sessionIDPattern is what a session id may look like (ADR-032): the same set
// the gateway admits, so a path that cannot name a session is a 404 without a
// database lookup.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// listSessions answers GET /api/projects/{id}/sessions: the project's sessions,
// the most recently active first, with their request and token totals. It
// reads session_usage, so it costs no GROUP BY over the request logs. The
// caller needs the same access as to the project's request logs.
func (a *Admin) listSessions(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	list, err := a.Store.ListSessions(r.Context(), p.ID, limit, offset)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// getSession answers GET /api/projects/{id}/sessions/{sessionId}: the session's
// totals and its request log rows, newest first.
func (a *Admin) getSession(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	sid := chi.URLParam(r, "sessionId")
	if !sessionIDPattern.MatchString(sid) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	sess, err := a.Store.GetSession(r.Context(), p.ID, sid)
	if err != nil {
		a.fail(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Store.SessionRequests(r.Context(), p.ID, sid, limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionDetail{Session: sess, Requests: rows})
}

// sessionDetail is the body of getSession.
type sessionDetail struct {
	Session  *store.SessionUsage `json:"session"`
	Requests []*store.RequestLog `json:"requests"`
}
