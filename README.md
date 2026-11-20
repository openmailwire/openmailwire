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
| **A person in the loop, per agent.** | approval holds the server enforces: a key's first contact with someone, links, attachments or many recipients wait until a person (or a reviewing agent) approves | unreleased (v0.6) |
| **Brakes on a bad run.** One agent must not burn the domain. | circuit breakers that pause a key, domain or project whose bounce or complaint rate goes over the limit, and a per-recipient daily cap across all agents | unreleased (v0.6) |
| **Bulk-mail rules built in.** | one-click unsubscribe (RFC 8058) on broadcast mail, `Feedback-ID` on every message, a suppressions API, and a pre-send check | unreleased (v0.6) |
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
| `setup_domain_dns` | Publish a domain's DNS records at Cloudflare and verify it, with the token a person connected for the domain; the agent never handles it. `dry_run` shows the changes first. Marked destructive; full-access keys only. |

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

A sending-only key can send, read and cancel its own emails, and read domains; it cannot add domains, webhooks or keys. It reads only the inboxes you list for it (`--inbox support@acme.com`). The limits are enforced by the server, on every path that sends, MCP included. See [API keys](#api-keys).

**Received mail is untrusted input.** The inbox tools show everything a sender wrote (names, addresses, subject, body, attachment names) quoted, between two marker lines with a random tag per call, and remove text hidden with CSS. Each message carries the server's sender verdict (`authenticated`, `unauthenticated` or `failed`, from SPF, DKIM and DMARC) and a warning when its text looks written to steer an agent. Mail whose sender is not authenticated is held from agent keys until you release it.

### Docs for models and tools

Every control plane serves two documents that need no API key:

- **`GET /llms.txt`**: a short guide for language models. It covers the safe send flow, every endpoint in one line, what each delivery status means, how to recover from each error, webhook verification and MCP setup. Point an agent at it before it uses the API. Copy: [docs/llms.txt](docs/llms.txt).
- **`GET /openapi.json`**: the OpenAPI 3.1 reference for every endpoint, error and webhook payload, with examples. Use it to generate a client or load it into an API tool. Copy: [docs/openapi.json](docs/openapi.json).

## Why run it yourself

- **You own the whole path.** No per-message fees, no shared IP pool with strangers, no third party that can pause your account.
- **Every message has a timeline.** Queued, each attempt (which worker, which IP, which mail server, TLS or not, the receiver's exact reply), and the final state.
- **It is honest about uncertainty.** If a connection drops after the message body was sent but before the receiver replied, the attempt is recorded as `unknown`. It is not quietly marked sent or failed.
- **The API is shaped like Resend's.** `POST /emails` takes `from`, `to`, `subject`, `html` and `text`, so existing code moves with a base-URL change.

> **Status: alpha (v0.6).** The send → sign → queue → deliver → report path works end to end and is covered by tests. It has not been run at volume yet. Read the [roadmap](#roadmap) before you send production login mail through it.

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
- The control plane **learns each receiver's limits**, per mailbox provider (Gmail, Outlook, Yahoo… recognised by their MX hosts) and sending IP. A connection-level "slow down" from two recipients halves the concurrency and pauses that IP for that provider; deliveries raise it again. New IPs **warm up**: a daily cap per provider that grows each day.
- An optional **relay worker** hands mail to another provider's smarthost when your own IPs could not deliver it.
- **Inbound nodes** (optional) receive mail from the internet on port 25: replies to agents' inboxes, bounces and spam complaints. Like workers they hold nothing: each recipient is checked with the control plane, and each message is stored there before the sender gets `250`.

More detail: [docs/architecture.md](docs/architecture.md).

## Features

- `POST /emails`: up to 50 recipients (to, cc and bcc together), HTML and text, attachments (inline images too), reply-to, custom headers, tags, `Idempotency-Key`, per-message expiry, and scheduled sends that can be canceled.
- `POST /emails/batch`: up to 100 emails, stored all or nothing.
- `POST /emails/raw`: send a complete message you built yourself (any MIME structure). OpenMailWire removes `Bcc`, adds `Message-ID` and `Date` if missing, and signs it.
- SMTP submission (`openmailwire submission`) on ports 587 and 465, for apps that only speak SMTP. Clients log in with an API key.
- `GET /emails/{id}`: the status of each recipient, every attempt, and the event timeline. `GET /emails` lists them, newest first.
- API keys with a permission (full or sending-only), a sending policy (caps per hour and per day, sender and recipient allowlists), the networks they may be used from (`allowed_ips`) and an expiry. Every email records the key that sent it, and each key has a request rate limit.
- An append-only, hash-chained **audit log** of who changed keys, domains, webhooks and inboxes (`GET /audit-log`).
- Domains: add a domain, get its DNS records (DKIM 2048-bit, SPF, DMARC, the return-path MX), and verify them against live DNS. Each record explains the Gmail, Yahoo and Microsoft sender rules behind it.
- **Automatic DNS with Cloudflare**: publish every record a domain needs with one call, idempotently, without touching the domain's own MX or DMARC policy; a remembered token publishes DKIM rotations and Ed25519 by itself, and keeps the operator's SPF include listing the live workers' IPs. See [Automatic DNS with Cloudflare](#automatic-dns-with-cloudflare).
- HTML-only mail gets a generated `text/plain` part. Header lines are folded and never exceed 998 bytes.
- Delivery:
  - MX lookup with preference order, implicit MX and null MX (RFC 7505).
  - IPv4-only by default.
  - Opportunistic STARTTLS (TLS 1.0 or later: any encryption beats none), recording whether the certificate verified.
  - One connection per MX host carries several messages (`RSET` in between), within the concurrency limits.
  - 8-bit messages go only to servers that announce `8BITMIME`, and messages with non-ASCII addresses or header fields only to servers that announce `SMTPUTF8`; otherwise the attempt is deferred with `4.6.3`, because converting the message would break its DKIM signature.
  - RFC 5321 timeouts: a deadline per block of the message body, then 10 minutes for the final reply, and every job ends before its lease does.
  - **MTA-STS** (RFC 8461): where a receiving domain publishes an `enforce` policy, mail goes only to the MX hosts it names, over TLS with a valid certificate, or waits (`4.7.5`). Policies in `testing` mode are recorded, not enforced.
  - **DANE** (RFC 7672), with a validating resolver (`OMW_DNSSEC_RESOLVER`): MX and address lookups go through it, and an MX host with DNSSEC-signed TLSA records gets TLS only with a matching certificate. TLSA records that are all unusable still make TLS mandatory.
  - A per-domain concurrency cap shared by all workers.
  - **Throttling per mailbox provider and sending IP**: recipient domains are grouped by the provider their MX belongs to (Google, Microsoft, Yahoo, Apple, Zoho and others; [rules file](internal/policy/providers.json), replaceable with `OMW_PROVIDER_RULES`). Only connection- or IP-level replies count as "slow down" (a 421, or `4.7.0`, `4.7.28`, `4.7.650` before any recipient is named), and only after two different recipients got one: then the provider's concurrency for that IP halves and that IP pauses for 1, 2, 4… up to 30 minutes, while other IPs go on. A busy mailbox (`4.2.x`) never slows a provider down for everyone. Each delivery raises the limit by one again.
  - **Sender authentication problems** (Gmail's `4.7.26`–`4.7.32` and `5.7.x` equivalents: SPF, DKIM, DMARC or alignment failed) are recorded for the project's sending domain (`auth_problem` event, `GET /domains/{id}/dkim`) instead of throttling anyone.
  - **IP warm-up**: a new sending IP gets a daily cap per provider (50, 100, 200… by default, for about two weeks). What is over the cap waits for the next day, or goes to a relay worker when one is live. `openmailwire workers list` shows the remaining capacity.
  - **Fallback relay worker**: deliveries that failed twice on your IPs (greylisting not counted), or whose provider no direct IP can take right now, can go out through another provider's smarthost (STARTTLS and `AUTH PLAIN`). The message is unchanged, so DKIM still verifies.
- Retries: the first after 5 to 15 minutes, then 30 min, 1 h, 2 h and 4 h, with expiry instead of endless retries. Greylisting (`451 … greylisted`) is recognised: its retry comes 5 to 15 minutes later and it does not count toward the relay hand-over.
- DKIM: 2048-bit RSA, the key headers oversigned (a second `From` or `Subject` added later breaks the signature), an optional Ed25519 signature next to it (RFC 8463), and key rotation with two selectors and an overlap. Scheduled mail is dated and signed again when it is sent.
- Workers check themselves when they start: `OMW_EHLO` is a fully qualified name whose A record is `OMW_PUBLIC_IP`, the IP's reverse DNS names it, and port 25 is open. They refuse to start otherwise (`OMW_SKIP_SELF_CHECK=true` for development), and report the result in their heartbeat.
- Recipient failures (5.1.x) are suppressed automatically. Policy blocks (5.7.x) never suppress recipients.
- Each project has its own API keys, domains, messages and suppression list.
- Agent safety and compliance (see [Agent safety](#agent-safety)):
  - **approval holds**: a key's `require_approval` rules (`always`, `first_contact`, `has_attachments`, `has_links`, `recipients_over`) hold matching mail until a person or a reviewing key approves it;
  - **streams**: `transactional` and `broadcast`; broadcast mail gets a one-click unsubscribe link (RFC 8058), and every message a `Feedback-ID` for Gmail's feedback loop;
  - **suppressions API**: list, add, remove and export (CSV);
  - **per-recipient cap**: at most N emails per recipient per day, across every key of the project;
  - **circuit breakers**: a key, sending domain or project whose bounce rate goes over 5% or complaint rate over 0.1% (configurable) is paused until a person resumes it;
  - **`POST /emails/check`**: what a send would do, and why, without sending.
- Inbound mail (`openmailwire inbound`), see [Inbound mail](#inbound-mail):
  - inboxes on your domains, with messages threaded to the emails they answer, attachments, and replies that go through the normal send path;
  - bounce processing: delivery status notifications (RFC 3464) to the signed VERP address mark a delivery `bounced`, even after `delivered`;
  - complaint processing: feedback-loop reports (ARF, RFC 5965) become `email.complained` and suppress the address when a signed VERP address identifies it; forged reports change nothing;
  - a sender verdict on every received message (SPF, DKIM, DMARC: `authenticated`, `unauthenticated` or `failed`), with unauthenticated mail held from agent keys until released, and flags for text aimed at agents;
  - DMARC aggregate and TLS-RPT reports received for each domain and summarized: pass rates by source and reporter, unknown senders, TLS failures;
  - abuse limits on the inbound nodes: refused-recipient and per-address limits, a recipient cache, connection caps, and an hourly cap per inbox.
- Signed webhooks, verifiable by handlers written for Resend, and optionally with Ed25519 (Standard Webhooks `v1a`) against a published public key. Secrets rotate with an overlap in which both signatures are sent. Webhooks are retried for about a day, fairly across projects and endpoints, with a circuit breaker per endpoint, and blocked from internal networks.
- `GET /insights`: acceptance, deferral and bounce rates and time to acceptance, per mailbox provider and per sending IP, over 24 hours, 7 days and 30 days.
- Message bodies, DKIM keys and webhook secrets are encrypted at rest (AES-256-GCM). Keys rotate online (`OMW_SECRET_KEYS`, `openmailwire rotate-secrets`), and the control plane refuses to start with a key that doesn't match the data. Bodies are purged after the retention window.
- A web dashboard at `/dashboard/` to send and receive, and to run the whole system: an overview with a setup checklist, Compose and Mail, approvals, suppressions, pauses, insights, the audit log, DKIM rotation and DNS setup with Cloudflare. No build step and no third-party requests.
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
bin/openmailwire workers add local-1     # prints a delivery worker token (omwk_…)

bin/openmailwire dev-sink 127.0.0.1:1025 &          # plays the internet: prints every message workers send it
OMW_LISTEN=127.0.0.1:8080 bin/openmailwire serve &
OMW_CONTROL_URL=http://127.0.0.1:8080 \
OMW_WORKER_TOKEN=omwk_… \
OMW_SMTP_OVERRIDE=127.0.0.1:1025 \
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

With Docker, `docker compose up` starts PostgreSQL, the control plane and [Mailpit](https://mailpit.axllent.org) (a web inbox at http://localhost:8025); with the tokens in `.env`, also a delivery worker and an inbound node. See the comments in `docker-compose.yml`.

## Receiving mail

Agents read replies in inboxes. Both directions work on one machine, without the internet:

| Direction | Path | Locally |
|---|---|---|
| Send | API → worker → the recipient's mail server | the worker delivers everything to `dev-sink` (or Mailpit, with Docker) |
| Receive | a sender's mail server → inbound node → control plane → inbox | `openmailwire dev-deliver` plays the sender's mail server |

Start an inbound node. It needs a worker token with the **inbound** role. With Docker:

```sh
docker compose exec control openmailwire workers add inbound-1 --role inbound
echo "OMW_INBOUND_TOKEN=omwk_…" >> .env
echo "COMPOSE_PROFILES=worker,inbound" >> .env    # also starts the delivery worker (OMW_WORKER_TOKEN)
docker compose up -d                              # the inbound node listens on 127.0.0.1:2525
```

Without Docker, next to the quick start's processes:

```sh
bin/openmailwire workers add inbound-1 --role inbound
OMW_CONTROL_URL=http://127.0.0.1:8080 OMW_WORKER_TOKEN=omwk_… \
OMW_INBOUND_LISTEN=127.0.0.1:2525 bin/openmailwire inbound &
```

Then create an inbox, send it mail and read it:

```sh
curl -X POST localhost:8080/inboxes -H "Authorization: Bearer omw_…" \
  -H 'Content-Type: application/json' -d '{"address": "agent@example.com"}'     # on a verified domain

bin/openmailwire dev-deliver --to agent@example.com --subject "Can we meet on Friday?" --attach ./agenda.pdf
# 127.0.0.1:2525 accepted Message-ID <…dev-deliver@sender.test> from alex@sender.test for agent@example.com (… bytes)

curl localhost:8080/inboxes/<inbox id>/messages -H "Authorization: Bearer omw_…"
```

- **See it** in the dashboard (`/dashboard/`, Inboxes), or from an agent over MCP (`openmailwire mcp` or `/mcp`): `list_inboxes`, `list_messages`, `read_message`, and `reply_to_message`, whose reply goes out through the worker to dev-sink or Mailpit.
- **Try a threaded reply.** Send an email from `agent@example.com`, copy its `Message-ID` from dev-sink's output (or Mailpit), and answer it: `dev-deliver --to agent@example.com --in-reply-to '<ed01a077-…@example.com>'`. `GET /emails/{id}/replies` and the MCP tool `wait_for_reply` find the answer.
- **Held from agents.** dev-deliver's default sender (`sender.test`) cannot pass SPF, DKIM or DMARC, so its mail is `unauthenticated` and held from `sending_access` keys: full-access keys see it at once. Release a message with `POST /messages/{id}/release`, or, for a local test inbox only, turn the hold off with `PATCH /inboxes/{id}` `{"hold_unauthenticated": false}`.
- `dev-deliver` also takes `--from`, `--text`, more `--to` and `--attach`, and `--inbound host:port` (default `127.0.0.1:2525`). Inside Docker's network: `docker compose exec control openmailwire dev-deliver --inbound inbound:2525 --to agent@example.com`.
- The SMTP submission server works locally too: `COMPOSE_PROFILES=worker,inbound,submission` serves it on 127.0.0.1:1587 with a self-signed certificate (`openmailwire submission --dev-tls`), so clients must skip certificate checks: `swaks --server 127.0.0.1:1587 --tls --tls-verify=0 --auth PLAIN --auth-user test --auth-password omw_… --from hello@example.com --to you@example.org`.

On a server, the inbound node listens on port 25 and the domain's MX points to it: see [Inbound mail](#inbound-mail).

## Automatic DNS with Cloudflare

When a domain's DNS is at Cloudflare, OpenMailWire publishes its records itself, instead of you copying them by hand.

1. **Create a token** in Cloudflare: My Profile → API Tokens → Create Token → template "Edit zone DNS". Permissions: **Zone → DNS → Edit**, and optionally Zone → Email Routing → Read (then the check can tell whether Email Routing owns the domain's MX). Zone Resources: Include → Specific zone → the domain's zone. Nothing more.
2. **Look first, then apply**, with a full-access key:

```sh
curl -X POST localhost:8080/domains/<domain id>/dns/auto -H "Authorization: Bearer omw_…" \
  -H 'Content-Type: application/json' \
  -d '{"provider": "cloudflare", "api_token": "<Cloudflare token>", "dry_run": true}'
```

   The answer lists each record's `action` (`create`, `update`, `unchanged`, `skip`, `delete`) and the `warnings`. Send it again without `dry_run` to apply: the domain is then checked as by `POST /domains/{id}/verify`, and returned in `domain`.

What it publishes, and what it leaves alone:
- **Every record the domain needs**, from the same list as `GET /domains/{id}`: DKIM (also a key waiting in a rotation, the previous key during its overlap, and Ed25519), the return path's SPF and MX, DMARC, TLS reporting (`_smtp._tls`), and the `INBOUND` MX when this server receives mail (`OMW_INBOUND_MX`). Never proxied. Each new record is commented "Managed by OpenMailWire".
- **Idempotent.** A second run finds everything `unchanged` and writes nothing.
- **The domain's own mail keeps working.** When the domain already has MX records elsewhere (Google Workspace, Microsoft 365…), its MX is not added or changed, and a warning suggests a subdomain such as `agents.example.com` for agent inboxes. An existing DMARC record keeps its policy (a warning says how to add OpenMailWire's report address); a subdomain that inherits its parent's DMARC policy gets no record of its own. A DMARC record is created only where there is none: `v=DMARC1; p=none; rua=mailto:dmarc@bounce.<domain>`.
- **The return path is OpenMailWire's.** An SPF or MX record at `bounce.<domain>` that is not OpenMailWire's is replaced, and the replacement is reported.
- **Settings that break mail are reported** as warnings: Cloudflare Email Routing (it owns the apex MX), a proxied mail host or name ("orange cloud": Cloudflare's proxy carries only web traffic), a zone that is not active yet, and the zone's DNSSEC status.

**Let it keep up by itself.** With `"remember": true`, the token is kept, encrypted (`OMW_SECRET_KEYS`), for that domain. Then:
- `POST /domains/{id}/dkim/rotate` publishes the new key's record itself (see `dns_auto` in its answer), so the second call can switch signing as soon as DNS shows it, usually within a minute; the previous record is deleted when its 8-day overlap ends;
- turning on Ed25519 publishes its record;
- agents can run it with the MCP tool `setup_domain_dns` (with `dry_run` first). The tool has no token field: an agent never sees or handles one, and without a remembered token it tells the agent to ask a person to connect Cloudflare.

`GET /domains/{id}/dns/auto` shows whether a token is remembered (never the token) and the last run; `DELETE /domains/{id}/dns/auto` forgets it. The token is never returned, logged or written to the audit log, which records each run (`dns.applied`), `dns.token_remembered` and `dns.token_forgotten`.

| Error | Status | Meaning |
|---|---|---|
| `dns_provider_not_connected` | 409 | No `api_token` sent and none remembered for the domain. |
| `dns_provider_auth_failed` | 422 | Cloudflare refused the token: invalid, expired, or without Zone → DNS → Edit for the zone. |
| `dns_zone_not_found` | 422 | No zone the token can see holds the domain. Add the zone, give the token access to it, or pass `zone_id`. |
| `dns_provider_unavailable` | 503 | Cloudflare failed or kept rate-limiting. Retry after `Retry-After`; runs are safe to repeat. |
| `rate_limit_exceeded` | 429 | More than 10 runs for one domain in a short time (then one every 6 seconds), so that no loop wears out the Cloudflare account's API allowance. |

Operators can let the control plane keep their SPF include (`OMW_SPF_INCLUDE`) listing the live workers' IPs at Cloudflare: see [docs/production.md](docs/production.md#automatic-dns-with-cloudflare).

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
| `from` | string | `"Name <addr@domain>"` or `"addr@domain"`. The domain must be added and verified. Display names are at most 256 characters, addresses 254. |
| `to`, `cc`, `bcc` | string or string[] | 50 recipients at most across all three. |
| `subject` | string | Required, at most 998 bytes. UTF-8 is fine; long subjects are folded. |
| `html`, `text` | string | At least one. With both, a `multipart/alternative` message is sent. With only `html`, a `text/plain` part is made from it. |
| `reply_to` | string or string[] | |
| `headers` | object | Extra headers, e.g. `List-Unsubscribe` on transactional mail. Structural headers are refused, and so is `Feedback-ID`, which OpenMailWire sets. |
| `stream` | string | `transactional` (default) or `broadcast` (newsletters and other bulk mail). A broadcast email has one recipient and gets OpenMailWire's one-click unsubscribe headers; see [Streams and unsubscribe](#streams-and-one-click-unsubscribe). |
| `tags` | `[{name, value}]` | Letters, digits, `_` and `-`. |
| `expires_at` | RFC 3339 time | Default 48 h, at most 7 days. OpenMailWire makes no new attempts after this. |
| `retry_uncertain` | bool | Default `true`. When an attempt's outcome is unknown, retry it (the recipient may get two copies) or stop. Use `false` for receipts. |
| `scheduled_at` | RFC 3339 time | Send later: in the future, at most 30 days ahead. `expires_at` then counts from `scheduled_at`. Cancel with `POST /emails/{id}/cancel`. |
| `attachments` | `[{filename, content, content_type?, content_id?}]` | `content` is the file, base64-encoded. `content_type` (at most 255 characters) defaults from the file extension. A `content_id` makes the file inline: refer to it in the HTML as `cid:<content_id>`. At most 20 files and 10 MB (decoded) in total. |

Header `Idempotency-Key` (printable ASCII, at most 256 characters): the same key with the same body returns the same `id` (with `Idempotent-Replayed: true`). The same key with a different body returns `409`. Keys belong to the API key that sent them, so one agent's keys never replay, reveal or block another's.

Request bodies may be up to 20 MB for `POST /emails`, `/emails/batch` and `/emails/raw`, and 4 MB elsewhere. A request with a body over 1 MB needs one of the server's large-request slots (`OMW_MAX_LARGE_REQUESTS`); when none frees up within 30 seconds the answer is `503 server_busy` with `Retry-After`.

Text that PostgreSQL cannot store (a NUL character, or bytes that are not UTF-8) is refused with `422`, on every path.

Attachments make the message `multipart/mixed`; inline files go in a `multipart/related` part with the HTML. Both are base64-encoded and covered by the DKIM signature.

### `POST /emails/batch`

A JSON array of 1 to 100 emails, each shaped like a `POST /emails` body without `attachments`. Every email is validated and checked against the key's policy first, and the key's caps apply to the batch as a whole; then all are stored in one transaction. If any email is refused, none is stored, and the error message names it (`emails[2]: …`).

Returns `{"data": [{"id"}, …]}` in the same order. An `Idempotency-Key` on the batch covers each email, in a namespace of its own: it never collides with a single send's key.

### `GET /emails`

`GET /emails?limit=20&before=<email id>&api_key_id=<key id>` lists emails newest first: `{"object": "list", "data": [{id, from, to, subject, created_at, scheduled_at, last_event, api_key_id}], "has_more", "next_before"}`. Pass `next_before` as `before` to get the next page. `limit` is 1 to 100. A sending-only key lists only its own emails.

`?thread_id=<id>` lists only the emails of one conversation: the email with that id, and the replies sent with `POST /messages/{id}/reply` in its thread (a received message's `thread_id`). The answer then echoes `"thread_id"`.
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
- adds a `DKIM-Signature` for the `From` domain on top. Signatures already in the message stay;
- sets `Feedback-ID` (replacing one in the message) and, on broadcast mail, the `List-Unsubscribe` pair (replacing any in the message). Pass `"stream": "broadcast"`, or an `X-OMW-Stream: broadcast` header, which is removed (SMTP submission clients can set it).

The `From` header must hold exactly one address, on a domain that is added to the project and verified. Header names are counted ignoring case, a header name with whitespace before its colon (`From :`) is refused, and so is a second `Sender`, `Reply-To` or `Subject`. Lines longer than 998 bytes are refused (`422`); encode such content with quoted-printable or base64, which mail libraries do for you. API key limits apply as for `POST /emails`, and so does `Idempotency-Key`.

```sh
curl -X POST localhost:8080/emails/raw \
  -H "Authorization: Bearer omw_…" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: weekly-report-2026-47' \
  -d "{\"raw\": \"$(base64 < message.eml | tr -d '\n')\"}"
# {"id":"5b0c…"}
```

| Status | `name` | When |
|---|---|---|
| `422` | `validation_error` | not base64, no or several `From` headers or addresses, a malformed header, a line over 998 bytes, a Subject that is not UTF-8, no recipients |
| `403` | `domain_not_found`, `domain_not_verified` | the `From` domain is not in this project, or not verified yet |
| `409` | `idempotency_conflict` | the `Idempotency-Key` was used with a different request |
| `413` | `payload_too_large` | the request is over 20 MB |

### `GET /emails/{id}`

Returns `recipients[]` (status: `queued`, `sending`, `deferred`, `delivered`, `bounced`, `expired`, `suppressed`, `unknown`, `canceled`, `held` (waiting for approval) or `rejected`), `attempts[]` (worker, source IP, MX host, outcome, SMTP code, enhanced code, response, TLS), `events[]`, `scheduled_at` and `api_key_id` (the key that sent it).

### `POST /emails/{id}/cancel`

Cancels every recipient that is still queued and has never been attempted, typically of a scheduled email. Returns `{"object": "email", "id", "canceled": [recipients]}`, records a `canceled` event per recipient and sends the `email.canceled` webhook. Answers `409 cannot_cancel` when nothing could be canceled.

### API keys

Each key has a **permission**:

| Permission | Can call |
|---|---|
| `full_access` (default) | everything |
| `sending_access` | `POST /emails`, `POST /emails/batch`, `POST /emails/raw`; `GET /emails` and `GET /emails/{id}` for its own emails; `POST /emails/{id}/cancel` for its own emails; `GET /domains` and `GET /domains/{id}`; `POST /emails/check`; `GET /emails/held` (its own held emails); reading `GET /suppressions`, `/suppressions/export`, `/pauses` and `/project`; approving and rejecting held emails with `can_approve`; for the inboxes its policy's `inboxes` lists (and none otherwise): `GET /inboxes`, `GET /inboxes/{id}`, `GET /inboxes/{id}/messages`, `GET /messages/{id}`, its attachments, `POST /messages/{id}/reply`, and `GET /emails/{id}/replies` for its own emails, never showing held messages; and `/mcp`, whose tools have the same limits |

Anything else answers `403 insufficient_permission`.

Each key can also have a **policy**. Every field is optional; a missing field means no limit.

```json
{
  "max_per_hour": 100,
  "max_per_day": 1000,
  "allowed_recipients": ["*@acme.com", "ops@example.org"],
  "allowed_from": ["agent@acme.com", "*@support.acme.com"],
  "inboxes": ["support@acme.com", "*@agents.acme.com"]
}
```

- `max_per_hour` and `max_per_day` count the recipients the key queued in a rolling hour and a rolling 24 hours. Suppressed and canceled recipients do not count.
- `require_approval` and `can_approve` hold the key's mail for approval, and let a key approve others' held mail: see [Approval holds](#approval-holds).
- `inboxes` lists the inboxes a `sending_access` key may read and reply from. Without it the key sees no inbox, so an SMTP submission password (which is an API key) cannot read mail. Full-access keys see every inbox.
- Patterns match lowercase addresses. `*` matches any characters within the local part or within the domain part, so `*@acme.com` does not match `a@mail.acme.com`; add `*@*.acme.com` for that.

Refusals:

| Rule broken | Status | `name` |
|---|---|---|
| sender not in `allowed_from` | 403 | `sender_not_allowed` |
| a recipient not in `allowed_recipients` | 403 | `recipient_not_allowed`; the message names the first such recipient |
| hourly or daily cap | 429 | `rate_limit_exceeded`, with a `Retry-After` header (seconds) |

A key can also be limited to networks and in time:

- `allowed_ips`: addresses or CIDR prefixes (`["203.0.113.7", "10.0.0.0/8", "2001:db8::/32"]`). A request from anywhere else answers `403 ip_not_allowed`. Behind a reverse proxy, list it in `OMW_TRUSTED_PROXIES`, so that the client's address is read from `X-Forwarded-For` (the right-most address that is not a trusted proxy).
- `expires_at`: after this time the key answers `401 api_key_expired`.

Every key may make `OMW_RATE_LIMIT` requests per second on average (bursts of `OMW_RATE_LIMIT_BURST`); more answer `429 rate_limit_exceeded` with `Retry-After`. A remote MCP tool call counts once, however many API calls it makes.

Endpoints (full access only):

| Method | Path | |
|---|---|---|
| `POST` | `/api-keys` | `{"name", "permission", "policy", "allowed_ips"?, "expires_at"?}`. Returns `201` with the key's fields and its `token`, shown once. |
| `GET` | `/api-keys` | The project's live keys: `id`, `name`, `prefix`, `permission`, `policy`, `allowed_ips`, `expires_at`, `created_at`, `last_used_at` (updated at most once a minute). |
| `GET` | `/api-keys/{id}` | One key. |
| `PATCH` | `/api-keys/{id}` | `{"name"?, "policy"?, "allowed_ips"?, "expires_at"?}`. The policy is replaced whole; `{}` removes every limit. `null` removes `allowed_ips` or `expires_at`. The permission cannot change. |
| `DELETE` | `/api-keys/{id}` | Revokes the key at once. |

### Audit log

Every change to API keys, domains, webhooks and inboxes is recorded with the key that made it (or `cli` for the `openmailwire` command, `system` for the control plane itself, e.g. an expired domain claim). Tokens and secrets are never recorded.

| Method | Path | |
|---|---|---|
| `GET` | `/audit-log?limit=50&before=<id>&action=<action>` | Newest first: `{id, created_at, actor, actor_key_id, action, target_type, target_id, data, entry, prev_hash, hash}`. Full access only. |
| `GET` | `/audit-log/verify` | Recomputes the chain: `{valid, entries, head_hash, first_invalid_id, problem}`. Full access only. |

The log is append-only (the database refuses `UPDATE` and `DELETE` on it) and hash-chained per project: `hash` is SHA-256 of the previous entry's `hash` bytes (32 zero bytes for the first) followed by `entry`, the exact JSON text of the entry. Changing or removing an entry breaks the chain. Keep the `head_hash` from time to time: if it later disappears from the chain, entries were removed from its end. Actions: `api_key.created`, `api_key.updated`, `api_key.revoked`, `domain.created`, `domain.verified`, `domain.unverified`, `domain.deleted`, `domain.expired`, `webhook.created`, `webhook.deleted`, `inbox.created`, `inbox.deleted`, `dns.applied`, `dns.token_remembered`, `dns.token_forgotten`.

An attempt's `tls` is one of:

| `tls` | Meaning |
|---|---|
| `none` | no TLS: the server did not offer STARTTLS |
| `opportunistic` | encrypted, but the certificate did not verify |
| `verified` | encrypted, with a certificate valid for the MX host |
| `verified+mta-sts` | as `verified`, and required by the domain's MTA-STS policy |
| `dane` | encrypted, with a certificate that matches the MX host's TLSA records |

Events also show what the control plane learned from an attempt: `throttled` (the receiver asked us to slow down; `data` has the `scope`, a provider such as `google` or the recipient domain, the `source_ip`, the new `limit` and `backoff_until`), `throttle_relaxed` (a delivery raised the limit again) and `auth_problem` (the receiver says the sending domain's SPF, DKIM or DMARC did not pass). An attempt's event carries the SMTP `stage` its reply came from, and `greylisted: true` for greylisting. `unseal_failed` means the control plane could not decrypt the message (a different `OMW_SECRET_KEY`); nothing was sent and the delivery is retried 5 minutes later. When a domain's MTA-STS policy is in `testing` mode and enforce mode would have refused the attempt, the attempt's event (`delivered`, `deferred`, …) says why in `data.tls_policy`.

### Domains

| Method | Path | |
|---|---|---|
| `POST` | `/domains` | `{"name": "example.com"}`. Creates a DKIM key and returns the records to publish. |
| `GET` | `/domains` | Lists domains. |
| `GET` | `/domains/{id}` | One domain with its records and their last check. |
| `POST` | `/domains/{id}/verify` | Checks DNS now. The domain can send once every required record is found. |
| `DELETE` | `/domains/{id}` | |
| `GET` | `/domains/{id}/dkim` | Every DKIM key with its record and state (`active`, `pending`, `retiring`), and the authentication problems receivers reported for the domain. |
| `POST` | `/domains/{id}/dkim/rotate` | Rotates the DKIM key in two steps: the first call creates a key under a new selector and answers `202` with the TXT record to publish (`DKIM_NEXT`); call again once it is published and signing switches to it (`200`). The previous record stays listed as `DKIM_PREVIOUS` until its `retire_at`, 8 days later, because mail queued before the switch carries its signature. `409 dkim_record_not_found` until the new record is in DNS. Full access. |
| `POST` | `/domains/{id}/dkim/ed25519` | `{"enabled": true}`: an Ed25519 key (RFC 8463) in the same two steps (record `DKIM_ED25519`); then mail carries an Ed25519 signature next to the RSA one. `{"enabled": false}` removes it. Full access. |
| `GET` | `/domains/{id}/reports` | DMARC aggregate and TLS reports the domain received (see [DMARC and TLS reports](#dmarc-and-tls-reports)). `?days=30` (1–180). Full-access keys only. |
| `POST` | `/domains/{id}/dns/auto` | `{"provider": "cloudflare", "api_token"?, "zone_id"?, "dry_run"?, "remember"?}`: publishes the domain's records at Cloudflare and verifies it (see [Automatic DNS with Cloudflare](#automatic-dns-with-cloudflare)). Without `api_token`, uses the remembered token. Full access. |
| `GET` | `/domains/{id}/dns/auto` | Whether a token is remembered (never the token), the zone, and the last run. Full access. |
| `DELETE` | `/domains/{id}/dns/auto` | Forgets the remembered token; published records stay. Full access. |

The records you publish:

| Record | Name | Value |
|---|---|---|
| DKIM (TXT, required) | `omw<date><rand>._domainkey.example.com` | `v=DKIM1; k=rsa; p=…` |
| SPF (TXT, required) | `bounce.example.com` | `v=spf1 include:<your SPF include> -all`, or the workers' IPs |
| DMARC (TXT, required) | `_dmarc.example.com` | at least `v=DMARC1; p=none;`; add `rua=mailto:…` for reports. A subdomain may rely on its parent's record. |
| MX (return path) | `bounce.example.com` | your bounce host: `OMW_BOUNCE_MX`, or else `OMW_INBOUND_MX`. Required when either is set; otherwise shown as optional, and any MX for the return path passes. |
| INBOUND (MX, optional) | `example.com` | your inbound host, when `OMW_INBOUND_MX` is set. Needed only to receive mail in inboxes; sending never depends on it. |

Each record's `detail` says why it is needed until it checks out. Gmail and Yahoo (since February 2024) and Microsoft's Outlook.com (since May 2025) require from anyone sending them 5,000 or more messages a day: SPF and DKIM, a DMARC record (`p=none` is enough) aligned with the `From` domain, one-click unsubscribe on marketing mail, and a spam rate under 0.3%. Smaller senders need SPF or DKIM, and fare better with all three.

Mail is sent with the envelope sender `b-<delivery>-<mac>@bounce.example.com`. DMARC passes through the DKIM signature, which is aligned with your `From` domain, and through SPF on the return path (relaxed alignment).

A domain name is unique within a project, and among verified domains. Any project may add a name, and only the project that proves control of its DNS can verify it; a second project that tries gets `409 domain_verified_elsewhere`. An unverified domain is removed 7 days after it was added (`claim_expires_at`), and so is a failing one once another project has verified the name.

Each domain also shows `report_addresses`: `dmarc@bounce.example.com` for the DMARC record's `rua=` and `tls-rpt@bounce.example.com` for a TLS-RPT record (`_smtp._tls.example.com TXT "v=TLSRPTv1; rua=mailto:tls-rpt@bounce.example.com"`). Reports sent there are read by the inbound nodes, so the return-path domain's MX must point to one (`OMW_BOUNCE_MX`).

### Webhooks

| Method | Path | |
|---|---|---|
| `POST` | `/webhooks` | `{"endpoint": "https://…", "events": ["email.bounced", …], "asymmetric_signing": false}`. Leave out `events` (or pass `[]`) to get every type. Returns `signing_secret` (`whsec_…`) once; with `asymmetric_signing`, also `public_key` (`whpk_…`). A project has at most 10 webhooks (`OMW_WEBHOOKS_PER_PROJECT`); one more answers `409 webhook_limit_reached`. |
| `GET` | `/webhooks` | Lists webhooks. |
| `GET` | `/webhooks/{id}` | One webhook with its 20 most recent deliveries, its `public_key`, and its circuit breaker (`consecutive_failures`, `paused_until`). |
| `POST` | `/webhooks/{id}/rotate-secret` | `{"overlap_seconds": 86400}` (default one day, at most 7 days, 0 for none). Returns the new `signing_secret` once (and a new `public_key`). Until `previous_secret_expires_at`, every request carries signatures by both secrets. |
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
| `email.held` | the email is held for approval; `data.approval.reasons` lists the rules that matched |
| `email.approved` | a held email was approved and is being delivered; `data.approval` has `approved_by` (a key id) and `note` |
| `email.rejected` | a held email was rejected and will never be sent; `data.approval` has `rejected_by` and `note` |
| `sending.paused` | a circuit breaker paused a key, a domain or the project; `data` is the email whose bounce or complaint tripped it, `data.pause` the pause (scope, reason, rate, threshold, counts, id) |

Every event about a sent email is about **one recipient**, and `data.to` holds only that recipient. A bounce reported later by a delivery status notification is an `email.bounced` too, with the DSN's status and diagnostic in `data.delivery`.

`email.received` describes an inbound message without its content: `data.email_id` is the message id (read it with `GET /messages/{id}`), `data.to` and `data.inbox` the inbox address that received it (`data.header_to` is the message's own `To` header, which the sender chose), plus `data.inbox_id`, `data.thread_id`, `data.in_reply_to_email_id`, `data.message_id`, the sender verdict `data.trust` with `data.authentication` (`spf`, `dkim`, `dmarc`), `data.held` and `data.suspicious_instructions`. An `email.complained` that does not say which recipient complained has `data.to: []`. The body uses Resend's field names (`type`, `created_at`, `data.email_id`, `data.from`, `data.to`, `data.subject`, `data.tags`, `data.bounce`), plus `data.delivery` with the SMTP details.

Requests are signed twice with the same HMAC-SHA256 signature: once in Resend's Svix headers (`svix-id`, `svix-timestamp`, `svix-signature`) and once in the [Standard Webhooks](https://www.standardwebhooks.com) headers (`webhook-id`, `webhook-timestamp`, `webhook-signature`). A handler written for Resend webhooks verifies them unchanged.

- **Ed25519.** A webhook created with `"asymmetric_signing": true` also signs every request with Ed25519, as Standard Webhooks' `v1a,<base64>`, in the same headers. Receivers verify with the `public_key` from `GET /webhooks/{id}` and hold no shared secret. The SDKs' verifiers take either the `whsec_…` secret or the `whpk_…` public key.
- **Rotation.** `POST /webhooks/{id}/rotate-secret` makes a new secret (and Ed25519 key). During the overlap the header lists both signatures (`v1,<new> v1,<old>`, plus the two `v1a` ones), so a receiver can switch to the new secret at any moment in it. Verifiers accept a request when any listed signature matches.

Delivery:

- Failed deliveries are retried after 5 s, 5 min, 30 min, 2 h, 5 h, 10 h and 10 h, then marked failed.
- An endpoint must start answering within 10 s and finish within 15 s; answer `2xx` first, then process the event. Connections are reused, and closed after 30 s idle.
- Endpoints are served fairly. At most 4 requests run per endpoint and 8 per project at once, and a slot is refilled as soon as its request ends, so a slow endpoint never delays another project's.
- **Circuit breaker**: after 5 failures in a row an endpoint gets no requests for 30 s, doubling with each further failure up to 10 minutes; then one request at a time until one succeeds. Paused deliveries keep their retries.
- `recent_deliveries[].last_error` says what went wrong in general terms (`timed out: …`, `could not connect: …`, `endpoint answered 500`). The details, which can name internal addresses, go only to the control plane's log.
- Endpoints must use `https` and resolve to public addresses. Only global unicast is dialed: every range in IANA's special-purpose registries is refused, as are NAT64 addresses that embed a non-public IPv4 address and every 6to4 and Teredo address. The check runs on the address actually dialed, after DNS, so a webhook cannot reach the control plane's internal network.

### Insights

`GET /insights` (full access) reports how receivers treat your mail, from the attempts of the project's emails:

```json
{"object": "insights", "window_basis": "sent_at", "provider_grouping": "mx_host_suffix",
 "emails_considered": 1840, "truncated": false,
 "windows": [{"window": "24h",
   "total": {"attempts": 412, "accepted": 398, "deferred": 11, "bounced": 3, "unknown": 0,
             "acceptance_rate": 0.966, "deferral_rate": 0.027, "bounce_rate": 0.007,
             "time_to_acceptance_p50_seconds": 2.1, "time_to_acceptance_p95_seconds": 64.8},
   "by_provider": [{"provider": "google", "attempts": 210, …}, {"provider": "microsoft", …}],
   "by_sending_ip": [{"sending_ip": "203.0.113.7", "attempts": 300, …}]},
  {"window": "7d", …}, {"window": "30d", …}]}
```

- Windows (`24h`, `7d`, `30d`) count emails by when they were sent. Rates are per attempt: accepted, deferred or bounced, out of all attempts.
- Time to acceptance runs from when an email was queued (or scheduled for) to the receiver's acceptance.
- Providers are recognised by the MX host each attempt talked to (`google`, `microsoft`, `yahoo`, `apple`, `zoho`, `proton`, `fastmail` and more; else `other`).
- At most the newest 100,000 emails of the last 30 days are read; `truncated` says when the longer windows cover fewer.

### Overview

`GET /overview` (full access) is the state of the whole system in one cheap call, for dashboards and for an agent that checks on things:

```json
{"object": "overview", "generated_at": "2026-11-20T10:00:00Z",
 "queue": {"queued": 4, "scheduled": 1, "sending": 0, "deferred": 2, "held": 3,
           "oldest_queued_at": "2026-11-20T09:52:10Z", "oldest_age_seconds": 470},
 "sending": {"truncated": false,
   "windows": [{"window": "24h", "emails": 120, "recipients": 150, "delivered": 141, "bounced": 3, "complained": 0, "suppressed": 1,
                "delivery_rate": 0.94, "bounce_rate": 0.02, "complaint_rate": 0}, {"window": "7d", …}],
   "daily": [{"day": "2026-11-14", "recipients": 20, "delivered": 19, "bounced": 1, "complained": 0}, …]},
 "held_emails": 3, "active_pauses": 0,
 "domains": [{"id": "…", "name": "acme.com", "status": "verified", "claim_expires_at": null, "dmarc": "verified",
              "inbound": "missing", "inboxes": 2, "dkim_rotation": "idle", "ed25519": "off", "auth_issues": 0,
              "problems": ["Inbound MX record not found: its inboxes receive no mail"]}],
 "receiving": {"enabled": true, "inbound_mx": "mx.mail.example.net", "inboxes": 2, "messages_24h": 14, "held_messages": 1},
 "workers": [{"name": "w1", "kind": "direct", "role": "delivery", "ip": "203.0.113.7", "version": "0.6.0", "draining": false,
              "last_seen_at": "…", "online": true, "self_check": {"ok": true, "problems": []},
              "warmup": {"enabled": true, "started": true, "warm": false, "day": 3, "remaining": 1500, "cap": 2000, "providers": […]}}]}
```

- `queue` counts this project's recipients that are not final yet. `queued` is due now; mail waiting for its `scheduled_at` is `scheduled`. `oldest_age_seconds` is how long the oldest queued, deferred or sending recipient has waited.
- `sending` counts the emails created in the last 24 hours and 7 days (at most the newest 100,000; `truncated` says when). Every rate is out of `recipients`, and is `null` when there were none. `daily` is the last 7 UTC days, oldest first.
- `domains[].problems` says in words what needs doing; `dmarc` and `inbound` are the stored record statuses, `inbound` is `null` when the server does not receive mail (`OMW_INBOUND_MX` unset).
- `workers` is server-wide. `online` means a heartbeat in the last 2 minutes. `warmup` is `null` for inbound nodes, relays and workers without an IP.

### Agent safety

Every rule here is enforced by the server, on every path that sends: `POST /emails`, batches, `POST /emails/raw` (and so SMTP submission), inbox replies and MCP.

#### Approval holds

A key's policy can hold its mail for a person, or a reviewing agent, to approve:

```json
{
  "require_approval": {"first_contact": true, "has_links": true, "recipients_over": 5},
  "allowed_from": ["agent@acme.com"]
}
```

| Rule | Holds an email when |
|---|---|
| `always: true` | always |
| `first_contact: true` | a recipient was never sent mail by this project (held, rejected and suppressed mail do not count) |
| `has_attachments: true` | it has an attachment, inline ones included |
| `has_links: true` | its text or HTML has a web link (`mailto:`, `cid:` and `#` links do not count) |
| `recipients_over: N` | it has more than N recipients |

A held email answers `{"id", "held": true, "hold_reasons": ["first_contact"]}`. Its recipients have status `held`, and the `email.held` webhook fires. Nothing is sent until it is decided:

| Method | Path | |
|---|---|---|
| `GET` | `/emails/held` | Held emails, newest first, with `hold_reasons`, the held recipients and the first 2000 characters of the text. `?limit` (up to 50) and `?before`. Approvers see the whole project's; other keys see their own. |
| `POST` | `/emails/{id}/approve` | `{"note"?}`. The held recipients are queued (webhook `email.approved`). A recipient suppressed in the meantime is not sent to. Refused while the sender's key, domain or project is paused. |
| `POST` | `/emails/{id}/reject` | `{"note"?}`. The held recipients become `rejected` (webhook `email.rejected`) and stop counting toward caps. |

Approving and rejecting need a `full_access` key or a key whose policy has `"can_approve": true`. No key approves its own email (`403 cannot_approve_own_email`), so an agent with `can_approve` can review other agents but never wave its own mail through. Deciding twice answers `409 not_held`. Held mail that nobody decides on expires at its `expires_at` (status `expired`, webhook `email.failed`); its body is kept until then. Each decision is recorded with the deciding key.

#### Streams and one-click unsubscribe

Send newsletters and other bulk mail with `"stream": "broadcast"`. A broadcast email has exactly one recipient, so its unsubscribe link belongs to that address; send many with `POST /emails/batch`. OpenMailWire adds, and DKIM signs:

```
List-Unsubscribe: <https://mail.example.com/u/{token}>
List-Unsubscribe-Post: List-Unsubscribe=One-Click
```

The token is signed (HMAC) over the project and the address, so it cannot be forged or pointed at someone else. Gmail, Yahoo and other providers `POST` to it when the recipient clicks "Unsubscribe": the address is then suppressed for broadcast mail at once (reason `unsubscribe`, stream `broadcast`), and transactional mail still reaches it. A browser that opens the link with `GET` only gets a page with a confirm button, because link scanners open links too. `/u/` needs no key and is rate-limited per client IP. Broadcast mail needs `OMW_PUBLIC_URL`; without it, it is refused with `422 unsubscribe_unavailable`. Put a visible unsubscribe link in the body as well: `POST /emails/check` warns when there is none.

Every message also gets `Feedback-ID: <stream>:<api key id>:<project id>:omw`, by which Gmail's feedback loop (Postmaster Tools) reports complaint rates, so a misbehaving agent shows up by its key.

#### Suppressions

| Method | Path | |
|---|---|---|
| `GET` | `/suppressions` | Newest first. Filters: `reason`, `stream` (`all` or `broadcast`), `address`, `domain`; `?limit` and `?before` (the opaque `next_before`). |
| `POST` | `/suppressions` | `{"address", "reason"?, "detail"?, "stream"?}`. `reason` is a short name (`manual` by default, e.g. `customer_request`). `stream` `all` (default) stops all mail, `broadcast` only bulk mail. `201`, or `200` with the existing row. Full access. |
| `DELETE` | `/suppressions/{address}` | Send to the address again (`?stream=` removes one stream's suppression only). Full access. |
| `GET` | `/suppressions/export` | Every suppression as CSV: `address,stream,reason,detail,created_at`. Takes the same filters. |

Bounces (5.1.x) and complaints suppress for every stream, unsubscribes for broadcast only. Sending keys may read but not change the list.

#### Project settings: per-recipient cap and circuit breakers

| Method | Path | |
|---|---|---|
| `GET` | `/project` | `{"id", "name", "max_per_recipient_per_day", "circuit_breaker": {"enabled", "bounce_rate", "complaint_rate", "min_recipients"}, "updated_at"}` |
| `PATCH` | `/project` | Change any of them; fields left out keep their value. `"max_per_recipient_per_day": null` turns the cap off. Full access. |

**`max_per_recipient_per_day`** (default: off) caps the emails one address gets from the project in any 24 hours, whichever keys send them. A send over it is refused with `429 recipient_rate_limited` and a `Retry-After` header. Suppressed recipients do not count; held mail counts until it is rejected. The cap is checked under a project lock, so parallel agents cannot overshoot it together. It counts the sends made while it is on.

**Circuit breakers** watch the rolling 24-hour bounce and complaint rates of every API key, every sending domain and the whole project. Once a scope has at least `min_recipients` recipients (default 50) and its bounces are above `bounce_rate` (default 5%) or its complaints above `complaint_rate` (default 0.1%), it is paused: every send from it answers `403 sending_paused`, with the rate, the scope and how to resume, and the `sending.paused` webhook fires. Bounces and complaints count wherever they are recorded: SMTP replies, bounce reports and feedback loops.

| Method | Path | |
|---|---|---|
| `GET` | `/pauses` | Pauses, newest first (`?active=true` for those in force): `scope` (`api_key`, `domain` or `project`), `scope_id`, `scope_name`, `reason`, `rate`, `threshold`, `recipients`, `bounces`, `complaints`, `created_at`, `resumed_at`, `resumed_by`, `active`. |
| `POST` | `/pauses/{id}/resume` | Resume after finding the cause. The scope's counts start over. Full access; recorded with the key. |

#### `POST /emails/check`

Takes the body of `POST /emails` and sends nothing. It answers what a send would do now (`"would": "send" | "hold" | "refuse"`), every refusal that would apply (the key's policy and caps, the domain, a pause, the per-recipient cap), the `hold_reasons`, the size, and warnings:

| `code` | |
|---|---|
| `no_text_part` | HTML without a plain-text part |
| `html_too_large` | HTML over 102 KB, which Gmail clips |
| `link_domain_mismatch` | links to sites other than the sender's domain |
| `missing_unsubscribe` | broadcast mail without a visible unsubscribe link, or without `OMW_PUBLIC_URL` |
| `domain_not_verified` | the `From` domain is missing or unverified |
| `large_message` | over 10 MB |

MCP tools: `check_email`, `list_held`, `approve_email` and `reject_email` (offered only to keys that may approve, and marked destructive), `list_suppressions`, `add_suppression` (full access) and `list_pauses`. `send_email` and `reply_to_message` say when a send is held. Resuming a pause and removing a suppression are left to people, on purpose.

### Inboxes and received messages

Mail to an inbox is received by an inbound node (see [Inbound mail](#inbound-mail)).

| Method | Path | |
|---|---|---|
| `POST` | `/inboxes` | `{"address": "support@example.com", "hold_unauthenticated"?: true}`. The domain must be added to the project and verified. Full-access keys only. |
| `GET` | `/inboxes` | Lists inboxes (for a `sending_access` key, those its policy lists). |
| `GET` | `/inboxes/{id}` | One inbox. |
| `PATCH` | `/inboxes/{id}` | `{"hold_unauthenticated": false}` turns the hold off (see below). Full-access keys only. |
| `DELETE` | `/inboxes/{id}` | Deletes the inbox **and its messages**. Full-access keys only. |
| `GET` | `/inboxes/{id}/messages` | `?limit=20&before=<message id>&thread_id=<id>`. Newest first: `{data:[{id, from, to, subject, received_at, thread_id, in_reply_to_email_id, snippet, trust, held, suspicious_instructions}], has_more, next_before}`. |
| `GET` | `/messages/{id}` | `from`, `to`, `cc`, `reply_to`, `subject`, `text`, `html`, `headers` (`message-id`, `in-reply-to`, `references`, `date`), `attachments[]` (`index`, `filename`, `content_type`, `size`), `thread_id`, `in_reply_to_email_id`, `received_at`, `body_purged`; the sender check: `mail_from`, `trust`, `authentication_results`; and the flags `held`, `released_at`, `suspicious_instructions`, `suspicious_rules`, `reply_to_mismatch`, `message_id_conflict`. |
| `GET` | `/messages/{id}/attachments/{index}` | The attachment's bytes, as a download. Indexes start at 0. |
| `POST` | `/messages/{id}/reply` | `{"text": "…", "html": "…", "to"?: [...]}`. Sends from the inbox address to the message's first `Reply-To` address (or `From`), with `Re: …`, `In-Reply-To` and `References`. To answer more or other people, name them in `to`. Returns `{"id", "thread_id", "to", "warnings"?}`; `warnings` says when other `Reply-To` addresses were left out or `Reply-To` is on another domain than `From`. |
| `POST` | `/messages/{id}/release` | Shows a held message to `sending_access` keys. Full-access keys only. |
| `GET` | `/emails/{id}/replies` | The received messages that answer a sent email, and its `thread_id`. |

**Sender verdict.** Every message is checked when it arrives: SPF for the envelope sender and the HELO name (using the address of the server that connected to the inbound node), every DKIM signature, and DMARC alignment with the `From` domain (relaxed or strict as the domain asks, with the public-suffix list for organizational domains). `trust` is:

| `trust` | Meaning |
|---|---|
| `authenticated` | The `From` domain is proven: a valid DKIM signature by it, or an SPF pass for an envelope sender on it (DMARC's "pass", whether or not the domain publishes a policy). |
| `failed` | The `From` domain publishes a DMARC policy and the message fails it, or SPF says the server may not send for an envelope sender on the `From` domain. Probably forged. |
| `unauthenticated` | Nothing proves who sent it. Most personal mail from small domains, and mail forwarded by lists, lands here. |

`authentication_results` holds the details (`spf`, `spf_helo`, `dkim[]`, `dmarc` with the domain's `policy`, the `reason` for the verdict) and the same as an `Authentication-Results` header (`header`).

**Holds.** An inbox holds messages that are not `authenticated` by default (`hold_unauthenticated: true`): full-access keys see them with `held: true`; `sending_access` keys (agents) do not see them at all until a full-access key calls `POST /messages/{id}/release`. Turn it off for inboxes whose senders cannot authenticate, with `PATCH /inboxes/{id}`. `email.received` is sent for held messages too, with `data.held`.

**Flags for agents.** `suspicious_instructions` is set when the text (subject, sender name, body, and HTML including its hidden parts) matches one of a few rules for text aimed at AI agents, listed in `suspicious_rules`: `override_instructions` ("ignore previous instructions"), `role_marker` ("System:", `<|im_start|>`), `addresses_agent` ("note to the AI"), `tool_instruction` ("call send_email", "forward all emails"), `conceal_from_user` ("do not tell the user") and `claims_approval` ("the user has approved"). It is a hint, never a reason to drop mail. `reply_to_mismatch` says `Reply-To` is on another domain than `From`. `message_id_conflict` says another message in the inbox has the same Message-ID but different content: one of them may be forged.

**Threading.** A received message whose `In-Reply-To` or `References` names one of the project's sent emails gets `in_reply_to_email_id`. `thread_id` is the id of the first email or message of the conversation: a reply sent with `POST /messages/{id}/reply` joins the thread of the message it answers, and the answer to that reply joins it too. For replies to reach an inbox, send from the inbox address.

**Replies are ordinary emails.** `POST /messages/{id}/reply` calls `POST /emails` with your own API key, so the domain must be verified and every send rule (key policies, suppression list) applies. An `Idempotency-Key` header is passed on. Refusals come back unchanged.

**Retention.** Message content (body, attachments and the sealed snippet) is deleted `OMW_INBOUND_RETENTION` after arrival (default 30 days). The message stays listed with its sender, subject and thread, and `body_purged` becomes `true`.

**Duplicates.** A copy with the Message-ID **and the content** of a message the inbox holds (the same envelope sender, verdict, headers a reader sees, body and attachments) is stored once, so a retried SMTP transaction is not stored twice. A different message that reuses a Message-ID is kept, and both are flagged `message_id_conflict`.

### DMARC and TLS reports

Receivers send daily DMARC aggregate reports (RFC 7489, and DMARCbis) to the `rua=` address of your DMARC record, and SMTP TLS reports (RFC 8460) to the `rua=` of your `_smtp._tls` record. Point both at the domain's `report_addresses` (`dmarc@bounce.<domain>` and `tls-rpt@bounce.<domain>`, shown by `GET /domains/{id}`):

```
_dmarc.example.com.     TXT  "v=DMARC1; p=none; rua=mailto:dmarc@bounce.example.com"
_smtp._tls.example.com. TXT  "v=TLSRPTv1; rua=mailto:tls-rpt@bounce.example.com"
```

The return-path domain shares `example.com`'s organizational domain, so reporters need no extra authorization record. Reports arrive through the inbound nodes, compressed (gzip or zip) or not. They are parsed with limits (16 MB decompressed whatever the archive claims, 16 zip entries, 10 000 rows, no XML entities, shallow nesting), stored for 180 days, and ignored when they are about another domain. A report sent twice is stored once.

`GET /domains/{id}/reports?days=30` (full access) returns:
- `dmarc`: messages, `pass`, `fail` and `pass_rate`; `by_organization` (Google, Microsoft, …); `by_source` (each sending IP with its DKIM and SPF passes and `openmailwire_worker` when it is one of your workers); and `unknown_senders`, the IPs none of whose mail passed DMARC and that are not your workers: spoofers, or a service that still needs SPF or DKIM;
- `tls`: successful and failed sessions per reporter and policy, and each failure (`result_type` such as `certificate-expired`, the sending and receiving hosts);
- `reports`: the 50 most recent reports.

## Receiving mail

To give an agent an inbox that receives mail from the internet:

1. **Run an inbound node.** `openmailwire inbound` on a host that accepts port 25, with a worker token of the inbound role (`openmailwire workers add inbound-1 --role inbound`). See [Inbound mail](#inbound-mail) below.
2. **Tell the control plane about it.** Set `OMW_INBOUND_MX` to the node's host name. Each domain then lists an `INBOUND` MX record: publish it, and verify the domain (`POST /domains/{id}/verify`, or Verify on the domain's page in the dashboard).
3. **Create an inbox** on the domain: `POST /inboxes` with `{"address": "agent@example.com"}`, or the dashboard's Inboxes page.
4. **Send it a message**, from Gmail or any mail client, and read it with `GET /inboxes/{id}/messages`, the MCP tools, or the dashboard's Mail page. Unauthenticated mail is held from agent keys until a full-access key releases it.

Agent keys (`sending_access`) read only the inboxes their policy's `inboxes` lists.

## Inbound mail

`openmailwire inbound` is an SMTP server for mail from the internet. It is stateless: for every `RCPT TO` it asks the control plane whether the address exists, and it acknowledges a message only after the control plane has stored it. Unknown addresses are refused with `550 5.1.1` during the SMTP conversation, so OpenMailWire never sends bounces of its own (no backscatter). Inbound nodes authenticate with a worker token.

It receives three kinds of mail:

| Sent to | What happens |
|---|---|
| a bounce address `b-<delivery>-<hmac>@bounce.example.com` | The HMAC is checked first; forged addresses are refused. A delivery status notification (RFC 3464, and the plain-text "failure notice" some providers still send) with `Action: failed` marks that delivery `bounced`, even if it was `delivered`, records the status and the remote server's diagnostic, suppresses the address on 5.1.x, and sends `email.bounced`. Delays, auto-replies and anything else are ignored. |
| `OMW_FBL_ADDRESS` (or a bounce address) | A feedback-loop complaint (ARF, RFC 5965; Yahoo, Microsoft and others). Anyone can send one, so: a report that carries a valid VERP address (it was sent to one, or its `Original-Mail-From` or the quoted original's `Return-Path` is one) names exactly one delivery, which gets `complained`, its address suppressed with reason `complaint`, and `email.complained`. A report that names the email only by its Message-ID counts only when a feedback loop in `OMW_FBL_TRUSTED_DOMAINS` signed it with DKIM; it is looked up only among emails on the return path or sending domain the report names, records at most one complaint per email, suppresses nobody and has `data.to: []`. DMARC failure reports and `not-spam` feedback are ignored. |
| an inbox address | The sender is checked (SPF, DKIM, DMARC: the `trust` verdict). The message is sealed (AES-256-GCM), threaded, stored, held from agent keys when not authenticated, and announced with `email.received`. A copy with the Message-ID and content of one the inbox already holds is stored once. An inbox takes at most `OMW_INBOX_MAX_PER_HOUR` messages an hour; more get `451` and are retried by the sender. |
| `dmarc@` or `tls-rpt@` a return-path domain | DMARC aggregate or TLS-RPT reports, stored for `GET /domains/{id}/reports`. |
| `postmaster@` or `abuse@` a domain the nodes serve | Accepted, as RFC 5321 requires: delivered to the inbox `OMW_POSTMASTER_INBOX` names, or logged (sender domain and size) and dropped. An inbox with that exact address takes precedence. |

**Abuse limits.** A node closes a connection after 10 refused recipients, limits the recipient checks one address (an IPv6 /64) causes on the control plane (120 a minute, bursts of 60), keeps recent answers for a minute (refusals for 15 seconds), takes at most 20 connections from one address, and waits at most a minute for each command until the client sends `MAIL`. The control plane processes at most `OMW_INBOUND_CONCURRENCY` messages at once; the rest get `451` and are retried.

Run it next to the control plane or anywhere that can reach it:

```sh
OMW_CONTROL_URL=https://mail.example.com \
OMW_WORKER_TOKEN=omwk_… \
OMW_INBOUND_HOSTNAME=mx.mail.example.net \
OMW_TLS_CERT=/etc/ssl/mx.crt OMW_TLS_KEY=/etc/ssl/mx.key \
  openmailwire inbound
```

Then point the MX records at it: the return-path domains through `OMW_BOUNCE_MX` and inbox domains through `OMW_INBOUND_MX` (see [docs/production.md](docs/production.md#6-inbound-mail-mx-bounces-and-complaints)).

To try it locally, with `openmailwire dev-deliver` playing the sender's mail server, see [Receiving mail](#receiving-mail).

## SDKs

Client libraries live in [`sdks/`](sdks): Node.js/TypeScript, Python, Go and PHP. Each covers the whole API (emails with checks and approvals, domains with DKIM and reports, webhooks, API keys, inboxes and messages, suppressions, pauses, the project's settings, the audit log and insights), can wait for delivery or for a reply, and verifies webhook signatures. They use only each language's standard library (curl for PHP), and they are MIT licensed so that apps under any licence can embed them. They are not published to npm, PyPI or Packagist yet; each README says how to install from this repository.

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
```

## Dashboard

Every control plane serves a web dashboard at **`/dashboard/`** (for example `http://localhost:8080/dashboard/`). Sign in by pasting an API key. It sends and receives mail, and runs the rest of the system.

| Page | What you can do |
|---|---|
| Overview | The whole system at a glance, refreshed live: sent, delivered, bounce and complaint rates for 24 hours and 7 days with a daily trend, the queue and the age of its oldest recipient, held emails, active pauses, domains needing attention, receiving, workers (state, self-check, warm-up) and recent emails. Every tile links to its page. A new project gets a setup checklist for sending (domain, DNS records with copy buttons, verify, agent key, test email) and receiving (inbound MX record, inbox, first message); it can be hidden. Needs a full-access key (`GET /overview`). |
| Compose | Write an email: From on a verified domain with a display name, To, Cc, Bcc and Reply-To as address chips, subject, text and HTML with a live preview, attachments (20 files, 10 MB), stream, send time, give-up time, and an automatic idempotency key. **Check** shows what Send would do (send, hold or refuse, with warnings) without sending. Send opens the email's timeline. |
| Mail | Every inbox the key may read in one list, newest first, live: filter by inbox, sender verdict, held and suspicious, search, and see what is new since your last visit. A message opens as a thread with your sent replies in order; reply inline (the reply's recipients and warnings are shown), release held mail, download attachments. |
| Held | Emails waiting for approval with the rules that held them and their text: approve or reject, with a note. A key cannot approve its own email; the page says so. |
| Emails | Search and filter loaded emails by text, status, recipient domain and dates; open one by ID. An email shows each recipient's progress (queued, each attempt, final status) and a timeline of every attempt (worker, source IP, MX host, TLS, the receiver's SMTP reply), refreshed live while anything is in flight. Held emails can be approved or rejected there. |
| Suppressions | Filter, page, add (address, stream, reason), remove, and export as CSV. |
| Pauses | Circuit breakers in force and resumed, with the rate, the limit and the counts; resume one. |
| Domains | Add a domain, copy each DNS record (DMARC is required), verify, and see when an unverified claim expires. **Set up DNS with Cloudflare** previews and writes the records with an API token, and can remember it (encrypted, on the server) for DKIM rotations. DKIM keys with the two-step rotation and the Ed25519 switch, the report addresses to publish, and the DMARC and TLS-RPT report summary. |
| Inboxes | Add and delete inboxes, hold or show unauthenticated mail per inbox, read messages with the sender verdict (SPF, DKIM, DMARC), flags for text aimed at agents, and release held mail. |
| Webhooks | Create one (optionally signed with Ed25519 too; the secret is shown once), see its public key and its circuit breaker, rotate its secret with an overlap, and see its 20 most recent deliveries. |
| API keys | Create and edit keys: permission, hourly and daily caps, recipient and sender allowlists, approval rules, `can_approve`, readable inboxes, allowed IPs and expiry. The token is shown once. Revoke keys. |
| Settings | The per-recipient daily cap and the circuit breaker limits. |
| Insights | Acceptance, deferral and bounce rates and time to acceptance per mailbox provider and per sending IP, for 24 hours, 7 days and 30 days, as tables with small bars. |
| Audit log | Every administrative change with its actor, and a button that verifies the hash chain. |

Keyboard: `/` searches the page, `?` lists the shortcuts, and `g` then a letter goes to a page (`g o` overview, `g c` compose, `g m` mail, `g e` emails, `g h` held, `g d` domains, `g i` insights, and more). Destructive actions ask in a dialog first, and finished actions show a short notice.

When the server is older than a page, the page says "Not available on this server version". When the key lacks the permission, it says "This key can't manage …" or "can't see …".

**How it keeps the key safe.** The browser holds an API key, so the dashboard is built to be boring to attack:
- The key is kept in `sessionStorage` only: it is gone when the tab closes, and never sent anywhere but the same-origin API, as `Authorization: Bearer`. No cookies, so no CSRF. `localStorage` holds only a few non-secret settings (a hidden checklist, when Mail was last read).
- Every dashboard response carries `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`, plus `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`. The page itself is `Cache-Control: no-store`.
- There is no inline script or style, no framework, no CDN and no third-party request. The files are embedded in the binary (`internal/dashboard/static`).
- Subjects, addresses, SMTP replies, webhook URLs, inbound mail and DMARC reports are attacker-controlled. They are only ever inserted as text, never as HTML, and what a sender wrote is visibly marked as sender-provided. Inbound HTML mail is shown as source, never rendered, and attachments always download. A test (`internal/dashboard/tests`) fails if `innerHTML`, `outerHTML`, `insertAdjacentHTML` or `document.write` appears in the scripts.
- The only HTML ever rendered is what you type in Compose, in an `<iframe sandbox="">` with no permissions: no scripts, no same origin, no forms. It also inherits the page's CSP, so it loads no remote images and applies no inline styles.
- A Cloudflare token is typed into a password field, sent only in the body of one request, cleared afterwards, and never stored in the browser or put in a URL.

Serve the control plane over HTTPS when the dashboard is used from another machine: the key travels with every request.

To work on the dashboard, edit the files in `internal/dashboard/static` and rebuild; there is no build step for them. Its JavaScript tests need Node 22:

```sh
node --test internal/dashboard/tests/*.test.mjs
```

## Running in production

Delivering mail yourself is mostly about IP reputation and DNS hygiene, not code. Read [docs/production.md](docs/production.md) before you send real mail. In short:

1. Port 25 must be open outbound. Many clouds block it by default.
2. Each worker needs a static IPv4, matching reverse DNS (PTR), and a clean blocklist history.
3. Publish one SPF include listing your workers' IPs and set `OMW_SPF_INCLUDE`. With `OMW_CLOUDFLARE_TOKEN`, the control plane keeps it up to date at Cloudflare.
4. Put TLS in front of the control plane, back up PostgreSQL, and back up `OMW_SECRET_KEY` (or `OMW_SECRET_KEYS`) apart from it. See [rotating keys](docs/production.md#rotating-secrets).
5. Ramp volume up slowly and watch Google Postmaster Tools.

## Configuration

Control plane (`openmailwire serve`):

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | none | PostgreSQL connection string. |
| `OMW_SECRET_KEY` | none | 32 bytes, base64 (`openmailwire keygen`). Encrypts DKIM keys, webhook secrets and queued messages. The control plane refuses to start if it doesn't open the data in the database. |
| `OMW_SECRET_KEYS` | none | Several keys, comma-separated, for rotation: the first encrypts new values, the others only decrypt old ones. Replaces `OMW_SECRET_KEY`, which, if also set, must be one of them. See [rotating keys](docs/production.md#rotating-secrets). |
| `OMW_LISTEN` | `127.0.0.1:8080` | Set `:8080` (or a specific address) to accept connections from other machines, e.g. behind a reverse proxy on another host. |
| `OMW_RATE_LIMIT` | `20` | Requests per second each API key may make on average; `0` turns the limit off. Counted per control plane. |
| `OMW_RATE_LIMIT_BURST` | twice the rate | Requests a key may make at once. |
| `OMW_MAX_LARGE_REQUESTS` | `4` | Requests with a body over 1 MB handled at once. More wait up to 30 seconds, then get `503 server_busy`. |
| `OMW_TRUSTED_PROXIES` | none | Comma-separated addresses or CIDR prefixes of your reverse proxies. Their `X-Forwarded-For` gives the client's address for `allowed_ips`. |
| `OMW_PUBLIC_URL` | none | Where recipients and mailbox providers reach the control plane, e.g. `https://mail.example.com`. Broadcast mail needs it for its one-click unsubscribe links (`/u/{token}`); without it, broadcast sends are refused. Use `https`: mailbox providers ignore one-click links over plain `http`. |
| `OMW_OPERATOR_PROJECTS` | none | Comma-separated project ids that see the installation's workers in detail (names, addresses, versions, self-check problems) in `GET /overview`. When unset, the only project of a single-tenant install sees them; on a multi-tenant install every project sees worker health without those details. |
| `OMW_SPF_INCLUDE` | none | e.g. `_spf.mail.example.net`. If unset, SPF records list the workers' IPs. |
| `OMW_CLOUDFLARE_TOKEN` | none | The operator's Cloudflare API token, with Zone → DNS → Edit on the zone that holds `OMW_SPF_INCLUDE` (and nothing else). The control plane then keeps that TXT record listing the IPs delivery workers announced in the last 7 days (`v=spf1 ip4:… -all`), checking every 5 minutes and writing only when it changes. Never used for customers' domains. Needs `OMW_SPF_INCLUDE`. |
| `OMW_CLOUDFLARE_API_BASE` | `https://api.cloudflare.com/client/v4` | Cloudflare's API, for automatic DNS. Only for tests (a local fake): plain `http` is accepted only to a loopback address. |
| `OMW_BOUNCE_MX` | none | Host that receives bounces for the return-path domains: your inbound node. |
| `OMW_INBOUND_MX` | none | Host that receives mail for inboxes. When set, every domain shows an optional `INBOUND` MX record. |
| `OMW_FBL_ADDRESS` | none | Address you registered with feedback loops, e.g. `fbl@mail.example.net`. Its domain's MX must point to an inbound node. |
| `OMW_INBOUND_RETENTION` | `720h` | Inbound message content is deleted this long after arrival. |
| `OMW_INBOUND_MAX_SIZE` | `25MB` | Largest inbound message the control plane accepts (MB = 1024 × 1024 bytes). |
| `OMW_FBL_TRUSTED_DOMAINS` | Yahoo, AOL, Microsoft, Comcast, Validity, Fastmail, Zoho, Mail.ru, Yandex | Feedback loops whose DKIM-signed reports count when they name an email only by Message-ID: `yahoo.com,aol.com,hotmail.com,outlook.com,comcast.net,returnpath.net,fastmail.com,messagingengine.com,zoho.com,mail.ru,yandex.ru,yandex.net`. A signing domain matches an entry or its subdomains. `none` keeps only reports that carry a VERP address. |
| `OMW_POSTMASTER_INBOX` | none | An inbox (e.g. `ops@example.com`) that receives `postmaster@` and `abuse@` mail for every served domain. Unset: such mail is logged and dropped. |
| `OMW_INBOX_MAX_PER_HOUR` | `1000` | Messages one inbox accepts per hour; more get `451` and are retried. |
| `OMW_INBOUND_CONCURRENCY` | `8` | Inbound messages the control plane processes at once. |
| `OMW_RETURN_PATH_LABEL` | `bounce` | Gives `bounce.<domain>`. |
| `OMW_DEFAULT_EXPIRY` | `48h` | |
| `OMW_RETENTION` | `72h` | Message bodies are deleted this long after their last delivery. |
| `OMW_LEASE` | `15m` | Must exceed the workers' `OMW_JOB_TIMEOUT`. |
| `OMW_DOMAIN_CONCURRENCY` | `10` | In-flight deliveries per recipient domain, across all workers. Also the per-IP limit of a provider the rules give none, and of a domain of no known provider. Throttling lowers a provider's limit for an IP that is asked to slow down, and raises it back. |
| `OMW_RELAY_AFTER_ATTEMPTS` | `2` | While a relay worker is live, deliveries with this many attempts (greylisting not counted) go to it instead of direct workers. |
| `OMW_PROVIDER_RULES` | built in | A JSON file of mailbox providers: MX host suffixes, per-IP concurrency and warm-up schedule. Start from [internal/policy/providers.json](internal/policy/providers.json). |
| `OMW_WARMUP` | `true` | New sending IPs get a daily cap per provider that grows each day. IPs that sent before v0.6 count as warm. `false` turns warm-up off. |
| `OMW_DNS_SERVER` | system | e.g. `1.1.1.1:53`, for domain checks. |
| `OMW_ALLOW_UNVERIFIED` | `false` | Development only. |
| `OMW_WEBHOOK_ALLOW_HTTP` | `false` | Development only: accept `http://` webhook endpoints. |
| `OMW_WEBHOOK_ALLOW_PRIVATE` | `false` | Development only: allow webhook endpoints on localhost or private networks. |
| `OMW_WEBHOOKS_PER_PROJECT` | `10` | Most webhooks a project may have. |

Worker (`openmailwire worker`):

| Variable | Default | |
|---|---|---|
| `OMW_CONTROL_URL` | none | |
| `OMW_WORKER_TOKEN` | none | From `openmailwire workers add <name>` (role `delivery`). |
| `OMW_EHLO` | hostname | Must equal the sending IP's reverse DNS. The self-check refuses to start otherwise. |
| `OMW_SOURCE_IP` | OS default | Local address to send from. |
| `OMW_PUBLIC_IP` | `OMW_SOURCE_IP` | Advertised to the control plane, used in SPF and shown in attempts. |
| `OMW_CONCURRENCY` | `20` | |
| `OMW_IPV4_ONLY` | `true` | Receivers reject IPv6 senders without IPv6 reverse DNS and SPF. |
| `OMW_JOB_TIMEOUT` | `10m` | A job also ends before its lease does (`OMW_LEASE` on the control plane), with a margin for the report. |
| `OMW_STATE_DIR` | `./openmailwire-worker-state` | Holds the report outbox. |
| `OMW_SMTP_OVERRIDE` | none | Development only: send everything to this `host:port`. MTA-STS and DANE do not apply. |
| `OMW_DNSSEC_RESOLVER` | none | `host:port` of a DNSSEC-validating resolver the worker trusts, e.g. a local Unbound on `127.0.0.1:53`. Turns on DANE; MX and address lookups then go through it too. |
| `OMW_SKIP_SELF_CHECK` | `false` | Development only: start without checking the EHLO name, its A record, reverse DNS and port 25. Skipped anyway with `OMW_SMTP_OVERRIDE` and on relay workers. |
| `OMW_SELF_CHECK_SMTP` | Gmail's MX | `host:port` the port 25 self-check connects to. |

SMTP submission (`openmailwire submission`):

| Variable | Default | |
|---|---|---|
| `OMW_API_URL` | `http://localhost:8080` | The control plane. Use `https://` when it runs on another machine. |
| `OMW_TLS_CERT`, `OMW_TLS_KEY` | none (required) | PEM certificate chain and private key. Re-read when the files change, so renewals need no restart. For local development only, `openmailwire submission --dev-tls` makes a self-signed certificate instead. |
| `OMW_SUBMISSION_LISTEN` | `:587` | STARTTLS; clients must use it before `AUTH`. |
| `OMW_SUBMISSION_TLS_LISTEN` | none | e.g. `:465`, for implicit TLS. |
| `OMW_SUBMISSION_HOSTNAME` | hostname | Announced in the greeting. Use the name on the certificate. |
| `OMW_SUBMISSION_MAX_SIZE` | `14MB` | The largest message. 14MB is also the maximum, because `/emails/raw` takes 20 MB of base64. |
| `OMW_SUBMISSION_MAX_CONNS` | `200` | Concurrent connections over both ports. Each lasts at most 30 minutes, and gets a minute per command until it logs in. |
| `OMW_SUBMISSION_MAX_CONNS_PER_IP` | `20` | Concurrent connections from one address (an IPv6 /64). After 10 failed logins in 15 minutes, an address's logins are refused until the window passes. |

Inbound node (`openmailwire inbound`):

| Variable | Default | |
|---|---|---|
| `OMW_CONTROL_URL` | none | |
| `OMW_WORKER_TOKEN` | none | Inbound nodes authenticate with a worker token of role inbound: `openmailwire workers add <name> --role inbound`. |
| `OMW_INBOUND_LISTEN` | `:25` | |
| `OMW_INBOUND_HOSTNAME` | hostname | Announced in the greeting and in `Received` headers. Use the name your MX records point to. |
| `OMW_TLS_CERT`, `OMW_TLS_KEY` | none | PEM files. When set, STARTTLS is offered. |
| `OMW_INBOUND_MAX_SIZE` | `25MB` | Larger messages are refused with `552 5.3.4`. |
| `OMW_INBOUND_MAX_CONNS_PER_IP` | `20` | Concurrent connections from one address (an IPv6 /64). |

Worker admin: `openmailwire workers list`, `openmailwire workers drain <name>` (stop new jobs, finish current ones), `openmailwire workers resume <name>`.

MTA-STS needs no setting: workers look up `_mta-sts.<domain>` and fetch the policy over HTTPS (no redirects, 64 KB at most), cached by its `id` and `max_age`.

A **relay worker** is an ordinary worker with `OMW_RELAY_HOST` set. It needs its own token, of role relay (`openmailwire workers add relay-1 --role relay`).

| Variable | Default | |
|---|---|---|
| `OMW_RELAY_HOST` | none | The smarthost, e.g. `smtp.provider.example`. Makes this worker a relay. |
| `OMW_RELAY_PORT` | `587` | |
| `OMW_RELAY_TLS` | `starttls` | `starttls` (required, never plaintext) or `implicit` (TLS from the first byte, usually port 465). The smarthost's certificate must be valid. |
| `OMW_RELAY_USERNAME`, `OMW_RELAY_PASSWORD` | none | Credentials for `AUTH PLAIN`. Leave both empty for a smarthost that admits your IP. |

Relay workers take only deliveries that already had `OMW_RELAY_AFTER_ATTEMPTS` attempts (greylisting not counted), or whose provider no live direct worker can take now: paused by throttling, or over the day's warm-up cap, on every direct IP. While one is live (a heartbeat in the last 2 minutes), direct workers leave the former to it. Without one, nothing changes.

**Worker tokens have a role**: `delivery` (the default), `relay` or `inbound`. A delivery token cannot lease as a relay or hand over received mail, and the reverse; such calls answer `403 wrong_worker_role`. Tokens created before v0.6 keep working for everything.

Worker admin:
- `openmailwire workers add <name> [--role delivery|inbound|relay]`;
- `openmailwire workers list`: each worker's role, kind (`direct` or `relay`), last heartbeat, self-check result, and its IP's warm-up (`day 3, google 120/200 left, …`, or `warm`);
- `openmailwire workers drain <name>` (stop new jobs, finish current ones) and `openmailwire workers resume <name>`;
- `openmailwire workers warm <name>` treats the worker's IP as warm (an IP that already has a sending history elsewhere); `--restart` starts its warm-up again.

Operator SPF include (it reads `DATABASE_URL`, `OMW_SPF_INCLUDE` and `OMW_CLOUDFLARE_TOKEN`): `openmailwire dns sync [--dry-run]` publishes it once and prints the difference; the control plane does the same every 5 minutes.

Key rotation (it reads `DATABASE_URL` and `OMW_SECRET_KEYS`): `openmailwire rotate-secrets [--batch N] [--pause 50ms]` re-encrypts every stored secret with the first key, online, in small transactions. It says when the old keys can be removed. See [docs/production.md](docs/production.md#rotating-secrets).

API key admin (it reads `DATABASE_URL`):

```sh
openmailwire apikey create <project-id> <name> [--sending-only] [--max-per-hour N] [--max-per-day N] \
  [--allow-to PATTERN]... [--allow-from PATTERN]... [--inbox PATTERN]... [--allow-ip CIDR]... [--expires-in DURATION]
openmailwire apikey list <project-id>
```

`--allow-to`, `--allow-from`, `--inbox` and `--allow-ip` can be repeated; `--expires-in 720h` makes the key stop working after 30 days. The same keys can be managed over the API (`/api-keys`). Keys created on the command line are recorded in the audit log with the actor `cli`.

The control plane's HTTP server limits each request: 10 seconds for the header, 3 minutes for the whole request (a 20 MB body needs about 1 Mbit/s), 4 minutes to answer, 64 KB of headers, and 2 minutes for an idle keep-alive connection. Every answer carries `X-Content-Type-Options: nosniff`, and API answers `Cache-Control: no-store`. `GET /health` shows the version only to a caller with a valid API key or worker token.

## Roadmap

**Agent-first**
- [x] **MCP server**: stdio (`openmailwire mcp`) and remote (`/mcp`). Tools to send, track, and add and verify domains (v0.3).
- [x] **Agent keys**: sending-only keys, per-key send caps (per hour, per day), recipient and sender allowlists, and an audit trail per key: every email records its key, and `GET /emails?api_key_id=` lists them (v0.4).
- [x] **`llms.txt` and OpenAPI**, served by the control plane at `/llms.txt` and `/openapi.json` (v0.5).
- [x] **Agent inboxes**: inbound SMTP, replies threaded by `In-Reply-To`, an `email.received` webhook and MCP tools to wait for and answer replies (v0.5).
- [x] **Agent safety**: approval holds, a per-recipient daily cap, circuit breakers on bounce and complaint rates, a pre-send check (unreleased, v0.6).
- [x] **Bulk-mail compliance**: streams, one-click unsubscribe (RFC 8058), `Feedback-ID`, a suppressions API (unreleased, v0.6).

**Delivery**
- [x] **Bounce processing**: an inbound listener for the return-path MX that parses DSNs and checks the VERP HMAC (v0.5).
- [x] **Signed webhooks**, Resend-compatible (v0.2).
- [x] **Complaint feedback loops**: `email.complained` from Yahoo, Microsoft and other feedback loops (v0.5).
- [x] **MTA-STS and DANE**: enforce TLS where receivers require it (v0.5).
- [x] **Adaptive throttling**: learn per-receiver limits from 421 deferrals (v0.5).
- [x] **Fallback relay worker**: hand mail to another provider when your IPs are throttled (v0.5).
- [x] **Provider-aware throttling and IP warm-up**: limits per mailbox provider and sending IP, a daily warm-up schedule with relay overflow, greylisting, worker self-checks, DKIM oversigning, Ed25519 and key rotation (v0.6).
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
