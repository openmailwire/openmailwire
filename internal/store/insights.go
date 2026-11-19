package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Mailbox providers, recognised by the suffix of the MX host an attempt
// talked to.
//
// TODO(merge): lane C groups recipient domains by MX provider for
// throttling (and ships a rules file). Use that classifier here once both
// are merged, so insights and throttling agree on the names.
var mxProviders = []struct {
	Name     string
	Suffixes []string
}{
	{"google", []string{"google.com", "googlemail.com"}},
	{"microsoft", []string{"outlook.com", "hotmail.com", "microsoft.com"}},
	{"yahoo", []string{"yahoodns.net", "yahoo.com", "yahoo.net", "aol.com"}},
	{"apple", []string{"icloud.com", "me.com", "mac.com"}},
	{"zoho", []string{"zoho.com", "zoho.eu", "zoho.in", "zohomail.com", "zoho.com.au"}},
	{"proton", []string{"protonmail.ch", "proton.me"}},
	{"fastmail", []string{"messagingengine.com"}},
	{"yandex", []string{"yandex.net", "yandex.ru"}},
	{"mail.ru", []string{"mail.ru"}},
	{"gmx", []string{"gmx.net", "gmx.com", "web.de"}},
	{"ionos", []string{"ionos.com", "ionos.de", "kundenserver.de"}},
	{"mimecast", []string{"mimecast.com"}},
	{"proofpoint", []string{"pphosted.com", "ppe-hosted.com"}},
	{"barracuda", []string{"barracudanetworks.com"}},
}

// providerSQL is a CASE expression naming the provider of attempt a's MX
// host. The suffixes are constants, never input.
var providerSQL = func() string {
	var b strings.Builder
	b.WriteString(`CASE WHEN h = '' THEN 'unknown'`)
	for _, p := range mxProviders {
		var conds []string
		for _, s := range p.Suffixes {
			conds = append(conds, fmt.Sprintf(`h = '%s' OR h LIKE '%%.%s'`, s, s))
		}
		fmt.Fprintf(&b, ` WHEN %s THEN '%s'`, strings.Join(conds, " OR "), p.Name)
	}
	b.WriteString(` ELSE 'other' END`)
	return b.String()
}()

// InsightWindows are the periods GET /insights reports on.
var InsightWindows = []struct {
	Name string
	Span time.Duration
}{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}}

// InsightStats are the delivery attempts of one group in one window.
type InsightStats struct {
	Attempts int `json:"attempts"`
	Accepted int `json:"accepted"`
	Deferred int `json:"deferred"`
	Bounced  int `json:"bounced"`
	Unknown  int `json:"unknown"`

	AcceptanceRate *float64 `json:"acceptance_rate"`
	DeferralRate   *float64 `json:"deferral_rate"`
	BounceRate     *float64 `json:"bounce_rate"`

	// From when the email was queued (or scheduled for) to the receiver's
	// acceptance, over accepted recipients.
	TimeToAcceptanceP50 *float64 `json:"time_to_acceptance_p50_seconds"`
	TimeToAcceptanceP95 *float64 `json:"time_to_acceptance_p95_seconds"`
}

func (s *InsightStats) rates() {
	if s.Attempts == 0 {
		return
	}
	r := func(n int) *float64 {
		v := float64(n) / float64(s.Attempts)
		return &v
	}
	s.AcceptanceRate, s.DeferralRate, s.BounceRate = r(s.Accepted), r(s.Deferred), r(s.Bounced)
}

// InsightGroup is one row: a mailbox provider or a sending IP.
type InsightGroup struct {
	Provider  string `json:"provider,omitempty"`
	SendingIP string `json:"sending_ip,omitempty"`
	InsightStats
}

// InsightWindow is one period.
type InsightWindow struct {
	Window      string         `json:"window"`
	Total       InsightStats   `json:"total"`
	ByProvider  []InsightGroup `json:"by_provider"`
	BySendingIP []InsightGroup `json:"by_sending_ip"`
}

// Insights is what GET /insights returns.
type Insights struct {
	Windows []InsightWindow `json:"windows"`
	// The emails looked at: those sent in the longest window, newest first,
	// at most InsightsMaxEmails. Truncated is true when the cap was hit:
	// the longer windows then cover only the newest emails.
	EmailsConsidered int  `json:"emails_considered"`
	Truncated        bool `json:"truncated"`
}

// InsightsMaxEmails bounds the work of one insights query.
const InsightsMaxEmails = 100_000

