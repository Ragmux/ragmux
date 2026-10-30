package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

func TestRetrieveEndpoint(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 1}, "")
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh.")
	if doc["_status"] != float64(202) {
		t.Fatalf("upload: %v", doc)
	}
	e.waitReady(int64(doc["id"].(float64)))
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID, "rag_store_id": storeID}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	pid := int64(proj["project"].(map[string]any)["id"].(float64))

	// A viewer, a member of the project, mints a key with the retrieve scope.
	// /v1 does not read the role: the scope decides.
	vw := e.call("POST", "/admin/api/users", map[string]any{"username": "reader", "password": "reader-pass-1",
		"role": "viewer", "project_ids": []int64{pid}}, "")
	if vw["_status"] != float64(201) {
		t.Fatalf("create viewer: %v", vw)
	}
	viewerSession := e.login("reader", "reader-pass-1")
	mint := func(scopes ...string) string {
		k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": strings.Join(scopes, "-"),
			"scopes": scopes, "project_ids": []int64{pid}, "default_project_id": pid}, viewerSession)
		if k["_status"] != float64(201) {
			t.Fatalf("viewer mints key %v: %v", scopes, k)
		}
		return k["api_key"].(string)
	}
	retrieveKey := mint("retrieve")

	res, hdr := e.callRaw("POST", "/v1/retrieve", map[string]any{"query": "tell me about banana"}, retrieveKey)
	if res["_status"] != float64(200) {
		t.Fatalf("viewer retrieve: %v", res)
	}
	hits, ok := res["hits"].([]any)
	if !ok || len(hits) != 1 {
		t.Fatalf("hits: %v", res["hits"])
	}
	hit := hits[0].(map[string]any)
	meta := hit["metadata"].(map[string]any)
	if !strings.Contains(hit["content"].(string), "Banana") || meta["filename"] != "fruits.md" || meta["section"] == nil {
		t.Errorf("hit: %v", hit)
	}
	if _, ok := hit["score"].(float64); !ok || hit["chunk_id"] == nil || hit["document_id"] == nil {
		t.Errorf("hit scores and ids: %v", hit)
	}
	if res["mode"] == "" || res["mode"] == nil || res["backend"] == nil || res["reranked"] != false {
		t.Errorf("mode and backend: %v", res)
	}
	if u, _ := res["usage"].(map[string]any); u == nil || u["prompt_tokens"].(float64) <= 0 {
		t.Errorf("usage: %v", res["usage"])
	}

	// top_k overrides the store's top_k within bounds.
	if wide := e.call("POST", "/v1/retrieve", map[string]any{"query": "fruit", "top_k": 3}, retrieveKey); len(wide["hits"].([]any)) < 2 {
		t.Errorf("top_k 3: %v", wide["hits"])
	}

	// The request is accounted like any other.
	row, found := e.logRow(hdr.Get("X-Request-Id"))
	if !found {
		t.Fatal("no request_logs row for the retrieve call")
	}
	if row.endpoint != "retrieve" || row.status != 200 || row.promptTokens <= 0 || row.modelName != "mock-model" {
		t.Errorf("log row: %+v", row)
	}
	var ragHits int
	var ragUsed bool
	if err := e.store.DB().QueryRow(t.Context(), `SELECT rag_used, rag_hits FROM request_logs WHERE request_id = $1`,
		hdr.Get("X-Request-Id")).Scan(&ragUsed, &ragHits); err != nil || !ragUsed || ragHits != 1 {
		t.Errorf("rag columns: used=%v hits=%d err=%v", ragUsed, ragHits, err)
	}

	// Scope: a key without retrieve is refused, so is one with only other scopes.
	for _, k := range []string{mint("chat"), mint("embeddings")} {
		if r := e.call("POST", "/v1/retrieve", map[string]any{"query": "banana"}, k); r["_status"] != float64(403) {
			t.Errorf("key without the retrieve scope: %v", r)
		}
	}
	// And a retrieve-only key does not open the other endpoints.
	if r := e.call("POST", "/v1/embeddings", map[string]any{"input": "x"}, retrieveKey); r["_status"] != float64(403) {
		t.Errorf("retrieve key on embeddings: %v", r)
	}

	// Validation.
	for name, body := range map[string]map[string]any{
		"no query":    {},
		"blank query": {"query": "   "},
		"negative k":  {"query": "x", "top_k": -1},
		"huge k":      {"query": "x", "top_k": 1000},
	} {
		if r := e.call("POST", "/v1/retrieve", body, retrieveKey); r["_status"] != float64(400) {
			t.Errorf("%s: want 400, got %v", name, r)
		}
	}
	if r := e.call("POST", "/v1/retrieve", map[string]any{"query": "x"}, "sk-proj-bogus"); r["_status"] != float64(401) {
		t.Errorf("bad key: %v", r)
	}

	// A project with no RAG store has nothing to search.
	bare := e.call("POST", "/admin/api/projects", map[string]any{"name": "bare", "model_connection_id": connID}, "")
	r := e.call("POST", "/v1/retrieve", map[string]any{"query": "banana"}, bare["api_key"].(string))
	if errObj, _ := r["error"].(map[string]any); r["_status"] != float64(400) || errObj["code"] != "no_rag_store" {
		t.Errorf("project without a store: %v", r)
	}
}

