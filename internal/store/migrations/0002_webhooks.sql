-- Webhooks: endpoints per project, an outbox of signed deliveries, and a
-- cursor over the events table so every event is fanned out exactly once.

CREATE TABLE webhooks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    endpoint    text NOT NULL,
    events      text[] NOT NULL DEFAULT '{}',   -- empty = every event type
    secret      bytea NOT NULL,                 -- signing secret, sealed
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhooks_project ON webhooks (project_id);

CREATE TABLE webhook_deliveries (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_id       uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event_id         bigint NOT NULL,
    type             text NOT NULL,
    payload          bytea NOT NULL,            -- the exact JSON body that is signed and sent
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts         int NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_status      int,
    last_error       text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    delivered_at     timestamptz,
    UNIQUE (webhook_id, event_id)
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_webhook ON webhook_deliveries (webhook_id, created_at DESC);

CREATE TABLE webhook_cursor (
    id             int PRIMARY KEY CHECK (id = 1),
    last_event_id  bigint NOT NULL
);
-- Start after the events that already exist: no backfill on upgrade.
INSERT INTO webhook_cursor (id, last_event_id) SELECT 1, COALESCE(max(id), 0) FROM events;
