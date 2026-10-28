package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func TestValidateMetadata(t *testing.T) {
	long := strings.Repeat("x", 257)
	manyKeys := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`"k%d":1`, i)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	bigArray := "[" + strings.TrimSuffix(strings.Repeat("1,", 101), ",") + "]"
	// 32 keys of 256 bytes each: valid per key, over 8 KiB together.
	var fat []string
	for i := 0; i < 32; i++ {
		fat = append(fat, fmt.Sprintf(`"f%d":"%s"`, i, strings.Repeat("y", 256)))
	}

	valid := map[string]string{
		`{}`:                                       `{}`,
		` { "b": 1, "a": "x" } `:                   `{"a":"x","b":1}`,
		`{"lang":"tr","year":2026,"ok":true}`:      `{"lang":"tr","ok":true,"year":2026}`,
		`{"tags":["a","b"],"n":[1,2.5,false]}`:     `{"n":[1,2.5,false],"tags":["a","b"]}`,
		`{"empty":[]}`:                             `{"empty":[]}`,
		`{"a.b-c_d":"<&>"}`:                        `{"a.b-c_d":"<&>"}`,
		`{"k":"` + strings.Repeat("x", 256) + `"}`: `{"k":"` + strings.Repeat("x", 256) + `"}`,
		manyKeys(32):                               "", // checked below for validity only
	}
	for in, want := range valid {
		got, err := store.ValidateMetadata(json.RawMessage(in))
		if err != nil {
			t.Errorf("ValidateMetadata(%.60s) = %v, want ok", in, err)
			continue
		}
		if want != "" && string(got) != want {
			t.Errorf("ValidateMetadata(%.60s) = %s, want %s", in, got, want)
		}
	}

	invalid := map[string]string{
		"null":                     `null`,
		"array":                    `[]`,
		"string":                   `"x"`,
		"empty input":              ``,
		"trailing data":            `{} {}`,
		"truncated":                `{"a":`,
		"nested object":            `{"a":{"b":1}}`,
		"nested array":             `{"a":[[1]]}`,
		"object in array":          `{"a":[{"b":1}]}`,
		"null value":               `{"a":null}`,
		"null in array":            `{"a":[null]}`,
		"duplicate key":            `{"a":1,"a":2}`,
		"empty key":                `{"":1}`,
		"bad key char":             `{"a b":1}`,
		"slash key":                `{"a/b":1}`,
		"long key":                 `{"` + strings.Repeat("k", 65) + `":1}`,
		"long string":              `{"k":"` + long + `"}`,
		"long string in array":     `{"k":["` + long + `"]}`,
		"multibyte over the limit": `{"k":"` + strings.Repeat("é", 129) + `"}`,
		"NUL in string":            `{"k":"a\u0000b"}`,
		"too many keys":            manyKeys(33),
		"array too long":           `{"k":` + bigArray + `}`,
		"over 8 KiB":               "{" + strings.Join(fat, ",") + "}",
	}
	for name, in := range invalid {
		_, err := store.ValidateMetadata(json.RawMessage(in))
		if err == nil {
			t.Errorf("%s: want an error", name)
		} else if !errors.Is(err, store.ErrInvalidMetadata) {
			t.Errorf("%s: error %v does not wrap ErrInvalidMetadata", name, err)
		}
	}

	// An error never repeats a value back.
	_, err := store.ValidateMetadata(json.RawMessage(`{"k":"` + long + `"}`))
	if err == nil || strings.Contains(err.Error(), long) {
		t.Errorf("error echoes the value: %v", err)
	}
	if got := store.MetadataKeys(json.RawMessage(`{"b":1,"a":2}`)); strings.Join(got, ",") != "a,b" {
		t.Errorf("MetadataKeys = %v", got)
	}
}

