package nx

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"nx-remote-cache/internal/auth"
	"nx-remote-cache/internal/cache"
	"nx-remote-cache/internal/protocol"
	"nx-remote-cache/internal/storage"
	"nx-remote-cache/internal/store"
)

const (
	readToken  = "read-token"
	writeToken = "write-token"
)

// fakeAuthenticator satisfies auth.TokenAuthenticator without needing
// Postgres: tests only care about the read/write scope decision, which
// internal/auth (tested separately) is responsible for deriving from a
// store.Token.
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

// noopArtifacts supplies the cache.Tracker methods the Nx adapter never
// depends on: Nx uploads carry no metadata.
type noopArtifacts struct{}

func (noopArtifacts) RecordCacheArtifact(context.Context, string, int64, store.ArtifactMeta) error {
	return nil
}

func (noopArtifacts) GetCacheArtifactInfoBatch(context.Context, []string) (map[string]store.ArtifactInfo, error) {
	return nil, nil
}

func (noopArtifacts) GetCacheArtifactMeta(context.Context, string) (store.ArtifactMeta, error) {
	return store.ArtifactMeta{}, store.ErrNotFound
}

// fakeReadTracker satisfies cache.Tracker without Postgres. notify
// lets a test block until an async RecordCacheRead call actually lands,
// since cache.Service.Get fires it in a goroutine.
type fakeReadTracker struct {
	noopArtifacts
	mu     sync.Mutex
	calls  []string
	notify chan string
}

func newFakeReadTracker() *fakeReadTracker {
	return &fakeReadTracker{notify: make(chan string, 16)}
}

func (f *fakeReadTracker) RecordCacheRead(_ context.Context, hash string) error {
	f.mu.Lock()
	f.calls = append(f.calls, hash)
	f.mu.Unlock()
	f.notify <- hash
	return nil
}

// testServer pairs the cache core with the Nx routes mounted the way
// cmd/server mounts them, so tests can both send requests and change the
// size limit.
type testServer struct {
	*cache.Service
	handler http.Handler
}

func (s *testServer) Handler() http.Handler { return s.handler }

func newServer(backend storage.Backend, tokens *auth.CacheTokenAuth, reads cache.Tracker) *testServer {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := cache.New(backend, tokens, reads, log, 10*1024*1024)
	return &testServer{Service: svc, handler: protocol.Handler(log, New(svc, log))}
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	srv, _ := newTestServerWithReads(t)
	return srv
}

func newTestServerWithReads(t *testing.T) (*testServer, *fakeReadTracker) {
	t.Helper()
	backend, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("new local backend: %v", err)
	}
	fake := &fakeAuthenticator{byHash: map[string]store.Token{
		auth.HashToken(readToken):  {Scope: store.ScopeRead},
		auth.HashToken(writeToken): {Scope: store.ScopeWrite},
	}}
	tokens := auth.NewCacheTokenAuth(fake)
	reads := newFakeReadTracker()
	return newServer(backend, tokens, reads), reads
}

func doRequest(t *testing.T, h http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPutThenGetRoundtrip(t *testing.T) {
	h := newTestServer(t).Handler()
	payload := []byte("cached task output")

	putResp := doRequest(t, h, http.MethodPut, "/v1/cache/abc123", writeToken, payload)
	if putResp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", putResp.Code, putResp.Body)
	}

	getResp := doRequest(t, h, http.MethodGet, "/v1/cache/abc123", readToken, nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", getResp.Code, getResp.Body)
	}
	if getResp.Body.String() != string(payload) {
		t.Fatalf("GET body = %q, want %q", getResp.Body.String(), payload)
	}
}

func TestGetHitRecordsReadButMissDoesNot(t *testing.T) {
	srv, reads := newTestServerWithReads(t)
	h := srv.Handler()

	putResp := doRequest(t, h, http.MethodPut, "/v1/cache/abc123", writeToken, []byte("payload"))
	if putResp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", putResp.Code)
	}

	getResp := doRequest(t, h, http.MethodGet, "/v1/cache/abc123", readToken, nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.Code)
	}
	select {
	case hash := <-reads.notify:
		if hash != "nx/abc123" {
			t.Fatalf("recorded read for %q, want nx/abc123", hash)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RecordCacheRead was not called within 2s of a cache hit")
	}

	missResp := doRequest(t, h, http.MethodGet, "/v1/cache/doesnotexist", readToken, nil)
	if missResp.Code != http.StatusNotFound {
		t.Fatalf("GET miss status = %d, want 404", missResp.Code)
	}
	select {
	case hash := <-reads.notify:
		t.Fatalf("RecordCacheRead should not be called on a miss, got call for %q", hash)
	case <-time.After(200 * time.Millisecond):
		// expected: no call
	}
}

