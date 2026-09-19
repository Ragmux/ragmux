package store

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// resolveBackend and the reason strings are the observable half of the boot
// smoke test: when pg_search is installed but does not speak the query API
// this build uses, detectCapabilities clears Capabilities.PgSearch and keeps
// the version. Everything downstream must then behave exactly as if the
// extension were absent -- but say something more useful than "not
// installed", because it is.
func testStore(caps Capabilities) *Store {
	return &Store{caps: caps, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestResolveBackendAfterSmokeTestDowngrade(t *testing.T) {
	downgraded := testStore(Capabilities{PgSearch: false, PgSearchVersion: "0.15.0"})
	if downgraded.SearchBackendAvailable(BackendPgSearch) {
		t.Error("a downgraded pg_search must not be available")
	}
	reason := downgraded.SearchBackendReason(BackendPgSearch)
	if !strings.Contains(reason, "0.15.0") || !strings.Contains(reason, "query API") {
		t.Errorf("reason should name the version and the mismatch: %q", reason)
	}
	if got := downgraded.resolveBackend(BackendPgSearch, 1).Name(); got != BackendPgvector {
		t.Errorf("resolved backend = %q, want pgvector", got)
	}

	absent := testStore(Capabilities{})
	if r := absent.SearchBackendReason(BackendPgSearch); !strings.Contains(r, "not installed") {
		t.Errorf("absent reason: %q", r)
	}

	present := testStore(Capabilities{PgSearch: true, PgSearchVersion: "0.15.0"})
	if !present.SearchBackendAvailable(BackendPgSearch) || present.SearchBackendReason(BackendPgSearch) != "" {
		t.Error("an installed, compatible pg_search must be available with no reason")
	}
	if got := present.resolveBackend(BackendPgSearch, 1).Name(); got != BackendPgSearch {
		t.Errorf("resolved backend = %q, want pg_search", got)
	}

	// An empty or unknown backend name is pgvector, never an error: the
	// column's CHECK is the gate, and a row that slipped past it still has
	// to answer searches.
	for _, name := range []string{"", "lucene"} {
		if got := present.resolveBackend(name, 1).Name(); got != BackendPgvector {
			t.Errorf("backend %q resolved to %q, want pgvector", name, got)
		}
	}
}
