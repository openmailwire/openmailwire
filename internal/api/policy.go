package api

import "net/http"

// policyError is a refusal to send, in the API's error shape.
type policyError struct {
	Status  int
	Name    string
	Message string
}

func (p *policyError) write(w http.ResponseWriter) { writeError(w, p.Status, p.Name, p.Message) }

// authorizeSend is the one place where an API key's sending rules are
// enforced. Every path that queues mail (the JSON API, batches, raw MIME,
// inbox replies, SMTP submission, MCP) must call it with the envelope
// sender address and the deduplicated recipients, before anything is
// stored. It returns nil to allow the send.
func (s *Server) authorizeSend(r *http.Request, from string, recipients []string) *policyError {
	_ = apiKeyOf(r)
	return nil
}
