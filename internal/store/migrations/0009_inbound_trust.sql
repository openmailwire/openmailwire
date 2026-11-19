-- Inbound trust (v0.6): sender authentication and holds for inboxes,
-- deduplication that a forged copy cannot pre-empt, sealed snippets, and
-- DMARC and TLS-RPT aggregate reports received for sending domains.

-- Mail that fails or lacks sender authentication waits for a full-access
-- key to release it before agent keys can see it.
ALTER TABLE inboxes ADD COLUMN hold_unauthenticated boolean NOT NULL DEFAULT true;

ALTER TABLE inbound_messages
    ADD COLUMN trust text NOT NULL DEFAULT 'unauthenticated'
        CHECK (trust IN ('authenticated', 'unauthenticated', 'failed')),
    ADD COLUMN authentication_results jsonb NOT NULL DEFAULT '{}',  -- SPF, DKIM and DMARC details
    ADD COLUMN held boolean NOT NULL DEFAULT false,                 -- hidden from sending_access keys until released
    ADD COLUMN released_at timestamptz,
    ADD COLUMN suspicious_rules text[] NOT NULL DEFAULT '{}',       -- prompt-injection rules the text matched
    ADD COLUMN content_hash text NOT NULL DEFAULT '',               -- envelope sender, verdict, headers and body
    ADD COLUMN message_id_conflict boolean NOT NULL DEFAULT false,  -- another copy with this Message-ID differs
    ADD COLUMN snippet_sealed bytea;                                -- sealed like raw; NULL once purged

-- Snippets (sign-in codes, among others) were stored in plain text. They
-- are sealed from now on; the old ones are derived again from the sealed
-- body the next time they are listed.
UPDATE inbound_messages SET snippet = '' WHERE snippet <> '';

-- A message is a duplicate only when its content matches too, so a copy
-- sent first with a stolen Message-ID no longer hides the genuine one.
DROP INDEX inbound_messages_dedupe;
CREATE UNIQUE INDEX inbound_messages_dedupe ON inbound_messages (inbox_id, message_id, content_hash) WHERE message_id <> '';

-- Aggregate reports sent to a domain's rua= addresses: DMARC (RFC 7489)
-- and SMTP TLS reporting (RFC 8460).
CREATE TABLE domain_reports (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    domain_id    uuid NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('dmarc', 'tls')),
    org_name     text NOT NULL DEFAULT '',
    report_id    text NOT NULL DEFAULT '',
    begin_at     timestamptz,
    end_at       timestamptz,
    policy       jsonb NOT NULL DEFAULT '{}',   -- dmarc: the policy the reporter found
    messages     bigint NOT NULL DEFAULT 0,     -- dmarc: messages; tls: sessions
    failed       bigint NOT NULL DEFAULT 0,     -- dmarc: messages failing DMARC; tls: failed sessions
    truncated    boolean NOT NULL DEFAULT false,
    received_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX domain_reports_dedupe ON domain_reports (domain_id, kind, org_name, report_id);
CREATE INDEX domain_reports_domain ON domain_reports (domain_id, kind, received_at DESC);
CREATE INDEX domain_reports_received ON domain_reports (received_at);

CREATE TABLE dmarc_report_rows (
    report_id      uuid NOT NULL REFERENCES domain_reports(id) ON DELETE CASCADE,
    source_ip      text NOT NULL,
    count          bigint NOT NULL,
    disposition    text NOT NULL DEFAULT '',
    dkim           text NOT NULL DEFAULT '',   -- aligned DKIM, as the reporter evaluated it
    spf            text NOT NULL DEFAULT '',   -- aligned SPF
    header_from    text NOT NULL DEFAULT '',
    envelope_from  text NOT NULL DEFAULT '',
    auth           jsonb NOT NULL DEFAULT '{}' -- raw DKIM and SPF results
);
CREATE INDEX dmarc_report_rows_report ON dmarc_report_rows (report_id);

CREATE TABLE tls_report_rows (
    report_id      uuid NOT NULL REFERENCES domain_reports(id) ON DELETE CASCADE,
    policy_type    text NOT NULL DEFAULT '',
    policy_domain  text NOT NULL DEFAULT '',
    mx_hosts       text[] NOT NULL DEFAULT '{}',
    successful     bigint NOT NULL DEFAULT 0,
    failed         bigint NOT NULL DEFAULT 0,
    failures       jsonb NOT NULL DEFAULT '[]' -- failure details, at most 100
);
CREATE INDEX tls_report_rows_report ON tls_report_rows (report_id);
