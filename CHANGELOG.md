# Changelog

## Unreleased

**Docs for people and models**
- `GET /openapi.json`: an OpenAPI 3.1 description of every public endpoint:
  - emails (send with attachments, `scheduled_at`, `expires_at`, `retry_uncertain` and `Idempotency-Key`; get, list, cancel, batch and raw);
  - domains, webhooks, API keys, inboxes and messages (with reply and attachments).
  - It also documents the bearer scheme, which operations need a `full_access` key, every error name with its status, and examples for the main operations.
  - Its `webhooks` section gives each event's payload and the Svix and Standard Webhooks signature headers.
- `GET /llms.txt`: a short guide for language models. It covers auth, the safe send flow, every endpoint in one line, the delivery statuses, how to recover from each error, webhook verification and MCP setup.
- Both are embedded in the binary, need no API key, and are cached for 5 minutes (with an ETag).
- Copies live in `docs/openapi.json` and `docs/llms.txt`. A test keeps them identical to the embedded files. Another test fails when the server registers a public route that the spec does not document.

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