// An LLM rerank is a chat call on the project's chat connection. A retrieve-only
// key opens it, so its tokens are counted on the request row and in the
// response usage, not just the query's embedding tokens.
func TestRetrieveCountsRerankTokens(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh.")
	e.waitReady(int64(doc["id"].(float64)))
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID, "rag_store_id": storeID}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "ret", "scopes": []string{"retrieve"},
		"project_ids": []int64{pid}, "default_project_id": pid}, "")
	key := k["api_key"].(string)

	res, hdr := e.callRaw("POST", "/v1/retrieve", map[string]any{"query": "tell me about fruit"}, key)
	if res["_status"] != float64(200) || res["reranked"] != true {
		t.Fatalf("reranked retrieve: %v", res)
	}
	row, found := e.logRow(hdr.Get("X-Request-Id"))
	if !found {
		t.Fatal("no request_logs row")
	}
	if row.completionTokens <= 0 || !row.estimated {
		t.Errorf("rerank tokens missing from the row: %+v", row)
	}
	usage := res["usage"].(map[string]any)
	if int(usage["total_tokens"].(float64)) != row.promptTokens+row.completionTokens {
		t.Errorf("response usage %v does not match the row %+v", usage, row)
	}
}

// uploadWithMetadata is upload with the optional metadata form field.
func (e *env) uploadWithMetadata(storeID int64, name, content, metadata string) map[string]any {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(content))
	if metadata != "" {
		mw.WriteField("metadata", metadata)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/admin/api/rag-stores/%d/documents", e.srv.URL, storeID), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+e.session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out == nil {
		out = map[string]any{}
	}
	out["_status"] = float64(resp.StatusCode)
	return out
}

