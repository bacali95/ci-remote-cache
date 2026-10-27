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
	"strings"
	"sync/atomic"
	"time"

	"nx-remote-cache/internal/auth"
	"nx-remote-cache/internal/storage"
	"nx-remote-cache/internal/store"
)

// ErrUnauthorized means the bearer token is missing, unknown, or revoked.
var ErrUnauthorized = errors.New("cache: missing or invalid token")

// ErrForbidden means the token is valid but lacks the requested scope.
var ErrForbidden = errors.New("cache: token lacks required scope")

// ErrTooLarge is returned by Put when size exceeds the configured maximum.
var ErrTooLarge = errors.New("cache: artifact exceeds maximum allowed size")

// legacyNamespace owns the bare-hash keys at the storage root, written
// before keys were namespaced. Reads in this namespace fall back to them;
// the janitor's max-age rule removes them over time.
const legacyNamespace = "nx"

// Tracker persists per-artifact rows alongside the storage backend: read
// activity for the admin UI and the janitor's "unread" prune rule, and
// client-supplied metadata. Recording is best-effort — a tracking failure
// never fails a cache request (see Get and Put).
type Tracker interface {
	RecordCacheRead(ctx context.Context, key string) error
	RecordCacheArtifact(ctx context.Context, key string, size int64, meta store.ArtifactMeta) error
	GetCacheArtifactMeta(ctx context.Context, key string) (store.ArtifactMeta, error)
	GetCacheArtifactInfoBatch(ctx context.Context, keys []string) (map[string]store.ArtifactInfo, error)
}

type Service struct {
	backend       storage.Backend
	tokens        *auth.CacheTokenAuth
	tracker       Tracker
	log           *slog.Logger
	maxEntryBytes atomic.Int64
}

func New(backend storage.Backend, tokens *auth.CacheTokenAuth, tracker Tracker, log *slog.Logger, maxEntryBytes int64) *Service {
	s := &Service{backend: backend, tokens: tokens, tracker: tracker, log: log}
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

// Get returns the artifact stored for hash in namespace and its size,
// recording the read on success. Returns storage.ErrNotFound on a miss.
// Callers must close the reader.
func (s *Service) Get(ctx context.Context, namespace, hash string) (io.ReadCloser, int64, error) {
	key := storage.Key(namespace, hash)
	body, size, err := s.backend.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) && namespace == legacyNamespace {
		key = hash
		body, size, err = s.backend.Get(ctx, key)
	}
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
		if err := s.tracker.RecordCacheRead(ctx, key); err != nil {
			s.log.Warn("record cache read failed", "key", key, "error", err)
		}
	}()

	return body, size, nil
}

// Exists reports whether an artifact is stored for hash in namespace,
// following the same legacy-key fallback as Get.
func (s *Service) Exists(ctx context.Context, namespace, hash string) (bool, error) {
	ok, err := s.backend.Exists(ctx, storage.Key(namespace, hash))
	if err == nil && !ok && namespace == legacyNamespace {
		return s.backend.Exists(ctx, hash)
	}
	return ok, err
}

// Put stores size bytes from r for hash in namespace, along with meta.
// Returns ErrTooLarge if size is over the limit, or
// storage.ErrAlreadyExists if the artifact is already stored.
func (s *Service) Put(ctx context.Context, namespace, hash string, r io.Reader, size int64, meta store.ArtifactMeta) error {
	if size > s.maxEntryBytes.Load() {
		return ErrTooLarge
	}
	key := storage.Key(namespace, hash)
	if err := s.backend.Put(ctx, key, r, size); err != nil {
		return err
	}
	if err := s.tracker.RecordCacheArtifact(ctx, key, size, meta); err != nil {
		s.log.Warn("record cache artifact failed", "key", key, "error", err)
	}
	return nil
}

// Meta returns the metadata uploaded with hash in namespace, or the zero
// value if none was recorded.
func (s *Service) Meta(ctx context.Context, namespace, hash string) (store.ArtifactMeta, error) {
	meta, err := s.tracker.GetCacheArtifactMeta(ctx, storage.Key(namespace, hash))
	if errors.Is(err, store.ErrNotFound) {
		return store.ArtifactMeta{}, nil
	}
	return meta, err
}

// Query returns size and metadata for each of hashes in namespace that
// has a recorded upload, keyed by hash. Hashes without one are absent.
func (s *Service) Query(ctx context.Context, namespace string, hashes []string) (map[string]store.ArtifactInfo, error) {
	keys := make([]string, len(hashes))
	for i, h := range hashes {
		keys[i] = storage.Key(namespace, h)
	}
	byKey, err := s.tracker.GetCacheArtifactInfoBatch(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.ArtifactInfo, len(byKey))
	for i, key := range keys {
		if info, ok := byKey[key]; ok {
			out[hashes[i]] = info
		}
	}
	return out, nil
}

// SplitKey returns the protocol namespace and hash a storage key belongs
// to. legacy is true for bare keys stored before namespacing, which belong
// to the legacy namespace.
func SplitKey(key string) (namespace, hash string, legacy bool) {
	if ns, h, ok := strings.Cut(key, "/"); ok {
		return ns, h, false
	}
	return legacyNamespace, key, true
}
