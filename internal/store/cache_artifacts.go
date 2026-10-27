package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type CacheReadStats struct {
	ReadCount  int64
	LastReadAt *time.Time
}

// ArtifactMeta is client-supplied metadata stored alongside an artifact
// and handed back on download (Turborepo's x-artifact-* headers). Clients
// that send none (Nx) get the zero value.
type ArtifactMeta struct {
	DurationMs int64
	Tag        string
	SHA        string
	DirtyHash  string
}

// RecordCacheArtifact stores key's size, upload time and metadata,
// creating the row if this is the first time key has been seen. Existing
// read stats are preserved.
func (s *Store) RecordCacheArtifact(ctx context.Context, key string, size int64, meta ArtifactMeta) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cache_artifacts (key, size, created_at, duration_ms, tag, sha, dirty_hash)
		VALUES ($1, $2, now(), $3, $4, $5, $6)
		ON CONFLICT (key) DO UPDATE SET
			size = EXCLUDED.size,
			created_at = EXCLUDED.created_at,
			duration_ms = EXCLUDED.duration_ms,
			tag = EXCLUDED.tag,
			sha = EXCLUDED.sha,
			dirty_hash = EXCLUDED.dirty_hash
	`, key, size, meta.DurationMs, meta.Tag, meta.SHA, meta.DirtyHash)
	return err
}

// GetCacheArtifactMeta returns the metadata recorded for key, or
// ErrNotFound if key has no row.
func (s *Store) GetCacheArtifactMeta(ctx context.Context, key string) (ArtifactMeta, error) {
	var meta ArtifactMeta
	err := s.pool.QueryRow(ctx,
		`SELECT duration_ms, tag, sha, dirty_hash FROM cache_artifacts WHERE key = $1`, key,
	).Scan(&meta.DurationMs, &meta.Tag, &meta.SHA, &meta.DirtyHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArtifactMeta{}, ErrNotFound
	}
	return meta, err
}

type ArtifactInfo struct {
	Size int64
	Meta ArtifactMeta
}

// GetCacheArtifactInfoBatch returns size and metadata for the given keys.
// Keys with no row, or whose row predates upload tracking (no size), are
// absent from the result.
func (s *Store) GetCacheArtifactInfoBatch(ctx context.Context, keys []string) (map[string]ArtifactInfo, error) {
	out := make(map[string]ArtifactInfo, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT key, size, duration_ms, tag, sha, dirty_hash FROM cache_artifacts
		WHERE key = ANY($1) AND size IS NOT NULL
	`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var info ArtifactInfo
		if err := rows.Scan(&key, &info.Size, &info.Meta.DurationMs, &info.Meta.Tag, &info.Meta.SHA, &info.Meta.DirtyHash); err != nil {
			return nil, err
		}
		out[key] = info
	}
	return out, rows.Err()
}

// RecordCacheRead increments the read counter and stamps last_read_at for
// key, creating the row on first read.
func (s *Store) RecordCacheRead(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cache_artifacts (key, read_count, last_read_at)
		VALUES ($1, 1, now())
		ON CONFLICT (key) DO UPDATE SET
			read_count = cache_artifacts.read_count + 1,
			last_read_at = now()
	`, key)
	return err
}

// DeleteCacheArtifact removes key's row. Call this whenever the underlying
// cache entry is deleted (single delete, bulk delete, prune, or the
// janitor sweep) so rows don't accumulate for keys that no longer exist.
func (s *Store) DeleteCacheArtifact(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM cache_artifacts WHERE key = $1`, key)
	return err
}

// GetCacheReadStatsBatch returns stats for exactly the given keys (missing
// keys are simply absent from the result, meaning "never read"). Sized for
// enriching one page of the admin cache list.
func (s *Store) GetCacheReadStatsBatch(ctx context.Context, keys []string) (map[string]CacheReadStats, error) {
	out := make(map[string]CacheReadStats, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT key, read_count, last_read_at FROM cache_artifacts WHERE key = ANY($1) AND read_count > 0`,
		keys,
	)
	if err != nil {
		return nil, err
	}
	return scanReadStats(rows)
}

// ListAllCacheReadStats returns every read key's stats in one query, for
// the janitor's full-table sweep (avoids one query per stored entry).
func (s *Store) ListAllCacheReadStats(ctx context.Context) (map[string]CacheReadStats, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key, read_count, last_read_at FROM cache_artifacts WHERE read_count > 0`)
	if err != nil {
		return nil, err
	}
	return scanReadStats(rows)
}

func scanReadStats(rows pgx.Rows) (map[string]CacheReadStats, error) {
	defer rows.Close()
	out := make(map[string]CacheReadStats)
	for rows.Next() {
		var key string
		var stats CacheReadStats
		if err := rows.Scan(&key, &stats.ReadCount, &stats.LastReadAt); err != nil {
			return nil, err
		}
		out[key] = stats
	}
	return out, rows.Err()
}
