package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gcssdk "cloud.google.com/go/storage"
)

// fakeGCS is a minimal in-memory stand-in for the GCS JSON API, covering
// just the requests cloud.google.com/go/storage issues for the operations
// storage.GCS uses: a single-shot "multipart" object insert (our payloads
// are always small enough that the client never switches to a chunked
// resumable upload), metadata/media GET, DELETE, and Objects.list. Driven
// via STORAGE_EMULATOR_HOST, which the real SDK has built-in support for
// (see cloud.google.com/go/storage's NewClient) — no real GCP project or
// network access needed.
type fakeGCS struct {
	mu      sync.Mutex
	objects map[string][]byte
	updated map[string]time.Time
}

func newFakeGCS(t *testing.T) (*httptest.Server, *fakeGCS) {
	t.Helper()
	f := &fakeGCS{objects: map[string][]byte{}, updated: map[string]time.Time{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv, f
}

func gcsErrorJSON(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"errors":  []map[string]string{{"reason": reason, "message": message}},
		},
	})
}

func (f *fakeGCS) objectJSON(bucket, name string) map[string]any {
	return map[string]any{
		"kind":    "storage#object",
		"bucket":  bucket,
		"name":    name,
		"size":    strconv.Itoa(len(f.objects[name])),
		"updated": f.updated[name].UTC().Format(time.RFC3339Nano),
	}
}

func (f *fakeGCS) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/upload/storage/v1/b/") && strings.HasSuffix(r.URL.Path, "/o"):
		f.insert(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/") && strings.HasSuffix(r.URL.Path, "/o"):
		f.list(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/") && strings.Contains(r.URL.Path, "/o/"):
		f.getMetadata(w, r)
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/o/"):
		f.delete(w, r)
	case r.Method == http.MethodGet:
		// ObjectHandle.NewReader (used by Get) downloads via a plain
		// /{bucket}/{object} path, not the JSON API — see the GCS "XML
		// API"-style direct object access, which the SDK's read path uses
		// even when everything else goes through storage/v1.
		f.media(w, r)
	default:
		http.NotFound(w, r)
	}
}

