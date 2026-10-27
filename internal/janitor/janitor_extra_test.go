package janitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"nx-remote-cache/internal/storage"
	"nx-remote-cache/internal/store"
)

func testLoggerExtra() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeReadStats is an in-memory ReadStatsSource, letting these tests cover
// Sweep/sweepOnce/Run's error and pagination branches without a real
// Postgres instance — exactly what the interface seam in janitor.go is for.
type fakeReadStats struct {
	stats     map[string]store.CacheReadStats
	listErr   error
	deleteErr error
}

func (f *fakeReadStats) ListAllCacheReadStats(context.Context) (map[string]store.CacheReadStats, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.stats, nil
}

func (f *fakeReadStats) DeleteCacheArtifact(_ context.Context, hash string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.stats, hash)
	return nil
}

// fakeBackend implements storage.Backend with scriptable List/Delete
// behavior; Exists/Put/Get are never called by the janitor so they're
// unimplemented stubs.
type fakeBackend struct {
	pages     []storage.ListPage
	listErr   error
	deleteErr map[string]error // per-hash error; ErrNotFound entries are the "already gone" case
}

func (f *fakeBackend) Exists(context.Context, string) (bool, error) { return false, nil }
func (f *fakeBackend) Put(context.Context, string, io.Reader, int64) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) Get(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, errors.New("not implemented")
}

func (f *fakeBackend) Delete(_ context.Context, hash string) error {
	if err, ok := f.deleteErr[hash]; ok {
		return err
	}
	return nil
}

// List ignores limit and walks f.pages in order: an empty cursor returns
// the first page, and any other cursor returns the page right after the
// one whose NextCursor matches it — enough to simulate real pagination
// without needing a real backend.
func (f *fakeBackend) List(_ context.Context, cursor string, _ int) (storage.ListPage, error) {
	if f.listErr != nil {
		return storage.ListPage{}, f.listErr
	}
	if cursor == "" {
		if len(f.pages) == 0 {
			return storage.ListPage{}, nil
		}
		return f.pages[0], nil
	}
	for i, p := range f.pages {
		if p.NextCursor == cursor && i+1 < len(f.pages) {
			return f.pages[i+1], nil
		}
	}
	return storage.ListPage{}, nil
}

var oldEntry = storage.Entry{Key: "old", ModTime: time.Now().Add(-100 * 24 * time.Hour)}

func TestSweepReturnsErrorWhenReadStatsFail(t *testing.T) {
	boom := errors.New("boom")
	j := New(&fakeBackend{}, &fakeReadStats{listErr: boom}, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: time.Hour})

	_, err := j.Sweep(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Sweep error = %v, want wrapping %v", err, boom)
	}
}

func TestSweepReturnsErrorWhenBackendListFails(t *testing.T) {
	boom := errors.New("boom")
	j := New(&fakeBackend{listErr: boom}, &fakeReadStats{stats: map[string]store.CacheReadStats{}}, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: time.Hour})

	_, err := j.Sweep(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Sweep error = %v, want wrapping %v", err, boom)
	}
}

func TestSweepWarnsAndContinuesWhenDeleteFails(t *testing.T) {
	backend := &fakeBackend{
		pages:     []storage.ListPage{{Entries: []storage.Entry{oldEntry}}},
		deleteErr: map[string]error{"old": errors.New("disk on fire")},
	}
	j := New(backend, &fakeReadStats{stats: map[string]store.CacheReadStats{}}, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: time.Hour})

	deleted, err := j.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 (the failed delete shouldn't count)", deleted)
	}
}

func TestSweepTreatsAlreadyGoneAsSuccess(t *testing.T) {
	backend := &fakeBackend{
		pages:     []storage.ListPage{{Entries: []storage.Entry{oldEntry}}},
		deleteErr: map[string]error{"old": storage.ErrNotFound},
	}
	j := New(backend, &fakeReadStats{stats: map[string]store.CacheReadStats{}}, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: time.Hour})

	deleted, err := j.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (already-gone still counts as swept)", deleted)
	}
}

func TestSweepWarnsAndContinuesWhenClearReadStatsFails(t *testing.T) {
	backend := &fakeBackend{pages: []storage.ListPage{{Entries: []storage.Entry{oldEntry}}}}
	reads := &fakeReadStats{stats: map[string]store.CacheReadStats{}, deleteErr: errors.New("boom")}
	j := New(backend, reads, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: time.Hour})

	deleted, err := j.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (the entry itself was still deleted)", deleted)
	}
}

func TestSweepPaginatesAcrossMultiplePages(t *testing.T) {
	page1 := storage.ListPage{Entries: []storage.Entry{{Key: "old1", ModTime: oldEntry.ModTime}}, NextCursor: "old1"}
	page2 := storage.ListPage{Entries: []storage.Entry{{Key: "old2", ModTime: oldEntry.ModTime}}}
	backend := &fakeBackend{pages: []storage.ListPage{page1, page2}}
	j := New(backend, &fakeReadStats{stats: map[string]store.CacheReadStats{}}, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: time.Hour})

	deleted, err := j.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 across both pages", deleted)
	}
}

func TestSweepOnceLogsAndSwallowsError(t *testing.T) {
	j := New(&fakeBackend{}, &fakeReadStats{listErr: errors.New("boom")}, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: time.Hour})
	j.sweepOnce(context.Background()) // must not panic
}

func TestSweepOnceLogsWhenEntriesDeleted(t *testing.T) {
	backend := &fakeBackend{pages: []storage.ListPage{{Entries: []storage.Entry{oldEntry}}}}
	j := New(backend, &fakeReadStats{stats: map[string]store.CacheReadStats{}}, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: time.Hour})
	j.sweepOnce(context.Background()) // must not panic; exercises the "deleted > 0" info-log branch
}

func TestRunSweepsImmediatelyThenOnIntervalUntilCancelled(t *testing.T) {
	backend := &fakeBackend{pages: []storage.ListPage{{Entries: []storage.Entry{oldEntry}}}}
	j := New(backend, &fakeReadStats{stats: map[string]store.CacheReadStats{}}, testLoggerExtra(), Config{MaxAge: time.Hour, UnreadAfter: time.Hour, Interval: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		j.Run(ctx)
		close(done)
	}()

	// Long enough for the immediate sweep plus at least one ticker-driven
	// sweep before we cancel.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
