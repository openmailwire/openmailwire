package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

// Dispatcher defaults.
const (
	DefaultConcurrency      = 32
	DefaultPerWebhook       = 4
	DefaultPerProject       = 8
	DefaultBreakerThreshold = 5
	DefaultBreakerBase      = 30 * time.Second
	DefaultBreakerMax       = 10 * time.Minute

	// claimLease keeps a claimed delivery from being claimed again while
	// its request runs; a crash only delays it this long.
	claimLease = 2 * time.Minute
)

// Dispatcher fans events out to webhook deliveries and sends them.
//
// Requests run in a pool of Concurrency workers. Deliveries are claimed
// per webhook, fairly: at most PerWebhook requests to one endpoint and
// PerProject to one project's endpoints are in flight at once, and a slot is
// refilled as soon as its request ends, never after a whole batch. After
// BreakerThreshold consecutive failures an endpoint's circuit breaker opens:
// it gets no requests for BreakerBase, doubling up to BreakerMax, then one
// probe at a time until a request succeeds.
type Dispatcher struct {
	Store  *store.Store
	Sealer *secret.Sealer
	HTTP   *http.Client
	Log    *slog.Logger
	// UserAgent is sent with every request.
	UserAgent string

	// Limits; zero takes the default.
	Concurrency      int
	PerWebhook       int
	PerProject       int
	BreakerThreshold int
	BreakerBase      time.Duration
	BreakerMax       time.Duration

	once            sync.Once
	mu              sync.Mutex
	running         int
	inFlight        map[string]int // per webhook
	inFlightProject map[string]int
	wg              sync.WaitGroup
	freed           chan struct{}
}

func (d *Dispatcher) init() {
	d.once.Do(func() {
		d.Concurrency = orDefault(d.Concurrency, DefaultConcurrency)
		d.PerWebhook = orDefault(d.PerWebhook, DefaultPerWebhook)
		d.PerProject = orDefault(d.PerProject, DefaultPerProject)
		d.BreakerThreshold = orDefault(d.BreakerThreshold, DefaultBreakerThreshold)
		if d.BreakerBase <= 0 {
			d.BreakerBase = DefaultBreakerBase
		}
		if d.BreakerMax <= 0 {
			d.BreakerMax = DefaultBreakerMax
		}
		d.inFlight = map[string]int{}
		d.inFlightProject = map[string]int{}
		d.freed = make(chan struct{}, 1)
	})
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// Run works until ctx is cancelled: every tick it fans out new events and
// claims due deliveries; between ticks it refills the pool as requests
// end. On return no request is running.
func (d *Dispatcher) Run(ctx context.Context, tick time.Duration) {
	d.init()
	defer d.wg.Wait()
	t := time.NewTicker(tick)
	defer t.Stop()
	d.fanout(ctx)
	d.fill(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.fanout(ctx)
			d.fill(ctx)
		case <-d.freed:
			// Refill without waiting for the tick once a quarter of the
			// pool is free, so claims stay few under load.
			d.mu.Lock()
			free := d.Concurrency - d.running
			d.mu.Unlock()
			if free >= max(1, d.Concurrency/4) {
				d.fill(ctx)
			}
		}
	}
}

// Step fans out new events, claims what is due and waits for those
// requests: one synchronous round, for tests and one-shot tools.
func (d *Dispatcher) Step(ctx context.Context) {
	d.init()
	d.fanout(ctx)
	d.fill(ctx)
	d.wg.Wait()
}

func (d *Dispatcher) fanout(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := d.Store.FanoutWebhooks(ctx, 500, Build)
		if err != nil {
			if ctx.Err() == nil {
				d.Log.Error("webhook fanout", "err", err)
			}
			return
		}
		if n == 0 {
			return
		}
	}
}

func (d *Dispatcher) fill(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	free := d.Concurrency - d.running
	inFlight := make(map[string]int, len(d.inFlight))
	for k, v := range d.inFlight {
		inFlight[k] = v
	}
	inFlightProject := make(map[string]int, len(d.inFlightProject))
	for k, v := range d.inFlightProject {
		inFlightProject[k] = v
	}
	d.mu.Unlock()
	if free <= 0 {
		return
	}
	due, err := d.Store.ClaimWebhookDeliveries(ctx, store.ClaimOptions{
		Max: free, Lease: claimLease, PerWebhook: d.PerWebhook, PerProject: d.PerProject,
		BreakerThreshold: d.BreakerThreshold, InFlight: inFlight, InFlightProject: inFlightProject,
	})
	if err != nil {
		if ctx.Err() == nil {
			d.Log.Error("webhook claim", "err", err)
		}
		return
	}
	for _, w := range due {
		d.mu.Lock()
		d.running++
		d.inFlight[w.WebhookID]++
		d.inFlightProject[w.ProjectID]++
		d.mu.Unlock()
		d.wg.Add(1)
		go func(w store.DueWebhook) {
			defer d.done(w)
			d.send(ctx, w)
		}(w)
	}
}

