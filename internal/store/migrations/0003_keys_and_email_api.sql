-- API keys for agents (permission, sending policy, last use) and the email
-- API: which key queued each email, scheduled sends, and canceled deliveries.

ALTER TABLE api_keys
    ADD COLUMN permission   text NOT NULL DEFAULT 'full_access'
                            CHECK (permission IN ('full_access', 'sending_access')),
    ADD COLUMN policy       jsonb NOT NULL DEFAULT '{}',   -- caps and allowlists, see internal/apikey
    ADD COLUMN last_used_at timestamptz;                   -- updated at most once a minute
CREATE INDEX api_keys_project ON api_keys (project_id, created_at) WHERE revoked_at IS NULL;

ALTER TABLE emails
    ADD COLUMN api_key_id   uuid REFERENCES api_keys(id) ON DELETE SET NULL,
    ADD COLUMN scheduled_at timestamptz,
    -- Recipients that count toward the key's send caps: not suppressed, not
    -- canceled. Kept on the email so a cap check never joins deliveries.
    ADD COLUMN quota_rcpts  int NOT NULL DEFAULT 0;
CREATE INDEX emails_project_key_created ON emails (project_id, api_key_id, created_at) INCLUDE (quota_rcpts);

ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_check
    CHECK (status IN ('queued', 'sending', 'deferred', 'delivered', 'bounced', 'expired', 'suppressed', 'unknown', 'canceled'));
