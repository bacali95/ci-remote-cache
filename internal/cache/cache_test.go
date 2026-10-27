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

// fakeReadTracker returns err from every call and reports each read key on
// notify, since Get records reads in a goroutine. Recorded artifact
// metadata is kept in meta.
type fakeReadTracker struct {
	err    error
	notify chan string

	mu   sync.Mutex
	meta map[string]store.ArtifactMeta
}

func (f *fakeReadTracker) RecordCacheRead(_ context.Context, key string) error {
	f.notify <- key
	return f.err
}

func (f *fakeReadTracker) RecordCacheArtifact(_ context.Context, key string, _ int64, meta store.ArtifactMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.meta == nil {
		f.meta = map[string]store.ArtifactMeta{}
	}
	f.meta[key] = meta
	return f.err
}

func (f *fakeReadTracker) GetCacheArtifactInfoBatch(_ context.Context, keys []string) (map[string]store.ArtifactInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]store.ArtifactInfo{}
	for _, k := range keys {
		if meta, ok := f.meta[k]; ok {
			out[k] = store.ArtifactInfo{Meta: meta}
		}
	}
	return out, nil
}

func (f *fakeReadTracker) GetCacheArtifactMeta(_ context.Context, key string) (store.ArtifactMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.ArtifactMeta{}, f.err
	}
	meta, ok := f.meta[key]
	if !ok {
		return store.ArtifactMeta{}, store.ErrNotFound
	}
	return meta, nil
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

