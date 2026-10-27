// Package adminapi implements the JSON API behind the admin UI: login
// sessions, user management, cache access token management, and browsing
// and pruning cache entries. Everything here requires an authenticated
// session except POST /admin/api/auth/login.
package adminapi

import (
	"io/fs"
	"log/slog"
	"net/http"

	"ci-remote-cache/internal/httplog"
	"ci-remote-cache/internal/session"
	"ci-remote-cache/internal/settings"
	"ci-remote-cache/internal/storage"
	"ci-remote-cache/internal/store"
)

type Server struct {
	store        *store.Store
	sessions     *session.Manager
	backend      storage.Backend
	settings     *settings.Manager
	log          *slog.Logger
	cookieSecure bool
	loginLimiter *loginLimiter
	uiFS         fs.FS // nil skips serving the static UI (e.g. in tests)
}

func New(st *store.Store, sessions *session.Manager, backend storage.Backend, settingsMgr *settings.Manager, log *slog.Logger, cookieSecure bool, uiFS fs.FS) *Server {
	return &Server{
		store:        st,
		sessions:     sessions,
		backend:      backend,
		settings:     settingsMgr,
		log:          log,
		cookieSecure: cookieSecure,
		loginLimiter: newLoginLimiter(),
		uiFS:         uiFS,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /admin/api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /admin/api/auth/logout", s.protected(s.handleLogout))
	mux.HandleFunc("GET /admin/api/auth/me", s.protected(s.handleMe))

	mux.HandleFunc("GET /admin/api/users", s.protected(s.handleListUsers))
	mux.HandleFunc("POST /admin/api/users", s.protected(s.handleCreateUser))
	mux.HandleFunc("DELETE /admin/api/users/{id}", s.protected(s.handleDeleteUser))
	mux.HandleFunc("POST /admin/api/account/password", s.protected(s.handleChangePassword))

	mux.HandleFunc("GET /admin/api/tokens", s.protected(s.handleListTokens))
	mux.HandleFunc("POST /admin/api/tokens", s.protected(s.handleCreateToken))
	mux.HandleFunc("DELETE /admin/api/tokens/{id}", s.protected(s.handleRevokeToken))

	mux.HandleFunc("GET /admin/api/cache", s.protected(s.handleListCache))
	mux.HandleFunc("DELETE /admin/api/cache/{key}", s.protected(s.handleDeleteCacheEntry))
	mux.HandleFunc("POST /admin/api/cache/bulk-delete", s.protected(s.handleBulkDelete))
	mux.HandleFunc("POST /admin/api/cache/prune", s.protected(s.handlePrune))

	mux.HandleFunc("GET /admin/api/settings", s.protected(s.handleGetSettings))
	mux.HandleFunc("PUT /admin/api/settings", s.protected(s.handleUpdateSettings))

	if s.uiFS != nil {
		mux.Handle("GET /admin/", s.spaHandler())
	}

	return httplog.WithLogging(s.log, mux)
}
