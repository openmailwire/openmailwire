-- DNS automation (lane G): a domain's records published through its DNS
-- host's API (Cloudflare) by POST /domains/{id}/dns/auto. One row per
-- domain that has used it.
CREATE TABLE dns_automation (
    domain_id      uuid PRIMARY KEY REFERENCES domains(id) ON DELETE CASCADE,
    provider       text NOT NULL,
    -- The zone the records went to.
    zone_id        text NOT NULL DEFAULT '',
    zone_name      text NOT NULL DEFAULT '',
    -- The provider API token the project chose to remember, sealed with
    -- OMW_SECRET_KEYS (store.SealedColumns); NULL when none is remembered.
    -- With it, DKIM rotations and Ed25519 publish their own records, and
    -- retired DKIM records are deleted.
    api_token      bytea,
    remembered_at  timestamptz,
    -- DKIM record names published through the API; each is deleted at the
    -- provider once its key is retired.
    published_dkim text[] NOT NULL DEFAULT '{}',
    -- The last run: the changes and warnings (never the token).
    last_run       jsonb,
    last_run_at    timestamptz,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- The maintenance loop looks only at rows with a remembered token.
CREATE INDEX dns_automation_remembered ON dns_automation (domain_id) WHERE api_token IS NOT NULL;
