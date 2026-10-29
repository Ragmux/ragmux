package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// Per-store language (ADR-027): the mapping, the migrations, ingest, the
// search rule and the rebuild, against a live server.

// turkishTexts are the chunks of the language tests. Chunk 0 holds "kitap"
// (book); the query "kitaplarımız" (our books) reaches it only through the
// Turkish stemmer.
var turkishTexts = []string{
	"Bu kitap çok güzel bir eser",
	"Yarın hava yağmurlu olacak",
	"Bilgisayar ağı yapılandırması",
}

// seedLanguageStore creates a store with the given language and one document
// with one chunk per text. Every chunk has the same embedding, so the vector
// leg cannot tell them apart and only the full-text rank can.
func seedLanguageStore(t *testing.T, s *store.Store, name, language string, texts ...string) (*store.RAGStore, *store.Document) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb-" + name, ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: name, EmbeddingConnectionID: conn.ID,
		ChunkSize: 100, ChunkOverlap: 10, TopK: 3, Language: language})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: name + ".txt"}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) > 0 {
		replaceChunks(t, s, doc, texts...)
	}
	r, err = s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r, doc
}

func replaceChunks(t *testing.T, s *store.Store, doc *store.Document, texts ...string) {
	t.Helper()
	chunks := make([]*store.Chunk, len(texts))
	for i, text := range texts {
		chunks[i] = &store.Chunk{Index: i, Content: text, Embedding: []float32{1, 0, 0, 0}}
	}
	if err := s.ReplaceDocumentChunks(context.Background(), doc, chunks, ""); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}
}

var langQuery = []float32{1, 0, 0, 0}

// ftsHit searches in hybrid mode and reports whether chunk idx came back
// through the full-text leg.
func ftsHit(t *testing.T, s *store.Store, storeID int64, query string, idx int) bool {
	t.Helper()
	hits, err := s.Search(context.Background(), storeID, query, langQuery,
		store.SearchOptions{Mode: store.SearchHybrid, Candidates: 10})
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	if len(hits) == 0 {
		t.Fatalf("search %q returned nothing: the vector leg alone must find the chunks", query)
	}
	return lexicalHit(hits, idx)
}

func updateLanguage(t *testing.T, s *store.Store, r *store.RAGStore, language string) *store.RAGStore {
	t.Helper()
	cp := *r
	cp.Language, cp.FTSConfig = language, ""
	out, err := s.UpdateRAGStore(context.Background(), &cp)
	if err != nil {
		t.Fatalf("update language %q: %v", language, err)
	}
	return out
}

func TestStoreLanguageMigrationsApply(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	for _, v := range []int{27, 28} {
		if !migrationRecorded(t, s, v) {
			t.Errorf("migration %d is not recorded", v)
		}
	}
	var def, ftsNullable, ftsType string
	var valid *bool
	var generated string
	if err := s.DB().QueryRow(ctx, `SELECT
		(SELECT column_default FROM information_schema.columns WHERE table_schema = current_schema()
		   AND table_name = 'rag_stores' AND column_name = 'language'),
		(SELECT is_nullable FROM information_schema.columns WHERE table_schema = current_schema()
		   AND table_name = 'chunks' AND column_name = 'fts'),
		(SELECT udt_name FROM information_schema.columns WHERE table_schema = current_schema()
		   AND table_name = 'chunks' AND column_name = 'fts'),
		(SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('idx_chunks_fts')),
		(SELECT is_generated FROM information_schema.columns WHERE table_schema = current_schema()
		   AND table_name = 'chunks' AND column_name = 'tsv')`).Scan(&def, &ftsNullable, &ftsType, &valid, &generated); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "simple") {
		t.Errorf("rag_stores.language default = %q, want 'simple'", def)
	}
	if ftsNullable != "YES" || ftsType != "tsvector" {
		t.Errorf("chunks.fts nullable=%q type=%q, want YES tsvector", ftsNullable, ftsType)
	}
	if valid == nil || !*valid {
		t.Errorf("idx_chunks_fts: valid = %v", valid)
	}
	if generated != "ALWAYS" {
		t.Errorf("chunks.tsv is_generated = %q: the generated column must stay (ADR-001)", generated)
	}
	var partial string
	if err := s.DB().QueryRow(ctx, "SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'idx_chunks_fts'").Scan(&partial); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(partial, "gin") || !strings.Contains(partial, "fts IS NOT NULL") {
		t.Errorf("idx_chunks_fts = %q, want a partial GIN index", partial)
	}
}

