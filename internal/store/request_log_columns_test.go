package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// TestRequestedModelIsBounded checks that the client's model string cannot
// make a request log row arbitrarily large, and that a NUL byte in it does
// not cost the row.
func TestRequestedModelIsBounded(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}

	for id, model := range map[string]string{
		"long":  strings.Repeat("m", 1<<20),
		"multi": strings.Repeat("é", 1000),
		"nul":   "gpt\x00-4o",
		"plain": "gpt-4o",
		"empty": "",
	} {
		if err := s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200,
			RequestID: id, RequestedModel: model}); err != nil {
			t.Fatalf("%s: insert: %v", id, err)
		}
	}

	stored := func(id string) (string, bool) {
		var v *string
		if err := s.DB().QueryRow(ctx, "SELECT requested_model FROM request_logs WHERE request_id = $1", id).Scan(&v); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if v == nil {
			return "", false
		}
		return *v, true
	}
	if v, _ := stored("long"); len(v) != store.MaxRequestedModelBytes || strings.Trim(v, "m") != "" {
		t.Errorf("long model stored with %d bytes, want %d", len(v), store.MaxRequestedModelBytes)
	}
	if v, _ := stored("multi"); len(v) > store.MaxRequestedModelBytes || strings.Trim(v, "é") != "" {
		t.Errorf("multi-byte model stored with %d bytes or a split character", len(v))
	}
	if v, _ := stored("nul"); v != "gpt-4o" {
		t.Errorf("model with a NUL stored as %q, want %q", v, "gpt-4o")
	}
	if v, _ := stored("plain"); v != "gpt-4o" {
		t.Errorf("plain model stored as %q", v)
	}
	if _, present := stored("empty"); present {
		t.Error("an empty model should be stored as NULL")
	}
}
