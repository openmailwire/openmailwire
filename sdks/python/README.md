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
)
```

The constructor raises `ValueError` if there is no API key. Requests use the API's own field names (`from`, `reply_to`, `scheduled_at`), and answers are plain `dict`s, exactly as the API sends them.

## Methods

### `emails`

| Method | API | Notes |
|---|---|---|
| `send(params, idempotency_key=None)` | `POST /emails` | Returns `{"id": …}`. |
| `get(email_id)` | `GET /emails/{id}` | Recipients with their status, every attempt, the event timeline. |
| `list(limit=None, before=None, api_key_id=None)` | `GET /emails` | Newest first. Pass `next_before` as `before` for the next page. |
| `cancel(email_id)` | `POST /emails/{id}/cancel` | Cancels recipients not tried yet; `409 cannot_cancel` if there are none. |
| `batch(emails, idempotency_key=None)` | `POST /emails/batch` | 1 to 100 emails, no attachments. Returns `{"data": [{"id": …}]}`. |
| `send_raw(raw, mail_from=None, rcpts=None, idempotency_key=None)` | `POST /emails/raw` | A complete MIME message, as `bytes` or `str`. |

`send` params: `from`, `to`, `cc`, `bcc`, `reply_to`, `subject`, `html`, `text`, `headers`, `tags`, `attachments`, `scheduled_at`, `expires_at`, `retry_uncertain`. Keys set to `None` are left out.

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

### `webhooks`

| Method | API |
|---|---|
| `create(endpoint, events=None)` | `POST /webhooks`: the answer has `signing_secret`, shown once |
| `list()` | `GET /webhooks` |
| `get(webhook_id)` | `GET /webhooks/{id}`: with its 20 most recent deliveries |
| `delete(webhook_id)` | `DELETE /webhooks/{id}` |

### `api_keys` (full-access keys only)

| Method | API |
|---|---|
| `create(name, permission=None, policy=None)` | `POST /api-keys`: the answer has `token`, shown once |
| `list()` | `GET /api-keys` |
| `get(key_id)` | `GET /api-keys/{id}` |
| `update(key_id, name=None, policy=None)` | `PATCH /api-keys/{id}`; `policy={}` removes every limit |
| `delete(key_id)` | `DELETE /api-keys/{id}`: revokes the key |

```python
key = omw.api_keys.create(
    "support-agent",
    permission="sending_access",
    policy={"max_per_hour": 100, "max_per_day": 1000, "allowed_recipients": ["*@acme.com"]},
)
print(key["token"])  # omw_…
```

### `inboxes` and `messages`

| Method | API |
|---|---|
| `inboxes.create(address)` | `POST /inboxes` |
| `inboxes.list()` | `GET /inboxes` |
| `inboxes.delete(inbox_id)` | `DELETE /inboxes/{id}` |
| `inboxes.list_messages(inbox_id, limit=None, before=None, thread_id=None)` | `GET /inboxes/{id}/messages` |
| `messages.get(message_id)` | `GET /messages/{id}` |
| `messages.get_attachment(message_id, index)` | `GET /messages/{id}/attachments/{index}`: `bytes` |
| `messages.reply(message_id, text=None, html=None)` | `POST /messages/{id}/reply`: returns `{"id": …}` |

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
    e.retry_after  # seconds, from Retry-After (429), or None
```

Network failures and timeouts raise `OpenMailWireError` too, with `status_code == 0` and `name == "connection_error"`. The SDK never retries by itself.

## Verifying webhooks

`verify_webhook(secret, headers, raw_body, tolerance=300)` checks the signature and the timestamp, and returns the parsed event. It raises `WebhookVerificationError` if any check fails.

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

Django: `verify_webhook(SECRET, request.headers, request.body)`. FastAPI: `verify_webhook(SECRET, request.headers, await request.body())`.

## Development

```sh
python -m unittest discover -s tests -v   # runs against a local mock server
```

## Licence

MIT. See [LICENSE](LICENSE). The OpenMailWire server is AGPL-3.0; this client is not, so it can be embedded in software under any licence.
