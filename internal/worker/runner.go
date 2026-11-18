// Package worker pulls deliveries from the control plane, sends them over
// SMTP from this machine's IP and reports each outcome.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/protocol"
)

// deliverer sends one job: a Deliverer (direct to the recipient's MX) or a
// Relay (through a smarthost).
type deliverer interface {
	Deliver(ctx context.Context, j protocol.Job) Result
}

type Runner struct {
	cfg     config.Worker
	client  *Client
	d       deliverer
	kind    string // "" for a direct worker (the wire default), protocol.KindRelay for a relay
	log     *slog.Logger
	version string
	outbox  string
}

func NewRunner(cfg config.Worker, log *slog.Logger, version string) *Runner {
	r := &Runner{
		cfg:     cfg,
		client:  &Client{Base: cfg.ControlURL, Token: cfg.Token, UserAgent: "openmailwire-worker/" + version, HTTP: &http.Client{Timeout: 60 * time.Second}},
		log:     log,
		version: version,
		outbox:  filepath.Join(cfg.StateDir, "outbox"),
	}
	if cfg.Relay() {
		r.kind = protocol.KindRelay
		relay := &Relay{
			Host: cfg.RelayHost, Port: cfg.RelayPort, Username: cfg.RelayUsername, Password: cfg.RelayPassword,
			ImplicitTLS: cfg.RelayTLS == "implicit", EHLO: cfg.EHLO, SourceIP: cfg.SourceIP, RootCAs: cfg.TLSRoots,
		}
		relay.setDefaults() // before any concurrent Deliver
		r.d = relay
		return r
	}
	d := &Deliverer{
		EHLO:     cfg.EHLO,
		SourceIP: cfg.SourceIP,
		Override: cfg.SMTPOverride,
		IPv4Only: cfg.IPv4Only,
		Resolver: net.DefaultResolver,
		RootCAs:  cfg.TLSRoots,
		STS:      &STSFetcher{Resolver: net.DefaultResolver, RootCAs: cfg.TLSRoots},
	}
	if cfg.DNSSECResolver != "" {
		d.DANE = &DANEResolver{Server: cfg.DNSSECResolver}
	}
	d.setDefaults()
	r.d = d
	return r
}

// Run leases and delivers until ctx is cancelled, then finishes the
// deliveries already in flight before returning.
func (r *Runner) Run(ctx context.Context) error {
	if err := os.MkdirAll(r.outbox, 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	if r.cfg.SMTPOverride != "" {
		r.log.Warn("every message goes to the SMTP override, not to real recipients", "override", r.cfg.SMTPOverride)
	}
	if r.cfg.Relay() {
		r.log.Info("relay worker: deliveries routed to relays go through the smarthost",
			"relay", net.JoinHostPort(r.cfg.RelayHost, strconv.Itoa(r.cfg.RelayPort)), "tls", r.cfg.RelayTLS)
	}
	r.flushOutbox(ctx)
	// The first heartbeat goes out before any lease, so the control plane
	// knows this worker's kind when it hands out jobs.
	r.beat(ctx)
	go r.heartbeats(ctx)
	go r.outboxLoop(ctx)

	sem := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		if ctx.Err() != nil {
			r.log.Info("stopping; finishing deliveries in flight", "in_flight", len(sem))
			return nil
		}
		free := cap(sem) - len(sem)
		if free == 0 {
			sleep(ctx, 200*time.Millisecond)
			continue
		}
		var resp protocol.LeaseResponse
		if err := r.client.Post(ctx, "/internal/worker/lease", protocol.LeaseRequest{Max: free, Kind: r.kind}, &resp); err != nil {
			if ctx.Err() == nil {
				r.log.Warn("lease failed", "err", err)
			}
			sleep(ctx, 2*r.cfg.PollInterval)
			continue
		}
		if len(resp.Jobs) == 0 {
			sleep(ctx, r.cfg.PollInterval)
			continue
		}
		for _, j := range resp.Jobs {
			sem <- struct{}{}
			wg.Add(1)
			go func(j protocol.Job) {
				defer func() { <-sem; wg.Done() }()
				r.handle(ctx, j)
			}(j)
		}
	}
}

