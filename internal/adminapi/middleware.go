package adminapi

import (
	"net"
	"net/http"
	"sync"
	"time"

	"ci-remote-cache/internal/store"

	"golang.org/x/time/rate"
)

// csrfHeader must be present on every mutating admin request. The session
// cookie is SameSite=Strict, which already blocks it from being sent on
// cross-site requests in modern browsers; requiring a custom header is a
// second, independent layer, since a cross-site <form> POST cannot set
// custom headers.
const csrfHeader = "X-CI-Cache-Admin"

// protected wraps a handler that needs an authenticated admin. Mutating
// methods (anything but GET/HEAD) also require the CSRF header.
func (s *Server) protected(next func(w http.ResponseWriter, r *http.Request, user store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(csrfHeader) == "" {
				writeError(w, http.StatusForbidden, "missing required header")
				return
			}
		}

		raw := ""
		if cookie, err := r.Cookie(sessionCookieName); err == nil {
			raw = cookie.Value
		}

		user, err := s.sessions.CurrentUser(r.Context(), raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		next(w, r, user)
	}
}

// loginLimiter throttles login attempts per client IP. It's an in-memory,
// single-instance limiter — enough for a small self-hosted admin tool; a
// horizontally scaled deployment would need a shared store instead.
type loginLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{limiters: make(map[string]*rate.Limiter)}
}

func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.limiters[key]
	if !ok {
		// 5 attempts, refilling one every 12s (5/min sustained).
		lim = rate.NewLimiter(rate.Every(12*time.Second), 5)
		l.limiters[key] = lim
	}
	return lim.Allow()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
