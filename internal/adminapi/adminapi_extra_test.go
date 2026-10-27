package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"nx-remote-cache/internal/session"
)

func TestHandlerServesSpaWhenUIFSProvided(t *testing.T) {
	fsys := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>index</html>")}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(nil, nil, nil, nil, log, false, fsys)

	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK || w.Body.String() != "<html>index</html>" {
		t.Fatalf("status=%d body=%q, want the SPA route wired into Handler()", w.Code, w.Body.String())
	}
}

func TestClientIPFallsBackToRawRemoteAddrWithoutPort(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")

	r := httptest.NewRequest(http.MethodPost, "/admin/api/auth/login", bytes.NewReader([]byte(`{"email":"admin@example.com","password":"correct-password"}`)))
	r.RemoteAddr = "no-port-here" // net.SplitHostPort fails on this, exercising clientIP's fallback
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(csrfHeader, "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("login with unparseable RemoteAddr: status = %d, body = %s", w.Code, w.Body)
	}
}

func TestListUsersOverHTTP(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodGet, "/admin/api/users", nil, cookie, false)
	if w.Code != http.StatusOK {
		t.Fatalf("list users: status = %d, body = %s", w.Code, w.Body)
	}
	var users []userResponse
	if err := json.Unmarshal(w.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(users) != 1 || users[0].Email != "admin@example.com" {
		t.Fatalf("users = %+v, want one entry for admin@example.com", users)
	}
}

func TestCreateUserValidation(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	cases := []struct {
		name string
		req  createUserRequest
	}{
		{"missing @", createUserRequest{Email: "notanemail", Password: "longenough"}},
		{"too short", createUserRequest{Email: "a@", Password: "longenough"}},
		{"short password", createUserRequest{Email: "new@example.com", Password: "short"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doJSON(t, h, http.MethodPost, "/admin/api/users", c.req, cookie, true)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body)
			}
		})
	}

	w := doJSON(t, h, http.MethodPost, "/admin/api/users", createUserRequest{Email: "dup@example.com", Password: "longenough"}, cookie, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, body = %s", w.Code, w.Body)
	}
	w = doJSON(t, h, http.MethodPost, "/admin/api/users", createUserRequest{Email: "dup@example.com", Password: "longenough"}, cookie, true)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate create: status = %d, want 409, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodPost, "/admin/api/users", "not-json", cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid body: status = %d, want 400", w.Code)
	}
}

// bcrypt rejects passwords over 72 bytes — the one realistic, deterministic
// way to make session.HashPassword fail and exercise that error branch.
func tooLongForBcrypt() string {
	b := make([]byte, 73)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestCreateUserHashPasswordTooLong(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodPost, "/admin/api/users", createUserRequest{
		Email:    "new@example.com",
		Password: tooLongForBcrypt(),
	}, cookie, true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", w.Code, w.Body)
	}
}

func TestChangePasswordHashPasswordTooLong(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodPost, "/admin/api/account/password", changePasswordRequest{
		CurrentPassword: "correct-password",
		NewPassword:     tooLongForBcrypt(),
	}, cookie, true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", w.Code, w.Body)
	}
}

func TestDeleteUserNotFound(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	// A second admin so the "last remaining admin" guard doesn't shadow the
	// not-found check for an unrelated, nonexistent id.
	w := doJSON(t, h, http.MethodPost, "/admin/api/users", createUserRequest{Email: "second@example.com", Password: "longenough"}, cookie, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed second admin: status = %d, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodDelete, "/admin/api/users/999999", nil, cookie, true)
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete missing user: status = %d, want 404, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodDelete, "/admin/api/users/not-a-number", nil, cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("delete with invalid id: status = %d, want 400", w.Code)
	}
}

