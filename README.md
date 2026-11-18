<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo.svg" width="96" height="96" alt="OpenMailWire logo: an envelope with an antenna">
  </picture>
</p>

<h1 align="center">OpenMailWire</h1>

<p align="center">
  <b>Open-source email for AI agents</b>, and for the apps they run in.<br>
  Send from your own domains and IPs, with every delivery attempt on record.
</p>

<p align="center">
  <a href="https://github.com/openmailwire/openmailwire/actions/workflows/ci.yml"><img src="https://github.com/openmailwire/openmailwire/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/openmailwire/openmailwire/releases"><img src="https://img.shields.io/github/v/release/openmailwire/openmailwire" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="License: AGPL-3.0"></a>
</p>

An HTTP API for email, DKIM signing, and workers that deliver straight to your recipients' mail servers from your own IP addresses. Built so that an AI agent can use it as safely as a developer can.

## Built for AI agents

Agents are becoming email's busiest senders: support agents, sales agents, agents that confirm, remind and follow up. They need more than an SMTP relay:

| What an agent needs | OpenMailWire | Status |
|---|---|---|
| **Use email as a tool.** Send, check delivery and set up domains from Claude, Cursor or any MCP client. | an MCP server: `openmailwire mcp` (stdio) and `/mcp` (remote) | available (v0.3) |
| **Limits it cannot talk its way past.** A looping agent must not spam. | API keys per agent: sending-only permission, send caps per hour and day, sender and recipient allowlists, enforced by the server | available (v0.4) |
| **Facts, not guesses.** "Did it arrive?" needs a real answer. | a per-recipient timeline with the receiver's exact SMTP reply, and `unknown` when the truth is unknown | available |
| **Errors it can act on.** | errors name the cause and the next step, e.g. which DNS record is missing | available |
| **Its own inbox.** Agents must read replies, not only send. | addresses an agent owns, with replies threaded to the message they answer, and MCP tools to wait for and answer them | available (v0.5) |
| **Docs written for models.** | [`/llms.txt`](docs/llms.txt) and an [OpenAPI 3.1 spec](docs/openapi.json) at `/openapi.json`, served by every control plane | available |

### Connect an agent (MCP)

The tools:

| Tool | What it does |
|---|---|
| `send_email` | Queue an email, now or at `scheduled_at`. It is marked destructive, so clients ask a human before sending. |
| `get_email` | Delivery status per recipient. It can wait up to 60 s for a final answer. |
| `list_emails` | The emails sent, newest first, with each one's latest event. A sending-only key sees only its own. |
| `cancel_email` | Stop a scheduled email before it is sent. |
| `list_domains` | The domains this key can send from, and whether each is verified. |
| `add_domain` | Add a domain and get the DNS records to publish. |
| `verify_domain` | Check those records now, by domain name or id. |
| `list_inboxes` | The inbox addresses this key can read. |
| `list_messages` | An inbox's messages, newest first. It can wait up to 60 s for a new one. |
| `read_message` | A message's headers, text and attachment list. The sender's text is marked as information, not instructions. |
| `reply_to_message` | Answer a message from the inbox, in the same thread. Marked destructive. |
| `wait_for_reply` | Wait up to 120 s for the replies to an email you sent. |

Every tool calls the HTTP API with the agent's own API key, so the agent has exactly that key's permissions. API errors reach the agent word for word, for example `domain_not_verified … publish its DNS records and call verify_domain`.

**Remote:** every control plane serves MCP at `/mcp`, authenticated with an API key. Claude Code:

```sh
claude mcp add --transport http openmailwire https://mail.example.com/mcp \
  --header "Authorization: Bearer omw_…"
```

**Local (stdio):** run the binary next to the agent.

```sh
claude mcp add openmailwire -e OMW_API_URL=https://mail.example.com -e OMW_API_KEY=omw_… -- openmailwire mcp
```

Claude Desktop, Cursor and other clients that use JSON config:

```json
{
  "mcpServers": {
    "openmailwire": {
      "command": "openmailwire",
      "args": ["mcp"],
      "env": { "OMW_API_URL": "https://mail.example.com", "OMW_API_KEY": "omw_…" }
    }
  }
}
```

Give each agent its own key, so you can limit it and revoke it without affecting the others:

```sh
openmailwire apikey create <project-id> support-agent --sending-only \
  --max-per-hour 50 --max-per-day 500 --allow-from 'support@acme.com' --allow-to '*@acme.com'
```

