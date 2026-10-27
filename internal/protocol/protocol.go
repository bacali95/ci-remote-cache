// Package protocol assembles the cache data plane from one or more client
// protocol adapters (Nx, ...), each translating its own HTTP contract onto
// the shared internal/cache core. Adapters own disjoint URL paths, so every
// enabled protocol is served side by side on the same handler.
package protocol

import (
	"log/slog"
	"net/http"

	"nx-remote-cache/internal/httplog"
)

type Protocol interface {
	// Name identifies the protocol in logs, e.g. "nx".
	Name() string

	// Register adds the protocol's routes to mux.
	Register(mux *http.ServeMux)
}

// Handler serves GET /health plus the routes of every given protocol,
// with request logging.
func Handler(log *slog.Logger, protocols ...Protocol) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	for _, p := range protocols {
		p.Register(mux)
		log.Info("cache protocol enabled", "protocol", p.Name())
	}
	return httplog.WithLogging(log, mux)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
