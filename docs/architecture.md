# Architecture

OpenMailWire's roles are built into one binary.

| Role | Command | Holds |
|---|---|---|
| Control plane | `openmailwire serve` | The API, PostgreSQL, DKIM private keys, scheduling, suppression, events, inboxes |
| Worker | `openmailwire worker` | Nothing durable except a small outbox of unsent reports |
| Inbound node (optional) | `openmailwire inbound` | Nothing: it checks each recipient with the control plane and hands each message over before answering `250` |

## Life of a message

1. **Submit.** `POST /emails` validates the request and checks the API key's policy (allowed senders and recipients, send caps). It renders the MIME message, signs it with the domain's DKIM key, and encrypts it. In one transaction it inserts the email, one *delivery* per recipient, and a `queued` event, then returns `{"id"}`. A suppressed recipient gets a `suppressed` delivery instead of a queued one. A scheduled email's deliveries are not due until `scheduled_at`.
2. **Lease.** A worker asks for up to N jobs. In one statement the control plane:
   - picks due deliveries (`FOR UPDATE SKIP LOCKED`), skipping recipient domains that are already at the in-flight cap;
   - marks them `sending`;
   - creates a fresh attempt ID with an expiring lease;
   - returns the signed message and a VERP envelope sender.
3. **Deliver.** The worker resolves MX records (preference order, implicit MX, null MX) and connects on port 25. It offers STARTTLS when the server supports it and records `none`, `opportunistic` or `verified`. When the domain publishes an MTA-STS policy in `enforce` mode, or the MX host has DNSSEC-signed TLSA records (DANE), TLS is required and must check out (`verified+mta-sts`, `dane`); otherwise the attempt is deferred with `4.7.5`. Then it runs MAIL, RCPT and DATA. The final reply text, which usually contains the receiver's queue ID, is kept.
4. **Report.** The worker writes the outcome to its outbox, posts it, and deletes it once the control plane has accepted it. The control plane records the attempt, applies the policy (below), and appends an event.
5. **Sweep.** Every 15 seconds the control plane reclaims expired leases, expires overdue deliveries, and purges message bodies past retention (`OMW_RETENTION` for sent mail, `OMW_INBOUND_RETENTION` for inbox messages).

## Inbound mail

