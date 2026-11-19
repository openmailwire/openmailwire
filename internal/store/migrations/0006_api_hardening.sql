-- API hardening (v0.6, lane A): domain names per project, idempotency keys
-- per API key, API key IP allowlists and expiry, and an append-only,
-- hash-chained audit log.

-- A domain name is unique within a project, and among verified domains.
-- Any project may add a name; only the one that proves it controls the DNS
-- can verify it. An unverified claim expires (claim_expires_at, 7 days after
-- it is added); verifying clears it for good.
ALTER TABLE domains DROP CONSTRAINT domains_name_key;
ALTER TABLE domains ADD CONSTRAINT domains_project_name UNIQUE (project_id, name);
CREATE UNIQUE INDEX domains_verified_name ON domains (name) WHERE status = 'verified';
ALTER TABLE domains ADD COLUMN claim_expires_at timestamptz;
UPDATE domains SET claim_expires_at = now() + interval '7 days' WHERE status <> 'verified';
CREATE INDEX domains_claim_expiry ON domains (claim_expires_at) WHERE claim_expires_at IS NOT NULL;

-- An Idempotency-Key belongs to the API key that sent it. Batch items use
-- their own namespace (see internal/api), so they never meet single sends.
DROP INDEX emails_idempotency;
CREATE UNIQUE INDEX emails_idempotency_key ON emails (api_key_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

ALTER TABLE api_keys
    ADD COLUMN allowed_ips text[],      -- CIDR prefixes the key may be used from; NULL = anywhere
    ADD COLUMN expires_at  timestamptz; -- NULL = never

-- Who did what. Each row's hash is SHA-256(prev_hash || entry), where entry
-- is the canonical JSON of the row and prev_hash the hash of the previous
-- row of the same project (32 zero bytes for the first). Rows cannot be
-- changed or deleted, and any edit made around that breaks the chain.
CREATE TABLE audit_log (
    id            bigint PRIMARY KEY,
    project_id    uuid REFERENCES projects(id),   -- NULL: a server-wide entry
    created_at    timestamptz NOT NULL,
    actor         text NOT NULL,                  -- api_key, cli or system
    actor_key_id  uuid,                           -- no foreign key: entries outlive keys
    action        text NOT NULL,                  -- e.g. api_key.created, domain.verified
    target_type   text NOT NULL DEFAULT '',
    target_id     text NOT NULL DEFAULT '',
    entry         text NOT NULL,
    prev_hash     bytea NOT NULL,
    hash          bytea NOT NULL
);
CREATE SEQUENCE audit_log_id_seq OWNED BY audit_log.id;
CREATE INDEX audit_log_project ON audit_log (project_id, id DESC);

CREATE FUNCTION audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END
$$;
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();
