# Changelog

## Unreleased

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