func TestParseMetadataFilter(t *testing.T) {
	valid := []string{
		`{}`,
		`{"lang":"tr"}`,
		`{"year":2026,"draft":false,"lang":"tr"}`,
		`{"lang":{"in":["tr","en"]}}`,
		`{"y":{"in":[1,2.5,true,"x"]}}`,
		`{"lang":{"in":["a"]}}`,
		`{"n":1e308,"m":{"in":[1e-308,-2.5E+3,1e-40]}}`,
	}
	for _, in := range valid {
		if _, err := store.ParseMetadataFilter(json.RawMessage(in)); err != nil {
			t.Errorf("ParseMetadataFilter(%s) = %v, want ok", in, err)
		}
	}
	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = fmt.Sprint(i)
	}
	if _, err := store.ParseMetadataFilter(json.RawMessage(`{"k":{"in":[` + strings.Join(hundred, ",") + `]}}`)); err != nil {
		t.Errorf("100 values: %v", err)
	}

	invalid := map[string]string{
		"null":               `null`,
		"array":              `[]`,
		"string":             `"lang"`,
		"empty":              ``,
		"null value":         `{"a":null}`,
		"array value":        `{"a":["x"]}`,
		"nested object":      `{"a":{"b":1}}`,
		"range operator":     `{"a":{"gt":1}}`,
		"not operator":       `{"a":{"not":"x"}}`,
		"or operator":        `{"or":[{"a":1}]}`,
		"in with extra":      `{"a":{"in":["x"],"gt":1}}`,
		"in empty":           `{"a":{"in":[]}}`,
		"in not array":       `{"a":{"in":"x"}}`,
		"in nested":          `{"a":{"in":[["x"]]}}`,
		"in null element":    `{"a":{"in":[null]}}`,
		"in 101":             `{"a":{"in":[` + strings.Join(append(hundred, "100"), ",") + `]}}`,
		"empty operator":     `{"a":{}}`,
		"bad key":            `{"a b":1}`,
		"duplicate key":      `{"a":1,"a":2}`,
		"string too long":    `{"a":"` + strings.Repeat("x", 257) + `"}`,
		"exponent overflow":  `{"a":1e999999}`,
		"exponent 130000":    `{"a":1e130000}`,
		"exponent 309":       `{"a":1e309}`,
		"exponent -309":      `{"a":1e-309}`,
		"overflows float64":  `{"a":{"in":[9e308]}}`,
		"in exponent":        `{"a":{"in":[1,1e999999]}}`,
		"number too long":    `{"a":1` + strings.Repeat("0", 70) + `}`,
		"in string too long": `{"a":{"in":["` + strings.Repeat("x", 257) + `"]}}`,
	}
	for name, in := range invalid {
		_, err := store.ParseMetadataFilter(json.RawMessage(in))
		if err == nil {
			t.Errorf("%s: want an error", name)
		} else if !errors.Is(err, store.ErrInvalidMetadata) {
			t.Errorf("%s: error %v does not wrap ErrInvalidMetadata", name, err)
		}
	}

	var nilFilter *store.MetadataFilter
	empty, _ := store.ParseMetadataFilter(json.RawMessage(`{}`))
	if !nilFilter.Empty() || !empty.Empty() {
		t.Error("nil and {} filters must be empty")
	}
	f, _ := store.ParseMetadataFilter(json.RawMessage(`{"b":1,"a":2}`))
	if f.Empty() || strings.Join(f.Keys(), ",") != "a,b" {
		t.Errorf("keys = %v", f.Keys())
	}
}

// ---- search against a real database ----

// metaSeed describes one document of a seeded store.
type metaSeed struct {
	metadata string
	chunks   int
	// vec builds the embedding of chunk j; text its content.
	vec  func(j int) []float32
	text func(j int) string
}