// DeliveryInsights computes acceptance, deferral and bounce rates and time
// to acceptance for a project's attempts, per mailbox provider and per
// sending IP, for each of InsightWindows. Windows count emails by when they
// were sent. The query walks the project's newest emails through
// (project_id, created_at), then their deliveries and attempts by index, and
// stops after maxEmails emails.
func (s *Store) DeliveryInsights(ctx context.Context, projectID string, maxEmails int) (Insights, error) {
	if maxEmails <= 0 {
		maxEmails = InsightsMaxEmails
	}
	longest := InsightWindows[len(InsightWindows)-1].Span
	var aggs []string
	for i, w := range InsightWindows {
		in := fmt.Sprintf(`sent_at >= now() - make_interval(secs => %d)`, int64(w.Span.Seconds()))
		aggs = append(aggs, fmt.Sprintf(`count(*) FILTER (WHERE %[1]s) AS n%[2]d,
			count(*) FILTER (WHERE %[1]s AND outcome = 'delivered') AS a%[2]d,
			count(*) FILTER (WHERE %[1]s AND outcome = 'deferred') AS d%[2]d,
			count(*) FILTER (WHERE %[1]s AND outcome = 'bounced') AS b%[2]d,
			count(*) FILTER (WHERE %[1]s AND outcome = 'unknown') AS u%[2]d,
			percentile_cont(0.5) WITHIN GROUP (ORDER BY tta) FILTER (WHERE %[1]s) AS p50_%[2]d,
			percentile_cont(0.95) WITHIN GROUP (ORDER BY tta) FILTER (WHERE %[1]s) AS p95_%[2]d`, in, i))
	}
	q := fmt.Sprintf(`
		WITH recent AS (
			SELECT id, created_at, GREATEST(created_at, COALESCE(scheduled_at, created_at)) AS due_at
			FROM emails
			WHERE project_id = $1 AND created_at >= now() - make_interval(secs => $2)
			ORDER BY created_at DESC LIMIT $3
		),
		att AS (
			SELECT r.created_at AS sent_at, a.outcome, a.source_ip,
				lower(rtrim(a.mx_host, '.')) AS h,
				CASE WHEN a.outcome = 'delivered' AND a.finished_at IS NOT NULL
					THEN GREATEST(extract(epoch FROM a.finished_at - r.due_at), 0) END AS tta
			FROM recent r
			JOIN deliveries d ON d.email_id = r.id
			JOIN attempts a ON a.delivery_id = d.id
			WHERE a.outcome IS NOT NULL
		)
		SELECT GROUPING(provider), GROUPING(source_ip), COALESCE(provider, ''), COALESCE(source_ip, ''),
			(SELECT count(*) FROM recent), %s
		FROM (SELECT att.*, %s AS provider FROM att) x
		GROUP BY GROUPING SETS ((provider), (source_ip), ())`, strings.Join(aggs, ",\n"), providerSQL)

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Insights{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '20s'`); err != nil {
		return Insights{}, err
	}
	rows, err := tx.Query(ctx, q, projectID, longest.Seconds(), maxEmails)
	if err != nil {
		return Insights{}, err
	}
	defer rows.Close()

	out := Insights{Windows: make([]InsightWindow, len(InsightWindows))}
	for i, w := range InsightWindows {
		out.Windows[i] = InsightWindow{Window: w.Name, ByProvider: []InsightGroup{}, BySendingIP: []InsightGroup{}}
	}
	for rows.Next() {
		var gProvider, gIP int
		var provider, ip string
		var considered int
		stats := make([]InsightStats, len(InsightWindows))
		dest := []any{&gProvider, &gIP, &provider, &ip, &considered}
		for i := range stats {
			st := &stats[i]
			dest = append(dest, &st.Attempts, &st.Accepted, &st.Deferred, &st.Bounced, &st.Unknown,
				&st.TimeToAcceptanceP50, &st.TimeToAcceptanceP95)
		}
		if err := rows.Scan(dest...); err != nil {
			return Insights{}, err
		}
		out.EmailsConsidered = considered
		for i := range stats {
			st := stats[i]
			if st.Attempts == 0 && !(gProvider == 1 && gIP == 1) {
				continue // nothing for this group in this window
			}
			st.rates()
			switch {
			case gProvider == 1 && gIP == 1:
				out.Windows[i].Total = st
			case gIP == 1:
				out.Windows[i].ByProvider = append(out.Windows[i].ByProvider, InsightGroup{Provider: provider, InsightStats: st})
			default:
				label := ip
				if label == "" {
					label = "unknown"
				}
				out.Windows[i].BySendingIP = append(out.Windows[i].BySendingIP, InsightGroup{SendingIP: label, InsightStats: st})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Insights{}, err
	}
	if out.EmailsConsidered == 0 {
		// No attempts at all gives no rows; count the emails anyway.
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM emails WHERE project_id = $1
			AND created_at >= now() - make_interval(secs => $2) LIMIT $3) x`, projectID, longest.Seconds(), maxEmails).
			Scan(&out.EmailsConsidered); err != nil {
			return Insights{}, err
		}
	}
	out.Truncated = out.EmailsConsidered >= maxEmails
	for i := range out.Windows {
		sortGroups(out.Windows[i].ByProvider)
		sortGroups(out.Windows[i].BySendingIP)
	}
	return out, nil
}

// sortGroups orders groups by attempts, most first.
func sortGroups(gs []InsightGroup) {
	for i := 1; i < len(gs); i++ {
		for j := i; j > 0 && gs[j].Attempts > gs[j-1].Attempts; j-- {
			gs[j], gs[j-1] = gs[j-1], gs[j]
		}
	}
}

// ProviderOf names the mailbox provider of an MX host, as insights groups
// it.
func ProviderOf(mxHost string) string {
	h := strings.ToLower(strings.TrimSuffix(mxHost, "."))
	if h == "" {
		return "unknown"
	}
	for _, p := range mxProviders {
		for _, s := range p.Suffixes {
			if h == s || strings.HasSuffix(h, "."+s) {
				return p.Name
			}
		}
	}
	return "other"
}
