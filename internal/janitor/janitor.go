// Package janitor runs a background sweep that prunes cache entries the
// admin has no way to prune from the UI on a schedule: anything old enough
// or unused for long enough, per Config.
package janitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"ci-remote-cache/internal/storage"
	"ci-remote-cache/internal/store"
)

// ReadStatsSource is the subset of *store.Store the janitor needs — kept
// as an interface so tests can use a fake instead of a real Postgres
// instance for the pure scheduling/decision logic.
type ReadStatsSource interface {
	ListAllCacheReadStats(ctx context.Context) (map[string]store.CacheReadStats, error)
	DeleteCacheArtifact(ctx context.Context, hash string) error
}

type Config struct {
	// MaxAge: entries older than this are deleted regardless of whether
	// they've ever been read.
	MaxAge time.Duration

	// UnreadAfter: entries at least this old AND not read within the last
	// UnreadAfter are deleted, even if younger than MaxAge. The age gate
	// matters — without it, a brand-new upload would qualify as "unread"
	// the instant it's created (it can't have been read yet), and get
	// deleted before anyone had a chance to use it.
	UnreadAfter time.Duration

	// Interval: how often to sweep.
	Interval time.Duration
}

type Janitor struct {
	backend storage.Backend
	reads   ReadStatsSource
	log     *slog.Logger
	cfg     Config
}

func New(backend storage.Backend, reads ReadStatsSource, log *slog.Logger, cfg Config) *Janitor {
	return &Janitor{backend: backend, reads: reads, log: log, cfg: cfg}
}

// Run sweeps once immediately, then again every cfg.Interval, until ctx is
// cancelled. Intended to be started in its own goroutine at startup.
func (j *Janitor) Run(ctx context.Context) {
	j.sweepOnce(ctx)

	ticker := time.NewTicker(j.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.sweepOnce(ctx)
		}
	}
}

func (j *Janitor) sweepOnce(ctx context.Context) {
	deleted, err := j.Sweep(ctx)
	if err != nil {
		j.log.Error("janitor sweep failed", "error", err)
		return
	}
	if deleted > 0 {
		j.log.Info("janitor sweep complete", "deleted", deleted)
	}
}

// Sweep runs one pass over every stored entry and deletes the ones that
// match Config's rules. Exported so it can be triggered directly in tests
// without waiting on a ticker.
func (j *Janitor) Sweep(ctx context.Context) (int, error) {
	reads, err := j.reads.ListAllCacheReadStats(ctx)
	if err != nil {
		return 0, fmt.Errorf("load read stats: %w", err)
	}

	now := time.Now()
	ageCutoff := now.Add(-j.cfg.MaxAge)
	unreadCutoff := now.Add(-j.cfg.UnreadAfter)

	deleted := 0
	cursor := ""
	for {
		page, err := j.backend.List(ctx, "", cursor, 200)
		if err != nil {
			return deleted, fmt.Errorf("list entries: %w", err)
		}
		for _, e := range page.Entries {
			if !shouldDelete(e, reads[e.Key], ageCutoff, unreadCutoff) {
				continue
			}
			if err := j.backend.Delete(ctx, e.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				j.log.Warn("janitor: failed to delete entry", "hash", e.Key, "error", err)
				continue
			}
			if err := j.reads.DeleteCacheArtifact(ctx, e.Key); err != nil {
				j.log.Warn("janitor: failed to clear read stats", "hash", e.Key, "error", err)
			}
			deleted++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return deleted, nil
}

func shouldDelete(e storage.Entry, stats store.CacheReadStats, ageCutoff, unreadCutoff time.Time) bool {
	if e.ModTime.Before(ageCutoff) {
		return true
	}
	if e.ModTime.Before(unreadCutoff) && (stats.LastReadAt == nil || stats.LastReadAt.Before(unreadCutoff)) {
		return true
	}
	return false
}