// seedMetaStore creates a rag store (4-dimensional vectors) with one document
// per seed.
func seedMetaStore(t *testing.T, s *store.Store, name, backend string, docs []metaSeed) (*store.RAGStore, []int64) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb-" + name, ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: name, EmbeddingConnectionID: conn.ID, ChunkSize: 100,
		ChunkOverlap: 10, TopK: 10, SearchBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i, d := range docs {
		var meta json.RawMessage
		if d.metadata != "" {
			if meta, err = store.ValidateMetadata(json.RawMessage(d.metadata)); err != nil {
				t.Fatalf("seed metadata %s: %v", d.metadata, err)
			}
		}
		doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: fmt.Sprintf("doc%d.txt", i), Metadata: meta}, []byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		chunks := make([]*store.Chunk, d.chunks)
		for j := range chunks {
			chunks[j] = &store.Chunk{Index: j, Content: d.text(j), Embedding: d.vec(j)}
		}
		if err := s.ReplaceDocumentChunks(ctx, doc, chunks, ""); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.ID)
	}
	r, err = s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r, ids
}

// nearVec is close to the query (1,0,0,0); farVec is almost orthogonal to it.
func nearVec(j int) []float32 { return []float32{1, float32(j%7) * 0.01, 0, 0} }
func farVec(j int) []float32  { return []float32{0, 0, 1, float32(j) * 0.01} }

// langStore is the shape of the acceptance test: 20 documents of 10 chunks,
// one of them (5% of the chunks) tagged lang=tr and far from the query, the
// rest lang=en and near it.
func langStore(t *testing.T, s *store.Store, backend string) (*store.RAGStore, int64) {
	t.Helper()
	var docs []metaSeed
	for i := 0; i < 20; i++ {
		d := metaSeed{metadata: `{"lang":"en"}`, chunks: 10, vec: nearVec,
			text: func(j int) string { return fmt.Sprintf("kiwi kiwi kiwi english filler %d", j) }}
		if i == 7 {
			d = metaSeed{metadata: `{"lang":"tr","tags":["a","b"],"year":2026}`, chunks: 10, vec: farVec,
				text: func(j int) string { return fmt.Sprintf("kiwi turkish passage %d", j) }}
		}
		docs = append(docs, d)
	}
	r, ids := seedMetaStore(t, s, "lang-"+backend, backend, docs)
	return r, ids[7]
}

func mustFilter(t *testing.T, raw string) *store.MetadataFilter {
	t.Helper()
	f, err := store.ParseMetadataFilter(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("filter %s: %v", raw, err)
	}
	return f
}

func filteredSearchStaysFull(t *testing.T, backend string) {
	ctx := context.Background()
	s := testdb.Open(t)
	if backend == store.BackendPgSearch {
		testdb.RequirePgSearch(t, s)
	}
	r, trDoc := langStore(t, s, backend)
	q := []float32{1, 0, 0, 0}

	for _, mode := range []string{store.SearchVector, store.SearchHybrid} {
		opts := store.SearchOptions{Mode: mode, Backend: backend, Candidates: 10, Filter: mustFilter(t, `{"lang":"tr"}`)}
		hits, used, err := s.SearchWithBackend(ctx, r.ID, "kiwi", q, opts)
		if err != nil {
			t.Fatalf("%s/%s: %v", backend, mode, err)
		}
		if mode == store.SearchHybrid && used != backend {
			t.Fatalf("backend = %q, want %q (a fallback would hide the test)", used, backend)
		}
		// Only ~5% of the chunks pass the filter and they are the farthest
		// ones: a filter applied after the candidate lists were cut at
		// top-k would return none of them.
		if len(hits) != 10 {
			t.Fatalf("%s/%s: %d hits, want 10", backend, mode, len(hits))
		}
		for _, h := range hits {
			if h.DocumentID != trDoc {
				t.Errorf("%s/%s: hit from document %d, filter allows only %d", backend, mode, h.DocumentID, trDoc)
			}
		}
		if mode == store.SearchHybrid {
			// The lexical leg is filtered before its own LIMIT too: every
			// chunk contains "kiwi", the English ones say it three times and
			// outscore the Turkish ones, so a leg cut first would be empty.
			lexical := 0
			for _, h := range hits {
				if h.FTSRank > 0 {
					lexical++
				}
			}
			if lexical == 0 {
				t.Errorf("%s: the lexical leg returned nothing under the filter: %+v", backend, hits)
			}
		}
	}

	// No filter: the English chunks win.
	hits, err := s.Search(ctx, r.ID, "kiwi", q, store.SearchOptions{Mode: store.SearchVector, Backend: backend, Candidates: 10})
	if err != nil || len(hits) != 10 || hits[0].DocumentID == trDoc {
		t.Errorf("unfiltered: %v %+v", err, hits)
	}
}