// TestMetadataFilterEndToEnd drives ADR-025 through the whole stack: metadata
// written by upload and PATCH, filters on /v1/retrieve, the admin search and
// the chat extension, and the documents' metadata on the hits.
func TestMetadataFilterEndToEnd(t *testing.T) {
	backend := mockUpstream(t)
	defer backend.Close()
	var embeds atomic.Int64
	up := countingEmbeddings(t, backend, &embeds)
	e := newEnv(t, testdb.Config(t))

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 5}, "")
	storeID := int64(rs["id"].(float64))

	// Three documents, each a fruit the keyword embedder knows: one tagged
	// "en" with array tags, one "tr", one with no metadata at all.
	ids := map[string]int64{}
	for name, d := range map[string]struct{ text, meta string }{
		"apple.md":  {"Apples are red and crunchy.", `{"lang":"en","tags":["fruit","red"],"year":2026}`},
		"banana.md": {"Bananas are yellow and soft.", `{"lang":"tr","tags":["fruit"]}`},
		"cherry.md": {"Cherries are small and sweet.", ``},
	} {
		doc := e.uploadWithMetadata(storeID, name, d.text, d.meta)
		if doc["_status"] != float64(202) {
			t.Fatalf("upload %s: %v", name, doc)
		}
		ids[name] = int64(doc["id"].(float64))
		e.waitReady(ids[name])
	}
	// The upload response and a later read both carry the metadata.
	got := e.call("GET", fmt.Sprintf("/admin/api/documents/%d", ids["apple.md"]), nil, "")
	if m, _ := got["metadata"].(map[string]any); m["lang"] != "en" || m["year"] != float64(2026) {
		t.Errorf("document read: metadata = %v", got["metadata"])
	}
	if m, ok := e.call("GET", fmt.Sprintf("/admin/api/documents/%d", ids["cherry.md"]), nil, "")["metadata"].(map[string]any); !ok || len(m) != 0 {
		t.Errorf("a document without metadata reads as {}: %v", m)
	}

	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID, "rag_store_id": storeID}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	key := proj["api_key"].(string)

	// docsOf runs one retrieval and returns the filenames hit.
	retrieve := func(filter any) (map[string]any, map[string]bool) {
		t.Helper()
		body := map[string]any{"query": "apple banana cherry fruit", "top_k": 10}
		if filter != nil {
			body["filter"] = filter
		}
		res := e.call("POST", "/v1/retrieve", body, key)
		files := map[string]bool{}
		if hits, ok := res["hits"].([]any); ok {
			for _, h := range hits {
				files[h.(map[string]any)["metadata"].(map[string]any)["filename"].(string)] = true
			}
		}
		return res, files
	}
	same := func(name string, got map[string]bool, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: hit files %v, want %v", name, got, want)
			return
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("%s: hit files %v, want %v", name, got, want)
			}
		}
	}

	res, files := retrieve(nil)
	same("no filter", files, "apple.md", "banana.md", "cherry.md")
	// document_metadata rides on every hit.
	for _, h := range res["hits"].([]any) {
		hit := h.(map[string]any)
		dm, ok := hit["document_metadata"].(map[string]any)
		if !ok {
			t.Fatalf("hit without document_metadata: %v", hit)
		}
		switch hit["metadata"].(map[string]any)["filename"] {
		case "apple.md":
			if dm["lang"] != "en" || dm["year"] != float64(2026) {
				t.Errorf("apple document_metadata = %v", dm)
			}
		case "cherry.md":
			if len(dm) != 0 {
				t.Errorf("cherry document_metadata = %v", dm)
			}
		}
	}

	_, files = retrieve(map[string]any{"lang": "tr"})
	same("lang=tr", files, "banana.md")
	_, files = retrieve(map[string]any{"lang": map[string]any{"in": []string{"tr", "en"}}})
	same("lang in [tr,en]", files, "apple.md", "banana.md")
	_, files = retrieve(map[string]any{"tags": "fruit"})
	same("tags=fruit (array value)", files, "apple.md", "banana.md")
	_, files = retrieve(map[string]any{"tags": "red"})
	same("tags=red", files, "apple.md")
	_, files = retrieve(map[string]any{"year": 2026})
	same("year=2026", files, "apple.md")
	_, files = retrieve(map[string]any{"year": "2026"})
	same("year=\"2026\" (a string is not the number)", files)
	_, files = retrieve(map[string]any{})
	same("empty filter", files, "apple.md", "banana.md", "cherry.md")

	// A filter nothing matches answers 200 with no hits, and the query is
	// never embedded.
	before := embeds.Load()
	res, files = retrieve(map[string]any{"lang": "zz"})
	same("no match", files)
	if res["_status"] != float64(200) || len(res["hits"].([]any)) != 0 || embeds.Load() != before {
		t.Errorf("no match: %v (embeds +%d)", res, embeds.Load()-before)
	}

	// Outside the grammar: 400.
	for name, raw := range map[string]string{
		"array value": `{"lang":["en"]}`, "range": `{"year":{"gt":1}}`, "not": `{"lang":{"not":"en"}}`, "or": `{"or":[{"lang":"en"}]}`,
		"string": `"lang"`, "null": `null`, "null value": `{"lang":null}`, "nested": `{"lang":{"a":{"b":1}}}`,
		"empty in": `{"lang":{"in":[]}}`, "bad key": `{"la ng":"en"}`, "list": `[1]`,
		"exponent overflow": `{"year":1e999999}`, "exponent 130000": `{"year":{"in":[1e130000]}}`,
	} {
		out := e.call("POST", "/v1/retrieve", json.RawMessage(`{"query":"apple","filter":`+raw+`}`), key)
		if out["_status"] != float64(400) {
			t.Errorf("%s: want 400, got %v", name, out)
		}
	}

	// PATCH replaces the metadata of one document and the very next filtered
	// search sees it.
	patch := e.call("PATCH", fmt.Sprintf("/admin/api/documents/%d", ids["banana.md"]),
		map[string]any{"metadata": map[string]any{"lang": "de", "tags": []string{"fruit", "yellow"}}}, "")
	if patch["_status"] != float64(200) || patch["metadata"].(map[string]any)["lang"] != "de" {
		t.Fatalf("patch: %v", patch)
	}
	_, files = retrieve(map[string]any{"lang": "tr"})
	same("after the patch, lang=tr", files)
	_, files = retrieve(map[string]any{"lang": "de"})
	same("after the patch, lang=de", files, "banana.md")
	_, files = retrieve(map[string]any{"tags": "yellow"})
	same("after the patch, tags=yellow", files, "banana.md")
	// {} clears it.
	if clr := e.call("PATCH", fmt.Sprintf("/admin/api/documents/%d", ids["banana.md"]), map[string]any{"metadata": map[string]any{}}, ""); clr["_status"] != float64(200) {
		t.Fatalf("clear: %v", clr)
	}
	_, files = retrieve(map[string]any{"lang": "de"})
	same("after clearing", files)

	// PATCH validation: same 400s as at upload; a missing document is a 404.
	for name, body := range map[string]any{
		"missing":      map[string]any{},
		"null":         map[string]any{"metadata": nil},
		"nested":       map[string]any{"metadata": map[string]any{"a": map[string]any{"b": 1}}},
		"null value":   map[string]any{"metadata": map[string]any{"a": nil}},
		"bad key":      map[string]any{"metadata": map[string]any{"a b": 1}},
		"long string":  map[string]any{"metadata": map[string]any{"a": strings.Repeat("x", 257)}},
		"array":        map[string]any{"metadata": []any{1}},
		"nested array": map[string]any{"metadata": map[string]any{"a": []any{[]any{1}}}},
	} {
		if out := e.call("PATCH", fmt.Sprintf("/admin/api/documents/%d", ids["apple.md"]), body, ""); out["_status"] != float64(400) {
			t.Errorf("patch %s: want 400, got %v", name, out)
		}
	}
	// Numbers with a runaway exponent are a 400, not a 500 and not a response
	// that grows by orders of magnitude when read back.
	for _, raw := range []string{`1e999999`, `1e130000`, `[1,1e309]`} {
		req, _ := http.NewRequest("PATCH", fmt.Sprintf("%s/admin/api/documents/%d", e.srv.URL, ids["apple.md"]),
			strings.NewReader(`{"metadata":{"a":`+raw+`}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.session)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("patch with %s: status %d, want 400", raw, resp.StatusCode)
		}
	}
	// The 8 KiB limit counts what is read back: a number is stored in full, so
	// "1e308" is 309 bytes. 27 of them (a request of ~170 bytes) are refused;
	// 26 are accepted and the document reads back within 8 KiB.
	numbers := func(n int) string { return `[` + strings.TrimSuffix(strings.Repeat("1e308,", n), ",") + `]` }
	patchRaw := func(id int64, body string) int {
		t.Helper()
		req, _ := http.NewRequest("PATCH", fmt.Sprintf("%s/admin/api/documents/%d", e.srv.URL, id), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.session)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := patchRaw(ids["banana.md"], `{"metadata":{"a":`+numbers(27)+`}}`); st != http.StatusBadRequest {
		t.Errorf("patch with 27 x 1e308: status %d, want 400", st)
	}
	if st := patchRaw(ids["banana.md"], `{"metadata":{"a":`+numbers(26)+`}}`); st != http.StatusOK {
		t.Errorf("patch with 26 x 1e308: status %d, want 200", st)
	}
	var readBack int
	if err := e.store.DB().QueryRow(t.Context(), `SELECT octet_length(metadata::text) FROM documents WHERE id = $1`,
		ids["banana.md"]).Scan(&readBack); err != nil || readBack > 8192 || readBack < 8000 {
		t.Errorf("a document at the limit reads back as %d bytes (err %v), want 8000..8192", readBack, err)
	}
	if out := e.call("PATCH", "/admin/api/documents/999999", map[string]any{"metadata": map[string]any{}}, ""); out["_status"] != float64(404) {
		t.Errorf("patch of a missing document: %v", out)
	}
	_, files = retrieve(map[string]any{"lang": "en"})
	same("apple untouched by the rejected patches", files, "apple.md")

	// Upload: a bad metadata field is refused before anything is written.
	docsBefore := len(e.call("GET", fmt.Sprintf("/admin/api/rag-stores/%d/documents", storeID), nil, "")["_list"].([]any))
	for _, bad := range []string{`{"a":`, `[1]`, `{"a":{"b":1}}`, `{"a b":1}`, `{"a":1e999999}`, `{"a":[1e130000]}`, `{"a":` + numbers(27) + `}`} {
		if out := e.uploadWithMetadata(storeID, "durian.md", "Durian is spiky.", bad); out["_status"] != float64(400) {
			t.Errorf("upload with metadata %s: %v", bad, out)
		}
	}
	if n := len(e.call("GET", fmt.Sprintf("/admin/api/rag-stores/%d/documents", storeID), nil, "")["_list"].([]any)); n != docsBefore {
		t.Errorf("a refused upload left %d documents behind", n-docsBefore)
	}

	// The audit trail names the keys, never the values.
	var details string
	if err := e.store.DB().QueryRow(t.Context(), `SELECT details::text FROM audit_logs
		WHERE action = 'document.metadata_update' ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if !strings.Contains(details, "metadata_keys") || strings.Contains(details, "yellow") || strings.Contains(details, "de\"") {
		t.Errorf("audit details = %s", details)
	}
	if err := e.store.DB().QueryRow(t.Context(), `SELECT details::text FROM audit_logs
		WHERE action = 'document.upload' AND details->>'filename' = 'apple.md'`).Scan(&details); err != nil {
		t.Fatalf("upload audit row: %v", err)
	}
	if !strings.Contains(details, `"metadata_keys"`) || !strings.Contains(details, "lang") || strings.Contains(details, "fruit") {
		t.Errorf("upload audit details = %s", details)
	}

	// The admin search panel takes the same filter.
	search := func(filter any) map[string]any {
		t.Helper()
		return e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", storeID),
			map[string]any{"query": "apple banana cherry fruit", "top_k": 10, "filter": filter}, "")
	}
	out := search(map[string]any{"lang": "en"})
	if hits, _ := out["hits"].([]any); out["_status"] != float64(200) || len(hits) != 1 ||
		hits[0].(map[string]any)["filename"] != "apple.md" {
		t.Errorf("admin search with a filter: %v", out)
	}
	if out := search(map[string]any{"lang": []string{"en"}}); out["_status"] != float64(400) {
		t.Errorf("admin search with a bad filter: %v", out)
	}

	// Chat: ragmux.filter narrows what reaches the model.
	msgs := []map[string]any{{"role": "user", "content": "apple banana cherry fruit"}}
	sourcesOf := func(ragmux map[string]any) (map[string]any, http.Header) {
		t.Helper()
		out, h := e.callRaw("POST", "/v1/chat/completions", map[string]any{"messages": msgs, "ragmux": ragmux}, key)
		return out, h
	}
	chatFiles := func(out map[string]any) map[string]bool {
		files := map[string]bool{}
		rm, _ := out["ragmux"].(map[string]any)
		l, _ := rm["sources"].([]any)
		for _, s := range l {
			files[s.(map[string]any)["filename"].(string)] = true
		}
		return files
	}
	out, _ = sourcesOf(map[string]any{"sources": "inline"})
	same("chat, no filter", chatFiles(out), "apple.md", "banana.md", "cherry.md")
	out, h := sourcesOf(map[string]any{"sources": "inline", "filter": map[string]any{"lang": "en"}})
	same("chat, lang=en", chatFiles(out), "apple.md")
	if out["_status"] != float64(200) || len(h.Values("Warning")) != 0 {
		t.Errorf("chat, lang=en: %v Warning %q", out["_status"], h.Values("Warning"))
	}
	// Chat sources carry no metadata.
	for _, s := range out["ragmux"].(map[string]any)["sources"].([]any) {
		if _, has := s.(map[string]any)["document_metadata"]; has {
			t.Errorf("a chat source carries metadata: %v", s)
		}
	}
	// A filter outside the grammar is the 400 of ADR-024, before any embedding.
	before = embeds.Load()
	for _, raw := range []string{`{"lang":["en"]}`, `{"year":{"gt":1}}`, `"x"`, `null`, `[]`, `{"year":1e999999}`, `{"year":{"in":[1e130000]}}`} {
		out, _ := e.callRaw("POST", "/v1/chat/completions", json.RawMessage(
			`{"messages":[{"role":"user","content":"apple"}],"ragmux":{"filter":`+raw+`}}`), key)
		errObj, _ := out["error"].(map[string]any)
		if out["_status"] != float64(400) || errObj["code"] != "invalid_ragmux" || embeds.Load() != before {
			t.Errorf("ragmux.filter=%s: %v (embeds +%d)", raw, out, embeds.Load()-before)
		}
	}
	// A filter on a request whose retrieval is switched off narrows nothing:
	// a Warning says so.
	_, h = sourcesOf(map[string]any{"retrieval": "off", "filter": map[string]any{"lang": "en"}})
	if w := h.Values("Warning"); len(w) != 1 || !strings.Contains(w[0], "ragmux.filter has no effect") {
		t.Errorf("filter with retrieval off: Warning %q", w)
	}
	// So does one on a project without a store.
	bare := e.call("POST", "/admin/api/projects", map[string]any{"name": "bare", "model_connection_id": connID}, "")
	out2, h2 := e.callRaw("POST", "/v1/chat/completions", map[string]any{"messages": msgs,
		"ragmux": map[string]any{"filter": map[string]any{"lang": "en"}}}, bare["api_key"].(string))
	if w := h2.Values("Warning"); out2["_status"] != float64(200) || len(w) != 1 || !strings.Contains(w[0], "ragmux.filter has no effect") {
		t.Errorf("filter without a store: %v Warning %q", out2["_status"], w)
	}
}
