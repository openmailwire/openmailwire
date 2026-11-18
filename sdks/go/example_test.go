package openmailwire_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	openmailwire "github.com/openmailwire/openmailwire/sdks/go"
)

func Example() {
	ctx := context.Background()
	client, err := openmailwire.New(openmailwire.Config{}) // OMW_API_URL, OMW_API_KEY
	if err != nil {
		log.Fatal(err)
	}
	sent, err := client.Emails.Send(ctx, &openmailwire.SendEmailRequest{
		From:    "Acme <hello@acme.com>",
		To:      []string{"you@example.org"},
		Subject: "Your sign-in code",
		Text:    "Your code is 482913",
	}, openmailwire.IdempotencyKey("signin-42"))
	var apiErr *openmailwire.Error
	if errors.As(err, &apiErr) {
		log.Fatalf("%s: %s", apiErr.Name, apiErr.Message) // e.g. domain_not_verified: …
	} else if err != nil {
		log.Fatal(err)
	}
	email, err := client.WaitForDelivery(ctx, sent.ID, 2*time.Minute)
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range email.Recipients {
		fmt.Println(r.Email, r.Status, r.LastResponse)
	}
}

func ExampleVerifyWebhook() {
	secret := "whsec_…" // from Webhooks.Create
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
		log.Printf("%s for %s", event.Type, event.Data.EmailID)
		w.WriteHeader(http.StatusNoContent)
	})
}
