# Roadmap contracts: v0.4 to v0.7

This file fixes the interfaces between the work streams ("lanes") that build the rest of the roadmap in parallel. A lane may add things it needs. It must not change a contract here without saying so in its report.

## Shared rules (all lanes)

- **Go:** Go 1.25, module `github.com/openmailwire/openmailwire`.
  - `gofmt`, `go vet ./...` and `go test -race ./...` must pass.
  - Set `GOTOOLCHAIN=local`.
  - When the module proxy refuses a download, use `GOPROXY='https://proxy.golang.org|direct'`.
- **Dependencies:** keep them few. Approved for v0.4–v0.7:
  - `github.com/emersion/go-smtp` and `github.com/emersion/go-sasl` (SMTP servers);
  - `github.com/miekg/dns` (DANE).
  - Anything else needs a reason in the lane report.
- **Migrations:** each lane owns exactly one new file. Never edit an existing migration.

  | Lane | Migration file |
  |---|---|
  | A | `0003_keys_and_email_api.sql` |
  | B | `0004_inbound.sql` |
  | C | `0005_delivery.sql` |
  | D | `0006_submission.sql` (only if it needs one) |

- **Send rules.** Every path that queues mail calls `(*api.Server).authorizeSend(r, from, recipients)` (`internal/api/policy.go`) before storing anything. Lane A implements it; the other lanes only call it.
- **Errors** keep the shape `{"statusCode", "name", "message"}`. Messages name the cause and the next step, because AI agents read them.
- **Tests:**
  - End-to-end tests go in a new file in `internal/e2e/`, built on `newEnv(t, tweak)` (`env_test.go`).
  - Tests in that package share one database, so never call `t.Parallel()` there.
  - Never send real email; use `internal/smtptest` or the worker's `SMTPOverride`.
- **Privacy:** never log message bodies or full recipient addresses. Log the recipient's domain.
- **Webhooks:** new webhook event types go in `internal/webhook` (`Types`, `ExternalType`).
- **MCP:** new tools go in `internal/mcpserver`, and they call the HTTP API only.
- **Docs:** update `README.md` (feature list, API section, configuration tables) and `CHANGELOG.md` under `## Unreleased`.

## Lane A: API keys for agents, and the email API (v0.4)

### API keys

Migration 0003 adds these columns to `api_keys`:
- `permission text NOT NULL DEFAULT 'full_access'` (`full_access` or `sending_access`);
- `policy jsonb NOT NULL DEFAULT '{}'`;
- `last_used_at timestamptz`.

It also adds `api_key_id uuid` to `emails`, with an index on `(project_id, api_key_id, created_at)`.

Policy JSON:

```json
{
  "max_per_hour": 100,
  "max_per_day": 1000,
  "allowed_recipients": ["*@acme.com", "ops@example.org"],
  "allowed_from": ["agent@acme.com", "*@support.acme.com"]
}
```

- Every field is optional. An absent field means no limit.
- Limits count recipients queued by this key in a rolling window. Suppressed recipients do not count.
- Patterns match lowercase addresses. `*` matches any characters within the local part or the domain part.

**Who can do what:**
- `sending_access` may call:
  - `POST /emails`, `POST /emails/batch`, `POST /emails/raw`;
  - `GET /emails` (only emails sent by that key) and `GET /emails/{id}` (same);
  - `POST /emails/{id}/cancel`;
  - `GET /domains` and `GET /domains/{id}`;
  - the inbox read and reply endpoints of lane B (`GET /inboxes`, `GET /inboxes/{id}/messages`, `GET /messages/{id}`, `POST /messages/{id}/reply`).
- Everything else needs `full_access`. Otherwise answer `403` with `name: "insufficient_permission"`.

**Endpoints** (full access only):
- `GET /api-keys`: `{"object":"list","data":[{id,name,prefix,permission,policy,created_at,last_used_at}]}`.
- `POST /api-keys` with `{name, permission, policy}`: `201` with the same fields plus `"token"`, shown once.
- `GET /api-keys/{id}`.
- `PATCH /api-keys/{id}` with `{name?, policy?}`.
- `DELETE /api-keys/{id}`: revokes the key.

