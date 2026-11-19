-- Agent safety and compliance (v0.6, lane B): approval holds, per-stream
-- suppressions and one-click unsubscribe, project settings, a per-recipient
-- frequency cap and circuit breakers on bounce and complaint rates.

-- A delivery can wait for a person to approve it ('held'); a rejected one
-- is final ('rejected').
ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_check
    CHECK (status IN ('queued', 'sending', 'deferred', 'delivered', 'bounced', 'expired', 'suppressed', 'unknown',
                      'canceled', 'held', 'rejected'));

-- One row per email held for approval, with v_reason and what was decided.
CREATE TABLE email_holds (
    email_id    uuid PRIMARY KEY REFERENCES emails(id) ON DELETE CASCADE,
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    api_key_id  uuid REFERENCES api_keys(id) ON DELETE SET NULL,   -- the key that sent it
    reasons     text[] NOT NULL DEFAULT '{}',                       -- the require_approval rules that matched
    stream      text NOT NULL DEFAULT 'transactional',
    created_at  timestamptz NOT NULL DEFAULT now(),
    decision    text CHECK (decision IN ('approved', 'rejected', 'expired')),
    decided_by  uuid REFERENCES api_keys(id) ON DELETE SET NULL,
    decided_at  timestamptz,
    note        text NOT NULL DEFAULT ''
);
CREATE INDEX email_holds_pending ON email_holds (project_id, created_at DESC, email_id) WHERE decision IS NULL;

-- Suppressions apply to every stream ('all', what bounces and complaints
-- add) or only to broadcast mail (one-click unsubscribes). An address can
-- have one row of each.
ALTER TABLE suppressions ADD COLUMN stream text NOT NULL DEFAULT 'all' CHECK (stream IN ('all', 'broadcast'));
ALTER TABLE suppressions DROP CONSTRAINT suppressions_pkey;
ALTER TABLE suppressions ADD PRIMARY KEY (project_id, address, stream);
CREATE INDEX suppressions_list ON suppressions (project_id, created_at DESC, address, stream);

-- Settings a full-access key changes with PATCH /project. No row means the
-- defaults.
CREATE TABLE project_settings (
    project_id                 uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    max_per_recipient_per_day  int CHECK (max_per_recipient_per_day >= 1),   -- NULL: no cap
    breaker_enabled            boolean NOT NULL DEFAULT true,
    breaker_bounce_rate        double precision NOT NULL DEFAULT 0.05 CHECK (breaker_bounce_rate > 0 AND breaker_bounce_rate <= 1),
    breaker_complaint_rate     double precision NOT NULL DEFAULT 0.001 CHECK (breaker_complaint_rate > 0 AND breaker_complaint_rate <= 1),
    breaker_min_recipients     int NOT NULL DEFAULT 50 CHECK (breaker_min_recipients >= 1),
    updated_at                 timestamptz NOT NULL DEFAULT now()
);

-- Every address the project has queued mail to (require_approval
-- first_contact). Held, suppressed, rejected and canceled mail does not
-- make a contact.
CREATE TABLE contacts (
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    address        text NOT NULL,
    first_sent_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, address)
);
INSERT INTO contacts (project_id, address, first_sent_at)
    SELECT e.project_id, d.rcpt, min(e.created_at)
    FROM deliveries d JOIN emails e ON e.id = d.email_id
    WHERE d.status NOT IN ('suppressed', 'canceled')
    GROUP BY e.project_id, d.rcpt;

-- Sends per recipient in the last 24 hours, kept only while the project has
-- max_per_recipient_per_day. A row is written before its email (so there is
-- no foreign key) and removed again when the email is not stored.
CREATE TABLE recipient_sends (
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    address     text NOT NULL,
    email_id    uuid NOT NULL,
    sent_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX recipient_sends_window ON recipient_sends (project_id, address, sent_at);
CREATE INDEX recipient_sends_email ON recipient_sends (email_id);
CREATE INDEX recipient_sends_age ON recipient_sends (sent_at);

-- Circuit breakers. Hourly counters per API key, sending domain and
-- project: recipients queued, bounces and complaints. The send path never
-- reads them; it only looks for an active pause.
CREATE TABLE safety_counters (
    scope       text NOT NULL CHECK (scope IN ('api_key', 'domain', 'project')),
    scope_id    uuid NOT NULL,
    hour        timestamptz NOT NULL,
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    recipients  int NOT NULL DEFAULT 0,
    bounces     int NOT NULL DEFAULT 0,
    complaints  int NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, scope_id, hour)
);
CREATE INDEX safety_counters_age ON safety_counters (hour);
CREATE INDEX safety_counters_project ON safety_counters (project_id);

