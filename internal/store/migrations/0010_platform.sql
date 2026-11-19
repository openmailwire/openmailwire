-- Platform (v0.6): webhook fan-out tracked per event, fair webhook claims
-- with a circuit breaker per endpoint, webhook secret rotation and Ed25519
-- signatures, and key ids for the keys that seal data at rest.

-- 1. Fan-out per event.
--
-- Until v0.5 a cursor over events.id decided what was fanned out. An id is
-- taken at insert, not at commit, so an event whose transaction committed
-- after a higher id had been fanned out was skipped forever. Now every event
-- carries its own flag: a transaction's events are invisible until it
-- commits, and are picked up then.
--
-- The column is added with DEFAULT true, so every existing row reads as
-- fanned out without a table rewrite; new rows then get false. Only the
-- backlog past the old cursor (not fanned out yet) is reset, by primary key.
ALTER TABLE events ADD COLUMN fanned_out boolean NOT NULL DEFAULT true;
ALTER TABLE events ALTER COLUMN fanned_out SET DEFAULT false;
UPDATE events SET fanned_out = false
WHERE id > COALESCE((SELECT last_event_id FROM webhook_cursor WHERE id = 1), 9223372036854775807);
CREATE INDEX events_fanout ON events (id) WHERE NOT fanned_out;
-- Unused from v0.6 on. Kept so a v0.5 control plane still running during a
-- rolling upgrade keeps working; drop it in a later release.
COMMENT ON TABLE webhook_cursor IS 'unused since 0010_platform';

-- 2. Webhooks.
ALTER TABLE webhooks
    -- Fair claims: when the webhook next has a delivery due (NULL: none
    -- pending). The dispatcher picks webhooks by this, not deliveries, so a
    -- backlog on one endpoint never hides another's.
    ADD COLUMN next_due_at timestamptz,
    -- Circuit breaker: consecutive failed attempts, and until when the
    -- endpoint gets no attempts at all.
    ADD COLUMN failures int NOT NULL DEFAULT 0,
    ADD COLUMN breaker_until timestamptz,
    -- Ed25519 signing (Standard Webhooks "v1a"), optional: the sealed
    -- private key and the public key published by GET /webhooks/{id}.
    ADD COLUMN ed25519_key bytea,
    ADD COLUMN ed25519_public bytea,
    -- Secret rotation: the previous secret and key keep signing until
    -- previous_expires_at, next to the new ones.
    ADD COLUMN secret_previous bytea,
    ADD COLUMN ed25519_previous_key bytea,
    ADD COLUMN ed25519_previous_public bytea,
    ADD COLUMN previous_expires_at timestamptz,
    ADD COLUMN secret_rotated_at timestamptz;

CREATE INDEX webhook_deliveries_webhook_due ON webhook_deliveries (webhook_id, next_attempt_at) WHERE status = 'pending';
UPDATE webhooks w SET next_due_at = (
    SELECT min(d.next_attempt_at) FROM webhook_deliveries d WHERE d.webhook_id = w.id AND d.status = 'pending');
CREATE INDEX webhooks_next_due ON webhooks (next_due_at) WHERE next_due_at IS NOT NULL;

-- Whatever inserts webhook deliveries (the fan-out, or any later feature),
-- the webhook's next_due_at moves up to the earliest new one, once per
-- statement. Without this a delivery inserted elsewhere would wait until
-- the webhook's next claim.
CREATE FUNCTION webhook_deliveries_due() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE webhooks w SET next_due_at = GREATEST(LEAST(w.next_due_at, n.due), w.breaker_until)
    FROM (SELECT webhook_id, min(next_attempt_at) AS due FROM new_deliveries
          WHERE status = 'pending' GROUP BY webhook_id) n
    WHERE w.id = n.webhook_id;
    RETURN NULL;
END $$;
CREATE TRIGGER webhook_deliveries_due AFTER INSERT ON webhook_deliveries
    REFERENCING NEW TABLE AS new_deliveries FOR EACH STATEMENT EXECUTE FUNCTION webhook_deliveries_due();
-- Claims go through webhooks_next_due and webhook_deliveries_webhook_due now.
DROP INDEX webhook_deliveries_due;

-- 3. Keys that seal data at rest (OMW_SECRET_KEYS).
--
-- The control plane refuses to start unless every key that still seals data
-- here is configured. A key is known by an id derived from it and a check
-- value (an HMAC with a fixed label); the key itself is never stored.
CREATE TABLE secret_keys (
    key_id       text PRIMARY KEY,
    check_value  bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    retired_at   timestamptz            -- set once rotate-secrets has moved every value off this key
);

-- Small sealed values the control plane keeps for itself, such as the root
-- of the keys that sign bounce (VERP) addresses: it is kept here, sealed,
-- so those addresses stay valid when the sealing key is rotated.
CREATE TABLE secret_values (
    name   text PRIMARY KEY,
    value  bytea NOT NULL             -- sealed
);
