// Package settings owns the app's runtime configuration (storage backend,
// session TTL, max cache entry size): loading it from Postgres at startup,
// decrypting secret fields, and applying changes from the admin UI live —
// building and connectivity-testing a new storage backend before atomically
// swapping it in, with no restart required.
package settings

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"ci-remote-cache/internal/storage"
	"ci-remote-cache/internal/store"
)

// ErrInvalidSettings wraps user-fixable problems (missing bucket, bad
// credentials, unreachable endpoint) — callers should surface these as 400s
// rather than 500s.
var ErrInvalidSettings = errors.New("settings: invalid configuration")

// sessionTTLSetter and maxEntryBytesSetter are the minimal interfaces this
// package needs from *session.Manager and *cache.Service, kept local so
// settings can be unit tested without wiring up either of those (or a real
// HTTP/DB stack) — see settings_test.go's fakes.
type sessionTTLSetter interface {
	SetTTL(time.Duration)
}

type maxEntryBytesSetter interface {
	SetMaxEntryBytes(int64)
}

// Settings is the decrypted, in-memory view of app_settings. Secret fields
// only ever exist as plaintext here, in the server process's memory, for as
// long as it takes to build a storage client — never logged, never
// returned from the admin API (see Current, which redacts them).
type Settings struct {
	StorageBackend store.StorageBackendType
	LocalDir       string

	S3Bucket             string
	S3Region             string
	S3Prefix             string
	S3Endpoint           string
	S3UsePathStyle       bool
	S3AccessKeyID        string
	S3AccessKeyIDSet     bool
	S3SecretAccessKey    string
	S3SecretAccessKeySet bool

	GCSBucket             string
	GCSPrefix             string
	GCSCredentialsJSON    string
	GCSCredentialsJSONSet bool

	SessionTTL         time.Duration
	MaxCacheEntryBytes int64

	UpdatedAt time.Time
	UpdatedBy *int64
}

// redacted strips secret plaintext, keeping only the *Set booleans — this
// is what the admin API's GET handler serializes.
func (s Settings) redacted() Settings {
	s.S3AccessKeyID = ""
	s.S3SecretAccessKey = ""
	s.GCSCredentialsJSON = ""
	return s
}

// ApplyInput mirrors an admin API update request. Secret fields are
// pointers so the caller can distinguish "leave unchanged" (nil) from
// "clear" (points to "") from "set" (points to a non-empty value) — the
// raw secret is never sent back to the browser, so the UI has no other way
// to say "keep what's already there."
type ApplyInput struct {
	StorageBackend store.StorageBackendType
	LocalDir       string

	S3Bucket          string
	S3Region          string
	S3Prefix          string
	S3Endpoint        string
	S3UsePathStyle    bool
	S3AccessKeyID     *string
	S3SecretAccessKey *string

	GCSBucket          string
	GCSPrefix          string
	GCSCredentialsJSON *string

	SessionTTL         time.Duration
	MaxCacheEntryBytes int64

	UpdatedBy int64
}

type Manager struct {
	mu       sync.RWMutex
	store    *store.Store
	enc      *Encryptor
	backend  *storage.Dynamic
	sessions sessionTTLSetter
	dataSrv  maxEntryBytesSetter

	current Settings
}

func NewManager(st *store.Store, enc *Encryptor, backend *storage.Dynamic, sessions sessionTTLSetter, dataSrv maxEntryBytesSetter) *Manager {
	return &Manager{store: st, enc: enc, backend: backend, sessions: sessions, dataSrv: dataSrv}
}

// Load reads settings from Postgres, decrypts secrets, builds the storage
// backend they describe, and applies everything (backend swap, session
// TTL, max entry bytes). Call once at startup — if this fails, the server
// has no working storage backend and should not start.
func (m *Manager) Load(ctx context.Context) error {
	raw, err := m.store.GetSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	s, err := m.decrypt(raw)
	if err != nil {
		return fmt.Errorf("decrypt settings: %w", err)
	}

	backend, err := buildBackend(ctx, s)
	if err != nil {
		return fmt.Errorf("build storage backend: %w", err)
	}

	m.mu.Lock()
	m.backend.Swap(backend)
	m.sessions.SetTTL(s.SessionTTL)
	m.dataSrv.SetMaxEntryBytes(s.MaxCacheEntryBytes)
	m.current = s
	m.mu.Unlock()
	return nil
}

