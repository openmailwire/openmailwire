package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

// openSecrets checks the configured keys against the database and returns
// the sealer and the root of the bounce-address (VERP) keys. It refuses to
// start the control plane when the keys don't match the data.
func openSecrets(ctx context.Context, st *store.Store, keys [][]byte, log *slog.Logger) (*secret.Sealer, []byte, error) {
	macRoot, err := st.VerifySecretKeys(ctx, keys)
	var kc *store.KeyCheckError
	if errors.As(err, &kc) {
		return nil, nil, fmt.Errorf("refusing to start: %s", kc.Msg)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("checking OMW_SECRET_KEYS: %w", err)
	}
	sealer, err := secret.NewKeyring(keys)
	if err != nil {
		return nil, nil, err
	}
	ids := sealer.IDs()
	log.Info("secret keys checked", "sealing_key", ids[0], "other_keys", strings.Join(ids[1:], ","))
	return sealer, macRoot, nil
}

const rotateUsage = `usage: openmailwire rotate-secrets [--batch N] [--pause DURATION]

Reseals every sealed value (DKIM keys, webhook secrets, messages) with the
first key of OMW_SECRET_KEYS, online, in small transactions, while the
control plane keeps running. Run it after every control plane runs with the
new OMW_SECRET_KEYS ("new,old"). When it reports that nothing is sealed with
the old keys any more, they may be removed from the list.

  --batch N         rows per transaction (default 100)
  --pause DURATION  pause between transactions (default 50ms)`

func rotateSecrets(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rotate-secrets", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	batch := fs.Int("batch", 100, "")
	pause := fs.Duration("pause", 50*time.Millisecond, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *batch < 1 {
		return errors.New(rotateUsage)
	}
	keys, err := config.SecretKeys()
	if err != nil {
		return err
	}
	return withStore(ctx, func(st *store.Store) error {
		if _, err := st.VerifySecretKeys(ctx, keys); err != nil {
			return err
		}
		sealer, err := secret.NewKeyring(keys)
		if err != nil {
			return err
		}
		fmt.Printf("resealing with key %s (other keys: %s)\n", sealer.PrimaryID(), strings.Join(sealer.IDs()[1:], ", "))
		unreadable := 0
		for pass := 1; pass <= 3; pass++ {
			for _, c := range store.SealedColumns {
				stats, err := st.ResealColumn(ctx, sealer, c, store.ResealOptions{Batch: *batch, Pause: *pause})
				if err != nil {
					return err
				}
				if stats.Resealed > 0 || stats.Unreadable > 0 {
					fmt.Printf("  %-36s resealed %d, unreadable %d\n", c, stats.Resealed, stats.Unreadable)
				}
				if pass == 1 {
					unreadable += stats.Unreadable
				}
			}
			// A control plane still running with the old key list seals new
			// values with the old key; another pass picks those up.
			left := 0
			for _, c := range store.SealedColumns {
				n, err := st.CountUnsealedByPrimary(ctx, sealer, c, 1000)
				if err != nil {
					return err
				}
				left += n
			}
			if left <= unreadable {
				break
			}
			if pass == 3 {
				return fmt.Errorf("%d values are still sealed with an older key; a control plane probably still runs "+
					"with the old OMW_SECRET_KEYS. Update it, then run rotate-secrets again", left-unreadable)
			}
		}
		if unreadable > 0 {
			return fmt.Errorf("%d values open with none of OMW_SECRET_KEYS and were left as they are; "+
				"add the key they were sealed with and run rotate-secrets again", unreadable)
		}
		retired, err := st.RetireSecretKeys(ctx, sealer.PrimaryID())
		if err != nil {
			return err
		}
		// A server-wide entry: the CLI acts without an API key.
		if err := st.AppendAudit(ctx, store.AuditEntry{
			Actor: store.ActorCLI, Action: "secret_keys.rotated", TargetType: "secret_keys",
			Data: map[string]any{"sealing_key": sealer.PrimaryID(), "retired": retired},
		}); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not write the audit log entry:", err)
		}
		if len(retired) > 0 {
			fmt.Printf("done: nothing is sealed with key %s any more. Remove it from OMW_SECRET_KEYS on every control plane.\n",
				strings.Join(retired, ", "))
		} else {
			fmt.Printf("done: every value is sealed with key %s.\n", sealer.PrimaryID())
		}
		return nil
	})
}
