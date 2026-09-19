// Command ragmux runs the AI gateway: an OpenAI-compatible proxy in front of
// multiple LLM providers with optional RAG, backed by PostgreSQL + pgvector.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ragmux/ragmux/internal/admin"
	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/config"
	"github.com/ragmux/ragmux/internal/gateway"
	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/maintenance"
	"github.com/ragmux/ragmux/internal/netguard"
	"github.com/ragmux/ragmux/internal/pricing"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/web"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "rotate-key" {
		os.Exit(rotateKey(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		os.Exit(resetPassword(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "pdf-extract" {
		// Internal: the ingester runs this on itself to parse PDFs out of
		// process (see rag.PDFWorker).
		os.Exit(rag.RunPDFWorker(os.Stdin, os.Stdout, os.Stderr))
	}
	healthcheck := flag.Bool("healthcheck", false, "probe the running server and exit (for container HEALTHCHECK)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ragmux " + version)
		return
	}
	if *healthcheck {
		port, err := config.PortFromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(2)
		}
		os.Exit(probe(port))
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func probe(port int) int {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	_ = resp.Body.Close()
	return 0
}

func run(cfg config.Config) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	admin.Version = version

	// ctx ends on SIGINT/SIGTERM and drives startup; background workers get
	// their own context so they can be drained in order during shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	st, err := store.Open(ctx, store.OpenConfig{
		DatabaseURL:  cfg.DatabaseURL,
		MaxConns:     cfg.DBMaxConns,
		SecretKeyHex: cfg.SecretKeyHex,
		DataDir:      cfg.DataDir,
	}, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Warn("close database", "err", err)
		}
	}()
	log.Info("database: connected", "postgres_version", st.ServerVersion, "max_conns", cfg.DBMaxConns,
		"secret_key_source", st.SecretKeySource)

	// Credentials written by releases before 0.2.3 are not bound to their
	// row; re-seal them with the connection id once, before anything reads them.
	if n, err := st.UpgradeConnectionKeys(ctx); err != nil {
		return fmt.Errorf("upgrade stored provider keys (is SECRET_KEY the right one?): %w", err)
	} else if n > 0 {
		log.Info("provider keys re-sealed with their connection id", "connections", n)
	}

	// The built-in price table is written after the migrations so the cost
	// estimate works out of the box. Rows an operator edited are left alone.
	if _, err := pricing.Seed(ctx, st.DB(), log); err != nil {
		return fmt.Errorf("seed model prices: %w", err)
	}
	prices := pricing.NewCache(st.DB(), log)

	if err := bootstrapAdmin(ctx, st, cfg, log); err != nil {
		return err
	}

	// Outbound provider calls: the dialer refuses private and local
	// addresses unless ALLOW_PRIVATE_UPSTREAMS / PRIVATE_UPSTREAM_ALLOWLIST
	// say otherwise. A proxy would connect on our behalf and bypass that
	// filter, so HTTP_PROXY is only honoured when private upstreams are
	// allowed anyway.
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: cfg.UpstreamTimeout,
		DialContext:           netguard.SafeDialContext(dialer, cfg.AllowPrivateUpstreams, cfg.PrivateUpstreamAllowlist),
	}
	if cfg.AllowPrivateUpstreams {
		transport.Proxy = http.ProxyFromEnvironment
	}
	httpClient := &http.Client{Transport: transport, CheckRedirect: netguard.CheckRedirect(3)}
	log.Info("upstream policy", "allow_private_upstreams", cfg.AllowPrivateUpstreams,
		"private_upstream_allowlist", len(cfg.PrivateUpstreamAllowlist))
	// Image fetching shares the hardened client on purpose. An image URL is
	// chosen by the API client rather than by an operator, so it needs the
	// same private-address and redirect policy a provider base URL gets.
	var images *provider.ImageFetcher
	if cfg.ImageFetch {
		images = &provider.ImageFetcher{
			Client:        httpClient,
			MaxBytes:      cfg.ImageFetchMaxBytes,
			Timeout:       cfg.ImageFetchTimeout,
			MaxPerRequest: cfg.ImageFetchMaxPerRequest,
			Logger:        log,
		}
		if cfg.ImageCacheEntries > 0 {
			images.Cache = provider.NewImageCache(cfg.ImageCacheEntries, cfg.ImageCacheTTL)
		}
	}
	log.Info("image policy", "fetch", cfg.ImageFetch, "max_mb", cfg.ImageFetchMaxBytes>>20,
		"max_per_request", cfg.ImageFetchMaxPerRequest, "cache_entries", cfg.ImageCacheEntries)
	provCfg := func(c *store.ModelConnection) provider.Config {
		return provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey,
			Model: c.ModelName, Timeout: cfg.UpstreamTimeout, Client: httpClient,
			StreamMaxDuration: cfg.StreamMaxDuration, StreamMaxBytes: cfg.StreamMaxBytes, Logger: log,
			Images: images}
	}
	providers := func(c *store.ModelConnection) (provider.Provider, error) { return provider.New(provCfg(c)) }
	embedders := func(c *store.ModelConnection) (provider.Embedder, error) { return provider.NewEmbedder(provCfg(c)) }

	if exe, err := os.Executable(); err == nil {
		rag.PDFWorker = []string{exe, "pdf-extract"}
	} else {
		log.Warn("pdf parsing stays in-process: cannot locate own executable", "err", err)
	}
	ingester := rag.NewIngester(bgCtx, st, embedders, cfg.IngestWorkers, log, rag.Settings{
		Lease: cfg.IngestLease, PollInterval: cfg.IngestPollInterval,
		MaxAttempts: cfg.IngestMaxAttempts, MaxPending: cfg.MaxPendingDocuments})
	ingester.MaxChunksPerDocument = cfg.MaxChunksPerDocument
	defer ingester.Stop()
	// Nothing to resume: the dispatcher's first poll claims every pending
	// document and every processing one whose lease expired, here and on
	// every other replica. The kick only saves it the first poll interval.
	ingester.Kick()
	log.Info("ingestion ready", "owner", ingester.Owner(), "workers", cfg.IngestWorkers,
		"lease", cfg.IngestLease, "poll_interval", cfg.IngestPollInterval,
		"max_attempts", cfg.IngestMaxAttempts, "max_pending", cfg.MaxPendingDocuments)
	retriever := rag.NewRetriever(st, embedders)
	retriever.Log = log
	// Rerankers resolves a store's rerank backend to a reranker. The API
	// backends read their credentials from the model connection the store
	// points at, so they go through the same decrypting Store lookup and the
	// same hardened outbound client as every other upstream call.
	retriever.Rerankers = func(ctx context.Context, rs *store.RAGStore) (rag.Reranker, error) {
		if rs.RerankBackend == "" || rs.RerankBackend == store.RerankLLM {
			return &rag.LLMReranker{}, nil
		}
		if rs.RerankConnectionID == nil {
			// The connection was deleted (ON DELETE SET NULL). Reranking is
			// skipped and logged, exactly like a reranker that fails.
			return nil, fmt.Errorf("%w: rag store %d has rerank_backend %q without a connection",
				rag.ErrRerankUnavailable, rs.ID, rs.RerankBackend)
		}
		conn, err := st.GetConnection(ctx, *rs.RerankConnectionID)
		if err != nil {
			return nil, fmt.Errorf("%w: rerank connection %d: %v", rag.ErrRerankUnavailable, *rs.RerankConnectionID, err)
		}
		client, err := provider.NewReranker(provCfg(conn))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", rag.ErrRerankUnavailable, err)
		}
		return &rag.APIReranker{Backend: rs.RerankBackend, Client: client}, nil
	}

	authSvc := &auth.Service{Store: st, TTL: cfg.SessionTTL, Secure: os.Getenv("SECURE_COOKIES") == "true",
		TrustProxy: cfg.TrustProxyHeaders}
	usage := &limits.Limiter{Store: st}
	gw := &gateway.Gateway{Store: st, Providers: providers, Retriever: retriever, Log: log, MaxBodyBytes: 4 << 20,
		Limiter: usage, Prices: prices}
	limiter := &auth.LoginLimiter{Store: st, PerIP: cfg.LoginRateLimitPerMin, PerUser: cfg.LoginUserLimitPerMin,
		LockoutFailures: cfg.LoginLockoutFailures, LockoutWindow: time.Duration(cfg.LoginLockoutMinutes) * time.Minute}
	adm := &admin.Admin{Store: st, Auth: authSvc, Ingester: ingester, Retriever: retriever, Providers: providers,
		Log: log, MaxUploadBytes: cfg.MaxUploadBytes, WebFS: web.FS, Limiter: limiter, Usage: usage, Prices: prices,
		ProviderConfig: provCfg, AllowPrivateUpstreams: cfg.AllowPrivateUpstreams, PrivateAllowlist: cfg.PrivateUpstreamAllowlist,
		MaxDocumentsPerStore: cfg.MaxDocumentsPerStore, MaxBytesPerStore: cfg.MaxBytesPerStore}

	scriptHash, err := dashboardScriptHash(web.FS)
	if err != nil {
		return err
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if cfg.TrustProxyHeaders {
		r.Use(realIP(cfg.TrustedProxyCIDRs))
	}
	r.Use(requestLogger(log))
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders(scriptHash))
	if len(cfg.CORSOrigins) > 0 {
		r.Use(cors(cfg.CORSOrigins))
	}
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.DB().Ping(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"` + version + `"}`))
	})
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	r.Route("/v1", gw.Routes)
	r.Route("/admin", adm.Routes)

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	janitor := &maintenance.Janitor{Store: st, Limiter: usage, Log: log,
		RequestLogDays: cfg.LogRetentionDays, AuditDays: cfg.AuditRetentionDays,
		IngestMaxAttempts: cfg.IngestMaxAttempts}
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		janitor.Run(bgCtx, maintenance.DefaultInterval)
	}()
	log.Info("retention job scheduled", "log_retention_days", cfg.LogRetentionDays,
		"audit_retention_days", cfg.AuditRetentionDays, "interval", maintenance.DefaultInterval)

	errc := make(chan error, 1)
	go func() {
		log.Info("ragmux listening", "addr", srv.Addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Shutdown order: stop accepting HTTP and drain in-flight requests, let
	// running ingestion jobs finish, stop the retention job, then the
	// deferred st.Close releases the pool.
	log.Info("shutting down: draining http")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shutErr := srv.Shutdown(shutCtx)
	if shutErr != nil {
		log.Warn("http shutdown", "err", shutErr)
	}
	log.Info("shutting down: waiting for ingestion jobs")
	if !ingester.StopWithTimeout(30 * time.Second) {
		log.Warn("ingestion jobs cancelled; their documents go back into the queue for another replica or the next start")
	}
	stopBackground()
	<-janitorDone
	log.Info("shutdown complete")
	return shutErr
}

// bootstrapAdmin pre-creates the first user from ADMIN_USER/ADMIN_PASSWORD
// when the users table is empty (unattended installs). Without a password
// nothing is created: the dashboard offers the first-run setup instead.
func bootstrapAdmin(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	n, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if cfg.AdminPassword == "" {
		log.Info("no users yet: open /admin/ to create the first administrator")
		return nil
	}
	hash, err := auth.HashPassword(cfg.AdminPassword)
	if err != nil {
		return err
	}
	u, err := st.CreateFirstUser(ctx, cfg.AdminUser, hash, string(auth.RoleAdmin))
	if errors.Is(err, store.ErrSetupDone) {
		return nil // another replica or a setup request got there first
	}
	if err != nil {
		return err
	}
	log.Info("admin user created from ADMIN_PASSWORD", "username", u.Username)
	return nil
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			if strings.HasPrefix(r.URL.Path, "/admin/") && !strings.HasPrefix(r.URL.Path, "/admin/api") {
				return // static assets
			}
			log.Info("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(),
				"bytes", ww.BytesWritten(), "dur_ms", time.Since(start).Milliseconds(),
				"req_id", middleware.GetReqID(r.Context()))
		})
	}
}

// realIP replaces RemoteAddr with the client address a trusted reverse proxy
// reported. It is only installed when TRUST_PROXY_HEADERS is set, because any
// client can send these headers when the gateway is reachable directly.
//
// X-Forwarded-For is read from the right: proxies append the address they
// accepted the connection from (nginx's $proxy_add_x_forwarded_for), so the
// last entry is the one written by the nearest proxy and the only one a
// client cannot forge. With trusted networks configured, headers are honoured
// only for connections from those networks, and entries that are themselves
// trusted proxies are skipped so the first untrusted hop wins. X-Real-IP is a
// fallback for proxies that do not set X-Forwarded-For at all.
func realIP(trusted []*net.IPNet) func(http.Handler) http.Handler {
	isTrusted := func(ip net.IP) bool {
		for _, n := range trusted {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := forwardedClientIP(r, trusted, isTrusted); ip != "" {
				r.RemoteAddr = ip
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedClientIP returns the proxy-reported client address, or "" when
// the headers must not be trusted or carry nothing usable.
func forwardedClientIP(r *http.Request, trusted []*net.IPNet, isTrusted func(net.IP) bool) string {
	if len(trusted) > 0 {
		peer := r.RemoteAddr
		if host, _, err := net.SplitHostPort(peer); err == nil {
			peer = host
		}
		if p := net.ParseIP(peer); p == nil || !isTrusted(p) {
			return ""
		}
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				hops = append(hops, e)
			}
		}
	}
	if len(hops) == 0 {
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
			return ip.String()
		}
		return ""
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil {
			return "" // a malformed hop: keep the peer address
		}
		if len(trusted) > 0 && i > 0 && isTrusted(ip) {
			continue // one of our own proxies; look further left
		}
		return ip.String()
	}
	return ""
}

// dashboardScriptHash returns the CSP sha256 source of the single inline
// script in the embedded dashboard, so the page can run under a policy that
// forbids every other script.
func dashboardScriptHash(webFS fs.FS) (string, error) {
	page, err := fs.ReadFile(webFS, "index.html")
	if err != nil {
		return "", fmt.Errorf("read dashboard: %w", err)
	}
	html := string(page)
	if strings.Count(html, "<script") != 1 {
		return "", errors.New("dashboard must contain exactly one <script> block for the CSP hash")
	}
	start := strings.Index(html, "<script>")
	end := strings.Index(html, "</script>")
	if start < 0 || end < start {
		return "", errors.New("dashboard <script> block not found")
	}
	sum := sha256.Sum256([]byte(html[start+len("<script>") : end]))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

// securityHeaders adds the browser hardening headers to every response and a
// Content-Security-Policy to the dashboard pages (not the JSON API). The
// dashboard uses inline style attributes and one inline <style>, so styles
// stay unrestricted; scripts are pinned to the embedded one by hash.
func securityHeaders(scriptHash string) func(http.Handler) http.Handler {
	csp := "default-src 'self'; script-src '" + scriptHash + "'; style-src 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-Frame-Options", "DENY")
			if strings.HasPrefix(r.URL.Path, "/admin") && !strings.HasPrefix(r.URL.Path, "/admin/api") {
				h.Set("Content-Security-Policy", csp)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func cors(origins []string) func(http.Handler) http.Handler {
	allowAll := false
	set := map[string]bool{}
	for _, o := range origins {
		if o == "*" {
			allowAll = true
		}
		set[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (allowAll || set[origin]) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Ragmux-Project")
				// Without this, browser JS cannot read a single one of the
				// headers the gateway sets: the fetch API hides every
				// response header that is not on the CORS safelist.
				w.Header().Set("Access-Control-Expose-Headers",
					"Retry-After, X-Request-Id, x-ratelimit-limit-requests, x-ratelimit-remaining-requests, "+
						"x-ratelimit-reset-requests, x-ragmux-budget-daily-remaining, x-ragmux-budget-monthly-remaining, "+
						"x-ragmux-rag-hits, x-ragmux-rag-sources, x-ragmux-projects")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
