package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"nx-remote-cache/internal/auth"
	"nx-remote-cache/internal/storage"
	"nx-remote-cache/internal/store"
)

const (
	readToken  = "read-token"
	writeToken = "write-token"
)

type fakeAuthenticator struct {
	byHash map[string]store.Token
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, tokenHash string) (store.Token, error) {
	tok, ok := f.byHash[tokenHash]
	if !ok {
		return store.Token{}, store.ErrNotFound
	}
	return tok, nil
}

// fakeReadTracker returns err from every call and reports each hash on
// notify, since Get records reads in a goroutine.
type fakeReadTracker struct {
	err    error
	notify chan string
}

func (f *fakeReadTracker) RecordCacheRead(_ context.Context, hash string) error {
	f.notify <- hash
	return f.err
}

// syncBuffer guards a bytes.Buffer that the read-tracking goroutine logs
// into while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestService(t *testing.T, reads ReadTracker, logOut io.Writer) *Service {
	t.Helper()
	backend, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	tokens := auth.NewCacheTokenAuth(&fakeAuthenticator{byHash: map[string]store.Token{
		auth.HashToken(readToken):  {Scope: store.ScopeRead},
		auth.HashToken(writeToken): {Scope: store.ScopeWrite},
	}})
	return New(backend, tokens, reads, slog.New(slog.NewTextHandler(logOut, nil)), 10)
}

func waitForRead(t *testing.T, reads *fakeReadTracker) string {
	t.Helper()
	select {
	case hash := <-reads.notify:
		return hash
	case <-time.After(2 * time.Second):
		t.Fatal("RecordCacheRead was not called within 2s")
		return ""
	}
}

func TestAuthorizeDelegatesToTokenAuth(t *testing.T) {
	svc := newTestService(t, &fakeReadTracker{notify: make(chan string, 1)}, io.Discard)
	ctx := context.Background()

	tests := []struct {
		token string
		scope auth.Scope
		want  error
	}{
		{"", auth.ScopeRead, ErrUnauthorized},
		{"unknown", auth.ScopeRead, ErrUnauthorized},
		{readToken, auth.ScopeRead, nil},
		{readToken, auth.ScopeWrite, ErrForbidden},
		{writeToken, auth.ScopeWrite, nil},
	}
	for _, tt := range tests {
		if err := svc.Authorize(ctx, tt.token, tt.scope); !errors.Is(err, tt.want) {
			t.Errorf("Authorize(%q, %v) = %v, want %v", tt.token, tt.scope, err, tt.want)
		}
	}
}

type errorAuthenticator struct{}

func (errorAuthenticator) Authenticate(context.Context, string) (store.Token, error) {
	return store.Token{}, errors.New("db down")
}

func TestAuthorizePassesThroughLookupErrors(t *testing.T) {
	svc := New(nil, auth.NewCacheTokenAuth(errorAuthenticator{}), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	err := svc.Authorize(context.Background(), "any", auth.ScopeRead)
	if err == nil || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) {
		t.Fatalf("Authorize err = %v, want the underlying lookup error", err)
	}
}

func TestPutThenGetRecordsRead(t *testing.T) {
	reads := &fakeReadTracker{notify: make(chan string, 1)}
	svc := newTestService(t, reads, io.Discard)
	ctx := context.Background()

	if err := svc.Put(ctx, "abc", strings.NewReader("data"), 4); err != nil {
		t.Fatalf("Put: %v", err)
	}
	body, size, err := svc.Get(ctx, "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if string(got) != "data" || size != 4 {
		t.Fatalf("Get = %q (size %d), want \"data\" (size 4)", got, size)
	}
	if hash := waitForRead(t, reads); hash != "abc" {
		t.Fatalf("recorded read for %q, want abc", hash)
	}
}

func TestGetMissDoesNotRecordRead(t *testing.T) {
	reads := &fakeReadTracker{notify: make(chan string, 1)}
	svc := newTestService(t, reads, io.Discard)

	if _, _, err := svc.Get(context.Background(), "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get miss err = %v, want storage.ErrNotFound", err)
	}
	select {
	case hash := <-reads.notify:
		t.Fatalf("RecordCacheRead called on a miss for %q", hash)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestGetLogsReadTrackingFailure(t *testing.T) {
	reads := &fakeReadTracker{err: errors.New("db down"), notify: make(chan string, 1)}
	var logs syncBuffer
	svc := newTestService(t, reads, &logs)
	ctx := context.Background()

	if err := svc.Put(ctx, "abc", strings.NewReader("x"), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	body, _, err := svc.Get(ctx, "abc")
	if err != nil {
		t.Fatalf("Get must succeed even if read tracking fails: %v", err)
	}
	_ = body.Close()
	waitForRead(t, reads)

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "record cache read failed") {
		if time.Now().After(deadline) {
			t.Fatalf("expected a read-tracking warning, logs: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPutEnforcesMaxEntryBytes(t *testing.T) {
	svc := newTestService(t, &fakeReadTracker{notify: make(chan string, 1)}, io.Discard)
	ctx := context.Background()

	if err := svc.Put(ctx, "big", strings.NewReader("12345678901"), 11); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Put over limit err = %v, want ErrTooLarge", err)
	}
	svc.SetMaxEntryBytes(20)
	if err := svc.Put(ctx, "big", strings.NewReader("12345678901"), 11); err != nil {
		t.Fatalf("Put after raising limit: %v", err)
	}
	if err := svc.Put(ctx, "big", strings.NewReader("again"), 5); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate Put err = %v, want storage.ErrAlreadyExists", err)
	}
}
