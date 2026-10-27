// Package auth implements bearer-token authentication for the cache data
// plane, backed by tokens stored in Postgres (see internal/store).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"ci-remote-cache/internal/store"
)

type Scope int

const (
	ScopeRead Scope = iota
	ScopeWrite
)

// TokenAuthenticator is the subset of *store.Store that CacheTokenAuth
// needs; defined as an interface so tests can inject a fake instead of
// standing up Postgres.
type TokenAuthenticator interface {
	Authenticate(ctx context.Context, tokenHash string) (store.Token, error)
}

// CacheTokenAuth authorizes bearer tokens against the tokens table. A write
// token may also read (it's a superset of read), but a read token may never
// write.
type CacheTokenAuth struct {
	authn TokenAuthenticator
}

func NewCacheTokenAuth(authn TokenAuthenticator) *CacheTokenAuth {
	return &CacheTokenAuth{authn: authn}
}

// Authorize checks token against scope. validToken is false when the token
// is unrecognized or revoked (caller should respond 401). allowed is false
// when the token is valid but lacks the requested scope (caller should
// respond 403).
func (a *CacheTokenAuth) Authorize(ctx context.Context, token string, scope Scope) (validToken, allowed bool, err error) {
	if token == "" {
		return false, false, nil
	}

	tok, err := a.authn.Authenticate(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}

	if scope == ScopeWrite {
		return true, tok.Scope == store.ScopeWrite, nil
	}
	return true, true, nil
}

// HashToken returns the SHA-256 hex digest of a bearer token. Only the hash
// is ever stored or queried; the raw token is shown to the user once, at
// creation time.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateToken returns a new random bearer token, prefixed for
// recognizability (e.g. in commit scanners), similar to GitHub PATs.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "crc_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// ExtractBearer pulls the token out of an "Authorization: Bearer <token>"
// header. Returns "" if the header is missing or malformed.
func ExtractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix))
}
