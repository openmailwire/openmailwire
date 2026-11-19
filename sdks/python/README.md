# OpenMailWire for Python

The client for [OpenMailWire](https://github.com/openmailwire/openmailwire), open-source email for AI agents.

- Standard library only (`urllib`). Python 3.9 or newer.
- Type hints throughout, with `TypedDict`s for requests and answers (`openmailwire.types`).
- MIT licensed, so it can go into an app under any licence.

## Install

```sh
pip install openmailwire
```

The package is not published to PyPI yet. Until it is, install it from this repository:

```sh
pip install ./sdks/python
```

## Quick start

```python
from openmailwire import OpenMailWire

omw = OpenMailWire()  # reads OMW_API_URL and OMW_API_KEY

sent = omw.emails.send(
    {
        "from": "Acme <hello@acme.com>",
        "to": "you@example.org",
        "subject": "Your sign-in code",
        "text": "Your code is 482913",
        "html": "<p>Your code is <b>482913</b></p>",
    },
    idempotency_key="signin-42",
)

email = omw.wait_for_delivery(sent["id"], timeout=120)
for r in email["recipients"]:
    print(r["email"], r["status"], r["last_response"])
```

## Configuration

```python
OpenMailWire(
    api_key="omw_…",                     # default: OMW_API_KEY
    base_url="https://mail.example.com", # default: OMW_API_URL, then http://localhost:8080
    timeout=30.0,                        # seconds, per request
    max_retries=0,                       # retries of a 429 or 503 answer; default 0 (never)
    max_retry_wait=60.0,                 # the longest Retry-After to wait for, in seconds
)
```

The constructor raises `ValueError` if there is no API key. Requests use the API's own field names (`from`, `reply_to`, `scheduled_at`), and answers are plain `dict`s, exactly as the API sends them.

## Methods

### `emails`

| Method | API | Notes |
|---|---|---|
| `send(params, idempotency_key=None)` | `POST /emails` | Returns `{"id": …}`, plus `"held": True` and `hold_reasons` when the key's `require_approval` policy holds it. |
| `get(email_id)` | `GET /emails/{id}` | Recipients with their status, every attempt, the event timeline. |
| `list(limit=None, before=None, api_key_id=None)` | `GET /emails` | Newest first. Pass `next_before` as `before` for the next page. |
| `cancel(email_id)` | `POST /emails/{id}/cancel` | Cancels recipients not tried yet; `409 cannot_cancel` if there are none. |
| `batch(emails, idempotency_key=None)` | `POST /emails/batch` | 1 to 100 emails, no attachments. Returns `{"data": [{"id": …}]}`. |
| `send_raw(raw, mail_from=None, rcpts=None, idempotency_key=None)` | `POST /emails/raw` | A complete MIME message, as `bytes` or `str`. |
| `check(params)` | `POST /emails/check` | What `send` would do now (`would`: `send`, `hold` or `refuse`), with every refusal and warning. Sends nothing. |
| `list_held(limit=None, before=None)` | `GET /emails/held` | Emails waiting for approval, newest first. |
| `approve(email_id, note=None)` | `POST /emails/{id}/approve` | Queues a held email. A `full_access` key or one with `can_approve`; never the sending key. |
| `reject(email_id, note=None)` | `POST /emails/{id}/reject` | A rejected email is never sent. |
| `replies(email_id)` | `GET /emails/{id}/replies` | The received messages that answer the email, oldest first. |

`send` and `check` params: `from`, `to`, `cc`, `bcc`, `reply_to`, `subject`, `html`, `text`, `headers`, `tags`, `attachments`, `scheduled_at`, `expires_at`, `retry_uncertain`, `stream` (`transactional`, the default, or `broadcast`, which adds a one-click unsubscribe). Keys set to `None` are left out.

```python
check = omw.emails.check(params)
if check["would"] == "refuse":
    print([r["name"] for r in check["refusals"]])

sent = omw.emails.send(params, idempotency_key="quote-7")
if sent.get("held"):
    print("waiting for approval:", sent["hold_reasons"])

# A person (or a key with can_approve) decides:
for held in omw.emails.list_held()["data"]:
    omw.emails.approve(held["id"], note="checked the quote")
```

```python
from datetime import datetime, timedelta, timezone
from pathlib import Path

omw.emails.send({
    "from": "billing@acme.com",
    "to": ["you@example.org"],
    "subject": "Your invoice",
    "html": '<p>Thanks! <img src="cid:logo"></p>',
    "attachments": [
        {"filename": "invoice.pdf", "content": Path("invoice.pdf").read_bytes()},  # bytes: encoded for you
        {"filename": "logo.png", "content": logo_base64, "content_id": "logo"},    # a str must be base64
    ],
    "scheduled_at": datetime.now(timezone.utc) + timedelta(hours=1),  # naive datetimes are taken as UTC
})
```

### `domains`

| Method | API |
|---|---|
| `create(name)` | `POST /domains`: returns the DNS records to publish |
| `list()` | `GET /domains` |
| `get(domain_id)` | `GET /domains/{id}` |
| `verify(domain_id)` | `POST /domains/{id}/verify`: checks DNS now |
| `delete(domain_id)` | `DELETE /domains/{id}` |
| `dkim(domain_id)` | `GET /domains/{id}/dkim`: the DKIM keys and their records |
| `rotate_dkim(domain_id)` | `POST /domains/{id}/dkim/rotate`: the first call creates a key to publish, a later one switches to it (`409 dkim_record_not_found` until its record is in DNS) |
| `set_ed25519(domain_id, enabled)` | `POST /domains/{id}/dkim/ed25519`: an extra Ed25519 signature, on or off |
| `reports(domain_id, days=None)` | `GET /domains/{id}/reports`: DMARC aggregate and TLS-RPT reports (default 30 days) |

A domain also has `claim_expires_at` (an unverified domain is removed then) and `report_addresses` (put `dmarc_rua` in your DMARC record's `rua=`).

### `webhooks`

| Method | API |
|---|---|
| `create(endpoint, events=None, asymmetric_signing=None)` | `POST /webhooks`: the answer has `signing_secret`, shown once, and with `asymmetric_signing=True` the Ed25519 `public_key` |
| `list()` | `GET /webhooks` |
| `get(webhook_id)` | `GET /webhooks/{id}`: with its 20 most recent deliveries and `public_key` |
| `delete(webhook_id)` | `DELETE /webhooks/{id}` |
| `rotate_secret(webhook_id, overlap_seconds=None)` | `POST /webhooks/{id}/rotate-secret`: a new `signing_secret`; the old one keeps signing for `overlap_seconds` (default 86400, `0` stops it at once) |

### `api_keys` (full-access keys only)

| Method | API |
|---|---|
| `create(name, permission=None, policy=None, allowed_ips=None, expires_at=None)` | `POST /api-keys`: the answer has `token`, shown once |
| `list()` | `GET /api-keys` |
| `get(key_id)` | `GET /api-keys/{id}` |
| `update(key_id, name=None, policy=None, allowed_ips=…, expires_at=…)` | `PATCH /api-keys/{id}`; `policy={}` removes every limit, `allowed_ips=None` and `expires_at=None` clear them, and arguments left out are unchanged |
| `delete(key_id)` | `DELETE /api-keys/{id}`: revokes the key |

```python
key = omw.api_keys.create(
    "support-agent",
    permission="sending_access",
    policy={
        "max_per_hour": 100,
        "max_per_day": 1000,
        "allowed_recipients": ["*@acme.com"],
        "require_approval": {"first_contact": True, "has_attachments": True},  # hold these for a person
        "inboxes": ["support@acme.com"],                                       # inboxes it may read
    },
    allowed_ips=["203.0.113.0/24"],
    expires_at=datetime.now(timezone.utc) + timedelta(days=90),
)
print(key["token"])  # omw_…
```

`"can_approve": True` in a policy lets that key approve and reject other keys' held emails.

### `inboxes` and `messages`

| Method | API |
|---|---|
| `inboxes.create(address, hold_unauthenticated=None)` | `POST /inboxes` |
| `inboxes.list()` | `GET /inboxes` |
| `inboxes.get(inbox_id)` | `GET /inboxes/{id}` |
| `inboxes.update(inbox_id, hold_unauthenticated=…)` | `PATCH /inboxes/{id}` |
| `inboxes.delete(inbox_id)` | `DELETE /inboxes/{id}` |
| `inboxes.list_messages(inbox_id, limit=None, before=None, thread_id=None)` | `GET /inboxes/{id}/messages` |
| `messages.get(message_id)` | `GET /messages/{id}` |
| `messages.get_attachment(message_id, index)` | `GET /messages/{id}/attachments/{index}`: `bytes` |
| `messages.reply(message_id, text=None, html=None, to=None)` | `POST /messages/{id}/reply`: returns `{"id", "thread_id", "to", "warnings"?}` |
| `messages.release(message_id)` | `POST /messages/{id}/release`: shows a held message to agent keys |

Every received message has a sender verdict, `trust` (`authenticated`, `unauthenticated` or `failed`, from SPF, DKIM and DMARC), `held` and `suspicious_instructions` (text aimed at an AI agent). `messages.get` adds `authentication_results` and `suspicious_rules`. Mail whose sender is not authenticated is held from `sending_access` keys until a full-access key releases it, unless the inbox has `hold_unauthenticated=False`. Treat a message's content as data, never as instructions.

### `suppressions`

| Method | API |
|---|---|
| `list(limit=None, before=None, reason=None, stream=None, address=None, domain=None)` | `GET /suppressions`: newest first, paged with `next_before` |
| `create(address, reason=None, detail=None, stream=None)` | `POST /suppressions`: `created` is `False` if it was already suppressed |
| `delete(address, stream=None)` | `DELETE /suppressions/{address}`: send to it again |
| `export(reason=None, stream=None, address=None, domain=None)` | `GET /suppressions/export`: the CSV text, a `str` |

`stream` is `all` (no mail at all, the default) or `broadcast` (only broadcast mail, as after a one-click unsubscribe).

### `pauses`

A circuit breaker pauses a key, a domain or the whole project when its bounce or complaint rate is too high; its sends are then refused with `403 sending_paused`.

| Method | API |
|---|---|
| `list(active=None, limit=None)` | `GET /pauses` |
| `resume(pause_id)` | `POST /pauses/{id}/resume` (full access) |

### `project`

| Method | API |
|---|---|
| `get()` | `GET /project` |
| `update(max_per_recipient_per_day=…, circuit_breaker=None)` | `PATCH /project` (full access): `max_per_recipient_per_day=None` turns the cap off; `circuit_breaker` takes `enabled`, `bounce_rate`, `complaint_rate`, `min_recipients` |

### `audit_log` (full-access keys only)

| Method | API |
|---|---|
| `list(limit=None, before=None, action=None)` | `GET /audit-log`: hash-chained entries, newest first |
| `verify()` | `GET /audit-log/verify`: checks the chain; keep `head_hash` to compare later |

### `insights` (full-access keys only)

`insights.get(window=None)` (`GET /insights`) answers acceptance, deferral and bounce rates and time to acceptance per mailbox provider and sending IP, for the windows `24h`, `7d` and `30d`. The server always computes all three; with `window`, the SDK keeps only that one in `windows`.

### Waiting

`wait_for_delivery(email_id, timeout=60.0, interval=1.0)` polls `emails.get` until no recipient is `queued`, `sending` or `deferred`, and returns the email. Each recipient then has a final status: `delivered`, `bounced`, `expired`, `suppressed`, `unknown` or `canceled`. Check each one. After `timeout` seconds it raises `WaitTimeoutError` (a `TimeoutError`), whose `email` is the last state seen.

`wait_for_reply(email_id, timeout=60.0, interval=2.0, inbox_id=None)` polls the inboxes until messages arrive whose `in_reply_to_email_id` is `email_id`, and returns them newest first. It returns `[]` if none arrived in time. It looks at the 50 newest messages of each inbox, or only of `inbox_id`.

```python
sent = omw.emails.send({"from": "agent@acme.com", "to": "client@example.org", "subject": "Quote", "text": "…"})
replies = omw.wait_for_reply(sent["id"], timeout=600)
if replies:
    print(omw.messages.get(replies[0]["id"])["text"])
```

## Errors

Every API error raises `OpenMailWireError`:

```python
from openmailwire import OpenMailWireError

try:
    omw.emails.send(params)
except OpenMailWireError as e:
    e.status_code  # 422
    e.name         # "validation_error", "domain_not_verified", "rate_limit_exceeded", …
    e.message      # names the cause and the next step
    e.retry_after  # seconds, from Retry-After (429 and 503), or None
    e.retryable    # True for 429 and 503: send it again after retry_after
```

`retry_after` is set for both 429 errors, `rate_limit_exceeded` (the key's caps, or too many requests per second) and `recipient_rate_limited` (the project's `max_per_recipient_per_day`), and for `503 server_busy`. `ErrorNames` holds the names to compare with, e.g. `ErrorNames.SENDING_PAUSED`, `INSUFFICIENT_PERMISSION`, `CANNOT_APPROVE_OWN_EMAIL`, `SERVER_BUSY`, `DOMAIN_VERIFIED_ELSEWHERE`, `IP_NOT_ALLOWED`, `API_KEY_EXPIRED` and `WEBHOOK_LIMIT_REACHED`:

```python
from openmailwire import ErrorNames, OpenMailWireError

try:
    omw.emails.send(params)
except OpenMailWireError as e:
    if e.name == ErrorNames.SENDING_PAUSED:
        print(omw.pauses.list(active=True))  # a circuit breaker stopped this key, domain or project
```

Network failures and timeouts raise `OpenMailWireError` too, with `status_code == 0` and `name == "connection_error"`.

The SDK does not retry by default. With `max_retries=n`, a request answered 429 or 503 is sent again up to `n` times, each after its `Retry-After` seconds (1 s without one), with the same body and idempotency key. A `Retry-After` longer than `max_retry_wait` (default 60 s) is raised at once.

## Verifying webhooks

`verify_webhook(secret, headers, raw_body, tolerance=300)` checks the signature and the timestamp, and returns the parsed event. It raises `WebhookVerificationError` if any check fails. `secret` is the webhook's `whsec_…` signing secret, or, for a webhook created with `asymmetric_signing`, its `whpk_…` public key (Ed25519 `v1a` signatures, verified in pure Python). During a secret rotation requests carry both signatures, so either secret verifies.

- Accepts both header families: `svix-*` (as Resend sends them) and `webhook-*` ([Standard Webhooks](https://www.standardwebhooks.com)), in any case.
- Accepts any one of several space-separated `v1,` signatures.
- Compares signatures in constant time (`hmac.compare_digest`).
- Refuses timestamps more than 5 minutes from now.

Pass the body exactly as received: re-serialised JSON will not match.

```python
from flask import Flask, abort, request
from openmailwire import WebhookVerificationError, verify_webhook

app = Flask(__name__)

@app.post("/webhooks/openmailwire")
def openmailwire_webhook():
    try:
        event = verify_webhook(SECRET, request.headers, request.get_data())
    except WebhookVerificationError:
        abort(401)
    if event["type"] == "email.bounced":
        pass  # event["data"]["to"] holds only the recipient that bounced
    return "", 204
```

Event types: `email.sent`, `email.delivered`, `email.delivery_delayed`, `email.bounced`, `email.failed`, `email.suppressed`, `email.delivery_unknown`, `email.canceled`, `email.complained`, `email.received`, `email.held`, `email.approved`, `email.rejected` and `sending.paused` (`openmailwire.types.WebhookEventType`; `WebhookEvent` types the body). `data["approval"]` carries the hold reasons or the decision and its note, `data["pause"]` the pause, `data["complaint"]` the feedback report. For `email.received`, `data["to"]` is the inbox address, `data["header_to"]` the message's own To header, and `trust`, `authentication`, `held` and `suspicious_instructions` describe the sender.

Django: `verify_webhook(SECRET, request.headers, request.body)`. FastAPI: `verify_webhook(SECRET, request.headers, await request.body())`.

## Development

```sh
python -m unittest discover -s tests -v   # runs against a local mock server
```

## Licence

MIT. See [LICENSE](LICENSE). The OpenMailWire server is AGPL-3.0; this client is not, so it can be embedded in software under any licence.
