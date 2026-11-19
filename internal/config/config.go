// Package config reads settings from the environment.
package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/policy"
)

// Control configures the control plane (openmailwire serve).
type Control struct {
	DatabaseURL       string
	Listen            string
	SecretKey         []byte // seals new values: the first of SecretKeys
	ReturnPathLabel   string
	SPFInclude        string
	BounceMX          string
	AllowUnverified   bool
	Retention         time.Duration
	DefaultExpiry     time.Duration
	LeaseDuration     time.Duration
	DomainConcurrency int
	// RelayAfterAttempts routes a delivery to relay workers, when one is
	// live, once it has had this many attempts.
	RelayAfterAttempts int
	DNSServer          string
	// Webhook endpoints must use https and public addresses unless these
	// are set (development only).
	WebhookAllowHTTP    bool
	WebhookAllowPrivate bool

	// Deliverability (lane C). ProviderRules group recipient domains by
	// mailbox provider for limits and warm-up (OMW_PROVIDER_RULES, or the
	// built-in rules); Warmup caps what new sending IPs send per provider
	// and day (OMW_WARMUP).
	ProviderRules *policy.Rules
	Warmup        bool

	// Inbound mail.
	InboundMX        string        // host that receives mail for inboxes; shown as an optional MX record
	FBLAddress       string        // where feedback loops (ARF complaints) are sent
	InboundRetention time.Duration // inbound message bodies are purged this long after arrival
	InboundMaxSize   int64         // largest inbound message, in bytes

	APILimits // rate limits, large bodies, trusted proxies (api.go)
	// PublicURL (OMW_PUBLIC_URL) is where people and mailbox providers
	// reach this control plane, e.g. https://mail.example.com. Broadcast
	// mail needs it for its one-click unsubscribe links (lane B).
	PublicURL string
	// Platform (secrets.go).
	SecretKeys         [][]byte // open sealed values: OMW_SECRET_KEYS, or OMW_SECRET_KEY
	WebhooksPerProject int      // OMW_WEBHOOKS_PER_PROJECT
}

// DatabaseURL returns DATABASE_URL or an error when it is unset.
func DatabaseURL() (string, error) {
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		return "", errors.New("DATABASE_URL is not set")
	}
	return u, nil
}

func LoadControl() (Control, error) {
	c := Control{
		Listen:          env("OMW_LISTEN", "127.0.0.1:8080"),
		ReturnPathLabel: env("OMW_RETURN_PATH_LABEL", "bounce"),
		SPFInclude:      os.Getenv("OMW_SPF_INCLUDE"),
		BounceMX:        os.Getenv("OMW_BOUNCE_MX"),
		DNSServer:       os.Getenv("OMW_DNS_SERVER"),
		InboundMX:       strings.TrimSuffix(os.Getenv("OMW_INBOUND_MX"), "."),
		FBLAddress:      strings.ToLower(strings.TrimSpace(os.Getenv("OMW_FBL_ADDRESS"))),
	}
	var err error
	if c.DatabaseURL, err = DatabaseURL(); err != nil {
		return c, err
	}
	if err := c.loadPlatform(); err != nil { // OMW_SECRET_KEYS and webhook limits (secrets.go)
		return c, err
	}
	if c.AllowUnverified, err = envBool("OMW_ALLOW_UNVERIFIED", false); err != nil {
		return c, err
	}
	if c.Retention, err = envDuration("OMW_RETENTION", 72*time.Hour); err != nil {
		return c, err
	}
	if c.DefaultExpiry, err = envDuration("OMW_DEFAULT_EXPIRY", 48*time.Hour); err != nil {
		return c, err
	}
	if c.LeaseDuration, err = envDuration("OMW_LEASE", 15*time.Minute); err != nil {
		return c, err
	}
	if c.DomainConcurrency, err = envInt("OMW_DOMAIN_CONCURRENCY", 10); err != nil {
		return c, err
	}
	if c.RelayAfterAttempts, err = envInt("OMW_RELAY_AFTER_ATTEMPTS", 2); err != nil {
		return c, err
	}
	if c.WebhookAllowHTTP, err = envBool("OMW_WEBHOOK_ALLOW_HTTP", false); err != nil {
		return c, err
	}
	if c.WebhookAllowPrivate, err = envBool("OMW_WEBHOOK_ALLOW_PRIVATE", false); err != nil {
		return c, err
	}
	if c.InboundRetention, err = envDuration("OMW_INBOUND_RETENTION", 720*time.Hour); err != nil {
		return c, err
	}
	if c.InboundMaxSize, err = envSize("OMW_INBOUND_MAX_SIZE", DefaultInboundMaxSize); err != nil {
		return c, err
	}
	if c.APILimits, err = loadAPILimits(); err != nil {
		return c, err
	}
	if c.PublicURL, err = PublicURL(os.Getenv("OMW_PUBLIC_URL")); err != nil { // lane B
		return c, err
	}
	if c.ProviderRules, err = policy.LoadRules(os.Getenv("OMW_PROVIDER_RULES")); err != nil {
		return c, err
	}
	if c.Warmup, err = envBool("OMW_WARMUP", true); err != nil {
		return c, err
	}
	return c, nil
}

