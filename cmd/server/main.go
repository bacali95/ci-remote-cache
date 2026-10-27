// Command server runs a self-hosted remote cache server for Nx
// (https://nx.dev/docs/kb/self-hosted-caching#build-your-own-caching-server)
// and Turborepo (https://turborepo.dev/api/remote-cache-spec) side by side,
// alongside an embedded admin UI for browsing/pruning the cache and
// managing users, access tokens, and runtime settings (storage backend,
// session TTL, max cache entry size — all live in Postgres and are
// editable from the UI, not env vars).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ci-remote-cache/internal/adminapi"
	"ci-remote-cache/internal/auth"
	"ci-remote-cache/internal/cache"
	"ci-remote-cache/internal/config"
	"ci-remote-cache/internal/db"
	"ci-remote-cache/internal/janitor"
	"ci-remote-cache/internal/protocol"
	"ci-remote-cache/internal/protocol/nx"
	"ci-remote-cache/internal/protocol/turbo"
	"ci-remote-cache/internal/session"
	"ci-remote-cache/internal/settings"
	"ci-remote-cache/internal/storage"
	"ci-remote-cache/internal/store"
	webui "ci-remote-cache/web"
)

// Background cleanup thresholds (see internal/janitor for the exact
// semantics, in particular why unreadAfter needs an age gate). Not yet
// exposed as admin-configurable settings — worth adding to the Settings
// page if these ever need tuning per deployment.
const (
	janitorMaxAge      = 30 * 24 * time.Hour
	janitorUnreadAfter = 14 * 24 * time.Hour
	janitorInterval    = 6 * time.Hour
)

func main() {
	// The runtime image is distroless (no shell, no curl/wget) — Docker
	// Compose's healthcheck runs this binary itself instead:
	// ["CMD", "/server", "healthcheck"].
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func runHealthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:" + port + "/health")
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	enc, err := settings.NewEncryptor(cfg.SettingsEncryptionKey)
	if err != nil {
		return fmt.Errorf("SETTINGS_ENCRYPTION_KEY: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := waitForDB(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool)

	if err := bootstrapAdmin(ctx, st, cfg, log); err != nil {
		return err
	}

	uiFS, err := webui.DistFS()
	if err != nil {
		return fmt.Errorf("load embedded admin UI: %w", err)
	}

	// The storage backend, session TTL, and max entry size are runtime
	// settings (internal/settings) rather than fixed at startup: dynBackend
	// is a swappable indirection both the cache core and the admin API hold
	// onto, and settingsMgr.Load populates it (plus the session/cache
	// atomics) from whatever's currently in Postgres before either starts
	// accepting requests.
	dynBackend := storage.NewDynamic(nil)
	sessions := session.NewManager(st, 0)
	cacheSvc := cache.New(dynBackend, auth.NewCacheTokenAuth(st), st, log, 0)
	settingsMgr := settings.NewManager(st, enc, dynBackend, sessions, cacheSvc)
	if err := settingsMgr.Load(ctx); err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	adminSrv := adminapi.New(st, sessions, dynBackend, settingsMgr, log, cfg.CookieSecure, uiFS)

	j := janitor.New(dynBackend, st, log, janitor.Config{
		MaxAge:      janitorMaxAge,
		UnreadAfter: janitorUnreadAfter,
		Interval:    janitorInterval,
	})
	go j.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	mux.Handle("/admin/", adminSrv.Handler())
	mux.Handle("/", protocol.Handler(log, nx.New(cacheSvc, log), turbo.New(cacheSvc, log)))

	httpServer := &http.Server{
		Addr:         cfg.Addr,
		Handler:      mux,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// waitForDB retries migrations until they succeed or ctx is cancelled.
// Postgres containers can report healthy (accepting connections) while
// still mid-restart from their own init sequence, during which auth briefly
// fails — a real race in orchestrators generally (compose, k8s, ECS), not
// just a local quirk, so this retry belongs here rather than in a
// docker-compose healthcheck tweak.
func waitForDB(ctx context.Context, url string) error {
	const (
		maxAttempts = 15
		backoff     = 1 * time.Second
	)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = db.Migrate(url)
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return fmt.Errorf("database not ready after %d attempts: %w", maxAttempts, lastErr)
}

// bootstrapAdmin creates the first admin user from env vars if the users
// table is empty. This is the only way to get a first login on a fresh
// deployment, since there is no public self-registration route.
func bootstrapAdmin(ctx context.Context, st *store.Store, cfg *config.Config, log *slog.Logger) error {
	if cfg.AdminBootstrapEmail == "" || cfg.AdminBootstrapPassword == "" {
		return nil
	}
	n, err := st.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		return nil
	}

	hash, err := session.HashPassword(cfg.AdminBootstrapPassword)
	if err != nil {
		return fmt.Errorf("hash bootstrap password: %w", err)
	}
	if _, err := st.CreateUser(ctx, cfg.AdminBootstrapEmail, hash); err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	log.Info("created bootstrap admin user", "email", cfg.AdminBootstrapEmail)
	return nil
}
