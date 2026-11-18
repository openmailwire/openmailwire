// Package apidocs embeds the API's machine-readable documentation and
// serves it: an OpenAPI 3.1 description (openapi.json) and a guide for
// language models (llms.txt).
//
// The files in this directory are the originals. docs/openapi.json and
// docs/llms.txt are copies for people browsing the repository, and a test
// fails when they differ.
package apidocs

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"time"
)

//go:embed openapi.json
var openapiJSON []byte

//go:embed llms.txt
var llmsTxt []byte

type document struct {
	body        []byte
	contentType string
	etag        string
}

func newDocument(body []byte, contentType string) document {
	sum := sha256.Sum256(body)
	return document{body: body, contentType: contentType, etag: `"` + hex.EncodeToString(sum[:12]) + `"`}
}

var documents = map[string]document{
	"/openapi.json": newDocument(openapiJSON, "application/json"),
	"/llms.txt":     newDocument(llmsTxt, "text/plain; charset=utf-8"),
}

// Handler serves GET /openapi.json and GET /llms.txt. Both are public: they
// need no API key, and browsers and tools on any origin may fetch them.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc, ok := documents[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", doc.contentType)
		h.Set("Cache-Control", "public, max-age=300")
		h.Set("ETag", doc.etag)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Access-Control-Allow-Origin", "*")
		// ServeContent answers HEAD, Range and If-None-Match (304).
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(doc.body))
	})
}
