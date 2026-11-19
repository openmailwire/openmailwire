// Command openmailwire runs the OpenMailWire control plane, a delivery worker, or an
// admin task. Run `openmailwire help` for the list.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openmailwire/openmailwire/internal/api"
	"github.com/openmailwire/openmailwire/internal/apikey"
	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/domains"
	"github.com/openmailwire/openmailwire/internal/inbound"
	"github.com/openmailwire/openmailwire/internal/mcpserver"
	"github.com/openmailwire/openmailwire/internal/policy"
	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/smtptest"
	"github.com/openmailwire/openmailwire/internal/store"
	"github.com/openmailwire/openmailwire/internal/webhook"
	"github.com/openmailwire/openmailwire/internal/worker"
)

var version = "dev"

const usage = `openmailwire — open-source email for AI agents and the apps they run in: an
HTTP API, an MCP server, DKIM, and workers that deliver from your own IPs.

Usage:
  openmailwire serve                          run the control plane (API, queue, scheduler)
  openmailwire worker                         run a delivery worker
  openmailwire submission                     SMTP submission on 587/465 for apps that only speak SMTP
  openmailwire inbound                        run an inbound SMTP node (inboxes, bounces, complaints)
  openmailwire migrate                        apply database migrations
  openmailwire keygen                         print a new OMW_SECRET_KEY
  openmailwire rotate-secrets                 reseal stored secrets with the first key of OMW_SECRET_KEYS (online)
  openmailwire project create <name>          create a project and its first API key
  openmailwire apikey create <project-id> <name> [--sending-only] [--max-per-hour N] [--max-per-day N]
                       [--allow-to PATTERN]... [--allow-from PATTERN]... [--allow-ip CIDR]... [--expires-in DURATION] [--inbox PATTERN]...
                                              create an API key, e.g. one per agent, with its limits
  openmailwire apikey list <project-id>       list a project's live API keys
  openmailwire workers add <name> [--role delivery|inbound|relay]
                                              register a worker (default role: delivery) and print its token
  openmailwire workers list                   workers, their self-check and their IPs' warm-up
  openmailwire workers drain <name>           stop giving the worker new jobs
  openmailwire workers resume <name>
  openmailwire workers warm <name> [--restart]
                                              treat the worker's IP as warm (no warm-up caps), or start over
  openmailwire domains force-verify <domain> [project-id]
                                              mark a domain verified without DNS (development only)
  openmailwire mcp                            MCP server on stdio for AI agents (needs OMW_API_URL, OMW_API_KEY)
  openmailwire dev-sink [addr]                local SMTP sink that prints what it receives (development only)
  openmailwire version

Configuration is read from the environment; see README.md.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, log)
	case "worker":
		err = runWorker(ctx, log)
	case "submission":
		err = runSubmission(ctx, log)
	case "inbound":
		err = runInbound(ctx, log)
	case "migrate":
		err = withStore(ctx, func(st *store.Store) error { return nil })
		if err == nil {
			fmt.Println("migrations applied")
		}
	case "keygen":
		fmt.Println(secret.NewKey())
	case "rotate-secrets":
		err = rotateSecrets(ctx, args)
	case "project":
		err = projectCmd(ctx, args)
	case "apikey":
		err = apikeyCmd(ctx, args)
	case "workers":
		err = workersCmd(ctx, args)
	case "domains":
		err = domainsCmd(ctx, args)
	case "dev-sink":
		err = devSink(ctx, args)
	case "mcp":
		err = runMCP(ctx)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadControl()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	sealer, macRoot, err := openSecrets(ctx, st, cfg.SecretKeys, log) // checks the keys against the data (secrets.go)
	if err != nil {
		return err
	}
	if cfg.AllowUnverified {
		log.Warn("OMW_ALLOW_UNVERIFIED is on: unverified domains can send. Never use this in production.")
	}
	srv := &api.Server{
		Store: st, Sealer: sealer, MACKey: secret.Derive(macRoot, "openmailwire/verp"),
		Resolver: domains.NewResolver(cfg.DNSServer), Cfg: cfg, Log: log, Version: version,
	}
	httpSrv := newHTTPServer(cfg.Listen, srv.Handler())

	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := st.Sweep(ctx, cfg.Retention); err != nil && ctx.Err() == nil {
					log.Error("sweep", "err", err)
				}
				if err := st.SweepInbound(ctx, cfg.InboundRetention); err != nil && ctx.Err() == nil {
					log.Error("sweep inbound", "err", err)
				}
				if err := st.SweepSafety(ctx); err != nil && ctx.Err() == nil { // lane B: held mail expiry, cap rows
					log.Error("sweep safety", "err", err)
				}
			}
		}
	}()
	go func() {
		// Unverified domain claims expire (store.DomainClaim).
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			expired, err := st.ExpireDomainClaims(ctx)
			if err != nil && ctx.Err() == nil {
				log.Error("expire domain claims", "err", err)
			}
			for _, d := range expired {
				log.Info("unverified domain claim expired", "domain", d.Name, "project", d.ProjectID)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	if cfg.WebhookAllowHTTP || cfg.WebhookAllowPrivate {
		log.Warn("webhook endpoints may use plain http or private addresses. Never use this in production.")
	}
	dispatcher := &webhook.Dispatcher{
		Store: st, Sealer: sealer, HTTP: webhook.NewHTTPClient(cfg.WebhookAllowPrivate),
		Log: log, UserAgent: "openmailwire-webhooks/" + version,
	}
	go dispatcher.Run(ctx, time.Second)

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(sctx)
	}()

	log.Info("control plane listening", "addr", cfg.Listen, "version", version)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// HTTP server limits. ReadTimeout bounds a whole request, body included, and
// also the handler's run: when it passes, net/http cancels the request's
// context. The longest handlers are MCP waits (wait_for_reply, up to 120 s),
// and a 20 MB body needs about 1 Mbit/s to arrive within it. WriteTimeout
// counts from the end of the request header, so it covers the body read,
// the work and the answer.
const (
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 3 * time.Minute
	httpWriteTimeout      = 4 * time.Minute
	httpIdleTimeout       = 2 * time.Minute
	httpMaxHeaderBytes    = 64 << 10
)

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr: addr, Handler: h,
		ReadHeaderTimeout: httpReadHeaderTimeout, ReadTimeout: httpReadTimeout,
		WriteTimeout: httpWriteTimeout, IdleTimeout: httpIdleTimeout, MaxHeaderBytes: httpMaxHeaderBytes,
	}
}

func runWorker(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	kind := "direct"
	if cfg.Relay() {
		kind = "relay"
	}
	log.Info("worker starting", "control", cfg.ControlURL, "kind", kind, "ehlo", cfg.EHLO, "public_ip", cfg.PublicIP,
		"concurrency", cfg.Concurrency, "dane", cfg.DNSSECResolver != "", "self_check", !cfg.SkipSelfCheck, "version", version)
	return worker.NewRunner(cfg, log, version).Run(ctx)
}

// runInbound runs an inbound SMTP node. It holds no data: every recipient is
// checked with the control plane and every message is stored there.
func runInbound(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadInbound()
	if err != nil {
		return err
	}
	var tlsConfig *tls.Config
	if cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return fmt.Errorf("OMW_TLS_CERT/OMW_TLS_KEY: %w", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	} else {
		log.Warn("OMW_TLS_CERT is not set: STARTTLS is off, so senders deliver to this node in plain text")
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := inbound.New(inbound.Config{Hostname: cfg.Hostname, TLS: tlsConfig, MaxSize: cfg.MaxSize, MaxConnsPerIP: cfg.MaxConnsPerIP},
		&inbound.Client{URL: cfg.ControlURL, Token: cfg.Token, HTTP: &http.Client{Timeout: 3 * time.Minute}}, log)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			srv.Close()
		}
	}()
	log.Info("inbound SMTP listening", "addr", cfg.Listen, "hostname", cfg.Hostname, "starttls", tlsConfig != nil,
		"max_size", cfg.MaxSize, "control", cfg.ControlURL, "version", version)
	return srv.Serve(ln)
}

func withStore(ctx context.Context, fn func(*store.Store) error) error {
	url, err := config.DatabaseURL()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	return fn(st)
}

func projectCmd(ctx context.Context, args []string) error {
	if len(args) != 2 || args[0] != "create" {
		return errors.New("usage: openmailwire project create <name>")
	}
	return withStore(ctx, func(st *store.Store) error {
		id, err := st.CreateProject(ctx, args[1])
		if err != nil {
			return err
		}
		token, err := newAPIKey(ctx, st, id, "default")
		if err != nil {
			return err
		}
		fmt.Printf("project  %s\napi key  %s\n\nThe key is shown once. Store it now.\n", id, token)
		return nil
	})
}

const apikeyUsage = `usage:
  openmailwire apikey create <project-id> <name> [--sending-only] [--max-per-hour N] [--max-per-day N]
                             [--allow-to PATTERN]... [--allow-from PATTERN]... [--inbox PATTERN]...
                             [--allow-ip CIDR]... [--expires-in DURATION]
  openmailwire apikey list <project-id>

