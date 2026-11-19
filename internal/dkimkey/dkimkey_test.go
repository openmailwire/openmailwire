package dkimkey

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

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

func lookupFor(records map[string]string) func(string) ([]string, error) {
	return func(domain string) ([]string, error) {
		if r, ok := records[domain]; ok {
			return []string{r}, nil
		}
		return nil, errors.New("no record")
	}
}

// TestOversigning: a second From (or Subject, To...) added after signing
// breaks the signature instead of being shown to the reader.
func TestOversigning(t *testing.T) {
	key, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := PublicBase64(key)
	raw := []byte("From: a@acme.test\r\nTo: b@example.test\r\nSubject: hi\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\nMessage-ID: <1@acme.test>\r\n\r\nhello\r\n")
	signed, err := Sign(raw, "acme.test", "s1", key)
	if err != nil {
		t.Fatal(err)
	}
	lookup := lookupFor(map[string]string{"s1._domainkey.acme.test": TXTRecord(pub)})
	sig := string(signed[:bytes.Index(signed, []byte("\r\n\r\n"))])
	for _, h := range Oversigned {
		if strings.Count(strings.ToLower(sig), strings.ToLower(h)+":") < 1 {
			t.Errorf("%s is not oversigned: %s", h, sig)
		}
	}
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{LookupTXT: lookup})
	if err != nil || len(vs) != 1 || vs[0].Err != nil {
		t.Fatalf("clean message: %v %+v", err, vs)
	}
	for _, added := range []string{"From: ceo@acme.test\r\n", "Subject: urgent\r\n", "Reply-To: x@evil.test\r\n", "Cc: x@evil.test\r\n"} {
		// Prepended right after the signature, where a forger would put it.
		i := bytes.Index(signed, []byte("\r\nFrom: a@acme.test"))
		tampered := append(append(append([]byte{}, signed[:i+2]...), added...), signed[i+2:]...)
		vs, err := dkim.VerifyWithOptions(bytes.NewReader(tampered), &dkim.VerifyOptions{LookupTXT: lookup})
		if err == nil && len(vs) == 1 && vs[0].Err == nil {
			t.Errorf("adding %q kept the signature valid", strings.TrimSpace(added))
		}
	}
}

func TestEd25519NextToRSA(t *testing.T) {
	rsaKey, _ := Generate()
	edKey, err := GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	der, _ := MarshalPrivate(edKey)
	signer, err := ParseSigner(der)
	if err != nil {
		t.Fatal(err)
	}
	rsaPub, _ := PublicBase64(rsaKey)
	raw := []byte("From: a@acme.test\r\nTo: b@example.test\r\nSubject: hi\r\n\r\nhello\r\n")
	signed, err := SignAll(raw, "acme.test", []Key{{"r1", rsaKey}, {"e1", signer}})
	if err != nil {
		t.Fatal(err)
	}
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{LookupTXT: lookupFor(map[string]string{
		"r1._domainkey.acme.test": TXTRecordFor(RSA, rsaPub),
		"e1._domainkey.acme.test": TXTRecordFor(Ed25519, PublicBase64Ed25519(edKey)),
	})})
	if err != nil || len(vs) != 2 {
		t.Fatalf("%v %+v", err, vs)
	}
	for _, v := range vs {
		if v.Err != nil {
			t.Fatalf("signature %+v", v)
		}
	}
	if !strings.HasPrefix(string(signed), "DKIM-Signature: a=rsa-sha256") && !strings.Contains(string(signed[:200]), "rsa-sha256") {
		t.Fatalf("the RSA signature should be on top: %s", signed[:200])
	}
}

// TestResign covers R3#3: a scheduled message gets a new Date and signature
// when it is sent, and nothing else changes, not even another domain's
// signature.
func TestResign(t *testing.T) {
	key, _ := Generate()
	pub, _ := PublicBase64(key)
	other, _ := Generate()
	raw := []byte("From: a@acme.test\r\nTo: b@example.test\r\nSubject: hi\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\n" +
		"Message-ID: <1@acme.test>\r\nX-Long: one\r\n two\r\n\r\nhello\r\n.dots\r\n")
	upstream, _ := Sign(raw, "list.test", "u1", other) // a signature of another domain, kept
	signed, err := Sign(upstream, "acme.test", "s1", key)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 12, 1, 9, 30, 0, 0, time.UTC)
	out, err := Resign(signed, "acme.test", []Key{{"s1", key}}, when)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(out), &dkim.VerifyOptions{LookupTXT: lookupFor(map[string]string{
		"s1._domainkey.acme.test": TXTRecord(pub)})})
	if err != nil {
		t.Fatal(err)
	}
	ours := 0
	for _, v := range vs {
		if v.Domain == "acme.test" {
			ours++
			if v.Err != nil {
				t.Fatalf("new signature: %v", v.Err)
			}
		}
	}
	if ours != 1 || len(vs) != 2 {
		t.Fatalf("want one acme.test and one list.test signature, got %+v", vs)
	}
	if !bytes.Contains(out, []byte("Date: Tue, 01 Dec 2026 09:30:00 +0000\r\n")) || bytes.Contains(out, []byte("2006")) {
		t.Fatalf("date not replaced:\n%s", out)
	}
	// Without the Date and acme.test's signature, the message is byte for
	// byte what it was, the other domain's signature included.
	before := string(signed[bytes.Index(signed, []byte("\r\nDKIM-Signature: "))+2:]) // the upstream signature and everything after
	after := string(out[bytes.Index(out, []byte("\r\nDKIM-Signature: "))+2:])
	before = strings.Replace(before, "Date: Mon, 02 Jan 2006 15:04:05 +0000\r\n", "", 1)
	after = strings.Replace(after, "Date: Tue, 01 Dec 2026 09:30:00 +0000\r\n", "", 1)
	if before != after {
		t.Fatalf("the message changed beyond its Date and signature:\n%q\n%q", before, after)
	}
}
