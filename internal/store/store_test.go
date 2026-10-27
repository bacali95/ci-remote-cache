package store

import (
	"context"
	"os"
	"testing"
	"time"

	"ci-remote-cache/internal/db"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestStore connects to TEST_DATABASE_URL, applies migrations, and
// truncates all tables so each test starts from a clean slate. Skips if the
// env var is unset (no Postgres available).
func newTestStore(t *testing.T) *Store {
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

	// app_settings.updated_by references users(id), so TRUNCATE ... CASCADE
	// on users also empties the settings singleton — reseed its one row.
	if _, err := pool.Exec(context.Background(), `TRUNCATE users, sessions, tokens, cache_artifacts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO app_settings (id) VALUES (true)`); err != nil {
		t.Fatalf("reseed app_settings: %v", err)
	}

	return New(pool)
}

func TestUserLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, "admin@example.com", "hashed-password")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID == 0 {
		t.Fatalf("expected non-zero ID")
	}

	if _, err := s.CreateUser(ctx, "admin@example.com", "other-hash"); err != ErrConflict {
		t.Fatalf("duplicate email: err = %v, want ErrConflict", err)
	}

	got, err := s.GetUserByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("GetUserByEmail returned different user")
	}

	if _, err := s.GetUserByEmail(ctx, "nope@example.com"); err != ErrNotFound {
		t.Fatalf("missing user: err = %v, want ErrNotFound", err)
	}

	n, err := s.CountUsers(ctx)
	if err != nil || n != 1 {
		t.Fatalf("CountUsers = (%d, %v), want (1, nil)", n, err)
	}

	if err := s.UpdatePassword(ctx, u.ID, "new-hash"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	got, _ = s.GetUserByID(ctx, u.ID)
	if got.PasswordHash != "new-hash" {
		t.Fatalf("password hash not updated")
	}

	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := s.DeleteUser(ctx, u.ID); err != ErrNotFound {
		t.Fatalf("delete again: err = %v, want ErrNotFound", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, "admin@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := s.CreateSession(ctx, "sesshash1", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sess, err := s.GetSession(ctx, "sesshash1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.UserID != u.ID {
		t.Fatalf("session user mismatch")
	}

	if err := s.CreateSession(ctx, "expired", u.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession (expired): %v", err)
	}
	if _, err := s.GetSession(ctx, "expired"); err != ErrNotFound {
		t.Fatalf("expired session: err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteSession(ctx, "sesshash1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, "sesshash1"); err != ErrNotFound {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
}

func TestTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, "admin@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	tok, err := s.CreateToken(ctx, "ci-write", ScopeWrite, "tokhash1", u.ID)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if tok.RevokedAt != nil {
		t.Fatalf("new token should not be revoked")
	}

	got, err := s.Authenticate(ctx, "tokhash1")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.Scope != ScopeWrite || got.LastUsedAt == nil {
		t.Fatalf("Authenticate did not stamp last_used_at: %+v", got)
	}

	if _, err := s.Authenticate(ctx, "does-not-exist"); err != ErrNotFound {
		t.Fatalf("unknown token: err = %v, want ErrNotFound", err)
	}

	tokens, err := s.ListTokens(ctx)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("ListTokens = (%d, %v), want (1, nil)", len(tokens), err)
	}

	if err := s.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := s.Authenticate(ctx, "tokhash1"); err != ErrNotFound {
		t.Fatalf("revoked token still authenticates: err = %v", err)
	}
	if err := s.RevokeToken(ctx, tok.ID); err != ErrNotFound {
		t.Fatalf("revoke again: err = %v, want ErrNotFound", err)
	}
}