Patterns match addresses; "*" matches any characters within the local part
or the domain part: --allow-to '*@acme.com' --allow-from agent@acme.com.
A --sending-only key reads only the inboxes --inbox names: --inbox support@acme.com.
--allow-ip limits where the key may be used from (203.0.113.7, 10.0.0.0/8);
--expires-in makes it stop working after a while (720h).`

// patterns collects a repeatable flag.
type patterns []string

func (p *patterns) String() string     { return strings.Join(*p, ",") }
func (p *patterns) Set(v string) error { *p = append(*p, v); return nil }

// parseInterspersed parses flags that may come before, between or after the
// positional arguments, and returns the positional ones.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func apikeyCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(apikeyUsage)
	}
	switch args[0] {
	case "create":
		return apikeyCreate(ctx, args[1:])
	case "list":
		if len(args) != 2 {
			return errors.New(apikeyUsage)
		}
		return apikeyList(ctx, args[1])
	}
	return errors.New(apikeyUsage)
}

func apikeyCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apikey create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sendingOnly := fs.Bool("sending-only", false, "")
	perHour := fs.Int("max-per-hour", 0, "")
	perDay := fs.Int("max-per-day", 0, "")
	var allowTo, allowFrom, allowIP, inboxes patterns
	fs.Var(&allowTo, "allow-to", "")
	fs.Var(&allowFrom, "allow-from", "")
	fs.Var(&allowIP, "allow-ip", "")
	fs.Var(&inboxes, "inbox", "") // policy.inboxes
	expiresIn := fs.Duration("expires-in", 0, "")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return fmt.Errorf("%v\n\n%s", err, apikeyUsage)
	}
	if len(pos) != 2 {
		return errors.New(apikeyUsage)
	}
	projectID, name := pos[0], pos[1]
	var policy apikey.Policy
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "max-per-hour":
			policy.MaxPerHour = perHour
		case "max-per-day":
			policy.MaxPerDay = perDay
		}
	})
	if len(allowTo) > 0 {
		policy.AllowedRecipients = allowTo
	}
	if len(allowFrom) > 0 {
		policy.AllowedFrom = allowFrom
	}
	if len(inboxes) > 0 {
		policy.Inboxes = inboxes
	}
	if err := policy.Normalize(); err != nil {
		return err
	}
	var ips []netip.Prefix
	if len(allowIP) > 0 {
		if ips, err = apikey.ParseAllowedIPs(allowIP); err != nil {
			return err
		}
	}
	var expires *time.Time
	if *expiresIn < 0 {
		return errors.New("--expires-in must be positive, like 720h")
	}
	if *expiresIn > 0 {
		t := time.Now().UTC().Add(*expiresIn)
		expires = &t
	}
	permission := apikey.FullAccess
	if *sendingOnly {
		permission = apikey.SendingAccess
	}
	return withStore(ctx, func(st *store.Store) error {
		token, hash := secret.NewToken("omw_")
		k, err := st.NewAPIKey(ctx, store.APIKey{
			ProjectID: projectID, Name: name, Prefix: token[:10], Permission: permission, Policy: policy,
			AllowedIPs: ips, ExpiresAt: expires,
		}, hash)
		if err != nil {
			return fmt.Errorf("create key (is %s a project id?): %w", projectID, err)
		}
		if err := st.AppendAudit(ctx, store.AuditEntry{ProjectID: projectID, Actor: store.ActorCLI, Action: "api_key.created",
			TargetType: "api_key", TargetID: k.ID, Data: map[string]any{"name": name, "prefix": k.Prefix,
				"permission": k.Permission, "policy": k.Policy, "allowed_ips": allowIP, "expires_at": k.ExpiresAt}}); err != nil {
			return err
		}
		fmt.Printf("api key     %s\nid          %s\npermission  %s\npolicy      %s\n", token, k.ID, k.Permission, policyJSON(k.Policy))
		if len(ips) > 0 {
			fmt.Printf("allowed ips %s\n", strings.Join(allowIP, ", "))
		}
		if expires != nil {
			fmt.Printf("expires     %s\n", expires.Format(time.RFC3339))
		}
		fmt.Print("\nThe key is shown once. Store it now.\n")
		return nil
	})
}

func apikeyList(ctx context.Context, projectID string) error {
	return withStore(ctx, func(st *store.Store) error {
		keys, err := st.ListAPIKeys(ctx, projectID)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tPREFIX\tPERMISSION\tPOLICY\tCREATED\tLAST USED")
		for _, k := range keys {
			used := "never"
			if k.LastUsedAt != nil {
				used = k.LastUsedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s…\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Prefix, k.Permission, policyJSON(k.Policy),
				k.CreatedAt.UTC().Format(time.RFC3339), used)
		}
		return tw.Flush()
	})
}

func policyJSON(p apikey.Policy) string {
	b, _ := json.Marshal(p)
	return string(b)
}

func newAPIKey(ctx context.Context, st *store.Store, projectID, name string) (string, error) {
	token, hash := secret.NewToken("omw_")
	k, err := st.NewAPIKey(ctx, store.APIKey{ProjectID: projectID, Name: name, Prefix: token[:10]}, hash)
	if err != nil {
		return "", err
	}
	if err := st.AppendAudit(ctx, store.AuditEntry{ProjectID: projectID, Actor: store.ActorCLI, Action: "api_key.created",
		TargetType: "api_key", TargetID: k.ID, Data: map[string]any{"name": name, "prefix": k.Prefix, "permission": k.Permission}}); err != nil {
		return "", err
	}
	return token, nil
}

const workersUsage = `usage:
  openmailwire workers add <name> [--role delivery|inbound|relay]
  openmailwire workers list
  openmailwire workers drain <name> | resume <name>
  openmailwire workers warm <name> [--restart]

