package dkimkey

import (
	"bytes"
	"errors"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

func TestSignVerifies(t *testing.T) {
	key, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	der, err := MarshalPrivate(key)
	if err != nil {
		t.Fatal(err)
	}
	key2, err := ParsePrivate(der)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := PublicBase64(key)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: a@acme.test\r\nTo: b@example.test\r\nSubject: hi\r\nMessage-ID: <1@acme.test>\r\n\r\nhello\r\n")
	signed, err := Sign(raw, "acme.test", "pk1", key2)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			if domain == "pk1._domainkey.acme.test" {
				return []string{TXTRecord(pub)}, nil
			}
			return nil, errors.New("no record")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Err != nil || vs[0].Domain != "acme.test" {
		t.Fatalf("verification: %+v", vs)
	}
}
