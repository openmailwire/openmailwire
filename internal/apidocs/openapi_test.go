package apidocs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/openmailwire/openmailwire/internal/webhook"
)

// Who may call a route.
const (
	public  = "public"  // no API key
	sending = "sending" // any API key, including sending_access
	full    = "full"    // full_access keys only
)

type route struct{ method, path, access string }

// publicRoutes is every public endpoint: the ones the server has today and
// the ones in docs/design/roadmap-contracts.md. openapi.json must describe
// exactly these, and the access column follows the contract's "who can do
// what" list. Internal worker endpoints (/internal/*) and /mcp are not REST
// operations of the public API, so they are not here.
var publicRoutes = []route{
	{"GET", "/health", public},
	{"GET", "/openapi.json", public},
	{"GET", "/llms.txt", public},

	{"POST", "/emails", sending},
	{"GET", "/emails", sending},
	{"POST", "/emails/batch", sending},
	{"POST", "/emails/raw", sending},
	{"GET", "/emails/{id}", sending},
	{"POST", "/emails/{id}/cancel", sending},

	{"POST", "/domains", full},
	{"GET", "/domains", sending},
	{"GET", "/domains/{id}", sending},
	{"POST", "/domains/{id}/verify", full},
	{"DELETE", "/domains/{id}", full},
	{"GET", "/domains/{id}/dkim", sending},
	{"POST", "/domains/{id}/dkim/rotate", full},
	{"POST", "/domains/{id}/dkim/ed25519", full},

	{"POST", "/webhooks", full},
	{"GET", "/webhooks", full},
	{"GET", "/webhooks/{id}", full},
	{"DELETE", "/webhooks/{id}", full},

	{"GET", "/api-keys", full},
	{"POST", "/api-keys", full},
	{"GET", "/api-keys/{id}", full},
	{"PATCH", "/api-keys/{id}", full},
	{"DELETE", "/api-keys/{id}", full},

	{"POST", "/inboxes", full},
	{"GET", "/inboxes", sending},
	{"DELETE", "/inboxes/{id}", full},
	{"GET", "/inboxes/{id}", sending},
	{"GET", "/emails/{id}/replies", sending},
	{"GET", "/inboxes/{id}/messages", sending},
	{"GET", "/messages/{id}", sending},
	{"GET", "/messages/{id}/attachments/{index}", sending},
	{"POST", "/messages/{id}/reply", sending},
}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(openapiJSON, &spec); err != nil {
		t.Fatalf("openapi.json does not parse: %v", err)
	}
	return spec
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// operations returns every operation in a paths or webhooks object, keyed
// "METHOD path".
func operations(items map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for path, item := range items {
		for _, m := range httpMethods {
			if op := asMap(asMap(item)[m]); op != nil {
				out[strings.ToUpper(m)+" "+path] = op
			}
		}
	}
	return out
}

// resolve follows a local "$ref" ("#/components/...") to its target.
func resolve(spec map[string]any, v any) (any, error) {
	for i := 0; i < 10; i++ {
		ref, ok := asMap(v)["$ref"].(string)
		if !ok {
			return v, nil
		}
		if !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("%s: only local references are allowed", ref)
		}
		var cur any = spec
		for _, part := range strings.Split(ref[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			next, ok := asMap(cur)[part]
			if !ok {
				return nil, fmt.Errorf("%s does not resolve", ref)
			}
			cur = next
		}
		v = cur
	}
	return nil, fmt.Errorf("reference chain too long")
}

func TestOpenAPIDocument(t *testing.T) {
	spec := loadSpec(t)
	if v, _ := spec["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Errorf("openapi = %q, want 3.1.x", v)
	}
	info := asMap(spec["info"])
	for _, f := range []string{"title", "version", "description"} {
		if s, _ := info[f].(string); s == "" {
			t.Errorf("info.%s is empty", f)
		}
	}
	desc, _ := info["description"].(string)
	for _, want := range []string{"/mcp", "openmailwire mcp", "/llms.txt", "full_access", "sending_access", "Idempotency-Key"} {
		if !strings.Contains(desc, want) {
			t.Errorf("info.description does not mention %s", want)
		}
	}
	scheme := asMap(asMap(asMap(spec["components"])["securitySchemes"])["apiKey"])
	if scheme["type"] != "http" || scheme["scheme"] != "bearer" {
		t.Errorf("components.securitySchemes.apiKey = %v, want HTTP bearer", scheme)
	}
}