func (r *Runner) handle(ctx context.Context, j protocol.Job) {
	// A shutdown signal does not abort a delivery mid-conversation; the job
	// timeout (shorter than the lease) bounds it instead.
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.JobTimeout)
	defer cancel()
	start := time.Now()
	res := r.d.Deliver(jctx, j)
	attrs := []any{"attempt", j.AttemptID, "rcpt_domain", j.RcptDomain, "outcome", res.Outcome,
		"code", res.Code, "enhanced", res.Enhanced, "mx", res.MXHost, "tls", res.TLS,
		"ms", time.Since(start).Milliseconds()}
	if res.TLSPolicy != "" {
		attrs = append(attrs, "tls_policy", res.TLSPolicy)
	}
	r.log.Info("attempt", attrs...)
	r.submit(context.WithoutCancel(ctx), protocol.Report{
		AttemptID: j.AttemptID, Outcome: res.Outcome, SMTPCode: res.Code, EnhancedCode: res.Enhanced,
		Response: res.Response, MXHost: res.MXHost, SourceIP: r.cfg.PublicIP, TLS: res.TLS, TLSPolicy: res.TLSPolicy,
	})
}

var attemptIDRE = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// submit writes the report to the outbox before sending it, so a crash or a
// control-plane outage never loses an outcome. Reports are idempotent.
func (r *Runner) submit(ctx context.Context, rep protocol.Report) {
	if !attemptIDRE.MatchString(rep.AttemptID) {
		r.log.Error("refusing report with a malformed attempt id", "attempt", rep.AttemptID)
		return
	}
	path := filepath.Join(r.outbox, rep.AttemptID+".json")
	data, _ := json.Marshal(rep)
	if err := writeFileSync(path, data); err != nil {
		r.log.Error("outbox write failed", "err", err)
	}
	r.send(ctx, path, rep)
}

func (r *Runner) send(ctx context.Context, path string, rep protocol.Report) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := r.client.Post(cctx, "/internal/worker/report", rep, nil)
	var he *HTTPError
	switch {
	case err == nil:
		os.Remove(path)
	case errors.As(err, &he) && he.Permanent():
		r.log.Error("report rejected; dropping it", "attempt", rep.AttemptID, "err", err)
		os.Remove(path)
	default:
		r.log.Warn("report kept in the outbox for retry", "attempt", rep.AttemptID, "err", err)
	}
}

func (r *Runner) flushOutbox(ctx context.Context) {
	entries, err := os.ReadDir(r.outbox)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(r.outbox, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rep protocol.Report
		if json.Unmarshal(data, &rep) != nil {
			os.Remove(path)
			continue
		}
		r.send(ctx, path, rep)
	}
}

func (r *Runner) outboxLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flushOutbox(context.WithoutCancel(ctx))
			return
		case <-t.C:
			r.flushOutbox(ctx)
		}
	}
}

func (r *Runner) beat(ctx context.Context) {
	bctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var resp protocol.HeartbeatResponse
	err := r.client.Post(bctx, "/internal/worker/heartbeat", protocol.HeartbeatRequest{
		PublicIP: r.cfg.PublicIP, EHLO: r.cfg.EHLO, Capacity: r.cfg.Concurrency, Version: r.version, Kind: r.kind,
	}, &resp)
	if err != nil && ctx.Err() == nil {
		r.log.Warn("heartbeat failed", "err", err)
	}
}

func (r *Runner) heartbeats(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.beat(ctx)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Client calls the control plane's worker API.
type Client struct {
	Base      string
	Token     string
	UserAgent string
	HTTP      *http.Client
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("control plane answered %d: %s", e.Status, e.Body)
}

// Permanent reports whether retrying the same request cannot succeed.
func (e *HTTPError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != 408 && e.Status != 429
}

func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
