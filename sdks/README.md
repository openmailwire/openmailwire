# OpenMailWire SDKs

Client libraries for the OpenMailWire HTTP API, in four languages. Each one covers the same API:

- **emails:** send (with an idempotency key, attachments and scheduled sends), get, list, cancel, batch, raw MIME;
- **domains:** create, list, get, verify, delete;
- **webhooks:** create, list, get, delete;
- **API keys:** create, list, get, update, delete;
- **inboxes and messages:** create, list and delete inboxes; list, read and reply to received messages; download attachments;
- **waiting:** wait until every recipient has a final status; wait for a reply to a sent email;
- **webhook verification** for both the Svix (`svix-*`) and Standard Webhooks (`webhook-*`) headers, with several signatures, a constant-time comparison and a timestamp tolerance.

| Language | Directory | Install | Needs |
|---|---|---|---|
| Node.js / TypeScript | [`node/`](node) | `npm install openmailwire` | Node 18+, no dependencies |
| Python | [`python/`](python) | `pip install openmailwire` | Python 3.9+, standard library only |
| Go | [`go/`](go) | `go get github.com/openmailwire/openmailwire/sdks/go` | Go 1.22+, standard library only |
| PHP | [`php/`](php) | `composer require openmailwire/openmailwire` | PHP 8.1+ with curl |

**Not published yet.** The npm, PyPI and Packagist packages do not exist yet, and the Go module has no version tag. Each SDK's README says how to install it from this repository in the meantime.

Every SDK reads `OMW_API_URL` (default `http://localhost:8080`) and `OMW_API_KEY` when they are not passed in, and raises a typed error carrying the API's `statusCode`, `name` and `message`, plus the `Retry-After` seconds of a 429.

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
