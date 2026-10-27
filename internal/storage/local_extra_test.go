package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNewLocalFailsWhenPathIsAFile(t *testing.T) {
	blockingFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}
	if _, err := NewLocal(filepath.Join(blockingFile, "subdir")); err == nil {
		t.Fatal("expected NewLocal to fail when its path is blocked by a file")
	}
}

func TestPutFailsOnShortRead(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	// Declares size=10 but the reader only has 2 bytes — Put should notice
	// the short write rather than silently storing a truncated artifact.
	err = l.Put(ctx, "short", strings.NewReader("ab"), 10)
	if err == nil {
		t.Fatal("expected an error for a reader shorter than the declared size")
	}
	if exists, _ := l.Exists(ctx, "short"); exists {
		t.Fatal("a short write should not leave a finalized artifact behind")
	}
}

// TestPutConcurrentSameHashRace exercises Put's TOCTOU race guard: two
// concurrent Puts for the same hash both pass the initial "does it already
// exist" check, then race on the atomic os.Link finalize step. Exactly one
// must win with a normal nil error; the other must see ErrAlreadyExists
// from the Link call itself (not the earlier Stat check).
func TestPutConcurrentSameHashRace(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes, conflicts int
	start := make(chan struct{})

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			payload := fmt.Appendf(nil, "payload-%d", n)
			err := l.Put(ctx, "racehash", bytes.NewReader(payload), int64(len(payload)))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrAlreadyExists):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (content-addressed writes are once-only)", successes)
	}
	if conflicts != attempts-1 {
		t.Fatalf("conflicts = %d, want %d", conflicts, attempts-1)
	}
}

func TestListFailsWhenDirRemoved(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := NewLocal(dir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := l.List(ctx, "", "", 10); err == nil {
		t.Fatal("expected List to fail once its directory is gone")
	}
}
