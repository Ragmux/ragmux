package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// Turkish case folding on the two lexical search paths, measured against a
// live server.
//
// PostgreSQL's full-text search lowercases with the database's LC_CTYPE, so
// the same chunk can be findable on one deployment and not on another: the
// all-in-one image runs initdb with C.UTF-8, the split layout uses the
// official image's en_US.UTF-8. pg_search lowercases inside tantivy and
// ignores the locale entirely, but with a different Unicode rule. These tests
// state what each path is expected to do with Turkish words, and the CI job
// "locale" runs the full-text one against both images and diffs the
// lexemes each server produced.
//
// The expectations are not hopes, and they are not copied booleans either:
// turkishFoldCases lists only word pairs, and each column is computed here
// from the rule it names, with the same helpers internal/bm25/turkish_test.go
// checks letter by letter (test files cannot share code across packages, so
// the rules are repeated, not the answers). "simple" is the one-to-one mapping
// glibc's towlower applies in any non-Turkish UTF-8 locale, "full" adds
// İ → i + U+0307, which is what tantivy's lowercaser does, and "turkish" is
// the answer a Turkish reader expects. A row where the path's answer differs
// from the Turkish one is a known gap: the test asserts the path's answer, so
// a locale change in either direction turns it red, and logs the gap.
type turkishFoldCase struct {
	doc, query            string
	simple, full, turkish bool
}

// turkishFoldPairs keeps the rows of internal/bm25's turkishCases, in the same
// order; the two ISTANBUL/ISLAK rows at the end are the price of
// Turkish-correct folding.
var turkishFoldPairs = [][2]string{
	{"İSTANBUL", "istanbul"},
	{"İstanbul", "İSTANBUL"},
	{"İzmir", "izmir"},
	{"ŞİŞLİ", "şişli"},
	{"ıslak", "ıslak"},
	{"ISLAK", "ıslak"},
	{"IĞDIR", "ığdır"},
	{"IĞDIR", "Iğdır"},
	{"ISTANBUL", "istanbul"},
	{"ISLAK", "islak"},
}

var turkishFoldCases = func() []turkishFoldCase {
	out := make([]turkishFoldCase, len(turkishFoldPairs))
	for i, p := range turkishFoldPairs {
		out[i] = turkishFoldCase{doc: p[0], query: p[1],
			simple:  foldMatches(foldSimple, p[0], p[1]),
			full:    foldMatches(foldFull, p[0], p[1]),
			turkish: foldMatches(foldTurkish, p[0], p[1]),
		}
	}
	return out
}()

func foldSimple(s string) string { return strings.Map(unicode.ToLower, s) }

// foldFull is foldSimple plus İ → i + U+0307, Unicode's one unconditional
// multi-rune lowercase mapping.
func foldFull(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == 'İ' {
			b.WriteString("i\u0307")
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func foldTurkish(s string) string { return strings.Map(unicode.TurkishCase.ToLower, s) }

// foldMatches reports whether any query term equals any document term once
// both went through fold; terms are split on anything not a letter or digit,
// before folding, as both search paths do.
func foldMatches(fold func(string) string, doc, query string) bool {
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	}
	terms := map[string]bool{}
	for _, tok := range split(doc) {
		terms[fold(tok)] = true
	}
	for _, tok := range split(query) {
		if terms[fold(tok)] {
			return true
		}
	}
	return false
}

// TestTurkishFoldExpectations pins the computed table, so a change to a rule
// helper above cannot silently move what the live-server tests assert.
func TestTurkishFoldExpectations(t *testing.T) {
	want := []struct{ simple, full, turkish bool }{
		{true, false, true}, {true, true, true}, {true, false, true}, {true, false, true}, {true, true, true},
		{false, false, true}, {false, false, true}, {false, false, true}, {true, true, false}, {true, true, false},
	}
	if len(want) != len(turkishFoldCases) {
		t.Fatalf("%d cases, %d expectations", len(turkishFoldCases), len(want))
	}
	for i, c := range turkishFoldCases {
		if w := want[i]; c.simple != w.simple || c.full != w.full || c.turkish != w.turkish {
			t.Errorf("%q vs %q: simple/full/turkish = %v/%v/%v, want %v/%v/%v",
				c.doc, c.query, c.simple, c.full, c.turkish, w.simple, w.full, w.turkish)
		}
	}
}

// localeReportEnv names a file the full-text test writes its measurements to.
// The CI job sets it once per image and diffs the two files, so a difference
// between the images shows up even where both happen to pass the assertions.
const localeReportEnv = "RAGMUX_LOCALE_REPORT"

// seedTurkishChunks puts one chunk per case into a fresh rag store, chunk i
// holding turkishFoldCases[i].doc inside an otherwise neutral sentence. Every
// chunk has the same embedding, so the vector leg cannot tell them apart and
// the full-text rank is the only signal.
func seedTurkishChunks(t *testing.T, ctx context.Context, s *store.Store, backend string) *store.RAGStore {
	t.Helper()
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb-tr", ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "turkish", EmbeddingConnectionID: conn.ID,
		ChunkSize: 100, ChunkOverlap: 10, TopK: 3, SearchBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "tr.txt"}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	chunks := make([]*store.Chunk, len(turkishFoldCases))
	for i, c := range turkishFoldCases {
		chunks[i] = &store.Chunk{Index: i, Content: fmt.Sprintf("kayıt %d: %s", i, c.doc), Embedding: []float32{1, 0, 0, 0}}
	}
	if err := s.ReplaceDocumentChunks(ctx, doc, chunks, ""); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}
	return r
}