func TestLanguageMappingExistsOnServer(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	for _, code := range store.LanguageCodes() {
		cfg, ok := store.LanguageConfig(code)
		if !ok {
			t.Errorf("language %q has no configuration", code)
			continue
		}
		if exists, err := s.TextSearchConfigExists(ctx, cfg); err != nil || !exists {
			t.Errorf("language %q maps to configuration %q, which this server lacks (err %v)", code, cfg, err)
		}
		if back, ok := store.LanguageForConfig(cfg); !ok || back != code {
			t.Errorf("configuration %q maps back to %q, want %q", cfg, back, code)
		}
	}
	if cfg, _ := store.LanguageConfig("tr"); cfg != "turkish" {
		t.Errorf("tr -> %q", cfg)
	}
	if cfg, _ := store.LanguageConfig("en"); cfg != "english" {
		t.Errorf("en -> %q", cfg)
	}
}

func TestResolveLanguage(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	ok := []struct {
		name, language, cfg, want string
		specified                 bool
	}{
		{"nothing", "", "", "", false},
		{"language", "tr", "", "tr", true},
		{"normalised", " TR ", "", "tr", true},
		{"simple", "simple", "", "simple", true},
		{"legacy english", "", "english", "en", true},
		{"legacy turkish", "", " Turkish ", "tr", true},
		{"legacy simple", "", "simple", "simple", true},
		{"both agree", "en", "english", "en", true},
	}
	for _, c := range ok {
		got, specified, err := s.ResolveLanguage(ctx, c.language, c.cfg)
		if err != nil || got != c.want || specified != c.specified {
			t.Errorf("%s: ResolveLanguage(%q, %q) = %q, %v, %v; want %q, %v, nil", c.name, c.language, c.cfg, got, specified, err, c.want, c.specified)
		}
	}
	bad := []struct{ name, language, cfg string }{
		{"unmapped code", "xx", ""},
		{"language name", "turkish", ""},
		{"unknown config", "", "klingon"},
		{"contradiction", "tr", "english"},
		{"contradiction with simple", "tr", "simple"},
	}
	for _, c := range bad {
		if _, _, err := s.ResolveLanguage(ctx, c.language, c.cfg); !errors.Is(err, store.ErrInvalidLanguage) {
			t.Errorf("%s: ResolveLanguage(%q, %q) err = %v, want ErrInvalidLanguage", c.name, c.language, c.cfg, err)
		}
	}
}

// A store created from an old client that only sends fts_config gets the
// language it names, and reads back both.
func TestLegacyFTSConfigBecomesLanguage(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "old", EmbeddingConnectionID: conn.ID, ChunkSize: 200, TopK: 3, FTSConfig: "english"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Language != "en" || r.FTSConfig != "english" {
		t.Fatalf("language/fts_config = %q/%q, want en/english", r.Language, r.FTSConfig)
	}
	def, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "default", EmbeddingConnectionID: conn.ID, ChunkSize: 200, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	if def.Language != "simple" || def.FTSConfig != "simple" || def.FTSStatus != store.FTSNotNeeded {
		t.Fatalf("default store: %+v", def)
	}
	// An update that names no language keeps the store's.
	r.FTSConfig = ""
	r.Language = ""
	r.Name = "renamed"
	r2, err := s.UpdateRAGStore(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Language != "en" || r2.Name != "renamed" {
		t.Errorf("an update without a language must keep it: %+v", r2)
	}
}

// Acceptance: a Turkish store finds "kitap" from "kitaplarımız", a 'simple'
// store does not.
func TestTurkishStoreStemsAndSimpleDoesNot(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	tr, _ := seedLanguageStore(t, s, "tr", "tr", turkishTexts...)
	simple, _ := seedLanguageStore(t, s, "simple", "simple", turkishTexts...)

	if tr.FTSStatus != store.FTSReady || tr.FTSLanguageApplied == nil || *tr.FTSLanguageApplied != "tr" {
		t.Fatalf("a store created with a language is complete at once: %+v", tr)
	}
	if !ftsHit(t, s, tr.ID, "kitaplarımız", 0) {
		t.Error("language=tr: \"kitaplarımız\" must reach the chunk with \"kitap\"")
	}
	if ftsHit(t, s, tr.ID, "kitaplarımız", 1) {
		t.Error("language=tr: \"kitaplarımız\" must not reach an unrelated chunk")
	}
	if ftsHit(t, s, simple.ID, "kitaplarımız", 0) {
		t.Error("language=simple: \"kitaplarımız\" must not reach \"kitap\"")
	}
	// The unstemmed word still works on both.
	if !ftsHit(t, s, simple.ID, "kitap", 0) || !ftsHit(t, s, tr.ID, "kitap", 0) {
		t.Error("the exact word must match on both stores")
	}

	// Ingest wrote chunks.fts for the Turkish store only.
	var trFTS, simpleFTS int
	if err := s.DB().QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE rag_store_id = $1 AND fts IS NOT NULL),
		count(*) FILTER (WHERE rag_store_id = $2 AND fts IS NOT NULL) FROM chunks`, tr.ID, simple.ID).Scan(&trFTS, &simpleFTS); err != nil {
		t.Fatal(err)
	}
	if trFTS != len(turkishTexts) || simpleFTS != 0 {
		t.Errorf("chunks with fts: tr=%d (want %d), simple=%d (want 0)", trFTS, len(turkishTexts), simpleFTS)
	}
	var mismatch int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE rag_store_id = $1 AND fts IS DISTINCT FROM to_tsvector('turkish', content)`, tr.ID).Scan(&mismatch); err != nil {
		t.Fatal(err)
	}
	if mismatch != 0 {
		t.Errorf("%d chunks carry an fts that is not to_tsvector('turkish', content)", mismatch)
	}
}

