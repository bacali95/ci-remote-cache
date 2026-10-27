package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"ci-remote-cache/internal/db"
	"ci-remote-cache/internal/storage"
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

func newTestEncryptor(t *testing.T) *Encryptor {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	enc, err := NewEncryptor(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

type fakeSessions struct{ ttl time.Duration }

func (f *fakeSessions) SetTTL(d time.Duration) { f.ttl = d }

type fakeServer struct{ maxBytes int64 }

func (f *fakeServer) SetMaxEntryBytes(n int64) { f.maxBytes = n }

func TestLoadAppliesSeededDefaults(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	enc := newTestEncryptor(t)
	dyn := storage.NewDynamic(nil)
	sessions := &fakeSessions{}
	dataSrv := &fakeServer{}

	m := NewManager(st, enc, dyn, sessions, dataSrv)
	if err := m.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := m.Current()
	if got.StorageBackend != store.StorageLocal {
		t.Fatalf("StorageBackend = %q, want local", got.StorageBackend)
	}
	if got.SessionTTL != 24*time.Hour {
		t.Fatalf("SessionTTL = %v, want 24h", got.SessionTTL)
	}
	if got.MaxCacheEntryBytes != 524288000 {
		t.Fatalf("MaxCacheEntryBytes = %d, want 524288000", got.MaxCacheEntryBytes)
	}
	if sessions.ttl != 24*time.Hour {
		t.Fatalf("session manager TTL not applied: got %v", sessions.ttl)
	}
	if dataSrv.maxBytes != 524288000 {
		t.Fatalf("server maxEntryBytes not applied: got %d", dataSrv.maxBytes)
	}

	// The Dynamic backend should now be usable (backed by a real local dir).
	if _, err := dyn.List(ctx, "", "", 1); err != nil {
		t.Fatalf("backend from Load should work: %v", err)
	}
}

func TestApplyLiveSwapsLocalBackendAndSettings(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	enc := newTestEncryptor(t)
	dyn := storage.NewDynamic(nil)
	sessions := &fakeSessions{}
	dataSrv := &fakeServer{}

	m := NewManager(st, enc, dyn, sessions, dataSrv)
	if err := m.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	u, err := st.CreateUser(ctx, "admin@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	newDir := t.TempDir()
	_, err = m.Apply(ctx, ApplyInput{
		StorageBackend:     store.StorageLocal,
		LocalDir:           newDir,
		SessionTTL:         2 * time.Hour,
		MaxCacheEntryBytes: 1024,
		UpdatedBy:          u.ID,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if sessions.ttl != 2*time.Hour {
		t.Fatalf("session TTL not live-updated: got %v", sessions.ttl)
	}
	if dataSrv.maxBytes != 1024 {
		t.Fatalf("maxEntryBytes not live-updated: got %d", dataSrv.maxBytes)
	}

	// Prove the live swap actually happened: put a fake artifact directly
	// in newDir and confirm the Dynamic backend (used by the whole app) can
	// see it.
	local, err := storage.NewLocal(newDir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := local.Put(ctx, "provehash", strings.NewReader("x"), 1); err != nil {
		t.Fatalf("seed newDir: %v", err)
	}
	if exists, err := dyn.Exists(ctx, "provehash"); err != nil || !exists {
		t.Fatalf("Dynamic backend did not swap to newDir: exists=%v err=%v", exists, err)
	}

	// Persisted to DB too, so a restart would pick up the same config.
	raw, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if raw.LocalDir != newDir || raw.SessionTTLSeconds != 7200 || raw.MaxCacheEntryBytes != 1024 {
		t.Fatalf("persisted settings = %+v, want localDir=%q ttl=7200 maxBytes=1024", raw, newDir)
	}
}

func TestApplyRejectsInvalidSettingsWithoutSwapping(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	enc := newTestEncryptor(t)
	dyn := storage.NewDynamic(nil)
	sessions := &fakeSessions{}
	dataSrv := &fakeServer{}

	m := NewManager(st, enc, dyn, sessions, dataSrv)
	if err := m.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	before := m.Current()

	// Missing bucket for s3 — should fail validation before ever touching
	// the network or the DB.
	_, err := m.Apply(ctx, ApplyInput{
		StorageBackend:     store.StorageS3,
		SessionTTL:         time.Hour,
		MaxCacheEntryBytes: 100,
		UpdatedBy:          1,
	})
	if !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("Apply with missing bucket: err = %v, want ErrInvalidSettings", err)
	}

	after := m.Current()
	if after != before {
		t.Fatalf("Current() changed after a rejected Apply: before=%+v after=%+v", before, after)
	}
	if _, err := dyn.List(ctx, "", "", 1); err != nil {
		t.Fatalf("backend should still be the original working one: %v", err)
	}

	raw, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if raw.StorageBackend != store.StorageLocal {
		t.Fatalf("DB settings should be untouched, got backend=%q", raw.StorageBackend)
	}
}

func TestApplyRejectsUnreachableS3WithoutSwapping(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	enc := newTestEncryptor(t)
	dyn := storage.NewDynamic(nil)
	sessions := &fakeSessions{}
	dataSrv := &fakeServer{}

	m := NewManager(st, enc, dyn, sessions, dataSrv)
	if err := m.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	badKey := "not-a-real-key"
	badSecret := "not-a-real-secret"
	_, err := m.Apply(ctx, ApplyInput{
		StorageBackend:     store.StorageS3,
		S3Bucket:           "definitely-does-not-exist-ci-remote-cache-test",
		S3Region:           "us-east-1",
		S3AccessKeyID:      &badKey,
		S3SecretAccessKey:  &badSecret,
		SessionTTL:         time.Hour,
		MaxCacheEntryBytes: 100,
		UpdatedBy:          1,
	})
	if !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("Apply against unreachable S3: err = %v, want ErrInvalidSettings", err)
	}

	if _, err := dyn.List(ctx, "", "", 1); err != nil {
		t.Fatalf("backend should still be the original local one, not swapped to broken s3: %v", err)
	}
}
