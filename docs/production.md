# Running OpenMailWire in production

Code is the easy part of sending email. Receivers decide by reputation, and reputation follows your IPs and your domains. Work through this list before real users depend on it.

## 1. Workers: the machines that send

- **Outbound port 25.** Many cloud providers block it by default, some permanently. Test before you commit to a provider:
  `nc -vz gmail-smtp-in.l.google.com 25`
- **A static IPv4 per worker, with reverse DNS.**
  - The PTR record of the IP must name a host, e.g. `mail1.example.net`.
  - That host's A record must point back to the IP.
  - `OMW_EHLO` must be that name.
  - Gmail and Outlook check all three.
- **IPv6.** Keep `OMW_IPV4_ONLY=true` unless each IPv6 sending address has reverse DNS and is in your SPF. Gmail rejects IPv6 senders that lack them.
- **A clean history.** Check every IP before you use it:
  - Spamhaus ZEN: query it through your own resolver or a free Data Query Service key. Spamhaus refuses queries that come through public resolvers.
  - Barracuda.
  - Microsoft SNDS.
  - A listed IP is cheaper to replace than to repair.
- **Advertise the IP.** Set `OMW_PUBLIC_IP` so SPF records and the attempt timeline show the address mail actually leaves from.

## 2. DNS you publish once, as the operator

```
_spf.mail.example.net.   TXT  "v=spf1 ip4:203.0.113.7 ip4:198.51.100.9 -all"
mail1.example.net.       A    203.0.113.7      ; PTR of 203.0.113.7 → mail1.example.net
mail2.example.net.       A    198.51.100.9     ; PTR of 198.51.100.9 → mail2.example.net
bounces.example.net.     A    <host that will receive bounces>
```

Set `OMW_SPF_INCLUDE=_spf.mail.example.net` and `OMW_BOUNCE_MX=bounces.example.net`. Customer domains then publish `include:_spf.mail.example.net` once, and you can add or replace workers without asking them to change DNS.

The return-path domain needs an MX (or an A record) even if you do not process bounces: many receivers refuse envelope senders whose domain cannot receive mail. Point `bounces.example.net` at an inbound node (section 6) and bounces are processed too.

## 3. Each sending domain

