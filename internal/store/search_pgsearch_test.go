package store

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The SQL builders are pure functions, so these tests need no PostgreSQL at
// all and therefore run in every CI job -- including the one whose server has
// no pg_search and can never execute the statement they check. That is the
// point: the query the gateway would send to ParadeDB is reviewed on every
// commit even where it cannot be run.

func goldenSQL(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return string(b)
}

func testParams() SearchParams {
	return SearchParams{VecTable: "chunk_embeddings_4", Vector: "VEC", StoreID: 7, Candidates: 5,
		MaxDistance: 0.5, FTSConfig: "english", Query: "zyxquux protocol"}
}

func TestPgSearchHybridQueryMatchesGolden(t *testing.T) {
	sql, args := pgSearchBackend{}.HybridQuery(testParams())
	if want := goldenSQL(t, "hybrid_pgsearch.sql"); sql != want {
		t.Errorf("pg_search hybrid SQL changed:\n--- got ---\n%s\n--- want ---\n%s", sql, want)
	}
	// $1 vector, $2 store id, $3 candidates, $4 max distance, $5 query.
	if len(args) != 5 {
		t.Fatalf("args = %d, want 5: %v", len(args), args)
	}
	if args[0] != "VEC" || args[1] != int64(7) || args[2] != 5 || args[3] != 0.5 {
		t.Errorf("args order: %v", args)
	}
	// The query goes to the database verbatim: paradedb.match tokenises and
	// OR-joins the terms itself, so the tsquery rewriting must not run here.
	if args[4] != "zyxquux protocol" {
		t.Errorf("query arg = %q, want the raw query (ftsQuery must not be applied)", args[4])
	}
	if got := ftsQuery("zyxquux protocol"); strings.Contains(sql, got) || args[4] == got {
		t.Errorf("ftsQuery output %q leaked into the pg_search path", got)
	}
}

func TestPgSearchHybridQueryBindsEverythingUserSupplied(t *testing.T) {
	p := testParams()
	p.StoreID = 4242
	p.Query = "'; DROP TABLE chunks; --"
	sql, args := pgSearchBackend{}.HybridQuery(p)
	// The store id is a bind parameter, never interpolated: the SQL must not
	// contain its digits anywhere.
	if strings.Contains(sql, strconv.FormatInt(p.StoreID, 10)) {
		t.Errorf("store id was interpolated into the SQL:\n%s", sql)
	}
	if strings.Contains(sql, "DROP TABLE") {
		t.Errorf("query text was interpolated into the SQL:\n%s", sql)
	}
	if args[1] != int64(4242) || args[4] != p.Query {
		t.Errorf("args: %v", args)
	}
	// The only interpolation is the embedding table, derived from a
	// validated integer dimension.
	if !strings.Contains(sql, p.VecTable) {
		t.Errorf("vector table missing from the SQL:\n%s", sql)
	}
}

func TestPgSearchHybridQueryIgnoresFTSConfig(t *testing.T) {
	p := testParams()
	p.FTSConfig = "turkish"
	sql, args := pgSearchBackend{}.HybridQuery(p)
	// The BM25 index is global and tokenised once, so a per-store text
	// search configuration has nothing to select. It must not appear in the
	// statement, nor be smuggled in as a bind parameter.
	if strings.Contains(sql, "turkish") || strings.Contains(sql, "fts_config") ||
		strings.Contains(sql, "websearch_to_tsquery") || strings.Contains(sql, "ts_rank_cd") {
		t.Errorf("fts config reached the pg_search SQL:\n%s", sql)
	}
	for _, a := range args {
		if fmt.Sprint(a) == "turkish" {
			t.Errorf("fts config passed as a bind parameter: %v", args)
		}
	}
}

