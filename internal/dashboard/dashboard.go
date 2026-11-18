// Package dashboard serves the web dashboard: static files embedded in the
// binary, under /dashboard/. The page signs in with an API key and calls the
// same-origin HTTP API, so it holds a credential. Every response therefore
// carries a strict Content-Security-Policy, and the page never inserts API
// data as markup.
package dashboard

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// Prefix is where the dashboard is mounted.
const Prefix = "/dashboard"

// CSP allows only the dashboard's own files and same-origin API calls: no
// inline script or style, no framing, no form posts.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

//go:embed static
var static embed.FS

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
}

type asset struct {
	body  []byte
	ctype string
	etag  string
}

var assets = load()

func load() map[string]asset {
	out := map[string]asset{}
	err := fs.WalkDir(static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ctype, ok := contentTypes[path.Ext(p)]
		if !ok {
			return nil // only known types are served
		}
		body, err := static.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		out[strings.TrimPrefix(p, "static/")] = asset{body: body, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	if err != nil {
		panic("dashboard: " + err.Error())
	}
	return out
}

// Handler serves /dashboard (a redirect) and everything under /dashboard/.
func Handler() http.Handler { return http.HandlerFunc(serve) }

func serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == Prefix {
		http.Redirect(w, r, Prefix+"/", http.StatusMovedPermanently)
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, Prefix+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if name == "" {
		name = "index.html"
	}
	a, ok := assets[name]
	if !ok {
		h.Set("Cache-Control", "no-store")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.Set("Content-Type", a.ctype)
	if a.ctype == contentTypes[".html"] {
		// The page is the entry point that holds the key; never cache it.
		h.Set("Cache-Control", "no-store")
	} else {
		// Files have no hashed names, so browsers revalidate them (cheap,
		// thanks to the ETag) and always run the binary's version.
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", a.etag)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.body))
}
