# Changelog

## Unreleased

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
