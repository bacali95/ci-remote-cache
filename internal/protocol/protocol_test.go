package protocol

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeProtocol struct{}

func (fakeProtocol) Name() string { return "fake" }

func (fakeProtocol) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /fake", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
}

func TestHandlerServesHealthWithoutAuth(t *testing.T) {
	h := Handler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("GET /health = %d %q, want 200 \"ok\"", w.Code, w.Body)
	}
}

func TestHandlerRegistersProtocolsAndLogs(t *testing.T) {
	var logs bytes.Buffer
	h := Handler(slog.New(slog.NewTextHandler(&logs, nil)), fakeProtocol{})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fake", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("GET /fake = %d, want 418", w.Code)
	}

	out := logs.String()
	if !strings.Contains(out, "protocol=fake") {
		t.Errorf("logs missing protocol enabled line: %s", out)
	}
	if !strings.Contains(out, "path=/fake") {
		t.Errorf("logs missing request line: %s", out)
	}
}