// insert handles the "multipart" one-shot upload the SDK uses for small
// writes: POST .../o?uploadType=multipart&name=...&ifGenerationMatch=0,
// body is a multipart/related request with a JSON metadata part followed
// by the raw object bytes.
func (f *fakeGCS) insert(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	bucket := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upload/storage/v1/b/"), "/o")

	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, "bad content type", http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	// Part 1: JSON metadata (unused beyond what the URL query already gave
	// us — real GCS merges the two, but our fake doesn't need to).
	if _, err := mr.NextPart(); err != nil {
		http.Error(w, "missing metadata part", http.StatusBadRequest)
		return
	}
	dataPart, err := mr.NextPart()
	if err != nil {
		http.Error(w, "missing data part", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(dataPart)
	if err != nil {
		http.Error(w, "read data part", http.StatusInternalServerError)
		return
	}

	f.mu.Lock()
	_, exists := f.objects[name]
	if r.URL.Query().Get("ifGenerationMatch") == "0" && exists {
		f.mu.Unlock()
		gcsErrorJSON(w, http.StatusPreconditionFailed, "conditionNotMet", "precondition failed")
		return
	}
	f.objects[name] = body
	f.updated[name] = time.Now()
	resp := f.objectJSON(bucket, name)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func objectNameFromPath(path string) string {
	i := strings.Index(path, "/o/")
	if i < 0 {
		return ""
	}
	name := path[i+len("/o/"):]
	if unescaped, err := url.QueryUnescape(name); err == nil {
		return unescaped
	}
	return name
}

// getMetadata serves GET /storage/v1/b/{bucket}/o/{object} — object
// metadata as JSON, used by Attrs() (and therefore Exists()).
func (f *fakeGCS) getMetadata(w http.ResponseWriter, r *http.Request) {
	name := objectNameFromPath(r.URL.Path)
	bucket := strings.TrimSuffix(r.URL.Path[len("/storage/v1/b/"):], "/o/"+name)

	f.mu.Lock()
	_, ok := f.objects[name]
	var resp map[string]any
	if ok {
		resp = f.objectJSON(bucket, name)
	}
	f.mu.Unlock()

	if !ok {
		gcsErrorJSON(w, http.StatusNotFound, "notFound", "not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// media serves GET /{bucket}/{object} — raw object bytes, used by
// ObjectHandle.NewReader() (and therefore Get()).
func (f *fakeGCS) media(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	name := parts[1]

	f.mu.Lock()
	body, ok := f.objects[name]
	f.mu.Unlock()

	if !ok {
		gcsErrorJSON(w, http.StatusNotFound, "notFound", "not found")
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (f *fakeGCS) delete(w http.ResponseWriter, r *http.Request) {
	name := objectNameFromPath(r.URL.Path)
	f.mu.Lock()
	_, ok := f.objects[name]
	delete(f.objects, name)
	delete(f.updated, name)
	f.mu.Unlock()
	if !ok {
		gcsErrorJSON(w, http.StatusNotFound, "notFound", "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeGCS) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	pageToken := r.URL.Query().Get("pageToken")
	maxResults := 1000
	if v := r.URL.Query().Get("maxResults"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxResults = n
		}
	}

	f.mu.Lock()
	var names []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			names = append(names, k)
		}
	}
	sort.Strings(names)

	start := 0
	if pageToken != "" {
		start = sort.SearchStrings(names, pageToken) + 1
	}
	end := start + maxResults
	if end > len(names) {
		end = len(names)
	}
	if start > len(names) {
		start = len(names)
	}

	items := make([]map[string]any, 0, end-start)
	for _, n := range names[start:end] {
		items = append(items, f.objectJSON("test-bucket", n))
	}
	resp := map[string]any{"kind": "storage#objects", "items": items}
	if end < len(names) {
		resp["nextPageToken"] = names[end-1]
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func newTestGCS(t *testing.T, prefix string) *GCS {
	t.Helper()
	srv, _ := newFakeGCS(t)
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)

	g, err := NewGCS(context.Background(), GCSOptions{Bucket: "test-bucket", Prefix: prefix})
	if err != nil {
		t.Fatalf("NewGCS: %v", err)
	}
	return g
}

// newBrokenTestGCS returns a *GCS whose every request gets a 500, to
// exercise Exists/Put/Get/Delete/List's generic (non-404/precondition)
// error branches.
func newBrokenTestGCS(t *testing.T) *GCS {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gcsErrorJSON(w, http.StatusInternalServerError, "backendError", "fake failure")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)

	// Built by hand rather than through NewGCS so the retryer can be capped
	// at one attempt — a deterministic 500 would otherwise burn the SDK's
	// default multi-attempt exponential backoff on every call.
	client, err := gcssdk.NewClient(context.Background())
	if err != nil {
		t.Fatalf("gcssdk.NewClient: %v", err)
	}
	bucket := client.Bucket("test-bucket").Retryer(gcssdk.WithMaxAttempts(1))
	return &GCS{bucket: bucket}
}

// brokenTestCtx bounds how long a call against newBrokenTestGCS can spend
// retrying a deterministic 500 — the SDK's default backoff for some
// operations (notably the resumable/multipart Insert path Put uses) isn't
// fully silenced by Retryer(WithMaxAttempts(1)) alone.
func brokenTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestGCSExistsSurfacesUnexpectedError(t *testing.T) {
	g := newBrokenTestGCS(t)
	if _, err := g.Exists(brokenTestCtx(t), "hash1"); err == nil {
		t.Fatal("expected an error from a 500 response")
	}
}

func TestGCSPutSurfacesUnexpectedError(t *testing.T) {
	g := newBrokenTestGCS(t)
	err := g.Put(brokenTestCtx(t), "hash1", bytes.NewReader([]byte("x")), 1)
	if err == nil || errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Put error = %v, want a generic (non-ErrAlreadyExists) error", err)
	}
}

func TestGCSGetSurfacesUnexpectedError(t *testing.T) {
	g := newBrokenTestGCS(t)
	_, _, err := g.Get(brokenTestCtx(t), "hash1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want a generic (non-ErrNotFound) error", err)
	}
}

func TestGCSDeleteSurfacesUnexpectedError(t *testing.T) {
	g := newBrokenTestGCS(t)
	err := g.Delete(brokenTestCtx(t), "hash1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete error = %v, want a generic (non-ErrNotFound) error", err)
	}
}

func TestGCSListSurfacesUnexpectedError(t *testing.T) {
	g := newBrokenTestGCS(t)
	if _, err := g.List(brokenTestCtx(t), "", 10); err == nil {
		t.Fatal("expected an error from a 500 response")
	}
}

func TestGCSExistsPutGetDelete(t *testing.T) {
	ctx := context.Background()
	g := newTestGCS(t, "")

	if exists, err := g.Exists(ctx, "hash1"); err != nil || exists {
		t.Fatalf("Exists before put = (%v, %v), want (false, nil)", exists, err)
	}

	payload := []byte("hello gcs")
	if err := g.Put(ctx, "hash1", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if exists, err := g.Exists(ctx, "hash1"); err != nil || !exists {
		t.Fatalf("Exists after put = (%v, %v), want (true, nil)", exists, err)
	}

	r, size, err := g.Get(ctx, "hash1")
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

	if err := g.Delete(ctx, "hash1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, _ := g.Exists(ctx, "hash1"); exists {
		t.Fatal("entry still exists after Delete")
	}
}

func TestGCSPutDuplicateReturnsAlreadyExists(t *testing.T) {
	ctx := context.Background()
	g := newTestGCS(t, "")
	if err := g.Put(ctx, "hash1", bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	err := g.Put(ctx, "hash1", bytes.NewReader([]byte("b")), 1)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Put error = %v, want ErrAlreadyExists", err)
	}
}

func TestGCSGetMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	g := newTestGCS(t, "")
	_, _, err := g.Get(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestGCSDeleteMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	g := newTestGCS(t, "")
	if err := g.Delete(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete error = %v, want ErrNotFound", err)
	}
}

func TestGCSListWithPrefixAndPagination(t *testing.T) {
	ctx := context.Background()
	g := newTestGCS(t, "myprefix")

	hashes := []string{"a1", "a2", "a3"}
	for _, h := range hashes {
		if err := g.Put(ctx, h, bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("Put(%s): %v", h, err)
		}
	}

	page1, err := g.List(ctx, "", 2)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1.Entries) != 2 {
		t.Fatalf("page1 entries = %d, want 2", len(page1.Entries))
	}
	for _, e := range page1.Entries {
		if strings.Contains(e.Key, "myprefix") {
			t.Fatalf("entry hash %q still has the prefix", e.Key)
		}
	}
	if page1.NextCursor == "" {
		t.Fatal("expected a NextCursor for a truncated page")
	}

	page2, err := g.List(ctx, page1.NextCursor, 2)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2.Entries) != 1 {
		t.Fatalf("page2 entries = %d, want 1", len(page2.Entries))
	}
}

func TestGCSListWithoutPrefix(t *testing.T) {
	ctx := context.Background()
	g := newTestGCS(t, "")
	if err := g.Put(ctx, "onlyhash", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	page, err := g.List(ctx, "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Key != "onlyhash" {
		t.Fatalf("List = %+v, want one entry for onlyhash", page.Entries)
	}
}

func TestNewGCSWithCredentialsJSON(t *testing.T) {
	// A real service-account key isn't needed to exercise this branch:
	// STORAGE_EMULATOR_HOST (set by newTestGCS-style setup) makes the SDK
	// skip authentication entirely, so any well-formed key JSON just needs
	// to get past the client's own parsing.
	srv, _ := newFakeGCS(t)
	t.Setenv("STORAGE_EMULATOR_HOST", srv.URL)

	fakeKey := []byte(`{
		"type": "service_account",
		"project_id": "test-project",
		"private_key_id": "abc",
		"private_key": "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n-----END PRIVATE KEY-----\n",
		"client_email": "test@test-project.iam.gserviceaccount.com",
		"client_id": "123",
		"token_uri": "https://oauth2.googleapis.com/token"
	}`)
	_, err := NewGCS(context.Background(), GCSOptions{Bucket: "test-bucket", CredentialsJSON: fakeKey})
	if err != nil {
		t.Fatalf("NewGCS with credentials JSON: %v", err)
	}
}

func TestIsPreconditionFailed(t *testing.T) {
	if isPreconditionFailed(nil) {
		t.Error("isPreconditionFailed(nil) = true, want false")
	}
	if isPreconditionFailed(errors.New("some other error")) {
		t.Error("isPreconditionFailed(generic error) = true, want false")
	}
}
