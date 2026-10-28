package admin

// Docs drift test: the JSON field names the admin API sends and accepts must
// match what docs/api.md says it sends and accepts.
//
// Method. The code side is read by reflection over the compiled struct tags
// of the types the handlers serialise (embedded structs, `json:"-"` and
// omitempty included), so a tag edit, an added field or a renamed field shows
// up without any source parsing. Reflection cannot reach a struct declared
// inside a handler (`var in struct{...}`, the `me` response), so those few are
// read with go/parser from the handler's body instead. The reason for
// reflection first: it sees what encoding/json sees, including promoted
// fields of embedded types, which a syntax-level walk would have to
// re-implement.
//
// The docs side is read from the markdown: the keys of one JSON example
// object (selected by path and an anchor key), or a `{a, b, c?}` shorthand
// list. Two directions are checked for every case:
//
//   - code -> docs: every field must be a key of the example/shorthand. A
//     field that exists only in prose (a backtick span of the section) must be
//     listed in the case's prose list; a bare backtick mention never counts,
//     because names such as `model` or `status` are mentioned in a section
//     for other reasons and would hide a new field of the same name.
//   - docs -> code: every key of the example/shorthand must be a field.
//
// Fields deliberately left out of the docs, and example keys that are not
// fields, go in the explicit exception maps of each case, each with a reason.
// An exception or prose entry that no longer applies fails the test, so the
// lists cannot rot.
//
// Not covered: responses assembled from map[string]any literals (setup
// status, metrics summary envelope, audit envelope, security/logins, test
// results). They have no struct tags to compare.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

const apiDocPath = "../../docs/api.md"

// ---- code side ----------------------------------------------------------

// jsonFieldNames lists the JSON object keys encoding/json produces or reads
// for t, following embedded structs.
func jsonFieldNames(t reflect.Type) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, hasTag := f.Tag.Lookup("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" && tag == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, jsonFieldNames(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if !hasTag || name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// handlerStructFields lists the JSON keys of every struct type literal inside
// the named function of file, for payloads declared inline in a handler.
func handlerStructFields(t *testing.T, file, fn string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		found = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				if fld.Tag == nil {
					continue
				}
				tag, err := strconv.Unquote(fld.Tag.Value)
				if err != nil {
					continue
				}
				v, ok := reflect.StructTag(tag).Lookup("json")
				if !ok {
					continue
				}
				name, _, _ := strings.Cut(v, ",")
				if name != "" && name != "-" {
					out = append(out, name)
				}
			}
			return true
		})
	}
	if !found {
		t.Fatalf("function %s not found in %s", fn, file)
	}
	if len(out) == 0 {
		t.Fatalf("function %s in %s declares no tagged struct", fn, file)
	}
	return out
}

// ---- docs side ----------------------------------------------------------

type docSection struct {
	level int
	title string
	body  string // everything up to the next heading of the same or higher level
}

var headingRe = regexp.MustCompile(`^(#{1,6}) +(.+?) *$`)

// parseSections splits markdown into heading-delimited sections. Lines inside
// fenced code blocks are never headings.
func parseSections(md string) []docSection {
	lines := strings.Split(md, "\n")
	type head struct {
		idx, level int
		title      string
	}
	var heads []head
	inFence := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := headingRe.FindStringSubmatch(ln); m != nil {
			heads = append(heads, head{i, len(m[1]), m[2]})
		}
	}
	var out []docSection
	for k, h := range heads {
		end := len(lines)
		for _, n := range heads[k+1:] {
			if n.level <= h.level {
				end = n.idx
				break
			}
		}
		out = append(out, docSection{h.level, h.title, strings.Join(lines[h.idx+1:end], "\n")})
	}
	return out
}

func sectionBody(t *testing.T, secs []docSection, title string) string {
	t.Helper()
	var body []string
	for _, s := range secs {
		if s.title == title {
			body = append(body, s.body)
		}
	}
	switch len(body) {
	case 0:
		t.Fatalf("docs/api.md has no section %q", title)
	case 1:
	default:
		t.Fatalf("docs/api.md has %d sections titled %q; the drift test needs a unique title", len(body), title)
	}
	return body[0]
}

