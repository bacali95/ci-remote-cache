package adminapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ci-remote-cache/internal/auth"
	"ci-remote-cache/internal/store"
)

type tokenResponse struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

func tokenDTO(t store.Token) tokenResponse {
	return tokenResponse{
		ID:         t.ID,
		Name:       t.Name,
		Scope:      string(t.Scope),
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
		RevokedAt:  t.RevokedAt,
	}
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request, _ store.User) {
	tokens, err := s.store.ListTokens(r.Context())
	if err != nil {
		s.log.Error("list tokens failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]tokenResponse, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenDTO(t))
	}
	writeJSON(w, http.StatusOK, out)
}

type createTokenRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

type createTokenResponse struct {
	tokenResponse
	Token string `json:"token"` // raw value; shown once, never retrievable again
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, current store.User) {
	var req createTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	scope := store.TokenScope(req.Scope)
	if scope != store.ScopeRead && scope != store.ScopeWrite {
		writeError(w, http.StatusBadRequest, `scope must be "read" or "write"`)
		return
	}

	raw, err := auth.GenerateToken()
	if err != nil {
		s.log.Error("generate token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	t, err := s.store.CreateToken(r.Context(), req.Name, scope, auth.HashToken(raw), current.ID)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "token collision, please retry")
		return
	}
	if err != nil {
		s.log.Error("create token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, createTokenResponse{tokenResponse: tokenDTO(t), Token: raw})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid token id")
		return
	}

	err = s.store.RevokeToken(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		s.log.Error("revoke token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
