package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCheckVectorDims(t *testing.T) {
	for _, dims := range []int{1, 1536, MaxIndexedVectorDims, 3072, MaxHalfvecIndexedDims} {
		if err := CheckVectorDims(dims); err != nil {
			t.Errorf("CheckVectorDims(%d) = %v, want nil", dims, err)
		}
	}
	for _, dims := range []int{MaxHalfvecIndexedDims + 1, 4097, 8192} {
		err := CheckVectorDims(dims)
		if !errors.Is(err, ErrVectorDimsUnsupported) {
			t.Fatalf("CheckVectorDims(%d) = %v, want ErrVectorDimsUnsupported", dims, err)
		}
		for _, want := range []string{fmt.Sprint(dims), "4000", "binary quantization", "v0.9"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}
}

// Before pgvector 0.7.0 there is no halfvec, so the ceiling stays at 2000 and
// the message says why.
func TestCapabilitiesCheckVectorDims(t *testing.T) {
	for _, c := range []struct {
		version string
		dims    int
		ok      bool
	}{
		{"0.8.0", 3072, true}, {"0.7.0", 4000, true}, {"0.7.4", 2001, true},
		{"0.6.2", 2000, true}, {"0.6.2", 2001, false}, {"0.5.1", 3072, false},
		{"", 2001, false}, {"", 1536, true},
		{"0.8.0", 4001, false},
	} {
		err := Capabilities{PgvectorVersion: c.version}.checkVectorDims(c.dims)
		if c.ok != (err == nil) {
			t.Errorf("pgvector %q, %d dims: err = %v, want ok=%v", c.version, c.dims, err, c.ok)
			continue
		}
		if err != nil && !errors.Is(err, ErrVectorDimsUnsupported) {
			t.Errorf("pgvector %q, %d dims: err = %v, want ErrVectorDimsUnsupported", c.version, c.dims, err)
		}
		if err != nil && c.dims <= MaxHalfvecIndexedDims && !strings.Contains(err.Error(), "0.7.0") {
			t.Errorf("pgvector %q, %d dims: %q does not name the pgvector version it needs", c.version, c.dims, err)
		}
	}
}

func TestSupportsHalfvec(t *testing.T) {
	for v, want := range map[string]bool{"0.8.0": true, "0.7.0": true, "0.7.4": true, "1.0.0": true,
		"0.6.2": false, "0.5.1": false, "": false, "garbage": false} {
		if got := (Capabilities{PgvectorVersion: v}).SupportsHalfvec(); got != want {
			t.Errorf("SupportsHalfvec(%q) = %v, want %v", v, got, want)
		}
	}
}

// The halfvec vector leg orders by the expression the index is on and takes
// the distance from the stored full-precision vector; a plain table is
// untouched.
func TestVectorLegHalfvecOrdering(t *testing.T) {
	leg, _ := vectorLeg("chunk_embeddings_2500", 2500, nil, 5)
	for _, want := range []string{
		"e.embedding <=> $1::vector AS distance",
		"ORDER BY e.embedding::halfvec(2500) <=> $1::vector::halfvec(2500)",
		"ORDER BY distance + 0, chunk_id",
	} {
		if !strings.Contains(leg, want) {
			t.Errorf("halfvec leg lacks %q:\n%s", want, leg)
		}
	}
	plain, _ := vectorLeg("chunk_embeddings_1536", 0, nil, 5)
	if strings.Contains(plain, "halfvec") || !strings.Contains(plain, "ORDER BY distance\n") {
		t.Errorf("a plain leg must not change:\n%s", plain)
	}
}

// Only the widths the halfvec index covers search in halfvec order, and only
// on a server that has halfvec.
func TestUseHalfvecOrder(t *testing.T) {
	s := &Store{caps: Capabilities{PgvectorVersion: "0.8.0"}}
	for dims, want := range map[int]bool{1536: false, 2000: false, 2001: true, 3072: true, 4000: true, 4001: false} {
		if got := s.useHalfvecOrder(dims); got != want {
			t.Errorf("useHalfvecOrder(%d) = %v, want %v", dims, got, want)
		}
	}
	old := &Store{caps: Capabilities{PgvectorVersion: "0.6.2"}}
	if old.useHalfvecOrder(3072) {
		t.Error("halfvec ordering on a pgvector without halfvec")
	}
}

func TestHalfvecIndexDDL(t *testing.T) {
	want := "CREATE INDEX IF NOT EXISTS chunk_embeddings_3072_hnsw_half ON chunk_embeddings_3072 " +
		"USING hnsw ((embedding::halfvec(3072)) halfvec_cosine_ops)"
	if got := halfvecIndexDDL(3072, false); got != want {
		t.Errorf("DDL = %q\nwant %q", got, want)
	}
	if got := halfvecIndexDDL(3072, true); !strings.Contains(got, "CREATE INDEX CONCURRENTLY IF NOT EXISTS ") {
		t.Errorf("concurrent DDL = %q", got)
	}
}

func TestCheckStoreBinding(t *testing.T) {
	cases := []struct {
		name                string
		storeConn           int64
		storeDims, dims     int
		wantConn            int64
		wantErr             error
		wantMismatchMessage bool
	}{
		{name: "unbound store takes the width", storeConn: 1, dims: 4, wantConn: 1},
		{name: "same width", storeConn: 1, storeDims: 4, dims: 4, wantConn: 1},
		{name: "connection check skipped", storeConn: 1, storeDims: 4, dims: 4},
		{name: "unbound store takes 3072", storeConn: 1, dims: 3072, wantConn: 1},
		{name: "unbound store refuses 4097", storeConn: 1, dims: 4097, wantConn: 1, wantErr: ErrVectorDimsUnsupported},
		// A store bound before the limit keeps accepting its own width.
		{name: "existing 5000 store", storeConn: 1, storeDims: 5000, dims: 5000, wantConn: 1},
		{name: "connection changed", storeConn: 2, dims: 4, wantConn: 1, wantErr: ErrEmbeddingConnectionChanged},
		{name: "width mismatch", storeConn: 1, storeDims: 8, dims: 4, wantConn: 1, wantMismatchMessage: true},
	}
	for _, c := range cases {
		err := checkStoreBinding(c.storeConn, c.storeDims, c.wantConn, c.dims)
		switch {
		case c.wantErr != nil:
			if !errors.Is(err, c.wantErr) {
				t.Errorf("%s: err = %v, want %v", c.name, err, c.wantErr)
			}
		case c.wantMismatchMessage:
			if err == nil || !strings.Contains(err.Error(), "does not match store dimension") {
				t.Errorf("%s: err = %v, want a dimension mismatch", c.name, err)
			}
		case err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
	}
}

func TestFTSConfigWarning(t *testing.T) {
	if msg := FTSConfigWarning("turkish", BackendPgvector); !strings.Contains(msg, "turkish") || !strings.Contains(msg, "v0.6-9") {
		t.Errorf("turkish warning = %q", msg)
	}
	for _, c := range []struct{ cfg, backend string }{
		{"simple", BackendPgvector}, {"", BackendPgvector}, {" Simple ", BackendPgvector},
		// pg_search ignores fts_config, so there is nothing to warn about.
		{"turkish", BackendPgSearch},
	} {
		if msg := FTSConfigWarning(c.cfg, c.backend); msg != "" {
			t.Errorf("FTSConfigWarning(%q, %q) = %q, want none", c.cfg, c.backend, msg)
		}
	}
}
