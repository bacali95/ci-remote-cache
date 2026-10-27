package settings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ci-remote-cache/internal/storage"
	"ci-remote-cache/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

// openTestPool opens a second connection to the same test database that
// newTestStore(t) uses, for tests that need to poke at app_settings
// directly (corrupting a column, seeding pre-encrypted values) in ways the
// Manager/Store API doesn't expose.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func validSettings(backend store.StorageBackendType) Settings {
	s := Settings{
		StorageBackend:     backend,
		SessionTTL:         time.Hour,
		MaxCacheEntryBytes: 100,
	}
	switch backend {
	case store.StorageLocal:
		s.LocalDir = "./data"
	case store.StorageS3:
		s.S3Bucket = "a-bucket"
	case store.StorageGCS:
		s.GCSBucket = "a-bucket"
	}
	return s
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(s Settings) Settings
		wantErr bool
	}{
		{"valid local", func(s Settings) Settings { return validSettings(store.StorageLocal) }, false},
		{"valid s3", func(s Settings) Settings { return validSettings(store.StorageS3) }, false},
		{"valid gcs", func(s Settings) Settings { return validSettings(store.StorageGCS) }, false},
		{"local missing dir", func(Settings) Settings {
			s := validSettings(store.StorageLocal)
			s.LocalDir = ""
			return s
		}, true},
		{"s3 missing bucket", func(Settings) Settings {
			s := validSettings(store.StorageS3)
			s.S3Bucket = ""
			return s
		}, true},
		{"gcs missing bucket", func(Settings) Settings {
			s := validSettings(store.StorageGCS)
			s.GCSBucket = ""
			return s
		}, true},
		{"unknown backend", func(Settings) Settings {
			s := validSettings(store.StorageLocal)
			s.StorageBackend = "ftp"
			return s
		}, true},
		{"zero session ttl", func(Settings) Settings {
			s := validSettings(store.StorageLocal)
			s.SessionTTL = 0
			return s
		}, true},
		{"negative max bytes", func(Settings) Settings {
			s := validSettings(store.StorageLocal)
			s.MaxCacheEntryBytes = -1
			return s
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validate(c.mutate(Settings{}))
			if c.wantErr && !errors.Is(err, ErrInvalidSettings) {
				t.Fatalf("validate() = %v, want ErrInvalidSettings", err)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
		})
	}
}

func TestBuildBackendDefaultsToLocal(t *testing.T) {
	dir := t.TempDir()
	backend, err := buildBackend(context.Background(), Settings{StorageBackend: store.StorageLocal, LocalDir: dir})
	if err != nil {
		t.Fatalf("buildBackend: %v", err)
	}
	if _, ok := backend.(*storage.Local); !ok {
		t.Fatalf("buildBackend(local) = %T, want *storage.Local", backend)
	}
}

// TestLoadSurfacesDecryptFailure corrupts an encrypted settings field
// directly in Postgres (bypassing Apply, which always writes valid
// ciphertext) to exercise Load's decrypt-failure branch — a secret column
// that isn't valid ciphertext under the configured key, e.g. after a key
// rotation without re-encrypting existing rows.
func TestLoadSurfacesDecryptFailure(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	enc := newTestEncryptor(t)
	dyn := storage.NewDynamic(nil)
	m := NewManager(st, enc, dyn, &fakeSessions{}, &fakeServer{})

	pool := openTestPool(t)
	if _, err := pool.Exec(ctx, `UPDATE app_settings SET s3_access_key_id_enc = 'not-valid-ciphertext'`); err != nil {
		t.Fatalf("corrupt s3_access_key_id_enc: %v", err)
	}

	err := m.Load(ctx)
	if err == nil || !strings.Contains(err.Error(), "decrypt s3AccessKeyId") {
		t.Fatalf("Load error = %v, want it to mention decrypt s3AccessKeyId", err)
	}
}