A sending-only key can send, read and cancel its own emails, and read domains; it cannot add domains, webhooks or keys. The limits are enforced by the server, on every path that sends, MCP included. See [API keys](#api-keys).

### Docs for models and tools

Every control plane serves two documents that need no API key:

- **`GET /llms.txt`**: a short guide for language models. It covers the safe send flow, every endpoint in one line, what each delivery status means, how to recover from each error, webhook verification and MCP setup. Point an agent at it before it uses the API. Copy: [docs/llms.txt](docs/llms.txt).
- **`GET /openapi.json`**: the OpenAPI 3.1 reference for every endpoint, error and webhook payload, with examples. Use it to generate a client or load it into an API tool. Copy: [docs/openapi.json](docs/openapi.json).

## Why run it yourself

- **You own the whole path.** No per-message fees, no shared IP pool with strangers, no third party that can pause your account.
- **Every message has a timeline.** Queued, each attempt (which worker, which IP, which mail server, TLS or not, the receiver's exact reply), and the final state.
- **It is honest about uncertainty.** If a connection drops after the message body was sent but before the receiver replied, the attempt is recorded as `unknown`. It is not quietly marked sent or failed.
- **The API is shaped like Resend's.** `POST /emails` takes `from`, `to`, `subject`, `html` and `text`, so existing code moves with a base-URL change.

> **Status: alpha (v0.4).** The send → sign → queue → deliver → report path works end to end and is covered by tests. It has not been run at volume yet. Read the [roadmap](#roadmap) before you send production login mail through it.

## How it works

```
  your app ──HTTPS + API key──▶  control plane  ────  PostgreSQL
                                 · REST API            (queue, attempts,
                                 · DKIM signing         events, keys)
                                 · retries, expiry
                                 · suppression list
                                        ▲
                     workers pull jobs  │  and report results (HTTPS)
                    ┌───────────────────┴──────────────────┐
              worker A (IP 1)                        worker B (IP 2)
                    │ SMTP, port 25                        │
                    ▼                                      ▼
             recipients' mail servers (Gmail, Outlook, Yahoo, …)
```

- The **control plane** owns the API, the PostgreSQL queue, DKIM keys and every scheduling decision. A message is committed to the database before the API answers.
- **Workers** are stateless senders. They run anywhere with outbound port 25 and a clean IP. They pull jobs, look up MX records, speak SMTP with opportunistic TLS, and report the outcome. They never hold DKIM keys.
- Each recipient gets an expiring **lease** and a unique **attempt ID**. If a worker disappears, its lease expires and the attempt is recorded as `unknown`. Workers write each report to a local outbox before sending it, so an outcome survives a crash or a control-plane outage.
- The control plane **learns each receiver's limits**. A "slow down" deferral halves that domain's concurrency and pauses it; deliveries raise it again.
- An optional **relay worker** hands mail to another provider's smarthost when your own IPs could not deliver it.
- **Inbound nodes** (optional) receive mail from the internet on port 25: replies to agents' inboxes, bounces and spam complaints. Like workers they hold nothing: each recipient is checked with the control plane, and each message is stored there before the sender gets `250`.

More detail: [docs/architecture.md](docs/architecture.md).

## Features

- `POST /emails`: up to 50 recipients (to, cc and bcc together), HTML and text, attachments (inline images too), reply-to, custom headers, tags, `Idempotency-Key`, per-message expiry, and scheduled sends that can be canceled.
- `POST /emails/batch`: up to 100 emails, stored all or nothing.
- `POST /emails/raw`: send a complete message you built yourself (any MIME structure). OpenMailWire removes `Bcc`, adds `Message-ID` and `Date` if missing, and signs it.
- SMTP submission (`openmailwire submission`) on ports 587 and 465, for apps that only speak SMTP. Clients log in with an API key.
- `GET /emails/{id}`: the status of each recipient, every attempt, and the event timeline. `GET /emails` lists them, newest first.
- API keys with a permission (full or sending-only) and a sending policy: caps per hour and per day, and sender and recipient allowlists. Every email records the key that sent it.
- Domains: add a domain, get its DNS records (DKIM 2048-bit, SPF, return-path MX, DMARC), and verify them against live DNS.
- Delivery:
  - MX lookup with preference order, implicit MX and null MX (RFC 7505).
  - IPv4-only by default.
  - Opportunistic STARTTLS, recording whether the certificate verified.
  - **MTA-STS** (RFC 8461): where a receiving domain publishes an `enforce` policy, mail goes only to the MX hosts it names, over TLS with a valid certificate, or waits (`4.7.5`). Policies in `testing` mode are recorded, not enforced.
  - **DANE** (RFC 7672), with a validating resolver (`OMW_DNSSEC_RESOLVER`): an MX host with DNSSEC-signed TLSA records gets TLS only with a matching certificate.
  - A per-domain concurrency cap shared by all workers.
  - **Adaptive throttling**: a deferral that means "slow down" (421, `4.7.28`, `4.7.650`, "too many", "rate") halves the domain's concurrency and pauses it for 1, 2, 4… up to 30 minutes. Each delivery raises the limit by one again.
  - **Fallback relay worker**: deliveries that failed twice on your IPs, or whose domain is paused, can go out through another provider's smarthost (STARTTLS and `AUTH PLAIN`). The message is unchanged, so DKIM still verifies.
- Retries: a backoff schedule (1 min → 4 h), with expiry instead of endless retries.
- Recipient failures (5.1.x) are suppressed automatically. Policy blocks (5.7.x) never suppress recipients.
- Each project has its own API keys, domains, messages and suppression list.
- Inbound mail (`openmailwire inbound`), see [Inbound mail](#inbound-mail):
  - inboxes on your domains, with messages threaded to the emails they answer, attachments, and replies that go through the normal send path;
  - bounce processing: delivery status notifications (RFC 3464) to the signed VERP address mark a delivery `bounced`, even after `delivered`;
  - complaint processing: feedback-loop reports (ARF, RFC 5965) become `email.complained` and suppress the address.
- Signed webhooks, verifiable by handlers written for Resend. They are retried for about a day and blocked from internal networks.
- Message bodies and DKIM keys are encrypted at rest (AES-256-GCM). Bodies are purged after the retention window.
- A web dashboard at `/dashboard/`: emails with per-recipient timelines, domains and their DNS records, webhooks, API keys and inboxes. No build step and no third-party requests.
- `GET /openapi.json` (OpenAPI 3.1) and `GET /llms.txt` (a guide for models), served without a key.
- One static binary, `openmailwire`, plays every role: `serve`, `worker`, `submission` or `inbound`.
- SDKs for Node.js/TypeScript, Python, Go and PHP ([`sdks/`](sdks)), MIT licensed.

## Quick start (local, no email leaves your machine)

You need Go 1.25+ and PostgreSQL 14+.

```sh
go build -o bin/openmailwire ./cmd/openmailwire

export DATABASE_URL='postgres://openmailwire:openmailwire@localhost:5432/openmailwire?sslmode=disable'
export OMW_SECRET_KEY=$(bin/openmailwire keygen)   # keep it: it encrypts DKIM keys and queued mail

bin/openmailwire project create demo     # prints an API key (omw_…)
bin/openmailwire workers add local-1     # prints a worker token (omwk_…)

bin/openmailwire dev-sink 127.0.0.1:2525 &          # prints every message it receives
OMW_LISTEN=127.0.0.1:8080 bin/openmailwire serve &
OMW_CONTROL_URL=http://127.0.0.1:8080 \
OMW_WORKER_TOKEN=omwk_… \
OMW_SMTP_OVERRIDE=127.0.0.1:2525 \
  bin/openmailwire worker &
```

Add a domain, then send a message:

```sh
curl -X POST localhost:8080/domains \
  -H "Authorization: Bearer omw_…" -H 'Content-Type: application/json' \
  -d '{"name": "example.com"}'

# Locally you can skip DNS. Never do this in production:
bin/openmailwire domains force-verify example.com

curl -X POST localhost:8080/emails \
  -H "Authorization: Bearer omw_…" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: signup-42' \
  -d '{
        "from": "Acme <hello@example.com>",
        "to": ["you@example.org"],
        "subject": "Your sign-in code",
        "text": "Your code is 482913",
        "html": "<p>Your code is <b>482913</b></p>"
      }'
# {"id":"ed01a077-…"}

curl localhost:8080/emails/ed01a077-… -H "Authorization: Bearer omw_…"
```

With Docker, `docker compose up` starts PostgreSQL, the control plane and [Mailpit](https://mailpit.axllent.org) (a web inbox at http://localhost:8025). See the comments in `docker-compose.yml`.

## Send over SMTP

Some apps can only send through an SMTP server: WordPress, Ghost, Grafana, Jira, printers, older code. `openmailwire submission` is an SMTP submission server for them. A client logs in with an API key, and each message goes to the control plane's [`POST /emails/raw`](#post-emailsraw) with that key. It gets the same checks, key limits, DKIM signature and per-recipient timeline as a message sent through the API.

```sh
OMW_API_URL=https://mail.example.com \
OMW_TLS_CERT=/etc/openmailwire/tls/fullchain.pem \
OMW_TLS_KEY=/etc/openmailwire/tls/privkey.pem \
OMW_SUBMISSION_HOSTNAME=smtp.example.com \
OMW_SUBMISSION_TLS_LISTEN=:465 \
  openmailwire submission          # :587 with STARTTLS, plus :465 with implicit TLS
```

Settings for the app:

| Setting | Value |
|---|---|
| Server | your submission host, e.g. `smtp.example.com` |
| Port and security | `587` with STARTTLS (often labelled "TLS"), or `465` with implicit TLS ("SSL/TLS") if you enabled it |
| Authentication | `PLAIN` or `LOGIN` |
| Username | anything; it is ignored. The app's name is a good choice. |
| Password | an API key (`omw_…`). Give each app its own, so you can revoke one alone. |
| From address | an address on a verified domain of the key's project |

**WordPress** (WP Mail SMTP, mailer "Other SMTP"): SMTP Host `smtp.example.com`, Encryption `TLS`, SMTP Port `587`, Authentication on, SMTP Username `wordpress`, SMTP Password `omw_…`. Set From Email to an address on your verified domain.

**Node.js** (Nodemailer):

```js
const transport = nodemailer.createTransport({
  host: "smtp.example.com", port: 587, secure: false, requireTLS: true,
  auth: { user: "my-app", pass: process.env.OMW_API_KEY },
});
await transport.sendMail({ from: "hello@example.com", to: "you@example.org", subject: "Hi", text: "Sent over SMTP" });
```

**Python** (standard library):

```python
import os, smtplib
from email.message import EmailMessage

msg = EmailMessage()
msg["From"], msg["To"], msg["Subject"] = "hello@example.com", "you@example.org", "Hi"
msg.set_content("Sent over SMTP")
with smtplib.SMTP("smtp.example.com", 587) as s:
    s.starttls()
    s.login("my-app", os.environ["OMW_API_KEY"])
    s.send_message(msg)
```

**Django:** `EMAIL_HOST = "smtp.example.com"`, `EMAIL_PORT = 587`, `EMAIL_USE_TLS = True`, `EMAIL_HOST_USER = "django"`, `EMAIL_HOST_PASSWORD = os.environ["OMW_API_KEY"]`.

**Rails:**

```ruby
config.action_mailer.smtp_settings = {
  address: "smtp.example.com", port: 587, authentication: :plain, enable_starttls_auto: true,
  user_name: "rails", password: ENV["OMW_API_KEY"]
}
```

**Postfix**, to relay a whole server's outgoing mail:

```
# /etc/postfix/main.cf
relayhost = [smtp.example.com]:587
smtp_sasl_auth_enable = yes
smtp_sasl_password_maps = hash:/etc/postfix/sasl_passwd   # [smtp.example.com]:587 postfix:omw_…
smtp_sasl_security_options = noanonymous
smtp_tls_security_level = encrypt
```

**Test from a shell** with [swaks](https://jetmore.org/john/code/swaks/):

```sh
swaks --server smtp.example.com:587 --tls --auth PLAIN --auth-user test --auth-password omw_… \
  --from hello@example.com --to you@example.org --header "Subject: test"
```

How it behaves:
- **TLS first.** On port 587, `AUTH` is neither offered nor accepted until the client has run `STARTTLS`.
- **The key is checked at login,** so a wrong or revoked key gets `535 5.7.8` straight away. After three failed logins the server closes the connection.
- **Recipients** are the envelope recipients (`RCPT TO`). The `Bcc` header is removed, `Message-ID` and `Date` are added when missing, and the message is DKIM-signed. Nothing else is changed.
- **The envelope sender** (`MAIL FROM`) is not used for delivery. Mail leaves with the domain's return path, so bounces come back to OpenMailWire.
- **Limits:** 14 MB per message (announced with `SIZE`) and 50 recipients. Further recipients get `452`, and the client sends them in another transaction.
- **Retries are safe.** When a message carries its own `Message-ID` and `Date`, a byte-identical resubmission (say, after a timeout) returns the first copy's ID instead of sending it again.
- **Replies** tell the app what happened:

  | API answer | SMTP reply |
  |---|---|
  | queued | `250 2.0.0 queued as <email id>`; look it up with `GET /emails/{id}` |
  | invalid or revoked key | `535 5.7.8` |
  | refused (domain not added or not verified, key policy) | `550 5.7.1` and the API's message, e.g. `the From domain example.com is not verified yet; publish its DNS records and call POST /domains/…/verify` |
  | malformed message | `554 5.6.0` and the reason, e.g. a line longer than 998 bytes |
  | key's sending cap reached | `451 4.7.1` and the API's message; the app retries later |
  | control plane error or unreachable | `451 4.3.0`; the app retries later |

The submission server keeps no state and holds no DKIM keys. Run it wherever apps can reach it, with `OMW_API_URL` pointing at the control plane. TLS certificates: see [docs/production.md](docs/production.md#5-smtp-submission).

## API

Authenticate with `Authorization: Bearer <api key>`. Errors use Resend's shape: `{"statusCode": 422, "name": "validation_error", "message": "…"}`.

The complete reference is the OpenAPI 3.1 document at `/openapi.json` (copy: [docs/openapi.json](docs/openapi.json)).

### `POST /emails`

| Field | Type | Notes |
|---|---|---|
| `from` | string | `"Name <addr@domain>"` or `"addr@domain"`. The domain must be added and verified. |
| `to`, `cc`, `bcc` | string or string[] | 50 recipients at most across all three. |
| `subject` | string | Required. UTF-8 is fine. |
| `html`, `text` | string | At least one. With both, a `multipart/alternative` message is sent. |
| `reply_to` | string or string[] | |
| `headers` | object | Extra headers, e.g. `List-Unsubscribe`. Structural headers are refused. |
| `tags` | `[{name, value}]` | Letters, digits, `_` and `-`. |
| `expires_at` | RFC 3339 time | Default 48 h, at most 7 days. OpenMailWire makes no new attempts after this. |
| `retry_uncertain` | bool | Default `true`. When an attempt's outcome is unknown, retry it (the recipient may get two copies) or stop. Use `false` for receipts. |
| `scheduled_at` | RFC 3339 time | Send later: in the future, at most 30 days ahead. `expires_at` then counts from `scheduled_at`. Cancel with `POST /emails/{id}/cancel`. |
| `attachments` | `[{filename, content, content_type?, content_id?}]` | `content` is the file, base64-encoded. `content_type` defaults from the file extension. A `content_id` makes the file inline: refer to it in the HTML as `cid:<content_id>`. At most 20 files and 10 MB (decoded) in total. |

Header `Idempotency-Key`: the same key with the same body returns the same `id` (with `Idempotent-Replayed: true`). The same key with a different body returns `409`.

Request bodies may be up to 20 MB for `POST /emails`, `/emails/batch` and `/emails/raw`, and 4 MB elsewhere.

Attachments make the message `multipart/mixed`; inline files go in a `multipart/related` part with the HTML. Both are base64-encoded and covered by the DKIM signature.

### `POST /emails/batch`

A JSON array of 1 to 100 emails, each shaped like a `POST /emails` body without `attachments`. Every email is validated and checked against the key's policy first, and the key's caps apply to the batch as a whole; then all are stored in one transaction. If any email is refused, none is stored, and the error message names it (`emails[2]: …`).

Returns `{"data": [{"id"}, …]}` in the same order. An `Idempotency-Key` on the batch is applied to each email as `<key>:<index>`.

### `GET /emails`

`GET /emails?limit=20&before=<email id>&api_key_id=<key id>` lists emails newest first: `{"object": "list", "data": [{id, from, to, subject, created_at, scheduled_at, last_event, api_key_id}], "has_more", "next_before"}`. Pass `next_before` as `before` to get the next page. `limit` is 1 to 100. A sending-only key lists only its own emails.
### `POST /emails/raw`

Sends a complete RFC 5322 message that you built yourself, for example with your language's MIME library. Use it for structures `POST /emails` does not cover, or to move existing code that already writes MIME. The SMTP submission server uses it too.

| Field | Type | Notes |
|---|---|---|
| `raw` | string | Required. The whole message, header and body, base64-encoded. The request may be up to 20 MB, so the message can be about 15 MB. |
| `rcpts` | string[] | The envelope recipients. If you leave it out, every address in `To`, `Cc` and `Bcc` gets the message. 50 at most. |
| `mail_from` | string | Optional envelope sender. It is only checked for syntax: mail always leaves with the domain's return path, so bounces come back to OpenMailWire. |

What OpenMailWire changes, and nothing else:
- removes `Bcc` (and `Resent-Bcc`);
- adds `Message-ID` and `Date` when they are missing;
- turns bare LF or CR line endings into CRLF;
- adds a `DKIM-Signature` for the `From` domain on top. Signatures already in the message stay.

The `From` header must hold exactly one address, on a domain that is added to the project and verified. Lines longer than 998 bytes are refused (`422`); encode such content with quoted-printable or base64, which mail libraries do for you. API key limits apply as for `POST /emails`, and so does `Idempotency-Key`.

```sh
curl -X POST localhost:8080/emails/raw \
  -H "Authorization: Bearer omw_…" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: weekly-report-2026-47' \
  -d "{\"raw\": \"$(base64 < message.eml | tr -d '\n')\"}"
# {"id":"5b0c…"}
```

| Status | `name` | When |
|---|---|---|
| `422` | `validation_error` | not base64, no or several `From` addresses, a malformed header, a line over 998 bytes, no recipients |
| `403` | `domain_not_found`, `domain_not_verified` | the `From` domain is not in this project, or not verified yet |
| `409` | `idempotency_conflict` | the `Idempotency-Key` was used with a different request |
| `413` | `payload_too_large` | the request is over 20 MB |

### `GET /emails/{id}`

Returns `recipients[]` (status: `queued`, `sending`, `deferred`, `delivered`, `bounced`, `expired`, `suppressed`, `unknown` or `canceled`), `attempts[]` (worker, source IP, MX host, outcome, SMTP code, enhanced code, response, TLS), `events[]`, `scheduled_at` and `api_key_id` (the key that sent it).

### `POST /emails/{id}/cancel`

Cancels every recipient that is still queued and has never been attempted, typically of a scheduled email. Returns `{"object": "email", "id", "canceled": [recipients]}`, records a `canceled` event per recipient and sends the `email.canceled` webhook. Answers `409 cannot_cancel` when nothing could be canceled.

### API keys

Each key has a **permission**:

| Permission | Can call |
|---|---|
| `full_access` (default) | everything |
| `sending_access` | `POST /emails`, `POST /emails/batch`, `POST /emails/raw`; `GET /emails` and `GET /emails/{id}` for its own emails; `POST /emails/{id}/cancel` for its own emails; `GET /domains` and `GET /domains/{id}`; and `/mcp`, whose tools have the same limits |

Anything else answers `403 insufficient_permission`.

Each key can also have a **policy**. Every field is optional; a missing field means no limit.

```json
{
  "max_per_hour": 100,
  "max_per_day": 1000,
  "allowed_recipients": ["*@acme.com", "ops@example.org"],
  "allowed_from": ["agent@acme.com", "*@support.acme.com"]
}
```

- `max_per_hour` and `max_per_day` count the recipients the key queued in a rolling hour and a rolling 24 hours. Suppressed and canceled recipients do not count.
- Patterns match lowercase addresses. `*` matches any characters within the local part or within the domain part, so `*@acme.com` does not match `a@mail.acme.com`; add `*@*.acme.com` for that.

Refusals:

| Rule broken | Status | `name` |
|---|---|---|
| sender not in `allowed_from` | 403 | `sender_not_allowed` |
| a recipient not in `allowed_recipients` | 403 | `recipient_not_allowed`; the message names the first such recipient |
| hourly or daily cap | 429 | `rate_limit_exceeded`, with a `Retry-After` header (seconds) |

Endpoints (full access only):

| Method | Path | |
|---|---|---|
| `POST` | `/api-keys` | `{"name", "permission", "policy"}`. Returns `201` with the key's fields and its `token`, shown once. |
| `GET` | `/api-keys` | The project's live keys: `id`, `name`, `prefix`, `permission`, `policy`, `created_at`, `last_used_at` (updated at most once a minute). |
| `GET` | `/api-keys/{id}` | One key. |
| `PATCH` | `/api-keys/{id}` | `{"name"?, "policy"?}`. The policy is replaced whole; `{}` removes every limit. The permission cannot change. |
| `DELETE` | `/api-keys/{id}` | Revokes the key at once. |

An attempt's `tls` is one of:

| `tls` | Meaning |
|---|---|
| `none` | no TLS: the server did not offer STARTTLS |
| `opportunistic` | encrypted, but the certificate did not verify |
| `verified` | encrypted, with a certificate valid for the MX host |
| `verified+mta-sts` | as `verified`, and required by the domain's MTA-STS policy |
| `dane` | encrypted, with a certificate that matches the MX host's TLSA records |

Events also show what the control plane learned from an attempt: `throttled` (the receiver asked us to slow down; `data` has the domain's new `limit` and `backoff_until`) and `throttle_relaxed` (a delivery raised the limit again). When a domain's MTA-STS policy is in `testing` mode and enforce mode would have refused the attempt, the attempt's event (`delivered`, `deferred`, …) says why in `data.tls_policy`.

### Domains

| Method | Path | |
|---|---|---|
| `POST` | `/domains` | `{"name": "example.com"}`. Creates a DKIM key and returns the records to publish. |
| `GET` | `/domains` | Lists domains. |
| `GET` | `/domains/{id}` | One domain with its records and their last check. |
| `POST` | `/domains/{id}/verify` | Checks DNS now. The domain can send once DKIM and SPF (and the bounce MX, if configured) are found. |
| `DELETE` | `/domains/{id}` | |

The records you publish:

| Record | Name | Value |
|---|---|---|
| DKIM (TXT) | `omw<date><rand>._domainkey.example.com` | `v=DKIM1; k=rsa; p=…` |
| SPF (TXT) | `bounce.example.com` | `v=spf1 include:<your SPF include> -all`, or the workers' IPs |
| MX | `bounce.example.com` | your bounce host, when `OMW_BOUNCE_MX` is set |
| DMARC (TXT, recommended) | `_dmarc.example.com` | start with `v=DMARC1; p=none; rua=mailto:…` |
| INBOUND (MX, optional) | `example.com` | your inbound host, when `OMW_INBOUND_MX` is set. Needed only to receive mail in inboxes; sending never depends on it. |

Mail is sent with the envelope sender `b-<delivery>-<mac>@bounce.example.com`. DMARC passes through the DKIM signature, which is aligned with your `From` domain.

### Webhooks

| Method | Path | |
|---|---|---|
| `POST` | `/webhooks` | `{"endpoint": "https://…", "events": ["email.bounced", …]}`. Leave out `events` (or pass `[]`) to get every type. Returns `signing_secret` (`whsec_…`) once. |
| `GET` | `/webhooks` | Lists webhooks. |
| `GET` | `/webhooks/{id}` | One webhook with its 20 most recent deliveries. |
| `DELETE` | `/webhooks/{id}` | |

Event types:

| Type | Sent when |
|---|---|
| `email.sent` | the message is accepted and queued |
| `email.delivered` | the receiving server accepted it |
| `email.delivery_delayed` | a temporary failure, or an unknown outcome that will be retried |
| `email.bounced` | a permanent failure |
| `email.failed` | the message expired before it could be delivered |
| `email.suppressed` | the recipient is on the suppression list, so nothing was sent |
| `email.delivery_unknown` | the outcome is unknown and `retry_uncertain` is `false` |
| `email.canceled` | the recipient of a scheduled email was canceled before it was sent |
| `email.complained` | the recipient reported the email as spam through a feedback loop; `data.complaint` holds the feedback type |
| `email.received` | an inbox received a message (see below) |

Every event about a sent email is about **one recipient**, and `data.to` holds only that recipient. A bounce reported later by a delivery status notification is an `email.bounced` too, with the DSN's status and diagnostic in `data.delivery`.

`email.received` describes an inbound message without its content: `data.email_id` is the message id (read it with `GET /messages/{id}`), plus `data.inbox`, `data.inbox_id`, `data.thread_id`, `data.in_reply_to_email_id` and `data.message_id`. The body uses Resend's field names (`type`, `created_at`, `data.email_id`, `data.from`, `data.to`, `data.subject`, `data.tags`, `data.bounce`), plus `data.delivery` with the SMTP details.

Requests are signed twice with the same HMAC-SHA256 signature: once in Resend's Svix headers (`svix-id`, `svix-timestamp`, `svix-signature`) and once in the [Standard Webhooks](https://www.standardwebhooks.com) headers (`webhook-id`, `webhook-timestamp`, `webhook-signature`). A handler written for Resend webhooks verifies them unchanged.

Failed deliveries are retried after 5 s, 5 min, 30 min, 2 h, 5 h, 10 h and 10 h, then marked failed. Endpoints must use `https` and resolve to public addresses (checked when connecting). That way a webhook cannot be used to reach the control plane's internal network.

### Inboxes and received messages

Mail to an inbox is received by an inbound node (see [Inbound mail](#inbound-mail)).

| Method | Path | |
|---|---|---|
| `POST` | `/inboxes` | `{"address": "support@example.com"}`. The domain must be added to the project and verified. Full-access keys only. |
| `GET` | `/inboxes` | Lists inboxes. |
| `GET` | `/inboxes/{id}` | One inbox. |
| `DELETE` | `/inboxes/{id}` | Deletes the inbox **and its messages**. Full-access keys only. |
| `GET` | `/inboxes/{id}/messages` | `?limit=20&before=<message id>&thread_id=<id>`. Newest first: `{data:[{id, from, to, subject, received_at, thread_id, in_reply_to_email_id, snippet}], has_more, next_before}`. |
| `GET` | `/messages/{id}` | `from`, `to`, `cc`, `reply_to`, `subject`, `text`, `html`, `headers` (`message-id`, `in-reply-to`, `references`, `date`), `attachments[]` (`index`, `filename`, `content_type`, `size`), `thread_id`, `in_reply_to_email_id`, `received_at`, `body_purged`. |
| `GET` | `/messages/{id}/attachments/{index}` | The attachment's bytes, as a download. Indexes start at 0. |
| `POST` | `/messages/{id}/reply` | `{"text": "…", "html": "…"}`. Sends from the inbox address to the message's `Reply-To` (or `From`), with `Re: …`, `In-Reply-To` and `References`. Returns `{"id", "thread_id"}`. |
| `GET` | `/emails/{id}/replies` | The received messages that answer a sent email, and its `thread_id`. |

**Threading.** A received message whose `In-Reply-To` or `References` names one of the project's sent emails gets `in_reply_to_email_id`. `thread_id` is the id of the first email or message of the conversation: a reply sent with `POST /messages/{id}/reply` joins the thread of the message it answers, and the answer to that reply joins it too. For replies to reach an inbox, send from the inbox address.

**Replies are ordinary emails.** `POST /messages/{id}/reply` calls `POST /emails` with your own API key, so the domain must be verified and every send rule (key policies, suppression list) applies. An `Idempotency-Key` header is passed on. Refusals come back unchanged.

**Retention.** Message content (body and attachments) is deleted `OMW_INBOUND_RETENTION` after arrival (default 30 days). The message stays listed with its sender, subject and thread, and `body_purged` becomes `true`.

## Inbound mail

`openmailwire inbound` is an SMTP server for mail from the internet. It is stateless: for every `RCPT TO` it asks the control plane whether the address exists, and it acknowledges a message only after the control plane has stored it. Unknown addresses are refused with `550 5.1.1` during the SMTP conversation, so OpenMailWire never sends bounces of its own (no backscatter). Inbound nodes authenticate with a worker token.

It receives three kinds of mail:

| Sent to | What happens |
|---|---|
| a bounce address `b-<delivery>-<hmac>@bounce.example.com` | The HMAC is checked first; forged addresses are refused. A delivery status notification (RFC 3464, and the plain-text "failure notice" some providers still send) with `Action: failed` marks that delivery `bounced`, even if it was `delivered`, records the status and the remote server's diagnostic, suppresses the address on 5.1.x, and sends `email.bounced`. Delays, auto-replies and anything else are ignored. |
| `OMW_FBL_ADDRESS` (or a bounce address) | A feedback-loop complaint (ARF, RFC 5965; Yahoo, Microsoft and others). The email is found through the VERP envelope sender in the report, or else its Message-ID and `Original-Rcpt-To`. The address is suppressed with reason `complaint` and `email.complained` is sent. DMARC failure reports and `not-spam` feedback are ignored. |
| an inbox address | The message is sealed (AES-256-GCM), threaded, stored, and announced with `email.received`. A copy with a Message-ID the inbox already holds is stored once. |

Run it next to the control plane or anywhere that can reach it:

```sh
OMW_CONTROL_URL=https://mail.example.com \
OMW_WORKER_TOKEN=omwk_… \
OMW_INBOUND_HOSTNAME=mx.mail.example.net \
OMW_TLS_CERT=/etc/ssl/mx.crt OMW_TLS_KEY=/etc/ssl/mx.key \
  openmailwire inbound
```

Then point the MX records at it: the return-path domains through `OMW_BOUNCE_MX` and inbox domains through `OMW_INBOUND_MX` (see [docs/production.md](docs/production.md#6-inbound-mail-mx-bounces-and-complaints)).

To try it locally with the [quick start](#quick-start-local-no-email-leaves-your-machine) setup:

```sh
bin/openmailwire workers add inbound-1                      # inbound nodes use a worker token
OMW_CONTROL_URL=http://127.0.0.1:8080 OMW_WORKER_TOKEN=omwk_… \
OMW_INBOUND_LISTEN=127.0.0.1:2526 bin/openmailwire inbound &

curl -X POST localhost:8080/inboxes -H "Authorization: Bearer omw_…" \
  -H 'Content-Type: application/json' -d '{"address": "agent@example.com"}'
swaks --server 127.0.0.1:2526 --from you@example.org --to agent@example.com   # any SMTP client works
curl localhost:8080/inboxes/<inbox id>/messages -H "Authorization: Bearer omw_…"

## SDKs

Client libraries live in [`sdks/`](sdks): Node.js/TypeScript, Python, Go and PHP. Each covers the whole API (emails, domains, webhooks, API keys, inboxes and messages), can wait for delivery or for a reply, and verifies webhook signatures. They use only each language's standard library (curl for PHP), and they are MIT licensed so that apps under any licence can embed them. They are not published to npm, PyPI or Packagist yet; each README says how to install from this repository.

| Language | Install | Docs |
|---|---|---|
| Node.js / TypeScript | `npm install openmailwire` | [sdks/node](sdks/node) |
| Python | `pip install openmailwire` | [sdks/python](sdks/python) |
| Go | `go get github.com/openmailwire/openmailwire/sdks/go` | [sdks/go](sdks/go) |
| PHP | `composer require openmailwire/openmailwire` | [sdks/php](sdks/php) |

```ts
import { OpenMailWire } from "openmailwire";

const omw = new OpenMailWire(); // OMW_API_URL and OMW_API_KEY
const { id } = await omw.emails.send(
  { from: "Acme <hello@example.com>", to: "you@example.org", subject: "Hi", text: "Hello" },
  { idempotencyKey: "welcome-42" },
);
const email = await omw.waitForDelivery(id, 120_000); // every recipient final

## Dashboard

Every control plane serves a web dashboard at **`/dashboard/`** (for example `http://localhost:8080/dashboard/`). Sign in by pasting an API key.

| Page | What you can do |
|---|---|
| Emails | List sent emails (newest first, "Load more"), open one by ID, and send a test email. An email shows each recipient's status and a timeline of every attempt (worker, source IP, MX host, TLS, the receiver's SMTP reply) and event. It refreshes itself every 5 s until every recipient has a final status. |
| Domains | Add a domain, copy each DNS record, and verify it against live DNS. |
| Webhooks | Create one (its signing secret is shown once, with a copy button), delete it, and see its 20 most recent deliveries. |
| API keys | Create keys with full or sending-only access, hourly and daily caps, and recipient and sender allowlists. The token is shown once. Revoke keys. Needs a full-access key. |
| Inboxes | Add inboxes, read messages, follow a thread, download attachments, and reply. |

When the server is older than a page (for example API keys before v0.4), the page says "Not available on this server version". When the key lacks the permission, it says "This key can't manage …".

**How it keeps the key safe.** The browser holds an API key, so the dashboard is built to be boring to attack:
- The key is kept in `sessionStorage` only: it is gone when the tab closes, and never sent anywhere but the same-origin API, as `Authorization: Bearer`. No cookies, so no CSRF.
- Every dashboard response carries `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`, plus `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`. The page itself is `Cache-Control: no-store`.
- There is no inline script or style, no framework, no CDN and no third-party request. The files are embedded in the binary (`internal/dashboard/static`).
- Subjects, addresses, SMTP replies, webhook URLs and inbound mail are attacker-controlled. They are only ever inserted as text, never as HTML. Inbound HTML mail is shown as source, never rendered, and attachments always download. A test (`internal/dashboard/tests`) fails if `innerHTML`, `outerHTML`, `insertAdjacentHTML` or `document.write` appears in the scripts.

Serve the control plane over HTTPS when the dashboard is used from another machine: the key travels with every request.

To work on the dashboard, edit the files in `internal/dashboard/static` and rebuild; there is no build step for them. Its JavaScript tests need Node 22:

```sh
node --test internal/dashboard/tests/*.test.mjs
```

## Running in production

Delivering mail yourself is mostly about IP reputation and DNS hygiene, not code. Read [docs/production.md](docs/production.md) before you send real mail. In short:

1. Port 25 must be open outbound. Many clouds block it by default.
2. Each worker needs a static IPv4, matching reverse DNS (PTR), and a clean blocklist history.
3. Publish one SPF include listing your workers' IPs and set `OMW_SPF_INCLUDE`.
4. Put TLS in front of the control plane, back up PostgreSQL, and back up `OMW_SECRET_KEY`.
5. Ramp volume up slowly and watch Google Postmaster Tools.

## Configuration

Control plane (`openmailwire serve`):

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | none | PostgreSQL connection string. |
| `OMW_SECRET_KEY` | none | 32 bytes, base64 (`openmailwire keygen`). Encrypts DKIM keys and queued messages. |
| `OMW_LISTEN` | `:8080` | |
| `OMW_SPF_INCLUDE` | none | e.g. `_spf.mail.example.net`. If unset, SPF records list the workers' IPs. |
| `OMW_BOUNCE_MX` | none | Host that receives bounces for the return-path domains: your inbound node. |
| `OMW_INBOUND_MX` | none | Host that receives mail for inboxes. When set, every domain shows an optional `INBOUND` MX record. |
| `OMW_FBL_ADDRESS` | none | Address you registered with feedback loops, e.g. `fbl@mail.example.net`. Its domain's MX must point to an inbound node. |
| `OMW_INBOUND_RETENTION` | `720h` | Inbound message content is deleted this long after arrival. |
| `OMW_INBOUND_MAX_SIZE` | `25MB` | Largest inbound message the control plane accepts (MB = 1024 × 1024 bytes). |
| `OMW_RETURN_PATH_LABEL` | `bounce` | Gives `bounce.<domain>`. |
| `OMW_DEFAULT_EXPIRY` | `48h` | |
| `OMW_RETENTION` | `72h` | Message bodies are deleted this long after their last delivery. |
| `OMW_LEASE` | `15m` | Must exceed the workers' `OMW_JOB_TIMEOUT`. |
| `OMW_DOMAIN_CONCURRENCY` | `10` | In-flight deliveries per recipient domain, across all workers. Adaptive throttling lowers it for a domain that asks us to slow down, and raises it back up to this value. |
| `OMW_RELAY_AFTER_ATTEMPTS` | `2` | While a relay worker is live, deliveries with this many attempts go to it instead of direct workers. |
| `OMW_DNS_SERVER` | system | e.g. `1.1.1.1:53`, for domain checks. |
| `OMW_ALLOW_UNVERIFIED` | `false` | Development only. |
| `OMW_WEBHOOK_ALLOW_HTTP` | `false` | Development only: accept `http://` webhook endpoints. |
| `OMW_WEBHOOK_ALLOW_PRIVATE` | `false` | Development only: allow webhook endpoints on localhost or private networks. |

Worker (`openmailwire worker`):

| Variable | Default | |
|---|---|---|
| `OMW_CONTROL_URL` | none | |
| `OMW_WORKER_TOKEN` | none | From `openmailwire workers add`. |
| `OMW_EHLO` | hostname | Must equal the sending IP's reverse DNS. |
| `OMW_SOURCE_IP` | OS default | Local address to send from. |
| `OMW_PUBLIC_IP` | `OMW_SOURCE_IP` | Advertised to the control plane, used in SPF and shown in attempts. |
| `OMW_CONCURRENCY` | `20` | |
| `OMW_IPV4_ONLY` | `true` | Receivers reject IPv6 senders without IPv6 reverse DNS and SPF. |
| `OMW_JOB_TIMEOUT` | `10m` | |
| `OMW_STATE_DIR` | `./openmailwire-worker-state` | Holds the report outbox. |
| `OMW_SMTP_OVERRIDE` | none | Development only: send everything to this `host:port`. MTA-STS and DANE do not apply. |
| `OMW_DNSSEC_RESOLVER` | none | `host:port` of a DNSSEC-validating resolver the worker trusts, e.g. a local Unbound on `127.0.0.1:53`. Turns on DANE. |

SMTP submission (`openmailwire submission`):

| Variable | Default | |
|---|---|---|
| `OMW_API_URL` | `http://localhost:8080` | The control plane. Use `https://` when it runs on another machine. |
| `OMW_TLS_CERT`, `OMW_TLS_KEY` | none (required) | PEM certificate chain and private key. Re-read when the files change, so renewals need no restart. |
| `OMW_SUBMISSION_LISTEN` | `:587` | STARTTLS; clients must use it before `AUTH`. |
| `OMW_SUBMISSION_TLS_LISTEN` | none | e.g. `:465`, for implicit TLS. |
| `OMW_SUBMISSION_HOSTNAME` | hostname | Announced in the greeting. Use the name on the certificate. |
| `OMW_SUBMISSION_MAX_SIZE` | `14MB` | The largest message. 14MB is also the maximum, because `/emails/raw` takes 20 MB of base64. |

Inbound node (`openmailwire inbound`):

| Variable | Default | |
|---|---|---|
| `OMW_CONTROL_URL` | none | |
| `OMW_WORKER_TOKEN` | none | Inbound nodes authenticate as workers: `openmailwire workers add <name>`. |
| `OMW_INBOUND_LISTEN` | `:25` | |
| `OMW_INBOUND_HOSTNAME` | hostname | Announced in the greeting and in `Received` headers. Use the name your MX records point to. |
| `OMW_TLS_CERT`, `OMW_TLS_KEY` | none | PEM files. When set, STARTTLS is offered. |
| `OMW_INBOUND_MAX_SIZE` | `25MB` | Larger messages are refused with `552 5.3.4`. |

Worker admin: `openmailwire workers list`, `openmailwire workers drain <name>` (stop new jobs, finish current ones), `openmailwire workers resume <name>`.

MTA-STS needs no setting: workers look up `_mta-sts.<domain>` and fetch the policy over HTTPS (no redirects, 64 KB at most), cached by its `id` and `max_age`.

A **relay worker** is an ordinary worker with `OMW_RELAY_HOST` set. It needs its own token (`openmailwire workers add relay-1`).

| Variable | Default | |
|---|---|---|
| `OMW_RELAY_HOST` | none | The smarthost, e.g. `smtp.provider.example`. Makes this worker a relay. |
| `OMW_RELAY_PORT` | `587` | |
| `OMW_RELAY_TLS` | `starttls` | `starttls` (required, never plaintext) or `implicit` (TLS from the first byte, usually port 465). The smarthost's certificate must be valid. |
| `OMW_RELAY_USERNAME`, `OMW_RELAY_PASSWORD` | none | Credentials for `AUTH PLAIN`. Leave both empty for a smarthost that admits your IP. |

Relay workers take only deliveries that already had `OMW_RELAY_AFTER_ATTEMPTS` attempts, or whose domain is paused by throttling. While one is live (a heartbeat in the last 2 minutes), direct workers leave those deliveries to it. Without one, nothing changes.

Worker admin: `openmailwire workers list` (with each worker's kind, `direct` or `relay`), `openmailwire workers drain <name>` (stop new jobs, finish current ones), `openmailwire workers resume <name>`.

API key admin (it reads `DATABASE_URL`):

```sh
openmailwire apikey create <project-id> <name> [--sending-only] [--max-per-hour N] [--max-per-day N] \
  [--allow-to PATTERN]... [--allow-from PATTERN]...
openmailwire apikey list <project-id>
```

`--allow-to` and `--allow-from` can be repeated. The same keys can be managed over the API (`/api-keys`).

## Roadmap

**Agent-first**
- [x] **MCP server**: stdio (`openmailwire mcp`) and remote (`/mcp`). Tools to send, track, and add and verify domains (v0.3).
- [x] **Agent keys**: sending-only keys, per-key send caps (per hour, per day), recipient and sender allowlists, and an audit trail per key: every email records its key, and `GET /emails?api_key_id=` lists them (v0.4).
- [x] **`llms.txt` and OpenAPI**, served by the control plane at `/llms.txt` and `/openapi.json` (v0.5).
- [x] **Agent inboxes**: inbound SMTP, replies threaded by `In-Reply-To`, an `email.received` webhook and MCP tools to wait for and answer replies (v0.5).

**Delivery**
- [x] **Bounce processing**: an inbound listener for the return-path MX that parses DSNs and checks the VERP HMAC (v0.5).
- [x] **Signed webhooks**, Resend-compatible (v0.2).
- [x] **Complaint feedback loops**: `email.complained` from Yahoo, Microsoft and other feedback loops (v0.5).
- [x] **MTA-STS and DANE**: enforce TLS where receivers require it (v0.5).
- [x] **Adaptive throttling**: learn per-receiver limits from 421 deferrals (v0.5).
- [x] **Fallback relay worker**: hand mail to another provider when your IPs are throttled (v0.5).
- [x] Attachments, scheduled sends and a batch endpoint (v0.4).
- [x] **SMTP submission** on ports 587 and 465, and `POST /emails/raw`, for apps that only speak SMTP (v0.5).
- [x] **Dashboard** at `/dashboard/` (v0.5).
- [x] **SDKs** for Node, Python, Go and PHP, MIT licensed ([`sdks/`](sdks); not published yet) (v0.5).

## Compared with…

| | OpenMailWire | Resend | Amazon SES | Postal |
|---|---|---|---|---|
| Who runs it | you | Resend | AWS | you |
| Sending IPs | yours | shared pool or dedicated (paid) | shared pool or dedicated (paid) | yours |
| Price per message | none (you pay for servers) | plan-based | $0.10 per 1,000 | none |
| Per-attempt timeline | yes | events | via SNS/CloudWatch setup | yes |
| License | AGPL-3.0 | proprietary | proprietary | MIT |

OpenMailWire is younger than all of them. Choose it if you want to own delivery and are willing to manage IP reputation.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md). Report security problems privately: [SECURITY.md](SECURITY.md).

## License

[AGPL-3.0](LICENSE). You can run, modify and self-host OpenMailWire freely. If you offer a modified OpenMailWire to others as a network service, you must publish your changes under the same license.

The client SDKs in [`sdks/`](sdks) are MIT licensed instead, so they can be embedded in applications under any license.
