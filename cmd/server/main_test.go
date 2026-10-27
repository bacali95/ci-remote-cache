package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"ci-remote-cache/internal/config"
	"ci-remote-cache/internal/db"
	"ci-remote-cache/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

// main() and run() are deliberately not covered here: they bind a real OS
// TCP listener and block on real OS signals (SIGINT/SIGTERM) for the
// process's entire lifetime, which is the same "process bootstrap, not
// business logic" territory as main.tsx on the frontend — see
// internal/integration for a test that exercises the same wiring these
// two functions assemble (data-plane server + admin API sharing one
// store), without the real-listener/real-signal parts. runHealthcheck,
// waitForDB, and bootstrapAdmin below are the logic-bearing pieces of
// this file, and are covered directly.

func newLoopbackListener(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	_, portStr, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}
	return l, port
}

func TestRunHealthcheckSucceeds(t *testing.T) {
	l, port := newLoopbackListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	t.Setenv("PORT", strconv.Itoa(port))
	if got := runHealthcheck(); got != 0 {
		t.Fatalf("runHealthcheck() = %d, want 0", got)
	}
}

func TestRunHealthcheckFailsOnNonOKStatus(t *testing.T) {
	l, port := newLoopbackListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	t.Setenv("PORT", strconv.Itoa(port))
	if got := runHealthcheck(); got != 1 {
		t.Fatalf("runHealthcheck() = %d, want 1", got)
	}
}

func TestRunHealthcheckFailsWhenUnreachable(t *testing.T) {
	// Nothing listens on this port (it was just closed), so the request
	// itself fails — exercises the client.Get error branch, as opposed to
	// the non-OK-status branch above. Also leaves PORT unset, which
	// exercises the "default to 3000" fallback; if something else in this
	// environment happens to be listening on 3000 and answers 200, this
	// would false-fail, but nothing legitimate should be there in a test
	// sandbox.
	l, _ := newLoopbackListener(t)
	_ = l.Close()

	t.Setenv("PORT", "")
	if got := runHealthcheck(); got != 1 {
		t.Fatalf("runHealthcheck() = %d, want 1 (nothing listening on the default port)", got)
	}
}

func TestWaitForDBSucceedsImmediately(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping Postgres integration test")
	}
	if err := waitForDB(context.Background(), url); err != nil {
		t.Fatalf("waitForDB: %v", err)
	}
}

func TestWaitForDBReturnsCtxErrWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before waitForDB's first retry-select

	err := waitForDB(ctx, "not a valid connection string")
	if err != context.Canceled {
		t.Fatalf("waitForDB with cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestWaitForDBExhaustsAttempts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow (~15s) retry-exhaustion test in -short mode")
	}
	err := waitForDB(context.Background(), "not a valid connection string")
	if err == nil || !strings.Contains(err.Error(), "database not ready after") {
		t.Fatalf("waitForDB error = %v, want it to mention exhausted attempts", err)
	}
}

func newBootstrapTestStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping Postgres integration test")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE users, sessions, tokens RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return store.New(pool)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBootstrapAdminNoopWithoutCredentials(t *testing.T) {
	ctx := context.Background()
	st := newBootstrapTestStore(t)

	if err := bootstrapAdmin(ctx, st, &config.Config{}, testLogger()); err != nil {
		t.Fatalf("bootstrapAdmin: %v", err)
	}
	n, err := st.CountUsers(ctx)
	if err != nil || n != 0 {
		t.Fatalf("CountUsers = (%d, %v), want (0, nil): bootstrap should have been a no-op", n, err)
	}
}

func TestBootstrapAdminCreatesFirstUser(t *testing.T) {
	ctx := context.Background()
	st := newBootstrapTestStore(t)
	cfg := &config.Config{AdminBootstrapEmail: "admin@example.com", AdminBootstrapPassword: "hunter2"}

	if err := bootstrapAdmin(ctx, st, cfg, testLogger()); err != nil {
		t.Fatalf("bootstrapAdmin: %v", err)
	}
	u, err := st.GetUserByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if u.PasswordHash == "" || u.PasswordHash == "hunter2" {
		t.Fatalf("password should be hashed, got %q", u.PasswordHash)
	}
}

func TestBootstrapAdminNoopWhenUsersAlreadyExist(t *testing.T) {
	ctx := context.Background()
	st := newBootstrapTestStore(t)
	if _, err := st.CreateUser(ctx, "existing@example.com", "hash"); err != nil {
		t.Fatalf("seed CreateUser: %v", err)
	}

	cfg := &config.Config{AdminBootstrapEmail: "admin@example.com", AdminBootstrapPassword: "hunter2"}
	if err := bootstrapAdmin(ctx, st, cfg, testLogger()); err != nil {
		t.Fatalf("bootstrapAdmin: %v", err)
	}

	if _, err := st.GetUserByEmail(ctx, "admin@example.com"); err != store.ErrNotFound {
		t.Fatalf("bootstrap should not have created a second admin: err = %v", err)
	}
}

func TestBootstrapAdminHashPasswordTooLong(t *testing.T) {
	ctx := context.Background()
	st := newBootstrapTestStore(t)
	tooLong := strings.Repeat("a", 73) // bcrypt rejects passwords over 72 bytes
	cfg := &config.Config{AdminBootstrapEmail: "admin@example.com", AdminBootstrapPassword: tooLong}

	err := bootstrapAdmin(ctx, st, cfg, testLogger())
	if err == nil || !strings.Contains(err.Error(), "hash bootstrap password") {
		t.Fatalf("bootstrapAdmin error = %v, want it to mention hash bootstrap password", err)
	}
}
