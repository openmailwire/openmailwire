package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// DNSAutomation is a domain's use of automatic DNS setup (lane G): the
// zone its records went to, the provider token the project remembered (if
// any), and the last run.
type DNSAutomation struct {
	DomainID  string
	ProjectID string
	Provider  string
	ZoneID    string
	ZoneName  string
	// Token is the remembered API token, sealed; nil when none is.
	Token        []byte
	RememberedAt *time.Time
	// PublishedDKIM are DKIM record names published through the provider,
	// deleted there once their key is retired.
	PublishedDKIM []string
	LastRun       []byte // JSON, never holding the token
	LastRunAt     *time.Time
}

const dnsAutoCols = `a.domain_id::text, d.project_id::text, a.provider, a.zone_id, a.zone_name, a.api_token, a.remembered_at,
	a.published_dkim, a.last_run, a.last_run_at`

func scanDNSAutomation(row pgx.Row) (DNSAutomation, error) {
	var a DNSAutomation
	err := row.Scan(&a.DomainID, &a.ProjectID, &a.Provider, &a.ZoneID, &a.ZoneName, &a.Token, &a.RememberedAt,
		&a.PublishedDKIM, &a.LastRun, &a.LastRunAt)
	return a, err
}

// DNSAutomation returns a domain's row, or ErrNotFound when the domain has
// never used automatic DNS setup.
func (s *Store) DNSAutomation(ctx context.Context, domainID string) (DNSAutomation, error) {
	a, err := scanDNSAutomation(s.Pool.QueryRow(ctx, `SELECT `+dnsAutoCols+`
		FROM dns_automation a JOIN domains d ON d.id = a.domain_id WHERE a.domain_id = $1`, domainID))
	return a, noRows(err)
}

// DNSRun is what SaveDNSRun records.
type DNSRun struct {
	DomainID string
	Provider string
	ZoneID   string
	ZoneName string
	LastRun  []byte // JSON
	// AddDKIM are DKIM record names this run published (or found
	// published), to delete once their key is retired.
	AddDKIM []string
	// Token, when set, is a sealed token to remember from now on, for
	// Provider and the zone.
	Token []byte
}

// SaveDNSRun records a run. The row's provider and zone change only when
// the run remembers a token or none is remembered, so they always describe
// the remembered token.
func (s *Store) SaveDNSRun(ctx context.Context, r DNSRun) error {
	if r.AddDKIM == nil {
		r.AddDKIM = []string{}
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO dns_automation AS a (domain_id, provider, zone_id, zone_name, api_token, remembered_at,
			published_dkim, last_run, last_run_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $5::bytea IS NULL THEN NULL ELSE now() END, $6, $7, now(), now())
		ON CONFLICT (domain_id) DO UPDATE SET
			provider  = CASE WHEN EXCLUDED.api_token IS NOT NULL OR a.api_token IS NULL THEN EXCLUDED.provider ELSE a.provider END,
			zone_id   = CASE WHEN EXCLUDED.api_token IS NOT NULL OR a.api_token IS NULL THEN EXCLUDED.zone_id ELSE a.zone_id END,
			zone_name = CASE WHEN EXCLUDED.api_token IS NOT NULL OR a.api_token IS NULL THEN EXCLUDED.zone_name ELSE a.zone_name END,
			remembered_at = CASE WHEN EXCLUDED.api_token IS NOT NULL THEN now() ELSE a.remembered_at END,
			api_token = COALESCE(EXCLUDED.api_token, a.api_token),
			published_dkim = ARRAY(SELECT DISTINCT n FROM unnest(a.published_dkim || EXCLUDED.published_dkim) n ORDER BY n),
			last_run = EXCLUDED.last_run, last_run_at = now(), updated_at = now()`,
		r.DomainID, r.Provider, r.ZoneID, r.ZoneName, r.Token, r.AddDKIM, r.LastRun)
	return err
}

// ForgetDNSToken removes a domain's remembered token. It reports whether
// there was one.
func (s *Store) ForgetDNSToken(ctx context.Context, domainID string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE dns_automation SET api_token = NULL, remembered_at = NULL, updated_at = now()
		WHERE domain_id = $1 AND api_token IS NOT NULL`, domainID)
	return tag.RowsAffected() > 0, err
}

// RememberedDNS lists every domain with a remembered token.
func (s *Store) RememberedDNS(ctx context.Context) ([]DNSAutomation, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+dnsAutoCols+`
		FROM dns_automation a JOIN domains d ON d.id = a.domain_id WHERE a.api_token IS NOT NULL ORDER BY a.domain_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DNSAutomation
	for rows.Next() {
		a, err := scanDNSAutomation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TrackPublishedDKIM adds DKIM record names to retire later, and removes
// those already deleted at the provider.
func (s *Store) TrackPublishedDKIM(ctx context.Context, domainID string, add, remove []string) error {
	if add == nil {
		add = []string{}
	}
	if remove == nil {
		remove = []string{}
	}
	_, err := s.Pool.Exec(ctx, `UPDATE dns_automation SET
			published_dkim = ARRAY(SELECT DISTINCT n FROM unnest(published_dkim || $2::text[]) n WHERE NOT n = ANY($3::text[]) ORDER BY n),
			updated_at = now()
		WHERE domain_id = $1`, domainID, add, remove)
	return err
}

// SaveDNSLastRun replaces a domain's last run without touching the rest.
func (s *Store) SaveDNSLastRun(ctx context.Context, domainID string, run []byte) error {
	_, err := s.Pool.Exec(ctx, `UPDATE dns_automation SET last_run = $2, last_run_at = now(), updated_at = now() WHERE domain_id = $1`,
		domainID, run)
	return err
}

// LiveSenderIPs lists the public IPs that direct delivery workers announced
// in a heartbeat within the last `within`: the addresses an SPF record
// must authorize. Inbound nodes and relay workers do not send from theirs.
func (s *Store) LiveSenderIPs(ctx context.Context, within time.Duration) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ip FROM workers
		WHERE ip <> '' AND kind = 'direct' AND role IN ('delivery', 'any')
		  AND last_seen_at > now() - make_interval(secs => $1)
		ORDER BY ip`, within.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ips []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		ips = append(ips, ip)
	}
	return ips, rows.Err()
}
