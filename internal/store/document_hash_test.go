package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// TestDocumentContentHashMigrationsApply: 0025 adds the nullable column, 0026
// builds the per-store partial index outside a transaction.
func TestDocumentContentHashMigrationsApply(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	for _, v := range []int{25, 26} {
		if !migrationRecorded(t, s, v) {
			t.Errorf("migration %d is not recorded", v)
		}
	}
	var nullable string
	var valid *bool
	if err := s.DB().QueryRow(ctx, `SELECT
		(SELECT is_nullable FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'documents' AND column_name = 'content_sha256'),
		(SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('idx_documents_store_content_sha256'))`).Scan(&nullable, &valid); err != nil {
		t.Fatal(err)
	}
	if nullable != "YES" {
		t.Errorf("documents.content_sha256 is_nullable = %q, want YES", nullable)
	}
	if valid == nil || !*valid {
		t.Errorf("idx_documents_store_content_sha256: valid = %v", valid)
	}
}

// TestFindDuplicateDocuments covers the lookup rules: same store, ready, same
// hash, optionally the same file name; never itself, never an unhashed row.
func TestFindDuplicateDocuments(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "e", ProviderType: "custom_openai", BaseURL: "http://example.invalid/v1", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) int64 {
		r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: name, EmbeddingConnectionID: conn.ID})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	storeA, storeB := mk("a"), mk("b")
	add := func(storeID int64, file string, ready bool) *store.Document {
		d, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: storeID, Filename: file}, []byte("same bytes"))
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			// Ready with chunks, without going through an embedder.
			if _, err := s.DB().Exec(ctx, "UPDATE documents SET status='ready', chunk_count=1 WHERE id=$1", d.ID); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	donor := add(storeA, "a.md", true)
	add(storeB, "a.md", true) // another store: never a duplicate
	newer := add(storeA, "a.md", true)
	pending := add(storeA, "a.md", false)
	renamed := add(storeA, "b.md", false)

	ids := func(d *store.Document, sameName bool) []int64 {
		t.Helper()
		got, err := s.FindDuplicateDocuments(ctx, d, sameName, 5)
		if err != nil {
			t.Fatal(err)
		}
		out := []int64{}
		for _, g := range got {
			out = append(out, g.ID)
		}
		return out
	}
	same := func(got []int64, want ...int64) bool {
		return fmt.Sprint(got) == fmt.Sprint(append([]int64{}, want...))
	}
	if got := ids(pending, true); !same(got, newer.ID, donor.ID) {
		t.Errorf("same name: %v, want newest first [%d %d]", got, newer.ID, donor.ID)
	}
	if got := ids(renamed, false); !same(got, newer.ID, donor.ID) {
		t.Errorf("name ignored: %v, want [%d %d]", got, newer.ID, donor.ID)
	}
	if got := ids(renamed, true); len(got) != 0 {
		t.Errorf("renamed copy with name required: %v, want none", got)
	}
	if got := ids(donor, false); !same(got, newer.ID) {
		t.Errorf("a document is no duplicate of itself: %v, want [%d]", got, newer.ID)
	}
	if got, _ := s.FindDuplicateDocuments(ctx, pending, false, 1); len(got) != 1 || got[0].ID != newer.ID {
		t.Errorf("limit 1: %v, want the newest", got)
	}
	if _, err := s.DB().Exec(ctx, "UPDATE documents SET content_sha256 = NULL WHERE id = $1", pending.ID); err != nil {
		t.Fatal(err)
	}
	if got := ids(pending, false); len(got) != 0 {
		t.Errorf("a row without a hash has no duplicate: %v", got)
	}
}
