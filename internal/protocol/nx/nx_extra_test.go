package nx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nx-remote-cache/internal/auth"
	"nx-remote-cache/internal/storage"
	"nx-remote-cache/internal/store"
)

// errorAuthenticator always returns a non-ErrNotFound error, to exercise
// handleGet/handlePut's "token authorize failed" 500 branch.
type errorAuthenticator struct{ err error }

func (e *errorAuthenticator) Authenticate(context.Context, string) (store.Token, error) {
	return store.Token{}, e.err
}

// erroringBackend wraps a real *storage.Local but can be told to fail Get
// or Put on demand, to exercise handleGet/handlePut's internal-error
// branches (a real backend basically never fails Get/Put for a valid,
// present/absent hash otherwise, so those branches need a fake).
type erroringBackend struct {
	*storage.Local
	getErr error
	putErr error
}

func (b *erroringBackend) Get(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	if b.getErr != nil {
		return nil, 0, b.getErr
	}
	return b.Local.Get(ctx, hash)
}

func (b *erroringBackend) Put(ctx context.Context, hash string, r io.Reader, size int64) error {
	if b.putErr != nil {
		return b.putErr
	}
	return b.Local.Put(ctx, hash, r, size)
}

func newErroringBackend(t *testing.T) *erroringBackend {
	t.Helper()
	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return &erroringBackend{Local: local}
}

func TestHandleGetReturns500WhenAuthorizeFails(t *testing.T) {
	backend := newErroringBackend(t)
	tokens := auth.NewCacheTokenAuth(&errorAuthenticator{err: errors.New("boom")})
	h := newServer(backend, tokens, newFakeReadTracker()).Handler()

	resp := doRequest(t, h, "GET", "/v1/cache/abc123", "any-token", nil)
	if resp.Code != 500 {
		t.Fatalf("status = %d, want 500", resp.Code)
	}
}

func TestHandlePutReturns500WhenAuthorizeFails(t *testing.T) {
	backend := newErroringBackend(t)
	tokens := auth.NewCacheTokenAuth(&errorAuthenticator{err: errors.New("boom")})
	h := newServer(backend, tokens, newFakeReadTracker()).Handler()

	resp := doRequest(t, h, "PUT", "/v1/cache/abc123", "any-token", []byte("x"))
	if resp.Code != 500 {
		t.Fatalf("status = %d, want 500", resp.Code)
	}
}

func TestHandleGetReturns500WhenBackendGetFails(t *testing.T) {
	backend := newErroringBackend(t)
	backend.getErr = errors.New("disk on fire")
	fake := &fakeAuthenticator{byHash: map[string]store.Token{
		auth.HashToken(readToken): {Scope: store.ScopeRead},
	}}
	h := newServer(backend, auth.NewCacheTokenAuth(fake), newFakeReadTracker()).Handler()

	resp := doRequest(t, h, "GET", "/v1/cache/abc123", readToken, nil)
	if resp.Code != 500 {
		t.Fatalf("status = %d, want 500", resp.Code)
	}
}

func TestHandlePutReturns500WhenBackendPutFails(t *testing.T) {
	backend := newErroringBackend(t)
	backend.putErr = errors.New("disk on fire")
	fake := &fakeAuthenticator{byHash: map[string]store.Token{
		auth.HashToken(writeToken): {Scope: store.ScopeWrite},
	}}
	h := newServer(backend, auth.NewCacheTokenAuth(fake), newFakeReadTracker()).Handler()

	resp := doRequest(t, h, "PUT", "/v1/cache/abc123", writeToken, []byte("x"))
	if resp.Code != 500 {
		t.Fatalf("status = %d, want 500", resp.Code)
	}
}

// failingReadTracker always errors: the download itself must still succeed
// even though tracking the read errors out.
type failingReadTracker struct {
	noopArtifacts
	done chan struct{}
}

func (f *failingReadTracker) RecordCacheRead(context.Context, string) error {
	f.done <- struct{}{}
	return errors.New("tracking store is down")
}

func TestHandlePutInvalidHashRejected(t *testing.T) {
	h := newTestServer(t).Handler()
	resp := doRequest(t, h, "PUT", "/v1/cache/abc%2e%2e%24def", writeToken, []byte("x"))
	if resp.Code != 400 {
		t.Fatalf("status = %d, want 400 for hash with disallowed characters", resp.Code)
	}
}

// failWriteResponseWriter fails every Write after WriteHeader, to simulate
// a client disconnecting mid-download and exercise handleGet's io.Copy
// error-logging branch (a real *httptest.ResponseRecorder never fails a
// write, so that branch needs a fake ResponseWriter).
type failWriteResponseWriter struct {
	header http.Header
	code   int
}

func (f *failWriteResponseWriter) Header() http.Header  { return f.header }
func (f *failWriteResponseWriter) WriteHeader(code int) { f.code = code }
func (f *failWriteResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestHandleGetLogsButDoesNotPanicWhenStreamingFails(t *testing.T) {
	backend := newErroringBackend(t)
	fake := &fakeAuthenticator{byHash: map[string]store.Token{
		auth.HashToken(readToken):  {Scope: store.ScopeRead},
		auth.HashToken(writeToken): {Scope: store.ScopeWrite},
	}}
	h := newServer(backend, auth.NewCacheTokenAuth(fake), newFakeReadTracker()).Handler()

	put := doRequest(t, h, "PUT", "/v1/cache/streamfail", writeToken, []byte("payload"))
	if put.Code != 200 {
		t.Fatalf("seed PUT status = %d, want 200", put.Code)
	}

	r := httptest.NewRequest("GET", "/v1/cache/streamfail", nil)
	r.Header.Set("Authorization", "Bearer "+readToken)
	w := &failWriteResponseWriter{header: http.Header{}}
	h.ServeHTTP(w, r) // must not panic despite the write failing mid-stream
}

func TestHandleGetStillSucceedsWhenReadTrackingFails(t *testing.T) {
	backend := newErroringBackend(t)
	fake := &fakeAuthenticator{byHash: map[string]store.Token{
		auth.HashToken(readToken):  {Scope: store.ScopeRead},
		auth.HashToken(writeToken): {Scope: store.ScopeWrite},
	}}
	failingReads := &failingReadTracker{done: make(chan struct{}, 1)}
	h := newServer(backend, auth.NewCacheTokenAuth(fake), failingReads).Handler()

	put := doRequest(t, h, "PUT", "/v1/cache/warntest", writeToken, []byte("payload"))
	if put.Code != 200 {
		t.Fatalf("seed PUT status = %d, want 200", put.Code)
	}

	get := doRequest(t, h, "GET", "/v1/cache/warntest", readToken, nil)
	if get.Code != 200 {
		t.Fatalf("GET status = %d, want 200 even though read tracking fails", get.Code)
	}

	select {
	case <-failingReads.done:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordCacheRead was never called")
	}
}
