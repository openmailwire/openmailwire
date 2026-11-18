# Contributing to OpenMailWire

Thanks for helping. Small, focused pull requests are easiest to review.

## Setup

```sh
go build ./...
go vet ./...
go test ./...                     # unit tests, no database needed
```

The end-to-end test runs the API, a worker, PostgreSQL and a scripted SMTP server together. **It empties the OpenMailWire tables in the database you give it**, so use a throwaway one:

```sh
docker run -d --name openmailwire-test-pg -p 127.0.0.1:55433:5432 \
  -e POSTGRES_USER=openmailwire -e POSTGRES_PASSWORD=openmailwire -e POSTGRES_DB=openmailwire_test postgres:16-alpine
OMW_TEST_DATABASE_URL='postgres://openmailwire:openmailwire@127.0.0.1:55433/openmailwire_test?sslmode=disable' \
  go test -race ./...
```

## Guidelines

- Keep dependencies few. Today there are two: `pgx` and `go-msgauth`.
- Schema changes go in a **new** file in `internal/store/migrations/`. Never edit an applied migration.
- Delivery behaviour is part of the contract. If you change how an SMTP reply maps to an outcome or a retry, update `internal/policy` and its tests, and `docs/architecture.md`.
- Never log message bodies or full recipient addresses. Log the recipient's domain instead.
- Use `internal/smtptest` for anything that talks SMTP in tests. Tests must never send real email.

## Reporting bugs

Include the `GET /emails/{id}` output (redact addresses) and the worker's log lines for that attempt ID.
