# Architecture

OpenMailWire's roles are built into one binary.

| Role | Command | Holds |
|---|---|---|
| Control plane | `openmailwire serve` | The API, PostgreSQL, DKIM private keys, scheduling, suppression, events, inboxes |
| Worker | `openmailwire worker` | Nothing durable except a small outbox of unsent reports |
| Inbound node (optional) | `openmailwire inbound` | Nothing: it checks each recipient with the control plane and hands each message over before answering `250` |

## Life of a message

1. **Submit.** `POST /emails` validates the request and checks the API key's policy (allowed senders and recipients, send caps). It renders the MIME message, signs it with the domain's DKIM key, and encrypts it. In one transaction it inserts the email, one *delivery* per recipient, and a `queued` event, then returns `{"id"}`. A suppressed recipient gets a `suppressed` delivery instead of a queued one. A scheduled email's deliveries are not due until `scheduled_at`.
2. **Lease.** A worker asks for up to N jobs. Under the lease lock the control plane:
   - reads the oldest due deliveries (a fixed number, however long the queue), and only when they are all held up goes on domain by domain from where the last such lease stopped, so a backlog on one busy domain never slows leasing down;
   - skips recipient domains at their in-flight cap, providers in backoff or at their limit for the worker's IP, and providers over the day's warm-up cap of a new IP;
   - locks the chosen deliveries (`FOR UPDATE SKIP LOCKED`) and checks their status again, so a cancel or an expiry that committed meanwhile wins;
   - marks them `sending` and creates a fresh attempt ID with an expiring lease;
   - returns the signed message and a VERP envelope sender. A scheduled message gets a new `Date` and DKIM signature here; a message the control plane cannot decrypt is given back at once (`unseal_failed`), not sent.
3. **Deliver.** The worker resolves MX records (preference order, implicit MX, null MX) and connects on port 25, or reuses an open connection to that server (`RSET` in between). It uses STARTTLS when the server offers it and records `none`, `opportunistic` or `verified`. When the domain publishes an MTA-STS policy in `enforce` mode, or the MX host has DNSSEC-signed TLSA records (DANE), TLS is required and must check out (`verified+mta-sts`, `dane`); otherwise the attempt is deferred with `4.7.5`. Then it runs MAIL, RCPT and DATA, with a deadline per command, per block of the message and for the final reply, all inside the lease. The final reply text, which usually contains the receiver's queue ID, is kept, with the step it came from.
4. **Report.** The worker writes the outcome to its outbox, posts it, and deletes it once the control plane has accepted it. The control plane records the attempt, applies the policy (below), and appends an event.
5. **Sweep.** Every 15 seconds the control plane reclaims expired leases, expires overdue deliveries, and purges message bodies past retention (`OMW_RETENTION` for sent mail, `OMW_INBOUND_RETENTION` for inbox messages).

## Inbound mail

1. **RCPT.** For each `RCPT TO` the inbound node calls `POST /internal/inbound/check`. The control plane answers what the address is: a bounce address (`b-<delivery>-<hmac>@<return path>`, with a valid HMAC and an existing delivery on that return path), the feedback-loop address, or an inbox. Anything else gets `550 5.1.1`, so no bounce is ever generated afterwards.
2. **DATA.** The node adds a `Received` header and posts the message to `POST /internal/inbound`. It answers `250` only when the control plane has stored it; otherwise `451`, and the sender retries.
3. **Per recipient**, the control plane:
   - **bounce address**: parses the report. A DSN with `Action: failed` sets the delivery to `bounced` (from any state but `bounced` and `suppressed`, so a late DSN after `delivered` counts), records a `bounced` event with `source: dsn`, and suppresses the address on 5.1.x. An ARF report there is a complaint about that delivery.
   - **feedback-loop address**: an ARF report is matched to a delivery through the VERP sender it quotes, or else the original Message-ID and recipient. It records `complained` and suppresses the address with reason `complaint`.
   - **inbox**: seals the raw message, links it to a thread, stores it and records a `received` event (events now point at an email or at an inbound message).
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
| 4xx at any step, or connect/DNS failure | `deferred` | yes: first after 5–15 min, then 30m, 1h, 2h, 4h, 4h… until expiry; greylisting after 5–15 min each time |
| 5xx | `bounced` | no |
| The connection dropped after the body was sent but before the final reply | `unknown` | by default yes, with the same Message-ID |
| The worker never reported before its lease expired | `unknown` | same as above |

