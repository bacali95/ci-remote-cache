package store

import (
	"context"
	"errors"
	"testing"
)

func TestRecordCacheReadIncrementsAndStampsLastRead(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.RecordCacheRead(ctx, "hash1"); err != nil {
		t.Fatalf("first RecordCacheRead: %v", err)
	}
	stats := mustBatch(t, s, "hash1")
	if stats.ReadCount != 1 || stats.LastReadAt == nil {
		t.Fatalf("after first read: %+v", stats)
	}
	firstRead := *stats.LastReadAt

	if err := s.RecordCacheRead(ctx, "hash1"); err != nil {
		t.Fatalf("second RecordCacheRead: %v", err)
	}
	stats = mustBatch(t, s, "hash1")
	if stats.ReadCount != 2 {
		t.Fatalf("after second read: ReadCount = %d, want 2", stats.ReadCount)
	}
	if stats.LastReadAt.Before(firstRead) {
		t.Fatalf("last_read_at went backwards")
	}
}

func TestGetCacheReadStatsBatchOmitsUnknownHashes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.RecordCacheRead(ctx, "known"); err != nil {
		t.Fatalf("RecordCacheRead: %v", err)
	}

	batch, err := s.GetCacheReadStatsBatch(ctx, []string{"known", "never-read"})
	if err != nil {
		t.Fatalf("GetCacheReadStatsBatch: %v", err)
	}
	if _, ok := batch["known"]; !ok {
		t.Fatalf("expected stats for 'known'")
	}
	if _, ok := batch["never-read"]; ok {
		t.Fatalf("expected no entry for a hash that was never read")
	}
}

func TestDeleteCacheArtifact(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.RecordCacheRead(ctx, "hash1"); err != nil {
		t.Fatalf("RecordCacheRead: %v", err)
	}
	if err := s.DeleteCacheArtifact(ctx, "hash1"); err != nil {
		t.Fatalf("DeleteCacheArtifact: %v", err)
	}
	batch, err := s.GetCacheReadStatsBatch(ctx, []string{"hash1"})
	if err != nil {
		t.Fatalf("GetCacheReadStatsBatch: %v", err)
	}
	if _, ok := batch["hash1"]; ok {
		t.Fatalf("expected stats to be gone after delete")
	}
}

func TestListAllCacheReadStats(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, h := range []string{"a", "b", "c"} {
		if err := s.RecordCacheRead(ctx, h); err != nil {
			t.Fatalf("RecordCacheRead(%s): %v", h, err)
		}
	}

	all, err := s.ListAllCacheReadStats(ctx)
	if err != nil {
		t.Fatalf("ListAllCacheReadStats: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListAllCacheReadStats returned %d entries, want 3", len(all))
	}
}

func mustBatch(t *testing.T, s *Store, hash string) CacheReadStats {
	t.Helper()
	batch, err := s.GetCacheReadStatsBatch(context.Background(), []string{hash})
	if err != nil {
		t.Fatalf("GetCacheReadStatsBatch: %v", err)
	}
	stats, ok := batch[hash]
	if !ok {
		t.Fatalf("no stats found for %q", hash)
	}
	return stats
}

func TestRecordCacheArtifactRoundtripsMetaAndKeepsReads(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.RecordCacheRead(ctx, "turbo/abc"); err != nil {
		t.Fatalf("RecordCacheRead: %v", err)
	}
	want := ArtifactMeta{DurationMs: 1234, Tag: "sig", SHA: "deadbeef", DirtyHash: "d1"}
	if err := s.RecordCacheArtifact(ctx, "turbo/abc", 42, want); err != nil {
		t.Fatalf("RecordCacheArtifact: %v", err)
	}

	got, err := s.GetCacheArtifactMeta(ctx, "turbo/abc")
	if err != nil {
		t.Fatalf("GetCacheArtifactMeta: %v", err)
	}
	if got != want {
		t.Fatalf("GetCacheArtifactMeta = %+v, want %+v", got, want)
	}
	if stats := mustBatch(t, s, "turbo/abc"); stats.ReadCount != 1 {
		t.Fatalf("ReadCount = %d after recording metadata, want 1 preserved", stats.ReadCount)
	}
}

func TestGetCacheArtifactMetaMissing(t *testing.T) {
	_, err := newTestStore(t).GetCacheArtifactMeta(context.Background(), "nx/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUploadedButUnreadArtifactsAreNotReadStats(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.RecordCacheArtifact(ctx, "nx/unread", 1, ArtifactMeta{}); err != nil {
		t.Fatalf("RecordCacheArtifact: %v", err)
	}
	batch, err := s.GetCacheReadStatsBatch(ctx, []string{"nx/unread"})
	if err != nil {
		t.Fatalf("GetCacheReadStatsBatch: %v", err)
	}
	all, err := s.ListAllCacheReadStats(ctx)
	if err != nil {
		t.Fatalf("ListAllCacheReadStats: %v", err)
	}
	if len(batch) != 0 || len(all) != 0 {
		t.Fatalf("unread artifact reported as read: batch=%v all=%v", batch, all)
	}
}