// Current returns the last-applied settings with secrets redacted, for the
// GET /admin/api/settings handler.
func (m *Manager) Current() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current.redacted()
}

// Apply validates the desired settings, builds and connectivity-tests the
// storage backend they describe, and only then persists (encrypting
// secrets) and swaps live. Nothing changes if validation or the
// connectivity test fails.
func (m *Manager) Apply(ctx context.Context, in ApplyInput) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.current
	next.StorageBackend = in.StorageBackend
	next.LocalDir = in.LocalDir
	next.S3Bucket = in.S3Bucket
	next.S3Region = in.S3Region
	next.S3Prefix = in.S3Prefix
	next.S3Endpoint = in.S3Endpoint
	next.S3UsePathStyle = in.S3UsePathStyle
	next.GCSBucket = in.GCSBucket
	next.GCSPrefix = in.GCSPrefix
	next.SessionTTL = in.SessionTTL
	next.MaxCacheEntryBytes = in.MaxCacheEntryBytes

	if in.S3AccessKeyID != nil {
		next.S3AccessKeyID = *in.S3AccessKeyID
		next.S3AccessKeyIDSet = *in.S3AccessKeyID != ""
	}
	if in.S3SecretAccessKey != nil {
		next.S3SecretAccessKey = *in.S3SecretAccessKey
		next.S3SecretAccessKeySet = *in.S3SecretAccessKey != ""
	}
	if in.GCSCredentialsJSON != nil {
		next.GCSCredentialsJSON = *in.GCSCredentialsJSON
		next.GCSCredentialsJSONSet = *in.GCSCredentialsJSON != ""
	}

	if err := validate(next); err != nil {
		return Settings{}, err
	}

	newBackend, err := buildBackend(ctx, next)
	if err != nil {
		return Settings{}, fmt.Errorf("%w: connect to storage backend: %v", ErrInvalidSettings, err)
	}
	if _, err := newBackend.List(ctx, "", "", 1); err != nil {
		return Settings{}, fmt.Errorf("%w: storage backend connectivity check failed: %v", ErrInvalidSettings, err)
	}

	rawToStore, err := m.encryptForStorage(next)
	if err != nil {
		return Settings{}, fmt.Errorf("encrypt settings: %w", err)
	}
	if err := m.store.UpdateSettings(ctx, rawToStore, in.UpdatedBy); err != nil {
		return Settings{}, fmt.Errorf("persist settings: %w", err)
	}

	m.backend.Swap(newBackend)
	m.sessions.SetTTL(next.SessionTTL)
	m.dataSrv.SetMaxEntryBytes(next.MaxCacheEntryBytes)
	next.UpdatedAt = time.Now()
	m.current = next

	return next.redacted(), nil
}

func validate(s Settings) error {
	switch s.StorageBackend {
	case store.StorageLocal:
		if s.LocalDir == "" {
			return fmt.Errorf("%w: localDir is required for the local backend", ErrInvalidSettings)
		}
	case store.StorageS3:
		if s.S3Bucket == "" {
			return fmt.Errorf("%w: s3Bucket is required for the s3 backend", ErrInvalidSettings)
		}
	case store.StorageGCS:
		if s.GCSBucket == "" {
			return fmt.Errorf("%w: gcsBucket is required for the gcs backend", ErrInvalidSettings)
		}
	default:
		return fmt.Errorf("%w: storageBackend must be \"local\", \"s3\", or \"gcs\"", ErrInvalidSettings)
	}
	if s.SessionTTL <= 0 {
		return fmt.Errorf("%w: sessionTtlSeconds must be positive", ErrInvalidSettings)
	}
	if s.MaxCacheEntryBytes <= 0 {
		return fmt.Errorf("%w: maxCacheEntryBytes must be positive", ErrInvalidSettings)
	}
	return nil
}

