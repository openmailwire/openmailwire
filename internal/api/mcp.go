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
	rec := httptest.NewRecorder()
	t.h().ServeHTTP(rec, req)
	return rec.Result(), nil
}

// mcpHandler serves the remote MCP endpoint. The caller authenticates with
// its API key, and every tool acts with that same key.
func (s *Server) mcpHandler(root func() http.Handler) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return mcpserver.New(&mcpserver.APIClient{
			Base: "http://openmailwire.internal",
			Key:  bearer(r),
			HTTP: &http.Client{Transport: inProcess{root}},
		}, s.Version)
	}, &mcp.StreamableHTTPOptions{Stateless: true, Logger: s.Log})
	return s.withProject(h.ServeHTTP)
}
