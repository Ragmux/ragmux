package store

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckVectorDims(t *testing.T) {
	for _, dims := range []int{1, 1536, MaxIndexedVectorDims} {
		if err := CheckVectorDims(dims); err != nil {
			t.Errorf("CheckVectorDims(%d) = %v, want nil", dims, err)
		}
	}
	err := CheckVectorDims(3072)
	if !errors.Is(err, ErrVectorDimsUnsupported) {
		t.Fatalf("CheckVectorDims(3072) = %v, want ErrVectorDimsUnsupported", err)
	}
	for _, want := range []string{"3072", "2000", "halfvec", "v0.6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
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
		{name: "unbound store refuses 3072", storeConn: 1, dims: 3072, wantConn: 1, wantErr: ErrVectorDimsUnsupported},
		// A store bound before the limit keeps accepting its own width.
		{name: "existing 3072 store", storeConn: 1, storeDims: 3072, dims: 3072, wantConn: 1},
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
