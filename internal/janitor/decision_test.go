package janitor

import (
	"testing"
	"time"

	"ci-remote-cache/internal/storage"
	"ci-remote-cache/internal/store"
)

func TestShouldDelete(t *testing.T) {
	now := time.Now()
	ageCutoff := now.Add(-30 * 24 * time.Hour)
	unreadCutoff := now.Add(-14 * 24 * time.Hour)

	ptr := func(t time.Time) *time.Time { return &t }

	cases := []struct {
		name    string
		entry   storage.Entry
		stats   store.CacheReadStats
		wantDel bool
	}{
		{
			name:    "too old, never read",
			entry:   storage.Entry{ModTime: now.Add(-40 * 24 * time.Hour)},
			stats:   store.CacheReadStats{},
			wantDel: true,
		},
		{
			name:    "too old even if read recently",
			entry:   storage.Entry{ModTime: now.Add(-40 * 24 * time.Hour)},
			stats:   store.CacheReadStats{LastReadAt: ptr(now.Add(-time.Hour))},
			wantDel: true,
		},
		{
			name:    "old enough to judge, never read",
			entry:   storage.Entry{ModTime: now.Add(-20 * 24 * time.Hour)},
			stats:   store.CacheReadStats{},
			wantDel: true,
		},
		{
			name:    "old enough to judge, read long ago",
			entry:   storage.Entry{ModTime: now.Add(-20 * 24 * time.Hour)},
			stats:   store.CacheReadStats{LastReadAt: ptr(now.Add(-20 * 24 * time.Hour))},
			wantDel: true,
		},
		{
			name:    "old enough to judge, read recently",
			entry:   storage.Entry{ModTime: now.Add(-20 * 24 * time.Hour)},
			stats:   store.CacheReadStats{LastReadAt: ptr(now.Add(-2 * 24 * time.Hour))},
			wantDel: false,
		},
		{
			name:    "too young to judge, never read",
			entry:   storage.Entry{ModTime: now.Add(-5 * 24 * time.Hour)},
			stats:   store.CacheReadStats{},
			wantDel: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldDelete(tc.entry, tc.stats, ageCutoff, unreadCutoff)
			if got != tc.wantDel {
				t.Fatalf("shouldDelete() = %v, want %v", got, tc.wantDel)
			}
		})
	}
}
