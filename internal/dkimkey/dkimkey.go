// Package dkimkey creates per-domain DKIM keys and signs messages.
package dkimkey

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// Bits is the RSA key size. 1024-bit keys are still common but weak.
const Bits = 2048

// Algorithms of the keys a domain can have.
const (
	RSA     = "rsa"
	Ed25519 = "ed25519"
)

// Oversigned header fields are listed in the signature once more than they
// occur (RFC 6376 section 5.4.2, for fields that are absent: once), so a
// copy added in transit, such as a second From or Subject, breaks the
// signature instead of being shown to the reader.
var Oversigned = []string{"From", "To", "Subject", "Date", "Message-ID", "Reply-To", "Cc"}

// SignedHeaders are signed when present. From is required by RFC 6376.
var SignedHeaders = []string{
	"From", "Reply-To", "Subject", "Date", "To", "Cc", "Message-ID",
	"MIME-Version", "Content-Type", "Content-Transfer-Encoding",
	"List-Unsubscribe", "List-Unsubscribe-Post", "Feedback-ID",
}

// headerKeys is the h= list: every signed field, then each oversigned field
// once more.
func headerKeys() []string {
	return append(append([]string{}, SignedHeaders...), Oversigned...)
}

func Generate() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, Bits)
}

// GenerateEd25519 creates an Ed25519 key (RFC 8463).
func GenerateEd25519() (ed25519.PrivateKey, error) {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	return k, err
}

// MarshalPrivate encodes an RSA or Ed25519 key as PKCS#8.
func MarshalPrivate(k crypto.Signer) ([]byte, error) {
	return x509.MarshalPKCS8PrivateKey(k)
}

// ParsePrivate decodes an RSA key from PKCS#8.
func ParsePrivate(der []byte) (*rsa.PrivateKey, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("dkim: key is not RSA")
	}
	return rk, nil
}

// ParseSigner decodes an RSA or Ed25519 key from PKCS#8.
func ParseSigner(der []byte) (crypto.Signer, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	switch k := k.(type) {
	case *rsa.PrivateKey:
		return k, nil
	case ed25519.PrivateKey:
		return k, nil
	}
	return nil, errors.New("dkim: key is neither RSA nor Ed25519")
}

// PublicBase64 returns the p= value for the DNS record of an RSA key.
func PublicBase64(k *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// PublicBase64Ed25519 returns the p= value for an Ed25519 key: the raw 32
// bytes of the public key (RFC 8463 section 4.2).
func PublicBase64Ed25519(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

// TXTRecord is the value to publish at <selector>._domainkey.<domain>.
func TXTRecord(publicB64 string) string {
	return "v=DKIM1; k=rsa; p=" + publicB64
}

// TXTRecordFor is TXTRecord for a key of either algorithm.
func TXTRecordFor(algorithm, publicB64 string) string {
	if algorithm == Ed25519 {
		return "v=DKIM1; k=ed25519; p=" + publicB64
	}
	return TXTRecord(publicB64)
}

// Key is one signing key of a domain.
type Key struct {
	Selector string
	Signer   crypto.Signer // *rsa.PrivateKey or ed25519.PrivateKey
}

// Sign returns raw with a DKIM-Signature header prepended (relaxed/relaxed,
// rsa-sha256), oversigning the fields in Oversigned.
func Sign(raw []byte, domain, selector string, key *rsa.PrivateKey) ([]byte, error) {
	return SignAll(raw, domain, []Key{{Selector: selector, Signer: key}})
}

// SignAll signs raw with every key, one DKIM-Signature each (an RSA and an
// Ed25519 signature side by side, RFC 8463 section 6). The first key's
// signature ends up on top.
func SignAll(raw []byte, domain string, keys []Key) ([]byte, error) {
	if len(keys) == 0 {
		return nil, errors.New("dkim: no key")
	}
	out := raw
	for i := len(keys) - 1; i >= 0; i-- {
		var b bytes.Buffer
		err := dkim.Sign(&b, bytes.NewReader(out), &dkim.SignOptions{
			Domain:                 domain,
			Selector:               keys[i].Selector,
			Signer:                 keys[i].Signer,
			HeaderCanonicalization: dkim.CanonicalizationRelaxed,
			BodyCanonicalization:   dkim.CanonicalizationRelaxed,
			HeaderKeys:             headerKeys(),
		})
		if err != nil {
			return nil, err
		}
		out = b.Bytes()
	}
	return out, nil
}

// Redate prepares a signed message to be signed again: it drops the
// DKIM-Signature fields at the top of the header that domain added, and
// replaces the Date field (or adds one) with date. Everything else stays
// byte for byte, including signatures of other domains.
func Redate(raw []byte, domain string, date time.Time) ([]byte, error) {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		return nil, errors.New("dkim: message has no header/body separator")
	}
	fields := splitFields(raw[:end+2])
	var out bytes.Buffer
	out.Grow(len(raw) + 64)
	top, dated := true, false
	newDate := "Date: " + date.UTC().Format(time.RFC1123Z) + "\r\n"
	for _, f := range fields {
		name := fieldName(f)
		if top && strings.EqualFold(name, "DKIM-Signature") {
			if strings.EqualFold(sigDomain(f), domain) {
				continue
			}
		} else {
			top = false
		}
		if strings.EqualFold(name, "Date") {
			if !dated {
				out.WriteString(newDate)
				dated = true
			}
			continue
		}
		out.Write(f)
	}
	if !dated {
		out.WriteString(newDate)
	}
	out.Write(raw[end+2:])
	return out.Bytes(), nil
}

// Resign re-dates a signed message to date and signs it again with keys.
func Resign(raw []byte, domain string, keys []Key, date time.Time) ([]byte, error) {
	r, err := Redate(raw, domain, date)
	if err != nil {
		return nil, err
	}
	return SignAll(r, domain, keys)
}

// splitFields splits a header block (CRLF line endings, ending in CRLF) into
// fields, each with its continuation lines and final CRLF.
func splitFields(h []byte) [][]byte {
	var fields [][]byte
	start := 0
	for i := 0; i < len(h); {
		j := bytes.Index(h[i:], []byte("\r\n"))
		if j < 0 {
			break
		}
		next := i + j + 2
		if next < len(h) && (h[next] == ' ' || h[next] == '\t') {
			i = next // a continuation line
			continue
		}
		fields = append(fields, h[start:next])
		start, i = next, next
	}
	if start < len(h) {
		fields = append(fields, h[start:])
	}
	return fields
}

func fieldName(f []byte) string {
	if i := bytes.IndexByte(f, ':'); i >= 0 {
		return strings.TrimSpace(string(f[:i]))
	}
	return ""
}

// sigDomain is the d= tag of a DKIM-Signature field.
func sigDomain(f []byte) string {
	_, v, _ := strings.Cut(string(f), ":")
	for _, tag := range strings.Split(v, ";") {
		k, val, ok := strings.Cut(tag, "=")
		if ok && strings.TrimSpace(k) == "d" {
			return strings.Join(strings.Fields(val), "")
		}
	}
	return ""
}
