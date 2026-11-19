-- Delivery and deliverability (v0.6, lane C): throttling per mailbox
-- provider and sending IP, IP warm-up, greylisting, faster leases, sender
-- authentication problems, worker token roles and self-checks, and DKIM keys
-- for rotation and Ed25519.

-- Throttling state is kept per scope and sending IP. A scope is a mailbox
-- provider ("google", "microsoft", ...: recipient domains grouped by their MX
-- hosts) or, for a domain whose MX belongs to no known provider, the
-- recipient domain itself. The old per-domain rows are transient, so they go.
DELETE FROM receiver_state;
ALTER TABLE receiver_state RENAME COLUMN rcpt_domain TO scope;
ALTER TABLE receiver_state ADD COLUMN source_ip text NOT NULL DEFAULT '';
ALTER TABLE receiver_state DROP CONSTRAINT receiver_state_pkey;
ALTER TABLE receiver_state ADD PRIMARY KEY (scope, source_ip);
-- A throttling deferral halves the limit only when a second one comes from a
-- different recipient. The first one waits here: a hash of its recipient.
ALTER TABLE receiver_state ADD COLUMN pending_rcpt text;
ALTER TABLE receiver_state ADD COLUMN pending_at timestamptz;

-- The mailbox provider of each recipient domain, learned from the MX host
-- in workers' reports. provider is '' when the MX belongs to no known
-- provider.
CREATE TABLE rcpt_domain_providers (
    rcpt_domain text PRIMARY KEY,
    provider    text NOT NULL,
    mx_host     text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Sending IPs and their warm-up. An IP with warm_at set is warm: no daily
-- caps. IPs that sent mail before this migration are warm.
CREATE TABLE sending_ips (
    ip             text PRIMARY KEY,
    warmup_started date NOT NULL DEFAULT (now() AT TIME ZONE 'UTC')::date,
    warm_at        timestamptz
);
INSERT INTO sending_ips (ip, warm_at)
    SELECT DISTINCT ip, now() FROM workers WHERE ip <> ''
    ON CONFLICT DO NOTHING;
INSERT INTO sending_ips (ip, warm_at)
    SELECT DISTINCT source_ip, now() FROM attempts WHERE source_ip <> ''
    ON CONFLICT DO NOTHING;

-- Attempts leased per sending IP, provider and UTC day, for warm-up caps.
CREATE TABLE warmup_usage (
    ip       text NOT NULL,
    provider text NOT NULL,
    day      date NOT NULL,
    sent     int  NOT NULL DEFAULT 0,
    PRIMARY KEY (ip, provider, day)
);

-- Greylisting deferrals do not count toward the relay hand-over.
ALTER TABLE deliveries ADD COLUMN greylists int NOT NULL DEFAULT 0;

-- Leases pick due deliveries domain by domain when the head of the queue
-- is held up (domains at their limit or in backoff), so a backlog on one
-- domain does not slow every lease down.
CREATE INDEX deliveries_domain_due ON deliveries (rcpt_domain, next_attempt_at)
    WHERE status IN ('queued', 'deferred');
CREATE TABLE lease_cursor (
    kind        text PRIMARY KEY,
    rcpt_domain text NOT NULL
);

-- Authentication problems receivers reported (Gmail's 4.7.26 to 4.7.32 and
-- the 5.7.x equivalents), per project and sending domain. They are about
-- one sender, so they never throttle a provider for everyone.
CREATE TABLE sender_auth_issues (
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    sending_domain text NOT NULL,
    scope          text NOT NULL,
    enhanced_code  text NOT NULL,
    count          int  NOT NULL DEFAULT 1,
    first_seen_at  timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_response  text NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, sending_domain, scope, enhanced_code)
);

-- Worker tokens have a role. Tokens created before roles existed keep
-- 'any'. A worker reports its start-up self-check in its heartbeat.
ALTER TABLE workers ADD COLUMN role text NOT NULL DEFAULT 'any'
    CHECK (role IN ('any', 'delivery', 'inbound', 'relay'));
ALTER TABLE workers ADD COLUMN self_check jsonb;

-- DKIM keys besides the domain's current RSA key (domains.dkim_*): a new
-- RSA key waiting for its DNS record during a rotation ('pending'), the
-- previous key kept published for an overlap ('retiring'), and an Ed25519
-- key ('pending' until its record is published, then 'active').
CREATE TABLE dkim_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    domain_id    uuid NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    selector     text NOT NULL,
    algorithm    text NOT NULL CHECK (algorithm IN ('rsa', 'ed25519')),
    public_key   text NOT NULL,
    private_key  bytea NOT NULL,  -- PKCS#8, sealed with OMW_SECRET_KEY
    state        text NOT NULL CHECK (state IN ('pending', 'active', 'retiring')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,
    retire_at    timestamptz,
    UNIQUE (domain_id, selector)
);
