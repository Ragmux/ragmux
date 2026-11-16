package main

import (
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
)

// The first line is the one operators and scripts have always seen; a sealed
// column is mentioned only when it actually held rows.
func TestRotateSummary(t *testing.T) {
	got := rotateSummary(store.SealedReport{
		Connections: 3,
		Columns: []store.SealedCount{
			{Table: "webhook_endpoints", Column: "secret_enc", Rows: 2},
			{Table: "webhook_endpoints", Column: "prev_secret_enc", Rows: 0},
		},
	})
	want := "rotate-key: re-encrypted 3 model connection(s); start the gateway with the new SECRET_KEY\n" +
		"rotate-key: re-encrypted 2 webhook_endpoints.secret_enc value(s)\n"
	if got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "prev_secret_enc") {
		t.Error("a column with no rows was reported")
	}

	// Nothing but connections: exactly the historical output.
	if got := rotateSummary(store.SealedReport{Connections: 0}); got != "rotate-key: re-encrypted 0 model connection(s); start the gateway with the new SECRET_KEY\n" {
		t.Errorf("empty summary = %q", got)
	}
}
