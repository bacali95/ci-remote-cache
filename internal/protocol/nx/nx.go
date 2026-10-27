// Package nx implements the Nx self-hosted remote cache HTTP contract:
// https://nx.dev/docs/kb/self-hosted-caching#build-your-own-caching-server
package nx

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"nx-remote-cache/internal/auth"
	"nx-remote-cache/internal/cache"
	"nx-remote-cache/internal/storage"
)

type Protocol struct {
	svc *cache.Service
	log *slog.Logger
}

func New(svc *cache.Service, log *slog.Logger) *Protocol {
	return &Protocol{svc: svc, log: log}
}

func (p *Protocol) Name() string { return "nx" }

func (p *Protocol) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/cache/{hash}", p.handleGet)
	mux.HandleFunc("PUT /v1/cache/{hash}", p.handlePut)
}

// authorize writes the Nx-contract error response and returns false if the
// request's bearer token doesn't grant scope.
func (p *Protocol) authorize(w http.ResponseWriter, r *http.Request, scope auth.Scope) bool {
	err := p.svc.Authorize(r.Context(), auth.ExtractBearer(r), scope)
	switch {
	case err == nil:
		return true
	case errors.Is(err, cache.ErrUnauthorized):
		http.Error(w, "Missing or invalid authentication token", http.StatusUnauthorized)
	case errors.Is(err, cache.ErrForbidden):
		http.Error(w, "Access forbidden", http.StatusForbidden)
	default:
		p.log.Error("token authorize failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
	return false
}

// handleGet implements "Download a task output": 200 + binary body on hit,
// 404 on miss, 401/403 on auth failure.
func (p *Protocol) handleGet(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !storage.ValidHash(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	if !p.authorize(w, r, auth.ScopeRead) {
		return
	}

	body, size, err := p.svc.Get(r.Context(), hash)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		p.log.Error("get artifact failed", "hash", hash, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		p.log.Warn("stream artifact interrupted", "hash", hash, "error", err)
	}
}

// handlePut implements "Upload a task output": 200 on success, 409 if the
// hash is already stored (cache entries are immutable/content-addressed),
// 401/403 on auth failure.
func (p *Protocol) handlePut(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !storage.ValidHash(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	if !p.authorize(w, r, auth.ScopeWrite) {
		return
	}

	if r.ContentLength <= 0 {
		http.Error(w, "Content-Length header is required", http.StatusLengthRequired)
		return
	}

	err := p.svc.Put(r.Context(), hash, r.Body, r.ContentLength)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, cache.ErrTooLarge):
		http.Error(w, "artifact exceeds maximum allowed size", http.StatusRequestEntityTooLarge)
	case errors.Is(err, storage.ErrAlreadyExists):
		http.Error(w, "artifact already exists", http.StatusConflict)
	default:
		p.log.Error("put artifact failed", "hash", hash, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
