package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
	"github.com/ragmux/ragmux/internal/tracing"
)

// TestIngestFailureSpanCarriesNoFilename is the failing-ingest half of the
// tracing rule. TestSpansCarryNoSecrets covers a document that ingests
// cleanly; the leak lives on the other path, where the pipeline's error
// reaches span.RecordError on ingest.document and is exported verbatim as
// exception.message.
//
// The document is created in the store rather than uploaded, because the
// upload handler rejects an unsupported extension before ingestion ever
// sees it. That is a second line of defence, not the one under test: a
// document can also arrive from an older release, a restored backup or a
// future upload path, and the ingester must not export its filename either
// way.
func TestIngestFailureSpanCarriesNoFilename(t *testing.T) {
	// The secret is the extension, which is what the error message used to
	// quote back: fmt.Errorf("unsupported file type %q", filepath.Ext(name)).
	const secretExt = "SECRETEXT-hunter2-do-not-export"
	filename := "quarterly-payroll." + secretExt

	up := mockUpstream(t)
	defer up.Close()
	col := newCollector(t)
	tracer := tracing.New(tracing.Config{Endpoint: col.URL, ServiceName: "ragmux-test",
		SampleRatio: 1, Timeout: 5 * time.Second})
	e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, tracer: tracer})

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create connection: %v", conn)
	}
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit",
		"embedding_connection_id": int64(conn["id"].(float64)),
		"chunk_size":              200, "chunk_overlap": 0, "top_k": 1}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))

	doc, err := e.store.CreateDocument(context.Background(),
		&store.Document{RAGStoreID: storeID, Filename: filename, SizeBytes: 3}, []byte("abc"))
	if err != nil {
		t.Fatalf("create document: %v", err)
	}

	// The ingester polls; wait for it to give up on this one.
	deadline := time.Now().Add(20 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		d := e.call("GET", fmt.Sprintf("/admin/api/documents/%d", doc.ID), nil, "")
		if status, _ = d["status"].(string); status == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status != "failed" {
		t.Fatalf("document status is %q, want failed; the ingest error path was never taken", status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tracer.Shutdown(ctx); err != nil {
		t.Fatalf("flush spans: %v", err)
	}

	payload := col.payload()
	if payload == "" {
		t.Fatal("the collector received no spans at all; the test proves nothing")
	}
	// Not vacuous: the span whose RecordError carries the failure did arrive.
	if names := col.spanNames(); names["ingest.document"] == 0 {
		t.Fatalf("no ingest.document span was exported; got %v", names)
	}
	for _, secret := range []string{secretExt, filename} {
		if strings.Contains(payload, secret) {
			t.Errorf("a span carried %q; a filename must never reach a collector (PRD rule 10)", secret)
		}
	}
}