func TestOpenAPIRoutes(t *testing.T) {
	spec := loadSpec(t)
	paths := asMap(spec["paths"])
	ops := operations(paths)

	for _, r := range publicRoutes {
		key := r.method + " " + r.path
		op, ok := ops[key]
		if !ok {
			t.Errorf("%s is not documented", key)
			continue
		}
		var roles []string
		security, _ := op["security"].([]any)
		switch {
		case security == nil:
			t.Errorf("%s: set security explicitly", key)
			continue
		case len(security) > 1:
			t.Errorf("%s: want one security requirement, got %d", key, len(security))
			continue
		case len(security) == 1:
			apiKey, ok := asMap(security[0])["apiKey"].([]any)
			if !ok {
				t.Errorf("%s: security must use the apiKey scheme", key)
				continue
			}
			for _, role := range apiKey {
				roles = append(roles, fmt.Sprint(role))
			}
		}
		access := sending
		switch {
		case len(security) == 0:
			access = public
		case slices.Equal(roles, []string{"full_access"}):
			access = full
		case len(roles) != 0:
			t.Errorf("%s: unknown roles %v", key, roles)
		}
		if access != r.access {
			t.Errorf("%s: documented as %s, the contract says %s", key, access, r.access)
		}
		responses := asMap(op["responses"])
		if access != public && responses["401"] == nil {
			t.Errorf("%s: no 401 response", key)
		}
		if access == full && responses["403"] == nil {
			t.Errorf("%s: needs full_access but has no 403 response", key)
		}
		opDesc, _ := op["description"].(string)
		if access == full && !strings.Contains(opDesc, "full_access") {
			t.Errorf("%s: the description should say it needs a full_access key", key)
		}
	}

	// Nothing undocumented in the list, and nothing in the document that is
	// missing from the list.
	listed := map[string]bool{}
	for _, r := range publicRoutes {
		listed[r.method+" "+r.path] = true
	}
	for key := range ops {
		if !listed[key] {
			t.Errorf("%s is documented but missing from publicRoutes in this test", key)
		}
	}
	for path := range paths {
		if strings.HasPrefix(path, "/internal") || path == "/mcp" {
			t.Errorf("%s must not be in the public API document", path)
		}
	}
}

func TestOpenAPIOperationsHaveIDAndSummary(t *testing.T) {
	spec := loadSpec(t)
	ids := map[string]string{}
	all := operations(asMap(spec["paths"]))
	for key, op := range operations(asMap(spec["webhooks"])) {
		all["webhook "+key] = op
	}
	for key, op := range all {
		id, _ := op["operationId"].(string)
		if id == "" {
			t.Errorf("%s has no operationId", key)
		} else if other, dup := ids[id]; dup {
			t.Errorf("operationId %s is used by %s and %s", id, key, other)
		}
		ids[id] = key
		if s, _ := op["summary"].(string); s == "" {
			t.Errorf("%s has no summary", key)
		}
	}
}

// Every route the server registers outside /internal must be documented.
// This catches a lane that adds an endpoint without documenting it.
func TestOpenAPICoversServerRoutes(t *testing.T) {
	spec := loadSpec(t)
	ops := operations(asMap(spec["paths"]))
	files, err := filepath.Glob("../api/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no files in ../api: %v", err)
	}
	pattern := regexp.MustCompile(`mux\.Handle(?:Func)?\(\s*"([A-Z]+) (/[^"]*)"`)
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pattern.FindAllStringSubmatch(string(src), -1) {
			method, path := m[1], m[2]
			if strings.HasPrefix(path, "/internal/") || strings.HasPrefix(path, "/dashboard") {
				continue
			}
			found++
			if ops[method+" "+path] == nil {
				t.Errorf("%s registers %s %s, but openapi.json does not document it", filepath.Base(f), method, path)
			}
		}
	}
	if found < 10 {
		t.Errorf("found only %d routes in ../api; has the registration style changed?", found)
	}
}

