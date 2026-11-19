package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/openmailwire/openmailwire/internal/policy"
)

// WarmupStatus is where one sending IP stands in its warm-up.
type WarmupStatus struct {
	IP      string
	Started bool // the IP has leased at least once since warm-up existed
	Warm    bool // no warm-up caps apply any more
	Day     int  // 1 on the first day
	// Left is today's remaining capacity per provider with a cap; empty
	// once the IP is warm.
	Left []ProviderCapacity
}

// ProviderCapacity is one provider's daily cap for a warming IP.
type ProviderCapacity struct {
	Provider string
	Cap      int
	Used     int
}

// IPWarmup returns the warm-up status of a sending IP under rules.
func (s *Store) IPWarmup(ctx context.Context, rules *policy.Rules, ip string) (WarmupStatus, error) {
	st := WarmupStatus{IP: ip}
	err := s.Pool.QueryRow(ctx, `SELECT warm_at IS NOT NULL, ((now() AT TIME ZONE 'UTC')::date - warmup_started) + 1
		FROM sending_ips WHERE ip = $1`, ip).Scan(&st.Warm, &st.Day)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Started = true
	if st.Warm {
		return st, nil
	}
	used := map[string]int{}
	rows, err := s.Pool.Query(ctx, `SELECT provider, sent FROM warmup_usage
		WHERE ip = $1 AND day = (now() AT TIME ZONE 'UTC')::date`, ip)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			rows.Close()
			return st, err
		}
		used[p] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}
	for _, p := range rules.Providers {
		if c, ok := rules.WarmupCap(p.Name, st.Day); ok {
			st.Left = append(st.Left, ProviderCapacity{Provider: p.Name, Cap: c, Used: used[p.Name]})
		}
	}
	if len(st.Left) == 0 && st.Day > rules.WarmupDays() {
		st.Warm = true
	}
	return st, nil
}

// SetIPWarm marks a sending IP warm (no more warm-up caps), or starts its
// warm-up again from day 1.
func (s *Store) SetIPWarm(ctx context.Context, ip string, warm bool) error {
	if warm {
		_, err := s.Pool.Exec(ctx, `INSERT INTO sending_ips (ip, warm_at) VALUES ($1, now())
			ON CONFLICT (ip) DO UPDATE SET warm_at = now()`, ip)
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO sending_ips (ip) VALUES ($1)
		ON CONFLICT (ip) DO UPDATE SET warm_at = NULL, warmup_started = (now() AT TIME ZONE 'UTC')::date`, ip)
	return err
}
