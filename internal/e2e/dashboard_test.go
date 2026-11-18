package e2e

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openmailwire/openmailwire/internal/dashboard"
)

// TestDashboard checks the dashboard as the control plane mounts it, and
// the API behaviour its pages rely on.
func TestDashboard(t *testing.T) {
	e := newEnv(t, nil)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	res, err := noRedirect.Get(e.ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/dashboard/" {
		t.Fatalf("/dashboard: %d, Location %q", res.StatusCode, res.Header.Get("Location"))
	}

	// The files need no API key: the page asks for one.
	for path, ctype := range map[string]string{
		"/dashboard/":           "text/html; charset=utf-8",
		"/dashboard/app.js":     "text/javascript; charset=utf-8",
		"/dashboard/styles.css": "text/css; charset=utf-8",
	} {
		res, err := http.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != ctype || len(b) == 0 {
			t.Errorf("%s: %d %q (%d bytes)", path, res.StatusCode, res.Header.Get("Content-Type"), len(b))
		}
		if res.Header.Get("Content-Security-Policy") != dashboard.CSP {
			t.Errorf("%s: CSP %q", path, res.Header.Get("Content-Security-Policy"))
		}
	}

	// Sign-in validates the key with GET /domains.
	if code, v := e.call("GET", "/domains", "", nil); code != 200 || v["object"] != "list" {
		t.Errorf("GET /domains: %d %v", code, v)
	}
	bad := &harness{t: t, st: e.st, url: e.ts.URL, apiKey: "omw_not-a-real-key"}
	if code, v := bad.call("GET", "/domains", "", nil); code != 401 || v["name"] != "invalid_api_key" {
		t.Errorf("GET /domains with a bad key: %d %v", code, v)
	}

	// Pages show "Not available on this server version" when an endpoint is
	// missing; they recognise the catch-all answer by this message.
	code, v := e.call("GET", "/no-such-endpoint", "", nil)
	if code != 404 || v["name"] != "not_found" || !strings.Contains(v["message"].(string), "no such endpoint") {
		t.Errorf("unknown endpoint: %d %v", code, v)
	}
}
