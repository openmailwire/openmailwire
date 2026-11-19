package api

import (
	"net/http"
	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openmailwire/openmailwire/internal/mcpserver"
)

// inProcess sends a request straight into a handler, so remote MCP tools
// call the same API code an HTTP client would, without a network hop.
type inProcess struct{ h func() http.Handler }

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		// A server request always has a body; handlers that read one
		// would panic on nil (lane B: POST /emails/{id}/approve).
		req = req.Clone(req.Context())
		req.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	t.h().ServeHTTP(rec, req)
	return rec.Result(), nil
}

// mcpHandler serves the remote MCP endpoint. The caller authenticates with
// its API key, and every tool acts with that same key.
func (s *Server) mcpHandler(root func() http.Handler) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		k := apiKeyOf(r)
		return mcpserver.New(&mcpserver.APIClient{
			Base: "http://openmailwire.internal",
			Key:  bearer(r),
			HTTP: &http.Client{Transport: inProcess{root}},
			// Lane B: offer approve/reject and add_suppression only to keys that may use them.
			Access: &mcpserver.KeyAccess{FullAccess: k.FullAccess(), CanApprove: canApprove(k)},
		}, s.Version)
	}, &mcp.StreamableHTTPOptions{Stateless: true, Logger: s.Log})
	return s.withProject(h.ServeHTTP)
}
