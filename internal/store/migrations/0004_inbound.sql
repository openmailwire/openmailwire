-- Inbound mail (v0.5): inboxes that agents own, the messages they receive,
-- thread links for sent replies, and timeline events about inbound messages.

CREATE TABLE inboxes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    domain_id   uuid NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    address     text NOT NULL UNIQUE,       -- lowercase; on one of the project's domains
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX inboxes_project ON inboxes (project_id, created_at);
CREATE INDEX inboxes_domain ON inboxes (domain_id);

CREATE TABLE inbound_messages (
    id                    uuid PRIMARY KEY,
    inbox_id              uuid NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
    project_id            uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    mail_from             text NOT NULL DEFAULT '',     -- envelope sender
    from_addr             text NOT NULL DEFAULT '',     -- From header, decoded
    to_addrs              text[] NOT NULL DEFAULT '{}',
    cc_addrs              text[] NOT NULL DEFAULT '{}',
    reply_to              text[] NOT NULL DEFAULT '{}',
    subject               text NOT NULL DEFAULT '',
    message_id            text NOT NULL DEFAULT '',     -- without angle brackets
    in_reply_to           text NOT NULL DEFAULT '',     -- without angle brackets
    refs                  text[] NOT NULL DEFAULT '{}', -- References, without angle brackets
    date_header           text NOT NULL DEFAULT '',
    thread_id             uuid NOT NULL,                -- first email or message of the chain
    in_reply_to_email_id  uuid REFERENCES emails(id) ON DELETE SET NULL,
    snippet               text NOT NULL DEFAULT '',     -- emptied when the body is purged
    size                  int NOT NULL DEFAULT 0,
    remote_ip             text NOT NULL DEFAULT '',
    helo                  text NOT NULL DEFAULT '',
    tls                   boolean NOT NULL DEFAULT false,
    raw                   bytea,                        -- sealed; NULL once purged
    received_at           timestamptz NOT NULL DEFAULT now(),
    purged_at             timestamptz
);
CREATE INDEX inbound_messages_inbox ON inbound_messages (inbox_id, received_at DESC, id DESC);
CREATE INDEX inbound_messages_thread ON inbound_messages (project_id, thread_id);
CREATE INDEX inbound_messages_reply ON inbound_messages (in_reply_to_email_id) WHERE in_reply_to_email_id IS NOT NULL;
CREATE INDEX inbound_messages_msgid ON inbound_messages (project_id, message_id) WHERE message_id <> '';
CREATE INDEX inbound_messages_purge ON inbound_messages (received_at) WHERE raw IS NOT NULL;
-- A retried SMTP transaction, or a copy that arrives twice, is stored once.
CREATE UNIQUE INDEX inbound_messages_dedupe ON inbound_messages (inbox_id, message_id) WHERE message_id <> '';

-- Thread links for sent emails. They live here, not in emails, so the send
-- path is unchanged: an email without a row starts its own thread.
CREATE TABLE email_threads (
    email_id                uuid PRIMARY KEY REFERENCES emails(id) ON DELETE CASCADE,
    thread_id               uuid NOT NULL,
    in_reply_to_message_id  uuid REFERENCES inbound_messages(id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_threads_thread ON email_threads (thread_id);

-- Threading and complaint handling look sent emails up by Message-ID.
CREATE INDEX emails_message_id ON emails (message_id);

-- A timeline event is about a sent email or, now, an inbound message.
ALTER TABLE events ALTER COLUMN email_id DROP NOT NULL;
ALTER TABLE events ADD COLUMN inbound_message_id uuid REFERENCES inbound_messages(id) ON DELETE CASCADE;
ALTER TABLE events ADD CONSTRAINT events_subject CHECK (email_id IS NOT NULL OR inbound_message_id IS NOT NULL);
CREATE INDEX events_inbound ON events (inbound_message_id) WHERE inbound_message_id IS NOT NULL;
