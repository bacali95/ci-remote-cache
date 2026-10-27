// Package integration wires the data plane (internal/cache plus the
// internal/protocol adapters) and the admin API (internal/adminapi)
// together exactly like cmd/server/main.go does, then drives the whole
// system over real HTTP. Neither side's own tests do this: the protocol
// tests fake token auth, internal/adminapi never
// touches the data plane, so a token minted by the admin API and a request
// against /v1/cache have never actually been exercised together before.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"ci-remote-cache/internal/adminapi"
	"ci-remote-cache/internal/auth"
	"ci-remote-cache/internal/cache"
	"ci-remote-cache/internal/db"
	"ci-remote-cache/internal/protocol"
	"ci-remote-cache/internal/protocol/nx"
	"ci-remote-cache/internal/protocol/turbo"
	"ci-remote-cache/internal/session"
	"ci-remote-cache/internal/settings"
	"ci-remote-cache/internal/storage"
	"ci-remote-cache/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

const csrfHeader = "X-CI-Cache-Admin"

// testSystem boots the full app (data-plane + admin API, sharing one store,
// one dynamic storage backend, one settings manager) against a real
// Postgres, the same composition as cmd/server/main.go's run(). Skips
// unless TEST_DATABASE_URL is set.
func testSystem(t *testing.T) (baseURL string, st *store.Store) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping Postgres integration test")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE users, sessions, tokens, cache_artifacts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO app_settings (id) VALUES (true)`); err != nil {
		t.Fatalf("reseed app_settings: %v", err)
	}

	tempDir := t.TempDir()
	if _, err := pool.Exec(context.Background(), `UPDATE app_settings SET local_dir = $1`, tempDir); err != nil {
		t.Fatalf("set local_dir: %v", err)
	}
	st = store.New(pool)

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate encryption key: %v", err)
	}
	enc, err := settings.NewEncryptor(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	dynBackend := storage.NewDynamic(nil)
	sessions := session.NewManager(st, time.Hour)
	cacheSvc := cache.New(dynBackend, auth.NewCacheTokenAuth(st), st, log, 0)
	settingsMgr := settings.NewManager(st, enc, dynBackend, sessions, cacheSvc)
	if err := settingsMgr.Load(context.Background()); err != nil {
		t.Fatalf("settings Load: %v", err)
	}
	adminSrv := adminapi.New(st, sessions, dynBackend, settingsMgr, log, false, nil)

	mux := http.NewServeMux()
	mux.Handle("/admin/", adminSrv.Handler())
	mux.Handle("/", protocol.Handler(log, nx.New(cacheSvc, log), turbo.New(cacheSvc, log)))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, st
}

func seedUser(t *testing.T, st *store.Store, email, password string) {
	t.Helper()
	hash, err := session.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := st.CreateUser(context.Background(), email, hash); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
}

// adminClient drives the admin API with a real *http.Client, so the login
// session cookie and CSRF header behave exactly as they would for the
// browser.
type adminClient struct {
	t       *testing.T
	baseURL string
	client  *http.Client
}

func newAdminClient(t *testing.T, baseURL string) *adminClient {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	return &adminClient{t: t, baseURL: baseURL, client: &http.Client{Jar: jar}}
}

func (c *adminClient) do(method, path string, body any, csrf bool) *http.Response {
	c.t.Helper()
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set(csrfHeader, "1")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func decodeBody[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return out
}

func (c *adminClient) login(email, password string) {
	c.t.Helper()
	resp := c.do(http.MethodPost, "/admin/api/auth/login",
		map[string]string{"email": email, "password": password}, true)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("login: status = %d", resp.StatusCode)
	}
}

type createdToken struct {
	ID    int64  `json:"id"`
	Token string `json:"token"`
}

func (c *adminClient) createToken(name, scope string) createdToken {
	c.t.Helper()
	resp := c.do(http.MethodPost, "/admin/api/tokens", map[string]string{"name": name, "scope": scope}, true)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("create token: status = %d, body = %s", resp.StatusCode, body)
	}
	return decodeBody[createdToken](c.t, resp)
}

func (c *adminClient) revokeToken(id int64) {
	c.t.Helper()
	resp := c.do(http.MethodDelete, fmt.Sprintf("/admin/api/tokens/%d", id), nil, true)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		c.t.Fatalf("revoke token: status = %d", resp.StatusCode)
	}
}

type cacheEntry struct {
	Key       string `json:"key"`
	Protocol  string `json:"protocol"`
	Hash      string `json:"hash"`
	ReadCount int64  `json:"readCount"`
}

func (c *adminClient) listCache() []cacheEntry {
	c.t.Helper()
	resp := c.do(http.MethodGet, "/admin/api/cache", nil, false)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("list cache: status = %d", resp.StatusCode)
	}
	out := decodeBody[struct {
		Entries []cacheEntry `json:"entries"`
	}](c.t, resp)
	return out.Entries
}

func (c *adminClient) deleteCacheEntry(key string) {
	c.t.Helper()
	resp := c.do(http.MethodDelete, "/admin/api/cache/"+url.PathEscape(key), nil, true)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		c.t.Fatalf("delete cache entry: status = %d", resp.StatusCode)
	}
}

func cacheRequest(t *testing.T, baseURL, method, hash, token string, body []byte) *http.Response {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+"/v1/cache/"+hash, reqBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s /v1/cache/%s: %v", method, hash, err)
	}
	return resp
}

