// Package session implements admin login: bcrypt password verification and
// server-side sessions (the raw session id lives in a cookie, only its
// SHA-256 hash is ever stored, mirroring how cache tokens are handled).
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"time"

	"ci-remote-cache/internal/store"

	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidCredentials = errors.New("session: invalid email or password")

type Manager struct {
	store    *store.Store
	ttlNanos atomic.Int64
}

func NewManager(s *store.Store, ttl time.Duration) *Manager {
	m := &Manager{store: s}
	m.SetTTL(ttl)
	return m
}

// SetTTL updates the session lifetime used for sessions created from now
// on (existing sessions keep whatever expires_at they were given). Safe to
// call concurrently with Login — see internal/settings, which calls this
// when an admin changes the setting from the UI.
func (m *Manager) SetTTL(ttl time.Duration) {
	m.ttlNanos.Store(int64(ttl))
}

func (m *Manager) TTL() time.Duration {
	return time.Duration(m.ttlNanos.Load())
}

// Login verifies credentials and creates a new session, returning the raw
// session id to hand back to the client as a cookie value.
func (m *Manager) Login(ctx context.Context, email, password string) (string, error) {
	u, err := m.store.GetUserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		// Still run a bcrypt compare against a dummy hash so login timing
		// doesn't reveal whether the email exists.
		_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		return "", ErrInvalidCredentials
	}
	if err != nil {
		return "", err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return "", ErrInvalidCredentials
	}

	raw, err := generateID()
	if err != nil {
		return "", err
	}
	if err := m.store.CreateSession(ctx, hashID(raw), u.ID, time.Now().Add(m.TTL())); err != nil {
		return "", err
	}
	return raw, nil
}

func (m *Manager) Logout(ctx context.Context, rawSessionID string) error {
	if rawSessionID == "" {
		return nil
	}
	return m.store.DeleteSession(ctx, hashID(rawSessionID))
}

// CurrentUser resolves a raw session id (from the cookie) to its user.
// Returns store.ErrNotFound if the session is missing, expired, or the
// cookie is empty.
func (m *Manager) CurrentUser(ctx context.Context, rawSessionID string) (store.User, error) {
	if rawSessionID == "" {
		return store.User{}, store.ErrNotFound
	}
	sess, err := m.store.GetSession(ctx, hashID(rawSessionID))
	if err != nil {
		return store.User{}, err
	}
	return m.store.GetUserByID(ctx, sess.UserID)
}

func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func generateID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// A precomputed bcrypt hash with no matching password, used only to keep
// login timing constant when the email doesn't exist.
const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
