-- OpenMailWire schema v1. Applied once, inside a transaction, by `openmailwire migrate`
-- (and on `openmailwire serve` start). Never edit an applied migration; add a new
-- file instead.

CREATE TABLE projects (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        text NOT NULL DEFAULT '',
    prefix      text NOT NULL,             -- first characters, for display only
    token_hash  bytea NOT NULL UNIQUE,     -- sha256 of the token; the token itself is never stored
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

CREATE TABLE domains (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name          text NOT NULL UNIQUE,
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'verified', 'failed')),
    dkim_selector text NOT NULL,
    dkim_public   text NOT NULL,
    dkim_private  bytea NOT NULL,           -- PKCS#8, sealed with OMW_SECRET_KEY
    return_path   text NOT NULL,            -- envelope sender domain, e.g. bounce.example.com
    checks        jsonb NOT NULL DEFAULT '[]',
    created_at    timestamptz NOT NULL DEFAULT now(),
    verified_at   timestamptz,
    checked_at    timestamptz
);

CREATE TABLE workers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    token_hash    bytea NOT NULL UNIQUE,
    ip            text NOT NULL DEFAULT '',  -- public sending IP the worker advertises
    ehlo          text NOT NULL DEFAULT '',
    capacity      int  NOT NULL DEFAULT 0,
    version       text NOT NULL DEFAULT '',
    draining      boolean NOT NULL DEFAULT false,
    last_seen_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE emails (
    id               uuid PRIMARY KEY,
    project_id       uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    domain_id        uuid REFERENCES domains(id) ON DELETE SET NULL,
    from_addr        text NOT NULL,
    to_addrs         text[] NOT NULL DEFAULT '{}',
    cc_addrs         text[] NOT NULL DEFAULT '{}',
    bcc_addrs        text[] NOT NULL DEFAULT '{}',
    subject          text NOT NULL DEFAULT '',
    message_id       text NOT NULL,
    return_path      text NOT NULL,
    idempotency_key  text,
    request_hash     bytea,
    raw              bytea,                  -- signed message, sealed; NULL once purged
    tags             jsonb NOT NULL DEFAULT '{}',
    retry_uncertain  boolean NOT NULL DEFAULT true,
    expires_at       timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    purged_at        timestamptz
);
CREATE UNIQUE INDEX emails_idempotency ON emails (project_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX emails_project_created ON emails (project_id, created_at DESC);

CREATE TABLE deliveries (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email_id          uuid NOT NULL REFERENCES emails(id) ON DELETE CASCADE,
    rcpt              text NOT NULL,
    rcpt_domain       text NOT NULL,
    status            text NOT NULL CHECK (status IN ('queued', 'sending', 'deferred', 'delivered', 'bounced', 'expired', 'suppressed', 'unknown')),
    attempts          int NOT NULL DEFAULT 0,
    next_attempt_at   timestamptz NOT NULL DEFAULT now(),
    lease_worker      uuid REFERENCES workers(id) ON DELETE SET NULL,
    lease_attempt     uuid,
    lease_expires_at  timestamptz,
    last_code         int,
    last_response     text NOT NULL DEFAULT '',
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deliveries_due ON deliveries (next_attempt_at) WHERE status IN ('queued', 'deferred');
CREATE INDEX deliveries_in_flight ON deliveries (rcpt_domain) WHERE status = 'sending';
CREATE INDEX deliveries_email ON deliveries (email_id);

CREATE TABLE attempts (
    id             uuid PRIMARY KEY,
    delivery_id    uuid NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
    worker_id      uuid REFERENCES workers(id) ON DELETE SET NULL,
    source_ip      text NOT NULL DEFAULT '',
    mx_host        text NOT NULL DEFAULT '',
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    reported_at    timestamptz,             -- set only by a worker report; replays are ignored
    outcome        text CHECK (outcome IN ('delivered', 'deferred', 'bounced', 'unknown')),
    smtp_code      int,
    enhanced_code  text NOT NULL DEFAULT '',
    response       text NOT NULL DEFAULT '',
    tls            text NOT NULL DEFAULT ''
);
CREATE INDEX attempts_delivery ON attempts (delivery_id);

CREATE TABLE suppressions (
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    address     text NOT NULL,
    reason      text NOT NULL,
    detail      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, address)
);

CREATE TABLE events (
    id           bigserial PRIMARY KEY,
    email_id     uuid NOT NULL REFERENCES emails(id) ON DELETE CASCADE,
    delivery_id  uuid REFERENCES deliveries(id) ON DELETE CASCADE,
    type         text NOT NULL,
    data         jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_email ON events (email_id, id);
