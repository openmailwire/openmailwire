# OpenMailWire SDKs

Client libraries for the OpenMailWire HTTP API, in four languages. Each one covers the same API:

- **emails:** send (with an idempotency key, attachments, scheduled sends and a `broadcast` stream), get, list, cancel, batch, raw MIME; check a send without sending it; list held emails, approve and reject them; list an email's replies;
- **domains:** create, list, get, verify, delete; DKIM keys, two-step DKIM rotation, the Ed25519 signature on or off; DMARC and TLS-RPT reports;
- **webhooks:** create (optionally Ed25519-signed), list, get, delete, rotate the secret with an overlap;
- **API keys:** create, list, get, update, delete, with allowed IPs, an expiry and an approval policy (`require_approval`, `can_approve`, `inboxes`);
- **inboxes and messages:** create, list, get, update (`hold_unauthenticated`) and delete inboxes; list, read and reply to received messages (with the sender's `trust`, `held` and `suspicious_instructions`); release held messages; download attachments;
- **suppressions:** list with filters, add, remove, export as CSV;
- **pauses** (circuit breakers): list, resume;
- **project:** get and update the per-recipient cap and the circuit breaker;
- **audit log:** list, verify the hash chain;
- **insights:** acceptance, deferral and bounce rates per mailbox provider and sending IP, for 24 hours, 7 days and 30 days;
- **waiting:** wait until every recipient has a final status (a held recipient is not final); wait for a reply to a sent email;
- **webhook verification** for both the Svix (`svix-*`) and Standard Webhooks (`webhook-*`) headers, with several signatures (so secret rotations need no code change), a constant-time comparison and a timestamp tolerance. Pass the `whsec_…` secret for HMAC (`v1`) signatures, or the webhook's `whpk_…` public key for Ed25519 (`v1a`) signatures.
- **no redirects:** a 3xx answer is an error named `redirect_refused`, so the API key is never sent to another address.

| Language | Directory | Install | Needs |
|---|---|---|---|
| Node.js / TypeScript | [`node/`](node) | `npm install openmailwire` | Node 18+, no dependencies |
| Python | [`python/`](python) | `pip install openmailwire` | Python 3.9+, standard library only |
| Go | [`go/`](go) | `go get github.com/openmailwire/openmailwire/sdks/go` | Go 1.22+, standard library only |
| PHP | [`php/`](php) | `composer require openmailwire/openmailwire` | PHP 8.1+ with curl |

**Not published yet.** The npm, PyPI and Packagist packages do not exist yet, and the Go module has no version tag. Each SDK's README says how to install it from this repository in the meantime.

Every SDK reads `OMW_API_URL` (default `http://localhost:8080`) and `OMW_API_KEY` when they are not passed in, and raises a typed error carrying the API's `statusCode`, `name` and `message`, plus the `Retry-After` seconds of a 429 (`rate_limit_exceeded`, `recipient_rate_limited`) or a 503 (`server_busy`). Each one has constants for the error names and says whether an error may be retried. None retries by default; an option (`maxRetries`, `max_retries`, `Config.MaxRetries`) retries 429 and 503 answers after their `Retry-After`, with the same body and idempotency key.

| | Node | Python | Go | PHP |
|---|---|---|---|---|
| Error names | `ErrorNames.sendingPaused` | `ErrorNames.SENDING_PAUSED` | `ErrorNameSendingPaused`, `IsError` | `OpenMailWireException::SENDING_PAUSED` |
| Retryable | `err.retryable` | `e.retryable` | `apiErr.Retryable()` | `$e->isRetryable()` |
| Retry option | `maxRetries`, `maxRetryWaitMs` | `max_retries`, `max_retry_wait` | `MaxRetries`, `MaxRetryWait` | `maxRetries`, `maxRetryWait` |

## Licence

The SDKs are licensed under the **MIT licence**, not the AGPL-3.0 of the server, so that applications under any licence can embed them. Each SDK directory has its own `LICENSE` file. Everything outside `sdks/` remains AGPL-3.0.

## Tests

Each SDK is tested against a local mock HTTP server; no test reaches a real service. CI runs them in `.github/workflows/sdks.yml` whenever something under `sdks/` changes.

| SDK | Command (from its directory) |
|---|---|
| Node | `npm install && npm test` |
| Python | `python -m unittest discover -s tests -v` |
| Go | `go test -race ./...` |
| PHP | `composer install && vendor/bin/phpunit` |

## Publishing (maintainers)

The package metadata is ready; publishing needs the maintainers' accounts.

- **npm:** `cd sdks/node && npm publish` (`prepublishOnly` builds and tests first).
- **PyPI:** `cd sdks/python && python -m build && twine upload dist/*`.
- **Go:** tag the module as `sdks/go/vX.Y.Z`; the Go proxy picks it up.
- **Packagist** reads `composer.json` from the root of a repository, so the PHP SDK needs a read-only split repository (for example `openmailwire/openmailwire-php`, kept in sync with `git subtree split --prefix sdks/php`), which is then submitted to Packagist.
