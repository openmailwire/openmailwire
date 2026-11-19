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
})
```

`New` returns an error only when there is no API key. A `Client` is safe for concurrent use.

## Methods

### `client.Emails`

| Method | API | Notes |
|---|---|---|
| `Send(ctx, *SendEmailRequest, ...RequestOption)` | `POST /emails` | Returns `*SendResponse` (`ID`). |
| `Get(ctx, id)` | `GET /emails/{id}` | Recipients with their status, every attempt, the event timeline. |
| `List(ctx, *ListEmailsParams)` | `GET /emails` | Newest first. Pass `NextBefore` as `Before` for the next page. |
| `Cancel(ctx, id)` | `POST /emails/{id}/cancel` | Cancels recipients not tried yet; `409 cannot_cancel` if there are none. |
| `Batch(ctx, []SendEmailRequest, ...RequestOption)` | `POST /emails/batch` | 1 to 100 emails, no attachments. |
| `SendRaw(ctx, *SendRawRequest, ...RequestOption)` | `POST /emails/raw` | A complete MIME message. |

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

### `client.Webhooks`

| Method | API |
|---|---|
| `Create(ctx, *CreateWebhookRequest)` | `POST /webhooks`: the answer has `SigningSecret`, shown once |
| `List(ctx)` | `GET /webhooks` |
| `Get(ctx, id)` | `GET /webhooks/{id}`: with its 20 most recent deliveries |
| `Delete(ctx, id)` | `DELETE /webhooks/{id}` |

### `client.APIKeys` (full-access keys only)

| Method | API |
|---|---|
| `Create(ctx, *CreateAPIKeyRequest)` | `POST /api-keys`: the answer has `Token`, shown once |
| `List(ctx)` | `GET /api-keys` |
| `Get(ctx, id)` | `GET /api-keys/{id}` |
| `Update(ctx, id, *UpdateAPIKeyRequest)` | `PATCH /api-keys/{id}`; a non-nil `Policy` replaces the policy |
| `Delete(ctx, id)` | `DELETE /api-keys/{id}`: revokes the key |

```go
key, err := client.APIKeys.Create(ctx, &openmailwire.CreateAPIKeyRequest{
	Name:       "support-agent",
	Permission: openmailwire.SendingAccess,
	Policy: &openmailwire.APIKeyPolicy{
		MaxPerHour:        openmailwire.Ptr(100),
		AllowedRecipients: []string{"*@acme.com"},
	},
})
fmt.Println(key.Token) // omw_…
```

### `client.Inboxes` and `client.Messages`

| Method | API |
|---|---|
| `Inboxes.Create(ctx, address)` | `POST /inboxes` |
| `Inboxes.List(ctx)` | `GET /inboxes` |
| `Inboxes.Delete(ctx, id)` | `DELETE /inboxes/{id}` |
| `Inboxes.ListMessages(ctx, inboxID, *ListMessagesParams)` | `GET /inboxes/{id}/messages` |
| `Messages.Get(ctx, id)` | `GET /messages/{id}` |
| `Messages.GetAttachment(ctx, id, index)` | `GET /messages/{id}/attachments/{index}`: `[]byte` |
| `Messages.Reply(ctx, id, *ReplyRequest)` | `POST /messages/{id}/reply`: returns the queued email's ID |

### Waiting

`client.WaitForDelivery(ctx, emailID, timeout)` polls `Emails.Get` until no recipient is `queued`, `sending` or `deferred`, and returns the email. Each recipient then has a final status (`Recipient.Final()`): `delivered`, `bounced`, `expired`, `suppressed`, `unknown` or `canceled`. Check each one. After `timeout` it returns the last state seen together with an error wrapping `ErrWaitTimeout`. A timeout of 0 waits until `ctx` is done.

`client.WaitForReply(ctx, emailID, timeout, inboxIDs...)` polls the inboxes until messages arrive whose `InReplyToEmailID` is `emailID`, and returns them newest first. It returns an empty slice and no error if none arrived in time. It looks at the 50 newest messages of each inbox, or only of `inboxIDs`.

## Errors

API errors are `*openmailwire.Error`:

```go
var apiErr *openmailwire.Error
if errors.As(err, &apiErr) {
	apiErr.StatusCode // 422
	apiErr.Name       // "validation_error", "domain_not_verified", "rate_limit_exceeded", …
	apiErr.Message    // names the cause and the next step
	apiErr.RetryAfter // from Retry-After (429), or 0
}
```

Network failures are returned as the underlying `net/http` error, wrapped. The SDK never retries by itself.

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
	if event.Type == "email.bounced" {
		// event.Data.To holds only the recipient that bounced
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