func TestFilteredSearchStaysFullPgvector(t *testing.T) {
	filteredSearchStaysFull(t, store.BackendPgvector)
}

func TestFilteredSearchStaysFullPgSearch(t *testing.T) {
	filteredSearchStaysFull(t, store.BackendPgSearch)
}

// A pg_search store whose lexical backend fails answers from pgvector, and the
// filter goes with it.
func TestFilterSurvivesTheBackendFallback(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, trDoc := langStore(t, s, store.BackendPgSearch)
	defer store.SwapSearchBackend(store.BackendPgSearch, failingBackend{})()
	hits, used, err := s.SearchWithBackend(ctx, r.ID, "kiwi", []float32{1, 0, 0, 0}, store.SearchOptions{
		Mode: store.SearchHybrid, Backend: store.BackendPgSearch, Candidates: 10, Filter: mustFilter(t, `{"lang":"tr"}`)})
	if err != nil || used != store.BackendPgvector {
		t.Fatalf("fallback: used=%q err=%v", used, err)
	}
	if len(hits) != 10 {
		t.Fatalf("%d hits, want 10", len(hits))
	}
	for _, h := range hits {
		if h.DocumentID != trDoc {
			t.Errorf("the fallback dropped the filter: %+v", h)
		}
	}
}

type failingBackend struct{}

func (failingBackend) Name() string                                { return store.BackendPgSearch }
func (failingBackend) Available(store.Capabilities) bool           { return true }
func (failingBackend) Prepare(context.Context, *store.Store) error { return nil }
func (failingBackend) Invalidate(*store.Store)                     {}
func (failingBackend) HybridQuery(store.SearchParams) (string, []any) {
	return "SELECT 1 FROM chunks_without_a_bm25_index", nil
}

// randVecs returns n reproducible pseudo-random 4-dimensional vectors. Random
// directions, not separated clusters: HNSW is a graph, and a cluster the graph
// does not link to the query's neighbourhood cannot be reached by any scan.
// Interleaved random vectors are what a real corpus looks like to the index.
func randVecs(seed int64, n int) func(j int) []float32 {
	rng := rand.New(rand.NewSource(seed))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = []float32{float32(rng.NormFloat64()), float32(rng.NormFloat64()),
			float32(rng.NormFloat64()), float32(rng.NormFloat64())}
	}
	return func(j int) []float32 { return vecs[j] }
}

