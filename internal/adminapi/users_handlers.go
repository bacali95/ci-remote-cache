package adminapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"ci-remote-cache/internal/session"
	"ci-remote-cache/internal/store"
)

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, _ store.User) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.log.Error("list users failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]userResponse, 0, len(users))
	for _, u := range users {
		out = append(out, userDTO(u))
	}
	writeJSON(w, http.StatusOK, out)
}

type createUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if !strings.Contains(req.Email, "@") || len(req.Email) < 3 {
		writeError(w, http.StatusBadRequest, "invalid email")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	hash, err := session.HashPassword(req.Password)
	if err != nil {
		s.log.Error("hash password failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	u, err := s.store.CreateUser(r.Context(), req.Email, hash)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a user with that email already exists")
		return
	}
	if err != nil {
		s.log.Error("create user failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, userDTO(u))
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, current store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if id == current.ID {
		writeError(w, http.StatusBadRequest, "cannot delete your own account")
		return
	}

	n, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.log.Error("count users failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n <= 1 {
		writeError(w, http.StatusBadRequest, "cannot delete the last remaining admin")
		return
	}

	err = s.store.DeleteUser(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		s.log.Error("delete user failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, current store.User) {
	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !session.VerifyPassword(current.PasswordHash, req.CurrentPassword) {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	hash, err := session.HashPassword(req.NewPassword)
	if err != nil {
		s.log.Error("hash password failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.UpdatePassword(r.Context(), current.ID, hash); err != nil {
		s.log.Error("update password failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
