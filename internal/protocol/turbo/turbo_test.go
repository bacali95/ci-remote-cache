package turbo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

type fakeAuthenticator struct{ err error }

func (f fakeAuthenticator) Authenticate(_ context.Context, tokenHash string) (store.Token, error) {
	switch {
	case f.err != nil:
		return store.Token{}, f.err
	case tokenHash == auth.HashToken(readToken):
		return store.Token{Scope: store.ScopeRead}, nil
	case tokenHash == auth.HashToken(writeToken):
		return store.Token{Scope: store.ScopeWrite}, nil
	}
	return store.Token{}, store.ErrNotFound
}

// fakeTracker keeps artifact rows in memory. metaErr makes metadata and
// batch lookups fail.
type fakeTracker struct {
	mu      sync.Mutex
	rows    map[string]store.ArtifactInfo
	metaErr error
}

func (f *fakeTracker) RecordCacheRead(context.Context, string) error { return nil }

func (f *fakeTracker) RecordCacheArtifact(_ context.Context, key string, size int64, meta store.ArtifactMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[key] = store.ArtifactInfo{Size: size, Meta: meta}
	return nil
}

func (f *fakeTracker) GetCacheArtifactMeta(_ context.Context, key string) (store.ArtifactMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.metaErr != nil {
		return store.ArtifactMeta{}, f.metaErr
	}
	row, ok := f.rows[key]
	if !ok {
		return store.ArtifactMeta{}, store.ErrNotFound
	}
	return row.Meta, nil
}

func (f *fakeTracker) GetCacheArtifactInfoBatch(_ context.Context, keys []string) (map[string]store.ArtifactInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.metaErr != nil {
		return nil, f.metaErr
	}
	out := map[string]store.ArtifactInfo{}
	for _, k := range keys {
		if row, ok := f.rows[k]; ok {
			out[k] = row
		}
	}
	return out, nil
}

// erroringBackend wraps a real *storage.Local but fails any operation
// whose error is set.
type erroringBackend struct {
	*storage.Local
	existsErr, getErr, putErr error
}

func (b *erroringBackend) Exists(ctx context.Context, key string) (bool, error) {
	if b.existsErr != nil {
		return false, b.existsErr
	}
	return b.Local.Exists(ctx, key)
}

func (b *erroringBackend) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if b.getErr != nil {
		return nil, 0, b.getErr
	}
	return b.Local.Get(ctx, key)
}

func (b *erroringBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if b.putErr != nil {
		return b.putErr
	}
	return b.Local.Put(ctx, key, r, size)
}

type fixture struct {
	h       http.Handler
	backend *erroringBackend
	tracker *fakeTracker
}

func newFixture(t *testing.T, authn auth.TokenAuthenticator) *fixture {
	t.Helper()
	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	f := &fixture{
		backend: &erroringBackend{Local: local},
		tracker: &fakeTracker{rows: map[string]store.ArtifactInfo{}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := cache.New(f.backend, auth.NewCacheTokenAuth(authn), f.tracker, log, 1024)
	f.h = protocol.Handler(log, New(svc, log))
	return f
}

func (f *fixture) do(t *testing.T, method, path, token string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, status, w.Body)
	}
}

func TestUploadDownloadRoundtripsMetadata(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	meta := map[string]string{
		"x-artifact-duration":   "1500",
		"x-artifact-tag":        "c2lnbmF0dXJl",
		"x-artifact-sha":        "0123abcd",
		"x-artifact-dirty-hash": "d1rty",
	}

	w := f.do(t, http.MethodPut, "/v8/artifacts/abc123?teamId=team_x", writeToken, []byte("tarball"), meta)
	wantStatus(t, w, http.StatusAccepted)
	var up uploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil || len(up.URLs) != 1 {
		t.Fatalf("upload response = %s, want {urls:[...]}", w.Body)
	}

	w = f.do(t, http.MethodGet, "/v8/artifacts/abc123?slug=team", readToken, nil, nil)
	wantStatus(t, w, http.StatusOK)
	if w.Body.String() != "tarball" || w.Header().Get("Content-Length") != "7" {
		t.Fatalf("GET body = %q, Content-Length = %q", w.Body, w.Header().Get("Content-Length"))
	}
	for k, v := range meta {
		if got := w.Header().Get(k); got != v {
			t.Errorf("GET header %s = %q, want %q", k, got, v)
		}
	}

	w = f.do(t, http.MethodHead, "/v8/artifacts/abc123", readToken, nil, nil)
	wantStatus(t, w, http.StatusOK)
	if got := w.Header().Get("x-artifact-tag"); got != meta["x-artifact-tag"] {
		t.Errorf("HEAD x-artifact-tag = %q, want %q", got, meta["x-artifact-tag"])
	}

	if ok, err := f.backend.Local.Exists(context.Background(), "turbo/abc123"); err != nil || !ok {
		t.Fatalf("artifact not stored under the turbo namespace: %v, %v", ok, err)
	}
}

