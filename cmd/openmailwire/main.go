// Command openmailwire runs the OpenMailWire control plane, a delivery worker, or an
// admin task. Run `openmailwire help` for the list.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openmailwire/openmailwire/internal/api"
	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/domains"
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
  openmailwire migrate                        apply database migrations
  openmailwire keygen                         print a new OMW_SECRET_KEY
  openmailwire project create <name>          create a project and its first API key
  openmailwire apikey create <project-id> [name]
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
	log.Info("worker starting", "control", cfg.ControlURL, "ehlo", cfg.EHLO, "public_ip", cfg.PublicIP,
		"concurrency", cfg.Concurrency, "version", version)
	return worker.NewRunner(cfg, log, version).Run(ctx)
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

func apikeyCmd(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "create" {
		return errors.New("usage: openmailwire apikey create <project-id> [name]")
	}
	name := ""
	if len(args) > 2 {
		name = args[2]
	}
	return withStore(ctx, func(st *store.Store) error {
		token, err := newAPIKey(ctx, st, args[1], name)
		if err != nil {
			return err
		}
		fmt.Printf("api key  %s\n\nThe key is shown once. Store it now.\n", token)
		return nil
	})
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
			fmt.Fprintln(tw, "NAME\tIP\tEHLO\tCAPACITY\tDRAINING\tLAST SEEN\tVERSION")
			for _, w := range ws {
				seen := "never"
				if w.LastSeenAt != nil {
					seen = time.Since(*w.LastSeenAt).Round(time.Second).String() + " ago"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%v\t%s\t%s\n", w.Name, w.IP, w.EHLO, w.Capacity, w.Draining, seen, w.Version)
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