CREATE TABLE sending_pauses (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    scope       text NOT NULL CHECK (scope IN ('api_key', 'domain', 'project')),
    scope_id    uuid NOT NULL,
    scope_name  text NOT NULL DEFAULT '',     -- the key's or project's name, or the domain
    reason      text NOT NULL CHECK (reason IN ('bounce_rate', 'complaint_rate')),
    rate        double precision NOT NULL,
    threshold   double precision NOT NULL,
    recipients  int NOT NULL,
    bounces     int NOT NULL,
    complaints  int NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    resumed_at  timestamptz,
    resumed_by  uuid REFERENCES api_keys(id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX sending_pauses_active ON sending_pauses (scope, scope_id) WHERE resumed_at IS NULL;
CREATE INDEX sending_pauses_project ON sending_pauses (project_id, created_at DESC);

-- Every bounce and complaint event, from whichever path records it (worker
-- reports, DSNs, ARF), counts toward the breakers of its email's key,
-- domain and project, and trips a breaker once its rolling 24-hour rate is
-- over the project's threshold. Tripping stores a pause and records a
-- 'sending_paused' event on that email (webhook sending.paused). A failure
-- here is logged and never stops the bounce or complaint being recorded.
CREATE FUNCTION omw_breaker_count() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    em        record;
    cfg       record;
    sc        record;
    hr        timestamptz := date_trunc('hour', now());
    is_bounce int := CASE WHEN NEW.type = 'bounced' THEN 1 ELSE 0 END;
    is_compl  int := CASE WHEN NEW.type = 'complained' THEN 1 ELSE 0 END;
    b_rate    double precision;
    c_rate    double precision;
    pause_id  uuid;
    v_reason  text;
    v_rate    double precision;
    v_limit   double precision;
    v_name    text;
BEGIN
    BEGIN
        SELECT e.project_id, e.api_key_id, e.domain_id INTO em FROM emails e WHERE e.id = NEW.email_id;
        IF NOT FOUND THEN
            RETURN NULL;
        END IF;
        INSERT INTO safety_counters AS c (scope, scope_id, hour, project_id, bounces, complaints)
            SELECT s.scope, s.id, hr, em.project_id, is_bounce, is_compl
            FROM (VALUES ('api_key', em.api_key_id), ('domain', em.domain_id), ('project', em.project_id)) s(scope, id)
            WHERE s.id IS NOT NULL
            ORDER BY s.scope, s.id
        ON CONFLICT (scope, scope_id, hour) DO UPDATE
            SET bounces = c.bounces + EXCLUDED.bounces, complaints = c.complaints + EXCLUDED.complaints;

        SELECT COALESCE(ps.breaker_enabled, true) AS enabled,
               COALESCE(ps.breaker_bounce_rate, 0.05) AS bounce_rate,
               COALESCE(ps.breaker_complaint_rate, 0.001) AS complaint_rate,
               COALESCE(ps.breaker_min_recipients, 50) AS min_recipients
            INTO cfg
            FROM (SELECT 1) one LEFT JOIN project_settings ps ON ps.project_id = em.project_id;
        IF NOT cfg.enabled THEN
            RETURN NULL;
        END IF;

        FOR sc IN
            SELECT c.scope, c.scope_id, sum(c.recipients)::int AS n, sum(c.bounces)::int AS b, sum(c.complaints)::int AS cp
            FROM safety_counters c
            WHERE (c.scope, c.scope_id) IN (('api_key', em.api_key_id), ('domain', em.domain_id), ('project', em.project_id))
              AND c.hour > now() - interval '24 hours'
            GROUP BY c.scope, c.scope_id
            ORDER BY c.scope
        LOOP
            CONTINUE WHEN sc.n < cfg.min_recipients OR sc.n = 0;
            b_rate := sc.b::double precision / sc.n;
            c_rate := sc.cp::double precision / sc.n;
            IF b_rate > cfg.bounce_rate THEN
                v_reason := 'bounce_rate'; v_rate := b_rate; v_limit := cfg.bounce_rate;
            ELSIF c_rate > cfg.complaint_rate THEN
                v_reason := 'complaint_rate'; v_rate := c_rate; v_limit := cfg.complaint_rate;
            ELSE
                CONTINUE;
            END IF;
            v_name := CASE sc.scope
                WHEN 'api_key' THEN (SELECT k.name FROM api_keys k WHERE k.id = sc.scope_id)
                WHEN 'domain' THEN (SELECT d.name FROM domains d WHERE d.id = sc.scope_id)
                ELSE (SELECT p.name FROM projects p WHERE p.id = sc.scope_id) END;
            pause_id := NULL;
            INSERT INTO sending_pauses (project_id, scope, scope_id, scope_name, reason, rate, threshold,
                    recipients, bounces, complaints)
                VALUES (em.project_id, sc.scope, sc.scope_id, COALESCE(v_name, ''), v_reason, v_rate, v_limit, sc.n, sc.b, sc.cp)
                ON CONFLICT (scope, scope_id) WHERE resumed_at IS NULL DO NOTHING
                RETURNING id INTO pause_id;
            IF pause_id IS NOT NULL THEN
                INSERT INTO events (email_id, delivery_id, type, data) VALUES (NEW.email_id, NEW.delivery_id, 'sending_paused',
                    jsonb_build_object('pause', jsonb_build_object(
                        'id', pause_id, 'scope', sc.scope, 'scope_id', sc.scope_id, 'scope_name', COALESCE(v_name, ''),
                        'reason', v_reason, 'rate', v_rate, 'threshold', v_limit,
                        'recipients', sc.n, 'bounces', sc.b, 'complaints', sc.cp)));
            END IF;
        END LOOP;
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'openmailwire circuit breaker: %', SQLERRM;
    END;
    RETURN NULL;
END $$;

CREATE TRIGGER events_breaker AFTER INSERT ON events
    FOR EACH ROW WHEN (NEW.type IN ('bounced', 'complained') AND NEW.email_id IS NOT NULL)
    EXECUTE FUNCTION omw_breaker_count();
