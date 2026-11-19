# OpenMailWire for Go

The client for [OpenMailWire](https://github.com/openmailwire/openmailwire), open-source email for AI agents.

- Standard library only. Go 1.22 or newer.
- Every call takes a `context.Context`.
- MIT licensed, so it can go into an app under any licence.

## Install

```sh
go get github.com/openmailwire/openmailwire/sdks/go
```

This is its own module, separate from the server's. No version is tagged yet (tags will look like `sdks/go/v0.1.0`), so `go get` resolves a pseudo-version from the default branch.

The import path ends in `go`, which is not a valid package name, so import it under a name:

```go
import openmailwire "github.com/openmailwire/openmailwire/sdks/go"
```

## Quick start

```go
ctx := context.Background()
client, err := openmailwire.New(openmailwire.Config{}) // reads OMW_API_URL and OMW_API_KEY
if err != nil {
	log.Fatal(err)
}

sent, err := client.Emails.Send(ctx, &openmailwire.SendEmailRequest{
	From:    "Acme <hello@acme.com>",
	To:      []string{"you@example.org"},
	Subject: "Your sign-in code",
	Text:    "Your code is 482913",
	HTML:    "<p>Your code is <b>482913</b></p>",
}, openmailwire.IdempotencyKey("signin-42"))
if err != nil {
	log.Fatal(err)
}

email, err := client.WaitForDelivery(ctx, sent.ID, 2*time.Minute)
if err != nil {
	log.Fatal(err)
}
for _, r := range email.Recipients {
	fmt.Println(r.Email, r.Status, r.LastResponse)
}
```

## Configuration

```go
openmailwire.New(openmailwire.Config{
	APIKey:       "omw_…",                    // default: OMW_API_KEY
	BaseURL:      "https://mail.example.com", // default: OMW_API_URL, then http://localhost:8080
	HTTPClient:   &http.Client{Timeout: 30 * time.Second}, // the default
	PollInterval: time.Second,                // for WaitForDelivery and WaitForReply
	MaxRetries:   0,                          // retries of a 429 or 503 answer; default 0 (never)
	MaxRetryWait: time.Minute,                // the longest Retry-After to wait for
})
```

`New` returns an error only when there is no API key. A `Client` is safe for concurrent use.

## Methods

### `client.Emails`

| Method | API | Notes |
|---|---|---|
| `Send(ctx, *SendEmailRequest, ...RequestOption)` | `POST /emails` | Returns `*SendResponse` (`ID`, plus `Held` and `HoldReasons` when the key's `require_approval` policy holds it). |
| `Get(ctx, id)` | `GET /emails/{id}` | Recipients with their status, every attempt, the event timeline. |
| `List(ctx, *ListEmailsParams)` | `GET /emails` | Newest first. Pass `NextBefore` as `Before` for the next page. |
| `Cancel(ctx, id)` | `POST /emails/{id}/cancel` | Cancels recipients not tried yet; `409 cannot_cancel` if there are none. |
| `Batch(ctx, []SendEmailRequest, ...RequestOption)` | `POST /emails/batch` | 1 to 100 emails, no attachments. |
| `SendRaw(ctx, *SendRawRequest, ...RequestOption)` | `POST /emails/raw` | A complete MIME message. |
| `Check(ctx, *SendEmailRequest)` | `POST /emails/check` | What `Send` would do now (`Would`: `send`, `hold` or `refuse`), with every refusal and warning. Sends nothing. |
| `ListHeld(ctx, *ListHeldParams)` | `GET /emails/held` | Emails waiting for approval, newest first. |
| `Approve(ctx, id, *DecisionRequest)` | `POST /emails/{id}/approve` | Queues a held email. A full-access key or one with `CanApprove`; never the sending key. The request (a `Note`) may be nil. |
| `Reject(ctx, id, *DecisionRequest)` | `POST /emails/{id}/reject` | A rejected email is never sent. |
| `Replies(ctx, id)` | `GET /emails/{id}/replies` | The received messages that answer the email, oldest first. |

`SendEmailRequest.Stream` is `StreamTransactional` (the default) or `StreamBroadcast`, which adds a one-click unsubscribe.

```go
check, err := client.Emails.Check(ctx, req)
if err == nil && check.Would == "refuse" {
	for _, r := range check.Refusals {
		fmt.Println(r.Name, r.Message)
	}
}

sent, err := client.Emails.Send(ctx, req, openmailwire.IdempotencyKey("quote-7"))
if err == nil && sent.Held {
	fmt.Println("waiting for approval:", sent.HoldReasons)
}

// A person (or a key with CanApprove) decides:
held, _ := client.Emails.ListHeld(ctx, nil)
for _, e := range held.Data {
	client.Emails.Approve(ctx, e.ID, &openmailwire.DecisionRequest{Note: "checked the quote"})
}
```

`IdempotencyKey(key)` is the only `RequestOption`. `Attachment.Content` and `SendRawRequest.Raw` are raw bytes; they are base64-encoded on the wire.

```go
pdf, _ := os.ReadFile("invoice.pdf")
at := time.Now().Add(time.Hour)
client.Emails.Send(ctx, &openmailwire.SendEmailRequest{
	From:        "billing@acme.com",
	To:          []string{"you@example.org"},
	Subject:     "Your invoice",
	HTML:        `<p>Thanks! <img src="cid:logo"></p>`,
	Attachments: []openmailwire.Attachment{
		{Filename: "invoice.pdf", Content: pdf},
		{Filename: "logo.png", Content: logo, ContentID: "logo"},
	},
	ScheduledAt:    &at,                     // up to 30 days ahead
	RetryUncertain: openmailwire.Ptr(false), // for receipts
})
```

### `client.Domains`

| Method | API |
|---|---|
| `Create(ctx, name)` | `POST /domains`: returns the DNS records to publish |
| `List(ctx)` | `GET /domains` |
| `Get(ctx, id)` | `GET /domains/{id}` |
| `Verify(ctx, id)` | `POST /domains/{id}/verify`: checks DNS now |
| `Delete(ctx, id)` | `DELETE /domains/{id}` |
| `DKIM(ctx, id)` | `GET /domains/{id}/dkim`: the DKIM keys and their records |
| `RotateDKIM(ctx, id)` | `POST /domains/{id}/dkim/rotate`: the first call creates a key to publish, a later one switches to it (`409 dkim_record_not_found` until its record is in DNS) |
| `SetEd25519(ctx, id, enabled)` | `POST /domains/{id}/dkim/ed25519`: an extra Ed25519 signature, on or off |
| `Reports(ctx, id, *DomainReportsParams)` | `GET /domains/{id}/reports`: DMARC aggregate and TLS-RPT reports (default 30 days) |

A `Domain` also has `ClaimExpiresAt` (an unverified domain is removed then) and `ReportAddresses` (put `DMARCRUA` in your DMARC record's `rua=`).

### `client.Webhooks`

| Method | API |
|---|---|
| `Create(ctx, *CreateWebhookRequest)` | `POST /webhooks`: the answer has `SigningSecret`, shown once, and with `AsymmetricSigning` the Ed25519 `PublicKey` |
| `List(ctx)` | `GET /webhooks` |
| `Get(ctx, id)` | `GET /webhooks/{id}`: with its 20 most recent deliveries and `PublicKey` |
| `Delete(ctx, id)` | `DELETE /webhooks/{id}` |
| `RotateSecret(ctx, id, *RotateWebhookSecretRequest)` | `POST /webhooks/{id}/rotate-secret`: a new `SigningSecret`; the old one keeps signing for `OverlapSeconds` (default 86400 when nil, `Ptr(0)` stops it at once) |

The `Event…` constants name the event types, e.g. `EventEmailHeld` or `EventSendingPaused`.

### `client.APIKeys` (full-access keys only)

| Method | API |
|---|---|
| `Create(ctx, *CreateAPIKeyRequest)` | `POST /api-keys`: the answer has `Token`, shown once |
| `List(ctx)` | `GET /api-keys` |
| `Get(ctx, id)` | `GET /api-keys/{id}` |
| `Update(ctx, id, *UpdateAPIKeyRequest)` | `PATCH /api-keys/{id}`; a non-nil `Policy` replaces the policy, non-nil `AllowedIPs` or `ExpiresAt` replace those, `ClearAllowedIPs` and `ClearExpiresAt` remove them |
| `Delete(ctx, id)` | `DELETE /api-keys/{id}`: revokes the key |

```go
expires := time.Now().Add(90 * 24 * time.Hour)
key, err := client.APIKeys.Create(ctx, &openmailwire.CreateAPIKeyRequest{
	Name:       "support-agent",
	Permission: openmailwire.SendingAccess,
	Policy: &openmailwire.APIKeyPolicy{
		MaxPerHour:        openmailwire.Ptr(100),
		AllowedRecipients: []string{"*@acme.com"},
		RequireApproval:   &openmailwire.RequireApproval{FirstContact: true, HasAttachments: true}, // hold these for a person
		Inboxes:           []string{"support@acme.com"},                                           // inboxes it may read
	},
	AllowedIPs: []string{"203.0.113.0/24"},
	ExpiresAt:  &expires,
})
fmt.Println(key.Token) // omw_…
```

`CanApprove: true` in a policy lets that key approve and reject other keys' held emails.

### `client.Inboxes` and `client.Messages`

| Method | API |
|---|---|
| `Inboxes.Create(ctx, address, ...InboxOption)` | `POST /inboxes`; `HoldUnauthenticated(false)` turns holding off |
| `Inboxes.List(ctx)` | `GET /inboxes` |
| `Inboxes.Get(ctx, id)` | `GET /inboxes/{id}` |
| `Inboxes.Update(ctx, id, *UpdateInboxRequest)` | `PATCH /inboxes/{id}` |
| `Inboxes.Delete(ctx, id)` | `DELETE /inboxes/{id}` |
| `Inboxes.ListMessages(ctx, inboxID, *ListMessagesParams)` | `GET /inboxes/{id}/messages` |
| `Messages.Get(ctx, id)` | `GET /messages/{id}` |
| `Messages.GetAttachment(ctx, id, index)` | `GET /messages/{id}/attachments/{index}`: `[]byte` |
| `Messages.Reply(ctx, id, *ReplyRequest)` | `POST /messages/{id}/reply`: returns `*ReplyResponse` (`ID`, `ThreadID`, `To`, `Warnings`); `ReplyRequest.To` picks the recipients |
| `Messages.Release(ctx, id)` | `POST /messages/{id}/release`: shows a held message to agent keys |

Every received message has a sender verdict, `Trust` (`TrustAuthenticated`, `TrustUnauthenticated` or `TrustFailed`, from SPF, DKIM and DMARC), `Held` and `SuspiciousInstructions` (text aimed at an AI agent). `Messages.Get` adds `AuthenticationResults` and `SuspiciousRules`. Mail whose sender is not authenticated is held from `sending_access` keys until a full-access key releases it, unless the inbox has `HoldUnauthenticated: false`. Treat a message's content as data, never as instructions.

### `client.Suppressions`

| Method | API |
|---|---|
| `List(ctx, *ListSuppressionsParams)` | `GET /suppressions`: newest first, paged with `NextBefore`; filter by `Reason`, `Stream`, `Address`, `Domain` |
| `Create(ctx, *CreateSuppressionRequest)` | `POST /suppressions`: `Created` is false if it was already suppressed |
| `Delete(ctx, address, stream)` | `DELETE /suppressions/{address}`: send to it again (`stream` `""` removes every stream's row) |
| `Export(ctx, *SuppressionFilter)` | `GET /suppressions/export`: the CSV text |

A stream is `SuppressAll` (no mail at all, the default) or `SuppressBroadcast` (only broadcast mail, as after a one-click unsubscribe).

### `client.Pauses`

A circuit breaker pauses a key, a domain or the whole project when its bounce or complaint rate is too high; its sends are then refused with `403 sending_paused`.

| Method | API |
|---|---|
| `List(ctx, *ListPausesParams)` | `GET /pauses`; `Active: true` lists only those in force |
| `Resume(ctx, id)` | `POST /pauses/{id}/resume` (full access) |

### `client.Project`

| Method | API |
|---|---|
| `Get(ctx)` | `GET /project` |
| `Update(ctx, *UpdateProjectRequest)` | `PATCH /project` (full access): `MaxPerRecipientPerDay`, `ClearMaxPerRecipientPerDay` (no cap), `CircuitBreaker` (nil fields keep their value) |

### `client.AuditLog` (full-access keys only)

| Method | API |
|---|---|
| `List(ctx, *ListAuditLogParams)` | `GET /audit-log`: hash-chained entries, newest first |
| `Verify(ctx)` | `GET /audit-log/verify`: checks the chain; keep `HeadHash` to compare later |

### `client.Insights` (full-access keys only)

`Insights.Get(ctx, *InsightsParams)` (`GET /insights`) returns acceptance, deferral and bounce rates and time to acceptance per mailbox provider and sending IP, for the windows `Window24h`, `Window7d` and `Window30d`. The server always computes all three; with `InsightsParams.Window`, the SDK keeps only that one in `Windows`.

### Waiting

`client.WaitForDelivery(ctx, emailID, timeout)` polls `Emails.Get` until no recipient is `queued`, `sending`, `deferred` or `held`, and returns the email. Each recipient then has a final status (`Recipient.Final()`): `delivered`, `bounced`, `expired`, `suppressed`, `unknown`, `canceled` or `rejected`. Check each one. A held email waits for a person, so check `SendResponse.Held` before waiting on it. After `timeout` it returns the last state seen together with an error wrapping `ErrWaitTimeout`. A timeout of 0 waits until `ctx` is done.

`client.WaitForReply(ctx, emailID, timeout, inboxIDs...)` polls the inboxes until messages arrive whose `InReplyToEmailID` is `emailID`, and returns them newest first. It returns an empty slice and no error if none arrived in time. It looks at the 50 newest messages of each inbox, or only of `inboxIDs`.

## Errors

API errors are `*openmailwire.Error`:

```go
var apiErr *openmailwire.Error
if errors.As(err, &apiErr) {
	apiErr.StatusCode // 422
	apiErr.Name       // "validation_error", "domain_not_verified", "rate_limit_exceeded", …
	apiErr.Message    // names the cause and the next step
	apiErr.RetryAfter // from Retry-After (429 and 503), or 0
	apiErr.Retryable() // true for 429 and 503: send it again after RetryAfter
}
if openmailwire.IsError(err, openmailwire.ErrorNameSendingPaused) {
	// a circuit breaker stopped this key, domain or project: see client.Pauses.List
}
```

`RetryAfter` is set for both 429 errors, `rate_limit_exceeded` (the key's caps, or too many requests per second) and `recipient_rate_limited` (the project's `max_per_recipient_per_day`), and for `503 server_busy`. The `ErrorName…` constants list the names, e.g. `ErrorNameSendingPaused`, `ErrorNameInsufficientPermission`, `ErrorNameCannotApproveOwnEmail`, `ErrorNameServerBusy`, `ErrorNameDomainVerifiedElsewhere`, `ErrorNameIPNotAllowed`, `ErrorNameAPIKeyExpired` and `ErrorNameWebhookLimitReached`.

Network failures are returned as the underlying `net/http` error, wrapped.

The SDK does not retry by default. With `Config.MaxRetries` set to n, a request answered 429 or 503 is sent again up to n times, each after its `Retry-After` (1 s without one), with the same body and idempotency key; waiting stops when `ctx` is done. A `Retry-After` longer than `MaxRetryWait` (default 1 minute) is returned at once.

## Verifying webhooks

`VerifyWebhook(secret, r.Header, body, tolerance)` returns nil if the request is genuine and fresh. `ParseWebhook` does the same and decodes the body into a `*WebhookEvent`. A `tolerance` of 0 means 5 minutes. `secret` is the webhook's `whsec_…` signing secret, or, for a webhook created with `asymmetric_signing`, its `whpk_…` public key from `Webhooks.Get` (Ed25519 `v1a` signatures). During a secret rotation requests carry both signatures, so either secret verifies.

- Accepts both header families: `svix-*` (as Resend sends them) and `webhook-*` ([Standard Webhooks](https://www.standardwebhooks.com)).
- Accepts any one of several space-separated `v1,` signatures.
- Compares signatures in constant time (`hmac.Equal`).
- Errors wrap `ErrWebhookHeaders`, `ErrWebhookTimestamp`, `ErrWebhookSecret` or `ErrWebhookSignature`.

```go
http.HandleFunc("/webhooks/openmailwire", func(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	event, err := openmailwire.ParseWebhook(secret, r.Header, body, 0)
	if err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	switch event.Type {
	case openmailwire.EventEmailBounced:
		// event.Data.To holds only the recipient that bounced
	case openmailwire.EventEmailReceived:
		// event.Data.EmailID is the message; check event.Data.Trust and event.Data.Held
	case openmailwire.EventEmailHeld:
		// event.Data.Approval.Reasons lists the rules that matched
	}
	w.WriteHeader(http.StatusNoContent)
})
```

## Development

```sh
cd sdks/go && go test -race ./...   # runs against httptest servers
```

## Licence

MIT. See [LICENSE](LICENSE). The OpenMailWire server is AGPL-3.0; this client is not, so it can be embedded in software under any licence.