// lexicalHit reports whether chunk idx came back through the full-text leg.
func lexicalHit(hits []store.SearchHit, idx int) bool {
	for _, h := range hits {
		if h.Index == idx {
			return h.FTSRank > 0
		}
	}
	return false
}

func TestFullTextTurkishCaseFolding(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)

	conn, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })

	var version, ctype, provider string
	// PostgreSQL 16 dropped the lc_ctype setting; the database row is where
	// the locale lives now.
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version'), datctype,
		CASE datlocprovider WHEN 'c' THEN 'libc' WHEN 'i' THEN 'icu' WHEN 'b' THEN 'builtin' ELSE datlocprovider::text END
		FROM pg_database WHERE datname = current_database()`).Scan(&version, &ctype, &provider); err != nil {
		t.Fatalf("server info: %v", err)
	}
	t.Logf("server %s, lc_ctype %q, locale provider %q", version, ctype, provider)

	// The SQL the pgvector backend runs, reduced to the match: chunks.tsv is
	// to_tsvector('simple', content) and the query side is
	// websearch_to_tsquery with the store's fts_config, 'simple' by default.
	// plainto_tsquery is checked as well: it is the other parser an operator
	// is likely to reach for, and it must not disagree.
	for _, c := range turkishFoldCases {
		var web, plain bool
		if err := conn.QueryRow(ctx, `SELECT to_tsvector('simple', $1) @@ websearch_to_tsquery('simple', $2),
			to_tsvector('simple', $1) @@ plainto_tsquery('simple', $2)`, c.doc, c.query).Scan(&web, &plain); err != nil {
			t.Fatalf("%q vs %q: %v", c.doc, c.query, err)
		}
		if web != c.simple || plain != c.simple {
			t.Errorf("%q vs %q: websearch %v, plainto %v, want %v (Unicode simple lowercase, lc_ctype %q)",
				c.doc, c.query, web, plain, c.simple, ctype)
		}
		if c.simple != c.turkish {
			t.Logf("known gap: %q vs %q matches %v here, a Turkish reader expects %v", c.doc, c.query, c.simple, c.turkish)
		}
	}

	// The same through the real search path: the generated tsv column, its
	// GIN index and the hybrid query, so nothing above is a property of a
	// hand-written statement only.
	r := seedTurkishChunks(t, ctx, s, store.BackendPgvector)
	q := []float32{1, 0, 0, 0}
	for i, c := range turkishFoldCases {
		hits, err := s.Search(ctx, r.ID, c.query, q,
			store.SearchOptions{Mode: store.SearchHybrid, Candidates: len(turkishFoldCases)})
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		if got := lexicalHit(hits, i); got != c.simple {
			t.Errorf("search %q for chunk %q: full-text hit %v, want %v", c.query, c.doc, got, c.simple)
		}
	}

	writeLocaleReport(t, ctx, conn)
}

// writeLocaleReport records, for every word in the table, what this server
// made of it: the tsvector the chunk would be indexed under and lower(),
// which users.go relies on for case-insensitive usernames. The locale itself
// is left out on purpose -- it is expected to differ between the images, and
// the diff should only ever show behaviour.
func writeLocaleReport(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	path := os.Getenv(localeReportEnv)
	if path == "" {
		return
	}
	seen := map[string]bool{}
	var b strings.Builder
	b.WriteString("word\tto_tsvector('simple')\tlower()\tupper()\n")
	for _, c := range turkishFoldCases {
		for _, w := range []string{c.doc, c.query} {
			if seen[w] {
				continue
			}
			seen[w] = true
			var tsv, lower, upper string
			if err := conn.QueryRow(ctx, `SELECT to_tsvector('simple', $1)::text, lower($1), upper($1)`, w).
				Scan(&tsv, &lower, &upper); err != nil {
				t.Fatalf("report %q: %v", w, err)
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", w, tsv, lower, upper)
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestPgSearchTurkishCaseFolding is the BM25 side, and runs only where
// ParadeDB is. pg_search's "default" tokenizer lowercases with tantivy's
// LowerCaser, which applies Unicode's full mapping and ignores LC_CTYPE: İ
// becomes "i" + U+0307, so a dotted capital never meets the plain i a reader
// types. The "full" column states that. The stemming analysers are left out
// on purpose: a Turkish stemmer strips the copula -DIr, which would make
// "iğdir" and "iğdır" meet for a reason unrelated to case.
func TestPgSearchTurkishCaseFolding(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	testdb.RequirePgSearch(t, s)

	r := seedTurkishChunks(t, ctx, s, store.BackendPgSearch)
	q := []float32{1, 0, 0, 0}
	for i, c := range turkishFoldCases {
		hits, used, err := s.SearchWithBackend(ctx, r.ID, c.query, q,
			store.SearchOptions{Mode: store.SearchHybrid, Backend: store.BackendPgSearch, Candidates: len(turkishFoldCases)})
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		if used != store.BackendPgSearch {
			t.Fatalf("backend = %q, want pg_search", used)
		}
		if got := lexicalHit(hits, i); got != c.full {
			t.Errorf("search %q for chunk %q: BM25 hit %v, want %v (Unicode full lowercase)", c.query, c.doc, got, c.full)
		}
		if c.full != c.turkish {
			t.Logf("known gap: %q vs %q matches %v under BM25, a Turkish reader expects %v", c.doc, c.query, c.full, c.turkish)
		}
	}
}