func TestReuploadIsAccepted(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	wantStatus(t, f.do(t, http.MethodPut, "/v8/artifacts/dup", writeToken, []byte("one"), nil), http.StatusAccepted)
	wantStatus(t, f.do(t, http.MethodPut, "/v8/artifacts/dup", writeToken, []byte("two"), nil), http.StatusAccepted)
}

func TestMissingArtifact(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	wantStatus(t, f.do(t, http.MethodGet, "/v8/artifacts/missing", readToken, nil, nil), http.StatusNotFound)
	wantStatus(t, f.do(t, http.MethodHead, "/v8/artifacts/missing", readToken, nil, nil), http.StatusNotFound)
}

func TestLegacyNxEntriesAreInvisible(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	if err := f.backend.Local.Put(context.Background(), "legacy", strings.NewReader("x"), 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	wantStatus(t, f.do(t, http.MethodGet, "/v8/artifacts/legacy", readToken, nil, nil), http.StatusNotFound)
}

func TestStatus(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	w := f.do(t, http.MethodGet, "/v8/artifacts/status", readToken, nil, nil)
	wantStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"status":"enabled"`) {
		t.Fatalf("status body = %s", w.Body)
	}
	wantStatus(t, f.do(t, http.MethodGet, "/v8/artifacts/status", "", nil, nil), http.StatusUnauthorized)
}

func TestAuthErrors(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	tests := []struct {
		name, method, path, token string
		status                    int
	}{
		{"no token", http.MethodGet, "/v8/artifacts/abc", "", http.StatusUnauthorized},
		{"unknown token", http.MethodHead, "/v8/artifacts/abc", "nope", http.StatusUnauthorized},
		{"read token upload", http.MethodPut, "/v8/artifacts/abc", readToken, http.StatusForbidden},
		{"query no token", http.MethodPost, "/v8/artifacts", "", http.StatusUnauthorized},
		{"events no token", http.MethodPost, "/v8/artifacts/events", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := f.do(t, tt.method, tt.path, tt.token, []byte("x"), nil)
			wantStatus(t, w, tt.status)
			if tt.method != http.MethodHead && !strings.Contains(w.Body.String(), `"code"`) {
				t.Errorf("error body = %s, want a JSON {code, message}", w.Body)
			}
		})
	}
}

func TestUploadValidation(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	long := strings.Repeat("a", maxMetaHeaderLen+1)
	tests := []struct {
		name    string
		path    string
		body    []byte
		headers map[string]string
		status  int
	}{
		{"invalid hash", "/v8/artifacts/abc%24def", []byte("x"), nil, http.StatusBadRequest},
		{"empty body", "/v8/artifacts/abc", nil, nil, http.StatusLengthRequired},
		{"bad duration", "/v8/artifacts/abc", []byte("x"), map[string]string{"x-artifact-duration": "soon"}, http.StatusBadRequest},
		{"negative duration", "/v8/artifacts/abc", []byte("x"), map[string]string{"x-artifact-duration": "-1"}, http.StatusBadRequest},
		{"tag too long", "/v8/artifacts/abc", []byte("x"), map[string]string{"x-artifact-tag": long}, http.StatusBadRequest},
		{"too large", "/v8/artifacts/abc", make([]byte, 2048), nil, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantStatus(t, f.do(t, http.MethodPut, tt.path, writeToken, tt.body, tt.headers), tt.status)
		})
	}
}

func TestInternalErrors(t *testing.T) {
	boom := errors.New("boom")

	f := newFixture(t, fakeAuthenticator{err: boom})
	wantStatus(t, f.do(t, http.MethodGet, "/v8/artifacts/abc", readToken, nil, nil), http.StatusInternalServerError)

	f = newFixture(t, fakeAuthenticator{})
	f.backend.existsErr, f.backend.getErr, f.backend.putErr = boom, boom, boom
	wantStatus(t, f.do(t, http.MethodHead, "/v8/artifacts/abc", readToken, nil, nil), http.StatusInternalServerError)
	wantStatus(t, f.do(t, http.MethodGet, "/v8/artifacts/abc", readToken, nil, nil), http.StatusInternalServerError)
	wantStatus(t, f.do(t, http.MethodPut, "/v8/artifacts/abc", writeToken, []byte("x"), nil), http.StatusInternalServerError)
}

func TestMetadataLookupFailureStillServesArtifact(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	wantStatus(t, f.do(t, http.MethodPut, "/v8/artifacts/abc", writeToken, []byte("x"),
		map[string]string{"x-artifact-tag": "sig"}), http.StatusAccepted)
	f.tracker.metaErr = errors.New("db down")

	w := f.do(t, http.MethodGet, "/v8/artifacts/abc", readToken, nil, nil)
	wantStatus(t, w, http.StatusOK)
	if w.Header().Get("x-artifact-tag") != "" {
		t.Fatalf("x-artifact-tag = %q, want none when metadata can't be loaded", w.Header().Get("x-artifact-tag"))
	}
}

// failWriteResponseWriter fails every body write, simulating a client
// disconnecting mid-download.
type failWriteResponseWriter struct{ header http.Header }

func (f *failWriteResponseWriter) Header() http.Header { return f.header }
func (f *failWriteResponseWriter) WriteHeader(int)     {}
func (f *failWriteResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestDownloadSurvivesClientDisconnect(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	wantStatus(t, f.do(t, http.MethodPut, "/v8/artifacts/abc", writeToken, []byte("x"), nil), http.StatusAccepted)

	r := httptest.NewRequest(http.MethodGet, "/v8/artifacts/abc", nil)
	r.Header.Set("Authorization", "Bearer "+readToken)
	f.h.ServeHTTP(&failWriteResponseWriter{header: http.Header{}}, r)
}

func TestQuery(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	wantStatus(t, f.do(t, http.MethodPut, "/v8/artifacts/abc", writeToken, []byte("xyz"),
		map[string]string{"x-artifact-duration": "42", "x-artifact-tag": "sig"}), http.StatusAccepted)

	body, _ := json.Marshal(queryRequest{Hashes: []string{"abc", "missing", "bad$hash"}})
	w := f.do(t, http.MethodPost, "/v8/artifacts", readToken, body, nil)
	wantStatus(t, w, http.StatusOK)

	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body = %s", err, w.Body)
	}
	if string(got["abc"]) != `{"size":3,"taskDurationMs":42,"tag":"sig"}` {
		t.Errorf("abc = %s", got["abc"])
	}
	if string(got["missing"]) != "null" {
		t.Errorf("missing = %s, want null", got["missing"])
	}
	if !strings.Contains(string(got["bad$hash"]), `"error"`) {
		t.Errorf("bad$hash = %s, want an error object", got["bad$hash"])
	}
}

func TestQueryErrors(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	wantStatus(t, f.do(t, http.MethodPost, "/v8/artifacts", readToken, []byte("not json"), nil), http.StatusBadRequest)

	tooMany, _ := json.Marshal(queryRequest{Hashes: make([]string, maxQueryHashes+1)})
	wantStatus(t, f.do(t, http.MethodPost, "/v8/artifacts", readToken, tooMany, nil), http.StatusBadRequest)

	f.tracker.metaErr = errors.New("db down")
	ok, _ := json.Marshal(queryRequest{Hashes: []string{"abc"}})
	wantStatus(t, f.do(t, http.MethodPost, "/v8/artifacts", readToken, ok, nil), http.StatusInternalServerError)
}

func TestEvents(t *testing.T) {
	f := newFixture(t, fakeAuthenticator{})
	valid := `[{"sessionId":"6c5b8f0e-0000-0000-0000-000000000000","source":"REMOTE","event":"HIT","hash":"abc","duration":10},
	           {"sessionId":"6c5b8f0e-0000-0000-0000-000000000000","source":"LOCAL","event":"MISS","hash":"def"}]`
	wantStatus(t, f.do(t, http.MethodPost, "/v8/artifacts/events", readToken, []byte(valid), nil), http.StatusOK)

	for _, body := range []string{
		"not json",
		`[{"sessionId":"s","source":"CLOUD","event":"HIT","hash":"abc"}]`,
		`[{"sessionId":"s","source":"LOCAL","event":"MAYBE","hash":"abc"}]`,
		`[{"source":"LOCAL","event":"HIT","hash":"abc"}]`,
	} {
		wantStatus(t, f.do(t, http.MethodPost, "/v8/artifacts/events", readToken, []byte(body), nil), http.StatusBadRequest)
	}
}
