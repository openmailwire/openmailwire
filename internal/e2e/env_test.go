package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/openmailwire/openmailwire/internal/api"
	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/smtptest"
	"github.com/openmailwire/openmailwire/internal/store"
	"github.com/openmailwire/openmailwire/internal/webhook"
	"github.com/openmailwire/openmailwire/internal/worker"
)

// env is a ready control plane for one test:
//   - the database is emptied first;
//   - one project with a full-access API key;
//   - one registered worker (not running until startWorker);
//   - a scripted SMTP receiver;
//   - fake DNS.
//
// New end-to-end tests should start with newEnv rather than build their own
// setup. Tests in this package share one database, so they must not call
// t.Parallel.
type env struct {
	*harness
	t           *testing.T
	ctx         context.Context
	st          *store.Store
	srv         *api.Server
	ts          *httptest.Server
	log         *slog.Logger
	sealer      *secret.Sealer
	secretKey   []byte
	dns         *dns
	projectID   string
	workerID    string
	workerToken string
	smtp        *smtptest.Server
}

func newEnv(t *testing.T, tweak func(*config.Control)) *env {
	t.Helper()
	dbURL := os.Getenv("OMW_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("OMW_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `TRUNCATE projects, workers CASCADE`); err != nil {
		t.Fatal(err)
	}
	pid, err := st.CreateProject(ctx, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	apiKey, keyHash := secret.NewToken("omw_")
	if err := st.CreateAPIKey(ctx, pid, "e2e", apiKey[:10], keyHash); err != nil {
		t.Fatal(err)
	}
	workerToken, workerHash := secret.NewToken("omwk_")
	workerID, err := st.CreateWorker(ctx, "w1", workerHash)
	if err != nil {
		t.Fatal(err)
	}

	key := []byte(strings.Repeat("k", 32))
	sealer, _ := secret.NewSealer(key)
	cfg := config.Control{
		ReturnPathLabel: "bounce", SPFInclude: "_spf.openmailwire.test",
		DefaultExpiry: time.Hour, LeaseDuration: time.Minute, DomainConcurrency: 5, Retention: time.Hour,
		WebhookAllowHTTP: true, WebhookAllowPrivate: true,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &dns{txt: map[string][]string{}}
	srv := &api.Server{
		Store: st, Sealer: sealer, MACKey: secret.Derive(key, "openmailwire/verp"),
		Resolver: d, Cfg: cfg, Log: log, Version: "test",
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	smtp, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(smtp.Close)

	return &env{
		harness: &harness{t: t, st: st, url: ts.URL, apiKey: apiKey},
		t:       t, ctx: ctx, st: st, srv: srv, ts: ts, log: log, sealer: sealer, secretKey: key, dns: d,
		projectID: pid, workerID: workerID, workerToken: workerToken, smtp: smtp,
	}
}

// addVerifiedDomain adds name, publishes the records the API asks for in
// the fake DNS, and verifies it. It returns the domain id and the DKIM TXT
// value (for verifyDKIM).
func (e *env) addVerifiedDomain(name string) (id, dkimTXT string) {
	e.t.Helper()
	code, dom := e.call("POST", "/domains", "", map[string]string{"name": name})
	if code != 201 {
		e.t.Fatalf("add domain %s: %d %v", name, code, dom)
	}
	for _, r := range dom["records"].([]any) {
		rec := r.(map[string]any)
		n := rec["name"].(string)
		if rec["type"] == "TXT" {
			e.dns.txt[n] = append(e.dns.txt[n], rec["value"].(string))
		}
		if rec["record"] == "DKIM" {
			dkimTXT = rec["value"].(string)
		}
	}
	id = dom["id"].(string)
	if code, v := e.call("POST", "/domains/"+id+"/verify", "", nil); code != 200 || v["status"] != "verified" {
		e.t.Fatalf("verify domain %s: %d %v", name, code, v)
	}
	return id, dkimTXT
}

// startWorker runs a delivery worker that sends everything to e.smtp. It
// stops when the test ends or when stop is called.
func (e *env) startWorker() (stop func()) {
	e.t.Helper()
	r := worker.NewRunner(config.Worker{
		ControlURL: e.ts.URL, Token: e.workerToken, EHLO: "w1.openmailwire.test", PublicIP: "203.0.113.7",
		SMTPOverride: e.smtp.Addr, StateDir: e.t.TempDir(), Concurrency: 4,
		PollInterval: 50 * time.Millisecond, JobTimeout: 30 * time.Second,
	}, e.log, "test")
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	e.t.Cleanup(stop)
	return stop
}

// startWebhooks runs the webhook dispatcher until the test ends.
func (e *env) startWebhooks() {
	d := &webhook.Dispatcher{Store: e.st, Sealer: e.sealer, HTTP: webhook.NewHTTPClient(true), Log: e.log, UserAgent: "test"}
	go d.Run(e.ctx, 50*time.Millisecond)
}

// otherProject creates a second, separate project and returns a harness
// that calls the API with its key.
func (e *env) otherProject() *harness {
	e.t.Helper()
	pid, err := e.st.CreateProject(e.ctx, "other")
	if err != nil {
		e.t.Fatal(err)
	}
	k, h := secret.NewToken("omw_")
	if err := e.st.CreateAPIKey(e.ctx, pid, "other", k[:10], h); err != nil {
		e.t.Fatal(err)
	}
	return &harness{t: e.t, st: e.st, url: e.ts.URL, apiKey: k}
}

// verifyDKIM fails the test unless msg carries a valid DKIM signature for
// the key published as dkimTXT.
func verifyDKIM(t *testing.T, msg []byte, dkimTXT string) {
	t.Helper()
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(msg), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			if strings.Contains(domain, "._domainkey.") {
				return []string{dkimTXT}, nil
			}
			return nil, errors.New("not found")
		},
	})
	if err != nil || len(vs) == 0 || vs[0].Err != nil {
		t.Fatalf("dkim: %v %+v", err, vs)
	}
}

// TestEnvSmoke keeps the shared setup honest.
func TestEnvSmoke(t *testing.T) {
	e := newEnv(t, nil)
	_, dkimTXT := e.addVerifiedDomain("smoke.test")
	e.startWorker()
	code, b := e.call("POST", "/emails", "", map[string]any{"from": "a@smoke.test", "to": "x@example.test", "subject": "s", "text": "t"})
	if code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "x@example.test", "delivered")
	msgs := e.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	verifyDKIM(t, msgs[0].Data, dkimTXT)
}
