package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Local stores cache artifacts as files on disk. Suited for a single
// long-lived instance with a persistent volume; for multiple replicas or
// ephemeral hosts, use the S3 backend instead.
type Local struct {
	dir string
}

func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &Local{dir: dir}, nil
}

// Path returns the on-disk file path for key: a namespaced key's namespace
// becomes a subdirectory. Exported for operational tooling (and tests) that
// need to touch the underlying file directly.
func (l *Local) Path(key string) string {
	return filepath.Join(l.dir, filepath.FromSlash(key))
}

func (l *Local) Exists(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(l.Path(key))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (l *Local) Put(_ context.Context, key string, r io.Reader, size int64) error {
	dest := l.Path(key)
	if _, err := os.Stat(dest); err == nil {
		return ErrAlreadyExists
	} else if !os.IsNotExist(err) {
		return err
	}

	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create namespace dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(dest)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	n, err := io.Copy(tmp, io.LimitReader(r, size))
	closeErr := tmp.Close()
	if err != nil {
		return fmt.Errorf("write artifact: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close temp file: %w", closeErr)
	}
	if n != size {
		return fmt.Errorf("short write: expected %d bytes, wrote %d", size, n)
	}

	// Atomic rename avoids a second writer racing the same key into a
	// half-written file; the loser's temp file is removed by the defer.
	if err := os.Link(tmpName, dest); err != nil {
		if os.IsExist(err) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("finalize artifact: %w", err)
	}
	return nil
}

func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	f, err := os.Open(l.Path(key))
	if os.IsNotExist(err) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	err := os.Remove(l.Path(key))
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	return err
}

const defaultListLimit = 50

func (l *Local) List(_ context.Context, cursor string, limit int) (ListPage, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}

	names, err := l.keys()
	if err != nil {
		return ListPage{}, err
	}

	start := sort.Search(len(names), func(i int) bool { return names[i] > cursor })
	end := start + limit
	if end > len(names) {
		end = len(names)
	}

	page := ListPage{}
	for _, name := range names[start:end] {
		info, err := os.Stat(l.Path(name))
		if err != nil {
			continue // removed between ReadDir and Stat; skip rather than fail the whole page
		}
		page.Entries = append(page.Entries, Entry{Key: name, Size: info.Size(), ModTime: info.ModTime()})
	}
	if end < len(names) {
		page.NextCursor = names[end-1]
	}
	return page, nil
}

// keys returns every stored key, sorted, so a simple greater-than scan
// gives cursor-based pagination. Bare files at the root are keys stored
// before namespacing; each subdirectory is one namespace.
func (l *Local) keys() ([]string, error) {
	root, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range root {
		if !e.IsDir() {
			if !strings.Contains(e.Name(), ".tmp-") {
				keys = append(keys, e.Name())
			}
			continue
		}
		files, err := os.ReadDir(filepath.Join(l.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if !f.IsDir() && !strings.Contains(f.Name(), ".tmp-") {
				keys = append(keys, e.Name()+"/"+f.Name())
			}
		}
	}
	sort.Strings(keys)
	return keys, nil
}
