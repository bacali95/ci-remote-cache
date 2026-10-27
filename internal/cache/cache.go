// Package cache is the protocol-neutral core of the data plane: token
// authorization, the upload size limit, artifact storage, and read
// tracking. Client protocol adapters (internal/protocol/...) translate
// their own HTTP contract onto this and nothing else.
package cache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"nx-remote-cache/internal/auth"
	"nx-remote-cache/internal/storage"
)

// ErrUnauthorized means the bearer token is missing, unknown, or revoked.
var ErrUnauthorized = errors.New("cache: missing or invalid token")

// ErrForbidden means the token is valid but lacks the requested scope.
var ErrForbidden = errors.New("cache: token lacks required scope")

// ErrTooLarge is returned by Put when size exceeds the configured maximum.
var ErrTooLarge = errors.New("cache: artifact exceeds maximum allowed size")

// ReadTracker records cache hits (count + timestamp) for the admin UI and
// the background janitor's "unread" prune rule. Recording is best-effort
// and off the request's critical path — see Get.
type ReadTracker interface {
	RecordCacheRead(ctx context.Context, hash string) error
}

type Service struct {
	backend       storage.Backend
	tokens        *auth.CacheTokenAuth
	reads         ReadTracker
	log           *slog.Logger
	maxEntryBytes atomic.Int64
}

func New(backend storage.Backend, tokens *auth.CacheTokenAuth, reads ReadTracker, log *slog.Logger, maxEntryBytes int64) *Service {
	s := &Service{backend: backend, tokens: tokens, reads: reads, log: log}
	s.SetMaxEntryBytes(maxEntryBytes)
	return s
}

// SetMaxEntryBytes updates the upload size limit enforced on new Puts.
// Safe to call concurrently — see internal/settings, which calls this when
// an admin changes the setting from the UI.
func (s *Service) SetMaxEntryBytes(n int64) {
	s.maxEntryBytes.Store(n)
}

// Authorize returns nil if token grants scope, ErrUnauthorized or
// ErrForbidden if it doesn't, or any other error if the check itself
// failed.
func (s *Service) Authorize(ctx context.Context, token string, scope auth.Scope) error {
	valid, allowed, err := s.tokens.Authorize(ctx, token, scope)
	switch {
	case err != nil:
		return err
	case !valid:
		return ErrUnauthorized
	case !allowed:
		return ErrForbidden
	}
	return nil
}

// Get returns the artifact stored under hash and its size, recording the
// read on success. Returns storage.ErrNotFound on a miss. Callers must
// close the reader.
func (s *Service) Get(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	body, size, err := s.backend.Get(ctx, hash)
	if err != nil {
		return nil, 0, err
	}

	// Best-effort and off the critical path: a slow or failing tracking
	// write must never slow down or fail a cache download. Uses its own
	// context rather than the request's, since the request's is cancelled
	// the moment the handler returns — likely before this goroutine runs.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.reads.RecordCacheRead(ctx, hash); err != nil {
			s.log.Warn("record cache read failed", "hash", hash, "error", err)
		}
	}()

	return body, size, nil
}

// Put stores size bytes from r under hash. Returns ErrTooLarge if size is
// over the limit, or storage.ErrAlreadyExists if hash is already stored.
func (s *Service) Put(ctx context.Context, hash string, r io.Reader, size int64) error {
	if size > s.maxEntryBytes.Load() {
		return ErrTooLarge
	}
	return s.backend.Put(ctx, hash, r, size)
}