1. **RCPT.** For each `RCPT TO` the inbound node calls `POST /internal/inbound/check`, unless it answered the same address in the last minute (refusals: 15 seconds). The control plane answers what the address is: a bounce address (`b-<delivery>-<hmac>@<return path>`, with a valid HMAC and an existing delivery on that return path), the feedback-loop address, a report address (`dmarc@` or `tls-rpt@` a verified domain's return path), an inbox, or `postmaster@`/`abuse@` a domain the nodes serve. Anything else gets `550 5.1.1`, so no bounce is ever generated afterwards. After 10 refusals the node closes the connection; checks per client address are rate-limited.
2. **DATA.** The node adds a `Received` header and posts the message to `POST /internal/inbound`. It answers `250` only when the control plane has stored it; otherwise `451`, and the sender retries. A recipient the control plane defers (an inbox over its hourly cap) makes the whole message `451`; copies already stored are recognized as duplicates on the retry.
3. **Per recipient**, the control plane (at most `OMW_INBOUND_CONCURRENCY` messages at once):
   - **bounce address**: parses the report. A DSN with `Action: failed` sets the delivery to `bounced` (from any state but `bounced` and `suppressed`, so a late DSN after `delivered` counts), records a `bounced` event with `source: dsn`, and suppresses the address on 5.1.x. An ARF report there is a complaint about that delivery.
   - **feedback-loop address**: an ARF report that carries a valid VERP address (`Original-Mail-From`, or the quoted original's `Return-Path`) records `complained` on that delivery and suppresses the address with reason `complaint`. One that names the email only by Message-ID counts only with a DKIM signature from `OMW_FBL_TRUSTED_DOMAINS`; it is looked up among emails on the return path or sending domain it names, and records at most one complaint per email, with no recipient and no suppression.
   - **report address**: parses the DMARC aggregate (XML) or TLS-RPT (JSON) report, gzip or zip, within size, entry, depth and row limits, and stores it with its rows if it is about that domain.
   - **inbox**: checks the sender (SPF for the envelope sender and HELO with the client address the node passed, DKIM, DMARC alignment with the public-suffix list) and stores the verdict and `authentication_results`; flags text aimed at agents; seals the raw message and its snippet, links it to a thread, stores it (held from `sending_access` keys when the inbox holds unauthenticated mail) and records a `received` event, whose data carries the verdict for webhooks. Deduplication is on the Message-ID and a hash of the content, so a forged copy cannot pre-empt the genuine one; differing copies are flagged `message_id_conflict`.
4. **Threading.** `In-Reply-To`, then `References` from newest to oldest, are matched against the project's sent emails' Message-IDs and earlier inbound messages. The first match gives `thread_id`; the first sent email matched gives `in_reply_to_email_id`. A reply sent with `POST /messages/{id}/reply` is recorded in `email_threads`, so the answer to it lands in the same thread. That table keeps the send path unchanged: an email with no row starts its own thread.

## Delivery states

```
queued ──lease──▶ sending ──report──▶ delivered
   ▲                 │                 bounced     (5xx; 5.1.x also suppresses the address)
   │                 │                 expired     (the next retry would fall after expires_at)
   └──── deferred ◀──┤                 unknown     (lost final reply, and retry_uncertain = false)
                     └─lease expired──▶ deferred or unknown (by retry_uncertain)
suppressed  (the address was on the project's suppression list when the email was submitted)
canceled    (POST /emails/{id}/cancel, while the delivery was queued and never attempted)
```

## Outcomes, and why `unknown` exists

| What happened | Outcome | Retry? |
|---|---|---|
| 2xx after DATA | `delivered` | no |
| 4xx at any step, or connect/DNS failure | `deferred` | yes, with backoff 1m, 5m, 15m, 30m, 1h, 2h, 4h, 4h… until expiry |
| 5xx | `bounced` | no |
| The connection dropped after the body was sent but before the final reply | `unknown` | by default yes, with the same Message-ID |
| The worker never reported before its lease expired | `unknown` | same as above |

SMTP cannot guarantee exactly-once delivery across a lost final reply. The receiver may have queued the message. OpenMailWire says so instead of guessing. `retry_uncertain` lets each message choose:

- a duplicate login code is harmless, so retry;
- a duplicate receipt is confusing, so stop.

A late report still counts. If a worker reports `delivered` after its lease was reclaimed, the delivery is marked delivered.

## Idempotency

`Idempotency-Key` is unique per project. The request body is hashed:

- same key, same hash: the original ID;
- same key, different hash: `409`.

A unique index handles two requests that race.

## Concurrency and fairness

- The per-recipient-domain cap (`OMW_DOMAIN_CONCURRENCY`) is enforced centrally when leasing, so adding workers never multiplies the rate a receiver sees. Leases are serialized with an advisory lock, so two workers leasing at once cannot both take a domain's last slot.
- A slow receiver ties up only its own cap, not the whole queue.
- **Adaptive throttling.** `receiver_state` holds a lower limit for domains that asked us to slow down. A throttling deferral from a direct worker halves the domain's limit (minimum 1) and sets `backoff_until` to 1, 2, 4… up to 30 minutes after the n-th consecutive throttle; direct workers skip the domain until then. Each delivery raises the limit by one, up to the cap. Attempts already in flight when the domain throttled us belong to the same burst: they neither halve the limit again nor raise it.
- **Relay routing.** Workers have a kind, `direct` or `relay`. Relay workers lease only deliveries with at least `OMW_RELAY_AFTER_ATTEMPTS` attempts or whose domain is in backoff; while a relay worker is live (heartbeat in the last 2 minutes), direct workers do not lease those. Relay attempts count against their own per-domain cap and never change `receiver_state`, because the receiver sees the smarthost's IPs, not ours.

## Security model

- **API keys** (`omw_…`) and **worker tokens** (`omwk_…`) are random, shown once, and stored only as SHA-256 hashes.
- **Encryption at rest.** DKIM private keys and message bodies are sealed with AES-256-GCM under `OMW_SECRET_KEY`. Bodies are deleted after `OMW_RETENTION`.
- **Workers get only what they need:** signed messages, one lease at a time. They never see DKIM keys or other projects' data beyond the jobs they carry.
- **Project isolation.** A project's API key can read only that project's messages and domains.
- **Key permissions and policies.** A `sending_access` key can only send and read its own emails, and read and answer mail in the inboxes its policy's `inboxes` lists (none by default), never held messages. A key's policy (allowed senders and recipients, hourly and daily caps) is enforced in one place, `authorizeSend`, by every path that queues mail. The caps are re-checked under a per-key advisory lock in the insert transaction, so concurrent requests cannot overshoot them.
- **Signed bounce addresses.** VERP addresses carry an HMAC (a key derived from the secret key). The inbound node refuses bounces to addresses whose HMAC does not check out, so nobody can mark your deliveries bounced or suppress your recipients with forged DSNs. Complaint reports suppress an address only when such a VERP address names it.
- **Received mail is untrusted.** Every message has a sender verdict; unauthenticated mail is held from agent keys by default. The MCP tools show sender-written text quoted between markers with a random tag per call, never outside them, and HTML is rendered without its hidden parts.

## Database

The schema is in `internal/store/migrations`. Migrations are embedded in the binary and applied under an advisory lock by `openmailwire migrate` and on `openmailwire serve` start.
