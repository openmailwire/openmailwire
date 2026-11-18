package apidocs

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// The copies in docs/ are for people browsing the repository. They must stay
// identical to the embedded originals in this directory.
func TestDocsCopiesMatchEmbedded(t *testing.T) {
	for _, c := range []struct {
		name     string
		embedded []byte
	}{
		{"openapi.json", openapiJSON},
		{"llms.txt", llmsTxt},
	} {
		copied, err := os.ReadFile("../../docs/" + c.name)
		if err != nil {
			t.Errorf("docs/%s: %v (copy it: cp internal/apidocs/%s docs/)", c.name, err, c.name)
			continue
		}
		if !bytes.Equal(copied, c.embedded) {
			t.Errorf("docs/%s differs from internal/apidocs/%s. Edit the file in internal/apidocs, then copy it: cp internal/apidocs/%s docs/",
				c.name, c.name, c.name)
		}
	}
}

func TestHandler(t *testing.T) {
	h := Handler()
	for _, c := range []struct {
		path, contentType string
		body              []byte
	}{
		{"/openapi.json", "application/json", openapiJSON},
		{"/llms.txt", "text/plain; charset=utf-8", llmsTxt},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", c.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != c.contentType {
			t.Errorf("GET %s: Content-Type %q, want %q", c.path, got, c.contentType)
		}
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
			t.Errorf("GET %s: Cache-Control %q", c.path, got)
		}
		if !bytes.Equal(rec.Body.Bytes(), c.body) {
			t.Errorf("GET %s: body is not the embedded file", c.path)
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("GET %s: no ETag", c.path)
		}

		// A client that already has this version gets 304 and no body.
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		req.Header.Set("If-None-Match", etag)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("GET %s with If-None-Match: status %d, %d body bytes; want 304 and none", c.path, rec.Code, rec.Body.Len())
		}

		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, c.path, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Errorf("HEAD %s: status %d, %d body bytes", c.path, rec.Code, rec.Body.Len())
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /other.txt: status %d, want 404", rec.Code)
	}
}
