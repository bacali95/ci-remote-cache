package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLocalPutGetExists(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	exists, err := l.Exists(ctx, "hash1")
	if err != nil || exists {
		t.Fatalf("Exists before put = (%v, %v), want (false, nil)", exists, err)
	}

	payload := []byte("hello cache")
	if err := l.Put(ctx, "hash1", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	exists, err = l.Exists(ctx, "hash1")
	if err != nil || !exists {
		t.Fatalf("Exists after put = (%v, %v), want (true, nil)", exists, err)
	}

	r, size, err := l.Get(ctx, "hash1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = r.Close() }()
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestLocalPutDuplicateFails(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := l.Put(ctx, "hash1", bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	err = l.Put(ctx, "hash1", bytes.NewReader([]byte("b")), 1)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Put error = %v, want ErrAlreadyExists", err)
	}
}

func TestLocalGetMissing(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	_, _, err = l.Get(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestLocalDelete(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := l.Put(ctx, "hash1", bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := l.Delete(ctx, "hash1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, _ := l.Exists(ctx, "hash1"); exists {
		t.Fatalf("entry still exists after Delete")
	}
	if err := l.Delete(ctx, "hash1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: err = %v, want ErrNotFound", err)
	}
}

func TestLocalListPagination(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	hashes := []string{"a1", "a2", "a3", "a4", "a5"}
	for _, h := range hashes {
		if err := l.Put(ctx, h, bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("Put(%s): %v", h, err)
		}
	}

	page1, err := l.List(ctx, "", 2)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1.Entries) != 2 || page1.Entries[0].Key != "a1" || page1.Entries[1].Key != "a2" {
		t.Fatalf("page1 = %+v, want [a1 a2]", page1.Entries)
	}
	if page1.NextCursor != "a2" {
		t.Fatalf("page1.NextCursor = %q, want a2", page1.NextCursor)
	}

	page2, err := l.List(ctx, page1.NextCursor, 2)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2.Entries) != 2 || page2.Entries[0].Key != "a3" || page2.Entries[1].Key != "a4" {
		t.Fatalf("page2 = %+v, want [a3 a4]", page2.Entries)
	}

	page3, err := l.List(ctx, page2.NextCursor, 2)
	if err != nil {
		t.Fatalf("List page3: %v", err)
	}
	if len(page3.Entries) != 1 || page3.Entries[0].Key != "a5" {
		t.Fatalf("page3 = %+v, want [a5]", page3.Entries)
	}
	if page3.NextCursor != "" {
		t.Fatalf("page3.NextCursor = %q, want empty (last page)", page3.NextCursor)
	}
}

func TestValidHash(t *testing.T) {
	valid := []string{"abc123", "ABC_def-123", "a"}
	invalid := []string{"", "abc/def", "abc..def", "abc$def", "abc def"}
	for _, h := range valid {
		if !ValidHash(h) {
			t.Errorf("ValidHash(%q) = false, want true", h)
		}
	}
	for _, h := range invalid {
		if ValidHash(h) {
			t.Errorf("ValidHash(%q) = true, want false", h)
		}
	}
}

func TestValidKey(t *testing.T) {
	valid := []string{"abc123", "nx/abc123", "turbo/ABC_def-1"}
	invalid := []string{"", "nx/", "/abc", "NX/abc", "a/b/c", "nx/../abc", "../abc", "nx/abc$"}
	for _, k := range valid {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false, want true", k)
		}
	}
	for _, k := range invalid {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true, want false", k)
		}
	}
	if got := Key("nx", "abc"); got != "nx/abc" || !ValidKey(got) {
		t.Errorf("Key(nx, abc) = %q, want valid nx/abc", got)
	}
}

func TestLocalNamespacedKeysAlongsideLegacyRootKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := NewLocal(dir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	for _, k := range []string{"legacy", "nx/b", "turbo/a"} {
		if err := l.Put(ctx, k, bytes.NewReader([]byte(k)), int64(len(k))); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}
	if err := l.Put(ctx, "nx/b", bytes.NewReader([]byte("x")), 1); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate namespaced Put err = %v, want ErrAlreadyExists", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nx", "c.tmp-123"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("write stray temp file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nx", "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	r, size, err := l.Get(ctx, "turbo/a")
	if err != nil {
		t.Fatalf("Get(turbo/a): %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "turbo/a" || size != 7 {
		t.Fatalf("Get(turbo/a) = %q (size %d)", got, size)
	}

	page, err := l.List(ctx, "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var keys []string
	for _, e := range page.Entries {
		keys = append(keys, e.Key)
	}
	if want := []string{"legacy", "nx/b", "turbo/a"}; !slices.Equal(keys, want) {
		t.Fatalf("List keys = %v, want %v", keys, want)
	}

	if err := l.Delete(ctx, "nx/b"); err != nil {
		t.Fatalf("Delete(nx/b): %v", err)
	}
	if exists, _ := l.Exists(ctx, "nx/b"); exists {
		t.Fatal("nx/b still exists after Delete")
	}
}

func TestLocalListFailsWhenNamespaceDirUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	l, err := NewLocal(dir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := l.Put(context.Background(), "nx/a", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	nsDir := filepath.Join(dir, "nx")
	if err := os.Chmod(nsDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(nsDir, 0o755) })

	if _, err := l.List(context.Background(), "", 10); err == nil {
		t.Fatal("List err = nil, want an error for an unreadable namespace dir")
	}
}

func TestLocalPutFailsWhenNamespaceDirCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocal(dir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nx"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	if err := l.Put(context.Background(), "nx/a", bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("Put err = nil, want an error when the namespace path is a file")
	}
}
