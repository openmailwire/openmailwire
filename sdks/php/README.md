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
);
```

The constructor throws `InvalidArgumentException` if there is no API key. Requests use the API's own field names (`from`, `reply_to`, `scheduled_at`), and answers are decoded JSON arrays, exactly as the API sends them.

## Methods

### `$omw->emails`

| Method | API | Notes |
|---|---|---|
| `send(array $params, ?string $idempotencyKey = null)` | `POST /emails` | Returns `['id' => …]`. |
| `get(string $id)` | `GET /emails/{id}` | Recipients with their status, every attempt, the event timeline. |
| `list(?int $limit, ?string $before, ?string $apiKeyId)` | `GET /emails` | Newest first. Pass `next_before` as `$before` for the next page. |
| `cancel(string $id)` | `POST /emails/{id}/cancel` | Cancels recipients not tried yet; `409 cannot_cancel` if there are none. |
| `batch(array $emails, ?string $idempotencyKey = null)` | `POST /emails/batch` | 1 to 100 emails, no attachments. |
| `sendRaw(string $raw, ?string $mailFrom, ?array $rcpts, ?string $idempotencyKey)` | `POST /emails/raw` | A complete MIME message, base64-encoded for you. |

`send` params: `from`, `to`, `cc`, `bcc`, `reply_to`, `subject`, `html`, `text`, `headers`, `tags`, `attachments`, `scheduled_at`, `expires_at`, `retry_uncertain`. Keys set to `null` are left out. An attachment's `content` must be base64, because a PHP string cannot tell bytes from text. `scheduled_at` and `expires_at` may be `DateTimeInterface` objects.

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

### `$omw->domains`

| Method | API |
|---|---|
| `create(string $name)` | `POST /domains`: returns the DNS records to publish |
| `list()` | `GET /domains` |
| `get(string $id)` | `GET /domains/{id}` |
| `verify(string $id)` | `POST /domains/{id}/verify`: checks DNS now |
| `delete(string $id)` | `DELETE /domains/{id}` |

### `$omw->webhooks`

| Method | API |
|---|---|
| `create(string $endpoint, array $events = [])` | `POST /webhooks`: the answer has `signing_secret`, shown once |
| `list()` | `GET /webhooks` |
| `get(string $id)` | `GET /webhooks/{id}`: with its 20 most recent deliveries |
| `delete(string $id)` | `DELETE /webhooks/{id}` |

### `$omw->apiKeys` (full-access keys only)

| Method | API |
|---|---|
| `create(string $name, ?string $permission = null, ?array $policy = null)` | `POST /api-keys`: the answer has `token`, shown once |
| `list()` | `GET /api-keys` |
| `get(string $id)` | `GET /api-keys/{id}` |
| `update(string $id, ?string $name = null, ?array $policy = null)` | `PATCH /api-keys/{id}`; `policy: []` removes every limit |
| `delete(string $id)` | `DELETE /api-keys/{id}`: revokes the key |

```php
$key = $omw->apiKeys->create('support-agent', 'sending_access', [
    'max_per_hour' => 100,
    'max_per_day' => 1000,
    'allowed_recipients' => ['*@acme.com'],
]);
echo $key['token']; // omw_…
```

### `$omw->inboxes` and `$omw->messages`

| Method | API |
|---|---|
| `inboxes->create(string $address)` | `POST /inboxes` |
| `inboxes->list()` | `GET /inboxes` |
| `inboxes->delete(string $id)` | `DELETE /inboxes/{id}` |
| `inboxes->listMessages(string $inboxId, ?int $limit, ?string $before, ?string $threadId)` | `GET /inboxes/{id}/messages` |
| `messages->get(string $id)` | `GET /messages/{id}` |
| `messages->getAttachment(string $id, int $index)` | `GET /messages/{id}/attachments/{index}`: the raw bytes |
| `messages->reply(string $id, ?string $text = null, ?string $html = null)` | `POST /messages/{id}/reply`: returns `['id' => …]` |

### Waiting

`waitForDelivery(string $emailId, float $timeout = 60.0, float $interval = 1.0)` polls `emails->get()` until no recipient is `queued`, `sending` or `deferred`, and returns the email. Each recipient then has a final status: `delivered`, `bounced`, `expired`, `suppressed`, `unknown` or `canceled`. Check each one. After `$timeout` seconds it throws `WaitTimeoutException`, whose `$email` is the last state seen.

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
    $e->retryAfter;   // seconds, from Retry-After (429), or null
}
```

Network failures and timeouts throw it too, with `statusCode` 0 and `name` `connection_error`. The SDK never retries by itself.

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

Laravel: `Webhook::verify($secret, $request->headers->all(), $request->getContent())`.

## Development

```sh
composer install
vendor/bin/phpunit   # starts php -S with tests/router.php as a local mock server
```

## Licence

MIT. See [LICENSE](LICENSE). The OpenMailWire server is AGPL-3.0; this client is not, so it can be embedded in software under any licence.
