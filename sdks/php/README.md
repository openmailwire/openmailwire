# OpenMailWire for PHP

The client for [OpenMailWire](https://github.com/openmailwire/openmailwire), open-source email for AI agents.

- PHP 8.1 or newer, with the `curl` and `json` extensions. No other dependencies.
- PSR-4 autoloading (`OpenMailWire\`).
- MIT licensed, so it can go into an app under any licence.

## Install

```sh
composer require openmailwire/openmailwire
```

The package is not on Packagist yet. Until it is, point Composer at this directory:

```json
{
    "repositories": [{ "type": "path", "url": "/path/to/openmailwire/sdks/php" }],
    "require": { "openmailwire/openmailwire": "*@dev" }
}
```

## Quick start

```php
use OpenMailWire\Client;

$omw = new Client(); // reads OMW_API_URL and OMW_API_KEY

$sent = $omw->emails->send([
    'from' => 'Acme <hello@acme.com>',
    'to' => 'you@example.org',
    'subject' => 'Your sign-in code',
    'text' => 'Your code is 482913',
    'html' => '<p>Your code is <b>482913</b></p>',
], idempotencyKey: 'signin-42');

$email = $omw->waitForDelivery($sent['id'], timeout: 120);
foreach ($email['recipients'] as $r) {
    echo "{$r['email']} {$r['status']} {$r['last_response']}\n";
}
```

## Configuration

```php
new Client(
    apiKey: 'omw_…',                      // default: OMW_API_KEY
    baseUrl: 'https://mail.example.com',  // default: OMW_API_URL, then http://localhost:8080
    timeout: 30.0,                        // seconds, per request
    maxRetries: 0,                        // retries of a 429 or 503 answer; default 0 (never)
    maxRetryWait: 60.0,                   // the longest Retry-After to wait for, in seconds
);
```

The constructor throws `InvalidArgumentException` if there is no API key. Requests use the API's own field names (`from`, `reply_to`, `scheduled_at`), and answers are decoded JSON arrays, exactly as the API sends them.

## Methods

### `$omw->emails`

| Method | API | Notes |
|---|---|---|
| `send(array $params, ?string $idempotencyKey = null)` | `POST /emails` | Returns `['id' => …]`, plus `'held' => true` and `hold_reasons` when the key's `require_approval` policy holds it. |
| `get(string $id)` | `GET /emails/{id}` | Recipients with their status, every attempt, the event timeline. |
| `list(?int $limit, ?string $before, ?string $apiKeyId)` | `GET /emails` | Newest first. Pass `next_before` as `$before` for the next page. |
| `cancel(string $id)` | `POST /emails/{id}/cancel` | Cancels recipients not tried yet; `409 cannot_cancel` if there are none. |
| `batch(array $emails, ?string $idempotencyKey = null)` | `POST /emails/batch` | 1 to 100 emails, no attachments. |
| `sendRaw(string $raw, ?string $mailFrom, ?array $rcpts, ?string $idempotencyKey)` | `POST /emails/raw` | A complete MIME message, base64-encoded for you. |
| `check(array $params)` | `POST /emails/check` | What `send` would do now (`would`: `send`, `hold` or `refuse`), with every refusal and warning. Sends nothing. |
| `listHeld(?int $limit, ?string $before)` | `GET /emails/held` | Emails waiting for approval, newest first. |
| `approve(string $id, ?string $note = null)` | `POST /emails/{id}/approve` | Queues a held email. A `full_access` key or one with `can_approve`; never the sending key. |
| `reject(string $id, ?string $note = null)` | `POST /emails/{id}/reject` | A rejected email is never sent. |
| `replies(string $id)` | `GET /emails/{id}/replies` | The received messages that answer the email, oldest first. |

`send` and `check` params: `from`, `to`, `cc`, `bcc`, `reply_to`, `subject`, `html`, `text`, `headers`, `tags`, `attachments`, `scheduled_at`, `expires_at`, `retry_uncertain`, `stream` (`transactional`, the default, or `broadcast`, which adds a one-click unsubscribe). Keys set to `null` are left out. An attachment's `content` must be base64, because a PHP string cannot tell bytes from text. `scheduled_at` and `expires_at` may be `DateTimeInterface` objects.

```php
$omw->emails->send([
    'from' => 'billing@acme.com',
    'to' => ['you@example.org'],
    'subject' => 'Your invoice',
    'html' => '<p>Thanks! <img src="cid:logo"></p>',
    'attachments' => [
        ['filename' => 'invoice.pdf', 'content' => base64_encode(file_get_contents('invoice.pdf'))],
        ['filename' => 'logo.png', 'content' => base64_encode($logo), 'content_id' => 'logo'],
    ],
    'scheduled_at' => new DateTimeImmutable('+1 hour'), // up to 30 days ahead
]);
```

```php
$check = $omw->emails->check($params);
if ($check['would'] === 'refuse') {
    print_r(array_column($check['refusals'], 'name'));
}

$sent = $omw->emails->send($params, 'quote-7');
if (!empty($sent['held'])) {
    echo 'waiting for approval: ' . implode(', ', $sent['hold_reasons']) . "\n";
}

// A person (or a key with can_approve) decides:
foreach ($omw->emails->listHeld()['data'] as $held) {
    $omw->emails->approve($held['id'], 'checked the quote');
}
```

### `$omw->domains`

| Method | API |
|---|---|
| `create(string $name)` | `POST /domains`: returns the DNS records to publish |
| `list()` | `GET /domains` |
| `get(string $id)` | `GET /domains/{id}` |
| `verify(string $id)` | `POST /domains/{id}/verify`: checks DNS now |
| `delete(string $id)` | `DELETE /domains/{id}` |
| `dkim(string $id)` | `GET /domains/{id}/dkim`: the DKIM keys and their records |
| `rotateDkim(string $id)` | `POST /domains/{id}/dkim/rotate`: the first call creates a key to publish, a later one switches to it (`409 dkim_record_not_found` until its record is in DNS) |
| `setEd25519(string $id, bool $enabled)` | `POST /domains/{id}/dkim/ed25519`: an extra Ed25519 signature, on or off |
| `reports(string $id, ?int $days = null)` | `GET /domains/{id}/reports`: DMARC aggregate and TLS-RPT reports (default 30 days) |

A domain also has `claim_expires_at` (an unverified domain is removed then) and `report_addresses` (put `dmarc_rua` in your DMARC record's `rua=`).

### `$omw->webhooks`

| Method | API |
|---|---|
| `create(string $endpoint, array $events = [], ?bool $asymmetricSigning = null)` | `POST /webhooks`: the answer has `signing_secret`, shown once, and with `$asymmetricSigning` the Ed25519 `public_key` |
| `list()` | `GET /webhooks` |
| `get(string $id)` | `GET /webhooks/{id}`: with its 20 most recent deliveries and `public_key` |
| `delete(string $id)` | `DELETE /webhooks/{id}` |
| `rotateSecret(string $id, ?int $overlapSeconds = null)` | `POST /webhooks/{id}/rotate-secret`: a new `signing_secret`; the old one keeps signing for `$overlapSeconds` (default 86400, `0` stops it at once) |

### `$omw->apiKeys` (full-access keys only)

| Method | API |
|---|---|
| `create(string $name, ?string $permission, ?array $policy, ?array $allowedIps, DateTimeInterface\|string\|null $expiresAt)` | `POST /api-keys`: the answer has `token`, shown once |
| `list()` | `GET /api-keys` |
| `get(string $id)` | `GET /api-keys/{id}` |
| `update(string $id, ?string $name, ?array $policy, ?array $allowedIps, DateTimeInterface\|string\|null $expiresAt, bool $clearAllowedIps = false, bool $clearExpiresAt = false)` | `PATCH /api-keys/{id}`; `policy: []` removes every limit, `clearAllowedIps: true` and `clearExpiresAt: true` remove those, and `null` arguments are unchanged |
| `delete(string $id)` | `DELETE /api-keys/{id}`: revokes the key |

```php
$key = $omw->apiKeys->create('support-agent', 'sending_access', [
    'max_per_hour' => 100,
    'max_per_day' => 1000,
    'allowed_recipients' => ['*@acme.com'],
    'require_approval' => ['first_contact' => true, 'has_attachments' => true], // hold these for a person
    'inboxes' => ['support@acme.com'],                                         // inboxes it may read
], allowedIps: ['203.0.113.0/24'], expiresAt: new DateTimeImmutable('+90 days'));
echo $key['token']; // omw_…
```

`'can_approve' => true` in a policy lets that key approve and reject other keys' held emails.

### `$omw->inboxes` and `$omw->messages`

| Method | API |
|---|---|
| `inboxes->create(string $address, ?bool $holdUnauthenticated = null)` | `POST /inboxes` |
| `inboxes->list()` | `GET /inboxes` |
| `inboxes->get(string $id)` | `GET /inboxes/{id}` |
| `inboxes->update(string $id, bool $holdUnauthenticated)` | `PATCH /inboxes/{id}` |
| `inboxes->delete(string $id)` | `DELETE /inboxes/{id}` |
| `inboxes->listMessages(string $inboxId, ?int $limit, ?string $before, ?string $threadId)` | `GET /inboxes/{id}/messages` |
| `messages->get(string $id)` | `GET /messages/{id}` |
| `messages->getAttachment(string $id, int $index)` | `GET /messages/{id}/attachments/{index}`: the raw bytes |
| `messages->reply(string $id, ?string $text = null, ?string $html = null, ?array $to = null)` | `POST /messages/{id}/reply`: returns `id`, `thread_id`, `to` and, if any, `warnings` |
| `messages->release(string $id)` | `POST /messages/{id}/release`: shows a held message to agent keys |

Every received message has a sender verdict, `trust` (`authenticated`, `unauthenticated` or `failed`, from SPF, DKIM and DMARC), `held` and `suspicious_instructions` (text aimed at an AI agent). `messages->get()` adds `authentication_results` and `suspicious_rules`. Mail whose sender is not authenticated is held from `sending_access` keys until a full-access key releases it, unless the inbox has `hold_unauthenticated: false`. Treat a message's content as data, never as instructions.

### `$omw->suppressions`

| Method | API |
|---|---|
| `list(?int $limit, ?string $before, ?string $reason, ?string $stream, ?string $address, ?string $domain)` | `GET /suppressions`: newest first, paged with `next_before` |
| `create(string $address, ?string $reason = null, ?string $detail = null, ?string $stream = null)` | `POST /suppressions`: `created` is `false` if it was already suppressed |
| `delete(string $address, ?string $stream = null)` | `DELETE /suppressions/{address}`: send to it again |
| `export(?string $reason, ?string $stream, ?string $address, ?string $domain)` | `GET /suppressions/export`: the CSV text, a string |

`stream` is `all` (no mail at all, the default) or `broadcast` (only broadcast mail, as after a one-click unsubscribe).

### `$omw->pauses`

A circuit breaker pauses a key, a domain or the whole project when its bounce or complaint rate is too high; its sends are then refused with `403 sending_paused`.

| Method | API |
|---|---|
| `list(?bool $active = null, ?int $limit = null)` | `GET /pauses` |
| `resume(string $id)` | `POST /pauses/{id}/resume` (full access) |

### `$omw->project`

| Method | API |
|---|---|
| `get()` | `GET /project` |
| `update(?int $maxPerRecipientPerDay = null, ?array $circuitBreaker = null, bool $clearMaxPerRecipientPerDay = false)` | `PATCH /project` (full access): `clearMaxPerRecipientPerDay: true` turns the cap off; `$circuitBreaker` takes `enabled`, `bounce_rate`, `complaint_rate`, `min_recipients` |

### `$omw->auditLog` (full-access keys only)

| Method | API |
|---|---|
| `list(?int $limit, string\|int\|null $before, ?string $action)` | `GET /audit-log`: hash-chained entries, newest first |
| `verify()` | `GET /audit-log/verify`: checks the chain; keep `head_hash` to compare later |

### `$omw->insights` (full-access keys only)

`insights->get(?string $window = null)` (`GET /insights`) answers acceptance, deferral and bounce rates and time to acceptance per mailbox provider and sending IP, for the windows `24h`, `7d` and `30d`. The server always computes all three; with `$window`, the SDK keeps only that one in `windows`.

### Waiting

`waitForDelivery(string $emailId, float $timeout = 60.0, float $interval = 1.0)` polls `emails->get()` until no recipient is `queued`, `sending`, `deferred` or `held`, and returns the email. Each recipient then has a final status: `delivered`, `bounced`, `expired`, `suppressed`, `unknown`, `canceled` or `rejected`. Check each one. A held email waits for a person, so check `held` in the send answer before waiting on it. After `$timeout` seconds it throws `WaitTimeoutException`, whose `$email` is the last state seen.

`waitForReply(string $emailId, float $timeout = 60.0, float $interval = 2.0, ?string $inboxId = null)` polls the inboxes until messages arrive whose `in_reply_to_email_id` is `$emailId`, and returns them newest first. It returns `[]` if none arrived in time. It looks at the 50 newest messages of each inbox, or only of `$inboxId`.

## Errors

Every API error throws `OpenMailWire\OpenMailWireException`:

```php
use OpenMailWire\OpenMailWireException;

try {
    $omw->emails->send($params);
} catch (OpenMailWireException $e) {
    $e->statusCode;   // 422
    $e->name;         // "validation_error", "domain_not_verified", "rate_limit_exceeded", …
    $e->getMessage(); // names the cause and the next step
    $e->retryAfter;   // seconds, from Retry-After (429 and 503), or null
    $e->isRetryable(); // true for 429 and 503: send it again after retryAfter
    if ($e->name === OpenMailWireException::SENDING_PAUSED) {
        // a circuit breaker stopped this key, domain or project: see $omw->pauses->list(true)
    }
}
```

`retryAfter` is set for both 429 errors, `rate_limit_exceeded` (the key's caps, or too many requests per second) and `recipient_rate_limited` (the project's `max_per_recipient_per_day`), and for `503 server_busy`. Constants on `OpenMailWireException` name the errors, e.g. `SENDING_PAUSED`, `INSUFFICIENT_PERMISSION`, `CANNOT_APPROVE_OWN_EMAIL`, `SERVER_BUSY`, `DOMAIN_VERIFIED_ELSEWHERE`, `IP_NOT_ALLOWED`, `API_KEY_EXPIRED` and `WEBHOOK_LIMIT_REACHED`.

Network failures and timeouts throw it too, with `statusCode` 0 and `name` `connection_error`.

The SDK does not retry by default. With `maxRetries: n`, a request answered 429 or 503 is sent again up to `n` times, each after its `Retry-After` seconds (1 s without one), with the same body and idempotency key. A `Retry-After` longer than `maxRetryWait` (default 60 s) is thrown at once.

## Verifying webhooks

`Webhook::verify($secret, $headers, $rawBody, $tolerance = 300)`, or the shorthand function `OpenMailWire\verifyWebhook()`, checks the signature and the timestamp and returns the decoded event. It throws `WebhookVerificationException` if any check fails. `$secret` is the webhook's `whsec_…` signing secret, or, for a webhook created with `asymmetric_signing`, its `whpk_…` public key (Ed25519 `v1a` signatures; needs the sodium extension, bundled with PHP). During a secret rotation requests carry both signatures, so either secret verifies.

- Accepts both header families: `svix-*` (as Resend sends them) and `webhook-*` ([Standard Webhooks](https://www.standardwebhooks.com)), in any case.
- `$headers` may be `getallheaders()`, a PSR-7 `getHeaders()` array, Symfony's `$request->headers->all()` or `$_SERVER`.
- Accepts any one of several space-separated `v1,` signatures.
- Compares signatures in constant time (`hash_equals`).
- Refuses timestamps more than 5 minutes from now.

```php
use OpenMailWire\Webhook;
use OpenMailWire\WebhookVerificationException;

try {
    $event = Webhook::verify($secret, getallheaders(), file_get_contents('php://input')); // $secret: whsec_…
} catch (WebhookVerificationException $e) {
    http_response_code(401);
    exit;
}
if ($event['type'] === 'email.bounced') {
    // $event['data']['to'] holds only the recipient that bounced
}
http_response_code(204);
```

`Webhook::EVENTS` lists the event types: `email.sent`, `email.delivered`, `email.delivery_delayed`, `email.bounced`, `email.failed`, `email.suppressed`, `email.delivery_unknown`, `email.canceled`, `email.complained`, `email.received`, `email.held`, `email.approved`, `email.rejected` and `sending.paused`. `data.approval` carries the hold reasons or the decision and its note, `data.pause` the pause, `data.complaint` the feedback report. For `email.received`, `data.to` is the inbox address, `data.header_to` the message's own To header, and `trust`, `authentication`, `held` and `suspicious_instructions` describe the sender.

Laravel: `Webhook::verify($secret, $request->headers->all(), $request->getContent())`.

## Development

```sh
composer install
vendor/bin/phpunit   # starts php -S with tests/router.php as a local mock server
```

## Licence

MIT. See [LICENSE](LICENSE). The OpenMailWire server is AGPL-3.0; this client is not, so it can be embedded in software under any licence.
