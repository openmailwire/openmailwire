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
});
```

The constructor throws if there is no API key.

Requests take camelCase fields (`replyTo`, `scheduledAt`, `contentType`) and the SDK sends the API's snake_case names. Answers come back exactly as the API sends them, in snake_case (`created_at`, `last_event`).

## Methods

Every method returns a promise.

### `emails`

| Method | API | Notes |
|---|---|---|
| `send(params, { idempotencyKey? })` | `POST /emails` | Returns `{ id }`. |
| `get(id)` | `GET /emails/{id}` | Recipients with their status, every attempt, the event timeline. |
| `list({ limit?, before?, apiKeyId? })` | `GET /emails` | Newest first. Pass `next_before` as `before` for the next page. |
| `cancel(id)` | `POST /emails/{id}/cancel` | Cancels recipients not tried yet; `409 cannot_cancel` if there are none. |
| `batch(emails, { idempotencyKey? })` | `POST /emails/batch` | 1 to 100 emails, no attachments. Returns `{ data: [{ id }] }`. |
| `sendRaw({ raw, mailFrom?, rcpts? }, { idempotencyKey? })` | `POST /emails/raw` | A complete MIME message, as a string or bytes. |

`send` params: `from`, `to`, `cc`, `bcc`, `replyTo`, `subject`, `html`, `text`, `headers`, `tags`, `attachments`, `scheduledAt`, `expiresAt`, `retryUncertain`.

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

### `webhooks`

| Method | API |
|---|---|
| `create({ endpoint, events? })` | `POST /webhooks`: the answer has `signing_secret`, shown once |
| `list()` | `GET /webhooks` |
| `get(id)` | `GET /webhooks/{id}`: with its 20 most recent deliveries |
| `delete(id)` | `DELETE /webhooks/{id}` |

### `apiKeys` (full-access keys only)

| Method | API |
|---|---|
| `create({ name, permission?, policy? })` | `POST /api-keys`: the answer has `token`, shown once |
| `list()` | `GET /api-keys` |
| `get(id)` | `GET /api-keys/{id}` |
| `update(id, { name?, policy? })` | `PATCH /api-keys/{id}` |
| `delete(id)` | `DELETE /api-keys/{id}`: revokes the key |

`permission` is `full_access` (the default) or `sending_access`. The policy uses the API's field names:

```ts
const key = await omw.apiKeys.create({
  name: "support-agent",
  permission: "sending_access",
  policy: { max_per_hour: 100, max_per_day: 1000, allowed_recipients: ["*@acme.com"], allowed_from: ["agent@acme.com"] },
});
console.log(key.token); // omw_…
```

### `inboxes` and `messages`

| Method | API |
|---|---|
| `inboxes.create(address)` | `POST /inboxes` |
| `inboxes.list()` | `GET /inboxes` |
| `inboxes.delete(id)` | `DELETE /inboxes/{id}` |
| `inboxes.listMessages(inboxId, { limit?, before?, threadId? })` | `GET /inboxes/{id}/messages` |
| `messages.get(id)` | `GET /messages/{id}` |
| `messages.getAttachment(id, index)` | `GET /messages/{id}/attachments/{index}`: a `Uint8Array` |
| `messages.reply(id, { text?, html? })` | `POST /messages/{id}/reply`: returns `{ id }` |

### Waiting

`waitForDelivery(emailId, timeoutMs = 60000, { intervalMs = 1000 })` polls `emails.get` until no recipient is `queued`, `sending` or `deferred`, and returns the email. Each recipient then has a final status: `delivered`, `bounced`, `expired`, `suppressed`, `unknown` or `canceled`. Check each one. After the timeout it throws `WaitTimeoutError`, whose `email` is the last state seen.

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
    err.retryAfter; // seconds, from Retry-After (429), or null
  }
}
```

Network failures and timeouts throw `OpenMailWireError` too, with `statusCode: 0` and `name: "connection_error"`. The SDK never retries by itself.

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
    }
    res.sendStatus(204);
  } catch (err) {
    if (err instanceof WebhookVerificationError) return res.sendStatus(401);
    throw err;
  }
});
```

With the Fetch API (Next.js route handlers, Hono, Workers): `await verifyWebhook(secret, request.headers, await request.text())`.

## Development

```sh
npm install
npm test   # compiles with tsc, then runs node --test against a local mock server
```

## Licence

MIT. See [LICENSE](LICENSE). The OpenMailWire server is AGPL-3.0; this client is not, so it can be embedded in software under any licence.
