package store

import (
	"strings"
	"testing"
)

func TestLexicalPlan(t *testing.T) {
	tr := "tr"
	en := "en"
	cases := []struct {
		name            string
		r               RAGStore
		wantCol, wantFT string
	}{
		{"default store", RAGStore{}, "tsv", "simple"},
		{"simple", RAGStore{Language: "simple"}, "tsv", "simple"},
		{"simple with a stale applied", RAGStore{Language: "simple", FTSLanguageApplied: &tr}, "tsv", "simple"},
		{"language rebuilding", RAGStore{Language: "tr"}, "tsv", "simple"},
		{"applied for another language", RAGStore{Language: "tr", FTSLanguageApplied: &en}, "tsv", "simple"},
		{"applied", RAGStore{Language: "tr", FTSLanguageApplied: &tr}, "fts", "turkish"},
		{"unmapped language never reaches SQL", RAGStore{Language: "zz", FTSLanguageApplied: new(string)}, "tsv", "simple"},
	}
	for _, c := range cases {
		col, cfg := c.r.lexicalPlan()
		if col != c.wantCol || cfg != c.wantFT {
			t.Errorf("%s: plan = %s/%s, want %s/%s", c.name, col, cfg, c.wantCol, c.wantFT)
		}
	}
}

func TestPgvectorHybridQueryUsesPlannedColumn(t *testing.T) {
	p := testParams()
	p.FTSColumn, p.FTSConfig = ftsColumn, "turkish"
	sql, args := pgvectorBackend{}.HybridQuery(p)
	if !strings.Contains(sql, "c.fts @@ q.query") || !strings.Contains(sql, "ts_rank_cd(c.fts, q.query)") || strings.Contains(sql, "c.tsv") {
		t.Errorf("fts plan must search c.fts only:\n%s", sql)
	}
	if args[4] != "turkish" {
		t.Errorf("config arg = %v, want turkish", args[4])
	}
	for _, col := range []string{"", tsvColumn, "tsv; DROP TABLE chunks", "c.fts"} {
		p.FTSColumn = col
		sql, _ := pgvectorBackend{}.HybridQuery(p)
		if !strings.Contains(sql, "c.tsv @@ q.query") || strings.Contains(sql, "c.fts") || strings.Contains(sql, "DROP") {
			t.Errorf("column %q must fall back to tsv:\n%s", col, sql)
		}
	}
}

func TestDeriveFTS(t *testing.T) {
	tr := "tr"
	cases := []struct {
		r               RAGStore
		cfg, status     string
		appliesToSearch bool
	}{
		{RAGStore{SearchBackend: BackendPgvector}, "simple", FTSNotNeeded, true},
		{RAGStore{Language: "tr", SearchBackend: BackendPgvector}, "turkish", FTSRebuilding, true},
		{RAGStore{Language: "tr", FTSLanguageApplied: &tr, SearchBackend: BackendPgvector}, "turkish", FTSReady, true},
		{RAGStore{Language: "tr", FTSLanguageApplied: &tr, SearchBackend: BackendPgSearch}, "turkish", FTSReady, false},
	}
	for i, c := range cases {
		r := c.r
		r.deriveFTS()
		if r.FTSConfig != c.cfg || r.FTSStatus != c.status || r.LanguageAppliesToSearch != c.appliesToSearch {
			t.Errorf("case %d: %+v", i, r)
		}
	}
}

func TestFTSRebuildLockKeyIsInjective(t *testing.T) {
	seen := map[int64]int64{}
	for _, id := range []int64{0, 1, 2, 1 << 31, 1<<31 + 1, 1 << 32, 1<<32 + 1, 1 << 40, 1<<62 + 5} {
		k := ftsRebuildLockKey(id)
		if other, dup := seen[k]; dup {
			t.Errorf("store ids %d and %d share the lock key %d", other, id, k)
		}
		seen[k] = id
	}
}
