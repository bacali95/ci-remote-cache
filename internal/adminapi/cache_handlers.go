package adminapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"nx-remote-cache/internal/storage"
	"nx-remote-cache/internal/store"
)

const (
	defaultListLimit = 50
	maxListLimit     = 500
	maxBulkDelete    = 1000
)

type cacheEntryResponse struct {
	Hash       string     `json:"hash"`
	Size       int64      `json:"size"`
	ModTime    time.Time  `json:"modTime"`
	ReadCount  int64      `json:"readCount"`
	LastReadAt *time.Time `json:"lastReadAt,omitempty"`
}

type listCacheResponse struct {
	Entries    []cacheEntryResponse `json:"entries"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

func (s *Server) handleListCache(w http.ResponseWriter, r *http.Request, _ store.User) {
	cursor := r.URL.Query().Get("cursor")
	limit := defaultListLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxListLimit {
			limit = n
		}
	}

	page, err := s.backend.List(r.Context(), cursor, limit)
	if err != nil {
		s.log.Error("list cache entries failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	hashes := make([]string, len(page.Entries))
	for i, e := range page.Entries {
		hashes[i] = e.Key
	}
	readStats, err := s.store.GetCacheReadStatsBatch(r.Context(), hashes)
	if err != nil {
		// Read stats are supplementary — don't fail the whole listing over
		// them, just show zero reads for this page.
		s.log.Warn("load cache read stats failed", "error", err)
		readStats = map[string]store.CacheReadStats{}
	}

	out := listCacheResponse{NextCursor: page.NextCursor}
	for _, e := range page.Entries {
		entry := cacheEntryResponse{Hash: e.Key, Size: e.Size, ModTime: e.ModTime}
		if stats, ok := readStats[e.Key]; ok {
			entry.ReadCount = stats.ReadCount
			entry.LastReadAt = stats.LastReadAt
		}
		out.Entries = append(out.Entries, entry)
	}

	// Entries that have been read come first, most recently read first;
	// everything else (never read) follows, most recently modified first.
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if (a.LastReadAt != nil) != (b.LastReadAt != nil) {
			return a.LastReadAt != nil
		}
		if a.LastReadAt != nil {
			return a.LastReadAt.After(*b.LastReadAt)
		}
		return a.ModTime.After(b.ModTime)
	})

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteCacheEntry(w http.ResponseWriter, r *http.Request, _ store.User) {
	hash := r.PathValue("hash")
	if !storage.ValidKey(hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}

	err := s.backend.Delete(r.Context(), hash)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.log.Error("delete cache entry failed", "hash", hash, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.clearReadStats(r.Context(), hash)
	w.WriteHeader(http.StatusNoContent)
}

// clearReadStats removes a deleted entry's read-tracking row, best-effort —
// a failure here must never block or fail the delete that already
// succeeded against the storage backend.
func (s *Server) clearReadStats(ctx context.Context, hash string) {
	if err := s.store.DeleteCacheArtifact(ctx, hash); err != nil {
		s.log.Warn("clear cache read stats failed", "hash", hash, "error", err)
	}
}

type bulkDeleteRequest struct {
	Hashes []string `json:"hashes"`
}

type deleteCountResponse struct {
	Deleted int `json:"deleted"`
}

func (s *Server) handleBulkDelete(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req bulkDeleteRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Hashes) > maxBulkDelete {
		writeError(w, http.StatusBadRequest, "too many hashes in one request (max 1000)")
		return
	}

	deleted := 0
	for _, h := range req.Hashes {
		if !storage.ValidKey(h) {
			continue
		}
		err := s.backend.Delete(r.Context(), h)
		if err == nil {
			s.clearReadStats(r.Context(), h)
			deleted++
		} else if !errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("bulk delete: failed to delete entry", "hash", h, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, deleteCountResponse{Deleted: deleted})
}

type pruneRequest struct {
	OlderThanDays int `json:"olderThanDays"`
}

func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req pruneRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.OlderThanDays <= 0 {
		writeError(w, http.StatusBadRequest, "olderThanDays must be positive")
		return
	}

	cutoff := time.Now().Add(-time.Duration(req.OlderThanDays) * 24 * time.Hour)
	deleted, err := s.pruneOlderThan(r.Context(), cutoff)
	if err != nil {
		s.log.Error("prune failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, deleteCountResponse{Deleted: deleted})
}

// pruneOlderThan scans every entry (there's no way to ask most backends for
// "modified before X" server-side) and deletes the ones older than cutoff.
// For very large caches this is O(n) per call — acceptable for a
// self-hosted admin tool, but expect it to take a while on huge buckets.
func (s *Server) pruneOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	cursor := ""
	deleted := 0
	for {
		page, err := s.backend.List(ctx, cursor, 200)
		if err != nil {
			return deleted, err
		}
		for _, e := range page.Entries {
			if e.ModTime.Before(cutoff) {
				if err := s.backend.Delete(ctx, e.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
					return deleted, err
				}
				s.clearReadStats(ctx, e.Key)
				deleted++
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return deleted, nil
}