// PublicURL checks OMW_PUBLIC_URL and returns it without a trailing slash.
// Empty is allowed: broadcast mail is then refused.
func PublicURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("OMW_PUBLIC_URL must be an absolute http(s) URL without a query, like https://mail.example.com")
	}
	return raw, nil
}

// DefaultInboundMaxSize is the largest inbound message accepted by default.
const DefaultInboundMaxSize = 25 << 20

// Inbound configures an inbound SMTP node (openmailwire inbound).
type Inbound struct {
	Listen     string
	Hostname   string // announced in the greeting and in Received headers
	TLSCert    string // with TLSKey, enables STARTTLS
	TLSKey     string
	ControlURL string
	Token      string // a worker token (omwk_…)
	MaxSize    int64
}

func LoadInbound() (Inbound, error) {
	host, _ := os.Hostname()
	in := Inbound{
		Listen:     env("OMW_INBOUND_LISTEN", ":25"),
		Hostname:   env("OMW_INBOUND_HOSTNAME", host),
		TLSCert:    os.Getenv("OMW_TLS_CERT"),
		TLSKey:     os.Getenv("OMW_TLS_KEY"),
		ControlURL: strings.TrimRight(os.Getenv("OMW_CONTROL_URL"), "/"),
		Token:      os.Getenv("OMW_WORKER_TOKEN"),
	}
	if in.ControlURL == "" {
		return in, errors.New("OMW_CONTROL_URL is not set")
	}
	if !strings.HasPrefix(in.Token, "omwk_") {
		return in, errors.New("OMW_WORKER_TOKEN is not set; inbound nodes use a worker token: `openmailwire workers add <name>`")
	}
	if (in.TLSCert == "") != (in.TLSKey == "") {
		return in, errors.New("set both OMW_TLS_CERT and OMW_TLS_KEY to enable STARTTLS, or neither")
	}
	var err error
	if in.MaxSize, err = envSize("OMW_INBOUND_MAX_SIZE", DefaultInboundMaxSize); err != nil {
		return in, err
	}
	return in, nil
}

// Worker configures a delivery worker (openmailwire worker).
type Worker struct {
	ControlURL   string
	Token        string
	EHLO         string
	SourceIP     string
	PublicIP     string
	SMTPOverride string
	StateDir     string
	Concurrency  int
	IPv4Only     bool
	PollInterval time.Duration
	JobTimeout   time.Duration

	// DNSSECResolver (host:port) is a validating resolver the worker trusts.
	// When set, the worker honours DANE TLSA records (RFC 7672) and looks up
	// MX and address records through it.
	DNSSECResolver string

	// SkipSelfCheck skips the start-up checks (EHLO name, its A record,
	// reverse DNS, port 25). Development only. SelfCheckSMTP is the host:port
	// the port 25 check connects to; empty means Gmail's MX.
	SkipSelfCheck bool
	SelfCheckSMTP string

	// A relay worker hands every message to this smarthost instead of the
	// recipients' mail servers. Setting RelayHost makes the worker a relay.
	RelayHost     string
	RelayPort     int
	RelayUsername string
	RelayPassword string
	RelayTLS      string // "starttls" (default) or "implicit"

	// TLSRoots replaces the system roots for verifying certificates (MX
	// hosts, MTA-STS policy hosts, the relay). Not read from the
	// environment; tests set it.
	TLSRoots *x509.CertPool
}

// Relay reports whether the worker is a fallback relay worker.
func (w Worker) Relay() bool { return w.RelayHost != "" }