- Publish what `POST /domains` returns, then call `POST /domains/{id}/verify`.
- DKIM keys are 2048-bit. Their TXT value is longer than 255 characters. Most DNS dashboards split it automatically; with raw zone files, split it into quoted strings yourself.
- **DMARC.** Start with `p=none` and an aggregate report address (`rua=`). Read the reports, then move to `quarantine` or `reject`.
- **Register the domain** with [Google Postmaster Tools](https://postmaster.google.com). For Outlook, enroll your IPs in Microsoft SNDS and JMRP.

## 4. The control plane

- **TLS.** Put a reverse proxy (Caddy, nginx, a load balancer) in front of `OMW_LISTEN`. Workers and apps must reach it over HTTPS. `OMW_LISTEN` defaults to `127.0.0.1:8080`, which suits a proxy on the same machine; set `:8080` when the proxy runs elsewhere (the Docker image's compose file does).
- **Client addresses.** Set `OMW_TRUSTED_PROXIES` to your proxy's address, so that API keys with `allowed_ips` see the client's address (from `X-Forwarded-For`) instead of the proxy's.
- **Backups.** Back up PostgreSQL, and separately back up `OMW_SECRET_KEY` (or every key in `OMW_SECRET_KEYS`). Without the key, the DKIM keys, webhook secrets and queued messages in a restored database cannot be decrypted. At start the control plane checks the configured keys against the database and refuses to run with a key that doesn't match the data, instead of failing later on every send.
- **Never** set `OMW_ALLOW_UNVERIFIED=true`.
- **One instance** of `openmailwire serve` for now. Several can share a database safely (leases use `SKIP LOCKED` and migrations take an advisory lock), but that setup is not tested yet.

### Rotating secrets

Rotate `OMW_SECRET_KEY` when someone who knew it leaves, or on a schedule. Nothing stops while you do it.

1. Make a key: `openmailwire keygen`. Back it up.
2. On every control plane, set `OMW_SECRET_KEYS=<new>,<old>` (newest first; unset `OMW_SECRET_KEY` or leave it set to one of the two) and restart. New values are now sealed with the new key, and both open old ones. The start-up log names both key ids: `secret keys checked … sealing_key=… other_keys=…`.
3. Run `openmailwire rotate-secrets` once, from any machine with `DATABASE_URL` and the same `OMW_SECRET_KEYS`. It re-encrypts DKIM keys, webhook secrets, queued messages and inbox messages in transactions of 100 rows, pausing 50 ms between them (`--batch`, `--pause`). It can be stopped and run again. It finishes with `done: nothing is sealed with key … any more`.
4. Remove the old key from `OMW_SECRET_KEYS` and restart.

The control plane refuses to start while data is still sealed with a key it doesn't have, and the message says which key id is missing. A sealed value records the id of its key (8 hex characters derived from the key); the key itself is never stored, only a check value. Bounce (VERP) addresses already sent keep verifying across rotations: their signing key lives in the database, sealed.

### Webhook signing secrets

- **Rotate** a webhook's secret with `POST /webhooks/{id}/rotate-secret` (`{"overlap_seconds": …}`, default one day). Deploy the new secret to the receiver within the overlap; until it ends, every request is signed with both. A leaked secret: rotate with `"overlap_seconds": 0`.
- **Ed25519** (`"asymmetric_signing": true` on `POST /webhooks`): receivers verify with the public key from `GET /webhooks/{id}` and never hold a secret that could sign. Rotation replaces the key pair too; both public keys are listed during the overlap.
- **Limits.** A project has at most `OMW_WEBHOOKS_PER_PROJECT` webhooks (default 10). At most 4 requests run per endpoint and 8 per project at once. An endpoint that fails 5 times in a row is paused (30 s, doubling to 10 minutes), then probed one request at a time; `GET /webhooks/{id}` shows `consecutive_failures` and `paused_until`.
- **Errors.** Projects see a short, generic `last_error`. The control plane's log has the details (`webhook attempt failed`, with the error), including any address an endpoint resolved to.

### Delivery insights

`GET /insights` (full access) gives acceptance, deferral and bounce rates and time to acceptance per mailbox provider and per sending IP, for 24 hours, 7 days and 30 days. Watch it next to Google Postmaster Tools:

- A falling acceptance rate or rising deferral rate at one provider usually means reputation trouble there; slow down to it.
- One sending IP worse than the others: check its blocklist status and reverse DNS.
- A p95 time to acceptance in minutes means receivers are deferring you, often greylisting or throttling.

It reads at most the newest 100,000 emails of the last 30 days, through indexes, and runs at most two at a time per control plane.

## 5. SMTP submission

Run `openmailwire submission` only if some of your apps cannot call the HTTP API. It holds no DKIM keys and no database connection: give it `OMW_API_URL` (the control plane, over HTTPS when it is on another machine) and a TLS certificate.

**The TLS certificate**
- Clients send an API key as their SMTP password, so the server refuses `AUTH` until the connection is encrypted. The certificate is required, not optional.
- Use a certificate from a public CA for the exact name apps connect to, e.g. `smtp.example.com`, and set `OMW_SUBMISSION_HOSTNAME` to that name. Most mail clients and libraries refuse self-signed certificates, or need an "allow insecure" switch you should not turn on.
- Point `OMW_TLS_CERT` at the **full chain** (Let's Encrypt's `fullchain.pem`, not `cert.pem`). Clients that only know the root CA cannot verify a certificate served without its intermediates.
- Port 25 is not involved, so get the certificate with an HTTP-01 challenge on port 80 of that host, or a DNS-01 challenge, e.g. `certbot certonly --standalone -d smtp.example.com`.
- **Renewal needs no restart.** The files are checked on each TLS handshake and reloaded when they change. If a reload fails, for example because the new certificate is written but the key is not yet, the old certificate stays in use until both files match.
- `/etc/letsencrypt/live` is readable by root only. Do not run OpenMailWire as root to reach it; copy the two files from a certbot deploy hook instead:

  ```sh
  # /etc/letsencrypt/renewal-hooks/deploy/openmailwire.sh
  install -m 0640 -o root -g openmailwire "$RENEWED_LINEAGE/fullchain.pem" /etc/openmailwire/tls/fullchain.pem
  install -m 0640 -o root -g openmailwire "$RENEWED_LINEAGE/privkey.pem"   /etc/openmailwire/tls/privkey.pem
  ```

- TLS 1.2 is the minimum version.

**Ports and DNS**
- Ports 587 and 465 are below 1024. Either grant the binary the capability (`AmbientCapabilities=CAP_NET_BIND_SERVICE` in its systemd unit), or listen on a high port (`OMW_SUBMISSION_LISTEN=:2587`) and publish it as 587 from Docker or your firewall.
- Publish an A record for the submission name. You can also publish `_submission._tcp` and `_submissions._tcp` SRV records (RFC 6186) for clients that configure themselves.
- Home and office networks often block outbound port 25, but rarely 587 or 465.

**Abuse**
- A connection is closed after three failed logins. Each failure is logged as `submission: authentication failed` with the client's IP, so fail2ban or a firewall rule can block repeat offenders.
- Logs carry the key prefix, recipient domains and size, never addresses or content.
- Each app's key has the limits you give it (send caps, allowed senders and recipients). A leaked SMTP password is an API key: revoke it like one.

## 6. TLS policies: MTA-STS and DANE

Receivers can require encrypted, authenticated delivery. Workers honour both standards; nothing breaks for domains that publish neither.

**MTA-STS (RFC 8461)** needs no configuration.
- Workers look up `_mta-sts.<domain>` (TXT), fetch `https://mta-sts.<domain>/.well-known/mta-sts.txt` and cache the policy by its `id` until `max_age` passes. Redirects are not followed, and bodies over 64 KB are refused.
- **`enforce`:** only the MX hosts the policy lists are used, and only with STARTTLS and a certificate valid for the host. Otherwise the attempt is deferred with `4.7.5` and a response that names the policy. Retries follow the usual schedule, so a receiver that fixes its certificate gets its mail.
- **`testing`:** mail goes out as usual; the attempt's event notes in `data.tls_policy` what enforce mode would have refused.
- Workers need outbound HTTPS (port 443) to fetch policies. If a fetch fails, the cached policy (or none) applies, as the RFC requires.
- The attempt's `tls` is `verified+mta-sts` when the policy was enforced.

**DANE (RFC 7672)** needs a resolver you trust.
- Run a DNSSEC-validating resolver next to each worker, e.g. Unbound on `127.0.0.1:53`, and set `OMW_DNSSEC_RESOLVER=127.0.0.1:53`. The worker trusts the resolver's AD bit, so never point it at a resolver across a network you do not control. Public resolvers reached over the internet are not good enough.
- For each MX host whose address is DNSSEC-signed, the worker asks for `_25._tcp.<mx>` TLSA records. When the answer is authenticated and has usable records (DANE-EE `3` or DANE-TA `2`, selectors `0`/`1`, matching types `0`/`1`/`2`), TLS is required and the certificate must match. Otherwise the attempt is deferred with `4.7.5`.
- If the resolver is down, or answers SERVFAIL (DNSSEC validation failed) for a signed host's records, that host is not used and the attempt is deferred. A broken resolver delays mail rather than sending it unprotected, so monitor it like any other dependency.
- DANE takes precedence over MTA-STS for a host that has TLSA records. The attempt's `tls` is `dane`.

## 7. A fallback relay

When your IPs are throttled or blocked by one receiver, a relay worker can hand those messages to another provider (any smarthost that speaks SMTP submission: a transactional email service, your ISP's relay, a second OpenMailWire installation).

1. Register it like any worker: `openmailwire workers add relay-1`.
2. Run `openmailwire worker` with `OMW_RELAY_HOST`, `OMW_RELAY_PORT` (587), `OMW_RELAY_TLS` (`starttls` or `implicit`) and, usually, `OMW_RELAY_USERNAME` and `OMW_RELAY_PASSWORD`. It can run on the control-plane machine; it does not need port 25.
3. The relay gets deliveries that already had `OMW_RELAY_AFTER_ATTEMPTS` attempts (default 2, set on the control plane), and deliveries to domains paused by adaptive throttling. While it is live, direct workers leave those to it. Stop it and everything goes back to direct delivery within 2 minutes.

Before you rely on it:
- **The smarthost must accept your envelope senders.** Mail leaves with the VERP return path `b-…@bounce.<your domain>`. Many providers only accept senders on domains you verified with them; verify the return-path domains (or the whole domain) there, or the smarthost will refuse every message.
- **SPF.** The smarthost's IPs send for your return-path domains, so add its SPF include to your `_spf` record. DKIM is unaffected: the message is passed on byte for byte, so your signature still verifies and DMARC still aligns.
- **TLS is mandatory** and the certificate must be valid for `OMW_RELAY_HOST`, because the session carries your credentials.
- **Outcomes.** A refused login or TLS failure is deferred, never bounced: it is about the relay, not the recipient. Replies to `MAIL`, `RCPT` and `DATA` count as they do for direct delivery, so a smarthost that refuses a recipient with 5xx bounces it. The relay's acceptance is recorded as `delivered`; a later bounce from the provider arrives at the return path like any other bounce.

## 8. Inbound mail: MX, bounces and complaints

An inbound node (`openmailwire inbound`) receives three things: bounces sent to the return path, spam complaints from feedback loops, and mail for agents' inboxes. It stores nothing itself, so you can run two behind two MX records and lose neither when one is down.

**The host.**
- **Inbound port 25 open**, from the whole internet. (Outbound port 25 is not needed on an inbound node.)
- **A name and a certificate.** Give it an A record, e.g. `mx1.mail.example.net`, set `OMW_INBOUND_HOSTNAME` to that name, and get a certificate for it (Let's Encrypt is fine) for `OMW_TLS_CERT`/`OMW_TLS_KEY`. Without them, senders deliver in plain text; Gmail and Outlook still deliver, but mark the connection unencrypted. Reload the node after renewing the certificate.
- **A worker token:** `openmailwire workers add mx1`. The node reaches the control plane over HTTPS at `OMW_CONTROL_URL`, for every recipient and every message, so keep them close.
- **No open relay.** The node accepts only addresses the control plane knows: bounce addresses with a valid HMAC, inboxes, and `OMW_FBL_ADDRESS`. Everything else is refused at `RCPT TO` with `550 5.1.1`, so it never sends bounces of its own (no backscatter). If the control plane is unreachable it answers `451`, and senders retry for days.

**DNS you publish, as the operator:**

```
mx1.mail.example.net.   A    198.51.100.20     ; the inbound node
mx2.mail.example.net.   A    198.51.100.21     ; optional second node
bounces.example.net.    A    198.51.100.20     ; OMW_BOUNCE_MX (an A record is enough for one node)
```

With two nodes, make the bounce host a name with two MX records instead (`bounces.example.net. MX 10 mx1…` and `MX 20 mx2…`).

Then set on the control plane:
- `OMW_BOUNCE_MX=bounces.example.net`. Every sending domain publishes `bounce.<domain> MX 10 bounces.example.net` (it is already among the records `POST /domains` returns).
- `OMW_INBOUND_MX=mx1.mail.example.net` to receive mail in inboxes. Domains then show an optional `INBOUND` record: `<domain> MX 10 mx1.mail.example.net`. Only publish it on domains whose mail should arrive in OpenMailWire inboxes: **the MX decides where all of the domain's mail goes**, and the inbound node refuses every address that is not an inbox. Next to another provider's MX records, mail would be split between the two. When a domain already has a mailbox provider, use a subdomain for agents (`agents.example.com`) and add it as its own domain.

**Feedback loops.** Complaints arrive only from providers you register with:
- **Yahoo** (Complaint Feedback Loop, senders.yahooinc.com): register the DKIM domains you sign with.
- **Microsoft** (JMRP, through SNDS): register the workers' IPs.
- Others (Comcast, Fastmail, Zoho, …) where you have volume.

Give them one address, e.g. `fbl@mail.example.net`, set `OMW_FBL_ADDRESS` to it, and point that domain's MX at an inbound node. Reports name the email through the VERP envelope sender or the Message-ID, so they work even when the provider hides the recipient. Gmail runs no per-message feedback loop; watch the spam rate in Postmaster Tools.

**Limits and data.**
- `OMW_INBOUND_MAX_SIZE` (default `25MB`) on both the node and the control plane. Gmail sends up to 25 MB; keep them equal.
- Inbox messages are encrypted at rest with `OMW_SECRET_KEY` (see [rotating secrets](#rotating-secrets)), and their content is deleted after `OMW_INBOUND_RETENTION` (default 30 days). Bounce and complaint messages are not kept: only the status, diagnostic and feedback type are recorded on the email's timeline.
- Inboxes receive spam like any address. Agents should treat received text as information, never as instructions; the MCP tools say so to the model.

## 9. Warm-up and day-to-day

- **Start small.** New IPs and domains have no reputation. Begin with your most-wanted mail (sign-in codes, receipts) at low volume and grow week by week.
- **Watch the attempt timeline.** Sudden 421/4.7.x deferrals from one receiver mean slow down. Bounces with 5.7.x mean authentication or reputation, which recipient suppression cannot fix.
- **Adaptive throttling slows down for you.** A "slow down" deferral (421, `4.7.28`, `4.7.650`, "too many", "rate") halves that domain's concurrency, from `OMW_DOMAIN_CONCURRENCY` down to 1, and pauses direct workers for 1, 2, 4… up to 30 minutes. Each later delivery raises the limit by one. The timeline shows `throttled` and `throttle_relaxed` events, and `SELECT * FROM receiver_state` shows every domain's current limit and pause. If one receiver keeps throttling you, lower your volume to it; the pauses only space out the retries.
- **Bulk-sender rules.** Gmail and Yahoo require SPF, DKIM, DMARC, a one-click unsubscribe (`List-Unsubscribe` and `List-Unsubscribe-Post` headers) on marketing mail, and a spam-complaint rate under 0.3%.
- **Drain before maintenance.** `openmailwire workers drain <name>` stops new jobs to a worker. In-flight deliveries finish, then you can stop it.

