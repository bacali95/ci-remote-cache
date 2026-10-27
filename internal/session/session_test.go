package session

import (
	"context"
	"os"
	"testing"
	"time"

	"ci-remote-cache/internal/db"
	"ci-remote-cache/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newTestStore(t *testing.T) *store.Store {
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
	if _, err := pool.Exec(context.Background(), `INSERT INTO app_settings (id) VALUES (true)`); err != nil {
		t.Fatalf("reseed app_settings: %v", err)
	}
	return store.New(pool)
}

func TestLoginLogoutLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	m := NewManager(s, time.Hour)

	hash, err := HashPassword("correct-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := s.CreateUser(ctx, "admin@example.com", hash); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, err := m.Login(ctx, "admin@example.com", "wrong-password"); err != ErrInvalidCredentials {
		t.Fatalf("wrong password: err = %v, want ErrInvalidCredentials", err)
	}
	if _, err := m.Login(ctx, "nope@example.com", "whatever"); err != ErrInvalidCredentials {
		t.Fatalf("unknown email: err = %v, want ErrInvalidCredentials", err)
	}

	sessionID, err := m.Login(ctx, "admin@example.com", "correct-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sessionID == "" {
		t.Fatalf("expected non-empty session id")
	}

	u, err := m.CurrentUser(ctx, sessionID)
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if u.Email != "admin@example.com" {
		t.Fatalf("CurrentUser returned %q, want admin@example.com", u.Email)
	}

	if err := m.Logout(ctx, sessionID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := m.CurrentUser(ctx, sessionID); err != store.ErrNotFound {
		t.Fatalf("after logout: err = %v, want ErrNotFound", err)
	}
}

func TestSetTTLTakesEffectOnNextLogin(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	m := NewManager(s, time.Hour)

	hash, err := HashPassword("correct-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := s.CreateUser(ctx, "admin@example.com", hash); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	m.SetTTL(-time.Second) // already expired the instant it's created
	if got := m.TTL(); got != -time.Second {
		t.Fatalf("TTL() = %v, want -1s", got)
	}

	sessionID, err := m.Login(ctx, "admin@example.com", "correct-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := m.CurrentUser(ctx, sessionID); err != store.ErrNotFound {
		t.Fatalf("session created under a negative TTL should already be expired: err = %v", err)
	}

	m.SetTTL(time.Hour)
	sessionID2, err := m.Login(ctx, "admin@example.com", "correct-password")
	if err != nil {
		t.Fatalf("Login after restoring TTL: %v", err)
	}
	if _, err := m.CurrentUser(ctx, sessionID2); err != nil {
		t.Fatalf("session created under a 1h TTL should be valid: err = %v", err)
	}
}

func TestCurrentUserEmptySession(t *testing.T) {
	s := newTestStore(t)
	m := NewManager(s, time.Hour)
	if _, err := m.CurrentUser(context.Background(), ""); err != store.ErrNotFound {
		t.Fatalf("empty session: err = %v, want ErrNotFound", err)
	}
}

func TestLogoutWithEmptySessionIDIsANoop(t *testing.T) {
	s := newTestStore(t)
	m := NewManager(s, time.Hour)
	if err := m.Logout(context.Background(), ""); err != nil {
		t.Fatalf("Logout(\"\") = %v, want nil", err)
	}
}

func TestVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct-password") {
		t.Fatal("VerifyPassword should accept the correct password")
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Fatal("VerifyPassword should reject the wrong password")
	}
}
