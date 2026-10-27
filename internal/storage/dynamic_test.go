package storage

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

func TestDynamicSwap(t *testing.T) {
	ctx := context.Background()
	a, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal a: %v", err)
	}
	b, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal b: %v", err)
	}

	d := NewDynamic(a)
	if err := d.Put(ctx, "hash1", bytes.NewReader([]byte("in-a")), 4); err != nil {
		t.Fatalf("Put via a: %v", err)
	}
	if exists, _ := a.Exists(ctx, "hash1"); !exists {
		t.Fatalf("hash1 should exist directly in backend a")
	}

	d.Swap(b)
	if exists, _ := d.Exists(ctx, "hash1"); exists {
		t.Fatalf("after swap, Dynamic should see backend b (empty), not a's data")
	}
	if err := d.Put(ctx, "hash2", bytes.NewReader([]byte("in-b")), 4); err != nil {
		t.Fatalf("Put via b: %v", err)
	}
	if exists, _ := b.Exists(ctx, "hash2"); !exists {
		t.Fatalf("hash2 should have landed directly in backend b")
	}
	if exists, _ := a.Exists(ctx, "hash2"); exists {
		t.Fatalf("hash2 should not exist in backend a")
	}
}

func TestDynamicGetDeleteList(t *testing.T) {
	ctx := context.Background()
	backend, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	d := NewDynamic(backend)

	if err := d.Put(ctx, "hash1", bytes.NewReader([]byte("payload")), 7); err != nil {
		t.Fatalf("Put: %v", err)
	}

	r, size, err := d.Get(ctx, "hash1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = r.Close() }()
	if size != 7 {
		t.Fatalf("size = %d, want 7", size)
	}

	page, err := d.List(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Key != "hash1" {
		t.Fatalf("List = %+v, want one entry for hash1", page.Entries)
	}

	if err := d.Delete(ctx, "hash1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, _ := d.Exists(ctx, "hash1"); exists {
		t.Fatalf("hash1 should be gone after Delete")
	}
}

func TestDynamicConcurrentSwapIsRaceFree(t *testing.T) {
	ctx := context.Background()
	backend, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	d := NewDynamic(backend)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			_, _ = d.Exists(ctx, "whatever")
		}(i)
		go func() {
			defer wg.Done()
			other, err := NewLocal(t.TempDir())
			if err != nil {
				return
			}
			d.Swap(other)
		}()
	}
	wg.Wait()
}