func TestChangePasswordOverHTTP(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodPost, "/admin/api/account/password", changePasswordRequest{
		CurrentPassword: "wrong-password",
		NewPassword:     "new-long-password",
	}, cookie, true)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password: status = %d, want 401, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodPost, "/admin/api/account/password", changePasswordRequest{
		CurrentPassword: "correct-password",
		NewPassword:     "short",
	}, cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("short new password: status = %d, want 400, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodPost, "/admin/api/account/password", "not-json", cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid body: status = %d, want 400", w.Code)
	}

	w = doJSON(t, h, http.MethodPost, "/admin/api/account/password", changePasswordRequest{
		CurrentPassword: "correct-password",
		NewPassword:     "new-long-password",
	}, cookie, true)
	if w.Code != http.StatusOK {
		t.Fatalf("change password: status = %d, body = %s", w.Code, w.Body)
	}

	// The old password must no longer work, and the new one must.
	w = doJSON(t, h, http.MethodPost, "/admin/api/auth/login", loginRequest{Email: "admin@example.com", Password: "correct-password"}, nil, true)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password: status = %d, want 401", w.Code)
	}
	w = doJSON(t, h, http.MethodPost, "/admin/api/auth/login", loginRequest{Email: "admin@example.com", Password: "new-long-password"}, nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("login with new password: status = %d, want 200, body = %s", w.Code, w.Body)
	}
}

func TestLogoutClearsSessionOverHTTP(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodPost, "/admin/api/auth/logout", nil, cookie, true)
	if w.Code != http.StatusOK {
		t.Fatalf("logout: status = %d, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodGet, "/admin/api/auth/me", nil, cookie, false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: status = %d, want 401", w.Code)
	}
}

// TestLogoutWithoutCookieClearsCookieAnyway calls handleLogout directly
// (bypassing the protected() wrapper, which never lets a cookie-less
// request reach it over real HTTP) to exercise the defensive branch where
// r.Cookie finds nothing. handleLogout only touches s.sessions, so a
// minimal *Server sharing the same store's session table is enough.
func TestLogoutWithoutCookieClearsCookieAnyway(t *testing.T) {
	_, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	adminSrv := New(st, session.NewManager(st, time.Hour), nil, nil, log, false, nil)

	users, err := st.ListUsers(context.Background())
	if err != nil || len(users) == 0 {
		t.Fatalf("ListUsers: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/admin/api/auth/logout", nil)
	w := httptest.NewRecorder()
	adminSrv.handleLogout(w, r, users[0])

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(w.Result().Cookies()) != 1 {
		t.Fatalf("expected the handler to still clear the session cookie")
	}
}

func TestLoginRateLimited(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")

	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = doJSON(t, h, http.MethodPost, "/admin/api/auth/login", loginRequest{Email: "admin@example.com", Password: "wrong"}, nil, true)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("6th rapid login attempt: status = %d, want 429, body = %s", last.Code, last.Body)
	}

	w := doJSON(t, h, http.MethodPost, "/admin/api/auth/login", "not-json", nil, true)
	// Rate limited by now, so this also proves the limiter check runs before
	// body decoding — but the important case (invalid body) is covered
	// below with a fresh limiter.
	_ = w
}

func TestLoginInvalidBody(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodPost, "/admin/api/auth/login", "not-json", nil, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid login body: status = %d, want 400, body = %s", w.Code, w.Body)
	}
}

func TestCreateTokenValidation(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodPost, "/admin/api/tokens", createTokenRequest{Name: "  ", Scope: "write"}, cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("blank name: status = %d, want 400, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodPost, "/admin/api/tokens", createTokenRequest{Name: "bad-scope", Scope: "admin"}, cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope: status = %d, want 400, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodPost, "/admin/api/tokens", "not-json", cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid body: status = %d, want 400", w.Code)
	}
}

func TestRevokeTokenInvalidID(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodDelete, "/admin/api/tokens/not-a-number", nil, cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body)
	}
}

func TestUpdateSettingsInvalidBody(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodPut, "/admin/api/settings", "not-json", cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body)
	}
}

