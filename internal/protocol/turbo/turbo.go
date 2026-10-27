// Package turbo implements the Turborepo remote cache HTTP contract:
// https://turborepo.dev/api/remote-cache-spec
//
// The turbo client prefixes every path with /v8, so TURBO_API points at
// this server's root. The teamId/slug query parameters are accepted and
// ignored: every valid token sees the same cache.
package turbo

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"ci-remote-cache/internal/auth"
	"ci-remote-cache/internal/cache"
	"ci-remote-cache/internal/storage"
	"ci-remote-cache/internal/store"
)

const namespace = "turbo"

const (
	// maxMetaHeaderLen bounds each x-artifact-* string header; the spec
	// sets 600 for x-artifact-tag and leaves the others unbounded.
	maxMetaHeaderLen = 600
	maxQueryHashes   = 1000
	maxJSONBodyBytes = 1 << 20
)

type Protocol struct {
	svc *cache.Service
	log *slog.Logger
}

func New(svc *cache.Service, log *slog.Logger) *Protocol {
	return &Protocol{svc: svc, log: log}
}

func (p *Protocol) Name() string { return namespace }

func (p *Protocol) Register(mux *http.ServeMux) {
	// GET patterns also match HEAD; a separate HEAD /v8/artifacts/{hash}
	// pattern would conflict with GET /v8/artifacts/status, so handleGet
	// dispatches HEAD itself.
	mux.HandleFunc("GET /v8/artifacts/status", p.handleStatus)
	mux.HandleFunc("GET /v8/artifacts/{hash}", p.handleGet)
	mux.HandleFunc("PUT /v8/artifacts/{hash}", p.handlePut)
	mux.HandleFunc("POST /v8/artifacts", p.handleQuery)
	mux.HandleFunc("POST /v8/artifacts/events", p.handleEvents)
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

func (p *Protocol) internalError(w http.ResponseWriter, msg string, args ...any) {
	p.log.Error(msg, args...)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
}

// authorize writes the error response and returns false if the request's
// bearer token doesn't grant scope.
func (p *Protocol) authorize(w http.ResponseWriter, r *http.Request, scope auth.Scope) bool {
	err := p.svc.Authorize(r.Context(), auth.ExtractBearer(r), scope)
	switch {
	case err == nil:
		return true
	case errors.Is(err, cache.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid authentication token")
	case errors.Is(err, cache.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "token lacks permission for this operation")
	default:
		p.internalError(w, "token authorize failed", "error", err)
	}
	return false
}

// artifactRequest validates the path hash and token, writing the error
// response and returning false if either is rejected.
func (p *Protocol) artifactRequest(w http.ResponseWriter, r *http.Request, scope auth.Scope) (string, bool) {
	hash := r.PathValue("hash")
	if !storage.ValidHash(hash) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid artifact hash")
		return "", false
	}
	return hash, p.authorize(w, r, scope)
}

func (p *Protocol) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !p.authorize(w, r, auth.ScopeRead) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
}

func setMetaHeaders(h http.Header, meta store.ArtifactMeta) {
	if meta.DurationMs > 0 {
		h.Set("x-artifact-duration", strconv.FormatInt(meta.DurationMs, 10))
	}
	if meta.Tag != "" {
		h.Set("x-artifact-tag", meta.Tag)
	}
	if meta.SHA != "" {
		h.Set("x-artifact-sha", meta.SHA)
	}
	if meta.DirtyHash != "" {
		h.Set("x-artifact-dirty-hash", meta.DirtyHash)
	}
}

func (p *Protocol) handleHead(w http.ResponseWriter, r *http.Request, hash string) {
	exists, err := p.svc.Exists(r.Context(), namespace, hash)
	if err != nil {
		p.internalError(w, "check artifact failed", "hash", hash, "error", err)
		return
	}
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p.writeMetaHeaders(w, r, hash)
	w.WriteHeader(http.StatusOK)
}

// writeMetaHeaders adds the artifact's stored x-artifact-* headers. A
// lookup failure only drops the headers: turbo treats an artifact whose
// signature tag is missing as unverifiable, i.e. a cache miss.
func (p *Protocol) writeMetaHeaders(w http.ResponseWriter, r *http.Request, hash string) {
	meta, err := p.svc.Meta(r.Context(), namespace, hash)
	if err != nil {
		p.log.Warn("load artifact metadata failed", "hash", hash, "error", err)
		return
	}
	setMetaHeaders(w.Header(), meta)
}

