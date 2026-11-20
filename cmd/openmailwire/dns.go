package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/openmailwire/openmailwire/internal/api"
	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/store"
)

const dnsUsage = `usage: openmailwire dns sync [--dry-run]

Publishes the operator's SPF include (OMW_SPF_INCLUDE, e.g. _spf.mail.example.net)
at Cloudflare with OMW_CLOUDFLARE_TOKEN: "v=spf1 ip4:… ip6:… -all" with the IPs
that delivery workers announced in the last 7 days. It writes only when the value
changed, and prints the difference. The control plane does the same every 5
minutes; this runs it once. --dry-run only reads.

The token needs Zone > DNS > Edit for the zone that holds OMW_SPF_INCLUDE.`

func dnsCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "sync" {
		return errors.New(dnsUsage)
	}
	fs := flag.NewFlagSet("dns sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New(dnsUsage)
	}
	include := os.Getenv("OMW_SPF_INCLUDE")
	if include == "" {
		return errors.New("OMW_SPF_INCLUDE is not set: it names the TXT record to keep in sync, e.g. _spf.mail.example.net")
	}
	auto, err := config.LoadDNSAutomation(include)
	if err != nil {
		return err
	}
	if auto.CloudflareToken == "" {
		return fmt.Errorf("OMW_CLOUDFLARE_TOKEN is not set: create a Cloudflare API token with Zone > DNS > Edit for the zone that holds %s", include)
	}
	return withStore(ctx, func(st *store.Store) error {
		srv := &api.Server{Store: st, Cfg: config.Control{SPFInclude: include, DNSAutomation: auto}, Log: slog.New(slog.DiscardHandler)}
		res, err := srv.SyncOperatorSPF(ctx, *dry)
		printSPFSync(os.Stdout, res, *dry)
		return err
	})
}

// printSPFSync prints what a sync did as a small diff.
func printSPFSync(w io.Writer, r api.OperatorSPF, dry bool) {
	fmt.Fprintf(w, "TXT %s", r.Name)
	if r.Zone != "" {
		fmt.Fprintf(w, " (Cloudflare zone %s)", r.Zone)
	}
	fmt.Fprintln(w)
	switch r.Action {
	case "":
		return
	case "skip":
		fmt.Fprintf(w, "nothing changed: %s\n", r.Reason)
		return
	case "unchanged":
		fmt.Fprintf(w, "  %s\n", r.New)
	default:
		for _, o := range r.Old {
			fmt.Fprintf(w, "- %s\n", o)
		}
		fmt.Fprintf(w, "+ %s\n", r.New)
	}
	verb := map[string]string{"create": "created", "update": "updated", "unchanged": "unchanged"}[r.Action]
	if dry && r.Action != "unchanged" {
		verb = "dry run: would be " + verb
	}
	if r.Deleted > 0 {
		verb += fmt.Sprintf(", %d duplicate SPF record(s) deleted", r.Deleted)
		if dry {
			verb = verb[:len(verb)-len("deleted")] + "to delete"
		}
	}
	fmt.Fprintf(w, "%s (%d IPs of delivery workers seen in the last 7 days)\n", verb, len(r.IPs))
}
