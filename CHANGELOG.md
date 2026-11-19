# Changelog

## Unreleased

### API correctness and hardening (v0.6, lane A)

**Security fixes** (from the v0.5.0 audit; each has a regression test):
- **Smuggled From in raw messages (R1#3).** A header written `From :` (whitespace before the colon) slipped past `POST /emails/raw` and SMTP submission: Go kept it apart from `From`, so a key limited by `allowed_from` could queue and DKIM-sign mail from any address. Header names with whitespace before the colon are now refused, From/To/Cc/Bcc are counted ignoring case on exact names, and a second `Sender`, `Reply-To` or `Subject` is refused.
- **Domain squatting (R1#4).** Domain names were unique across all projects, so any project could hold a name forever, and the `409` told it which names existed elsewhere. Names are now unique within a project and among verified domains: any project may add a name, only the one that proves control of the DNS can verify it (`409 domain_verified_elsewhere` for a second), and an unverified claim is removed after 7 days (`claim_expires_at`). `openmailwire domains force-verify` takes an optional project id.
- **Idempotency keys per API key (R1#6).** Keys were per project and checked before the key's policy, so one key could tell which keys another had used (409 against 422), replay another key's request (getting its email id even for recipients outside its own allowlist), and pre-claim keys to block it. They now belong to the API key.
- **Batch idempotency namespace (R1#7).** A batch item's key `<key>:<index>` collided with a single send that used that literal key. Batch items now have a namespace of their own. A batch sent before the upgrade and retried after it is not recognised as a replay.
- **Memory per large request (R1#5).** One 13 MB send allocated about 363 MB, with no bound on how many ran at once. Bodies are read into one buffer of their exact size, attachments and raw messages are decoded straight from the request (no string copies), JSON is checked for unknown fields without a second buffer, and attachments are base64-encoded in place: the same send now allocates about 130 MB. Requests with bodies over 1 MB need one of `OMW_MAX_LARGE_REQUESTS` slots (default 4); one that waits more than 30 seconds gets `503 server_busy` with `Retry-After`.
- **Per-key request rate limit (R1#5).** Each API key may make `OMW_RATE_LIMIT` requests per second (default 20, bursts of `OMW_RATE_LIMIT_BURST`, default twice the rate); more get `429 rate_limit_exceeded` with `Retry-After`. A remote MCP tool call counts once.
- **Unstorable text (R1#8).** A NUL in a JSON subject, or a raw Subject that is not UTF-8 (8-bit Latin-1, or an encoded-word that decodes to invalid UTF-8), answered `500` (`451` over SMTP, so clients retried forever). Such text is now refused with `422` naming the field, and any text the database refuses is a `422` on every path, never a `500`. Custom header values may not hold control characters, and API key names neither.
- **Header lines over 998 bytes (R1#9, R3#10).** A 998-byte subject made a 1,007-byte line, long display names lines of 1,222 bytes and more, a long Persian name 1,834 bytes, a long `content_type` 1,108 bytes. Subjects are folded at spaces or written as encoded-words, one per line; address lists are folded between encoded-words and before the address. Display names are capped at 256 characters, addresses at 254 and attachment `content_type` at 255.
- **Long reply subjects (R1#10).** A received subject that already started with "Re:" was never shortened, so a reply to a 1,000-byte subject was refused. Reply subjects are now always cut to 998 bytes.
- **HTTP server limits (R4#5).** The control plane set only a header timeout. It now has `ReadTimeout` (3 min), `WriteTimeout` (4 min), `IdleTimeout` (2 min) and `MaxHeaderBytes` (64 KB). **`OMW_LISTEN` now defaults to `127.0.0.1:8080`**: set `:8080` to accept connections from other machines (the compose file already does).
- Every answer carries `X-Content-Type-Options: nosniff`, and API answers `Cache-Control: no-store`. `GET /health` shows the version only to a caller with a valid API key or worker token.

**Features:**
- **Generated text part:** an email with only `html` gets a `text/plain` part made from it (links keep their address, lists their bullets).
- **API key `allowed_ips` and `expires_at`:** a key can be limited to networks (`403 ip_not_allowed`) and in time (`401 api_key_expired`). Set them on `POST /api-keys`, change or remove them (`null`) with `PATCH`, or use `openmailwire apikey create … --allow-ip CIDR --expires-in 720h`. Behind a reverse proxy, `OMW_TRUSTED_PROXIES` names the proxies whose `X-Forwarded-For` is believed.
- **Audit log:** `GET /audit-log` (full access) lists who created, changed or removed API keys, domains, webhooks and inboxes, with the acting key. It is append-only (the database refuses `UPDATE` and `DELETE`) and hash-chained per project; `GET /audit-log/verify` recomputes the chain and names the first entry that does not check out. Other parts of the server record their own actions with `store.AppendAudit` / `store.AppendAuditTx`.
- **Domain records:**
  - DMARC (at least `p=none`) is now required. A subdomain may rely on its parent's record; two records, or one without a valid policy, fail.
  - The return-path MX is always shown. It points at `OMW_BOUNCE_MX`, or else `OMW_INBOUND_MX`, and is required when either is set.
  - Each record's `detail` explains the Gmail, Yahoo and Microsoft sender rules behind it until it checks out.
  - Existing domains keep their status until they are verified again; a re-check now needs the DMARC record.

**Upgrade notes:** migration `0006_api_hardening.sql` drops the global unique index on domain names and gives every currently unverified domain a claim that expires 7 days after the upgrade.

## v0.5.1: parser hardening, current Go

- The mail parser reads only the header of an embedded `message/*` part instead of parsing it again, and unfolds header lines in linear time with limits on header size and field count. Hostile inbound mail can no longer use excessive stack or CPU.
- Release binaries, CI and the container image are built with the newest stable Go instead of `go.mod`'s minimum version, so they include current standard-library security fixes. The container image now uses current Alpine.

### Agent safety and compliance

Server-enforced rules for agents, on every path that sends (`POST /emails`, batches, `POST /emails/raw` and SMTP submission, inbox replies, MCP).

**Approval holds.**
- Key policy `require_approval` with the rules `always`, `first_contact`, `has_attachments`, `has_links` and `recipients_over: N`. A matching email is stored with delivery status `held` and answers `{"id", "held": true, "hold_reasons"}`.
- `GET /emails/held`, `POST /emails/{id}/approve` and `POST /emails/{id}/reject` (with an optional `note`), for `full_access` keys and keys whose policy has `can_approve`. A key never approves its own email (`403 cannot_approve_own_email`); deciding twice answers `409 not_held`.
- New delivery statuses `held` and `rejected`; webhooks `email.held`, `email.approved` and `email.rejected`.
- Held mail expires at its `expires_at` (`email.failed`), and its body is not purged before.

**Streams and one-click unsubscribe.**
- `stream: "transactional" | "broadcast"` on `POST /emails` and batches; `stream` or an `X-OMW-Stream` header on `POST /emails/raw`.
- Broadcast mail goes to one recipient and gets `List-Unsubscribe` and `List-Unsubscribe-Post: List-Unsubscribe=One-Click` (RFC 8058), DKIM-signed, pointing at `OMW_PUBLIC_URL/u/{token}`. The token is HMAC-signed over the project and the address.
- `POST /u/{token}` (no key) suppresses the address for broadcast mail at once; `GET` only shows a confirm page. Both are rate-limited per client IP. Without `OMW_PUBLIC_URL`, broadcast mail is refused with `422 unsubscribe_unavailable`.
- Every message gets `Feedback-ID: <stream>:<api key id>:<project id>:omw`.
- Suppressions now have a stream: `all` (bounces, complaints) or `broadcast` (unsubscribes).

**Suppressions API.** `GET /suppressions` (filters and pagination), `POST /suppressions`, `DELETE /suppressions/{address}` and `GET /suppressions/export` (CSV). Sending keys may read.

**Project settings.** `GET /project` and `PATCH /project` (full access):
- `max_per_recipient_per_day`: at most N emails per recipient in any 24 hours, across all keys, checked under a project lock; `429 recipient_rate_limited` with `Retry-After`.
- `circuit_breaker`: `enabled`, `bounce_rate` (default 0.05), `complaint_rate` (default 0.001), `min_recipients` (default 50).

**Circuit breakers.** Rolling 24-hour bounce and complaint rates per API key, per sending domain and per project, counted from every bounce and complaint however it arrives. A tripped breaker refuses sends with `403 sending_paused` (naming the rate, the scope and how to resume) and fires `sending.paused`. `GET /pauses` and `POST /pauses/{id}/resume` (full access). The send path only looks up active pauses.

**`POST /emails/check`.** The body of `POST /emails`; sends nothing. Answers `would` (`send`, `hold` or `refuse`), every refusal that would apply, the hold reasons, the size and warnings (`no_text_part`, `html_too_large`, `link_domain_mismatch`, `missing_unsubscribe`, `domain_not_verified`, `large_message`).

**MCP.** New tools `check_email`, `list_held`, `approve_email`, `reject_email` (offered only to keys that may approve; destructive), `list_suppressions`, `add_suppression` and `list_pauses`. `send_email` takes `stream`, and it and `reply_to_message` say when a send is held.

**Configuration.** `OMW_PUBLIC_URL`. **Migration** `0007_agent_safety.sql`, which also backfills the project contacts from past deliveries. `golang.org/x/net` (already in the module graph) becomes a direct dependency, for the public suffix list.

### Platform, webhooks and supply chain (v0.6, lane E)

**Fixed**
- **Webhook events were lost under concurrent sends (audit R1#1, R4#1).** Fan-out followed a cursor over event ids, which are taken at insert, not at commit, so an event committed after a higher id had been fanned out never got its webhook (up to 1,973 of 2,400 `email.sent` in the audit). Each event now carries a `fanned_out` flag. Fan-out claims events with `FOR UPDATE SKIP LOCKED` through a partial index, and sees an event only once its transaction has committed. Events not yet fanned out at upgrade are still delivered; older ones are marked done.
- **One project's slow endpoints delayed every other project's webhooks (R4#4; 75 s in the audit).** Claims now pick webhooks, oldest due first, and give each a bounded share: at most 4 requests per endpoint and 8 per project in flight. A slot is refilled as soon as its request ends instead of after a whole batch. An endpoint must answer within 10 s (time to first byte; 15 s in all). A project has at most 10 webhooks (`OMW_WEBHOOKS_PER_PROJECT`; one more answers `409 webhook_limit_reached`).
- **Circuit breaker per endpoint:** after 5 failures in a row, no requests for 30 s, doubling up to 10 minutes, then one probe at a time. Paused deliveries keep their retries. `GET /webhooks/{id}` shows `consecutive_failures` and `paused_until`.
- **Webhook connections never closed (R4#6).** Idle connections now close after 30 s, with caps on idle connections overall and per host and on connections per host.
- **The webhook address guard missed NAT64 and 6to4 (R4#7).** Only global unicast addresses are dialed now, minus every range in the IANA special-purpose registries. NAT64 addresses are judged by the IPv4 address they carry; local-use NAT64, 6to4 and Teredo are refused. An IP literal that is not public is refused when the webhook is created.
- **`last_error` revealed internal addresses (R4#8)** such as `dial tcp [::1]:5432`. Projects now see a generic cause (`timed out: …`, `could not connect: …`); a name that doesn't resolve and one that resolves to a refused address read the same. Details go to the log.
- **Releases were built with Go 1.25.0 (R4#2, R4#3)**, which govulncheck found reaching 45 standard-library vulnerabilities. CI and releases now build with the newest Go 1.27 patch release (`check-latest`), `go.mod` names `toolchain go1.27.4` (the `go` directive stays 1.25.0), and `govulncheck` runs on every push and weekly. The image builds on `golang:1.27-alpine` and runs on `alpine:3.24`.
- **Workflow hardening (R4#11):** every action is pinned by commit SHA; every workflow defaults to `contents: read` and each release job asks only for what it needs; release binaries are built outside the checkout (`vcs.modified=false`, checked); release archives, `checksums.txt` and the image get signed build provenance (`actions/attest-build-provenance`, verifiable with `gh attestation verify`), and the attestation bundle is published with each release.
- **The Python SDK sent the API key on redirects (R4#10).** It now refuses redirects (`redirect_refused`) and marks the key unredirectable even with a caller's own opener. The Node and Go SDKs now refuse redirects too (Go forwarded the key on same-host redirects, including to `http://`); PHP never followed them and now names the error the same way.
- **The Go SDK accepted a timestamp of 9223372036854775807**, which overflowed the age check. Timestamps are limited to 15 digits, as in the other SDKs.
- **Start-up key check:** the control plane refuses to start when `OMW_SECRET_KEY` doesn't open the data in the database, with a message that says which key is missing.

**Added**
- **Secret rotation.** `OMW_SECRET_KEYS=new,old` holds several keys: the first seals new values, all of them open old ones. Sealed values now record the id of their key. `openmailwire rotate-secrets` re-encrypts every stored secret online, in small transactions, and reports when the old keys can be removed. `OMW_SECRET_KEY` keeps working. Bounce (VERP) addresses keep verifying across rotations.
- **Webhook secret rotation:** `POST /webhooks/{id}/rotate-secret` `{"overlap_seconds"}` (default one day, at most 7 days). During the overlap every request is signed with both secrets.
- **Ed25519 webhook signatures** (Standard Webhooks `v1a`): `"asymmetric_signing": true` on `POST /webhooks`; the public key (`whpk_…`) is in `GET /webhooks/{id}`. The four SDK verifiers accept a `whpk_…` key (Python in pure Python, PHP through sodium).
- **`GET /insights`** (full access): acceptance, deferral and bounce rates and p50/p95 time to acceptance, per mailbox provider (from the MX host) and per sending IP, over 24 hours, 7 days and 30 days. Bounded to the newest 100,000 emails of 30 days, through indexes.

**Upgrade notes**
- Migration `0010_platform.sql` adds `events.fanned_out` without rewriting the table, and builds small indexes on `events`, `webhooks` and `webhook_deliveries`. `webhook_cursor` is no longer used.
- The first start of v0.6 registers the key it is given, after checking that it opens the existing data. Keep using the same `OMW_SECRET_KEY`.
- Projects with more than 10 webhooks keep them, but cannot add more unless `OMW_WEBHOOKS_PER_PROJECT` is raised.
- Webhook endpoints that take longer than 10 s to start answering now time out.

### Delivery and deliverability

**Throttling per mailbox provider and sending IP.**
- Recipient domains are grouped by the provider their MX hosts belong to (Google, Microsoft, Yahoo, Apple, Zoho, Proton, Fastmail, Yandex, Mail.ru, GMX, Comcast, Mimecast, Proofpoint, Barracuda), learned from each attempt. A domain of no known provider is its own group. The rules ship in `internal/policy/providers.json`; `OMW_PROVIDER_RULES` points at your own.
- Limits and backoff apply per group and sending IP: a throttled IP pauses for that provider while other IPs go on. `OMW_DOMAIN_CONCURRENCY` still caps each recipient domain across all workers.
- Only connection- or IP-level replies count as "slow down": a 421, or `4.7.0`, `4.7.28`, `4.7.650` at the greeting, EHLO or MAIL FROM. Mailbox replies (`4.2.x`), `4.5.3` and authentication codes (`4.7.26`–`4.7.32` but `4.7.28`) never do, and the reply text is no longer matched. A limit halves only after two different recipients got such a reply within 15 minutes.
- Authentication problems are recorded per project and sending domain: an `auth_problem` event, and `auth_issues` in `GET /domains/{id}/dkim`.
- Events `throttled` and `throttle_relaxed` carry `scope` and `source_ip` instead of `rcpt_domain`.

**Automatic IP warm-up** (`OMW_WARMUP`, default on): a new sending IP gets a daily cap per provider (50, 100, 200… for 12 days by default). Overflow waits for the next day or goes to a live relay worker. `openmailwire workers list` shows the remaining capacity; `openmailwire workers warm <name> [--restart]` marks an IP warm or starts over. IPs that sent before this version count as warm.

**Retries.** The first retry comes 5 to 15 minutes after the first attempt (was 1 minute), then 30 minutes, 1, 2 and 4 hours. Greylisting is recognised: retried after 5 to 15 minutes, and not counted toward `OMW_RELAY_AFTER_ATTEMPTS`.

**DKIM.**
- `From`, `To`, `Subject`, `Date`, `Message-ID`, `Reply-To` and `Cc` are oversigned.
- Optional Ed25519 signature next to RSA (RFC 8463): `POST /domains/{id}/dkim/ed25519`.
- Key rotation with two selectors and an overlap: `POST /domains/{id}/dkim/rotate` creates a key, and switches to it once its record is published; the previous record stays listed for 8 days. `GET /domains/{id}/dkim` lists the keys. The domain view shows `DKIM_NEXT`, `DKIM_PREVIOUS` and `DKIM_ED25519` records.

**Workers.**
- Connection reuse: one connection per MX host carries several messages, with `RSET` in between (at most 50 per connection, idle for at most 20 seconds).
- A self-check at start: `OMW_EHLO` is an FQDN, its A record is `OMW_PUBLIC_IP`, the IP's PTR names it, and port 25 is open (`OMW_SELF_CHECK_SMTP`). The worker refuses to start on failure unless `OMW_SKIP_SELF_CHECK=true`; the result is sent with the heartbeat and shown by `workers list`.
- Worker tokens have a role: `openmailwire workers add <name> --role delivery|inbound|relay` (default `delivery`). Inbound endpoints refuse delivery and relay tokens and the reverse (`403 wrong_worker_role`); a delivery token cannot lease as a relay. Tokens created before keep working everywhere.
- Opportunistic TLS accepts TLS 1.0 and 1.1 (any encryption beats plaintext); MTA-STS and DANE still require TLS 1.2.
- Messages with 8-bit data go only to servers that announce `8BITMIME` (sent with `BODY=8BITMIME`), and those with non-ASCII addresses or header fields only to servers that announce `SMTPUTF8`. Otherwise the attempt is deferred with `4.6.3`, since converting would break the DKIM signature.
- With `OMW_DNSSEC_RESOLVER`, MX and address lookups go through the validating resolver, and DANE applies only behind a secure MX answer. Authenticated TLSA records that are all unusable still require TLS.
- MTA-STS policies are fetched again before they expire.

**Fixed** (v0.5.0 audit):
- A cancel or an expiry that committed while a lease was choosing deliveries was overwritten, and the canceled mail was sent. The lease now checks the status again on the locked rows.
- One project's per-mailbox deferral (Gmail's `450 4.2.1 … receiving mail at a rate`) or authentication code halved gmail.com's limit and paused it for every project.
- Scheduled mail kept the `Date` and DKIM signature of its request, up to 30 days old. It is dated and signed again when leased; nothing else in the message changes.
- A long reply with multi-byte characters was cut inside a character; the report then failed with a 500 forever and the mail was sent again. Text is cut on character boundaries and made valid UTF-8, without NUL bytes, on the worker and the control plane.
- The body of a large message and its final reply shared one 5-minute deadline. Each block now has 3 minutes, and the final reply 10 (RFC 5321 section 4.5.3.2).
- A job could run past its lease, which the control plane then gave to another attempt. Jobs end before the lease, with a margin for the report, and are not started when the lease is nearly over.
- A message the control plane could not decrypt stayed leased and could end `unknown`. It is released at once, with an `unseal_failed` event, and retried 5 minutes later.
- A report and the sweep could deadlock (opposite lock order). Both now lock the delivery, then the attempt.
- A 421 reply to EHLO lost its code, because the HELO fallback ran on the closed connection. HELO is used only after `500`/`502`, and every reply records its SMTP step (`stage` in reports and events).
- A lease read the whole due backlog of domains at their limit (about 0.5 µs a row, inside the global lock). It now reads a fixed head of the queue, then domain by domain (new index `deliveries_domain_due`).
- The worker API refused unknown fields, so a newer worker's reports were lost. They are ignored now; workers keep reports refused with 401, 403, or for unknown fields by an older control plane, and alert. A report for another worker's attempt answers `409 attempt_of_another_worker` (was 403).

**Upgrading.** Upgrade the control plane before the workers. Migration `0008_deliverability.sql` resets `receiver_state` (now keyed by scope and sending IP). See [docs/production.md](docs/production.md#10-upgrading-to-v06).

## v0.5.0: agent inboxes, SMTP submission, delivery quality, SDKs, dashboard, docs for models

### SMTP submission and raw messages (v0.5)

**`POST /emails/raw`** sends a complete message you built yourself: `{"raw": base64, "rcpts"?, "mail_from"?}`.
- The `From` domain must be added to the project and verified.
- Recipients are `rcpts`, or else every address in `To`, `Cc` and `Bcc`.
- OpenMailWire removes `Bcc`, adds `Message-ID` and `Date` when missing, turns line endings into CRLF and adds its DKIM signature on top. Nothing else changes.
- Lines over 998 bytes are refused with a `422` that says how to fix them.
- `Idempotency-Key` works as on `POST /emails`. The request may be up to 20 MB.

**New role `openmailwire submission`:** an SMTP submission server for apps that only speak SMTP (WordPress, Postfix relays, monitoring tools, older code).
- Port 587 with STARTTLS, which is required before `AUTH`. Optionally port 465 with implicit TLS (`OMW_SUBMISSION_TLS_LISTEN`).
- `AUTH PLAIN` and `LOGIN`: any username, and an API key as the password. The key is checked at login.
- Each message goes to `POST /emails/raw` with the client's key, so it gets the same checks, key limits and signing. API answers become SMTP replies: `250 2.0.0 queued as <id>`, `535 5.7.8`, `550 5.7.1`, `554 5.6.0`, `451 4.7.1` or `451 4.3.0`, with the API's message where there is one.
- Messages up to 14 MB (`OMW_SUBMISSION_MAX_SIZE`), 50 recipients each, command and data timeouts, and three failed logins per connection.
- A byte-identical resubmission of a message with its own `Message-ID` and `Date` returns the first copy instead of sending twice.
- The TLS certificate (`OMW_TLS_CERT`, `OMW_TLS_KEY`) is reloaded when its files change.
- Logs never contain message content or full addresses.

**New dependencies:** `github.com/emersion/go-smtp` v0.26.0 and `github.com/emersion/go-sasl`.

## v0.4.0: API keys for agents, attachments, scheduling, batch

### API keys for agents

Give each agent its own key, with limits the server enforces on every path that sends, MCP included.

- **Permission:** `full_access` (the default) or `sending_access`. A sending key can send, read and cancel its own emails, and read domains. Anything else answers `403 insufficient_permission`.
- **Policy** (every field optional):
  - `max_per_hour` and `max_per_day`: recipients queued in a rolling window. Suppressed and canceled recipients do not count.
  - `allowed_from` and `allowed_recipients`: address patterns where `*` matches within the local part or the domain part.
- **Refusals:** `403 sender_not_allowed`, `403 recipient_not_allowed` (naming the first such recipient), and `429 rate_limit_exceeded` with a `Retry-After` header that says when enough of the window frees up. Caps are checked again under a per-key lock when the email is stored, so parallel requests cannot overshoot them.
- **Endpoints** (full access only): `GET /api-keys`, `POST /api-keys` (the token is shown once), `GET /api-keys/{id}`, `PATCH /api-keys/{id}` (name and policy) and `DELETE /api-keys/{id}` (revokes).
- `last_used_at` is recorded, at most once a minute.
- **CLI:** `openmailwire apikey create <project-id> <name> [--sending-only] [--max-per-hour N] [--max-per-day N] [--allow-to PATTERN]... [--allow-from PATTERN]...` and `openmailwire apikey list <project-id>`.

### Email API

- **Attachments** on `POST /emails`: `[{filename, content (base64), content_type?, content_id?}]`.
  - Regular files make the message `multipart/mixed`.
  - A `content_id` makes a file inline, in a `multipart/related` part with the HTML.
  - At most 20 files and 10 MB decoded. DKIM covers them.
- **Request bodies** may be up to 20 MB for `/emails`, `/emails/batch` and `/emails/raw` (4 MB elsewhere).
- **Scheduled sends:** `scheduled_at` (RFC 3339, at most 30 days ahead) adds a `scheduled` event, and `expires_at` counts from it.
- **`POST /emails/{id}/cancel`** cancels every recipient still queued and never attempted. The new delivery status `canceled` comes with a `canceled` event and the `email.canceled` webhook. Answers `409 cannot_cancel` when nothing is left to cancel.
- **`POST /emails/batch`:** 1 to 100 emails, all validated and policy-checked first, then stored in one transaction; all or nothing. `Idempotency-Key` applies per email as `<key>:<index>`.
- **`GET /emails`:** newest first, with `limit`, `before` (cursor) and `api_key_id` filters.
- Every email records the key that sent it (`api_key_id`, also in `GET /emails/{id}`). A sending key sees only its own emails.

### MCP

- `send_email` takes `scheduled_at`.
- New tools: `list_emails` and `cancel_email` (marked destructive).

### Migration

`0003_keys_and_email_api.sql` runs on start. It adds:
- `permission`, `policy` and `last_used_at` to `api_keys`;
- `api_key_id`, `scheduled_at` and `quota_rcpts` to `emails`, with an index on `(project_id, api_key_id, created_at)`;
- `canceled` to the delivery statuses.

Existing keys keep full access with no limits.

**Docs for people and models**
- `GET /openapi.json`: an OpenAPI 3.1 description of every public endpoint:
  - emails (send with attachments, `scheduled_at`, `expires_at`, `retry_uncertain` and `Idempotency-Key`; get, list, cancel, batch and raw);
  - domains, webhooks, API keys, inboxes and messages (with reply and attachments).
  - It also documents the bearer scheme, which operations need a `full_access` key, every error name with its status, and examples for the main operations.
  - Its `webhooks` section gives each event's payload and the Svix and Standard Webhooks signature headers.
- `GET /llms.txt`: a short guide for language models. It covers auth, the safe send flow, every endpoint in one line, the delivery statuses, how to recover from each error, webhook verification and MCP setup.
- Both are embedded in the binary, need no API key, and are cached for 5 minutes (with an ETag).
- Copies live in `docs/openapi.json` and `docs/llms.txt`. A test keeps them identical to the embedded files. Another test fails when the server registers a public route that the spec does not document.

### SDKs for Node.js, Python, Go and PHP

Client libraries in [`sdks/`](sdks), one directory each, MIT licensed (the server stays AGPL-3.0) so that apps under any licence can embed them:
- `sdks/node`: TypeScript, ESM, no runtime dependencies, global `fetch`, Node 18+. Package `openmailwire`.
- `sdks/python`: standard library only (`urllib`), type hints, Python 3.9+. Package `openmailwire`.
- `sdks/go`: its own module, `github.com/openmailwire/openmailwire/sdks/go`; standard library only, context-aware, Go 1.22+.
- `sdks/php`: PHP 8.1+, curl, PSR-4. Package `openmailwire/openmailwire`.

**Each one covers:**
- emails: send (idempotency key, attachments, `scheduled_at`), get, list, cancel, batch and raw MIME;
- domains: create, list, get, verify, delete;
- webhooks: create, list, get, delete;
- API keys: create, list, get, update, delete;
- inboxes and messages: create, list and delete inboxes; list, read and reply to messages; download attachments;
- `waitForDelivery` (polls until no recipient is queued, sending or deferred) and `waitForReply` (polls the inboxes for messages answering a sent email);
- `verifyWebhook`: both the `svix-*` and `webhook-*` headers, several `v1,` signatures, constant-time comparison, a 5-minute timestamp tolerance.

**Configuration** comes from `OMW_API_URL` (default `http://localhost:8080`) and `OMW_API_KEY` when not passed in. **Errors** are typed and carry the API's `statusCode`, `name` and `message`, plus `Retry-After` for 429s.

Each SDK has its own README and tests against a local mock server; the new workflow `.github/workflows/sdks.yml` runs them when `sdks/` changes. Nothing is published to npm, PyPI or Packagist yet.

### Delivery quality (v0.5)

**MTA-STS (RFC 8461).** Workers discover a recipient domain's policy (`_mta-sts.<domain>` TXT), fetch it over HTTPS (no redirects, at most 64 KB) and cache it by `id` and `max_age`.
- `enforce`: only the MX hosts the policy names, only over TLS with a certificate valid for the host. Otherwise the attempt is deferred with `4.7.5` and a response naming the policy. The attempt's `tls` is `verified+mta-sts`.
- `testing`: delivery goes on; the attempt's event notes in `data.tls_policy` what enforce mode would have refused.

**DANE (RFC 7672),** with `OMW_DNSSEC_RESOLVER` set to a validating resolver. TLSA records at `_25._tcp.<mx>` are trusted only when the resolver sets the AD bit, and only for MX hosts whose address is signed. With usable records (DANE-EE or DANE-TA), TLS is required and the certificate must match; otherwise the attempt is deferred with `4.7.5`. The attempt's `tls` is `dane`. DANE takes precedence over MTA-STS.

**Adaptive throttling.** The control plane learns per-receiver limits.
- A throttling deferral (421, `4.7.0`, `4.7.28`, `4.7.650`, `4.3.2`, or a reply about "rate" or "too many") halves the domain's concurrency and pauses direct workers for 1, 2, 4… up to 30 minutes.
- Each delivery raises the limit by one, up to `OMW_DOMAIN_CONCURRENCY`.
- New timeline events `throttled` and `throttle_relaxed`, recorded before the attempt's own event. They are not webhooks.
- Leases now hold a domain to its limit even within one request, and are serialized so concurrent workers cannot overshoot it.

**Fallback relay worker.** A worker with `OMW_RELAY_HOST` (and `OMW_RELAY_PORT`, `OMW_RELAY_TLS`, `OMW_RELAY_USERNAME`, `OMW_RELAY_PASSWORD`) hands mail to a smarthost over STARTTLS or implicit TLS, with `AUTH PLAIN`. It takes deliveries with `OMW_RELAY_AFTER_ATTEMPTS` attempts (default 2) or whose domain is paused; while it is live, direct workers leave those to it. Messages pass unchanged, so DKIM still verifies.

**Worker protocol.** `HeartbeatRequest` and `LeaseRequest` gain `kind` (`direct`, the default when empty, or `relay`); `Report` gains `tls_policy`. Upgrade the control plane before the workers: an older control plane refuses relay workers and testing-mode notes.

**Operations.** `openmailwire workers list` shows each worker's kind. The worker starts leasing only after its first heartbeat.

**Migration:** `0005_delivery.sql` adds `receiver_state` and `workers.kind`.

**New dependency:** `github.com/miekg/dns` (DANE lookups with the DO and AD bits).

**For tests:** `internal/smtptest` gains STARTTLS (`EnableSTARTTLS`, `RequireTLS`), implicit TLS (`StartImplicitTLS`), `AUTH PLAIN` (`RequireAuth`), replies per recipient domain (`ReplyToDomain`), `RcptCount`, and throwaway certificates (`NewCA`, `SelfSigned`).

### Inbound mail: inboxes, bounces and complaints (v0.5)

**New role `openmailwire inbound`:** an SMTP server for mail from the internet (go-smtp), stateless like a worker.
- Checks every `RCPT TO` with the control plane and refuses unknown addresses with `550 5.1.1`, so it never sends bounces of its own.
- Answers `250` only once the control plane has stored the message; `451` otherwise, so senders retry.
- STARTTLS with `OMW_TLS_CERT`/`OMW_TLS_KEY`, a size limit (`OMW_INBOUND_MAX_SIZE`, default 25 MB), per-command and per-connection timeouts, and a cap on concurrent connections.
- Settings: `OMW_INBOUND_LISTEN`, `OMW_INBOUND_HOSTNAME`, `OMW_CONTROL_URL`, `OMW_WORKER_TOKEN` (inbound nodes authenticate as workers).

**Bounce processing.**
- Delivery status notifications sent to the signed VERP address are checked against its HMAC, then parsed (RFC 3464, including Gmail, Microsoft 365 and Postfix reports, and plain-text "failure notice" bounces).
- `Action: failed` marks the delivery `bounced`, even after `delivered`, with event data `{"source": "dsn", "status", "diagnostic", …}`, suppresses the address on 5.1.x, and sends `email.bounced`.

**Complaint processing.**
- ARF reports (RFC 5965) sent to `OMW_FBL_ADDRESS` or to a VERP address become a `complained` event, suppress the address with reason `complaint`, and send the new webhook `email.complained`.
- The email is found through the VERP envelope sender in the report, or else its Message-ID and `Original-Rcpt-To`. DMARC failure reports and `not-spam` feedback are ignored.

**Inboxes.**
- `POST /inboxes`, `GET /inboxes`, `GET /inboxes/{id}`, `DELETE /inboxes/{id}`. An inbox address is on one of the project's verified domains.
- `GET /inboxes/{id}/messages` (cursor pagination, `thread_id` filter), `GET /messages/{id}` (text, HTML, threading headers, attachment list), `GET /messages/{id}/attachments/{index}`.
- `POST /messages/{id}/reply`: sends `Re: …` from the inbox to the message's Reply-To or From, with `In-Reply-To` and `References`, through `POST /emails` with the caller's own key, so every send rule applies.
- `GET /emails/{id}/replies`: the received messages that answer a sent email.
- Threading: `in_reply_to_email_id` and `thread_id` from `In-Reply-To`/`References`; replies sent through the API join the thread.
- Messages are sealed with `OMW_SECRET_KEY`, deduplicated by Message-ID, announced with the new webhook `email.received` (without their content), and purged after `OMW_INBOUND_RETENTION` (default 30 days).
- Domains show an optional `INBOUND` MX record when `OMW_INBOUND_MX` is set. It is not required for verification.

**MCP tools:** `list_inboxes`, `list_messages` (can wait up to 60 s for new mail), `read_message`, `reply_to_message` and `wait_for_reply` (up to 120 s). `read_message` marks the sender's text as information, not instructions.

**Schema:** migration `0004_inbound.sql` adds `inboxes`, `inbound_messages` and `email_threads`, an index on `emails.message_id`, and lets timeline events point at an inbound message.

**Dependencies:** `github.com/emersion/go-smtp` (SMTP server), and `golang.org/x/text` (already in the module graph) to decode the character sets received mail uses.

### Web dashboard

The control plane serves a dashboard at `/dashboard/` (`/dashboard` redirects there). Sign in with an API key; it is kept in the tab's `sessionStorage` only.

**Pages:**
- **Emails:** the list with "Load more", open by ID, a test-send form, and an email's per-recipient status with a timeline of attempts (worker, IP, MX, TLS, SMTP reply) and events. It refreshes itself until every recipient is final.
- **Domains:** add, a records table with copy buttons, verify and delete.
- **Webhooks:** create (the signing secret is shown once), delete, and recent deliveries.
- **API keys:** create with permission and policy (caps, allowlists), revoke; the token is shown once.
- **Inboxes:** add and delete, messages, thread view, attachments and reply.
- Endpoints the server doesn't have yet show "Not available on this server version"; a key without the permission sees "This key can't manage …".

**Security:**
- A strict Content-Security-Policy on every dashboard response (no inline code, same-origin connections only, no framing, no form posts), plus `nosniff`, `no-referrer` and `no-store` on the page.
- API data is only ever inserted as text. Inbound HTML is shown as source and attachments always download. A Node test fails if an HTML sink appears in the scripts.
- No build step, no framework, no CDN: plain HTML, CSS and JavaScript modules embedded with `go:embed` (`internal/dashboard`).

Usable at phone width and in dark mode.

## v0.3.0: MCP server for AI agents

OpenMailWire is now built for AI agents first.

**Two ways for an agent to connect:**
- `openmailwire mcp` runs a Model Context Protocol server on stdio for Claude Desktop, Claude Code, Cursor and other MCP clients. Set `OMW_API_URL` and `OMW_API_KEY`.
- Every control plane also serves MCP at `/mcp` (streamable HTTP, stateless), authenticated with an API key.

**Tools:**
- `send_email`: marked destructive, so clients ask a human before sending.
- `get_email`: can wait up to 60 s for every recipient's final status.
- `list_domains`, `add_domain` (returns the DNS records to publish) and `verify_domain` (by name or id).

**How it behaves:**
- Every tool calls the HTTP API with the agent's key, so an agent has exactly that key's permissions and validation.
- API errors reach the agent word for word, with the cause and the next step.
- Server instructions teach the agent the safe flow: verified domain, idempotency key, then check delivery. They also tell it never to retry bounced or suppressed recipients.

**New dependency:** the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk` v1.8.0).

## v0.2.0: signed webhooks

**New endpoints:** `POST /webhooks`, `GET /webhooks`, `GET /webhooks/{id}` (with its 20 most recent deliveries) and `DELETE /webhooks/{id}`.

**Events:**
- `email.sent`, `email.delivered`, `email.delivery_delayed`, `email.bounced` and `email.failed`;
- `email.suppressed` and `email.delivery_unknown`, which Resend doesn't have.

Each event is about one recipient, and `data.to` holds only that recipient.

**Payloads** use Resend's field names, plus `data.delivery` with the SMTP code, enhanced code, response, MX host and attempt ID.

**Signing:** every request carries both the Svix headers (Resend's scheme) and the Standard Webhooks headers, so an existing Resend webhook handler verifies it unchanged.

**Delivery:**
- Webhooks go through a durable outbox in PostgreSQL, built from a cursor over the event timeline, so each event is sent once per webhook.
- Failures are retried about 8 times over roughly a day.
- Finished deliveries are purged after `OMW_RETENTION`.

**Endpoint safety:**
- Endpoints must use https.
- Connections to loopback, private, link-local and CGNAT addresses are refused after DNS resolution, and redirects are not followed.
- `OMW_WEBHOOK_ALLOW_HTTP` and `OMW_WEBHOOK_ALLOW_PRIVATE` relax this for development.

**Migration:** `0002_webhooks.sql` runs on start. It starts from the latest event, so old events are not replayed.

## v0.1.0: first public release

OpenMailWire is open-source email sending you run yourself: an HTTP API, DKIM signing, and workers that deliver straight to recipients' mail servers from your own IPs.

This is an **alpha**. The send → sign → queue → deliver → report path works end to end and is covered by tests, but it has not been run at volume.

**Sending API**
- `POST /emails`, Resend-shaped:
  - up to 50 recipients;
  - HTML and text;
  - reply-to, custom headers and tags;
  - `Idempotency-Key`;
  - per-message `expires_at` and `retry_uncertain`.
- `GET /emails/{id}`: per-recipient status, every attempt (worker, source IP, MX, TLS, the receiver's reply) and the event timeline.

**Domains**
- Add a domain to get its DNS records: 2048-bit DKIM, SPF, return-path MX and DMARC.
- Verify them against live DNS.

**Delivery workers**
- Pull jobs over HTTPS on expiring leases and send over SMTP on port 25.
- MX handling: preference order, implicit MX and null MX.
- IPv4-only by default.
- Opportunistic STARTTLS, recording whether the certificate verified.
- A durable outbox for reports.

**Reliability**
- Retry backoff from 1 minute to 4 hours, with expiry instead of endless retries.
- A lost final SMTP reply is recorded as `unknown`, not guessed.
- Expired leases are reclaimed.
- A late "delivered" report still counts.

**Safety**
- 5.1.x bounces suppress the address; 5.7.x policy blocks never suppress recipients.
- Each project's data is isolated from the others.
- API keys and worker tokens are stored only as hashes.
- DKIM keys and message bodies are encrypted at rest and purged after the retention window.
- Bounce (VERP) addresses are signed with an HMAC.

**Operations**
- One binary, `openmailwire`: `serve`, `worker`, `migrate`, `keygen`, `project`, `apikey`, `workers`, `domains` and `dev-sink`.

**Not yet**
- bounce processing;
- webhooks;
- MTA-STS and DANE;
- attachments;
- SMTP submission;
- a dashboard.

See the roadmap in the README.