Roles: delivery workers send from their own IP, relay workers through a
smarthost (OMW_RELAY_HOST), inbound nodes receive mail. A token works only for
its role.`

func workersCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(workersUsage)
	}
	return withStore(ctx, func(st *store.Store) error {
		switch {
		case args[0] == "add":
			fs := flag.NewFlagSet("workers add", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			role := fs.String("role", protocol.RoleDelivery, "")
			pos, err := parseInterspersed(fs, args[1:])
			if err != nil || len(pos) != 1 {
				return errors.New(workersUsage)
			}
			if !protocol.ValidRole(*role) {
				return fmt.Errorf("--role must be delivery, inbound or relay, not %q", *role)
			}
			token, hash := secret.NewToken("omwk_")
			if _, err := st.CreateWorkerWithRole(ctx, pos[0], *role, hash); err != nil {
				if errors.Is(err, store.ErrConflict) {
					return fmt.Errorf("a worker named %q already exists", pos[0])
				}
				return err
			}
			what := "worker"
			if *role == protocol.RoleInbound {
				what = "inbound node"
			}
			fmt.Printf("worker   %s\nrole     %s\ntoken    %s\n\nSet it as OMW_WORKER_TOKEN on that %s. It is shown once.\n",
				pos[0], *role, token, what)
		case args[0] == "list":
			return workersList(ctx, st)
		case (args[0] == "drain" || args[0] == "resume") && len(args) == 2:
			if err := st.SetDraining(ctx, args[1], args[0] == "drain"); err != nil {
				return err
			}
			fmt.Printf("worker %s: draining=%v\n", args[1], args[0] == "drain")
		case args[0] == "warm" && (len(args) == 2 || len(args) == 3 && args[2] == "--restart"):
			ws, err := st.ListWorkers(ctx)
			if err != nil {
				return err
			}
			for _, w := range ws {
				if w.Name != args[1] {
					continue
				}
				if w.IP == "" {
					return fmt.Errorf("worker %s has not announced its IP yet; start it with OMW_PUBLIC_IP set", w.Name)
				}
				restart := len(args) == 3
				if err := st.SetIPWarm(ctx, w.IP, !restart); err != nil {
					return err
				}
				if restart {
					fmt.Printf("%s (%s) starts its warm-up again at day 1\n", w.IP, w.Name)
				} else {
					fmt.Printf("%s (%s) is warm: no warm-up caps apply to it\n", w.IP, w.Name)
				}
				return nil
			}
			return fmt.Errorf("no worker named %q", args[1])
		default:
			return errors.New(workersUsage)
		}
		return nil
	})
}

// workersList prints every worker with its role, self-check and the
// warm-up of its IP (remaining capacity per provider today).
func workersList(ctx context.Context, st *store.Store) error {
	rules, err := policy.LoadRules(os.Getenv("OMW_PROVIDER_RULES"))
	if err != nil {
		return err
	}
	warmup := os.Getenv("OMW_WARMUP") != "false"
	ws, err := st.ListWorkers(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tKIND\tIP\tEHLO\tCAPACITY\tDRAINING\tLAST SEEN\tVERSION\tSELF-CHECK\tWARM-UP")
	for _, w := range ws {
		seen := "never"
		if w.LastSeenAt != nil {
			seen = time.Since(*w.LastSeenAt).Round(time.Second).String() + " ago"
		}
		warm := "-"
		switch {
		case w.Role == protocol.RoleInbound, w.Kind == protocol.KindRelay, w.IP == "":
		case !warmup:
			warm = "off"
		default:
			s, err := st.IPWarmup(ctx, rules, w.IP)
			if err != nil {
				return err
			}
			warm = warmupSummary(s)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%v\t%s\t%s\t%s\t%s\n", w.Name, w.Role, w.Kind, w.IP, w.EHLO, w.Capacity, w.Draining,
			seen, w.Version, selfCheckSummary(w.SelfCheck), warm)
	}
	return tw.Flush()
}

func selfCheckSummary(raw []byte) string {
	if len(raw) == 0 {
		return "-"
	}
	var c protocol.SelfCheck
	if json.Unmarshal(raw, &c) != nil {
		return "?"
	}
	if c.Skipped != "" {
		return "skipped"
	}
	if c.OK {
		return "ok"
	}
	var failed []string
	for _, i := range c.Checks {
		if !i.OK {
			failed = append(failed, i.Name)
		}
	}
	return "FAILED: " + strings.Join(failed, ", ")
}

func warmupSummary(s store.WarmupStatus) string {
	switch {
	case !s.Started:
		return "starts with its first lease"
	case s.Warm:
		return "warm"
	}
	parts := []string{fmt.Sprintf("day %d", s.Day)}
	for _, p := range s.Left {
		parts = append(parts, fmt.Sprintf("%s %d/%d left", p.Provider, max(0, p.Cap-p.Used), p.Cap))
	}
	return strings.Join(parts, ", ")
}

// runMCP serves the MCP tools on stdin/stdout, acting through a control
// plane's HTTP API with one API key. Logs go to stderr; stdout carries the
// protocol.
func runMCP(ctx context.Context) error {
	base := os.Getenv("OMW_API_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	key := os.Getenv("OMW_API_KEY")
	if !strings.HasPrefix(key, "omw_") {
		return errors.New("OMW_API_KEY is not set; create one with `openmailwire apikey create <project-id> <name>`")
	}
	srv := mcpserver.New(&mcpserver.APIClient{Base: base, Key: key, HTTP: &http.Client{Timeout: 90 * time.Second}}, version)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// devSink accepts SMTP on localhost and prints each message, for running a
// worker locally with OMW_SMTP_OVERRIDE. It is not a mail server.
func devSink(ctx context.Context, args []string) error {
	addr := "127.0.0.1:2525"
	if len(args) > 0 {
		addr = args[0]
	}
	srv, err := smtptest.StartAt(addr)
	if err != nil {
		return err
	}
	defer srv.Close()
	fmt.Printf("dev-sink listening on %s; point a worker at it with OMW_SMTP_OVERRIDE=%s\n", srv.Addr, srv.Addr)
	seen := 0
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			msgs := srv.Messages()
			for _, m := range msgs[seen:] {
				fmt.Printf("\n===== message %d  MAIL FROM:<%s>  RCPT TO:%v =====\n%s", seen+1, m.MailFrom, m.Rcpts, m.Data)
				seen++
			}
		}
	}
}

func domainsCmd(ctx context.Context, args []string) error {
	if (len(args) != 2 && len(args) != 3) || args[0] != "force-verify" {
		return errors.New("usage: openmailwire domains force-verify <domain> [project-id]")
	}
	project := ""
	if len(args) == 3 {
		project = args[2]
	}
	return withStore(ctx, func(st *store.Store) error {
		err := st.ForceVerify(ctx, args[1], project)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return fmt.Errorf("no project has the domain %s (add it with POST /domains first)", args[1])
		case errors.Is(err, store.ErrConflict) && project == "":
			return fmt.Errorf("several projects have added %s, or another has it verified; pass the project id", args[1])
		case errors.Is(err, store.ErrConflict):
			return fmt.Errorf("another project has %s verified; a domain can be verified in only one project", args[1])
		case err != nil:
			return err
		}
		fmt.Printf("%s marked verified without a DNS check. Receivers will still check DNS.\n", args[1])
		return nil
	})
}