func TestListCachePagination(t *testing.T) {
	h, st, backend := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")
	ctx := context.Background()

	for _, hash := range []string{"aaa1111111111111111111111111111111111111", "bbb2222222222222222222222222222222222222", "ccc3333333333333333333333333333333333333"} {
		if err := backend.Put(ctx, hash, bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("seed %s: %v", hash, err)
		}
	}

	w := doJSON(t, h, http.MethodGet, "/admin/api/cache?limit=2", nil, cookie, false)
	if w.Code != http.StatusOK {
		t.Fatalf("list cache: status = %d, body = %s", w.Code, w.Body)
	}
	var page listCacheResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Entries) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %+v, want 2 entries and a next cursor", page)
	}

	w = doJSON(t, h, http.MethodGet, "/admin/api/cache?cursor="+page.NextCursor, nil, cookie, false)
	if w.Code != http.StatusOK {
		t.Fatalf("second page: status = %d, body = %s", w.Code, w.Body)
	}
	var page2 listCacheResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	if len(page2.Entries) != 1 {
		t.Fatalf("page2 = %+v, want 1 remaining entry", page2)
	}

	// An out-of-range limit value (<=0 or >max) falls back to the default,
	// exercising that branch too.
	w = doJSON(t, h, http.MethodGet, "/admin/api/cache?limit=abc", nil, cookie, false)
	if w.Code != http.StatusOK {
		t.Fatalf("non-numeric limit: status = %d, want 200 (ignored, falls back to default)", w.Code)
	}
}

func TestDeleteCacheEntryInvalidHash(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodDelete, "/admin/api/cache/..%2f..%2fetc", nil, cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body)
	}
}

func TestPruneValidation(t *testing.T) {
	h, st, _ := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")

	w := doJSON(t, h, http.MethodPost, "/admin/api/cache/prune", pruneRequest{OlderThanDays: 0}, cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("zero days: status = %d, want 400, body = %s", w.Code, w.Body)
	}

	w = doJSON(t, h, http.MethodPost, "/admin/api/cache/prune", "not-json", cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid body: status = %d, want 400", w.Code)
	}
}

func TestBulkDeleteOverHTTP(t *testing.T) {
	h, st, backend := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")
	ctx := context.Background()

	if err := backend.Put(ctx, "bulk1", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("seed bulk1: %v", err)
	}
	if err := backend.Put(ctx, "bulk2", bytes.NewReader([]byte("y")), 1); err != nil {
		t.Fatalf("seed bulk2: %v", err)
	}

	w := doJSON(t, h, http.MethodPost, "/admin/api/cache/bulk-delete", bulkDeleteRequest{
		Keys: []string{"bulk1", "bulk2", "doesnotexist", "..invalid.."},
	}, cookie, true)
	if w.Code != http.StatusOK {
		t.Fatalf("bulk delete: status = %d, body = %s", w.Code, w.Body)
	}
	var result deleteCountResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (bulk1, bulk2; doesnotexist and the invalid hash don't count)", result.Deleted)
	}

	w = doJSON(t, h, http.MethodPost, "/admin/api/cache/bulk-delete", "not-json", cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid body: status = %d, want 400", w.Code)
	}

	tooMany := make([]string, 1001)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	w = doJSON(t, h, http.MethodPost, "/admin/api/cache/bulk-delete", bulkDeleteRequest{Keys: tooMany}, cookie, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("too many hashes: status = %d, want 400, body = %s", w.Code, w.Body)
	}
}

