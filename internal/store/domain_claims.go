package store

import (
	"context"
	"time"
)

// DomainClaim is how long a project may hold a domain name without
// verifying it. Names are unique only among verified domains, so a claim
// never blocks another project; expiry just clears out names nobody proved.
const DomainClaim = 7 * 24 * time.Hour

// ExpiredDomain is a domain removed by ExpireDomainClaims.
type ExpiredDomain struct {
	ID, ProjectID, Name string
}

// ExpireDomainClaims removes unverified domains whose claim has run out, and
// unverified domains (failed for more than DomainClaim) whose name another
// project has since verified. Each removal is recorded in the project's
// audit log.
func (s *Store) ExpireDomainClaims(ctx context.Context) ([]ExpiredDomain, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `DELETE FROM domains d
		WHERE d.status <> 'verified' AND (
			d.claim_expires_at < now()
			OR (COALESCE(d.checked_at, d.created_at) < now() - make_interval(secs => $1)
				AND EXISTS (SELECT 1 FROM domains v WHERE v.name = d.name AND v.status = 'verified' AND v.project_id <> d.project_id)))
		RETURNING d.id::text, d.project_id::text, d.name`, DomainClaim.Seconds())
	if err != nil {
		return nil, err
	}
	var out []ExpiredDomain
	for rows.Next() {
		var d ExpiredDomain
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Name); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		if err := AppendAuditTx(ctx, tx, AuditEntry{
			ProjectID: d.ProjectID, Actor: ActorSystem, Action: "domain.expired",
			TargetType: "domain", TargetID: d.ID, Data: map[string]any{"name": d.Name},
		}); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit(ctx)
}