func newTestService(t *testing.T, reads Tracker, logOut io.Writer) *Service {
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

	if err := svc.Put(ctx, "ns", "abc", strings.NewReader("data"), 4, store.ArtifactMeta{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	body, size, err := svc.Get(ctx, "ns", "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if string(got) != "data" || size != 4 {
		t.Fatalf("Get = %q (size %d), want \"data\" (size 4)", got, size)
	}
	if key := waitForRead(t, reads); key != "ns/abc" {
		t.Fatalf("recorded read for %q, want ns/abc", key)
	}
}

func TestGetMissDoesNotRecordRead(t *testing.T) {
	reads := &fakeReadTracker{notify: make(chan string, 1)}
	svc := newTestService(t, reads, io.Discard)

	if _, _, err := svc.Get(context.Background(), "ns", "missing"); !errors.Is(err, storage.ErrNotFound) {
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

	if err := svc.Put(ctx, "ns", "abc", strings.NewReader("x"), 1, store.ArtifactMeta{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	body, _, err := svc.Get(ctx, "ns", "abc")
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

	if err := svc.Put(ctx, "ns", "big", strings.NewReader("12345678901"), 11, store.ArtifactMeta{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Put over limit err = %v, want ErrTooLarge", err)
	}
	svc.SetMaxEntryBytes(20)
	if err := svc.Put(ctx, "ns", "big", strings.NewReader("12345678901"), 11, store.ArtifactMeta{}); err != nil {
		t.Fatalf("Put after raising limit: %v", err)
	}
	if err := svc.Put(ctx, "ns", "big", strings.NewReader("again"), 5, store.ArtifactMeta{}); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate Put err = %v, want storage.ErrAlreadyExists", err)
	}
}

func TestGetFallsBackToLegacyRootKeyForNxOnly(t *testing.T) {
	backend, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	if err := backend.Put(ctx, "old", strings.NewReader("legacy"), 6); err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}
	reads := &fakeReadTracker{notify: make(chan string, 1)}
	svc := New(backend, nil, reads, slog.New(slog.NewTextHandler(io.Discard, nil)), 10)

	body, _, err := svc.Get(ctx, "nx", "old")
	if err != nil {
		t.Fatalf("Get legacy nx entry: %v", err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "legacy" {
		t.Fatalf("Get = %q, want legacy", got)
	}
	if key := waitForRead(t, reads); key != "old" {
		t.Fatalf("recorded read for %q, want the legacy key old", key)
	}

	if _, _, err := svc.Get(ctx, "turbo", "old"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get legacy key via turbo err = %v, want storage.ErrNotFound", err)
	}
}

func TestPutRecordsMetaReturnedByMeta(t *testing.T) {
	svc := newTestService(t, &fakeReadTracker{notify: make(chan string, 1)}, io.Discard)
	ctx := context.Background()
	want := store.ArtifactMeta{DurationMs: 7, Tag: "sig"}

	if err := svc.Put(ctx, "turbo", "abc", strings.NewReader("x"), 1, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := svc.Meta(ctx, "turbo", "abc")
	if err != nil || got != want {
		t.Fatalf("Meta = %+v, %v; want %+v, nil", got, err, want)
	}
	got, err = svc.Meta(ctx, "turbo", "unknown")
	if err != nil || got != (store.ArtifactMeta{}) {
		t.Fatalf("Meta(unknown) = %+v, %v; want zero value, nil", got, err)
	}
}

func TestPutSucceedsAndMetaErrorsWhenTrackerFails(t *testing.T) {
	var logs syncBuffer
	svc := newTestService(t, &fakeReadTracker{err: errors.New("db down"), notify: make(chan string, 1)}, &logs)
	ctx := context.Background()

	if err := svc.Put(ctx, "turbo", "abc", strings.NewReader("x"), 1, store.ArtifactMeta{}); err != nil {
		t.Fatalf("Put must succeed even if recording the artifact fails: %v", err)
	}
	if !strings.Contains(logs.String(), "record cache artifact failed") {
		t.Fatalf("expected an artifact-recording warning, logs: %s", logs.String())
	}
	if _, err := svc.Meta(ctx, "turbo", "abc"); err == nil {
		t.Fatal("Meta err = nil, want the tracker's error")
	}
}

func TestExistsFollowsLegacyFallback(t *testing.T) {
	backend, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	for _, k := range []string{"old", "turbo/new"} {
		if err := backend.Put(ctx, k, strings.NewReader("x"), 1); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	svc := New(backend, nil, &fakeReadTracker{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 10)

	tests := []struct {
		namespace, hash string
		want            bool
	}{
		{"nx", "old", true},
		{"turbo", "old", false},
		{"turbo", "new", true},
		{"nx", "missing", false},
	}
	for _, tt := range tests {
		if got, err := svc.Exists(ctx, tt.namespace, tt.hash); err != nil || got != tt.want {
			t.Errorf("Exists(%s, %s) = %v, %v; want %v, nil", tt.namespace, tt.hash, got, err, tt.want)
		}
	}
}

func TestQueryMapsKeysBackToHashes(t *testing.T) {
	svc := newTestService(t, &fakeReadTracker{notify: make(chan string, 1)}, io.Discard)
	ctx := context.Background()
	meta := store.ArtifactMeta{Tag: "t"}
	if err := svc.Put(ctx, "turbo", "abc", strings.NewReader("x"), 1, meta); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := svc.Query(ctx, "turbo", []string{"abc", "missing"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got["abc"].Meta != meta {
		t.Fatalf("Query = %+v, want only abc with its meta", got)
	}

	failing := newTestService(t, &fakeReadTracker{err: errors.New("db down"), notify: make(chan string, 1)}, io.Discard)
	if _, err := failing.Query(ctx, "turbo", []string{"abc"}); err == nil {
		t.Fatal("Query err = nil, want the tracker's error")
	}
}

func TestSplitKey(t *testing.T) {
	tests := []struct {
		key, namespace, hash string
		legacy               bool
	}{
		{"nx/abc", "nx", "abc", false},
		{"turbo/def", "turbo", "def", false},
		{"old", "nx", "old", true},
	}
	for _, tt := range tests {
		ns, h, legacy := SplitKey(tt.key)
		if ns != tt.namespace || h != tt.hash || legacy != tt.legacy {
			t.Errorf("SplitKey(%q) = %q, %q, %v; want %q, %q, %v", tt.key, ns, h, legacy, tt.namespace, tt.hash, tt.legacy)
		}
	}
}
