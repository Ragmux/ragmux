package rag

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
)

// TestRerankFailureReasonIsClosed pins the label set. Anything that is not
// recognised must land on "upstream" rather than mint a new label value:
// err.Error() quotes upstream bodies and model replies, and as a label that
// is unbounded.
func TestRerankFailureReasonIsClosed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, ""},
		{"no reranker configured", ErrRerankUnavailable, obs.RerankUnavailable},
		{"wrapped unavailable", fmt.Errorf("rerank connection 3: %w", ErrRerankUnavailable), obs.RerankUnavailable},
		{"unreadable ranking", fmt.Errorf("rerank: %w from reply %q", errRerankParse, "sure!"), obs.RerankParse},
		{"deadline", fmt.Errorf("rerank: %w", context.DeadlineExceeded), obs.RerankTimeout},
		{"provider timeout", fmt.Errorf("rerank: %w", &provider.Error{Type: "timeout", Message: "upstream request timed out"}), obs.RerankTimeout},
		{"provider 500", fmt.Errorf("rerank: %w", &provider.Error{Type: "upstream_error", Message: "boom"}), obs.RerankUpstream},
		{"anything else", errors.New("who knows"), obs.RerankUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rerankFailureReason(tc.err); got != tc.want {
				t.Errorf("rerankFailureReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