**Refusals** from `authorizeSend`:

| Rule broken | Status | `name` |
|---|---|---|
| sender not allowed | 403 | `sender_not_allowed` |
| recipient not allowed | 403 | `recipient_not_allowed` (the message names the first such recipient) |
| hourly or daily cap | 429 | `rate_limit_exceeded`, with a `Retry-After` header |

The CLI gains `openmailwire apikey create <project-id> <name> [--sending-only] [--max-per-hour N] [--max-per-day N] [--allow-to PATTERN]... [--allow-from PATTERN]...` and `openmailwire apikey list <project-id>`.

### Email API

**`POST /emails`** gains:
- `attachments`: `[{filename, content (base64), content_type?, content_id?}]`.
  - A `content_id` makes the part inline, with a `multipart/related` structure.
  - At most 10 MB decoded in total, and at most 20 files.
  - The request-body limit for `/emails`, `/emails/batch` and `/emails/raw` is 20 MB.
- `scheduled_at`: RFC 3339, in the future and at most 30 days ahead.
  - Deliveries get `next_attempt_at = scheduled_at`, plus an event `scheduled`.
  - `expires_at` defaults to `scheduled_at + default expiry`.

**`POST /emails/{id}/cancel`:**
- Every delivery still `queued` with `attempts = 0` becomes `canceled`, with event `canceled`.
- `409 cannot_cancel` if nothing could be canceled.
- Migration 0003 extends the `deliveries.status` CHECK with `canceled`.

**`POST /emails/batch`:** a JSON array of 1–100 send requests (same shape as `POST /emails`, without attachments).
- Every item is validated (and `authorizeSend`-checked) first. Then all are stored in one transaction.
- The response is `{"data":[{"id"}]}`.
- An `Idempotency-Key` on the batch becomes `<key>:<index>` per item.

**`GET /emails?limit=20&before=<email id>&api_key_id=<id>`** returns `{"object":"list","data":[{id,from,to,subject,created_at,scheduled_at,last_event,api_key_id}],"has_more","next_before"}`, newest first.

**Webhook type** `email.canceled`, mapped from event `canceled`.

**MCP:** `send_email` gains `scheduled_at`, plus two new tools: `list_emails` and `cancel_email`.

## Lane B: inbound mail (v0.5)

**New role `openmailwire inbound`:** an SMTP server for incoming mail (go-smtp).

| Setting | Default / meaning |
|---|---|
| `OMW_INBOUND_LISTEN` | `:25` |
| `OMW_INBOUND_HOSTNAME` | the name it announces |
| `OMW_TLS_CERT`, `OMW_TLS_KEY` | optional; enable STARTTLS |
| `OMW_CONTROL_URL` | the control plane |
| `OMW_WORKER_TOKEN` | inbound nodes authenticate as workers (`omwk_…`) |
| `OMW_INBOUND_MAX_SIZE` | `25MB` |

**How it uses the control plane:**
- It checks each `RCPT TO` with `POST /internal/inbound/check` `{"rcpt"}` and gets `{"accept": bool, "kind": "bounce"|"inbox"|"complaint"}`. Unknown addresses get `550 5.1.1` at RCPT time, so no backscatter is ever sent.
- After `DATA`, it posts `POST /internal/inbound` `{"mail_from","rcpts":[],"raw":base64,"remote_ip","helo","tls":bool}`.

**What the control plane does with an accepted message:**
- **Bounce** (the recipient is a VERP address on a known return path and its HMAC checks out):
  - Parse the DSN (`multipart/report; report-type=delivery-status`).
  - With `Action: failed`, the delivery becomes `bounced`. This covers a bounce that arrives after a `delivered`.
  - Record event `bounced` with `{"source":"dsn","status","diagnostic"}`, and suppress the address on 5.1.x as today.
  - Webhook `email.bounced`.
- **Complaint** (ARF, RFC 5965; `report-type=feedback-report`; sent to `OMW_FBL_ADDRESS` or to a VERP address):
  - Find the email by the embedded original `Message-ID`, and the recipient by `Original-Rcpt-To` or the original `To`.
  - Event `complained`, suppression reason `complaint`.
  - Webhook `email.complained`.
