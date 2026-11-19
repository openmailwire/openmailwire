package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/openmailwire/openmailwire/internal/policy"
	"github.com/openmailwire/openmailwire/internal/protocol"
)

const (
	// DefaultDomainConcurrency is OMW_DOMAIN_CONCURRENCY's default.
	DefaultDomainConcurrency = 10
	// DefaultRelayAfterAttempts is OMW_RELAY_AFTER_ATTEMPTS's default.
	DefaultRelayAfterAttempts = 2
	// RelayLiveWindow: a worker counts as live while its last heartbeat is
	// this recent.
	RelayLiveWindow = 2 * time.Minute
	// ThrottleConfirmWindow: a throttling deferral halves a limit only when
	// another one, from a different recipient, came within this window.
	ThrottleConfirmWindow = 15 * time.Minute

	leaseLockKey = 7461230005 // serializes leases so per-domain limits hold across workers

	// A lease first reads this many due deliveries from the head of the
	// queue (oldest first). Only when they are all held up (their domains at
	// their limit or in backoff) does it go on domain by domain, visiting at
	// most leaseDomains domains, so its cost never grows with the backlog.
	leaseHead    = 1000
	leaseDomains = 1000
)

// LeaseOptions describes one lease request.
type LeaseOptions struct {
	WorkerID string
	Kind     string // protocol.KindDirect (or empty) or protocol.KindRelay
	Max      int
	Lease    time.Duration
	// DomainConcurrency caps in-flight deliveries per recipient domain
	// across workers of one kind (OMW_DOMAIN_CONCURRENCY). It is also the
	// per-IP cap of a provider the rules give no concurrency.
	DomainConcurrency int
	// RelayAfterAttempts routes a delivery to relay workers once it has had
	// this many attempts, greylisting not counted (OMW_RELAY_AFTER_ATTEMPTS).
	RelayAfterAttempts int
	// Rules group recipient domains by mailbox provider; nil means the
	// built-in rules.
	Rules *policy.Rules
	// Warmup applies the rules' warm-up caps to sending IPs that are not
	// warm yet (OMW_WARMUP).
	Warmup bool
}

func (o *LeaseOptions) defaults() {
	if o.Kind == "" {
		o.Kind = protocol.KindDirect
	}
	if o.DomainConcurrency < 1 {
		o.DomainConcurrency = DefaultDomainConcurrency
	}
	if o.RelayAfterAttempts < 1 {
		o.RelayAfterAttempts = DefaultRelayAfterAttempts
	}
	if o.Rules == nil {
		o.Rules = policy.DefaultRules()
	}
}

// leaseHook, when set (tests only), runs after a lease has chosen its
// deliveries and before it locks them: the window in which a cancel or an
// expiry can commit.
var leaseHook func()

// LeaseJobs hands up to o.Max due deliveries to a worker. Each gets a fresh
// attempt ID and an expiring lease.
//
// Limits, for direct workers:
//   - in-flight deliveries per recipient domain, across direct workers:
//     o.DomainConcurrency;
//   - in-flight deliveries per scope (mailbox provider, or recipient domain)
//     and sending IP: the rules' concurrency, lowered by throttling
//     (receiver_state); a scope in backoff for the IP is skipped;
//   - a sending IP still warming up gets at most the day's warm-up cap of
//     attempts per provider; the rest waits, or goes to a relay;
//   - while a relay worker is live, deliveries with at least
//     o.RelayAfterAttempts attempts (greylisting not counted) are left to it.
//
// Relay workers get only deliveries with that many attempts, or whose scope
// no live direct worker can take now (backoff or warm-up cap on every
// direct IP), up to o.DomainConcurrency in flight per domain through relays.
//
// Leases are serialized with an advisory lock, so two workers leasing at once
// cannot both fill the same domain's last slot. The chosen deliveries are
// re-checked when they are locked, so a cancel or an expiry that commits in
// between is never overwritten.
func (s *Store) LeaseJobs(ctx context.Context, o LeaseOptions) ([]LeasedJob, error) {
	o.defaults()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(leaseLockKey)); err != nil {
		return nil, err
	}
	// The kind a worker leases as is the kind its reports count as, even if
	// its heartbeat has not landed yet.
	var ip, kind string
	if err := tx.QueryRow(ctx, `SELECT ip, kind FROM workers WHERE id = $1`, o.WorkerID).Scan(&ip, &kind); err != nil {
		return nil, noRows(err)
	}
	if kind != o.Kind {
		if _, err := tx.Exec(ctx, `UPDATE workers SET kind = $2 WHERE id = $1`, o.WorkerID, o.Kind); err != nil {
			return nil, err
		}
	}
	st, err := loadLeaseState(ctx, tx, o, ip)
	if err != nil {
		return nil, err
	}

	head, read, err := scanHead(ctx, tx)
	if err != nil {
		return nil, err
	}
	picked := st.choose(head, o.Max)
	usedCursor := false
	if len(picked) < o.Max && read == leaseHead {
		// The head of the queue is held up. Go on domain by domain from
		// where the last such lease stopped, at most twice leaseDomains
		// domains.
		usedCursor = true
		for pass := 0; pass < 2 && len(picked) < o.Max; pass++ {
			more, all, err := st.scanDomains(ctx, tx, o.Max-len(picked))
			if err != nil {
				return nil, err
			}
			picked = append(picked, st.choose(more, o.Max-len(picked))...)
			if all {
				break
			}
		}
	}
	if leaseHook != nil {
		leaseHook()
	}
	jobs, err := lockAndLease(ctx, tx, o, picked)
	if err != nil {
		return nil, err
	}
	if err := st.recordWarmup(ctx, tx, jobs); err != nil {
		return nil, err
	}
	if usedCursor {
		if _, err := tx.Exec(ctx, `INSERT INTO lease_cursor (kind, rcpt_domain) VALUES ($1, $2)
			ON CONFLICT (kind) DO UPDATE SET rcpt_domain = EXCLUDED.rcpt_domain`, o.Kind, st.cursor); err != nil {
			return nil, err
		}
	}
	return jobs, tx.Commit(ctx)
}

