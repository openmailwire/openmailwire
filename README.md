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
| **Limits it cannot talk its way past.** A looping agent must not spam. | API keys per agent, with send caps and recipient allowlists, enforced by the server | v0.4 |
| **Facts, not guesses.** "Did it arrive?" needs a real answer. | a per-recipient timeline with the receiver's exact SMTP reply, and `unknown` when the truth is unknown | available |
| **Errors it can act on.** | errors name the cause and the next step, e.g. which DNS record is missing | available |
| **Its own inbox.** Agents must read replies, not only send. | addresses an agent owns, with replies threaded to the message they answer | v0.4 |
| **Docs written for models.** | `llms.txt` and an OpenAPI spec served by every control plane | v0.4 |

### Connect an agent (MCP)

Five tools:

| Tool | What it does |
|---|---|
| `send_email` | Queue an email. It is marked destructive, so clients ask a human before sending. |
| `get_email` | Delivery status per recipient. It can wait up to 60 s for a final answer. |
| `list_domains` | The domains this key can send from, and whether each is verified. |
| `add_domain` | Add a domain and get the DNS records to publish. |
| `verify_domain` | Check those records now, by domain name or id. |

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

Give each agent its own key (`openmailwire apikey create <project-id> <agent-name>`), so you can revoke one without affecting the others.

## Why run it yourself

