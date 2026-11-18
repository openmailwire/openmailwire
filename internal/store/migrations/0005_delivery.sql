-- Delivery quality (v0.6): adaptive per-receiver throttling and fallback
-- relay workers.

-- What the control plane has learned about each recipient domain. A row
-- exists only once the domain has throttled us; without one, the domain
-- gets OMW_DOMAIN_CONCURRENCY.
CREATE TABLE receiver_state (
    rcpt_domain       text PRIMARY KEY,
    "limit"           int NOT NULL CHECK ("limit" >= 1),  -- in-flight deliveries allowed from direct workers
    backoff_until     timestamptz,                        -- direct workers skip the domain until then
    throttles         int NOT NULL DEFAULT 0,             -- consecutive throttling deferrals; sets the next backoff
    last_throttled_at timestamptz,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- "direct" workers deliver from their own IPs; "relay" workers hand mail to
-- a smarthost.
ALTER TABLE workers ADD COLUMN kind text NOT NULL DEFAULT 'direct' CHECK (kind IN ('direct', 'relay'));