func TestSpaHandlerServesFilesAndFallsBackToIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>index</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(nil, nil, nil, nil, log, false, fsys)

	h := srv.spaHandler()

	// A real, existing file is served as-is.
	r := httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "console.log(1)" {
		t.Fatalf("asset: status=%d body=%q", w.Code, w.Body.String())
	}

	// A client-side route falls back to index.html.
	r = httptest.NewRequest(http.MethodGet, "/admin/tokens", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "<html>index</html>" {
		t.Fatalf("fallback: status=%d body=%q", w.Code, w.Body.String())
	}

	// "/admin/" itself (empty path after trimming) also serves index.html.
	r = httptest.NewRequest(http.MethodGet, "/admin/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "<html>index</html>" {
		t.Fatalf("root: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestSpaHandlerServeIndexErrorWhenMissing(t *testing.T) {
	fsys := fstest.MapFS{} // no index.html at all
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(nil, nil, nil, nil, log, false, fsys)

	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	w := httptest.NewRecorder()
	srv.spaHandler().ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when index.html itself is missing", w.Code)
	}
}

func TestDeleteNamespacedCacheEntries(t *testing.T) {
	h, st, backend := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")
	ctx := context.Background()

	for _, k := range []string{"nx/abc", "turbo/def"} {
		if err := backend.Put(ctx, k, bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	w := doJSON(t, h, http.MethodDelete, "/admin/api/cache/nx%2Fabc", nil, cookie, true)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete nx/abc: status = %d, body = %s", w.Code, w.Body)
	}
	w = doJSON(t, h, http.MethodPost, "/admin/api/cache/bulk-delete", bulkDeleteRequest{
		Keys: []string{"turbo/def"},
	}, cookie, true)
	var result deleteCountResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Deleted != 1 {
		t.Fatalf("bulk delete turbo/def: status = %d, body = %s", w.Code, w.Body)
	}

	page, err := backend.List(ctx, "", "", 10)
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("entries left after deletes: %+v, %v", page.Entries, err)
	}
}

func TestListCacheProtocolFieldsAndFilter(t *testing.T) {
	h, st, backend := testServer(t)
	seedUser(t, st, "admin@example.com", "correct-password")
	cookie := loginAndGetCookie(t, h, "admin@example.com", "correct-password")
	ctx := context.Background()

	for _, k := range []string{"oldhash", "nx/nxhash", "turbo/turbohash"} {
		if err := backend.Put(ctx, k, bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	list := func(query string) []cacheEntryResponse {
		t.Helper()
		w := doJSON(t, h, http.MethodGet, "/admin/api/cache"+query, nil, cookie, false)
		if w.Code != http.StatusOK {
			t.Fatalf("list %s: status = %d, body = %s", query, w.Code, w.Body)
		}
		var page listCacheResponse
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return page.Entries
	}

	byKey := map[string]cacheEntryResponse{}
	for _, e := range list("") {
		byKey[e.Key] = e
	}
	want := map[string]cacheEntryResponse{
		"oldhash":         {Key: "oldhash", Protocol: "nx", Hash: "oldhash", Legacy: true},
		"nx/nxhash":       {Key: "nx/nxhash", Protocol: "nx", Hash: "nxhash"},
		"turbo/turbohash": {Key: "turbo/turbohash", Protocol: "turbo", Hash: "turbohash"},
	}
	for k, w := range want {
		got := byKey[k]
		if got.Key != w.Key || got.Protocol != w.Protocol || got.Hash != w.Hash || got.Legacy != w.Legacy {
			t.Errorf("entry %s = %+v, want key/protocol/hash/legacy %+v", k, got, w)
		}
	}

	if got := list("?protocol=turbo"); len(got) != 1 || got[0].Key != "turbo/turbohash" {
		t.Fatalf("protocol=turbo entries = %+v, want only turbo/turbohash", got)
	}
	if got := list("?protocol=nx"); len(got) != 1 || got[0].Key != "nx/nxhash" {
		t.Fatalf("protocol=nx entries = %+v, want only nx/nxhash", got)
	}

	w := doJSON(t, h, http.MethodGet, "/admin/api/cache?protocol=..%2F", nil, cookie, false)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid protocol: status = %d, want 400", w.Code)
	}
}