func (p *Protocol) handleGet(w http.ResponseWriter, r *http.Request) {
	hash, ok := p.artifactRequest(w, r, auth.ScopeRead)
	if !ok {
		return
	}
	if r.Method == http.MethodHead {
		p.handleHead(w, r, hash)
		return
	}

	body, size, err := p.svc.Get(r.Context(), namespace, hash)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "artifact not found")
		return
	}
	if err != nil {
		p.internalError(w, "get artifact failed", "hash", hash, "error", err)
		return
	}
	defer func() { _ = body.Close() }()

	p.writeMetaHeaders(w, r, hash)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		p.log.Warn("stream artifact interrupted", "hash", hash, "error", err)
	}
}

// parseMeta reads the upload's x-artifact-* headers, returning a message
// describing the first invalid one.
func parseMeta(h http.Header) (store.ArtifactMeta, string) {
	meta := store.ArtifactMeta{
		Tag:       h.Get("x-artifact-tag"),
		SHA:       h.Get("x-artifact-sha"),
		DirtyHash: h.Get("x-artifact-dirty-hash"),
	}
	if v := h.Get("x-artifact-duration"); v != "" {
		d, err := strconv.ParseInt(v, 10, 64)
		if err != nil || d < 0 {
			return meta, "x-artifact-duration must be a non-negative integer"
		}
		meta.DurationMs = d
	}
	for _, v := range []string{meta.Tag, meta.SHA, meta.DirtyHash} {
		if len(v) > maxMetaHeaderLen {
			return meta, "x-artifact-* header exceeds 600 characters"
		}
	}
	return meta, ""
}

type uploadResponse struct {
	URLs []string `json:"urls"`
}

// handlePut stores the artifact and its metadata. Re-uploading an existing
// hash is a success: turbo treats any non-2xx upload as a failure, and the
// stored artifact is content-addressed, so it's already the right one.
func (p *Protocol) handlePut(w http.ResponseWriter, r *http.Request) {
	hash, ok := p.artifactRequest(w, r, auth.ScopeWrite)
	if !ok {
		return
	}
	if r.ContentLength <= 0 {
		writeError(w, http.StatusLengthRequired, "bad_request", "Content-Length header is required")
		return
	}
	meta, invalid := parseMeta(r.Header)
	if invalid != "" {
		writeError(w, http.StatusBadRequest, "bad_request", invalid)
		return
	}

	err := p.svc.Put(r.Context(), namespace, hash, r.Body, r.ContentLength, meta)
	switch {
	case err == nil, errors.Is(err, storage.ErrAlreadyExists):
		writeJSON(w, http.StatusAccepted, uploadResponse{URLs: []string{r.URL.Path}})
	case errors.Is(err, cache.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "artifact exceeds maximum allowed size")
	default:
		p.internalError(w, "put artifact failed", "hash", hash, "error", err)
	}
}

type queryRequest struct {
	Hashes []string `json:"hashes"`
}

type artifactInfo struct {
	Size           int64  `json:"size"`
	TaskDurationMs int64  `json:"taskDurationMs"`
	Tag            string `json:"tag,omitempty"`
}

type artifactError struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *Protocol) handleQuery(w http.ResponseWriter, r *http.Request) {
	if !p.authorize(w, r, auth.ScopeRead) {
		return
	}
	var req queryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if len(req.Hashes) > maxQueryHashes {
		writeError(w, http.StatusBadRequest, "bad_request", "too many hashes in one request (max 1000)")
		return
	}

	out := make(map[string]any, len(req.Hashes))
	var valid []string
	for _, h := range req.Hashes {
		if !storage.ValidHash(h) {
			var e artifactError
			e.Error.Message = "invalid artifact hash"
			out[h] = e
			continue
		}
		out[h] = nil
		valid = append(valid, h)
	}

	infos, err := p.svc.Query(r.Context(), namespace, valid)
	if err != nil {
		p.internalError(w, "query artifacts failed", "error", err)
		return
	}
	for h, info := range infos {
		out[h] = artifactInfo{Size: info.Size, TaskDurationMs: info.Meta.DurationMs, Tag: info.Meta.Tag}
	}
	writeJSON(w, http.StatusOK, out)
}

type cacheEvent struct {
	SessionID string `json:"sessionId"`
	Source    string `json:"source"`
	Event     string `json:"event"`
	Hash      string `json:"hash"`
	Duration  int64  `json:"duration,omitempty"`
}

// handleEvents validates and acknowledges turbo's hit/miss analytics.
// They aren't stored: remote hits are already counted when the artifact is
// downloaded, which is what the admin UI and janitor rely on.
func (p *Protocol) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !p.authorize(w, r, auth.ScopeRead) {
		return
	}
	var events []cacheEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&events); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	for _, e := range events {
		if e.SessionID == "" || e.Hash == "" ||
			(e.Source != "LOCAL" && e.Source != "REMOTE") ||
			(e.Event != "HIT" && e.Event != "MISS") {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid cache event")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
