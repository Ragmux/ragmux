package store

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

func filteredParams(t *testing.T) SearchParams {
	t.Helper()
	f, err := ParseMetadataFilter(json.RawMessage(`{"lang":{"in":["tr","en"]},"tags":"a'; DROP TABLE documents; --"}`))
	if err != nil {
		t.Fatal(err)
	}
	p := testParams()
	p.Filter = f
	return p
}

// The filter has to sit inside every candidate leg, before that leg's LIMIT.
// legBeforeLimit returns the text of the leg that starts at "name AS", up to
// the first LIMIT that ends it.
func legBeforeLimit(t *testing.T, sql, name string) string {
	t.Helper()
	i := strings.Index(sql, name+" AS")
	if i < 0 {
		t.Fatalf("no %s leg in:\n%s", name, sql)
	}
	rest := sql[i:]
	j := strings.Index(rest, "LIMIT $3")
	if j < 0 {
		t.Fatalf("leg %s has no LIMIT:\n%s", name, sql)
	}
	return rest[:j]
}

func TestFilteredHybridQueriesApplyTheFilterBeforeEveryLimit(t *testing.T) {
	p := filteredParams(t)
	for _, b := range []SearchBackend{pgvectorBackend{}, pgSearchBackend{}} {
		sql, args := b.HybridQuery(p)
		if v := legBeforeLimit(t, sql, "vc"); !strings.Contains(v, "metadata @> ANY") {
			t.Errorf("%s: the vector leg is not filtered before its LIMIT:\n%s", b.Name(), v)
		}
		if f := legBeforeLimit(t, sql, "f"); !strings.Contains(f, "metadata @> ANY") {
			t.Errorf("%s: the lexical leg is not filtered before its LIMIT:\n%s", b.Name(), f)
		}
		// Nothing the caller supplied is in the statement text; it is all bound.
		for _, leak := range []string{"DROP TABLE", "\"tr\"", "tags"} {
			if strings.Contains(sql, leak) {
				t.Errorf("%s: %q was interpolated into the SQL:\n%s", b.Name(), leak, sql)
			}
		}
		// The filter's arguments are the last ones, one text[] per key.
		n := len(args)
		for _, a := range args[n-2:] {
			docs, ok := a.([]string)
			if !ok || len(docs) == 0 {
				t.Fatalf("%s: filter argument %T %v", b.Name(), a, a)
			}
		}
		if !strings.Contains(sql, fmt.Sprintf("$%d::text[]::jsonb[]", n-1)) || !strings.Contains(sql, fmt.Sprintf("$%d::text[]::jsonb[]", n)) {
			t.Errorf("%s: placeholders do not line up with the %d arguments:\n%s", b.Name(), n, sql)
		}
	}
}

func TestFilteredQueriesMatchGolden(t *testing.T) {
	p := filteredParams(t)
	pgv, argsV := pgvectorBackend{}.HybridQuery(p)
	pgs, argsS := pgSearchBackend{}.HybridQuery(p)
	if want := goldenSQL(t, "hybrid_pgvector_filtered.sql"); pgv != want {
		t.Errorf("filtered pgvector SQL changed:\n--- got ---\n%s\n--- want ---\n%s", pgv, want)
	}
	if want := goldenSQL(t, "hybrid_pgsearch_filtered.sql"); pgs != want {
		t.Errorf("filtered pg_search SQL changed:\n--- got ---\n%s\n--- want ---\n%s", pgs, want)
	}
	if len(argsV) != 8 || len(argsS) != 7 {
		t.Errorf("argument counts: pgvector %d (want 8), pg_search %d (want 7)", len(argsV), len(argsS))
	}
	// The blank-query pg_search statement has no $5; the filter's arguments
	// must close the gap, or the bind fails.
	p.Query = "  "
	sql, args := pgSearchBackend{}.HybridQuery(p)
	if len(args) != 6 || strings.Contains(sql, "paradedb.match") || !strings.Contains(sql, "$5::text[]::jsonb[]") {
		t.Errorf("blank query with a filter: %d args\n%s", len(args), sql)
	}
}

