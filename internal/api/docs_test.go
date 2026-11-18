package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The docs are served by the real API handler, without an API key. No
// database is needed: these routes never touch the store.
func TestDocsRoutesArePublic(t *testing.T) {
	h := (&Server{Log: slog.New(slog.DiscardHandler)}).Handler()

	for _, c := range []struct{ path, contentType string }{
		{"/openapi.json", "application/json"},
		{"/llms.txt", "text/plain; charset=utf-8"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s without a key: status %d, body %s", c.path, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != c.contentType {
			t.Errorf("GET %s: Content-Type %q, want %q", c.path, got, c.contentType)
		}
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
			t.Errorf("GET %s: Cache-Control %q", c.path, got)
		}
		switch c.path {
		case "/openapi.json":
			var doc struct {
				OpenAPI string `json:"openapi"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || !strings.HasPrefix(doc.OpenAPI, "3.1.") {
				t.Errorf("GET /openapi.json: not an OpenAPI 3.1 document (%v, %q)", err, doc.OpenAPI)
			}
		case "/llms.txt":
			if !strings.HasPrefix(rec.Body.String(), "# OpenMailWire\n") {
				t.Errorf("GET /llms.txt: unexpected start %.40q", rec.Body.String())
			}
		}
	}

	// The API itself still needs a key.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/domains", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /domains without a key: status %d, want 401", rec.Code)
	}
}
