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

Even before bounce processing ships, the return-path domain needs an MX (or an A record). Many receivers refuse envelope senders whose domain cannot receive mail.

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
