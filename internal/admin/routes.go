package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// routeInput is the body of a route create or update. An update replaces the
// route as a whole, targets included (ADR-017).
type routeInput struct {
	Name     string             `json:"name"`
	Kind     string             `json:"kind"`
	Strategy string             `json:"strategy"`
	Targets  []routeTargetInput `json:"targets"`
}

type routeTargetInput struct {
	ConnectionID int64   `json:"connection_id"`
	Model        *string `json:"model"`
	// Weight and Priority are pointers so an omitted field takes its default
	// (1 and 0) while an explicit 0 weight is still rejected.
	Weight   *int `json:"weight"`
	Priority *int `json:"priority"`
}

// validText reports whether s is printable UTF-8 without control characters,
// which a name or model string must be to be safe in logs and in Postgres.
func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// routeFromInput validates the input against the store and returns the route to write.
func (a *Admin) routeFromInput(r *http.Request, projectID int64, in *routeInput) (*store.Route, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, routeBad("name is required")
	}
	if utf8.RuneCountInString(name) > store.MaxRouteNameLen {
		return nil, routeBadf("name must be at most %d characters", store.MaxRouteNameLen)
	}
	if !validText(name) {
		return nil, routeBad("name must not contain control characters")
	}
	kind := in.Kind
	if kind == "" {
		kind = store.RouteKindChat
	}
	if !store.IsValidRouteKind(kind) {
		return nil, routeBad(`kind must be "chat", "embeddings" or "rerank"`)
	}
	strategy := in.Strategy
	if strategy == "" {
		strategy = store.RouteStrategyWeighted
	}
	if strategy != store.RouteStrategyWeighted {
		return nil, routeBad(`strategy must be "weighted"`)
	}
	if len(in.Targets) == 0 {
		return nil, routeBad("a route needs at least one target")
	}
	if len(in.Targets) > store.MaxRouteTargets {
		return nil, routeBadf("a route has at most %d targets", store.MaxRouteTargets)
	}

	out := &store.Route{ProjectID: projectID, Name: name, Kind: kind, Strategy: strategy,
		Targets: make([]store.RouteTarget, 0, len(in.Targets))}
	conns := map[int64]*store.ModelConnection{}
	seen := map[string]bool{}
	for i, t := range in.Targets {
		conn, ok := conns[t.ConnectionID]
		if !ok {
			c, err := a.Store.GetConnection(r.Context(), t.ConnectionID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, routeBadf("targets[%d].connection_id does not reference an existing model connection", i)
				}
				return nil, err
			}
			conn, conns[t.ConnectionID] = c, c
		}
		if !providerSupportsKind(conn.ProviderType, kind) {
			return nil, routeBadf("targets[%d]: provider %q cannot serve a %s route", i, conn.ProviderType, kind)
		}

		weight, priority := 1, 0
		if t.Weight != nil {
			weight = *t.Weight
		}
		if t.Priority != nil {
			priority = *t.Priority
		}
		if weight < store.MinTargetWeight || weight > store.MaxTargetWeight {
			return nil, routeBadf("targets[%d].weight must be between %d and %d", i, store.MinTargetWeight, store.MaxTargetWeight)
		}
		if priority < 0 || priority > store.MaxTargetPriority {
			return nil, routeBadf("targets[%d].priority must be between 0 and %d", i, store.MaxTargetPriority)
		}

		var model *string
		if t.Model != nil {
			m := strings.TrimSpace(*t.Model)
			if m != "" {
				if utf8.RuneCountInString(m) > store.MaxRouteNameLen || !validText(m) {
					return nil, routeBadf("targets[%d].model is not a valid model name", i)
				}
				model = &m
			}
		}
		key := strconv.FormatInt(t.ConnectionID, 10) + "\x00"
		if model != nil {
			key += *model
		}
		if seen[key] {
			return nil, routeBadf("targets[%d] repeats an earlier connection and model pair", i)
		}
		seen[key] = true
		out.Targets = append(out.Targets, store.RouteTarget{ConnectionID: t.ConnectionID, Model: model,
			Weight: weight, Priority: priority})
	}
	return out, nil
}

// providerSupportsKind reports whether a provider type has an adapter for the
// endpoint family a route kind serves (registry: Provider, Embedder, Reranker).
func providerSupportsKind(providerType, kind string) bool {
	switch kind {
	case store.RouteKindChat:
		return provider.SupportsChat(providerType)
	case store.RouteKindEmbeddings:
		return provider.SupportsEmbeddings(providerType)
	case store.RouteKindRerank:
		return provider.SupportsRerank(providerType)
	}
	return false
}

// routeKindConflict returns a message when changing the connection to
// providerType would leave a route that uses it with a provider that cannot
// serve the route's kind (ADR-017: a target's provider must support the kind).
// The message is empty when the change is safe.
func (a *Admin) routeKindConflict(ctx context.Context, connectionID int64, providerType string) (string, error) {
	kinds, err := a.Store.ConnectionRouteKinds(ctx, connectionID)
	if err != nil {
		return "", err
	}
	for _, k := range kinds {
		if !providerSupportsKind(providerType, k) {
			return "provider type " + providerType + " cannot serve the " + k + " routes that use this connection; remove it from them first", nil
		}
	}
	return "", nil
}

func routeIDParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "routeId"), 10, 64)
}

// routeDetails is the audit payload of a route write: names and sizes only.
func routeDetails(rt *store.Route) map[string]any {
	return map[string]any{"project_id": rt.ProjectID, "name": rt.Name, "kind": rt.Kind, "targets": len(rt.Targets)}
}

func (a *Admin) listRoutes(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	list, err := a.Store.ListRoutes(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *Admin) getRoute(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	id, err := routeIDParam(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	rt, err := a.Store.GetRoute(r.Context(), p.ID, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rt)
}

func (a *Admin) createRoute(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	var in routeInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	rt, err := a.routeFromInput(r, p.ID, &in)
	if err != nil {
		a.routeInvalid(w, err)
		return
	}
	created, err := a.Store.CreateRoute(r.Context(), rt)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "route.create", "route", ptr(created.ID), routeDetails(created))
	writeJSON(w, http.StatusCreated, created)
}

func (a *Admin) updateRoute(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	id, err := routeIDParam(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if _, err := a.Store.GetRoute(r.Context(), p.ID, id); err != nil {
		a.fail(w, err)
		return
	}
	var in routeInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	rt, err := a.routeFromInput(r, p.ID, &in)
	if err != nil {
		a.routeInvalid(w, err)
		return
	}
	rt.ID = id
	updated, err := a.Store.UpdateRoute(r.Context(), rt)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "route.update", "route", ptr(updated.ID), routeDetails(updated))
	writeJSON(w, http.StatusOK, updated)
}

func (a *Admin) deleteRoute(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	id, err := routeIDParam(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	rt, err := a.Store.GetRoute(r.Context(), p.ID, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.DeleteRoute(r.Context(), p.ID, id); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "route.delete", "route", ptr(id), routeDetails(rt))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// routeInvalid answers a validation failure: a plain validation message is a
// 400, anything else (a store failure while checking a connection) goes
// through fail so SQL details never reach the client.
func (a *Admin) routeInvalid(w http.ResponseWriter, err error) {
	var verr routeValidationError
	if errors.As(err, &verr) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.fail(w, err)
}

// routeValidationError marks a validation message safe to show the caller.
type routeValidationError struct{ msg string }

func (e routeValidationError) Error() string { return e.msg }

func routeBad(msg string) error { return routeValidationError{msg: msg} }

func routeBadf(format string, args ...any) error {
	return routeValidationError{msg: fmt.Sprintf(format, args...)}
}
