package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/config"
	"github.com/ragmux/ragmux/internal/metrics"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// metricsRouter mounts /metrics exactly the way run() does.
func metricsRouter(t *testing.T, cfg config.Config) (*chi.Mux, *http.Server) {
	t.Helper()
	var reg *metrics.Registry
	if cfg.MetricsEnabled {
		reg = metrics.New(metrics.Options{MaxSeries: cfg.MetricsMaxSeries})
	}
	r := chi.NewRouter()
	return r, mountMetrics(r, cfg, reg)
}

func TestMetricsEndpointDisabled(t *testing.T) {
	r, second := metricsRouter(t, config.Config{})
	if second != nil {
		t.Fatal("a disabled endpoint must not start a second server")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when METRICS_ENABLED is off", w.Code)
	}
}

func TestMetricsEndpointRequiresToken(t *testing.T) {
	r, second := metricsRouter(t, config.Config{MetricsEnabled: true, MetricsToken: "s3cret"})
	if second != nil {
		t.Fatal("without METRICS_LISTEN there is no second server")
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status without a token = %d, want 401", w.Code)
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status with the wrong token = %d, want 401", w.Code)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status with the token = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want the Prometheus text exposition type", ct)
	}
	if !strings.Contains(w.Body.String(), "ragmux_metrics_series_dropped_total") {
		t.Errorf("body does not look like an exposition:\n%s", w.Body.String())
	}
}

// TestMetricsListenKeepsItOffTheMainRouter is the whole point of the second
// listener: a reverse proxy that forwards everything cannot reach a route
// the main router never had.
func TestMetricsListenKeepsItOffTheMainRouter(t *testing.T) {
	cfg := config.Config{MetricsEnabled: true, MetricsListen: "127.0.0.1:0"}
	r, second := metricsRouter(t, cfg)
	if second == nil {
		t.Fatal("METRICS_LISTEN must produce a second server")
	}
	if second.Addr != "127.0.0.1:0" {
		t.Errorf("second server Addr = %q", second.Addr)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("main router status = %d, want 404 while METRICS_LISTEN is set", w.Code)
	}

	// The second server does serve it, unauthenticated, because a loopback
	// bind is what config.Load accepts in place of a token.
	w = httptest.NewRecorder()
	second.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Errorf("second server status = %d, want 200", w.Code)
	}
}

// TestRequestLoggerSkipsMetrics: a fifteen-second scrape would otherwise
// drown the JSON log.
func TestRequestLoggerSkipsMetrics(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := requestLogger(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if buf.Len() != 0 {
		t.Errorf("/metrics was logged: %s", buf.String())
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(buf.String(), "/v1/models") {
		t.Errorf("an ordinary request should still be logged, got %q", buf.String())
	}
}

func TestConfigRejectsUnauthenticatedMetrics(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		token   string
		wantErr bool
	}{
		{"public bind without a token", "", "", true},
		{"explicit public bind without a token", "0.0.0.0:9090", "", true},
		{"an interface address without a token", "10.1.2.3:9090", "", true},
		{"a token on any bind", "0.0.0.0:9090", "t", false},
		{"loopback ipv4 needs none", "127.0.0.1:9090", "", false},
		{"loopback ipv6 needs none", "[::1]:9090", "", false},
		{"localhost needs none", "localhost:9090", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://u:p@h/db")
			t.Setenv("METRICS_ENABLED", "true")
			t.Setenv("METRICS_LISTEN", tc.listen)
			t.Setenv("METRICS_TOKEN", tc.token)
			_, err := config.Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a configuration error")
				}
				if !strings.Contains(err.Error(), "METRICS_TOKEN") {
					t.Errorf("the error must name the fix, got %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestReadyz(t *testing.T) {
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(discard{}, nil))
	r := chi.NewRouter()
	r.Get("/readyz", readyz(st, log))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on a healthy stack: %s", w.Code, w.Body.String())
	}
	var got struct {
		Status             string   `json:"status"`
		Migrations         int      `json:"migrations"`
		ExpectedMigrations int      `json:"expected_migrations"`
		Degraded           []string `json:"degraded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if got.Status != "ok" {
		t.Errorf("status = %q", got.Status)
	}
	if got.Migrations != store.HeadMigration() || got.ExpectedMigrations != store.HeadMigration() {
		t.Errorf("migrations = %d/%d, want the embedded head %d",
			got.Migrations, got.ExpectedMigrations, store.HeadMigration())
	}
	if len(got.Degraded) != 0 {
		t.Errorf("a plain stack should not be degraded: %v", got.Degraded)
	}
}

// TestReadyzDegradedStaysReady: a store configured for a search backend this
// server does not carry keeps answering through the pgvector fallback, so it
// must not take the replica out of rotation.
func TestReadyzDegradedStaysReady(t *testing.T) {
	st := testdb.Open(t)
	if st.Caps().PgSearch {
		t.Skip("this server carries pg_search, so nothing is degraded; the plain pgvector job covers this")
	}
	ctx := t.Context()
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{
		Name: "emb", ProviderType: "openai", BaseURL: "http://127.0.0.1:1/v1",
		APIKey: "k", ModelName: "text-embedding-3-small"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRAGStore(ctx, &store.RAGStore{
		Name: "s", EmbeddingConnectionID: conn.ID, Dimensions: 4,
		SearchMode: store.SearchHybrid, SearchBackend: store.BackendPgSearch}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(discard{}, nil))
	r := chi.NewRouter()
	r.Get("/readyz", readyz(st, log))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a degraded search backend is not a readiness failure", w.Code)
	}
	var got struct {
		Status   string   `json:"status"`
		Degraded []string `json:"degraded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if len(got.Degraded) != 1 || !strings.Contains(got.Degraded[0], "pg_search") {
		t.Errorf("degraded = %v, want one entry naming pg_search", got.Degraded)
	}
}

// discard swallows log output in tests.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