// TestFullCacheAndAdminFlow drives the whole product end to end: an admin
// logs in, mints a write token, an Nx client (a plain bearer-token HTTP
// caller, standing in for the real `nx` CLI) uses it to push and pull a
// cache artifact, the admin observes the read in the UI's cache list, a
// read-only token is confirmed to reject writes, revoking a token cuts off
// its access immediately, and deleting an entry from the admin UI makes it
// a 404 again on the data plane.
func TestFullCacheAndAdminFlow(t *testing.T) {
	baseURL, st := testSystem(t)
	seedUser(t, st, "admin@example.com", "correct-password")

	admin := newAdminClient(t, baseURL)
	admin.login("admin@example.com", "correct-password")

	writeTok := admin.createToken("ci-write", "write")
	const hash = "integrationhash1"
	payload := []byte("build output bytes")

	putResp := cacheRequest(t, baseURL, http.MethodPut, hash, writeTok.Token, payload)
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT with fresh write token: status = %d, want 200", putResp.StatusCode)
	}

	getResp := cacheRequest(t, baseURL, http.MethodGet, hash, writeTok.Token, nil)
	got, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET after PUT: status = %d, want 200", getResp.StatusCode)
	}
	if string(got) != string(payload) {
		t.Fatalf("GET body = %q, want %q", got, payload)
	}

	// The read is recorded off the request's critical path (see
	// cache.Service.Get), so poll instead of asserting immediately.
	var entries []cacheEntry
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries = admin.listCache()
		if len(entries) == 1 && entries[0].ReadCount > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(entries) != 1 || entries[0].Key != "nx/"+hash {
		t.Fatalf("admin cache list = %+v, want one entry for nx/%s", entries, hash)
	}
	if entries[0].ReadCount == 0 {
		t.Fatalf("admin cache list shows ReadCount = 0, want the earlier GET to have been recorded")
	}

	readTok := admin.createToken("ci-read", "read")
	if resp := cacheRequest(t, baseURL, http.MethodGet, hash, readTok.Token, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with read token: status = %d, want 200", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	if resp := cacheRequest(t, baseURL, http.MethodPut, "otherhash", readTok.Token, payload); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT with read-only token: status = %d, want 403", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}

	admin.revokeToken(writeTok.ID)
	if resp := cacheRequest(t, baseURL, http.MethodPut, "afterrevoke", writeTok.Token, payload); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("PUT with revoked token: status = %d, want 401", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}

	admin.deleteCacheEntry(entries[0].Key)
	if resp := cacheRequest(t, baseURL, http.MethodGet, hash, readTok.Token, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after admin delete: status = %d, want 404", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
}

// TestNxAndTurboShareTokensButNotArtifacts runs both clients against one
// server: a single admin-minted token works for both, the same hash
// uploaded by each lands in separate namespaces, and Turborepo's
// x-artifact-* metadata round-trips through Postgres.
func TestNxAndTurboShareTokensButNotArtifacts(t *testing.T) {
	baseURL, st := testSystem(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	admin := newAdminClient(t, baseURL)
	admin.login("admin@example.com", "correct-password")
	tok := admin.createToken("ci", "write").Token

	const hash = "abc123def456"
	if resp := cacheRequest(t, baseURL, http.MethodPut, hash, tok, []byte("nx-artifact")); resp.StatusCode != http.StatusOK {
		t.Fatalf("nx PUT: status = %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}

	turbo := func(method string, body []byte, headers map[string]string) *http.Response {
		t.Helper()
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, baseURL+"/v8/artifacts/"+hash+"?slug=team", reqBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("turbo %s: %v", method, err)
		}
		return resp
	}

	if resp := turbo(http.MethodGet, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("turbo GET before upload: status = %d, want 404 (nx artifact must not leak)", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}

	resp := turbo(http.MethodPut, []byte("turbo-artifact"), map[string]string{
		"x-artifact-duration": "250",
		"x-artifact-tag":      "dGFn",
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("turbo PUT: status = %d, want 202", resp.StatusCode)
	}

	resp = turbo(http.MethodGet, nil, nil)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != "turbo-artifact" {
		t.Fatalf("turbo GET = %d %q, want 200 turbo-artifact", resp.StatusCode, got)
	}
	if tag, dur := resp.Header.Get("x-artifact-tag"), resp.Header.Get("x-artifact-duration"); tag != "dGFn" || dur != "250" {
		t.Fatalf("turbo GET metadata = tag %q, duration %q; want dGFn, 250", tag, dur)
	}

	nxResp := cacheRequest(t, baseURL, http.MethodGet, hash, tok, nil)
	got, _ = io.ReadAll(nxResp.Body)
	_ = nxResp.Body.Close()
	if string(got) != "nx-artifact" {
		t.Fatalf("nx GET = %q, want nx-artifact unaffected by the turbo upload", got)
	}

	var keys []string
	for _, e := range admin.listCache() {
		if e.Hash != hash {
			t.Errorf("admin entry %s has hash %q, want %q", e.Key, e.Hash, hash)
		}
		keys = append(keys, e.Protocol+"/"+e.Hash)
	}
	slices.Sort(keys)
	if want := []string{"nx/" + hash, "turbo/" + hash}; !slices.Equal(keys, want) {
		t.Fatalf("admin cache keys = %v, want %v", keys, want)
	}
}