func TestOpenAPIReferencesResolve(t *testing.T) {
	spec := loadSpec(t)
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch v := v.(type) {
		case map[string]any:
			if _, ok := v["$ref"]; ok {
				if _, err := resolve(spec, v); err != nil {
					t.Errorf("%s: %v", at, err)
				}
			}
			for k, c := range v {
				walk(c, at+"/"+k)
			}
		case []any:
			for i, c := range v {
				walk(c, at+"/"+strconv.Itoa(i))
			}
		}
	}
	walk(spec, "#")
}

func TestOpenAPIWebhooks(t *testing.T) {
	spec := loadSpec(t)
	hooks := asMap(spec["webhooks"])
	schemas := asMap(asMap(spec["components"])["schemas"])
	var enum []string
	for _, v := range asMap(schemas["WebhookEventType"])["enum"].([]any) {
		enum = append(enum, v.(string))
	}
	for _, typ := range webhook.Types {
		if hooks[typ] == nil {
			t.Errorf("webhook event %s has no entry in the webhooks section", typ)
		}
		if !slices.Contains(enum, typ) {
			t.Errorf("webhook event %s is missing from the WebhookEventType enum", typ)
		}
	}
	for _, typ := range enum {
		if hooks[typ] == nil {
			t.Errorf("WebhookEventType %s has no entry in the webhooks section", typ)
		}
	}
	for typ, op := range operations(hooks) {
		var headers []string
		for _, p := range op["parameters"].([]any) {
			p, err := resolve(spec, p)
			if err != nil {
				t.Fatal(err)
			}
			if asMap(p)["in"] == "header" {
				headers = append(headers, asMap(p)["name"].(string))
			}
		}
		for _, h := range []string{"svix-id", "svix-timestamp", "svix-signature", "webhook-id", "webhook-timestamp", "webhook-signature"} {
			if !slices.Contains(headers, h) {
				t.Errorf("webhook %s does not document the %s header", typ, h)
			}
		}
		if asMap(op["requestBody"]) == nil {
			t.Errorf("webhook %s has no payload schema", typ)
		}
	}
}

// The error names an agent must be able to branch on, with their statuses.
var requiredErrors = map[string]int{
	"validation_error":        422,
	"domain_not_found":        403,
	"domain_not_verified":     403,
	"idempotency_conflict":    409,
	"insufficient_permission": 403,
	"sender_not_allowed":      403,
	"recipient_not_allowed":   403,
	"rate_limit_exceeded":     429,
	"cannot_cancel":           409,
	"not_found":               404,
	"missing_api_key":         401,
	"invalid_api_key":         401,
}

func TestOpenAPIErrors(t *testing.T) {
	spec := loadSpec(t)
	seen := map[string]int{}
	for key, op := range operations(asMap(spec["paths"])) {
		for status, resp := range asMap(op["responses"]) {
			resp, err := resolve(spec, resp)
			if err != nil {
				t.Fatal(err)
			}
			code, _ := strconv.Atoi(status)
			if code == http.StatusTooManyRequests && asMap(asMap(resp)["headers"])["Retry-After"] == nil {
				t.Errorf("%s: the 429 response does not document Retry-After", key)
			}
			media := asMap(asMap(asMap(resp)["content"])["application/json"])
			for name, ex := range asMap(media["examples"]) {
				v := asMap(asMap(ex)["value"])
				errName, ok := v["name"].(string)
				if !ok || v["statusCode"] == nil {
					continue
				}
				if got := int(v["statusCode"].(float64)); got != code {
					t.Errorf("%s %s: example %s has statusCode %d", key, status, name, got)
				}
				seen[errName] = code
			}
		}
	}
	for name, status := range requiredErrors {
		got, ok := seen[name]
		switch {
		case !ok:
			t.Errorf("no operation documents the error %s", name)
		case got != status:
			t.Errorf("error %s is documented with status %d, want %d", name, got, status)
		}
	}
	errSchema := asMap(asMap(asMap(spec["components"])["schemas"])["Error"])
	var required []string
	for _, r := range errSchema["required"].([]any) {
		required = append(required, r.(string))
	}
	slices.Sort(required)
	if !slices.Equal(required, []string{"message", "name", "statusCode"}) {
		t.Errorf("Error requires %v, want statusCode, name and message", required)
	}
}