func buildBackend(ctx context.Context, s Settings) (storage.Backend, error) {
	switch s.StorageBackend {
	case store.StorageS3:
		return storage.NewS3(ctx, storage.S3Options{
			Bucket:          s.S3Bucket,
			Region:          s.S3Region,
			Prefix:          s.S3Prefix,
			Endpoint:        s.S3Endpoint,
			UsePathStyle:    s.S3UsePathStyle,
			AccessKeyID:     s.S3AccessKeyID,
			SecretAccessKey: s.S3SecretAccessKey,
		})
	case store.StorageGCS:
		var creds []byte
		if s.GCSCredentialsJSON != "" {
			creds = []byte(s.GCSCredentialsJSON)
		}
		return storage.NewGCS(ctx, storage.GCSOptions{
			Bucket:          s.GCSBucket,
			Prefix:          s.GCSPrefix,
			CredentialsJSON: creds,
		})
	default:
		return storage.NewLocal(s.LocalDir)
	}
}

func (m *Manager) decrypt(raw store.AppSettings) (Settings, error) {
	s := Settings{
		StorageBackend:     raw.StorageBackend,
		LocalDir:           raw.LocalDir,
		S3Bucket:           raw.S3Bucket,
		S3Region:           raw.S3Region,
		S3Prefix:           raw.S3Prefix,
		S3Endpoint:         raw.S3Endpoint,
		S3UsePathStyle:     raw.S3UsePathStyle,
		GCSBucket:          raw.GCSBucket,
		GCSPrefix:          raw.GCSPrefix,
		SessionTTL:         time.Duration(raw.SessionTTLSeconds) * time.Second,
		MaxCacheEntryBytes: raw.MaxCacheEntryBytes,
		UpdatedAt:          raw.UpdatedAt,
		UpdatedBy:          raw.UpdatedBy,
	}

	if raw.S3AccessKeyIDEnc != nil {
		pt, err := m.enc.Decrypt(*raw.S3AccessKeyIDEnc)
		if err != nil {
			return Settings{}, fmt.Errorf("decrypt s3AccessKeyId: %w", err)
		}
		s.S3AccessKeyID, s.S3AccessKeyIDSet = pt, true
	}
	if raw.S3SecretAccessKeyEnc != nil {
		pt, err := m.enc.Decrypt(*raw.S3SecretAccessKeyEnc)
		if err != nil {
			return Settings{}, fmt.Errorf("decrypt s3SecretAccessKey: %w", err)
		}
		s.S3SecretAccessKey, s.S3SecretAccessKeySet = pt, true
	}
	if raw.GCSCredentialsJSONEnc != nil {
		pt, err := m.enc.Decrypt(*raw.GCSCredentialsJSONEnc)
		if err != nil {
			return Settings{}, fmt.Errorf("decrypt gcsCredentialsJson: %w", err)
		}
		s.GCSCredentialsJSON, s.GCSCredentialsJSONSet = pt, true
	}
	return s, nil
}

func (m *Manager) encryptForStorage(s Settings) (store.AppSettings, error) {
	raw := store.AppSettings{
		StorageBackend:     s.StorageBackend,
		LocalDir:           s.LocalDir,
		S3Bucket:           s.S3Bucket,
		S3Region:           s.S3Region,
		S3Prefix:           s.S3Prefix,
		S3Endpoint:         s.S3Endpoint,
		S3UsePathStyle:     s.S3UsePathStyle,
		GCSBucket:          s.GCSBucket,
		GCSPrefix:          s.GCSPrefix,
		SessionTTLSeconds:  int(s.SessionTTL / time.Second),
		MaxCacheEntryBytes: s.MaxCacheEntryBytes,
	}

	if s.S3AccessKeyID != "" {
		enc, err := m.enc.Encrypt(s.S3AccessKeyID)
		if err != nil {
			return store.AppSettings{}, fmt.Errorf("encrypt s3AccessKeyId: %w", err)
		}
		raw.S3AccessKeyIDEnc = &enc
	}
	if s.S3SecretAccessKey != "" {
		enc, err := m.enc.Encrypt(s.S3SecretAccessKey)
		if err != nil {
			return store.AppSettings{}, fmt.Errorf("encrypt s3SecretAccessKey: %w", err)
		}
		raw.S3SecretAccessKeyEnc = &enc
	}
	if s.GCSCredentialsJSON != "" {
		enc, err := m.enc.Encrypt(s.GCSCredentialsJSON)
		if err != nil {
			return store.AppSettings{}, fmt.Errorf("encrypt gcsCredentialsJson: %w", err)
		}
		raw.GCSCredentialsJSONEnc = &enc
	}
	return raw, nil
}