// The tables of one vector width are shared by every store of that width, so
// even an unfiltered search is filtered by rag_store_id after the index scan.
// A small store beside a big one must still get a full top-k.
func TestSmallStoreBesideBigStoreStillFillsTopK(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	text := func(prefix string) func(int) string {
		return func(j int) string { return fmt.Sprintf("%s chunk %d", prefix, j) }
	}
	seedMetaStore(t, s, "big", store.BackendPgvector,
		[]metaSeed{{chunks: 300, vec: randVecs(1, 300), text: text("big")}})
	r, _ := seedMetaStore(t, s, "small", store.BackendPgvector,
		[]metaSeed{{chunks: 20, vec: randVecs(2, 20), text: text("small")}})
	q := []float32{1, 0.5, -0.5, 0.25}

	for _, mode := range []string{store.SearchVector, store.SearchHybrid} {
		hits, err := s.Search(ctx, r.ID, "chunk", q, store.SearchOptions{Mode: mode, Candidates: 10})
		if err != nil || len(hits) != 10 {
			t.Errorf("%s: %d hits, err %v; want 10", mode, len(hits), err)
		}
	}

	// And through the HNSW index itself, forced: the planner would not pick
	// it for tables this small. The btree index on rag_store_id would let it
	// answer exactly without HNSW, so it is dropped inside the (rolled back)
	// transaction. ef_search = 40 leaves the small store (20 of 320 vectors)
	// about two of its own candidates; relaxed_order is what keeps the scan
	// going until ten survive the rag_store_id filter.
	force := []string{"DROP INDEX chunk_embeddings_4_store", "SET LOCAL enable_seqscan = off",
		"SET LOCAL enable_bitmapscan = off", "SET LOCAL hnsw.ef_search = 40"}
	opts := store.SearchOptions{Mode: store.SearchVector, Candidates: 10}
	hits, plan, err := s.SearchUnderSettings(ctx, r.ID, "", q, opts,
		append(force, "SET LOCAL hnsw.iterative_scan = relaxed_order")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "_hnsw") {
		t.Fatalf("the plan does not use the HNSW index, so this proves nothing:\n%s", plan)
	}
	if len(hits) != 10 {
		t.Errorf("through HNSW with iterative scan: %d hits, want 10\n%s", len(hits), plan)
	}
	// The control: the same statement without iterative scans comes up short,
	// which is the bug the setting exists to fix.
	hits, _, err = s.SearchUnderSettings(ctx, r.ID, "", q, opts, append(force, "SET LOCAL hnsw.iterative_scan = off")...)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) >= 10 {
		t.Errorf("without iterative scan the same search returned %d hits; the control no longer shows the problem", len(hits))
	}
}

func TestMetadataFilterMatching(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	text := func(j int) string { return fmt.Sprintf("passage %d", j) }
	one := func(meta string) metaSeed { return metaSeed{metadata: meta, chunks: 1, vec: nearVec, text: text} }
	r, ids := seedMetaStore(t, s, "match", store.BackendPgvector, []metaSeed{
		one(`{"lang":"tr","tags":["a","b"],"year":2026,"draft":false}`), // 0
		one(`{"lang":"en","tags":["c"],"year":"2026","draft":true}`),    // 1
		one(`{"lang":"de","year":2025.5}`),                              // 2
		one(``),                                                         // 3: no metadata
	})
	q := []float32{1, 0, 0, 0}
	search := func(filter string) map[int64]bool {
		t.Helper()
		hits, err := s.Search(ctx, r.ID, "", q, store.SearchOptions{Mode: store.SearchVector, Candidates: 10, Filter: mustFilter(t, filter)})
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		got := map[int64]bool{}
		for _, h := range hits {
			got[h.DocumentID] = true
		}
		return got
	}
	expect := func(filter string, want ...int) {
		t.Helper()
		got := search(filter)
		if len(got) != len(want) {
			t.Errorf("%s: got %d documents, want %v", filter, len(got), want)
			return
		}
		for _, w := range want {
			if !got[ids[w]] {
				t.Errorf("%s: document %d missing", filter, w)
			}
		}
	}

	expect(`{}`, 0, 1, 2, 3)
	expect(`{"lang":"tr"}`, 0)
	expect(`{"lang":{"in":["tr","en"]}}`, 0, 1)
	expect(`{"lang":{"in":["tr","en"]},"draft":true}`, 1)
	// A scalar filter matches an array-valued key when the array holds it.
	expect(`{"tags":"a"}`, 0)
	expect(`{"tags":"b"}`, 0)
	expect(`{"tags":"c"}`, 1)
	expect(`{"tags":{"in":["b","c"]}}`, 0, 1)
	expect(`{"tags":"z"}`)
	// Equality is JSON-type aware: the number 2026 is not the string "2026".
	expect(`{"year":2026}`, 0)
	expect(`{"year":"2026"}`, 1)
	expect(`{"year":2025.5}`, 2)
	expect(`{"draft":false}`, 0)
	expect(`{"draft":"false"}`)
	// Keys are ANDed.
	expect(`{"lang":"tr","year":"2026"}`)
	// An unknown key matches nothing, and a document without metadata only
	// ever passes an empty filter.
	expect(`{"nope":1}`)

	// Quote and backslash characters in a filter value are data, not syntax.
	expect(`{"lang":"tr\" } , {\"x\":\""}`)

	for filter, want := range map[string]bool{`{"lang":"tr"}`: true, `{"lang":"zz"}`: false, `{"tags":"c"}`: true} {
		got, err := s.HasMatchingDocuments(ctx, r.ID, mustFilter(t, filter))
		if err != nil || got != want {
			t.Errorf("HasMatchingDocuments(%s) = %v, %v; want %v", filter, got, err, want)
		}
	}
	// A filter is scoped to its store.
	other, _ := seedMetaStore(t, s, "other", store.BackendPgvector, []metaSeed{one(`{"lang":"zz"}`)})
	if got, _ := s.HasMatchingDocuments(ctx, r.ID, mustFilter(t, `{"lang":"zz"}`)); got {
		t.Error("a document of another store matched")
	}
	if got, _ := s.HasMatchingDocuments(ctx, other.ID, mustFilter(t, `{"lang":"zz"}`)); !got {
		t.Error("the store's own document did not match")
	}
}

func TestDocumentMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, ids := seedMetaStore(t, s, "rt", store.BackendPgvector, []metaSeed{
		{metadata: `{"lang":"tr"}`, chunks: 1, vec: nearVec, text: func(int) string { return "a" }},
		{chunks: 1, vec: nearVec, text: func(int) string { return "b" }},
	})
	d, err := s.GetDocument(ctx, ids[0])
	if err != nil || string(d.Metadata) != `{"lang": "tr"}` && string(d.Metadata) != `{"lang":"tr"}` {
		t.Errorf("stored metadata = %s, %v", d.Metadata, err)
	}
	if d, _ = s.GetDocument(ctx, ids[1]); strings.ReplaceAll(string(d.Metadata), " ", "") != `{}` {
		t.Errorf("default metadata = %s", d.Metadata)
	}
	docs, err := s.ListDocuments(ctx, r.ID)
	if err != nil || len(docs) != 2 || len(docs[0].Metadata) == 0 {
		t.Errorf("list: %v %+v", err, docs)
	}
	if _, err := s.SetDocumentMetadata(ctx, 999999, json.RawMessage(`{}`)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing document: %v", err)
	}
	m, err := s.DocumentMetadataByID(ctx, []int64{ids[0], ids[1], 999999})
	if err != nil || len(m) != 2 {
		t.Errorf("DocumentMetadataByID = %v, %v", m, err)
	}
}