- **Inbox** (the recipient is an inbox address): store the message and fire webhook `email.received`.

**Inboxes** (migration 0004 adds `inboxes` and `inbound_messages`):
- An inbox address must be on one of the project's domains. Its domain needs an MX record pointing to `OMW_INBOUND_MX`.
- `internal/domains` gains an optional `INBOUND` MX record. It is shown only when `OMW_INBOUND_MX` is set, and is not required for sending.
- `POST /inboxes {"address"}` (full access), `GET /inboxes`, `DELETE /inboxes/{id}`.
- `GET /inboxes/{id}/messages?limit&before&thread_id` returns `{data:[{id,from,to,subject,received_at,thread_id,in_reply_to_email_id,snippet}],has_more,next_before}`.
- `GET /messages/{id}` returns `{id,inbox_id,from,to,cc,subject,text,html,headers:{message-id,in-reply-to,references,date},attachments:[{index,filename,content_type,size}],thread_id,in_reply_to_email_id,received_at}`.
- `GET /messages/{id}/attachments/{index}` returns the raw bytes.
- `POST /messages/{id}/reply {"text","html"}`:
  - Sends from the inbox address to the original `Reply-To` or `From`.
  - Subject `Re: …`, with `In-Reply-To` and `References` set.
  - It goes through `authorizeSend` and the normal send path, and returns `{"id"}`.

**Threading:**
- A message whose `In-Reply-To` or `References` matches one of our sent emails' Message-IDs gets `in_reply_to_email_id`.
- `thread_id` is the id of the first email or message in the chain.
- Sent emails that are replies get the same `thread_id` (add `emails.thread_id`).

**Retention:** `OMW_INBOUND_RETENTION` (default `720h`) purges inbound message bodies.

**MCP tools:**
- `list_inboxes`;
- `list_messages` (`inbox`, optional `wait_seconds` up to 60, to wait for new mail);
- `read_message`;
- `reply_to_message`;
- `wait_for_reply` (`email_id`, `wait_seconds` up to 120): returns the inbound messages that answer that email.

## Lane C: delivery quality (v0.6), mostly in the worker

### MTA-STS (RFC 8461)
- The worker looks up `_mta-sts.<domain>` TXT, fetches `https://mta-sts.<domain>/.well-known/mta-sts.txt`, and caches it by `id` and `max_age`.
- **`enforce` mode:**
  - Only MX hosts that match the policy are used.
  - TLS must succeed with a certificate valid for the MX host.
  - Otherwise the outcome is `deferred` with enhanced code `4.7.5` and a response naming MTA-STS.
- **`testing` mode:** only recorded.
- The `tls` field becomes `verified+mta-sts` when the policy was enforced.

### DANE (RFC 7672)
- Only with `OMW_DNSSEC_RESOLVER=host:53`, a validating resolver the worker trusts.
- Look up `_25._tcp.<mx>` TLSA with the DO bit set, and trust it only when the AD bit is set.
- If usable TLSA records exist (DANE-EE 3, DANE-TA 2), TLS is required and must match them. Otherwise defer with `4.7.5`.
- The `tls` field becomes `dane`.

### Adaptive throttling (control plane)
- Migration 0005 adds `receiver_state(rcpt_domain pk, limit int, backoff_until timestamptz, updated_at)`.
- **A throttling deferral** (code 421, enhanced `4.7.0`, `4.7.28`, `4.7.650` or `4.3.2`, or a response containing "rate" / "too many"):
  - halves the domain's limit (minimum 1);
  - sets `backoff_until = now + 2^n minutes` (capped at 30 min).
- **Every delivered report** raises the limit by 1, up to `OMW_DOMAIN_CONCURRENCY`.
- **Lease** uses that limit and skips domains whose backoff hasn't passed.
- Events `throttled` and `throttle_relaxed` are recorded on the email whose report triggered them.

