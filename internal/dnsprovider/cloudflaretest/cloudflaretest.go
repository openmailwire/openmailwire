// Package cloudflaretest is a fake of the parts of Cloudflare's API v4 that
// OpenMailWire uses, for tests: token verification, zones, DNS records,
// Email Routing and DNSSEC status. It never talks to Cloudflare.
//
// It also answers DNS lookups (LookupTXT, LookupMX) from its records, so a
// test's control plane can verify what was published through it.
//
// Behaviour a test can set:
//   - Token: the only token it accepts (401 for any other);
//   - TokenStatus: what /user/tokens/verify reports ("active" by default);
//   - ReadOnly: writes answer 403, like a token without Zone > DNS > Edit;
//   - per zone: Hidden (the token cannot see it), EmailRouting ("enabled",
//     "disabled" or "forbidden") and DNSSEC ("active", "disabled", … or
//     "forbidden");
//   - Inject: canned failures (status, Retry-After) for the next requests.
package cloudflaretest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/openmailwire/openmailwire/internal/dnsprovider"
)

// Zone is a zone of the fake account.
type Zone struct {
	ID     string
	Name   string
	Status string // "active" unless set
	// Hidden zones exist but the token cannot see them.
	Hidden bool
	// EmailRouting is "enabled", "disabled" (the default) or "forbidden"
	// (the token may not read it: 403).
	EmailRouting string
	// DNSSEC is the zone's DNSSEC status ("disabled" unless set), or
	// "forbidden".
	DNSSEC string
}

// Record is a DNS record in the fake.
type Record struct {
	ID       string
	ZoneID   string
	Type     string
	Name     string
	Content  string // as sent: TXT content may be quoted
	Priority int
	Proxied  bool
	Comment  string
}

// Fault is a canned answer for matching requests.
type Fault struct {
	Method     string // "" matches any
	Path       string // matches when the request path contains it; "" matches any
	Status     int
	RetryAfter string // Retry-After header, if any
	Code       int    // Cloudflare error code in the body, if any
	Times      int    // how many requests it answers (at least 1)
}

// Server is the fake API. Its URL ends in /client/v4, like the real one.
type Server struct {
	URL         string
	Token       string
	TokenStatus string
	ReadOnly    bool

	mu       sync.Mutex
	srv      *httptest.Server
	zones    []*Zone
	records  []*Record
	faults   []*Fault
	requests []string
	auths    map[string]int
	next     int
}

// New starts a fake that accepts token.
func New(token string) *Server {
	s := &Server{Token: token, TokenStatus: "active", auths: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL + "/client/v4"
	return s
}

func (s *Server) Close() { s.srv.Close() }

func canon(n string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".") }

// AddZone adds a zone and returns it; change it with Zone.
func (s *Server) AddZone(name string) Zone {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := sha256.Sum256([]byte(canon(name)))
	z := &Zone{ID: hex.EncodeToString(sum[:16]), Name: canon(name), Status: "active", EmailRouting: "disabled", DNSSEC: "disabled"}
	s.zones = append(s.zones, z)
	return *z
}

// SetZone changes a zone's settings.
func (s *Server) SetZone(name string, f func(*Zone)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, z := range s.zones {
		if z.Name == canon(name) {
			f(z)
		}
	}
}

func (s *Server) zoneOf(name string) *Zone {
	name = canon(name)
	var best *Zone
	for _, z := range s.zones {
		if name == z.Name || strings.HasSuffix(name, "."+z.Name) {
			if best == nil || len(z.Name) > len(best.Name) {
				best = z
			}
		}
	}
	return best
}

func (s *Server) newID() string {
	s.next++
	return fmt.Sprintf("%032x", s.next)
}

// AddRecord puts a record in the zone that holds r.Name, as if the zone's
// owner had made it, and returns it with its id.
func (s *Server) AddRecord(r Record) Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zoneOf(r.Name)
	if z == nil {
		panic("cloudflaretest: no zone holds " + r.Name)
	}
	r.ID, r.ZoneID, r.Name, r.Type = s.newID(), z.ID, canon(r.Name), strings.ToUpper(r.Type)
	s.records = append(s.records, &r)
	return r
}

// Records returns the records at name (all of them for name "").
func (s *Server) Records(name string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.records {
		if name == "" || r.Name == canon(name) {
			out = append(out, *r)
		}
	}
	return out
}

// TXT returns the texts of the TXT records at name, as resolvers see them.
func (s *Server) TXT(name string) []string {
	var out []string
	for _, r := range s.Records(name) {
		if r.Type == "TXT" {
			out = append(out, dnsprovider.NormalizeTXT(r.Content))
		}
	}
	return out
}

// Inject queues a canned failure.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Times < 1 {
		f.Times = 1
	}
	s.faults = append(s.faults, &f)
}

// ClearFaults drops the failures still queued.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// Requests lists every request seen, as "METHOD /path?query" without the
// /client/v4 prefix.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Count counts the requests whose "METHOD /path" starts with prefix.
func (s *Server) Count(prefix string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// Authorizations counts the Authorization headers seen, by value.
func (s *Server) Authorizations() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.auths {
		out[k] = v
	}
	return out
}

