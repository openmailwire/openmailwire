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
  openmailwire project create <name>          create a project and its first API key
  openmailwire apikey create <project-id> <name> [--sending-only] [--max-per-hour N] [--max-per-day N]
                       [--allow-to PATTERN]... [--allow-from PATTERN]... [--inbox PATTERN]...
                                              create an API key, e.g. one per agent, with its limits
  openmailwire apikey list <project-id>       list a project's live API keys
  openmailwire workers add <name>             register a worker and print its token
  openmailwire workers list
  openmailwire workers drain <name>           stop giving the worker new jobs
  openmailwire workers resume <name>
  openmailwire domains force-verify <domain>  mark a domain verified without DNS (development only)
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
	sealer, err := secret.NewSealer(cfg.SecretKey)
	if err != nil {
		return err
	}
	if cfg.AllowUnverified {
		log.Warn("OMW_ALLOW_UNVERIFIED is on: unverified domains can send. Never use this in production.")
	}
	srv := &api.Server{
		Store: st, Sealer: sealer, MACKey: secret.Derive(cfg.SecretKey, "openmailwire/verp"),
		Resolver: domains.NewResolver(cfg.DNSServer), Cfg: cfg, Log: log, Version: version,
	}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

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
		"concurrency", cfg.Concurrency, "dane", cfg.DNSSECResolver != "", "version", version)
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
  openmailwire apikey list <project-id>

Patterns match addresses; "*" matches any characters within the local part
or the domain part: --allow-to '*@acme.com' --allow-from agent@acme.com
A --sending-only key reads only the inboxes --inbox names: --inbox support@acme.com`

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
	var allowTo, allowFrom, inboxes patterns
	fs.Var(&allowTo, "allow-to", "")
	fs.Var(&allowFrom, "allow-from", "")
	fs.Var(&inboxes, "inbox", "") // lane D: policy.inboxes
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
	permission := apikey.FullAccess
	if *sendingOnly {
		permission = apikey.SendingAccess
	}
	return withStore(ctx, func(st *store.Store) error {
		token, hash := secret.NewToken("omw_")
		k, err := st.NewAPIKey(ctx, store.APIKey{
			ProjectID: projectID, Name: name, Prefix: token[:10], Permission: permission, Policy: policy,
		}, hash)
		if err != nil {
			return fmt.Errorf("create key (is %s a project id?): %w", projectID, err)
		}
		fmt.Printf("api key     %s\nid          %s\npermission  %s\npolicy      %s\n\nThe key is shown once. Store it now.\n",
			token, k.ID, k.Permission, policyJSON(k.Policy))
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
	if err := st.CreateAPIKey(ctx, projectID, name, token[:10], hash); err != nil {
		return "", err
	}
	return token, nil
}

func workersCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: openmailwire workers add|list|drain|resume …")
	}
	return withStore(ctx, func(st *store.Store) error {
		switch {
		case args[0] == "add" && len(args) == 2:
			token, hash := secret.NewToken("omwk_")
			if _, err := st.CreateWorker(ctx, args[1], hash); err != nil {
				if errors.Is(err, store.ErrConflict) {
					return fmt.Errorf("a worker named %q already exists", args[1])
				}
				return err
			}
			fmt.Printf("worker   %s\ntoken    %s\n\nSet it as OMW_WORKER_TOKEN on that worker. It is shown once.\n", args[1], token)
		case args[0] == "list":
			ws, err := st.ListWorkers(ctx)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tIP\tEHLO\tCAPACITY\tDRAINING\tLAST SEEN\tVERSION")
			for _, w := range ws {
				seen := "never"
				if w.LastSeenAt != nil {
					seen = time.Since(*w.LastSeenAt).Round(time.Second).String() + " ago"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%v\t%s\t%s\n", w.Name, w.Kind, w.IP, w.EHLO, w.Capacity, w.Draining, seen, w.Version)
			}
			return tw.Flush()
		case (args[0] == "drain" || args[0] == "resume") && len(args) == 2:
			if err := st.SetDraining(ctx, args[1], args[0] == "drain"); err != nil {
				return err
			}
			fmt.Printf("worker %s: draining=%v\n", args[1], args[0] == "drain")
		default:
			return errors.New("usage: openmailwire workers add <name> | list | drain <name> | resume <name>")
		}
		return nil
	})
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
	if len(args) != 2 || args[0] != "force-verify" {
		return errors.New("usage: openmailwire domains force-verify <domain>")
	}
	return withStore(ctx, func(st *store.Store) error {
		if err := st.ForceVerify(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("%s marked verified without a DNS check. Receivers will still check DNS.\n", args[1])
		return nil
	})
}