### Fallback relay worker
- **Worker settings:** `OMW_RELAY_HOST`, `OMW_RELAY_PORT` (default 587), `OMW_RELAY_USERNAME`, `OMW_RELAY_PASSWORD`, `OMW_RELAY_TLS` (`starttls` or `implicit`). These make the worker `kind = "relay"`.
- **Protocol:** `HeartbeatRequest.Kind` and `LeaseRequest.Kind`; the `workers` table gets a `kind` column (migration 0005).
- **Direct workers** lease as today, minus deliveries routed to relay.
- **Relay workers** lease only deliveries with `attempts >= OMW_RELAY_AFTER_ATTEMPTS` (default 2), or whose domain is in backoff. Without a live relay worker, nothing changes.
- **Delivery:** STARTTLS (required), `AUTH PLAIN`, the same MAIL/RCPT/DATA handling and outcome mapping. The message is unchanged, so DKIM stays valid.

## Lane D: SMTP submission (v0.6)

**New role `openmailwire submission`:**

| Setting | Default / meaning |
|---|---|
| `OMW_SUBMISSION_LISTEN` | `:587` (STARTTLS required before AUTH) |
| `OMW_SUBMISSION_TLS_LISTEN` | optional, e.g. `:465` (implicit TLS) |
| `OMW_TLS_CERT`, `OMW_TLS_KEY` | required |
| `OMW_API_URL` | the control plane |

**Behaviour:**
- `AUTH PLAIN` or `LOGIN`: any username; the password is an API key (`omw_…`).
- On `DATA`, it calls `POST /emails/raw` on the control plane with that key.
- API answers become SMTP replies:

  | API answer | SMTP reply |
  |---|---|
  | 2xx | `250 2.0.0 queued as <id>` |
  | 401 | `535 5.7.8` |
  | 403 | `550 5.7.1` + message |
  | 422 | `554 5.6.0` + message |
  | 429 | `451 4.7.1` + message |
  | 5xx | `451 4.3.0` |

**Control plane `POST /emails/raw`** `{"raw": base64, "mail_from"?, "rcpts"?}`:
- Parse the headers. The `From` domain must be verified in the project.
- Envelope recipients: `rcpts`, or else `To` and `Cc`.
- Remove any `Bcc` header. Add `Message-ID` and `Date` when missing.
- `authorizeSend`, DKIM-sign, then store like any email: subject from the header, `api_key_id`, idempotency.
- `Idempotency-Key` is honoured.

## Lane E: dashboard (v0.7)

- Served by the control plane at `/dashboard`, from files embedded in the binary (`go:embed`). Plain HTML, CSS and JavaScript: no build step and no CDN.
- **Sign-in:** paste an API key, kept in `sessionStorage`.
- **Pages:**
  - emails: list and per-recipient timeline;
  - domains: records and verify button;
  - webhooks: deliveries;
  - API keys: create/revoke and policies, full access only;
  - inboxes and threads, once lane B is merged.
- The visual identity follows the logo (`docs/assets`): ink `#0E1726`, signal `#19D3A6`. It must work at phone width and in dark mode.

## Lane F: SDKs (v0.7)

- **Location:** `sdks/node` (TypeScript, no runtime dependencies, `fetch`), `sdks/python` (standard library only), `sdks/go` (its own module `github.com/openmailwire/openmailwire/sdks/go`), `sdks/php` (PHP 8.1+, curl).
- **Coverage:**
  - emails: send, get, list, cancel, batch, raw;
  - domains: add, list, get, verify, delete;
  - webhooks, API keys, inboxes and messages: list, get, reply, wait for reply;
  - a webhook signature verifier compatible with the Svix and Standard Webhooks headers.
- **Errors** become a typed exception or error carrying `statusCode`, `name` and `message`.
- Each SDK has its own README and tests against a local mock server.
- Publishing to npm, PyPI and Packagist needs the maintainers' accounts, so prepare the package metadata but do not publish.

## Lane G: docs for people and models (v0.7)

- `GET /openapi.json`: OpenAPI 3.1 for every public endpoint, embedded in the binary and also kept in `docs/openapi.json`.
- `GET /llms.txt`: a short, model-oriented guide (what OpenMailWire is, auth, the safe send flow, every endpoint in one line, error names, MCP setup), also kept at `docs/llms.txt`.
- The README is updated to match.