// Changing a document's metadata is one row update: the next filtered search
// sees it and no chunk was rewritten.
func TestSetDocumentMetadataReachesTheNextSearchWithoutTouchingChunks(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, ids := seedMetaStore(t, s, "patch", store.BackendPgvector, []metaSeed{
		{metadata: `{"lang":"en"}`, chunks: 3, vec: nearVec, text: func(j int) string { return fmt.Sprintf("p %d", j) }},
		{metadata: `{"lang":"en"}`, chunks: 3, vec: nearVec, text: func(j int) string { return fmt.Sprintf("q %d", j) }},
	})
	q := []float32{1, 0, 0, 0}
	count := func(filter string) int {
		t.Helper()
		hits, err := s.Search(ctx, r.ID, "", q, store.SearchOptions{Mode: store.SearchVector, Candidates: 10, Filter: mustFilter(t, filter)})
		if err != nil {
			t.Fatal(err)
		}
		return len(hits)
	}
	chunkIDs := func() string {
		t.Helper()
		var out string
		if err := s.DB().QueryRow(ctx, `SELECT COALESCE(string_agg(id::text, ',' ORDER BY id), '') FROM chunks WHERE rag_store_id = $1`, r.ID).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := chunkIDs()
	if count(`{"lang":"tr"}`) != 0 || count(`{"lang":"en"}`) != 6 {
		t.Fatal("setup")
	}

	meta, err := store.ValidateMetadata(json.RawMessage(`{"lang":"tr"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDocumentMetadata(ctx, ids[0], meta); err != nil {
		t.Fatal(err)
	}
	if got := count(`{"lang":"tr"}`); got != 3 {
		t.Errorf("after the update lang=tr finds %d chunks, want 3", got)
	}
	if got := count(`{"lang":"en"}`); got != 3 {
		t.Errorf("after the update lang=en finds %d chunks, want 3", got)
	}
	if after := chunkIDs(); after != before {
		t.Errorf("chunks were rewritten:\nbefore %s\nafter  %s", before, after)
	}

	// Full replacement: the old keys are gone.
	empty, _ := store.ValidateMetadata(json.RawMessage(`{}`))
	if _, err := s.SetDocumentMetadata(ctx, ids[0], empty); err != nil {
		t.Fatal(err)
	}
	if got := count(`{"lang":"tr"}`); got != 0 {
		t.Errorf("after clearing, lang=tr still finds %d chunks", got)
	}
}

// padded returns doc with one more key, "pad", holding l characters of text
// spread over strings of at most 256 bytes.
func padded(doc string, l int) json.RawMessage {
	var parts []string
	for left := l; left > 0; left -= 256 {
		parts = append(parts, `"`+strings.Repeat("x", min(left, 256))+`"`)
	}
	pad := `"pad":[` + strings.Join(parts, ",") + `]`
	if doc == "{}" {
		return json.RawMessage("{" + pad + "}")
	}
	return json.RawMessage(strings.TrimSuffix(doc, "}") + "," + pad + "}")
}

// maxPad is the longest padding for which ValidateMetadata still accepts doc.
func maxPad(doc string) int {
	lo, hi := 0, 25600 // 100 strings of 256 is the array limit
	if _, err := store.ValidateMetadata(padded(doc, 0)); err != nil {
		return -1
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if _, err := store.ValidateMetadata(padded(doc, mid)); err == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// MaxMetadataBytes is measured on what the database hands back, not on what
// was submitted. For each document below the test finds, by bisection, the
// largest padding ValidateMetadata accepts and asks PostgreSQL itself how big
// the document prints at that padding and at one character more: the first
// must fit in 8 KiB and the second must not. Any difference between the Go
// accounting (numbers in full decimal form, ": " and ", " separators, string
// escapes) and the server's is a failure here, in either direction.
func TestRenderedSizeMatchesPostgreSQL(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	docs := []string{
		`{}`,
		`{"a":1}`,
		`{"a":1e308}`,
		`{"a":[1e308,1e308,1e308,1e308,1e308]}`,
		`{"a":1e-308}`,
		`{"a":[1e-308,-1e-308,1e-40]}`,
		`{"a":1.50,"b":0.00,"c":-0,"d":0e5,"e":0e-5,"f":-0.0}`,
		`{"a":1.5e3,"b":1.5e-3,"c":123456789012345678901234567890,"d":-1.5E+10,"e":1E5}`,
		`{"a":0.5e3,"b":0.05e1,"c":120e-2,"d":12.0e1,"e":100e-2,"f":1.7e308,"g":-1.7e308}`,
		`{"a":"plain","b":"quo\"te","c":"back\\slash","d":"line\nbreak\ttab","e":"\u0001\u001f","f":"\u007f"}`,
		`{"a":"é","b":"日本語","c":"\u2028\u2029","d":"emoji 😀","e":"\b\f\r"}`,
		`{"a":true,"b":false,"c":[true,false,1,"x",2.5]}`,
		`{"empty":[],"k.1-x_y":[0],"z":"z"}`,
	}
	for _, doc := range docs {
		l := maxPad(doc)
		if l < 0 {
			t.Fatalf("%s is refused even without padding", doc)
		}
		size := func(l int) int {
			t.Helper()
			var n int
			if err := s.DB().QueryRow(ctx, `SELECT octet_length($1::jsonb::text)`, []byte(padded(doc, l))).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		at, over := size(l), size(l+1)
		if at > store.MaxMetadataBytes {
			t.Errorf("%s: accepted with padding %d, but PostgreSQL prints %d bytes (> %d)", doc, l, at, store.MaxMetadataBytes)
		}
		if over <= store.MaxMetadataBytes {
			t.Errorf("%s: refused at padding %d, but PostgreSQL prints only %d bytes", doc, l+1, over)
		}
		// l+1 may be refused for another reason when the array is full.
		if l+1 <= 25600 {
			if _, err := store.ValidateMetadata(padded(doc, l+1)); err == nil {
				t.Errorf("%s: padding %d should be refused", doc, l+1)
			}
		}
	}
}

// The 8 KiB limit holds for what is read back. A small input full of "1e308"
// (309 bytes each when printed) is refused with a 400-class error; the same
// document one number short of the limit is accepted, written, and reads
// back within 8 KiB.
func TestMetadataLimitCountsWhatIsReadBack(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, ids := seedMetaStore(t, s, "limit", store.BackendPgvector, []metaSeed{
		{chunks: 1, vec: nearVec, text: func(int) string { return "n" }}})

	doc := func(n int, num string) string {
		return `{"a":[` + strings.TrimSuffix(strings.Repeat(num+",", n), ",") + `]}`
	}
	// 26 x 309 + separators is 8094 + 52 + 8 bytes: it fits. 27 does not.
	if _, err := store.ValidateMetadata(json.RawMessage(doc(27, "1e308"))); !errors.Is(err, store.ErrInvalidMetadata) {
		t.Errorf("27 x 1e308 (%d bytes submitted): err = %v, want ErrInvalidMetadata", len(doc(27, "1e308")), err)
	}
	if len(doc(27, "1e308")) > 200 {
		t.Errorf("the refused input is %d bytes; the point is that it is small", len(doc(27, "1e308")))
	}
	for _, num := range []string{"1e308", "1e-308"} {
		// Largest accepted count for this number.
		n := 1
		for {
			if _, err := store.ValidateMetadata(json.RawMessage(doc(n+1, num))); err != nil {
				break
			}
			n++
		}
		if n >= 27 {
			t.Fatalf("%s: %d values accepted", num, n)
		}
		meta, err := store.ValidateMetadata(json.RawMessage(doc(n, num)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetDocumentMetadata(ctx, ids[0], meta); err != nil {
			t.Fatalf("%s: the limit-sized document is refused by the database: %v", num, err)
		}
		d, err := s.GetDocument(ctx, ids[0])
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d values accepted (%d bytes submitted), %d bytes read back", num, n, len(meta), len(d.Metadata))
		if len(d.Metadata) > store.MaxMetadataBytes {
			t.Errorf("%s: reads back as %d bytes, over %d", num, len(d.Metadata), store.MaxMetadataBytes)
		}
		if len(d.Metadata) < store.MaxMetadataBytes-400 {
			t.Errorf("%s: reads back as %d bytes; the limit should be nearly reached", num, len(d.Metadata))
		}
		// One value more is refused.
		if _, err := store.ValidateMetadata(json.RawMessage(doc(n+1, num))); !errors.Is(err, store.ErrInvalidMetadata) {
			t.Errorf("%s: %d values: err = %v", num, n+1, err)
		}
	}
	// The extremes of the allowed range are usable in a filter, which is not
	// size-limited the same way (it is never stored or returned).
	for _, f := range []string{`{"a":1e-308}`, `{"a":{"in":[1e308,1.7e308,0.00000000000000000000000000000100]}}`} {
		if _, err := s.HasMatchingDocuments(ctx, r.ID, mustFilter(t, f)); err != nil {
			t.Errorf("filter %s: %v", f, err)
		}
	}
	if found, err := s.HasMatchingDocuments(ctx, r.ID, mustFilter(t, `{"a":1e-308}`)); err != nil || !found {
		t.Errorf("1e-308 filter: found=%v err=%v", found, err)
	}
}