func TestGetMissingReturns404(t *testing.T) {
	h := newTestServer(t).Handler()
	resp := doRequest(t, h, http.MethodGet, "/v1/cache/doesnotexist", readToken, nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}

func TestPutDuplicateReturns409(t *testing.T) {
	h := newTestServer(t).Handler()
	payload := []byte("first write")

	first := doRequest(t, h, http.MethodPut, "/v1/cache/dup", writeToken, payload)
	if first.Code != http.StatusOK {
		t.Fatalf("first PUT status = %d, want 200", first.Code)
	}

	second := doRequest(t, h, http.MethodPut, "/v1/cache/dup", writeToken, []byte("second write"))
	if second.Code != http.StatusConflict {
		t.Fatalf("second PUT status = %d, want 409", second.Code)
	}
}

func TestNoTokenReturns401(t *testing.T) {
	h := newTestServer(t).Handler()

	get := doRequest(t, h, http.MethodGet, "/v1/cache/abc123", "", nil)
	if get.Code != http.StatusUnauthorized {
		t.Fatalf("GET without token status = %d, want 401", get.Code)
	}

	put := doRequest(t, h, http.MethodPut, "/v1/cache/abc123", "", []byte("x"))
	if put.Code != http.StatusUnauthorized {
		t.Fatalf("PUT without token status = %d, want 401", put.Code)
	}
}

func TestUnknownTokenReturns401(t *testing.T) {
	h := newTestServer(t).Handler()
	resp := doRequest(t, h, http.MethodGet, "/v1/cache/abc123", "not-a-real-token", nil)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.Code)
	}
}

func TestReadOnlyTokenCannotWrite(t *testing.T) {
	h := newTestServer(t).Handler()
	resp := doRequest(t, h, http.MethodPut, "/v1/cache/abc123", readToken, []byte("nope"))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.Code)
	}
}

func TestWriteTokenCanAlsoRead(t *testing.T) {
	h := newTestServer(t).Handler()
	payload := []byte("written by write token")
	if resp := doRequest(t, h, http.MethodPut, "/v1/cache/xyz", writeToken, payload); resp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.Code)
	}
	resp := doRequest(t, h, http.MethodGet, "/v1/cache/xyz", writeToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.Code)
	}
}

func TestPutWithoutContentLengthReturns411(t *testing.T) {
	h := newTestServer(t).Handler()
	r := httptest.NewRequest(http.MethodPut, "/v1/cache/nolen", bytes.NewReader([]byte("x")))
	r.ContentLength = 0
	r.Header.Set("Authorization", "Bearer "+writeToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusLengthRequired {
		t.Fatalf("status = %d, want 411", w.Code)
	}
}

func TestSetMaxEntryBytesTakesEffectImmediately(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	payload := []byte("0123456789")
	resp := doRequest(t, h, http.MethodPut, "/v1/cache/beforelimit", writeToken, payload)
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT under limit: status = %d, want 200", resp.Code)
	}

	srv.SetMaxEntryBytes(5)
	resp = doRequest(t, h, http.MethodPut, "/v1/cache/afterlimit", writeToken, payload)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT over new limit: status = %d, want 413", resp.Code)
	}

	srv.SetMaxEntryBytes(10 * 1024 * 1024)
	resp = doRequest(t, h, http.MethodPut, "/v1/cache/afterraise", writeToken, payload)
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT after raising limit back: status = %d, want 200", resp.Code)
	}
}

func TestInvalidHashRejected(t *testing.T) {
	h := newTestServer(t).Handler()
	resp := doRequest(t, h, http.MethodGet, "/v1/cache/abc%2e%2e%24def", readToken, nil)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for hash with disallowed characters", resp.Code)
	}
}