func (d *Dispatcher) done(w store.DueWebhook) {
	d.mu.Lock()
	d.running--
	if d.inFlight[w.WebhookID]--; d.inFlight[w.WebhookID] <= 0 {
		delete(d.inFlight, w.WebhookID)
	}
	if d.inFlightProject[w.ProjectID]--; d.inFlightProject[w.ProjectID] <= 0 {
		delete(d.inFlightProject, w.ProjectID)
	}
	d.mu.Unlock()
	d.wg.Done()
	select {
	case d.freed <- struct{}{}:
	default:
	}
}

// errUnseal marks a failure on our side: the signing material could not be
// read. It doesn't count against the endpoint.
var errUnseal = errors.New("webhook signing secret cannot be unsealed")

func (d *Dispatcher) send(ctx context.Context, w store.DueWebhook) {
	status, err := d.post(ctx, w)
	if err != nil && ctx.Err() != nil {
		return // shutting down: the claim lease runs out and the delivery is retried
	}
	ok := err == nil && status >= 200 && status < 300
	shown := ""
	switch {
	case err != nil:
		shown = describe(err)
		// The operator log gets the details (they may name the address the
		// endpoint resolved to); the project only sees shown.
		d.Log.Warn("webhook attempt failed", "delivery", w.ID, "webhook", w.WebhookID, "type", w.Type,
			"attempt", w.Attempts, "err", err)
	case !ok:
		shown = "endpoint answered " + strconv.Itoa(status)
		d.Log.Warn("webhook attempt failed", "delivery", w.ID, "webhook", w.WebhookID, "type", w.Type,
			"attempt", w.Attempts, "status", status)
	}
	var next *time.Time
	if !ok && w.Attempts <= len(Schedule) {
		t := time.Now().Add(Schedule[w.Attempts-1])
		next = &t
	}
	until, ferr := d.Store.FinishWebhookDelivery(context.WithoutCancel(ctx), store.WebhookAttempt{
		ID: w.ID, WebhookID: w.WebhookID, OK: ok, Status: status, Error: shown, Next: next,
		EndpointFault:    !errors.Is(err, errUnseal),
		BreakerThreshold: d.BreakerThreshold, BreakerBase: d.BreakerBase, BreakerMax: d.BreakerMax,
	})
	if ferr != nil {
		d.Log.Error("webhook finish", "delivery", w.ID, "err", ferr)
	}
	if !ok && until != nil && until.After(time.Now()) {
		d.Log.Warn("webhook endpoint paused by its circuit breaker", "webhook", w.WebhookID,
			"until", until.UTC().Format(time.RFC3339))
	}
}

// describe turns a request error into text a project may see: what went
// wrong, never the address an endpoint resolved to or anything else about
// the network the control plane runs in. A name that does not resolve and a
// name that resolves to a refused address read the same.
func describe(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	var invalidErr x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	var netErr net.Error
	switch {
	case errors.Is(err, errUnseal):
		return "internal error: the webhook's signing secret could not be read; the operator's log has the details"
	case errors.Is(err, errBlocked), errors.As(err, &dnsErr):
		return "could not connect: the endpoint's host name does not resolve to a public address"
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &authErr), errors.As(err, &invalidErr):
		return "TLS failed: the endpoint's certificate is not valid for its host name or not trusted"
	case errors.As(err, &recordErr), strings.Contains(err.Error(), "tls: "):
		return "TLS failed: the TLS handshake with the endpoint did not complete"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "could not connect: the endpoint refused the connection"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Sprintf("timed out: the endpoint did not answer within %s (answer 2xx first, then process the event)",
			ResponseHeaderTimeout)
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the endpoint closed the connection before answering"
	}
	return "the request to the endpoint failed"
}

// headers returns the signature header value: every current signature,
// space-separated. During a secret rotation that is two "v1" signatures,
// and two "v1a" ones when the webhook signs with Ed25519.
func (d *Dispatcher) signatures(w store.DueWebhook, id string, ts int64) (string, error) {
	var sigs []string
	for _, sealed := range w.Secrets {
		whsec, err := d.Sealer.Open(sealed)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errUnseal, err)
		}
		sig, err := Sign(string(whsec), id, ts, w.Payload)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errUnseal, err)
		}
		sigs = append(sigs, sig)
	}
	for _, sealed := range w.Ed25519Keys {
		seed, err := d.Sealer.Open(sealed)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errUnseal, err)
		}
		sig, err := SignEd25519(seed, id, ts, w.Payload)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errUnseal, err)
		}
		sigs = append(sigs, sig)
	}
	if len(sigs) == 0 {
		return "", errUnseal
	}
	return strings.Join(sigs, " "), nil
}

func (d *Dispatcher) post(ctx context.Context, w store.DueWebhook) (int, error) {
	id := "msg_" + strings.ReplaceAll(w.ID, "-", "")
	ts := time.Now().Unix()
	sig, err := d.signatures(w, id, ts)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Endpoint, bytes.NewReader(w.Payload))
	if err != nil {
		return 0, err
	}
	tsStr := strconv.FormatInt(ts, 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", d.UserAgent)
	req.Header.Set("svix-id", id)
	req.Header.Set("svix-timestamp", tsStr)
	req.Header.Set("svix-signature", sig)
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", tsStr)
	req.Header.Set("webhook-signature", sig)
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}
