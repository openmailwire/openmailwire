package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Aggregate reports (DMARC rua and TLS-RPT) received for sending domains.

// ReportRetention is how long received reports are kept.
var ReportRetention = 180 * 24 * time.Hour

// DomainByReturnPath finds the verified domain whose return-path domain
// (bounce.<domain>) is rp, in any project.
func (s *Store) DomainByReturnPath(ctx context.Context, rp string) (Domain, error) {
	d, err := scanDomain(s.Pool.QueryRow(ctx, `SELECT `+domainCols+` FROM domains
		WHERE return_path = $1 AND status = 'verified' ORDER BY verified_at LIMIT 1`, rp))
	return d, noRows(err)
}

// ReturnPathKnown reports whether rp is a verified domain's return-path
// domain.
func (s *Store) ReturnPathKnown(ctx context.Context, rp string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM domains WHERE return_path = $1 AND status = 'verified')`, rp).Scan(&ok)
	return ok, err
}

// NewDomainReport is one received report.
type NewDomainReport struct {
	ProjectID string
	DomainID  string
	Kind      string // dmarc or tls
	OrgName   string
	ReportID  string
	Begin     time.Time
	End       time.Time
	Policy    []byte // JSON
	Messages  int64
	Failed    int64
	Truncated bool
	DMARC     []DMARCRow
	TLS       []TLSRow
}

type DMARCRow struct {
	SourceIP     string
	Count        int64
	Disposition  string
	DKIM, SPF    string
	HeaderFrom   string
	EnvelopeFrom string
	Auth         []byte // JSON
}

type TLSRow struct {
	PolicyType   string
	PolicyDomain string
	MXHosts      []string
	Successful   int64
	Failed       int64
	Failures     []byte // JSON array
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// InsertDomainReport stores a report and its rows in one transaction. A
// report already stored (same domain, kind, reporter and report id) is
// not stored again: duplicate is true.
func (s *Store) InsertDomainReport(ctx context.Context, r NewDomainReport) (id string, duplicate bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)
	policy := r.Policy
	if len(policy) == 0 {
		policy = []byte("{}")
	}
	err = tx.QueryRow(ctx, `INSERT INTO domain_reports (project_id, domain_id, kind, org_name, report_id, begin_at, end_at,
			policy, messages, failed, truncated)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (domain_id, kind, org_name, report_id) DO NOTHING RETURNING id::text`,
		r.ProjectID, r.DomainID, r.Kind, clean(r.OrgName, 200), clean(r.ReportID, 200), nullTime(r.Begin), nullTime(r.End),
		policy, r.Messages, r.Failed, r.Truncated).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	switch r.Kind {
	case "dmarc":
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"dmarc_report_rows"},
			[]string{"report_id", "source_ip", "count", "disposition", "dkim", "spf", "header_from", "envelope_from", "auth"},
			pgx.CopyFromSlice(len(r.DMARC), func(i int) ([]any, error) {
				x := r.DMARC[i]
				auth := x.Auth
				if len(auth) == 0 {
					auth = []byte("{}")
				}
				return []any{id, clean(x.SourceIP, 45), x.Count, clean(x.Disposition, 20), clean(x.DKIM, 20), clean(x.SPF, 20),
					clean(x.HeaderFrom, 253), clean(x.EnvelopeFrom, 253), string(auth)}, nil
			}))
	case "tls":
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"tls_report_rows"},
			[]string{"report_id", "policy_type", "policy_domain", "mx_hosts", "successful", "failed", "failures"},
			pgx.CopyFromSlice(len(r.TLS), func(i int) ([]any, error) {
				x := r.TLS[i]
				f := x.Failures
				if len(f) == 0 {
					f = []byte("[]")
				}
				hosts := cleanList(x.MXHosts)
				return []any{id, clean(x.PolicyType, 30), clean(x.PolicyDomain, 253), hosts, x.Successful, x.Failed, string(f)}, nil
			}))
	}
	if err != nil {
		return "", false, err
	}
	return id, false, tx.Commit(ctx)
}

// DomainReport is a stored report, without its rows.
type DomainReport struct {
	ID         string
	Kind       string
	OrgName    string
	ReportID   string
	Begin      *time.Time
	End        *time.Time
	Messages   int64
	Failed     int64
	Truncated  bool
	ReceivedAt time.Time
}

// DMARCSource is the DMARC outcome for one source IP over a period.
type DMARCSource struct {
	SourceIP      string
	Messages      int64
	Pass          int64 // passed DMARC (aligned DKIM or SPF)
	DKIMPass      int64
	SPFPass       int64
	Organizations []string
	HeaderFroms   []string
}

// DMARCOrg is the DMARC outcome reported by one organization.
type DMARCOrg struct {
	OrgName  string
	Reports  int64
	Messages int64
	Pass     int64
}

// TLSOutcome is one reporter's TLS sessions for one policy.
type TLSOutcome struct {
	OrgName      string
	PolicyType   string
	PolicyDomain string
	Successful   int64
	Failed       int64
	Failures     [][]byte // JSON arrays of failure details, one per report
}

// DomainReports is everything GET /domains/{id}/reports shows.
type DomainReports struct {
	Recent  []DomainReport
	Sources []DMARCSource
	Orgs    []DMARCOrg
	TLS     []TLSOutcome
}

// ReportsSince summarizes a domain's reports whose period ended after
// since (or, without a period, that arrived after it).
func (s *Store) ReportsSince(ctx context.Context, projectID, domainID string, since time.Time) (DomainReports, error) {
	var out DomainReports
	rows, err := s.Pool.Query(ctx, `SELECT id::text, kind, org_name, report_id, begin_at, end_at, messages, failed, truncated, received_at
		FROM domain_reports WHERE project_id = $1 AND domain_id = $2 AND COALESCE(end_at, received_at) >= $3
		ORDER BY received_at DESC LIMIT 50`, projectID, domainID, since)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r DomainReport
		if err := rows.Scan(&r.ID, &r.Kind, &r.OrgName, &r.ReportID, &r.Begin, &r.End, &r.Messages, &r.Failed, &r.Truncated, &r.ReceivedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.Recent = append(out.Recent, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	const window = `FROM dmarc_report_rows x JOIN domain_reports r ON r.id = x.report_id
		WHERE r.project_id = $1 AND r.domain_id = $2 AND r.kind = 'dmarc' AND COALESCE(r.end_at, r.received_at) >= $3`
	rows, err = s.Pool.Query(ctx, `SELECT x.source_ip, sum(x.count)::bigint,
			sum(CASE WHEN x.dkim = 'pass' OR x.spf = 'pass' THEN x.count ELSE 0 END)::bigint,
			sum(CASE WHEN x.dkim = 'pass' THEN x.count ELSE 0 END)::bigint,
			sum(CASE WHEN x.spf = 'pass' THEN x.count ELSE 0 END)::bigint,
			(array_agg(DISTINCT r.org_name))[1:10], (array_agg(DISTINCT x.header_from))[1:10]
		`+window+` GROUP BY x.source_ip ORDER BY 2 DESC, 1 LIMIT 500`, projectID, domainID, since)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var x DMARCSource
		if err := rows.Scan(&x.SourceIP, &x.Messages, &x.Pass, &x.DKIMPass, &x.SPFPass, &x.Organizations, &x.HeaderFroms); err != nil {
			rows.Close()
			return out, err
		}
		out.Sources = append(out.Sources, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	rows, err = s.Pool.Query(ctx, `SELECT r.org_name, count(DISTINCT r.id), sum(x.count)::bigint,
			sum(CASE WHEN x.dkim = 'pass' OR x.spf = 'pass' THEN x.count ELSE 0 END)::bigint
		`+window+` GROUP BY r.org_name ORDER BY 3 DESC, 1 LIMIT 100`, projectID, domainID, since)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var o DMARCOrg
		if err := rows.Scan(&o.OrgName, &o.Reports, &o.Messages, &o.Pass); err != nil {
			rows.Close()
			return out, err
		}
		out.Orgs = append(out.Orgs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	rows, err = s.Pool.Query(ctx, `SELECT r.org_name, x.policy_type, x.policy_domain, sum(x.successful)::bigint, sum(x.failed)::bigint,
			(array_agg(x.failures ORDER BY r.received_at DESC) FILTER (WHERE x.failures <> '[]'::jsonb))[1:20]
		FROM tls_report_rows x JOIN domain_reports r ON r.id = x.report_id
		WHERE r.project_id = $1 AND r.domain_id = $2 AND r.kind = 'tls' AND COALESCE(r.end_at, r.received_at) >= $3
		GROUP BY 1, 2, 3 ORDER BY 5 DESC, 1 LIMIT 100`, projectID, domainID, since)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var o TLSOutcome
		var failures []string
		if err := rows.Scan(&o.OrgName, &o.PolicyType, &o.PolicyDomain, &o.Successful, &o.Failed, &failures); err != nil {
			rows.Close()
			return out, err
		}
		for _, f := range failures {
			o.Failures = append(o.Failures, []byte(f))
		}
		out.TLS = append(out.TLS, o)
	}
	rows.Close()
	return out, rows.Err()
}
