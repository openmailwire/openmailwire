# Architecture

OpenMailWire has two roles, built into one binary.

| Role | Command | Holds |
|---|---|---|
| Control plane | `openmailwire serve` | The API, PostgreSQL, DKIM private keys, scheduling, suppression, events |
| Worker | `openmailwire worker` | Nothing durable except a small outbox of unsent reports |

## Life of a message

1. **Submit.** `POST /emails` validates the request and checks the API key's policy (allowed senders and recipients, send caps). It renders the MIME message, signs it with the domain's DKIM key, and encrypts it. In one transaction it inserts the email, one *delivery* per recipient, and a `queued` event, then returns `{"id"}`. A suppressed recipient gets a `suppressed` delivery instead of a queued one. A scheduled email's deliveries are not due until `scheduled_at`.
2. **Lease.** A worker asks for up to N jobs. In one statement the control plane:
   - picks due deliveries (`FOR UPDATE SKIP LOCKED`), skipping recipient domains that are already at the in-flight cap;
   - marks them `sending`;
   - creates a fresh attempt ID with an expiring lease;
   - returns the signed message and a VERP envelope sender.
3. **Deliver.** The worker resolves MX records (preference order, implicit MX, null MX) and connects on port 25. It offers STARTTLS when the server supports it and records `none`, `opportunistic` or `verified`. Then it runs MAIL, RCPT and DATA. The final reply text, which usually contains the receiver's queue ID, is kept.
4. **Report.** The worker writes the outcome to its outbox, posts it, and deletes it once the control plane has accepted it. The control plane records the attempt, applies the policy (below), and appends an event.
5. **Sweep.** Every 15 seconds the control plane reclaims expired leases, expires overdue deliveries, and purges message bodies past retention.

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

- The per-recipient-domain cap (`OMW_DOMAIN_CONCURRENCY`) is enforced centrally when leasing, so adding workers never multiplies the rate a receiver sees.
- A slow receiver ties up only its own cap, not the whole queue.

## Security model

- **API keys** (`omw_…`) and **worker tokens** (`omwk_…`) are random, shown once, and stored only as SHA-256 hashes.
- **Encryption at rest.** DKIM private keys and message bodies are sealed with AES-256-GCM under `OMW_SECRET_KEY`. Bodies are deleted after `OMW_RETENTION`.
- **Workers get only what they need:** signed messages, one lease at a time. They never see DKIM keys or other projects' data beyond the jobs they carry.
- **Project isolation.** A project's API key can read only that project's messages and domains.
- **Key permissions and policies.** A `sending_access` key can only send and read its own emails. A key's policy (allowed senders and recipients, hourly and daily caps) is enforced in one place, `authorizeSend`, by every path that queues mail. The caps are re-checked under a per-key advisory lock in the insert transaction, so concurrent requests cannot overshoot them.
- **Signed bounce addresses.** VERP addresses carry an HMAC (a key derived from the secret key), so a future bounce processor can reject forged bounces.

## Database

The schema is in `internal/store/migrations`. Migrations are embedded in the binary and applied under an advisory lock by `openmailwire migrate` and on `openmailwire serve` start.