// TestLoadDecryptsAllThreeSecretFields seeds pre-encrypted values (as
// Apply would have written them) for all three secret columns directly, so
// Load's decrypt success path is exercised for S3SecretAccessKey and
// GCSCredentialsJSON too, not just S3AccessKeyID.
func TestLoadDecryptsAllThreeSecretFields(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	enc := newTestEncryptor(t)
	dyn := storage.NewDynamic(nil)
	m := NewManager(st, enc, dyn, &fakeSessions{}, &fakeServer{})

	accessKeyEnc, err := enc.Encrypt("AKIAEXAMPLE")
	if err != nil {
		t.Fatalf("Encrypt access key: %v", err)
	}
	secretEnc, err := enc.Encrypt("shh-its-a-secret")
	if err != nil {
		t.Fatalf("Encrypt secret: %v", err)
	}
	gcsEnc, err := enc.Encrypt(`{"type":"service_account"}`)
	if err != nil {
		t.Fatalf("Encrypt gcs creds: %v", err)
	}

	pool := openTestPool(t)
	if _, err := pool.Exec(ctx, `UPDATE app_settings SET
		s3_access_key_id_enc = $1, s3_secret_access_key_enc = $2, gcs_credentials_json_enc = $3
	`, accessKeyEnc, secretEnc, gcsEnc); err != nil {
		t.Fatalf("seed encrypted fields: %v", err)
	}

	if err := m.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Current() redacts secrets; read the unexported field directly (this
	// test file is in package settings) to see what Load actually decrypted.
	got := m.current
	if !got.S3AccessKeyIDSet || !got.S3SecretAccessKeySet || !got.GCSCredentialsJSONSet {
		t.Fatalf("expected all three *Set flags true, got %+v", got)
	}
	if got.S3AccessKeyID != "AKIAEXAMPLE" || got.S3SecretAccessKey != "shh-its-a-secret" || got.GCSCredentialsJSON != `{"type":"service_account"}` {
		t.Fatalf("decrypted values = %+v, want the plaintext seeded above", got)
	}
}

func TestLoadSurfacesDecryptFailureForSecretAndGCSFields(t *testing.T) {
	for _, column := range []string{"s3_secret_access_key_enc", "gcs_credentials_json_enc"} {
		t.Run(column, func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			enc := newTestEncryptor(t)
			dyn := storage.NewDynamic(nil)
			m := NewManager(st, enc, dyn, &fakeSessions{}, &fakeServer{})

			pool := openTestPool(t)
			if _, err := pool.Exec(ctx, `UPDATE app_settings SET `+column+` = 'not-valid-ciphertext'`); err != nil {
				t.Fatalf("corrupt %s: %v", column, err)
			}

			if err := m.Load(ctx); err == nil {
				t.Fatalf("expected Load to fail with a corrupted %s", column)
			}
		})
	}
}

// TestLoadSurfacesBuildBackendFailure points local_dir at a path that's
// already a regular file, so os.MkdirAll (and therefore buildBackend)
// fails deterministically without needing a real S3/GCS backend.
func TestLoadSurfacesBuildBackendFailure(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	enc := newTestEncryptor(t)
	dyn := storage.NewDynamic(nil)
	m := NewManager(st, enc, dyn, &fakeSessions{}, &fakeServer{})

	blockingFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}

	pool := openTestPool(t)
	if _, err := pool.Exec(ctx, `UPDATE app_settings SET local_dir = $1`, blockingFile); err != nil {
		t.Fatalf("set local_dir: %v", err)
	}

	err := m.Load(ctx)
	if err == nil || !strings.Contains(err.Error(), "build storage backend") {
		t.Fatalf("Load error = %v, want it to mention build storage backend", err)
	}
}

// TestApplySurfacesBuildBackendFailure is TestLoadSurfacesBuildBackendFailure's
// counterpart for Apply: a local_dir that's actually a file makes
// buildBackend fail inside Apply's own call to it, not just Load's.
func TestApplySurfacesBuildBackendFailure(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	enc := newTestEncryptor(t)
	dyn := storage.NewDynamic(nil)
	m := NewManager(st, enc, dyn, &fakeSessions{}, &fakeServer{})
	if err := m.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	blockingFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}

	_, err := m.Apply(ctx, ApplyInput{
		StorageBackend:     store.StorageLocal,
		LocalDir:           blockingFile,
		SessionTTL:         time.Hour,
		MaxCacheEntryBytes: 100,
	})
	if !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("Apply error = %v, want ErrInvalidSettings", err)
	}
}