// splitFences returns the contents of the ```json blocks and the prose with
// every fenced block removed.
func splitFences(body string) (jsonBlocks []string, prose string) {
	var prosePart, cur []string
	inFence, isJSON := false, false
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			if !inFence {
				inFence = true
				isJSON = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "```")) == "json"
				cur = nil
			} else {
				if isJSON {
					jsonBlocks = append(jsonBlocks, strings.Join(cur, "\n"))
				}
				inFence = false
			}
			continue
		}
		if inFence {
			cur = append(cur, ln)
		} else {
			prosePart = append(prosePart, ln)
		}
	}
	return jsonBlocks, strings.Join(prosePart, "\n")
}

type jsonObject struct {
	path string // "" top level, "a.b" nested objects, "a[]" array elements
	keys map[string]bool
}

// scanJSONObjects walks example JSON that need not be valid (placeholders such
// as `N` or `…` are allowed) and returns every object with the keys it has.
func scanJSONObjects(src string) []*jsonObject {
	type frame struct {
		isObj     bool
		path      string
		expectKey bool
		lastKey   string
		obj       *jsonObject
	}
	var stack []*frame
	var out []*jsonObject
	childPath := func() string {
		if len(stack) == 0 {
			return ""
		}
		p := stack[len(stack)-1]
		if !p.isObj {
			return p.path + "[]"
		}
		if p.path == "" {
			return p.lastKey
		}
		return p.path + "." + p.lastKey
	}
	for i := 0; i < len(src); i++ {
		switch c := src[i]; c {
		case '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j > len(src) {
				j = len(src)
			}
			s := src[i+1 : min(j, len(src))]
			i = j
			if n := len(stack); n > 0 && stack[n-1].isObj && stack[n-1].expectKey {
				top := stack[n-1]
				top.lastKey, top.expectKey = s, false
				if s != "…" {
					top.obj.keys[s] = true
				}
			}
		case '{':
			fr := &frame{isObj: true, path: childPath(), expectKey: true, obj: &jsonObject{keys: map[string]bool{}}}
			fr.obj.path = fr.path
			out = append(out, fr.obj)
			stack = append(stack, fr)
		case '[':
			stack = append(stack, &frame{path: childPath()})
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case ',':
			if n := len(stack); n > 0 && stack[n-1].isObj {
				stack[n-1].expectKey = true
			}
		}
	}
	return out
}

