package storage

import (
	"context"
	"io"
	"sync"
)

// Dynamic is a Backend whose underlying implementation can be swapped at
// runtime (see internal/settings), so an admin changing the storage
// backend from the UI takes effect immediately without a restart. Every
// method takes an RLock just long enough to grab the current backend
// reference, so in-flight requests keep running against whichever backend
// they started with even if Swap runs concurrently.
type Dynamic struct {
	mu      sync.RWMutex
	current Backend
}

func NewDynamic(initial Backend) *Dynamic {
	return &Dynamic{current: initial}
}

// Swap atomically replaces the active backend. The old backend keeps
// serving any requests already in flight against it; only new calls see
// the new one.
func (d *Dynamic) Swap(next Backend) {
	d.mu.Lock()
	d.current = next
	d.mu.Unlock()
}

// Active returns the currently active backend. Exported for introspection
// (e.g. tests that need to reach into the concrete backend type).
func (d *Dynamic) Active() Backend {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.current
}

func (d *Dynamic) Exists(ctx context.Context, hash string) (bool, error) {
	return d.Active().Exists(ctx, hash)
}

func (d *Dynamic) Put(ctx context.Context, hash string, r io.Reader, size int64) error {
	return d.Active().Put(ctx, hash, r, size)
}

func (d *Dynamic) Get(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	return d.Active().Get(ctx, hash)
}

func (d *Dynamic) Delete(ctx context.Context, hash string) error {
	return d.Active().Delete(ctx, hash)
}

func (d *Dynamic) List(ctx context.Context, prefix, cursor string, limit int) (ListPage, error) {
	return d.Active().List(ctx, prefix, cursor, limit)
}