// Acceptance: en -> tr. While the rebuild runs the search answers with
// tsv/'simple' and does not fail; afterwards the Turkish stem matches and
// fts_language_applied = 'tr'.
func TestLanguageChangeRebuildsInBackground(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, _ := seedLanguageStore(t, s, "docs", "en", turkishTexts...)
	if r.FTSStatus != store.FTSReady {
		t.Fatalf("store: %+v", r)
	}
	if ftsHit(t, s, r.ID, "kitaplarımız", 0) {
		t.Fatal("english stemming must not reach \"kitap\"")
	}

	r = updateLanguage(t, s, r, "tr")
	if r.Language != "tr" || r.FTSStatus != store.FTSRebuilding || r.FTSLanguageApplied != nil {
		t.Fatalf("after the change: %+v", r)
	}
	var rawCfg string
	if err := s.DB().QueryRow(ctx, "SELECT fts_config FROM rag_stores WHERE id = $1", r.ID).Scan(&rawCfg); err != nil || rawCfg != "turkish" {
		t.Errorf("a language change must rewrite the stored fts_config: %q, %v", rawCfg, err)
	}
	ids, err := s.StoresNeedingFTSRebuild(ctx)
	if err != nil || !slices.Equal(ids, []int64{r.ID}) {
		t.Fatalf("StoresNeedingFTSRebuild = %v, %v", ids, err)
	}

	// Rebuilding: tsv/'simple', valid, no error. "kitap" is a plain word, the
	// stem is not.
	if ftsHit(t, s, r.ID, "kitaplarımız", 0) {
		t.Error("while rebuilding the search must use tsv/simple, which has no stems")
	}
	if !ftsHit(t, s, r.ID, "kitap", 0) {
		t.Error("while rebuilding the exact word must still match through tsv")
	}

	// A new document written mid-rebuild gets the new language at once.
	extra, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "extra.txt"}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	replaceChunks(t, s, extra, "Defterlerimiz masanın üstünde")
	var wrong int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE document_id = $1 AND fts IS DISTINCT FROM to_tsvector('turkish', content)`, extra.ID).Scan(&wrong); err != nil || wrong != 0 {
		t.Errorf("a chunk ingested mid-rebuild must carry the new language's fts: %d wrong, err %v", wrong, err)
	}

	// One batch of two: not done, still on tsv.
	res, err := s.RebuildFTSBatch(ctx, r.ID, 2)
	if err != nil || res.Done || res.Busy || res.Rows != 2 {
		t.Fatalf("first batch = %+v, %v", res, err)
	}
	if got, _ := s.GetRAGStore(ctx, r.ID); got.FTSStatus != store.FTSRebuilding {
		t.Fatalf("after one batch: %+v", got)
	}
	if ftsHit(t, s, r.ID, "kitaplarımız", 0) {
		t.Error("a half-built column must not be searched")
	}
	// Saving the store again with the same language neither restarts nor
	// finishes the rebuild.
	r = updateLanguage(t, s, r, "tr")
	if r.FTSStatus != store.FTSRebuilding {
		t.Fatalf("a save with the same language: %+v", r)
	}
	res, err = s.RebuildFTSBatch(ctx, r.ID, 2)
	if err != nil || res.Done || res.Rows != 2 {
		t.Fatalf("second batch = %+v, %v (the cursor must have survived the save)", res, err)
	}
	res, err = s.RebuildFTSBatch(ctx, r.ID, 2)
	if err != nil || !res.Done || res.Rows != 0 {
		t.Fatalf("last batch = %+v, %v", res, err)
	}

	got, err := s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FTSStatus != store.FTSReady || got.FTSLanguageApplied == nil || *got.FTSLanguageApplied != "tr" {
		t.Fatalf("after the rebuild: %+v", got)
	}
	if !ftsHit(t, s, r.ID, "kitaplarımız", 0) {
		t.Error("after the rebuild the Turkish stem must match")
	}
	var mismatch int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE rag_store_id = $1 AND fts IS DISTINCT FROM to_tsvector('turkish', content)`, r.ID).Scan(&mismatch); err != nil || mismatch != 0 {
		t.Errorf("%d chunks with a wrong fts after the rebuild, err %v", mismatch, err)
	}
	if ids, _ := s.StoresNeedingFTSRebuild(ctx); len(ids) != 0 {
		t.Errorf("nothing should be left to rebuild: %v", ids)
	}
	res, err = s.RebuildFTSBatch(ctx, r.ID, 2)
	if err != nil || !res.Done || res.Rows != 0 {
		t.Errorf("a batch on an applied store = %+v, %v", res, err)
	}
}