// candidate is a due delivery a lease may take.
type candidate struct {
	id, domain, scope   string
	attempts, greylists int
}

// counted is the number of attempts that count toward the relay hand-over.
func (c candidate) counted() int { return c.attempts - c.greylists }

// scopeKey names a scope as seen from one sending IP.
func scopeKey(scope, ip string) string { return scope + "\x00" + ip }

type throttleRow struct {
	limit   int
	backoff bool
}

type warmIP struct {
	warm bool
	day  int // 1 on the first day of warm-up
}

// leaseState is what one lease needs to know about limits, loaded once.
type leaseState struct {
	o           LeaseOptions
	ip          string                 // the leasing worker's sending IP
	relayLive   bool                   // a relay worker is live
	directIPs   []string               // sending IPs of live direct workers (for relays)
	domainUsed  map[string]int         // in flight per recipient domain, this worker kind
	scopeUsed   map[string]int         // in flight per scopeKey, direct workers
	throttle    map[string]throttleRow // receiver_state per scopeKey
	ips         map[string]warmIP      // warm-up state per sending IP
	warmupUsed  map[string]int         // attempts today per scopeKey
	seen        map[string]bool        // deliveries already considered
	pickedScope map[string]string      // delivery id -> scope, for warm-up accounting
	cursor      string                 // where domain-by-domain leasing goes on
}