var (
	spanRe   = regexp.MustCompile("`([^`\n]+)`")
	identRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	quotedRe = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"\s*:`)
	braceRe  = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_?]*(?:,\s*[A-Za-z_][A-Za-z0-9_?]*)*)\}`)
)

// mentions returns the identifiers named in backtick spans of prose: `field`
// and the keys of an inline `{"field": value}`.
func mentions(prose string) map[string]bool {
	out := map[string]bool{}
	for _, m := range spanRe.FindAllStringSubmatch(prose, -1) {
		s := m[1]
		if identRe.MatchString(s) {
			out[s] = true
		}
		for _, q := range quotedRe.FindAllStringSubmatch(s, -1) {
			out[q[1]] = true
		}
	}
	return out
}

// braceLists returns every `{a, b, c?}` shorthand in prose as a key set.
func braceLists(prose string) []map[string]bool {
	var out []map[string]bool
	for _, m := range braceRe.FindAllStringSubmatch(prose, -1) {
		set := map[string]bool{}
		for _, w := range strings.Split(m[1], ",") {
			set[strings.TrimSuffix(strings.TrimSpace(w), "?")] = true
		}
		out = append(out, set)
	}
	return out
}

// ---- comparison ---------------------------------------------------------

// jsonSel picks the example objects that stand for a type: those at path that
// have the anchor key and do not have the not key.
type jsonSel struct{ path, anchor, not string }

type driftCase struct {
	name     string
	code     []string
	sections []string // titles in docs/api.md; their text is searched together
	obj      *jsonSel // JSON example selector, or
	brace    []string // anchors of a `{a, b}` shorthand list that holds them all
	// prose: fields documented only in running text (a backtick span of the
	// section), not in the example. Each must still be mentioned there; the
	// list is the only way a prose mention counts.
	prose []string
	// undocumented: code fields the docs deliberately omit, with the reason.
	undocumented map[string]string
	// docOnly: example keys that are not fields of the type, with the reason.
	docOnly map[string]string
}

// ragSettingsPending lists the ADR-024 project settings (migration 0021). They
// are in the admin API from the phase that added them; docs/api.md gets them
// in the v0.6 documentation phase, which is when these exceptions go.
var ragSettingsPending = map[string]string{
	"context_placement":         "ADR-024 setting, documented in the v0.6 docs phase",
	"retrieval_policy":          "ADR-024 setting, documented in the v0.6 docs phase",
	"query_strategy":            "ADR-024 setting, documented in the v0.6 docs phase",
	"history_turns_for_query":   "ADR-024 setting, documented in the v0.6 docs phase",
	"query_model_connection_id": "ADR-024 setting, documented in the v0.6 docs phase",
}

// compareDrift returns the code fields the docs lack and the doc keys the code
// lacks, plus problems with the case itself (anchor not found, stale
// exceptions).
func compareDrift(c driftCase, secs []docSection, titleBody func(string) string) (inCodeOnly, inDocsOnly, problems []string) {
	var prose strings.Builder
	var blocks []string
	for _, s := range c.sections {
		jb, p := splitFences(titleBody(s))
		blocks = append(blocks, jb...)
		prose.WriteString(p + "\n")
	}
	exact := map[string]bool{}
	switch {
	case c.obj != nil:
		found := 0
		for _, b := range blocks {
			for _, o := range scanJSONObjects(b) {
				if o.path != c.obj.path || !o.keys[c.obj.anchor] || (c.obj.not != "" && o.keys[c.obj.not]) {
					continue
				}
				found++
				for k := range o.keys {
					exact[k] = true
				}
			}
		}
		if found == 0 {
			problems = append(problems, "no JSON example object at path "+strconv.Quote(c.obj.path)+" with key "+strconv.Quote(c.obj.anchor))
		}
	case c.brace != nil:
		var hit []map[string]bool
		for _, l := range braceLists(prose.String()) {
			ok := true
			for _, a := range c.brace {
				ok = ok && l[a]
			}
			if ok {
				hit = append(hit, l)
			}
		}
		switch len(hit) {
		case 0:
			problems = append(problems, "no {…} shorthand list holding "+strings.Join(c.brace, ", "))
		case 1:
			exact = hit[0]
		default:
			problems = append(problems, "the anchors "+strings.Join(c.brace, ", ")+" match more than one {…} shorthand list; add anchors")
		}
	}
	if c.obj == nil && c.brace == nil {
		problems = append(problems, "case has neither an obj selector nor a brace list")
	}
	if len(problems) > 0 {
		return nil, nil, problems
	}

	mentioned := mentions(prose.String())
	documented := map[string]bool{}
	for k := range exact {
		documented[k] = true
	}
	for _, f := range c.prose {
		if mentioned[f] {
			documented[f] = true
		}
	}
	code := map[string]bool{}
	for _, f := range c.code {
		code[f] = true
	}
	for _, f := range c.code {
		if !documented[f] {
			if _, ok := c.undocumented[f]; !ok {
				inCodeOnly = append(inCodeOnly, f)
			}
		}
	}
	for k := range exact {
		if !code[k] {
			if _, ok := c.docOnly[k]; !ok {
				inDocsOnly = append(inDocsOnly, k)
			}
		}
	}
	for _, f := range c.prose {
		switch {
		case !code[f]:
			problems = append(problems, "stale prose entry: "+f+" is not a field of the type any more")
		case exact[f]:
			problems = append(problems, "stale prose entry: "+f+" is in the example now")
		}
	}
	for f, why := range c.undocumented {
		switch {
		case why == "":
			problems = append(problems, "exception for "+f+" has no reason")
		case !code[f]:
			problems = append(problems, "stale exception: "+f+" is not a field of the type any more")
		case documented[f]:
			problems = append(problems, "stale exception: "+f+" is documented now")
		}
	}
	for k, why := range c.docOnly {
		switch {
		case why == "":
			problems = append(problems, "docOnly exception for "+k+" has no reason")
		case !exact[k]:
			problems = append(problems, "stale docOnly exception: "+k+" is not in the docs example")
		case code[k]:
			problems = append(problems, "stale docOnly exception: "+k+" is a field of the type now")
		}
	}
	sort.Strings(inCodeOnly)
	sort.Strings(inDocsOnly)
	sort.Strings(problems)
	return
}

func union(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// ---- the registry -------------------------------------------------------

func driftCases(t *testing.T) []driftCase {
	user := jsonFieldNames(typeOf[store.User]())
	return []driftCase{
		// Session and account
		{name: "login body", code: handlerStructFields(t, "admin.go", "login"), sections: []string{"Session and account"},
			brace: []string{"username", "password", "bearer"}},
		{name: "setup body", code: handlerStructFields(t, "setup.go", "setup"), sections: []string{"First-run setup"},
			brace: []string{"username", "password", "bearer"}},
		{name: "me", code: union(user, handlerStructFields(t, "admin.go", "me")), sections: []string{"Session and account"},
			brace: []string{"id", "username", "role", "is_active"},
			prose: []string{"session_expires_at", "session_bearer", "session_kind", "scopes"}},
		{name: "password body", code: handlerStructFields(t, "admin.go", "changePassword"), sections: []string{"Session and account"},
			brace: []string{"current_password", "new_password"}},
		{name: "provider type", code: handlerStructFields(t, "admin.go", "providerTypes"), sections: []string{"Session and account"},
			obj: &jsonSel{"[]", "default_base_url", ""}},
		{name: "provider capabilities", code: jsonFieldNames(typeOf[provider.Capabilities]()), sections: []string{"Session and account"},
			obj: &jsonSel{"[].capabilities", "chat", ""}},
		{name: "provider preset", code: jsonFieldNames(typeOf[providerPreset]()), sections: []string{"Session and account"},
			obj: &jsonSel{"[]", "base_url", ""}},

		// Model connections
		{name: "connection", code: jsonFieldNames(typeOf[store.ModelConnection]()), sections: []string{"Model connections"},
			obj: &jsonSel{"", "api_key_masked", ""}},
		{name: "connection body", code: jsonFieldNames(typeOf[connInput]()), sections: []string{"Model connections"},
			obj: &jsonSel{"", "api_key", "mode"}},
		{name: "connection test body", code: handlerStructFields(t, "admin.go", "testConnection"), sections: []string{"Model connections"},
			brace: []string{"mode"}},
		{name: "unsaved connection test body", code: union(jsonFieldNames(typeOf[connInput]()), handlerStructFields(t, "admin.go", "testUnsavedConnection")),
			sections: []string{"Model connections"}, obj: &jsonSel{"", "connection_id", ""},
			prose: []string{"name"}},

		// RAG stores and documents
		{name: "rag store", code: jsonFieldNames(typeOf[ragStoreResponse]()), sections: []string{"RAG stores and documents"},
			obj: &jsonSel{"", "document_count", ""}, prose: []string{"reprocess_recommended"}},
		{name: "rag store body", code: jsonFieldNames(typeOf[ragInput]()), sections: []string{"RAG stores and documents"},
			obj: &jsonSel{"", "search_backend", "document_count"}},
		{name: "document", code: jsonFieldNames(typeOf[store.Document]()), sections: []string{"RAG stores and documents"},
			obj: &jsonSel{"", "progress_percent", ""}},
		{name: "search request", code: handlerStructFields(t, "admin.go", "searchRAGStore"), sections: []string{"RAG stores and documents"},
			obj: &jsonSel{"", "query", ""}, prose: []string{"backend", "rerank_backend", "rerank_connection_id"}},
		{name: "search hit", code: jsonFieldNames(typeOf[store.SearchHit]()), sections: []string{"RAG stores and documents"},
			obj: &jsonSel{"hits[]", "chunk_id", ""}},

		// Projects
		{name: "project", code: jsonFieldNames(typeOf[store.Project]()), sections: []string{"Projects"},
			obj: &jsonSel{"", "api_key_prefix", ""}, undocumented: ragSettingsPending},
		{name: "project body", code: jsonFieldNames(typeOf[projectInput]()), sections: []string{"Projects"},
			obj: &jsonSel{"", "member_user_ids", ""}, undocumented: ragSettingsPending},
		{name: "project members body", code: handlerStructFields(t, "admin.go", "setMembers"), sections: []string{"Projects"},
			brace: []string{"user_ids"}},
		{name: "project member", code: jsonFieldNames(typeOf[store.ProjectMember]()), sections: []string{"Projects"},
			brace: []string{"id", "username", "role", "added_at"}},
		{name: "usage report", code: jsonFieldNames(typeOf[limits.UsageReport]()), sections: []string{"Projects", "API keys"},
			obj: &jsonSel{"", "generated_at", ""}, prose: []string{"api_key_id"}},
		{name: "usage period", code: jsonFieldNames(typeOf[limits.PeriodUsage]()), sections: []string{"Projects"},
			obj: &jsonSel{"minute", "period_start", ""}},
		{name: "usage forecast", code: jsonFieldNames(typeOf[limits.Forecast]()), sections: []string{"Projects"},
			obj: &jsonSel{"forecast", "daily_exhausted_at", ""}},

		// Routes
		{name: "route", code: jsonFieldNames(typeOf[store.Route]()), sections: []string{"Routes"},
			obj: &jsonSel{"", "project_id", ""}},
		{name: "route target", code: jsonFieldNames(typeOf[store.RouteTarget]()), sections: []string{"Routes"},
			obj: &jsonSel{"targets[]", "connection_name", ""}},
		{name: "route body", code: jsonFieldNames(typeOf[routeInput]()), sections: []string{"Routes"},
			obj: &jsonSel{"", "strategy", "project_id"}},
		{name: "route target body", code: jsonFieldNames(typeOf[routeTargetInput]()), sections: []string{"Routes"},
			obj: &jsonSel{"targets[]", "connection_id", "connection_name"}},

		// API keys
		{name: "api key", code: jsonFieldNames(typeOf[store.APIKeyListItem]()), sections: []string{"API keys"},
			obj: &jsonSel{"", "key_prefix", ""}, prose: []string{"username"}},
		{name: "api key body", code: jsonFieldNames(typeOf[keyInput]()), sections: []string{"API keys"},
			obj: &jsonSel{"", "kind", "key_prefix"}},

		// Metrics
		{name: "metrics summary", code: jsonFieldNames(typeOf[store.MetricsSummary]()), sections: []string{"Metrics"},
			obj: &jsonSel{"window", "requests", ""}, prose: []string{"project_id"}},
		{name: "daily bucket", code: jsonFieldNames(typeOf[store.DailyBucket]()), sections: []string{"Metrics"},
			obj: &jsonSel{"daily[]", "day", ""}},
		{name: "project metrics", code: jsonFieldNames(typeOf[store.ProjectMetrics]()), sections: []string{"Metrics"},
			obj: &jsonSel{"projects[]", "project_id", ""}},
		{name: "request log", code: jsonFieldNames(typeOf[store.RequestLog]()), sections: []string{"Metrics"},
			obj: &jsonSel{"recent[]", "status_code", ""}},

		// Prices
		{name: "price", code: jsonFieldNames(typeOf[store.ModelPrice]()), sections: []string{"Model prices"},
			obj: &jsonSel{"", "model_pattern", ""}},
		{name: "price body", code: jsonFieldNames(typeOf[priceInput]()), sections: []string{"Model prices"},
			brace: []string{"provider_type", "model_pattern", "input_per_mtok"}},

		// Users and audit log
		{name: "user list entry", code: jsonFieldNames(typeOf[store.UserStats]()), sections: []string{"Users and audit log"},
			obj: &jsonSel{"[]", "project_count", ""}},
		{name: "lite user", code: jsonFieldNames(typeOf[liteUser]()), sections: []string{"Users and audit log"},
			brace: []string{"id", "username", "role"}},
		{name: "user create body", code: handlerStructFields(t, "users.go", "createUser"), sections: []string{"Users and audit log"},
			brace: []string{"username", "password", "role", "project_ids"}},
		{name: "user update body", code: handlerStructFields(t, "users.go", "updateUser"), sections: []string{"Users and audit log"},
			brace: []string{"role", "is_active"}},
		{name: "user reset password body", code: handlerStructFields(t, "users.go", "resetPassword"), sections: []string{"Users and audit log"},
			brace: []string{"new_password"}},
		{name: "audit entry", code: jsonFieldNames(typeOf[store.AuditLog]()), sections: []string{"Users and audit log"},
			obj: &jsonSel{"entries[]", "actor_user_id", ""}},

		// System
		{name: "system database", code: jsonFieldNames(typeOf[store.DatabaseInfo]()), sections: []string{"System"},
			obj: &jsonSel{"database", "postgres_version", ""}},
		{name: "system backup", code: jsonFieldNames(typeOf[store.BackupInfo]()), sections: []string{"System"},
			obj: &jsonSel{"backup", "tables", ""}},
	}
}

func TestAdminDocsDrift(t *testing.T) {
	raw, err := os.ReadFile(apiDocPath)
	if err != nil {
		t.Fatal(err)
	}
	secs := parseSections(string(raw))
	body := func(title string) string { return sectionBody(t, secs, title) }
	for _, c := range driftCases(t) {
		t.Run(c.name, func(t *testing.T) {
			codeOnly, docsOnly, problems := compareDrift(c, secs, body)
			for _, f := range codeOnly {
				t.Errorf("field %q is in the code but not in docs/api.md (%s); document it, or list it in undocumented with a reason", f, strings.Join(c.sections, ", "))
			}
			for _, k := range docsOnly {
				t.Errorf("key %q is in docs/api.md (%s) but not in the code; fix the docs, or list it in docOnly with a reason", k, strings.Join(c.sections, ", "))
			}
			for _, p := range problems {
				t.Error(p)
			}
		})
	}
}

// TestDocsDriftDetectsDrift runs the comparison against a synthetic document
// to show that both directions fail, and that exceptions silence exactly what
// they name.
func TestDocsDriftDetectsDrift(t *testing.T) {
	type thing struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Extra string `json:"extra,omitempty"`
		Skip  string `json:"-"`
	}
	md := "## Thing\n\nA thing:\n\n```json\n{\"id\": 1, \"name\": \"x\", \"ghost\": true, \"nested\": {\"a\": 1}}\n```\n"
	secs := parseSections(md)
	body := func(s string) string { return sectionBody(t, secs, s) }
	base := driftCase{name: "thing", code: jsonFieldNames(typeOf[thing]()), sections: []string{"Thing"}, obj: &jsonSel{"", "id", ""}}

	codeOnly, docsOnly, problems := compareDrift(base, secs, body)
	if strings.Join(codeOnly, ",") != "extra" || strings.Join(docsOnly, ",") != "ghost,nested" || len(problems) != 0 {
		t.Fatalf("unexpected result: codeOnly=%v docsOnly=%v problems=%v", codeOnly, docsOnly, problems)
	}

	silenced := base
	silenced.undocumented = map[string]string{"extra": "test"}
	silenced.docOnly = map[string]string{"ghost": "test", "nested": "test"}
	if a, b, p := compareDrift(silenced, secs, body); len(a)+len(b)+len(p) != 0 {
		t.Fatalf("exceptions did not silence: %v %v %v", a, b, p)
	}

	stale := base
	stale.undocumented = map[string]string{"id": "documented already", "missing": "no such field", "extra": ""}
	if _, _, p := compareDrift(stale, secs, body); len(p) != 3 {
		t.Fatalf("expected 3 stale-exception problems, got %v", p)
	}

	// A nested object is reachable by path.
	nested := driftCase{name: "n", code: []string{"a"}, sections: []string{"Thing"}, obj: &jsonSel{"nested", "a", ""}}
	if a, b, p := compareDrift(nested, secs, body); len(a)+len(b)+len(p) != 0 {
		t.Fatalf("nested object: %v %v %v", a, b, p)
	}

	// A bare backtick mention does not document a field: "name" is named in
	// the section for another reason, and a new field called "name" must
	// still fail until the example (or an explicit prose entry) covers it.
	type route struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	md2 := "## Route\n\nThe `model` request field picks a route; `name` labels it.\n\n```json\n{\"id\": 1, \"name\": \"r\"}\n```\n"
	secs2 := parseSections(md2)
	body2 := func(s string) string { return sectionBody(t, secs2, s) }
	common := driftCase{name: "r", code: jsonFieldNames(typeOf[route]()), sections: []string{"Route"}, obj: &jsonSel{"", "id", ""}}
	if a, _, _ := compareDrift(common, secs2, body2); strings.Join(a, ",") != "model" {
		t.Fatalf("a bare backtick mention must not document model, got %v", a)
	}

	// An explicit prose entry documents a field that is mentioned in the
	// section, and goes stale once the example carries it or the mention goes.
	prosed := common
	prosed.prose = []string{"model"}
	if a, b, p := compareDrift(prosed, secs2, body2); len(a)+len(b)+len(p) != 0 {
		t.Fatalf("prose entry did not document model: %v %v %v", a, b, p)
	}
	unmentioned := common
	unmentioned.prose = []string{"model"}
	secs3 := parseSections("## Route\n\nNothing here.\n\n```json\n{\"id\": 1, \"name\": \"r\"}\n```\n")
	body3 := func(s string) string { return sectionBody(t, secs3, s) }
	if a, _, _ := compareDrift(unmentioned, secs3, body3); strings.Join(a, ",") != "model" {
		t.Fatalf("a prose entry without a mention must fail, got %v", a)
	}
	staleProse := common
	staleProse.prose = []string{"id", "ghost"}
	if _, _, p := compareDrift(staleProse, secs2, body2); len(p) != 2 {
		t.Fatalf("expected 2 stale prose problems, got %v", p)
	}
}