func TestEmptyFilterChangesNothing(t *testing.T) {
	plain := testParams()
	empty := testParams()
	empty.Filter, _ = ParseMetadataFilter(json.RawMessage(`{}`))
	for _, b := range []SearchBackend{pgvectorBackend{}, pgSearchBackend{}} {
		a, aa := b.HybridQuery(plain)
		c, ca := b.HybridQuery(empty)
		if a != c || len(aa) != len(ca) {
			t.Errorf("%s: an empty filter changed the statement", b.Name())
		}
	}
}

func TestVectorOnlyQueryIsFilteredToo(t *testing.T) {
	r := &RAGStore{ID: 3, Dimensions: 4}
	p := filteredParams(t)
	sql, args := searchStatement(pgvectorBackend{}, r, "", []float32{1, 0, 0, 0},
		SearchOptions{Mode: SearchVector, Candidates: 5, Filter: p.Filter}, false)
	if v := legBeforeLimit(t, sql, "vc"); !strings.Contains(v, "metadata @> ANY") {
		t.Errorf("vector-only search is not filtered before its LIMIT:\n%s", sql)
	}
	if len(args) != 6 || !strings.Contains(sql, "$5::text[]::jsonb[]") || !strings.Contains(sql, "$6::text[]::jsonb[]") {
		t.Errorf("args %d\n%s", len(args), sql)
	}
	if strings.Contains(sql, "DROP TABLE") {
		t.Errorf("filter value interpolated:\n%s", sql)
	}
}

func TestPredicateEncodesBothShapesOfEveryValue(t *testing.T) {
	f, err := ParseMetadataFilter(json.RawMessage(`{"tags":{"in":["a",2,true]}}`))
	if err != nil {
		t.Fatal(err)
	}
	cond, args := f.predicate("d", 7)
	if cond != "d.metadata @> ANY($7::text[]::jsonb[])" || len(args) != 1 {
		t.Fatalf("predicate = %q %v", cond, args)
	}
	got := strings.Join(args[0].([]string), " | ")
	want := `{"tags":"a"} | {"tags":["a"]} | {"tags":2} | {"tags":[2]} | {"tags":true} | {"tags":[true]}`
	if got != want {
		t.Errorf("documents:\n got %s\nwant %s", got, want)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := map[string]bool{
		"0.8.0": true, "0.8.1": true, "0.8.7": true, "0.10.0": true, "1.0.0": true, "0.8": true, "0.8.0-dev": true,
		"0.7.4": false, "0.5.1": false, "": false, "garbage": false, "v0.8.0": false, "0.x.1": false,
	}
	for v, want := range cases {
		if got := versionAtLeast(v, 0, 8, 0); got != want {
			t.Errorf("versionAtLeast(%q, 0.8.0) = %v, want %v", v, got, want)
		}
		if got := (Capabilities{PgvectorVersion: v}).SupportsIterativeScan(); got != want {
			t.Errorf("SupportsIterativeScan(%q) = %v, want %v", v, got, want)
		}
	}
}

// Migration 0024 builds its index CONCURRENTLY, which PostgreSQL refuses
// inside a transaction: the file must carry the marker that makes migrate()
// run it outside one, hold exactly one statement, and name an index the runner
// can check for validity afterwards.
func TestDocumentMetadataIndexMigrationRunsOutsideATransaction(t *testing.T) {
	body, err := fs.ReadFile(migrationFS, "migrations/0024_document_metadata_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !hasNoTxMarker(body) {
		t.Error("0024 does not start with the no-transaction marker")
	}
	if got := concurrentIndexName(string(body)); got != "idx_documents_metadata" {
		t.Errorf("index name seen by the runner = %q", got)
	}
	code := sqlLineCommentRE.ReplaceAllString(string(body), "")
	if n := strings.Count(code, ";"); n != 1 {
		t.Errorf("0024 holds %d statements, want exactly one", n)
	}
	if !strings.Contains(code, "gin (metadata jsonb_path_ops)") {
		t.Errorf("0024 does not build a jsonb_path_ops GIN index:\n%s", code)
	}

	// 0023 is transactional DDL and must not carry the marker.
	add, err := fs.ReadFile(migrationFS, "migrations/0023_document_metadata.sql")
	if err != nil {
		t.Fatal(err)
	}
	if hasNoTxMarker(add) || !strings.Contains(string(add), "ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'") {
		t.Error("0023 is not the add-only metadata column")
	}
}
