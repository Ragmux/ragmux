package gateway

import (
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ragmux/ragmux/internal/store"
)

// maxModelEchoLen caps how much of a client's model string the model_not_found
// message repeats, so a hostile value cannot inflate the response.
const maxModelEchoLen = 128

// randFloat is the gateway's source of randomness for weighted target
// selection: it returns a value in [0, 1). Gateway.Rand replaces it in tests
// (ADR-017).
type randFloat func() float64

// rng returns the configured random source, or the process-wide generator,
// which is safe for concurrent use.
func (g *Gateway) rng() randFloat {
	if g.Rand != nil {
		return g.Rand
	}
	return rand.Float64
}

// orderTargets returns the candidates of a route in the order they are tried
// (ADR-017): priority groups ascending, and inside a group a weighted random
// order without replacement, so a heavier target is likelier to come first
// but every target of the group still appears once. The groups are
// concatenated. targets must already be sorted by priority ascending; the
// input slice is not modified.
func orderTargets(targets []store.RouteTarget, rnd randFloat) []store.RouteTarget {
	out := make([]store.RouteTarget, 0, len(targets))
	for start := 0; start < len(targets); {
		end := start + 1
		for end < len(targets) && targets[end].Priority == targets[start].Priority {
			end++
		}
		group := append([]store.RouteTarget(nil), targets[start:end]...)
		for len(group) > 0 {
			total := 0
			for _, t := range group {
				total += t.Weight
			}
			pick := int(rnd() * float64(total))
			idx := len(group) - 1
			for i, t := range group {
				if pick < t.Weight {
					idx = i
					break
				}
				pick -= t.Weight
			}
			out = append(out, group[idx])
			group = append(group[:idx], group[idx+1:]...)
		}
		start = end
	}
	return out
}

// effectiveConnection is the connection a target is called through: a copy of
// the stored connection whose model name is the target's override when it has
// one. Provider construction, pricing, the request log and the span all read
// the copy, so the stored row is never touched (ADR-017).
func effectiveConnection(t store.RouteTarget) *store.ModelConnection {
	conn := *t.Connection
	if t.Model != nil && *t.Model != "" {
		conn.ModelName = *t.Model
	}
	return &conn
}

// resolveChatTargets turns the client's model string into the ordered
// candidates of the call (ADR-018). It reports whether the project routes at
// all; when it does not, the caller keeps the v0.4 behaviour.
//
// With chat routes defined the order is: a route whose name equals model; then
// the implicit route, which is the project's own connection and answers to its
// model name and to an empty model; otherwise no match. The route name is
// empty for the implicit route.
func (g *Gateway) resolveChatTargets(c *call, model string) (targets []*store.ModelConnection, routeName string, routed, matched bool, err error) {
	has, rt, err := g.Store.ResolveProjectRoute(c.ctx(), c.project.ID, store.RouteKindChat, model)
	if err != nil {
		return nil, "", has, false, err
	}
	if !has {
		return nil, "", false, false, nil
	}
	if rt != nil {
		for _, t := range orderTargets(rt.Targets, g.rng()) {
			targets = append(targets, effectiveConnection(t))
		}
		return targets, rt.Name, true, true, nil
	}
	conn, err := g.Store.GetConnection(c.ctx(), c.project.ModelConnectionID)
	if err != nil {
		return nil, "", true, false, err
	}
	if model == "" || model == conn.ModelName {
		return []*store.ModelConnection{conn}, "", true, true, nil
	}
	return nil, "", true, false, nil
}

// modelNotFound answers a model string no route or implicit route matches.
// It names the requested value but never lists the valid ones.
func modelNotFound(c *call, model string) {
	if utf8.RuneCountInString(model) > maxModelEchoLen {
		model = string([]rune(model)[:maxModelEchoLen])
	}
	writeErrorCode(c.w, http.StatusNotFound, "invalid_request_error", "model_not_found",
		"the model "+quoteModel(model)+" does not exist or is not available to this project")
}

// quoteModel wraps a client-controlled string for a message, dropping control
// characters so it cannot forge extra lines in a log or a terminal.
func quoteModel(s string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return "'" + clean + "'"
}

// routedModels lists the model ids a routed project answers to: the chat
// route names, then the implicit route's name unless a route already has it.
// The second result is false, and the list nil, when the project has no chat
// routes.
func (g *Gateway) routedModels(r *http.Request, p *store.Project) ([]map[string]any, bool, error) {
	ctx := r.Context()
	routes, err := g.Store.ListRoutes(ctx, p.ID)
	if err != nil {
		return nil, false, err
	}
	var out []map[string]any
	names := map[string]bool{}
	for _, rt := range routes {
		if rt.Kind != store.RouteKindChat {
			continue
		}
		names[rt.Name] = true
		out = append(out, map[string]any{"id": rt.Name, "object": "model", "created": parseCreated(rt.CreatedAt), "owned_by": "ragmux"})
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	conn, err := g.Store.GetConnection(ctx, p.ModelConnectionID)
	if err != nil {
		return nil, true, err
	}
	if !names[conn.ModelName] {
		out = append(out, modelObject(conn))
	}
	return out, true, nil
}

// parseCreated reads a store timestamp as a unix time, falling back to now.
func parseCreated(ts string) int64 {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.Unix()
		}
	}
	return time.Now().Unix()
}

// resolveKindRoute is the route resolution of the endpoints that never fall
// back to the project's chat connection (embeddings, rerank; ADR-018): a route
// of the given kind named model; else the implicit route, which is the
// connection pick returns for the project's RAG store and answers to that
// connection's model name and to an empty model; else model_not_found. pick
// returns nil when the store has no such connection. what names the
// connection in log lines.
//
// When it returns true, c.targets holds at least one connection and c.conn is
// the first of them. When it returns false it has answered the client.
func (g *Gateway) resolveKindRoute(c *call, kind, model, what string, pick func(*store.RAGStore) *int64) bool {
	has, rt, err := g.Store.ResolveProjectRoute(c.ctx(), c.project.ID, kind, model)
	if err != nil {
		c.log.Error("resolve route", "err", err)
		return noRoute(c)
	}
	if has && rt != nil {
		var targets []*store.ModelConnection
		for _, t := range orderTargets(rt.Targets, g.rng()) {
			targets = append(targets, effectiveConnection(t))
		}
		c.routeName = rt.Name
		return acceptTargets(c, targets)
	}
	if c.project.RAGStoreID == nil {
		modelNotFound(c, model)
		return false
	}
	rs, err := g.Store.GetRAGStore(c.ctx(), *c.project.RAGStoreID)
	if err != nil {
		c.log.Error("resolve route: rag store", "id", *c.project.RAGStoreID, "err", err)
		return noRoute(c)
	}
	id := pick(rs)
	if id == nil {
		modelNotFound(c, model)
		return false
	}
	conn, err := g.Store.GetConnection(c.ctx(), *id)
	if err != nil {
		c.log.Error("resolve route: "+what+" connection", "id", *id, "err", err)
		return noRoute(c)
	}
	if model != "" && model != conn.ModelName {
		modelNotFound(c, model)
		return false
	}
	return acceptTargets(c, []*store.ModelConnection{conn})
}