// LookupTXT answers from the fake's records, like a resolver.
func (s *Server) LookupTXT(_ context.Context, name string) ([]string, error) {
	if txt := s.TXT(name); len(txt) > 0 {
		return txt, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// LookupMX answers from the fake's records, like a resolver.
func (s *Server) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	var out []*net.MX
	for _, r := range s.Records(name) {
		if r.Type == "MX" {
			out = append(out, &net.MX{Host: canon(r.Content) + ".", Pref: uint16(r.Priority)})
		}
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return out, nil
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func reply(w http.ResponseWriter, status int, result any, info map[string]int, errs ...apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if errs == nil {
		errs = []apiError{}
	}
	body := map[string]any{"success": status < 300, "errors": errs, "messages": []any{}, "result": result}
	if info != nil {
		body["result_info"] = info
	}
	json.NewEncoder(w).Encode(body)
}

type recordJSON struct {
	ID       string `json:"id"`
	ZoneID   string `json:"zone_id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Priority *int   `json:"priority,omitempty"`
	Proxied  bool   `json:"proxied"`
	TTL      int    `json:"ttl"`
	Comment  string `json:"comment,omitempty"`
}

func toJSON(r *Record) recordJSON {
	out := recordJSON{ID: r.ID, ZoneID: r.ZoneID, Type: r.Type, Name: r.Name, Content: r.Content, Proxied: r.Proxied, TTL: 1, Comment: r.Comment}
	if r.Type == "MX" {
		p := r.Priority
		out.Priority = &p
	}
	return out
}

func zoneJSON(z *Zone) map[string]any {
	return map[string]any{"id": z.ID, "name": z.Name, "status": z.Status}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	line := r.Method + " " + path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, line)
	s.auths[r.Header.Get("Authorization")]++

	for i, f := range s.faults {
		if (f.Method == "" || f.Method == r.Method) && (f.Path == "" || strings.Contains(path, f.Path)) {
			f.Times--
			if f.Times <= 0 {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
			if f.RetryAfter != "" {
				w.Header().Set("Retry-After", f.RetryAfter)
			}
			reply(w, f.Status, nil, nil, apiError{f.Code, http.StatusText(f.Status)})
			return
		}
	}

	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		reply(w, http.StatusUnauthorized, nil, nil, apiError{1000, "Invalid API Token"})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == "GET" && path == "/user/tokens/verify":
		reply(w, 200, map[string]any{"id": "f00dcafe", "status": s.TokenStatus}, nil)
	case r.Method == "GET" && path == "/zones":
		name := canon(r.URL.Query().Get("name"))
		var out []any
		for _, z := range s.zones {
			if !z.Hidden && (name == "" || z.Name == name) {
				out = append(out, zoneJSON(z))
			}
		}
		if out == nil {
			out = []any{}
		}
		reply(w, 200, out, map[string]int{"page": 1, "per_page": 5, "count": len(out), "total_count": len(out), "total_pages": 1})
	case len(parts) >= 2 && parts[0] == "zones":
		z := s.zoneByID(parts[1])
		if z == nil {
			reply(w, http.StatusNotFound, nil, nil, apiError{7003, "Could not route to " + path + ", perhaps your object identifier is invalid?"})
			return
		}
		s.serveZone(w, r, z, parts[2:], body)
	default:
		reply(w, http.StatusNotFound, nil, nil, apiError{7000, "No route for that URI"})
	}
}

func (s *Server) zoneByID(id string) *Zone {
	for _, z := range s.zones {
		if z.ID == id && !z.Hidden {
			return z
		}
	}
	return nil
}

func (s *Server) serveZone(w http.ResponseWriter, r *http.Request, z *Zone, rest []string, body []byte) {
	forbidden := apiError{10000, "Authentication error"}
	switch {
	case len(rest) == 0 && r.Method == "GET":
		reply(w, 200, zoneJSON(z), nil)
	case len(rest) == 2 && rest[0] == "email" && rest[1] == "routing" && r.Method == "GET":
		if z.EmailRouting == "forbidden" {
			reply(w, http.StatusForbidden, nil, nil, forbidden)
			return
		}
		on := z.EmailRouting == "enabled"
		status := "disabled"
		if on {
			status = "ready"
		}
		reply(w, 200, map[string]any{"enabled": on, "status": status, "name": z.Name, "tag": z.ID}, nil)
	case len(rest) == 1 && rest[0] == "dnssec" && r.Method == "GET":
		if z.DNSSEC == "forbidden" {
			reply(w, http.StatusForbidden, nil, nil, forbidden)
			return
		}
		reply(w, 200, map[string]any{"status": z.DNSSEC}, nil)
	case len(rest) >= 1 && rest[0] == "dns_records":
		s.serveRecords(w, r, z, rest[1:], body)
	default:
		reply(w, http.StatusNotFound, nil, nil, apiError{7000, "No route for that URI"})
	}
}

func (s *Server) serveRecords(w http.ResponseWriter, r *http.Request, z *Zone, rest []string, body []byte) {
	if r.Method != "GET" && s.ReadOnly {
		reply(w, http.StatusForbidden, nil, nil, apiError{10000, "Authentication error"})
		return
	}
	switch {
	case len(rest) == 0 && r.Method == "GET":
		q := r.URL.Query()
		name := canon(q.Get("name"))
		var match []*Record
		for _, rec := range s.records {
			if rec.ZoneID == z.ID && (name == "" || rec.Name == name) {
				match = append(match, rec)
			}
		}
		sort.SliceStable(match, func(i, j int) bool { return match[i].ID < match[j].ID })
		per, _ := strconv.Atoi(q.Get("per_page"))
		if per < 1 {
			per = 100
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if page < 1 {
			page = 1
		}
		pages := max(1, (len(match)+per-1)/per)
		out := []recordJSON{}
		for i := (page - 1) * per; i < len(match) && i < page*per; i++ {
			out = append(out, toJSON(match[i]))
		}
		reply(w, 200, out, map[string]int{"page": page, "per_page": per, "count": len(out), "total_count": len(match), "total_pages": pages})
	case len(rest) == 0 && r.Method == "POST":
		var in recordJSON
		if err := json.Unmarshal(body, &in); err != nil {
			reply(w, 400, nil, nil, apiError{9207, "Request body is invalid."})
			return
		}
		rec := &Record{ZoneID: z.ID, Type: strings.ToUpper(in.Type), Name: canon(in.Name), Content: in.Content, Proxied: in.Proxied, Comment: in.Comment}
		if in.Priority != nil {
			rec.Priority = *in.Priority
		}
		if e := s.check(z, rec, ""); e != nil {
			reply(w, 400, nil, nil, *e)
			return
		}
		rec.ID = s.newID()
		s.records = append(s.records, rec)
		reply(w, 200, toJSON(rec), nil)
	case len(rest) == 1 && (r.Method == "PATCH" || r.Method == "PUT"):
		rec := s.recordByID(z, rest[0])
		if rec == nil {
			reply(w, http.StatusNotFound, nil, nil, apiError{81044, "Record does not exist."})
			return
		}
		var in recordJSON
		if err := json.Unmarshal(body, &in); err != nil {
			reply(w, 400, nil, nil, apiError{9207, "Request body is invalid."})
			return
		}
		next := *rec
		if in.Content != "" {
			next.Content = in.Content
		}
		if in.Priority != nil {
			next.Priority = *in.Priority
		}
		if in.Type != "" {
			next.Type = strings.ToUpper(in.Type)
		}
		if in.Name != "" {
			next.Name = canon(in.Name)
		}
		next.Proxied, next.Comment = in.Proxied, in.Comment
		if e := s.check(z, &next, rec.ID); e != nil {
			reply(w, 400, nil, nil, *e)
			return
		}
		*rec = next
		reply(w, 200, toJSON(rec), nil)
	case len(rest) == 1 && r.Method == "DELETE":
		for i, rec := range s.records {
			if rec.ZoneID == z.ID && rec.ID == rest[0] {
				s.records = append(s.records[:i], s.records[i+1:]...)
				reply(w, 200, map[string]string{"id": rest[0]}, nil)
				return
			}
		}
		reply(w, http.StatusNotFound, nil, nil, apiError{81044, "Record does not exist."})
	default:
		reply(w, http.StatusNotFound, nil, nil, apiError{7000, "No route for that URI"})
	}
}

func (s *Server) recordByID(z *Zone, id string) *Record {
	for _, rec := range s.records {
		if rec.ZoneID == z.ID && rec.ID == id {
			return rec
		}
	}
	return nil
}

// check applies the rules of Cloudflare's that OpenMailWire can run into.
func (s *Server) check(z *Zone, rec *Record, self string) *apiError {
	switch {
	case rec.Name != z.Name && !strings.HasSuffix(rec.Name, "."+z.Name):
		return &apiError{9005, "Content for " + rec.Type + " record is invalid: the name is not in this zone"}
	case rec.Content == "":
		return &apiError{9005, "Content for " + rec.Type + " record is invalid."}
	case rec.Proxied && (rec.Type == "TXT" || rec.Type == "MX"):
		return &apiError{9004, "This record type cannot be proxied."}
	case rec.Type == "MX" && rec.Priority < 0:
		return &apiError{9100, "MX records need a priority."}
	}
	for _, o := range s.records {
		if o.ID == self || o.ZoneID != z.ID || o.Name != rec.Name {
			continue
		}
		if (o.Type == "CNAME") != (rec.Type == "CNAME") {
			return &apiError{81053, "An A, AAAA, or CNAME record with that host already exists."}
		}
		if o.Type == rec.Type && dnsprovider.NormalizeTXT(o.Content) == dnsprovider.NormalizeTXT(rec.Content) && o.Priority == rec.Priority {
			return &apiError{81058, "An identical record already exists."}
		}
	}
	return nil
}
