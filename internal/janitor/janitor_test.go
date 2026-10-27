package janitor

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"nx-remote-cache/internal/db"
	"nx-remote-cache/internal/storage"
	"nx-remote-cache/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testMaxAge      = 30 * 24 * time.Hour
	testUnreadAfter = 14 * 24 * time.Hour
)

func newTestFixture(t *testing.T) (*storage.Local, *store.Store, *pgxpool.Pool) {
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
	if _, err := pool.Exec(context.Background(), `TRUNCATE cache_artifacts`); err != nil {
		t.Fatalf("truncate cache_artifacts: %v", err)
	}

	backend, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return backend, store.New(pool), pool
}

func seedEntry(t *testing.T, backend *storage.Local, hash string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := backend.Put(ctx, hash, bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("seed %s: %v", hash, err)
	}
	modTime := time.Now().Add(-age)
	if err := os.Chtimes(backend.Path(hash), modTime, modTime); err != nil {
		t.Fatalf("backdate %s: %v", hash, err)
	}
}

// seedRead records a read for hash and then backdates last_read_at to
// `ago` in the past — store.RecordCacheRead always stamps now(), so
// simulating an old read needs a direct SQL override.
func seedRead(t *testing.T, pool *pgxpool.Pool, hash string, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO cache_artifacts (key, read_count, last_read_at) VALUES ($1, 1, now())
		ON CONFLICT (key) DO UPDATE SET read_count = cache_artifacts.read_count + 1, last_read_at = now()
	`, hash); err != nil {
		t.Fatalf("seed read for %s: %v", hash, err)
	}
	if ago > 0 {
		when := time.Now().Add(-ago)
		if _, err := pool.Exec(ctx, `UPDATE cache_artifacts SET last_read_at = $1 WHERE key = $2`, when, hash); err != nil {
			t.Fatalf("backdate read for %s: %v", hash, err)
		}
	}
}

func TestSweepDeletesOldEntriesRegardlessOfReads(t *testing.T) {
	backend, st, pool := newTestFixture(t)
	ctx := context.Background()

	seedEntry(t, backend, "old-unread", 40*24*time.Hour)
	seedEntry(t, backend, "old-read-recently", 40*24*time.Hour)
	seedRead(t, pool, "old-read-recently", 1*time.Hour)

	j := New(backend, st, testLogger(), Config{MaxAge: testMaxAge, UnreadAfter: testUnreadAfter, Interval: time.Hour})
	deleted, err := j.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (age rule ignores read recency)", deleted)
	}
	assertGone(t, backend, "old-unread")
	assertGone(t, backend, "old-read-recently")
}

func TestSweepDeletesUnreadEntriesOldEnoughToJudge(t *testing.T) {
	backend, st, pool := newTestFixture(t)
	ctx := context.Background()

	seedEntry(t, backend, "mid-never-read", 20*24*time.Hour)
	seedEntry(t, backend, "mid-read-long-ago", 20*24*time.Hour)
	seedRead(t, pool, "mid-read-long-ago", 20*24*time.Hour) // read once, but not recently

	j := New(backend, st, testLogger(), Config{MaxAge: testMaxAge, UnreadAfter: testUnreadAfter, Interval: time.Hour})
	deleted, err := j.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	assertGone(t, backend, "mid-never-read")
	assertGone(t, backend, "mid-read-long-ago")
}

func TestSweepKeepsRecentlyReadAndFreshEntries(t *testing.T) {
	backend, st, pool := newTestFixture(t)
	ctx := context.Background()

	seedEntry(t, backend, "mid-read-recently", 20*24*time.Hour)
	seedRead(t, pool, "mid-read-recently", 2*24*time.Hour) // read within the last 14 days

	seedEntry(t, backend, "fresh-unread", 5*24*time.Hour) // too young to judge as "unread" yet

	j := New(backend, st, testLogger(), Config{MaxAge: testMaxAge, UnreadAfter: testUnreadAfter, Interval: time.Hour})
	deleted, err := j.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	assertExists(t, backend, "mid-read-recently")
	assertExists(t, backend, "fresh-unread")
}

func TestSweepClearsReadStatsForDeletedEntries(t *testing.T) {
	backend, st, pool := newTestFixture(t)
	ctx := context.Background()

	seedEntry(t, backend, "old-with-stats", 40*24*time.Hour)
	seedRead(t, pool, "old-with-stats", 1*time.Hour)

	j := New(backend, st, testLogger(), Config{MaxAge: testMaxAge, UnreadAfter: testUnreadAfter, Interval: time.Hour})
	if _, err := j.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	stats, err := st.GetCacheReadStatsBatch(ctx, []string{"old-with-stats"})
	if err != nil {
		t.Fatalf("GetCacheReadStatsBatch: %v", err)
	}
	if _, ok := stats["old-with-stats"]; ok {
		t.Fatalf("expected read stats to be cleared for a deleted entry")
	}
}

func assertGone(t *testing.T, backend *storage.Local, hash string) {
	t.Helper()
	if exists, _ := backend.Exists(context.Background(), hash); exists {
		t.Fatalf("expected %q to be deleted, but it still exists", hash)
	}
}

func assertExists(t *testing.T, backend *storage.Local, hash string) {
	t.Helper()
	if exists, _ := backend.Exists(context.Background(), hash); !exists {
		t.Fatalf("expected %q to still exist, but it was deleted", hash)
	}
}