- **You own the whole path.** No per-message fees, no shared IP pool with strangers, no third party that can pause your account.
- **Every message has a timeline.** Queued, each attempt (which worker, which IP, which mail server, TLS or not, the receiver's exact reply), and the final state.
- **It is honest about uncertainty.** If a connection drops after the message body was sent but before the receiver replied, the attempt is recorded as `unknown`. It is not quietly marked sent or failed.
- **The API is shaped like Resend's.** `POST /emails` takes `from`, `to`, `subject`, `html` and `text`, so existing code moves with a base-URL change.

> **Status: alpha (v0.3).** The send → sign → queue → deliver → report path works end to end and is covered by tests. It has not been run at volume yet. Read the [roadmap](#roadmap) before you send production login mail through it.

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

More detail: [docs/architecture.md](docs/architecture.md).

## Features

- `POST /emails`: up to 50 recipients (to, cc and bcc together), HTML and text, reply-to, custom headers, tags, `Idempotency-Key`, and per-message expiry.
- `POST /emails/raw`: send a complete message you built yourself (any MIME structure). OpenMailWire removes `Bcc`, adds `Message-ID` and `Date` if missing, and signs it.
- SMTP submission (`openmailwire submission`) on ports 587 and 465, for apps that only speak SMTP. Clients log in with an API key.
- `GET /emails/{id}`: the status of each recipient, every attempt, and the event timeline.
- Domains: add a domain, get its DNS records (DKIM 2048-bit, SPF, return-path MX, DMARC), and verify them against live DNS.
- Delivery:
  - MX lookup with preference order, implicit MX and null MX (RFC 7505).
  - IPv4-only by default.
  - Opportunistic STARTTLS, recording whether the certificate verified.
  - A per-domain concurrency cap shared by all workers.
- Retries: a backoff schedule (1 min → 4 h), with expiry instead of endless retries.
- Recipient failures (5.1.x) are suppressed automatically. Policy blocks (5.7.x) never suppress recipients.
- Each project has its own API keys, domains, messages and suppression list.
- Bounce addresses (VERP) carry an HMAC, ready for bounce processing.
- Signed webhooks, verifiable by handlers written for Resend. They are retried for about a day and blocked from internal networks.
- Message bodies and DKIM keys are encrypted at rest (AES-256-GCM). Bodies are purged after the retention window.
- One static binary, `openmailwire`, plays every role: `serve`, `worker` or `submission`.

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

Header `Idempotency-Key`: the same key with the same body returns the same `id` (with `Idempotent-Replayed: true`). The same key with a different body returns `409`.

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

Returns `recipients[]` (status: `queued`, `sending`, `deferred`, `delivered`, `bounced`, `expired`, `suppressed` or `unknown`), `attempts[]` (worker, source IP, MX host, outcome, SMTP code, enhanced code, response, TLS) and `events[]`.

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

Every event is about **one recipient**, and `data.to` holds only that recipient. The body uses Resend's field names (`type`, `created_at`, `data.email_id`, `data.from`, `data.to`, `data.subject`, `data.tags`, `data.bounce`), plus `data.delivery` with the SMTP details.

Requests are signed twice with the same HMAC-SHA256 signature: once in Resend's Svix headers (`svix-id`, `svix-timestamp`, `svix-signature`) and once in the [Standard Webhooks](https://www.standardwebhooks.com) headers (`webhook-id`, `webhook-timestamp`, `webhook-signature`). A handler written for Resend webhooks verifies them unchanged.

Failed deliveries are retried after 5 s, 5 min, 30 min, 2 h, 5 h, 10 h and 10 h, then marked failed. Endpoints must use `https` and resolve to public addresses (checked when connecting). That way a webhook cannot be used to reach the control plane's internal network.

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
| `OMW_BOUNCE_MX` | none | Host that receives bounces for the return-path domains. |
| `OMW_RETURN_PATH_LABEL` | `bounce` | Gives `bounce.<domain>`. |
| `OMW_DEFAULT_EXPIRY` | `48h` | |
| `OMW_RETENTION` | `72h` | Message bodies are deleted this long after their last delivery. |
| `OMW_LEASE` | `15m` | Must exceed the workers' `OMW_JOB_TIMEOUT`. |
| `OMW_DOMAIN_CONCURRENCY` | `10` | In-flight deliveries per recipient domain, across all workers. |
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
| `OMW_SMTP_OVERRIDE` | none | Development only: send everything to this `host:port`. |

SMTP submission (`openmailwire submission`):

| Variable | Default | |
|---|---|---|
| `OMW_API_URL` | `http://localhost:8080` | The control plane. Use `https://` when it runs on another machine. |
| `OMW_TLS_CERT`, `OMW_TLS_KEY` | none (required) | PEM certificate chain and private key. Re-read when the files change, so renewals need no restart. |
| `OMW_SUBMISSION_LISTEN` | `:587` | STARTTLS; clients must use it before `AUTH`. |
| `OMW_SUBMISSION_TLS_LISTEN` | none | e.g. `:465`, for implicit TLS. |
| `OMW_SUBMISSION_HOSTNAME` | hostname | Announced in the greeting. Use the name on the certificate. |
| `OMW_SUBMISSION_MAX_SIZE` | `14MB` | The largest message. 14MB is also the maximum, because `/emails/raw` takes 20 MB of base64. |

Worker admin: `openmailwire workers list`, `openmailwire workers drain <name>` (stop new jobs, finish current ones), `openmailwire workers resume <name>`.

## Roadmap

**Agent-first**
- [x] **MCP server**: stdio (`openmailwire mcp`) and remote (`/mcp`). Tools to send, track, and add and verify domains (v0.3).
- [ ] **Agent keys**: per-key send caps (per hour, per day), recipient and sender allowlists, and an audit trail per key (v0.4).
- [ ] **`llms.txt` and OpenAPI**, served by the control plane (v0.4).
- [ ] **Agent inboxes**: inbound SMTP, replies threaded by `In-Reply-To`, and an inbound webhook and MCP tool (v0.5).

**Delivery**
- [ ] **Bounce processing**: an inbound listener for the return-path MX that parses DSNs and checks the VERP HMAC.
- [x] **Signed webhooks**, Resend-compatible (v0.2).
- [ ] **Complaint feedback loops**: `email.complained` from Yahoo, Microsoft and other feedback loops.
- [ ] **MTA-STS and DANE**: enforce TLS where receivers require it.
- [ ] **Adaptive throttling**: learn per-receiver limits from 421 deferrals.
- [ ] **Fallback relay worker**: hand mail to another provider when your IPs are throttled.
- [ ] Attachments, scheduled sends and a batch endpoint.
- [x] **SMTP submission** on ports 587 and 465, and `POST /emails/raw`, for apps that only speak SMTP (v0.6).
- [ ] Dashboard, then SDKs for Node, Python, Go and PHP.

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