// A language change in the middle of a rebuild starts over with the new
// language: no chunk keeps the old one.
func TestLanguageChangeDuringRebuildRestarts(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, _ := seedLanguageStore(t, s, "docs", "en", turkishTexts...)
	r = updateLanguage(t, s, r, "tr")
	if res, err := s.RebuildFTSBatch(ctx, r.ID, 2); err != nil || res.Done || res.Rows != 2 {
		t.Fatalf("batch = %+v, %v", res, err)
	}
	r = updateLanguage(t, s, r, "de")
	if r.FTSStatus != store.FTSRebuilding {
		t.Fatalf("after the second change: %+v", r)
	}
	res, err := s.RebuildFTSBatch(ctx, r.ID, 10)
	if err != nil || !res.Done || res.Rows != len(turkishTexts) {
		t.Fatalf("restarted batch = %+v, %v (every chunk is rewritten from the start)", res, err)
	}
	var wrong int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE rag_store_id = $1 AND fts IS DISTINCT FROM to_tsvector('german', content)`, r.ID).Scan(&wrong); err != nil || wrong != 0 {
		t.Errorf("%d chunks not in German after the restart, err %v", wrong, err)
	}
}

// 'simple' needs no rebuild, in either direction of the change to it; a store
// with no chunks is complete at once whatever it is changed to.
func TestSimpleAndEmptyStoresNeedNoRebuild(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, _ := seedLanguageStore(t, s, "docs", "tr", turkishTexts...)
	r = updateLanguage(t, s, r, "simple")
	if r.Language != "simple" || r.FTSStatus != store.FTSNotNeeded || r.FTSLanguageApplied != nil {
		t.Fatalf("back to simple: %+v", r)
	}
	if ids, _ := s.StoresNeedingFTSRebuild(ctx); len(ids) != 0 {
		t.Errorf("no rebuild for a simple store: %v", ids)
	}
	if ftsHit(t, s, r.ID, "kitaplarımız", 0) || !ftsHit(t, s, r.ID, "kitap", 0) {
		t.Error("a simple store searches tsv")
	}
	// Back to Turkish: the chunks still hold their old fts, but the column is
	// rebuilt from the start rather than trusted.
	r = updateLanguage(t, s, r, "tr")
	if r.FTSStatus != store.FTSRebuilding {
		t.Fatalf("simple -> tr: %+v", r)
	}

	empty, _ := seedLanguageStore(t, s, "empty", "simple")
	empty = updateLanguage(t, s, empty, "tr")
	if empty.FTSStatus != store.FTSReady {
		t.Errorf("an empty store has nothing to rebuild: %+v", empty)
	}
}

// pg_search ignores the store language. A store that falls back from pg_search
// to pgvector searches with the language rule.
func TestPgSearchFallbackAppliesStoreLanguage(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, _ := seedLanguageStore(t, s, "docs", "tr", turkishTexts...)
	r.SearchBackend = store.BackendPgSearch
	r.Language, r.FTSConfig = "", ""
	r, err := s.UpdateRAGStore(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if r.LanguageAppliesToSearch || r.Language != "tr" {
		t.Fatalf("a pg_search store must report that its language does not apply: %+v", r)
	}
	b := &brokenBackend{}
	defer store.SwapSearchBackend(store.BackendPgSearch, b)()
	hits, used, err := s.SearchWithBackend(ctx, r.ID, "kitaplarımız", langQuery,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: r.SearchBackend, Candidates: 10})
	if err != nil || used != store.BackendPgvector {
		t.Fatalf("backend=%q err=%v", used, err)
	}
	if !lexicalHit(hits, 0) {
		t.Error("the pgvector fallback must apply the store's language")
	}
}

// On a real ParadeDB the BM25 leg is global: the store language does not
// reach it, so the Turkish stem does not match there.
func TestPgSearchIgnoresStoreLanguage(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	testdb.RequirePgSearch(t, s)
	r, _ := seedLanguageStore(t, s, "docs", "tr", turkishTexts...)
	r.SearchBackend = store.BackendPgSearch
	r.Language, r.FTSConfig = "", ""
	r, err := s.UpdateRAGStore(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if r.LanguageAppliesToSearch {
		t.Fatalf("language_applies_to_search must be false on pg_search: %+v", r)
	}
	hits, used, err := s.SearchWithBackend(ctx, r.ID, "kitaplarımız", langQuery,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: r.SearchBackend, Candidates: 10})
	if err != nil || used != store.BackendPgSearch {
		t.Fatalf("backend=%q err=%v", used, err)
	}
	if lexicalHit(hits, 0) {
		t.Error("PG_SEARCH_TOKENIZER decides BM25 matching; the store language must not stem it")
	}
	if hits, _, err := s.SearchWithBackend(ctx, r.ID, "kitap", langQuery,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: r.SearchBackend, Candidates: 10}); err != nil || !lexicalHit(hits, 0) {
		t.Errorf("the exact word must still match under BM25: %v", err)
	}
}

// Several rebuilders on one store (replicas, or a process and its successor)
// finish it exactly once, with the right configuration on every chunk.
func TestConcurrentRebuildersAgree(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	texts := make([]string, 40)
	for i := range texts {
		texts[i] = turkishTexts[i%len(turkishTexts)] + " " + strings.Repeat("kitaplar ", i%5)
	}
	r, _ := seedLanguageStore(t, s, "docs", "en", texts...)
	r = updateLanguage(t, s, r, "tr")

	errs := make(chan error, 4)
	for range 4 {
		go func() {
			for {
				res, err := s.RebuildFTSBatch(ctx, r.ID, 7)
				if err != nil {
					errs <- err
					return
				}
				if res.Done {
					errs <- nil
					return
				}
			}
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetRAGStore(ctx, r.ID)
	if err != nil || got.FTSStatus != store.FTSReady {
		t.Fatalf("after the rebuilders: %+v, %v", got, err)
	}
	var wrong int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE rag_store_id = $1 AND fts IS DISTINCT FROM to_tsvector('turkish', content)`, r.ID).Scan(&wrong); err != nil || wrong != 0 {
		t.Errorf("%d chunks with a wrong fts, err %v", wrong, err)
	}
}