func TestPgSearchHybridQueryBlankQueryDropsTheLexicalLeg(t *testing.T) {
	p := testParams()
	p.Query = "   "
	sql, args := pgSearchBackend{}.HybridQuery(p)
	if want := goldenSQL(t, "hybrid_pgsearch_blank.sql"); sql != want {
		t.Errorf("blank-query SQL changed:\n--- got ---\n%s\n--- want ---\n%s", sql, want)
	}
	if strings.Contains(sql, "paradedb.match") {
		t.Errorf("a blank query must not reach paradedb.match:\n%s", sql)
	}
	// No $5, so exactly four arguments; a fifth would fail the bind.
	if len(args) != 4 {
		t.Errorf("args = %d, want 4: %v", len(args), args)
	}
}

func TestPgvectorHybridQueryMatchesGolden(t *testing.T) {
	sql, args := pgvectorBackend{}.HybridQuery(testParams())
	if want := goldenSQL(t, "hybrid_pgvector.sql"); sql != want {
		t.Errorf("pgvector hybrid SQL changed:\n--- got ---\n%s\n--- want ---\n%s", sql, want)
	}
	if len(args) != 6 {
		t.Fatalf("args = %d, want 6: %v", len(args), args)
	}
	if args[4] != "english" || args[5] != "zyxquux or protocol" {
		t.Errorf("fts config and rewritten query: %v", args[4:])
	}
	if strings.Contains(sql, "paradedb") {
		t.Errorf("pgvector SQL must not mention paradedb:\n%s", sql)
	}
}

// Both backends must fill the same twelve columns in the same order, because
// Store.Search scans them with one fixed argument list.
func TestHybridQueriesShareTheColumnContract(t *testing.T) {
	cols := "m.chunk_id, c.document_id, c.idx, c.content, d.filename"
	for _, b := range []SearchBackend{pgvectorBackend{}, pgSearchBackend{}} {
		sql, _ := b.HybridQuery(testParams())
		if !strings.Contains(sql, cols) {
			t.Errorf("%s: leading columns changed:\n%s", b.Name(), sql)
		}
		if !strings.Contains(sql, "m.score, m.vrank, m.frank") {
			t.Errorf("%s: score and rank columns changed:\n%s", b.Name(), sql)
		}
	}
}

func TestBM25IndexDDLUsesTheConfiguredTokenizer(t *testing.T) {
	t.Setenv("PG_SEARCH_TOKENIZER", "")
	if got := pgSearchTokenizer(); got != "default" {
		t.Errorf("default tokenizer = %q", got)
	}
	if ddl := bm25IndexDDL(); !strings.Contains(ddl, `"tokenizer":{"type":"default"}`) ||
		!strings.Contains(ddl, "CREATE INDEX IF NOT EXISTS idx_chunks_bm25 ON chunks") ||
		!strings.Contains(ddl, `key_field = 'id'`) ||
		!strings.Contains(ddl, `"rag_store_id":{"fast":true}`) {
		t.Errorf("index DDL:\n%s", ddl)
	}
	t.Setenv("PG_SEARCH_TOKENIZER", "en_stem")
	if !strings.Contains(bm25IndexDDL(), `"type":"en_stem"`) {
		t.Errorf("tokenizer override ignored:\n%s", bm25IndexDDL())
	}
	// The tokenizer is interpolated into DDL, which cannot take a bind
	// parameter, so anything but a plain identifier falls back to the default.
	for _, bad := range []string{"default'} , x => '", "DROP TABLE", "en stem", ""} {
		t.Setenv("PG_SEARCH_TOKENIZER", bad)
		if got := pgSearchTokenizer(); got != "default" {
			t.Errorf("tokenizer %q was accepted as %q", bad, got)
		}
	}
}

func TestSearchBackendRegistry(t *testing.T) {
	if !IsValidSearchBackend(BackendPgvector) || !IsValidSearchBackend(BackendPgSearch) || IsValidSearchBackend("lucene") {
		t.Error("backend validity")
	}
	if !(pgvectorBackend{}).Available(Capabilities{}) {
		t.Error("pgvector must always be available")
	}
	if (pgSearchBackend{}).Available(Capabilities{}) {
		t.Error("pg_search must not be available without the extension")
	}
	if !(pgSearchBackend{}).Available(Capabilities{PgSearch: true}) {
		t.Error("pg_search must be available with the extension")
	}
}
