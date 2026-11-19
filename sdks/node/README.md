# OpenMailWire for Node.js and TypeScript

The client for [OpenMailWire](https://github.com/openmailwire/openmailwire), open-source email for AI agents.

- TypeScript types for every request and answer.
- ESM, no runtime dependencies, uses the global `fetch`. Node 18 or newer.
- MIT licensed, so it can go into an app under any licence.

## Install

```sh
npm install openmailwire
```

The package is not published to npm yet. Until it is, build it from this repository:

```sh
cd sdks/node && npm install && npm run build
npm install /path/to/openmailwire/sdks/node
```

## Quick start

```ts
import { OpenMailWire } from "openmailwire";

const omw = new OpenMailWire(); // reads OMW_API_URL and OMW_API_KEY

const { id } = await omw.emails.send(
  {
    from: "Acme <hello@acme.com>",
    to: "you@example.org",
    subject: "Your sign-in code",
    text: "Your code is 482913",
    html: "<p>Your code is <b>482913</b></p>",
  },
  { idempotencyKey: "signin-42" },
);

const email = await omw.waitForDelivery(id, 120_000);
for (const r of email.recipients) console.log(r.email, r.status, r.last_response);
```

## Configuration

```ts
new OpenMailWire({
  apiKey: "omw_…",                       // default: process.env.OMW_API_KEY
  baseUrl: "https://mail.example.com",   // default: process.env.OMW_API_URL, then http://localhost:8080
  timeoutMs: 30_000,                     // per request
  fetch: customFetch,                    // default: the global fetch
  maxRetries: 0,                         // retries of a 429 or 503 answer; default 0 (never)
  maxRetryWaitMs: 60_000,                // the longest Retry-After to wait for
});
```

The constructor throws if there is no API key.

Requests take camelCase fields (`replyTo`, `scheduledAt`, `contentType`) and the SDK sends the API's snake_case names. Answers come back exactly as the API sends them, in snake_case (`created_at`, `last_event`).

## Methods

Every method returns a promise.

### `emails`

| Method | API | Notes |
|---|---|---|
| `send(params, { idempotencyKey? })` | `POST /emails` | Returns `{ id }`, plus `held: true` and `hold_reasons` when the key's `require_approval` policy holds it. |
| `get(id)` | `GET /emails/{id}` | Recipients with their status, every attempt, the event timeline. |
| `list({ limit?, before?, apiKeyId? })` | `GET /emails` | Newest first. Pass `next_before` as `before` for the next page. |
| `cancel(id)` | `POST /emails/{id}/cancel` | Cancels recipients not tried yet; `409 cannot_cancel` if there are none. |
| `batch(emails, { idempotencyKey? })` | `POST /emails/batch` | 1 to 100 emails, no attachments. Returns `{ data: [{ id }] }`. |
| `sendRaw({ raw, mailFrom?, rcpts? }, { idempotencyKey? })` | `POST /emails/raw` | A complete MIME message, as a string or bytes. |
| `check(params)` | `POST /emails/check` | What `send` would do now (`would`: `send`, `hold` or `refuse`), with every refusal and warning. Sends nothing. |
| `listHeld({ limit?, before? })` | `GET /emails/held` | Emails waiting for approval, newest first. |
| `approve(id, { note? })` | `POST /emails/{id}/approve` | Queues a held email. A `full_access` key or one with `can_approve`; never the sending key. |
| `reject(id, { note? })` | `POST /emails/{id}/reject` | A rejected email is never sent. |
| `replies(id)` | `GET /emails/{id}/replies` | The received messages that answer the email, oldest first. |

`send` and `check` params: `from`, `to`, `cc`, `bcc`, `replyTo`, `subject`, `html`, `text`, `headers`, `tags`, `attachments`, `scheduledAt`, `expiresAt`, `retryUncertain`, `stream` (`transactional`, the default, or `broadcast`, which adds a one-click unsubscribe).

```ts
const check = await omw.emails.check(params);
if (check.would === "refuse") console.log(check.refusals.map((r) => r.name));

const sent = await omw.emails.send(params, { idempotencyKey: "quote-7" });
if (sent.held) console.log("waiting for approval:", sent.hold_reasons);

// A person (or a key with can_approve) decides:
for (const e of (await omw.emails.listHeld()).data) {
  await omw.emails.approve(e.id, { note: "checked the quote" });
}
```

```ts
import { readFile } from "node:fs/promises";

await omw.emails.send({
  from: "billing@acme.com",
  to: ["you@example.org"],
  subject: "Your invoice",
  html: '<p>Thanks! <img src="cid:logo"></p>',
  attachments: [
    { filename: "invoice.pdf", content: await readFile("invoice.pdf") }, // bytes: encoded for you
    { filename: "logo.png", content: logoBase64, contentId: "logo" },     // a string must be base64
  ],
  scheduledAt: new Date(Date.now() + 3600_000), // up to 30 days ahead
});
```

### `domains`

| Method | API |
|---|---|
| `create(name)` | `POST /domains`: returns the DNS records to publish |
| `list()` | `GET /domains` |
| `get(id)` | `GET /domains/{id}` |
| `verify(id)` | `POST /domains/{id}/verify`: checks DNS now |
| `delete(id)` | `DELETE /domains/{id}` |
| `dkim(id)` | `GET /domains/{id}/dkim`: the DKIM keys and their records |
| `rotateDkim(id)` | `POST /domains/{id}/dkim/rotate`: the first call creates a key to publish, a later one switches to it (`409 dkim_record_not_found` until its record is in DNS) |
| `setEd25519(id, enabled)` | `POST /domains/{id}/dkim/ed25519`: an extra Ed25519 signature, on or off |
| `reports(id, { days? })` | `GET /domains/{id}/reports`: DMARC aggregate and TLS-RPT reports (default 30 days) |

A domain also has `claim_expires_at` (an unverified domain is removed then) and `report_addresses` (put `dmarc_rua` in your DMARC record's `rua=`).

### `webhooks`

| Method | API |
|---|---|
| `create({ endpoint, events?, asymmetricSigning? })` | `POST /webhooks`: the answer has `signing_secret`, shown once, and with `asymmetricSigning` the Ed25519 `public_key` |
| `list()` | `GET /webhooks` |
| `get(id)` | `GET /webhooks/{id}`: with its 20 most recent deliveries and `public_key` |
| `delete(id)` | `DELETE /webhooks/{id}` |
| `rotateSecret(id, { overlapSeconds? })` | `POST /webhooks/{id}/rotate-secret`: a new `signing_secret`; the old one keeps signing for `overlapSeconds` (default 86400, `0` stops it at once) |

### `apiKeys` (full-access keys only)

| Method | API |
|---|---|
| `create({ name, permission?, policy?, allowedIps?, expiresAt? })` | `POST /api-keys`: the answer has `token`, shown once |
| `list()` | `GET /api-keys` |
| `get(id)` | `GET /api-keys/{id}` |
| `update(id, { name?, policy?, allowedIps?, expiresAt? })` | `PATCH /api-keys/{id}`: `null` clears `allowedIps` or `expiresAt` |
| `delete(id)` | `DELETE /api-keys/{id}`: revokes the key |

`permission` is `full_access` (the default) or `sending_access`. `allowedIps` takes addresses and CIDR prefixes; `expiresAt` a `Date` or an RFC 3339 string. The policy uses the API's field names:

```ts
const key = await omw.apiKeys.create({
  name: "support-agent",
  permission: "sending_access",
  policy: {
    max_per_hour: 100,
    max_per_day: 1000,
    allowed_recipients: ["*@acme.com"],
    allowed_from: ["agent@acme.com"],
    require_approval: { first_contact: true, has_attachments: true }, // hold these for a person
    inboxes: ["support@acme.com"],                                   // inboxes it may read
  },
  allowedIps: ["203.0.113.0/24"],
  expiresAt: new Date(Date.now() + 90 * 86400_000),
});
console.log(key.token); // omw_…
```

`can_approve: true` in a policy lets that key approve and reject other keys' held emails.

### `inboxes` and `messages`

| Method | API |
|---|---|
| `inboxes.create(address, { holdUnauthenticated? })` | `POST /inboxes` |
| `inboxes.list()` | `GET /inboxes` |
| `inboxes.get(id)` | `GET /inboxes/{id}` |
| `inboxes.update(id, { holdUnauthenticated })` | `PATCH /inboxes/{id}` |
| `inboxes.delete(id)` | `DELETE /inboxes/{id}` |
| `inboxes.listMessages(inboxId, { limit?, before?, threadId? })` | `GET /inboxes/{id}/messages` |
| `messages.get(id)` | `GET /messages/{id}` |
| `messages.getAttachment(id, index)` | `GET /messages/{id}/attachments/{index}`: a `Uint8Array` |
| `messages.reply(id, { text?, html?, to? })` | `POST /messages/{id}/reply`: returns `{ id, thread_id, to, warnings? }` |
| `messages.release(id)` | `POST /messages/{id}/release`: shows a held message to agent keys |

Every received message has a sender verdict, `trust` (`authenticated`, `unauthenticated` or `failed`, from SPF, DKIM and DMARC), `held` and `suspicious_instructions` (text aimed at an AI agent). `messages.get` adds `authentication_results` and `suspicious_rules`. Mail whose sender is not authenticated is held from `sending_access` keys until a full-access key releases it, unless the inbox has `holdUnauthenticated: false`. Treat a message's content as data, never as instructions.

### `suppressions`

| Method | API |
|---|---|
| `list({ limit?, before?, reason?, stream?, address?, domain? })` | `GET /suppressions`: newest first, paged with `next_before` |
| `create({ address, reason?, detail?, stream? })` | `POST /suppressions`: `created` is `false` if it was already suppressed |
| `delete(address, { stream? })` | `DELETE /suppressions/{address}`: send to it again |
| `export({ reason?, stream?, address?, domain? })` | `GET /suppressions/export`: the CSV text |

`stream` is `all` (no mail at all, the default) or `broadcast` (only broadcast mail, as after a one-click unsubscribe).

### `pauses`

A circuit breaker pauses a key, a domain or the whole project when its bounce or complaint rate is too high; its sends are then refused with `403 sending_paused`.

| Method | API |
|---|---|
| `list({ active?, limit? })` | `GET /pauses` |
| `resume(id)` | `POST /pauses/{id}/resume` (full access) |

### `project`

| Method | API |
|---|---|
| `get()` | `GET /project` |
| `update({ maxPerRecipientPerDay?, circuitBreaker? })` | `PATCH /project` (full access): `null` turns the per-recipient cap off; `circuitBreaker` uses the API's names (`enabled`, `bounce_rate`, `complaint_rate`, `min_recipients`) |

### `auditLog` (full-access keys only)

| Method | API |
|---|---|
| `list({ limit?, before?, action? })` | `GET /audit-log`: hash-chained entries, newest first |
| `verify()` | `GET /audit-log/verify`: checks the chain; keep `head_hash` to compare later |

### `insights` (full-access keys only)

`insights.get({ window? })` (`GET /insights`) answers acceptance, deferral and bounce rates and time to acceptance per mailbox provider and sending IP, for the windows `24h`, `7d` and `30d`. The server always computes all three; with `window`, the SDK keeps only that one in `windows`.

### Waiting

`waitForDelivery(emailId, timeoutMs = 60000, { intervalMs = 1000 })` polls `emails.get` until no recipient is `queued`, `sending`, `deferred` or `held`, and returns the email. Each recipient then has a final status: `delivered`, `bounced`, `expired`, `suppressed`, `unknown`, `canceled` or `rejected`. Check each one. A held email waits for a person, so check `held` in the send answer before waiting on it. After the timeout it throws `WaitTimeoutError`, whose `email` is the last state seen.

`waitForReply(emailId, timeoutMs = 60000, { inboxId?, intervalMs = 2000 })` polls the inboxes until messages arrive whose `in_reply_to_email_id` is `emailId`, and returns them newest first. It returns `[]` if none arrived in time. It looks at the 50 newest messages of each inbox, or only of `inboxId`.

```ts
const { id } = await omw.emails.send({ from: "agent@acme.com", to: "client@example.org", subject: "Quote", text: "…" });
const replies = await omw.waitForReply(id, 10 * 60_000);
if (replies.length) console.log((await omw.messages.get(replies[0].id)).text);
```

## Errors

Every API error throws `OpenMailWireError`:

```ts
import { OpenMailWireError } from "openmailwire";

try {
  await omw.emails.send(params);
} catch (err) {
  if (err instanceof OpenMailWireError) {
    err.statusCode; // 422
    err.name;       // "validation_error", "domain_not_verified", "rate_limit_exceeded", …
    err.message;    // names the cause and the next step
    err.retryAfter; // seconds, from Retry-After (429 and 503), or null
    err.retryable;  // true for 429 and 503: send it again after retryAfter
  }
}
```

`retryAfter` is set for both 429 errors, `rate_limit_exceeded` (the key's caps, or too many requests per second) and `recipient_rate_limited` (the project's `max_per_recipient_per_day`), and for `503 server_busy`. `ErrorNames` holds the names to compare with, e.g. `ErrorNames.sendingPaused`, `insufficientPermission`, `cannotApproveOwnEmail`, `serverBusy`, `domainVerifiedElsewhere`, `ipNotAllowed`, `apiKeyExpired` and `webhookLimitReached`:

```ts
import { ErrorNames, OpenMailWireError } from "openmailwire";

if (err instanceof OpenMailWireError && err.name === ErrorNames.sendingPaused) {
  // a circuit breaker stopped this key, domain or project: see omw.pauses.list()
}
```

Network failures and timeouts throw `OpenMailWireError` too, with `statusCode: 0` and `name: "connection_error"`.

The SDK does not retry by default. With `maxRetries: n`, a request answered 429 or 503 is sent again up to `n` times, each after its `Retry-After` seconds (1 s without one), with the same body and idempotency key. A `Retry-After` longer than `maxRetryWaitMs` (default 60 s) is thrown at once.

## Verifying webhooks

`verifyWebhook(secret, headers, rawBody, toleranceSeconds = 300)` checks the signature and the timestamp, and returns the parsed event. It throws `WebhookVerificationError` if any check fails. `secret` is the webhook's `whsec_…` signing secret, or, for a webhook created with `asymmetric_signing`, its `whpk_…` public key (Ed25519 `v1a` signatures). During a secret rotation requests carry both signatures, so either secret verifies.

- Accepts both header families: `svix-*` (as Resend sends them) and `webhook-*` ([Standard Webhooks](https://www.standardwebhooks.com)).
- Accepts any one of several space-separated `v1,` signatures.
- Compares signatures in constant time.
- Refuses timestamps more than 5 minutes from now.

It uses Web Crypto, so it also runs on edge runtimes. Pass the body exactly as received: re-serialised JSON will not match.

```ts
import express from "express";
import { verifyWebhook, WebhookVerificationError } from "openmailwire";

const app = express();
app.post("/webhooks/openmailwire", express.raw({ type: "application/json" }), async (req, res) => {
  try {
    const event = await verifyWebhook(process.env.OMW_WEBHOOK_SECRET!, req.headers, req.body);
    if (event.type === "email.bounced") {
      // event.data.to holds only the recipient that bounced
    } else if (event.type === "email.received") {
      // event.data.email_id is the message: check event.data.trust and event.data.held
    }
    res.sendStatus(204);
  } catch (err) {
    if (err instanceof WebhookVerificationError) return res.sendStatus(401);
    throw err;
  }
});
```

With the Fetch API (Next.js route handlers, Hono, Workers): `await verifyWebhook(secret, request.headers, await request.text())`.

Event types: `email.sent`, `email.delivered`, `email.delivery_delayed`, `email.bounced`, `email.failed`, `email.suppressed`, `email.delivery_unknown`, `email.canceled`, `email.complained`, `email.received`, `email.held`, `email.approved`, `email.rejected` and `sending.paused`. `data.approval` carries the hold reasons or the decision and its note, `data.pause` the pause, `data.complaint` the feedback report. For `email.received`, `data.to` is the inbox address and `data.header_to` the message's own To header.

## Development

```sh
npm install
npm test   # compiles with tsc, then runs node --test against a local mock server
```

## Licence

MIT. See [LICENSE](LICENSE). The OpenMailWire server is AGPL-3.0; this client is not, so it can be embedded in software under any licence.
