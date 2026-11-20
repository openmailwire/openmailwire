package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, method, target string, header http.Header) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIndexHeaders(t *testing.T) {
	res := get(t, http.MethodGet, "/dashboard/", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	want := map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
		"X-Frame-Options":         "DENY",
	}
	for k, v := range want {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	html := body(t, res)
	if !strings.Contains(html, `<script type="module" src="app.js"></script>`) {
		t.Error("index.html does not load app.js as a module")
	}
	if index := get(t, http.MethodGet, "/dashboard/index.html", nil); body(t, index) != html {
		t.Error("/dashboard/index.html differs from /dashboard/")
	}
}

// The CSP forbids inline code, so the page must not contain any: it would
// silently not run.
func TestIndexHasNoInlineCode(t *testing.T) {
	html := body(t, get(t, http.MethodGet, "/dashboard/", nil))
	for _, tag := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("inline script: %s", tag)
		}
	}
	for _, re := range []string{`(?i)<style\b`, `(?i)\sstyle\s*=`, `(?i)\son[a-z]+\s*=`, `(?i)javascript:`} {
		if m := regexp.MustCompile(re).FindString(html); m != "" {
			t.Errorf("index.html contains %q", m)
		}
	}
}

func TestAssets(t *testing.T) {
	cases := map[string]string{
		"/dashboard/app.js":        "text/javascript; charset=utf-8",
		"/dashboard/lib.js":        "text/javascript; charset=utf-8",
		"/dashboard/ui.js":         "text/javascript; charset=utf-8",
		"/dashboard/ops.js":        "text/javascript; charset=utf-8",
		"/dashboard/domains.js":    "text/javascript; charset=utf-8",
		"/dashboard/styles.css":    "text/css; charset=utf-8",
		"/dashboard/logo.svg":      "image/svg+xml",
		"/dashboard/logo-dark.svg": "image/svg+xml",
	}
	for path, ctype := range cases {
		res := get(t, http.MethodGet, path, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, res.StatusCode)
			continue
		}
		if got := res.Header.Get("Content-Type"); got != ctype {
			t.Errorf("%s: Content-Type %q, want %q", path, got, ctype)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Content-Security-Policy") != CSP {
			t.Errorf("%s: missing security headers", path)
		}
		if len(body(t, res)) == 0 {
			t.Errorf("%s: empty", path)
		}
	}
}

func TestAssetRevalidation(t *testing.T) {
	res := get(t, http.MethodGet, "/dashboard/app.js", nil)
	etag := res.Header.Get("ETag")
	if etag == "" || res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("ETag %q, Cache-Control %q", etag, res.Header.Get("Cache-Control"))
	}
	again := get(t, http.MethodGet, "/dashboard/app.js", http.Header{"If-None-Match": {etag}})
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: status %d, want 304", again.StatusCode)
	}
}

func TestRedirect(t *testing.T) {
	res := get(t, http.MethodGet, "/dashboard", nil)
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/dashboard/" {
		t.Errorf("status %d, Location %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestNotFoundAndMethods(t *testing.T) {
	for _, path := range []string{"/dashboard/nope.js", "/dashboard/static/app.js", "/dashboard/../go.mod"} {
		if res := get(t, http.MethodGet, path, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, res.StatusCode)
		}
	}
	res := get(t, http.MethodPost, "/dashboard/", nil)
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: status %d, Allow %q", res.StatusCode, res.Header.Get("Allow"))
	}
	head := get(t, http.MethodHead, "/dashboard/", nil)
	if head.StatusCode != http.StatusOK || body(t, head) != "" {
		t.Errorf("HEAD: status %d", head.StatusCode)
	}
}

// Every module app.js imports, directly or not, must be served: a missing
// one would leave the page blank.
func TestModulesServed(t *testing.T) {
	importRE := regexp.MustCompile(`(?m)^\s*import\b[^'"]*['"]\./([\w-]+\.js)['"]`)
	seen := map[string]bool{"app.js": true}
	queue := []string{"app.js"}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		res := get(t, http.MethodGet, "/dashboard/"+name, nil)
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
			t.Fatalf("%s: status %d, %q", name, res.StatusCode, res.Header.Get("Content-Type"))
		}
		for _, m := range importRE.FindAllStringSubmatch(body(t, res), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				queue = append(queue, m[1])
			}
		}
	}
	for name := range assets {
		if strings.HasSuffix(name, ".js") && !seen[name] {
			t.Errorf("%s is embedded but no module imports it", name)
		}
	}
	if len(seen) < 5 {
		t.Errorf("only %d modules reachable: %v", len(seen), seen)
	}
}