func loadLeaseState(ctx context.Context, tx pgx.Tx, o LeaseOptions, ip string) (*leaseState, error) {
	st := &leaseState{
		o: o, ip: ip, domainUsed: map[string]int{}, scopeUsed: map[string]int{}, throttle: map[string]throttleRow{},
		ips: map[string]warmIP{}, warmupUsed: map[string]int{}, seen: map[string]bool{}, pickedScope: map[string]string{},
	}
	rows, err := tx.Query(ctx, `SELECT ip, kind FROM workers
		WHERE NOT draining AND last_seen_at > now() - make_interval(secs => $1)`, RelayLiveWindow.Seconds())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var wip, kind string
		if err := rows.Scan(&wip, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == protocol.KindRelay {
			st.relayLive = true
		} else if !contains(st.directIPs, wip) {
			st.directIPs = append(st.directIPs, wip)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// In flight: per recipient domain for this worker kind, and per scope
	// and IP for direct workers. The in-flight set is small (bounded by the
	// workers' concurrency), so this is cheap.
	rows, err = tx.Query(ctx, `SELECT x.rcpt_domain, COALESCE(NULLIF(p.provider, ''), x.rcpt_domain),
			COALESCE(w.kind, 'direct'), COALESCE(w.ip, ''), count(*)
		FROM deliveries x
		LEFT JOIN workers w ON w.id = x.lease_worker
		LEFT JOIN rcpt_domain_providers p ON p.rcpt_domain = x.rcpt_domain
		WHERE x.status = 'sending'
		GROUP BY 1, 2, 3, 4`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var domain, scope, kind, wip string
		var n int
		if err := rows.Scan(&domain, &scope, &kind, &wip, &n); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == o.Kind {
			st.domainUsed[domain] += n
		}
		if kind == protocol.KindDirect {
			st.scopeUsed[scopeKey(scope, wip)] += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.Query(ctx, `SELECT scope, source_ip, "limit", COALESCE(backoff_until > now(), false) FROM receiver_state`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var scope, sip string
		var t throttleRow
		if err := rows.Scan(&scope, &sip, &t.limit, &t.backoff); err != nil {
			rows.Close()
			return nil, err
		}
		st.throttle[scopeKey(scope, sip)] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Warm-up: the leasing IP for a direct worker, every live direct IP for
	// a relay (which takes what they cannot).
	if o.Warmup {
		ips := st.directIPs
		if o.Kind == protocol.KindDirect {
			ips = nil
			if ip != "" {
				ips = []string{ip}
				if _, err := tx.Exec(ctx, `INSERT INTO sending_ips (ip) VALUES ($1) ON CONFLICT DO NOTHING`, ip); err != nil {
					return nil, err
				}
			}
		}
		if err := st.loadWarmup(ctx, tx, ips); err != nil {
			return nil, err
		}
	}

	if err := tx.QueryRow(ctx, `SELECT rcpt_domain FROM lease_cursor WHERE kind = $1`, o.Kind).Scan(&st.cursor); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return st, nil
}

func (st *leaseState) loadWarmup(ctx context.Context, tx pgx.Tx, ips []string) error {
	var nonEmpty []string
	for _, ip := range ips {
		if ip != "" {
			nonEmpty = append(nonEmpty, ip)
		}
	}
	if len(nonEmpty) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT ip, warm_at IS NOT NULL,
			((now() AT TIME ZONE 'UTC')::date - warmup_started) + 1
		FROM sending_ips WHERE ip = ANY($1)`, nonEmpty)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ip string
		var w warmIP
		if err := rows.Scan(&ip, &w.warm, &w.day); err != nil {
			rows.Close()
			return err
		}
		st.ips[ip] = w
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT ip, provider, sent FROM warmup_usage
		WHERE ip = ANY($1) AND day = (now() AT TIME ZONE 'UTC')::date`, nonEmpty)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ip, provider string
		var n int
		if err := rows.Scan(&ip, &provider, &n); err != nil {
			return err
		}
		st.warmupUsed[scopeKey(provider, ip)] = n
	}
	return rows.Err()
}

// warmupLeft is how many more attempts ip may make to scope today, or -1
// when no warm-up cap applies.
func (st *leaseState) warmupLeft(scope, ip string) int {
	if !st.o.Warmup || ip == "" {
		return -1
	}
	w, ok := st.ips[ip]
	if !ok || w.warm {
		return -1
	}
	limit, capped := st.o.Rules.WarmupCap(scope, w.day)
	if !capped {
		return -1
	}
	return max(0, limit-st.warmupUsed[scopeKey(scope, ip)])
}

// directBlocked reports whether a direct worker with this IP cannot take
// scope now: backoff, or the day's warm-up cap reached.
func (st *leaseState) directBlocked(scope, ip string) bool {
	if st.throttle[scopeKey(scope, ip)].backoff {
		return true
	}
	return st.warmupLeft(scope, ip) == 0
}

// noDirectCanTake reports whether every live direct worker is blocked for
// scope, which routes its deliveries to relays. Without live direct
// workers, nothing is routed this way.
func (st *leaseState) noDirectCanTake(scope string) bool {
	if len(st.directIPs) == 0 {
		return false
	}
	for _, ip := range st.directIPs {
		if !st.directBlocked(scope, ip) {
			return false
		}
	}
	return true
}

// scopeLimit is the in-flight cap for scope from one sending IP.
func (st *leaseState) scopeLimit(scope, ip string) int {
	limit := st.o.Rules.Concurrency(scope, st.o.DomainConcurrency)
	if t, ok := st.throttle[scopeKey(scope, ip)]; ok && t.limit < limit {
		limit = t.limit
	}
	return max(1, limit)
}

// choose picks up to n of cands, in order, that the limits allow, and
// counts them as in flight.
func (st *leaseState) choose(cands []candidate, n int) []candidate {
	var out []candidate
	for _, c := range cands {
		if len(out) >= n {
			break
		}
		if st.seen[c.id] {
			continue
		}
		st.seen[c.id] = true
		if st.domainUsed[c.domain] >= st.o.DomainConcurrency {
			continue
		}
		key := scopeKey(c.scope, st.ip)
		if st.o.Kind == protocol.KindRelay {
			if c.counted() < st.o.RelayAfterAttempts && !st.noDirectCanTake(c.scope) {
				continue
			}
		} else {
			if st.relayLive && c.counted() >= st.o.RelayAfterAttempts {
				continue
			}
			if st.throttle[key].backoff || st.scopeUsed[key] >= st.scopeLimit(c.scope, st.ip) {
				continue
			}
			left := st.warmupLeft(c.scope, st.ip)
			if left == 0 {
				continue // overflow waits for tomorrow, or for a relay
			}
			st.scopeUsed[key]++
			if left > 0 {
				st.warmupUsed[key]++
				st.pickedScope[c.id] = c.scope
			}
		}
		st.domainUsed[c.domain]++
		out = append(out, c)
	}
	return out
}

// blockedFilters lists the domains and scopes no delivery can be taken for
// now, so the domain-by-domain scan skips them without reading their rows.
// For a relay, relayScopes are the scopes no direct worker can take now,
// whose deliveries it takes whatever their attempt count.
func (st *leaseState) blockedFilters() (domains, scopes, relayScopes []string) {
	domains, scopes, relayScopes = []string{}, []string{}, []string{}
	for d, n := range st.domainUsed {
		if n >= st.o.DomainConcurrency {
			domains = append(domains, d)
		}
	}
	seen := map[string]bool{}
	add := func(scope string) {
		if !seen[scope] {
			seen[scope] = true
			scopes = append(scopes, scope)
		}
	}
	if st.o.Kind == protocol.KindRelay {
		for key := range st.throttle {
			scope, _, _ := strings.Cut(key, "\x00")
			if !seen[scope] && st.noDirectCanTake(scope) {
				seen[scope] = true
				relayScopes = append(relayScopes, scope)
			}
		}
		for _, p := range st.o.Rules.Providers {
			if !seen[p.Name] && st.noDirectCanTake(p.Name) {
				seen[p.Name] = true
				relayScopes = append(relayScopes, p.Name)
			}
		}
		return domains, scopes, relayScopes
	}
	for key := range st.scopeUsed {
		scope, ip, _ := strings.Cut(key, "\x00")
		if ip == st.ip && st.scopeUsed[key] >= st.scopeLimit(scope, ip) {
			add(scope)
		}
	}
	for key, t := range st.throttle {
		scope, ip, _ := strings.Cut(key, "\x00")
		if ip == st.ip && t.backoff {
			add(scope)
		}
	}
	for _, p := range st.o.Rules.Providers {
		if st.warmupLeft(p.Name, st.ip) == 0 {
			add(p.Name)
		}
	}
	return domains, scopes, relayScopes
}

const candidateCols = `d.id::text, d.rcpt_domain, COALESCE(NULLIF(p.provider, ''), d.rcpt_domain), d.attempts, d.greylists`

// scanHead reads the oldest due deliveries. The LIMIT comes before any
// filter on limits, so this reads at most leaseHead rows however long the
// queue is. read is how many rows it read, expired ones included.
func scanHead(ctx context.Context, tx pgx.Tx) (cands []candidate, read int, err error) {
	rows, err := tx.Query(ctx, `SELECT `+candidateCols+`, e.expires_at > now() AND e.raw IS NOT NULL
		FROM (SELECT id, rcpt_domain, attempts, greylists, next_attempt_at, email_id FROM deliveries
		      WHERE status IN ('queued', 'deferred') AND next_attempt_at <= now()
		      ORDER BY next_attempt_at LIMIT $1) d
		JOIN emails e ON e.id = d.email_id
		LEFT JOIN rcpt_domain_providers p ON p.rcpt_domain = d.rcpt_domain
		ORDER BY d.next_attempt_at`, leaseHead)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var c candidate
		var live bool
		if err := rows.Scan(&c.id, &c.domain, &c.scope, &c.attempts, &c.greylists, &live); err != nil {
			return nil, 0, err
		}
		read++
		if live { // expired or purged ones wait for the sweep
			cands = append(cands, c)
		}
	}
	return cands, read, rows.Err()
}

// scanDomains visits recipient domains with queued or deferred deliveries
// in name order, starting after st.cursor (a skip scan over
// deliveries_domain_due: one index probe per domain), and returns the due
// deliveries of domains that are not blocked, up to want per domain, at most
// leaseDomains domains. It moves st.cursor on; all reports that the scan
// started at the first domain and reached the last.
func (st *leaseState) scanDomains(ctx context.Context, tx pgx.Tx, want int) (cands []candidate, all bool, err error) {
	domains, scopes, relayScopes := st.blockedFilters()
	perDomain := min(want, st.o.DomainConcurrency)
	relay := st.o.Kind == protocol.KindRelay
	start := st.cursor
	// Every visited domain gives at least one row (LEFT JOIN), so the outer
	// LIMIT also stops the domain walk once enough deliveries were found.
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE doms(dom) AS (
			(SELECT rcpt_domain FROM deliveries
			 WHERE status IN ('queued', 'deferred') AND rcpt_domain > $1
			 ORDER BY rcpt_domain LIMIT 1)
			UNION ALL
			SELECT (SELECT x.rcpt_domain FROM deliveries x
			        WHERE x.status IN ('queued', 'deferred') AND x.rcpt_domain > doms.dom
			        ORDER BY x.rcpt_domain LIMIT 1)
			FROM doms WHERE doms.dom IS NOT NULL
		)
		SELECT v.dom, `+candidateCols+`
		FROM (SELECT dom FROM doms WHERE dom IS NOT NULL LIMIT $2) v
		LEFT JOIN rcpt_domain_providers p ON p.rcpt_domain = v.dom
		LEFT JOIN LATERAL (
			SELECT x.id, x.rcpt_domain, x.attempts, x.greylists, x.next_attempt_at
			FROM deliveries x JOIN emails e ON e.id = x.email_id
			-- Row comparisons rather than rcpt_domain = v.dom: only
			-- deliveries_domain_due gives this order, so the planner reads just
			-- this domain's due rows, however skewed its statistics.
			WHERE (x.rcpt_domain, x.next_attempt_at) >= (v.dom, '-infinity'::timestamptz)
			  AND (x.rcpt_domain, x.next_attempt_at) <= (v.dom, now())
			  AND x.status IN ('queued', 'deferred')
			  AND e.expires_at > now() AND e.raw IS NOT NULL
			  AND v.dom <> ALL($3) AND COALESCE(NULLIF(p.provider, ''), v.dom) <> ALL($4)
			  AND (NOT $6 OR x.attempts - x.greylists >= $7 OR COALESCE(NULLIF(p.provider, ''), v.dom) = ANY($8))
			ORDER BY x.rcpt_domain, x.next_attempt_at LIMIT $5
		) d ON true
		LIMIT $9`,
		start, leaseDomains, domains, scopes, perDomain, relay, st.o.RelayAfterAttempts, relayScopes,
		leaseDomains+4*want)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	n, last := 0, ""
	for rows.Next() {
		var dom string
		var id, domain, scope *string
		var attempts, greylists *int
		if err := rows.Scan(&dom, &id, &domain, &scope, &attempts, &greylists); err != nil {
			return nil, false, err
		}
		if dom != last {
			n++
			last = dom
		}
		if id != nil {
			cands = append(cands, candidate{id: *id, domain: *domain, scope: *scope, attempts: *attempts, greylists: *greylists})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if n < leaseDomains && len(cands) < 4*want {
		// Reached the last domain: the next scan starts from the first.
		st.cursor = ""
		return cands, start == "", nil
	}
	st.cursor = last
	return cands, false, nil
}

// lockAndLease locks the chosen deliveries and leases them. The status and
// due time are checked again on the locked rows: a cancel or an expiry that
// committed after the candidates were read wins, and the delivery is left
// out.
func lockAndLease(ctx context.Context, tx pgx.Tx, o LeaseOptions, picked []candidate) ([]LeasedJob, error) {
	if len(picked) == 0 {
		return nil, nil
	}
	ids := make([]string, len(picked))
	for i, c := range picked {
		ids[i] = c.id
	}
	rows, err := tx.Query(ctx, `
		WITH locked AS (
			SELECT d.id FROM deliveries d JOIN emails e ON e.id = d.email_id
			WHERE d.id = ANY($1::uuid[])
			  AND d.status IN ('queued', 'deferred') AND d.next_attempt_at <= now()
			  AND e.expires_at > now() AND e.raw IS NOT NULL
			FOR UPDATE OF d SKIP LOCKED
		), leased AS (
			UPDATE deliveries d SET status = 'sending', attempts = d.attempts + 1, lease_worker = $2,
				lease_attempt = gen_random_uuid(), lease_expires_at = now() + make_interval(secs => $3),
				updated_at = now()
			FROM locked l WHERE d.id = l.id AND d.status IN ('queued', 'deferred')
			RETURNING d.id, d.email_id, d.rcpt, d.rcpt_domain, d.lease_attempt, d.lease_expires_at
		), started AS (
			INSERT INTO attempts (id, delivery_id, worker_id)
			SELECT lease_attempt, id, $2 FROM leased
		)
		SELECT l.id::text, l.rcpt, l.rcpt_domain, l.lease_attempt::text, l.lease_expires_at, e.raw, e.return_path,
			COALESCE(e.domain_id::text, ''), e.scheduled_at IS NOT NULL
		FROM leased l JOIN emails e ON e.id = l.email_id`,
		ids, o.WorkerID, o.Lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []LeasedJob
	for rows.Next() {
		var j LeasedJob
		if err := rows.Scan(&j.DeliveryID, &j.Rcpt, &j.RcptDomain, &j.AttemptID, &j.LeaseExpiresAt, &j.SealedRaw,
			&j.ReturnPath, &j.DomainID, &j.Scheduled); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// recordWarmup counts the leased attempts against the day's warm-up caps.
func (st *leaseState) recordWarmup(ctx context.Context, tx pgx.Tx, jobs []LeasedJob) error {
	if len(st.pickedScope) == 0 {
		return nil
	}
	n := map[string]int{}
	for _, j := range jobs {
		if scope, ok := st.pickedScope[j.DeliveryID]; ok {
			n[scope]++
		}
	}
	for scope, k := range n {
		if _, err := tx.Exec(ctx, `INSERT INTO warmup_usage (ip, provider, day, sent)
			VALUES ($1, $2, (now() AT TIME ZONE 'UTC')::date, $3)
			ON CONFLICT (ip, provider, day) DO UPDATE SET sent = warmup_usage.sent + EXCLUDED.sent`,
			st.ip, scope, k); err != nil {
			return err
		}
	}
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// ReleaseLease gives back a leased delivery that was never attempted (the
// control plane could not unseal its message). The attempt is removed, the
// delivery waits retryIn before it is due again, and an unseal_failed event
// is recorded. Without this the lease would expire into an "unknown"
// attempt, which may end the delivery although nothing was sent.
func (s *Store) ReleaseLease(ctx context.Context, attemptID, reason string, retryIn time.Duration) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var deliveryID, emailID string
	err = tx.QueryRow(ctx, `SELECT id::text, email_id::text FROM deliveries
		WHERE lease_attempt = $1 AND status = 'sending' FOR UPDATE`, attemptID).Scan(&deliveryID, &emailID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Never attempted means queued again when it had no earlier attempts,
	// so it stays cancelable; otherwise deferred.
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET attempts = attempts - 1,
			status = CASE WHEN attempts - 1 > 0 THEN 'deferred' ELSE 'queued' END,
			next_attempt_at = now() + make_interval(secs => $2), lease_worker = NULL, lease_attempt = NULL,
			lease_expires_at = NULL, last_response = $3, updated_at = now()
		WHERE id = $1`, deliveryID, retryIn.Seconds(), protocol.CleanText(reason, 1000)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM attempts WHERE id = $1`, attemptID); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"reason": protocol.CleanText(reason, 300)})
	if _, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type, data) VALUES ($1, $2, 'unseal_failed', $3)`,
		emailID, deliveryID, data); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReceiverState is what the control plane has learned about one scope (a
// mailbox provider, or a recipient domain of no known provider) as seen from
// one sending IP, from throttling deferrals.
type ReceiverState struct {
	Scope        string     `json:"scope"`
	SourceIP     string     `json:"source_ip"`
	Limit        int        `json:"limit"`
	BackoffUntil *time.Time `json:"backoff_until"`
	Throttles    int        `json:"throttles"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// GetReceiverState returns the learned state of a scope, for the sending IP
// that updated it last, or ErrNotFound when the scope never throttled us.
func (s *Store) GetReceiverState(ctx context.Context, scope string) (ReceiverState, error) {
	var r ReceiverState
	err := s.Pool.QueryRow(ctx, `SELECT scope, source_ip, "limit", backoff_until, throttles, updated_at
		FROM receiver_state WHERE scope = $1 ORDER BY updated_at DESC LIMIT 1`, scope).
		Scan(&r.Scope, &r.SourceIP, &r.Limit, &r.BackoffUntil, &r.Throttles, &r.UpdatedAt)
	return r, noRows(err)
}

// attemptRef names the attempt whose report changes a receiver's state.
type attemptRef struct {
	emailID, deliveryID, rcpt string
	scope, ip                 string
	startedAt                 time.Time
}

func rcptHash(rcpt string) string {
	h := sha256.Sum256([]byte(strings.ToLower(rcpt)))
	return hex.EncodeToString(h[:12])
}

// throttle handles a throttling deferral from a direct worker. The first
// one only waits (receiver_state.pending_rcpt): one recipient's mailbox can
// look like a throttle, and must not slow the provider down for everyone.
// When another one comes from a different recipient within
// ThrottleConfirmWindow, the scope's limit for that IP halves (minimum 1)
// and direct workers with that IP leave it alone for
// policy.ThrottleBackoff(n), n being the consecutive throttles so far.
//
// Deferrals from attempts that were already in flight when the scope last
// throttled us are the same burst, so they do not halve the limit again.
func throttle(ctx context.Context, tx pgx.Tx, at attemptRef, ceiling int) error {
	if _, err := tx.Exec(ctx, `INSERT INTO receiver_state (scope, source_ip, "limit") VALUES ($1, $2, $3)
		ON CONFLICT (scope, source_ip) DO NOTHING`, at.scope, at.ip, ceiling); err != nil {
		return err
	}
	var limit, throttles int
	var last, pendingAt *time.Time
	var pending *string
	if err := tx.QueryRow(ctx, `SELECT "limit", throttles, last_throttled_at, pending_rcpt, pending_at FROM receiver_state
		WHERE scope = $1 AND source_ip = $2 FOR UPDATE`, at.scope, at.ip).Scan(&limit, &throttles, &last, &pending, &pendingAt); err != nil {
		return err
	}
	if last != nil && at.startedAt.Before(*last) {
		return nil
	}
	h := rcptHash(at.rcpt)
	confirmed := pending != nil && *pending != h && pendingAt != nil && time.Since(*pendingAt) < ThrottleConfirmWindow
	if !confirmed {
		_, err := tx.Exec(ctx, `UPDATE receiver_state SET pending_rcpt = $3, pending_at = now(), updated_at = now()
			WHERE scope = $1 AND source_ip = $2`, at.scope, at.ip, h)
		return err
	}
	prev := min(limit, ceiling)
	next := max1(prev / 2)
	n := throttles + 1
	backoff := policy.ThrottleBackoff(n)
	var until time.Time
	if err := tx.QueryRow(ctx, `UPDATE receiver_state SET "limit" = $3, throttles = $4,
			backoff_until = now() + make_interval(secs => $5), last_throttled_at = now(),
			pending_rcpt = NULL, pending_at = NULL, updated_at = now()
		WHERE scope = $1 AND source_ip = $2 RETURNING backoff_until`, at.scope, at.ip, next, n, backoff.Seconds()).Scan(&until); err != nil {
		return err
	}
	data := map[string]any{
		"scope": at.scope, "limit": next, "previous_limit": prev,
		"backoff_until": until, "backoff_seconds": int(backoff.Seconds()),
	}
	if at.ip != "" {
		data["source_ip"] = at.ip
	}
	return receiverEvent(ctx, tx, at, "throttled", data)
}

// relax handles a delivery by a direct worker to a scope that throttled us
// before: its limit for that IP grows by 1, up to ceiling, and the run of
// consecutive throttles ends. Deliveries already in flight when the scope
// last throttled us do not count; they were not sent under the lower limit.
func relax(ctx context.Context, tx pgx.Tx, at attemptRef, ceiling int) error {
	var limit, throttles int
	var last *time.Time
	// Most deliveries go to scopes that never throttled us or have
	// recovered: look without a lock first, so their reports do not queue
	// on one row.
	err := tx.QueryRow(ctx, `SELECT "limit", throttles FROM receiver_state
		WHERE scope = $1 AND source_ip = $2`, at.scope, at.ip).Scan(&limit, &throttles)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && limit >= ceiling && throttles == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT "limit", throttles, last_throttled_at FROM receiver_state
		WHERE scope = $1 AND source_ip = $2 FOR UPDATE`, at.scope, at.ip).Scan(&limit, &throttles, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if last != nil && at.startedAt.Before(*last) {
		return nil
	}
	next := max1(min(limit+1, ceiling))
	if next == limit && throttles == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE receiver_state SET "limit" = $3, throttles = 0, updated_at = now()
		WHERE scope = $1 AND source_ip = $2`, at.scope, at.ip, next); err != nil {
		return err
	}
	if next <= limit {
		return nil
	}
	data := map[string]any{"scope": at.scope, "limit": next, "previous_limit": limit}
	if at.ip != "" {
		data["source_ip"] = at.ip
	}
	return receiverEvent(ctx, tx, at, "throttle_relaxed", data)
}

func receiverEvent(ctx context.Context, tx pgx.Tx, at attemptRef, typ string, data map[string]any) error {
	b, _ := json.Marshal(data)
	_, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type, data) VALUES ($1, $2, $3, $4)`,
		at.emailID, at.deliveryID, typ, b)
	return err
}

// learnProvider records which provider a recipient domain's MX belongs to,
// from a direct worker's report, and returns the domain's scope.
func learnProvider(ctx context.Context, tx pgx.Tx, rules *policy.Rules, domain, mxHost string) (string, error) {
	var provider string
	var fresh bool
	err := tx.QueryRow(ctx, `SELECT provider, updated_at > now() - interval '1 day' FROM rcpt_domain_providers
		WHERE rcpt_domain = $1`, domain).Scan(&provider, &fresh)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if mxHost != "" {
		learned := rules.ProviderForMX(mxHost)
		// Written only when new, changed or a day old, so reports to a busy
		// domain do not all lock its row.
		if errors.Is(err, pgx.ErrNoRows) || learned != provider || !fresh {
			if _, err := tx.Exec(ctx, `INSERT INTO rcpt_domain_providers (rcpt_domain, provider, mx_host) VALUES ($1, $2, $3)
				ON CONFLICT (rcpt_domain) DO UPDATE SET provider = EXCLUDED.provider, mx_host = EXCLUDED.mx_host, updated_at = now()`,
				domain, learned, protocol.CleanText(mxHost, 255)); err != nil {
				return "", err
			}
		}
		provider = learned
	}
	if provider == "" {
		return domain, nil
	}
	return provider, nil
}

// recordAuthIssue notes an authentication problem a receiver reported for
// the email's sending domain, on the project and on the email's timeline.
func recordAuthIssue(ctx context.Context, tx pgx.Tx, at attemptRef, projectID, fromAddr, enhanced, response string) error {
	sending := strings.ToLower(domainOf(fromAddr))
	if _, err := tx.Exec(ctx, `INSERT INTO sender_auth_issues (project_id, sending_domain, scope, enhanced_code, last_response)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project_id, sending_domain, scope, enhanced_code) DO UPDATE
		SET count = sender_auth_issues.count + 1, last_seen_at = now(), last_response = EXCLUDED.last_response`,
		projectID, sending, at.scope, enhanced, protocol.CleanText(response, 300)); err != nil {
		return err
	}
	return receiverEvent(ctx, tx, at, "auth_problem", map[string]any{
		"sending_domain": sending, "scope": at.scope, "enhanced_code": enhanced,
	})
}

// AuthIssue is an authentication problem receivers reported about one
// sending domain.
type AuthIssue struct {
	SendingDomain string    `json:"sending_domain"`
	Scope         string    `json:"scope"`
	EnhancedCode  string    `json:"enhanced_code"`
	Count         int       `json:"count"`
	FirstSeenAt   time.Time `json:"first_seen_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	LastResponse  string    `json:"last_response"`
}

// AuthIssues lists a project's authentication problems for one sending
// domain, most recent first.
func (s *Store) AuthIssues(ctx context.Context, projectID, sendingDomain string) ([]AuthIssue, error) {
	rows, err := s.Pool.Query(ctx, `SELECT sending_domain, scope, enhanced_code, count, first_seen_at, last_seen_at, last_response
		FROM sender_auth_issues WHERE project_id = $1 AND sending_domain = $2 ORDER BY last_seen_at DESC LIMIT 50`,
		projectID, sendingDomain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuthIssue{}
	for rows.Next() {
		var a AuthIssue
		if err := rows.Scan(&a.SendingDomain, &a.Scope, &a.EnhancedCode, &a.Count, &a.FirstSeenAt, &a.LastSeenAt, &a.LastResponse); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