SMTP cannot guarantee exactly-once delivery across a lost final reply. The receiver may have queued the message. OpenMailWire says so instead of guessing. `retry_uncertain` lets each message choose:

- a duplicate login code is harmless, so retry;
- a duplicate receipt is confusing, so stop.

A late report still counts. If a worker reports `delivered` after its lease was reclaimed, the delivery is marked delivered.

## Idempotency

`Idempotency-Key` is unique per API key, so one key can never replay, reveal or block another's requests. Batch items get keys in a namespace of their own (the batch's key, a control character, the item's index), which no header can produce. The request body is hashed:

- same key, same hash: the original ID;
- same key, different hash: `409`.

A unique index handles two requests that race.

## Concurrency and fairness

- The per-recipient-domain cap (`OMW_DOMAIN_CONCURRENCY`) is enforced centrally when leasing, so adding workers never multiplies the rate a receiver sees. Leases are serialized with an advisory lock, so two workers leasing at once cannot both take a domain's last slot.
- A slow receiver ties up only its own cap, not the whole queue.
- **Providers and sending IPs.** Receivers limit and judge senders per IP, and many domains share one provider's servers. So recipient domains are grouped into scopes: the mailbox provider their MX belongs to (`rcpt_domain_providers`, learned from reports and the rules in `internal/policy/providers.json`), or the domain itself. Each scope has an in-flight limit per sending IP (the provider's `concurrency`, or `OMW_DOMAIN_CONCURRENCY`).
- **Adaptive throttling.** `receiver_state` holds a lower limit for a scope and IP that asked us to slow down. Only connection- or IP-level replies count (a 421, or `4.7.0`, `4.7.28`, `4.7.650` before RCPT), never a mailbox's or an authentication code. The first one is noted; when a second comes from a different recipient within 15 minutes, the limit halves (minimum 1) and `backoff_until` is set 1, 2, 4… up to 30 minutes after the n-th consecutive throttle; direct workers with that IP skip the scope until then. Each delivery raises the limit by one, up to the cap. Attempts already in flight when the scope throttled us belong to the same burst: they neither halve the limit again nor raise it. Authentication codes are recorded per project and sending domain (`sender_auth_issues`).
- **Warm-up.** `sending_ips` records when each IP started; `warmup_usage` counts its leased attempts per provider and UTC day against the rules' schedule. IPs that sent before v0.6 are warm.
- **Relay routing.** Workers have a kind, `direct` or `relay`. Relay workers lease only deliveries with at least `OMW_RELAY_AFTER_ATTEMPTS` attempts, greylisting not counted, or whose scope no live direct worker can take now (backoff or warm-up cap on every direct IP); while a relay worker is live (heartbeat in the last 2 minutes), direct workers do not lease the former. Relay attempts count against their own per-domain cap and never change `receiver_state`, because the receiver sees the smarthost's IPs, not ours.
- **Lock order.** Reports, the sweep and leases lock a delivery before its attempt, so they never deadlock.

## Security model

- **API keys** (`omw_…`) and **worker tokens** (`omwk_…`) are random, shown once, and stored only as SHA-256 hashes. A worker token has one role (`delivery`, `relay` or `inbound`) and is refused elsewhere.
- **Encryption at rest.** DKIM private keys and message bodies are sealed with AES-256-GCM under `OMW_SECRET_KEY`. Bodies are deleted after `OMW_RETENTION`.
- **Workers get only what they need:** signed messages, one lease at a time. They never see DKIM keys or other projects' data beyond the jobs they carry.
- **Project isolation.** A project's API key can read only that project's messages and domains.
- **Key permissions and policies.** A `sending_access` key can only send and read its own emails. A key's policy (allowed senders and recipients, hourly and daily caps) is enforced in one place, `authorizeSend`, by every path that queues mail. The caps are re-checked under a per-key advisory lock in the insert transaction, so concurrent requests cannot overshoot them.
- **Signed bounce addresses.** VERP addresses carry an HMAC (a key derived from the secret key). The inbound node refuses bounces to addresses whose HMAC does not check out, so nobody can mark your deliveries bounced or suppress your recipients with forged DSNs.

## Database

The schema is in `internal/store/migrations`. Migrations are embedded in the binary and applied under an advisory lock by `openmailwire migrate` and on `openmailwire serve` start.
