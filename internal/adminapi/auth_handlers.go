package adminapi

import (
	"errors"
	"net/http"
	"time"

	"ci-remote-cache/internal/session"
	"ci-remote-cache/internal/store"
)

const sessionCookieName = "cicache_session"

func (s *Server) setSessionCookie(w http.ResponseWriter, rawSessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    rawSessionID,
		Path:     "/admin/api",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.sessions.TTL().Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/admin/api",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimiter.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts, try again shortly")
		return
	}

	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	raw, err := s.sessions.Login(r.Context(), req.Email, req.Password)
	if errors.Is(err, session.ErrInvalidCredentials) {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err != nil {
		s.log.Error("login failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	s.setSessionCookie(w, raw)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ store.User) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		_ = s.sessions.Logout(r.Context(), cookie.Value)
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, user store.User) {
	writeJSON(w, http.StatusOK, userDTO(user))
}

type userResponse struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"createdAt"`
}

func userDTO(u store.User) userResponse {
	return userResponse{ID: u.ID, Email: u.Email, CreatedAt: u.CreatedAt}
}
