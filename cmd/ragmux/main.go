// Command ragmux runs the AI gateway: an OpenAI-compatible proxy in front of
// multiple LLM providers with optional RAG, backed by PostgreSQL + pgvector.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/web"
)

var version = "dev"

func main() {
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

	if err := bootstrapAdmin(ctx, st, cfg, log); err != nil {
		return err
	}

	httpClient := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: cfg.UpstreamTimeout,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}}
	provCfg := func(c *store.ModelConnection) provider.Config {
		return provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey,
			Model: c.ModelName, Timeout: cfg.UpstreamTimeout, Client: httpClient}
	}
	providers := func(c *store.ModelConnection) (provider.Provider, error) { return provider.New(provCfg(c)) }
	embedders := func(c *store.ModelConnection) (provider.Embedder, error) { return provider.NewEmbedder(provCfg(c)) }

	ingester := rag.NewIngester(bgCtx, st, embedders, cfg.IngestWorkers, log)
	defer ingester.Stop()
	if err := ingester.Resume(ctx); err != nil {
		log.Warn("resume ingestion", "err", err)
	}
	retriever := rag.NewRetriever(st, embedders)

	authSvc := &auth.Service{Store: st, TTL: cfg.SessionTTL, Secure: os.Getenv("SECURE_COOKIES") == "true"}
	usage := &limits.Limiter{Store: st}
	gw := &gateway.Gateway{Store: st, Providers: providers, Retriever: retriever, Log: log, MaxBodyBytes: 4 << 20, Limiter: usage}
	limiter := &auth.LoginLimiter{Store: st, PerIP: cfg.LoginRateLimitPerMin, PerUser: cfg.LoginUserLimitPerMin,
		LockoutFailures: cfg.LoginLockoutFailures, LockoutWindow: time.Duration(cfg.LoginLockoutMinutes) * time.Minute}
	adm := &admin.Admin{Store: st, Auth: authSvc, Ingester: ingester, Retriever: retriever, Providers: providers,
		Log: log, MaxUploadBytes: cfg.MaxUploadBytes, WebFS: web.FS, Limiter: limiter, Usage: usage}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if cfg.TrustProxyHeaders {
		r.Use(realIP)
	}
	r.Use(requestLogger(log))
	r.Use(middleware.Recoverer)
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
		RequestLogDays: cfg.LogRetentionDays, AuditDays: cfg.AuditRetentionDays}
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
		log.Warn("ingestion jobs cancelled; unfinished documents resume on next start")
	}
	stopBackground()
	<-janitorDone
	log.Info("shutdown complete")
	return shutErr
}

// bootstrapAdmin creates the first user when the users table is empty.
func bootstrapAdmin(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	n, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	pw := cfg.AdminPassword
	generated := false
	if pw == "" {
		pw, err = store.GenerateSessionToken()
		if err != nil {
			return err
		}
		pw = pw[:20]
		generated = true
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if _, err := st.CreateUser(ctx, cfg.AdminUser, hash, string(auth.RoleAdmin)); err != nil {
		return err
	}
	if generated {
		// Printed once; set ADMIN_PASSWORD to avoid this.
		fmt.Fprintf(os.Stderr, "\n==========================================================\n"+
			"  Initial admin account created\n  username: %s\n  password: %s\n"+
			"  (change it in the dashboard; set ADMIN_PASSWORD to preset it)\n"+
			"==========================================================\n\n", cfg.AdminUser, pw)
	}
	log.Info("admin user created", "username", cfg.AdminUser, "generated_password", generated)
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
// reported in X-Real-IP or the first X-Forwarded-For entry. It is only
// installed when TRUST_PROXY_HEADERS is set, because any client can send
// these headers when the gateway is reachable directly.
func realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := strings.TrimSpace(r.Header.Get("X-Real-IP"))
		if ip == "" {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				ip, _, _ = strings.Cut(xff, ",")
				ip = strings.TrimSpace(ip)
			}
		}
		if ip != "" && net.ParseIP(ip) != nil {
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
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
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
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