func LoadWorker() (Worker, error) {
	host, _ := os.Hostname()
	w := Worker{
		ControlURL:   strings.TrimRight(os.Getenv("OMW_CONTROL_URL"), "/"),
		Token:        os.Getenv("OMW_WORKER_TOKEN"),
		EHLO:         env("OMW_EHLO", host),
		SourceIP:     os.Getenv("OMW_SOURCE_IP"),
		PublicIP:     os.Getenv("OMW_PUBLIC_IP"),
		SMTPOverride: os.Getenv("OMW_SMTP_OVERRIDE"),
		StateDir:     env("OMW_STATE_DIR", "./openmailwire-worker-state"),
	}
	if w.PublicIP == "" {
		w.PublicIP = w.SourceIP
	}
	if w.ControlURL == "" {
		return w, errors.New("OMW_CONTROL_URL is not set")
	}
	if !strings.HasPrefix(w.Token, "omwk_") {
		return w, errors.New("OMW_WORKER_TOKEN is not set; create one with `openmailwire workers add <name>`")
	}
	var err error
	if w.Concurrency, err = envInt("OMW_CONCURRENCY", 20); err != nil {
		return w, err
	}
	if w.IPv4Only, err = envBool("OMW_IPV4_ONLY", true); err != nil {
		return w, err
	}
	if w.PollInterval, err = envDuration("OMW_POLL_INTERVAL", 2*time.Second); err != nil {
		return w, err
	}
	if w.JobTimeout, err = envDuration("OMW_JOB_TIMEOUT", 10*time.Minute); err != nil {
		return w, err
	}
	if w.SkipSelfCheck, err = envBool("OMW_SKIP_SELF_CHECK", false); err != nil {
		return w, err
	}
	w.SelfCheckSMTP = os.Getenv("OMW_SELF_CHECK_SMTP")
	if w.SelfCheckSMTP != "" {
		if _, _, err := net.SplitHostPort(w.SelfCheckSMTP); err != nil {
			return w, errors.New("OMW_SELF_CHECK_SMTP must be host:port, e.g. mx.example.net:25")
		}
	}
	w.DNSSECResolver = os.Getenv("OMW_DNSSEC_RESOLVER")
	if w.DNSSECResolver != "" {
		if _, _, err := net.SplitHostPort(w.DNSSECResolver); err != nil {
			return w, errors.New("OMW_DNSSEC_RESOLVER must be host:port, e.g. 127.0.0.1:53")
		}
	}
	w.RelayHost = os.Getenv("OMW_RELAY_HOST")
	w.RelayUsername = os.Getenv("OMW_RELAY_USERNAME")
	w.RelayPassword = os.Getenv("OMW_RELAY_PASSWORD")
	w.RelayTLS = env("OMW_RELAY_TLS", "starttls")
	if w.RelayPort, err = envInt("OMW_RELAY_PORT", 587); err != nil {
		return w, err
	}
	if w.RelayTLS != "starttls" && w.RelayTLS != "implicit" {
		return w, errors.New(`OMW_RELAY_TLS must be "starttls" or "implicit"`)
	}
	if w.RelayHost == "" && (w.RelayUsername != "" || w.RelayPassword != "") {
		return w, errors.New("OMW_RELAY_USERNAME is set but OMW_RELAY_HOST is not; set the smarthost or remove the credentials")
	}
	if (w.RelayUsername == "") != (w.RelayPassword == "") {
		return w, errors.New("set both OMW_RELAY_USERNAME and OMW_RELAY_PASSWORD, or neither")
	}
	if w.Relay() && w.SMTPOverride != "" {
		return w, errors.New("OMW_SMTP_OVERRIDE cannot be combined with OMW_RELAY_HOST; a relay worker always sends to its smarthost")
	}
	return w, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

// envSize reads a size such as 25MB, 512KB or 1048576 (bytes). KB, MB and
// GB are powers of 1024.
func envSize(key string, def int64) (int64, error) {
	v := strings.ToUpper(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"B", 1}} {
		if n, ok := strings.CutSuffix(v, u.suffix); ok {
			v, mult = strings.TrimSpace(n), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 || n > (1<<40)/mult {
		return def, fmt.Errorf("%s must be a size like 25MB", key)
	}
	return n * mult, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def, fmt.Errorf("%s must be a positive duration like 30m or 48h", key)
	}
	return d, nil
}