// An operator who sets the language on purpose -- 'simple' included -- stops
// being warned about the fts_config a store saved before 0027 still carries;
// any other save leaves the warning alone.
func TestExplicitLanguageClearsLegacyFTSConfig(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, _ := seedLanguageStore(t, s, "old", "simple")
	setRaw := func() {
		t.Helper()
		if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET fts_config = 'turkish' WHERE id = $1", r.ID); err != nil {
			t.Fatal(err)
		}
	}
	raw := func() string {
		t.Helper()
		var cfg string
		if err := s.DB().QueryRow(ctx, "SELECT fts_config FROM rag_stores WHERE id = $1", r.ID).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	save := func(explicit bool) {
		t.Helper()
		cp := *r
		cp.Language, cp.FTSConfig, cp.LanguageExplicit = "simple", "", explicit
		if _, err := s.UpdateRAGStore(ctx, &cp); err != nil {
			t.Fatal(err)
		}
	}

	setRaw()
	save(false)
	if got := raw(); got != "turkish" {
		t.Errorf("a save that did not name the language rewrote fts_config to %q", got)
	}
	save(true)
	if got := raw(); got != "simple" {
		t.Errorf("naming the language 'simple' must clear the leftover, fts_config = %q", got)
	}
	if n, err := s.AuditRAGStores(ctx); err != nil || n.FTSMismatch != 0 {
		t.Errorf("audit after the acknowledgement: %+v, %v", n, err)
	}
}
