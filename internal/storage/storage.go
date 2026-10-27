// Package storage defines the cache artifact backend contract and its
// implementations (local filesystem, S3-compatible object storage).
package storage

import (
	"context"
	"errors"
	"io"
	"regexp"
	"time"
)

// ErrNotFound is returned by Get/Stat when the key has no stored artifact.
var ErrNotFound = errors.New("storage: object not found")

// ErrAlreadyExists is returned by Put when an artifact for the key was
// already uploaded. Cache entries are content-addressed and immutable: a
// key is written once and never overwritten.
var ErrAlreadyExists = errors.New("storage: object already exists")

// Backend persists and retrieves cache artifacts by storage key (see Key).
type Backend interface {
	// Exists reports whether an artifact for key has been stored.
	Exists(ctx context.Context, key string) (bool, error)

	// Put stores size bytes read from r under key. It returns
	// ErrAlreadyExists if the key is already present.
	Put(ctx context.Context, key string, r io.Reader, size int64) error

	// Get returns a reader for the artifact stored under key along with
	// its size. Callers must close the reader. Returns ErrNotFound if
	// absent.
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)

	// Delete removes the artifact stored under key. Returns ErrNotFound
	// if absent.
	Delete(ctx context.Context, key string) error

	// List returns up to limit entries starting after cursor (empty cursor
	// starts from the beginning). NextCursor in the returned page is empty
	// once there are no more entries.
	List(ctx context.Context, cursor string, limit int) (ListPage, error)
}

// Entry describes one stored cache artifact, as surfaced by List.
type Entry struct {
	Key     string
	Size    int64
	ModTime time.Time
}

type ListPage struct {
	Entries    []Entry
	NextCursor string
}

// validHash matches the hash formats clients send in the URL path:
// alphanumeric task identifiers. Rejecting anything else prevents path
// traversal and key-injection into object storage.
var validHash = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// validKey is a valid hash, optionally under one lowercase namespace
// segment. Keys stored before namespacing existed are bare hashes.
var validKey = regexp.MustCompile(`^([a-z0-9]{1,32}/)?[a-zA-Z0-9_-]{1,128}$`)

// ValidHash reports whether hash is a well-formed client cache hash.
func ValidHash(hash string) bool {
	return validHash.MatchString(hash)
}

// Key returns the storage key for hash within namespace (one per client
// protocol, e.g. "nx"), so different clients' artifacts never collide.
func Key(namespace, hash string) string {
	return namespace + "/" + hash
}

// ValidKey reports whether key is safe to pass to a Backend: either a
// bare hash or a Key(namespace, hash).
func ValidKey(key string) bool {
	return validKey.MatchString(key)
}
