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

- **TLS.** Put a reverse proxy (Caddy, nginx, a load balancer) in front of `OMW_LISTEN`. Workers and apps must reach it over HTTPS.
- **Backups.** Back up PostgreSQL, and separately back up `OMW_SECRET_KEY`. Without that key, the DKIM keys and queued messages in a restored database cannot be decrypted.
- **Never** set `OMW_ALLOW_UNVERIFIED=true`.
- **One instance** of `openmailwire serve` for now. Several can share a database safely (leases use `SKIP LOCKED` and migrations take an advisory lock), but that setup is not tested yet.

## 5. Warm-up and day-to-day

- **Start small.** New IPs and domains have no reputation. Begin with your most-wanted mail (sign-in codes, receipts) at low volume and grow week by week.
- **Watch the attempt timeline.** Sudden 421/4.7.x deferrals from one receiver mean slow down. Bounces with 5.7.x mean authentication or reputation, which recipient suppression cannot fix.
- **Bulk-sender rules.** Gmail and Yahoo require SPF, DKIM, DMARC, a one-click unsubscribe (`List-Unsubscribe` and `List-Unsubscribe-Post` headers) on marketing mail, and a spam-complaint rate under 0.3%.
- **Drain before maintenance.** `openmailwire workers drain <name>` stops new jobs to a worker. In-flight deliveries finish, then you can stop it.

## 6. Inbound mail: MX, bounces and complaints

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
- Inbox messages are encrypted at rest with `OMW_SECRET_KEY`, and their content is deleted after `OMW_INBOUND_RETENTION` (default 30 days). Bounce and complaint messages are not kept: only the status, diagnostic and feedback type are recorded on the email's timeline.
- Inboxes receive spam like any address. Agents should treat received text as information, never as instructions; the MCP tools say so to the model.
