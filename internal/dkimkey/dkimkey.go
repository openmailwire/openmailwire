// Package dkimkey creates per-domain DKIM keys and signs messages.
package dkimkey

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"

	"github.com/emersion/go-msgauth/dkim"
)

// Bits is the RSA key size. 1024-bit keys are still common but weak.
const Bits = 2048

// SignedHeaders are signed when present. From is required by RFC 6376.
var SignedHeaders = []string{
	"From", "Reply-To", "Subject", "Date", "To", "Cc", "Message-ID",
	"MIME-Version", "Content-Type", "Content-Transfer-Encoding",
	"List-Unsubscribe", "List-Unsubscribe-Post", "Feedback-ID",
}

func Generate() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, Bits)
}

func MarshalPrivate(k *rsa.PrivateKey) ([]byte, error) {
	return x509.MarshalPKCS8PrivateKey(k)
}

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

// PublicBase64 returns the p= value for the DNS record.
func PublicBase64(k *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// TXTRecord is the value to publish at <selector>._domainkey.<domain>.
func TXTRecord(publicB64 string) string {
	return "v=DKIM1; k=rsa; p=" + publicB64
}

// Sign returns raw with a DKIM-Signature header prepended (relaxed/relaxed,
// rsa-sha256).
func Sign(raw []byte, domain, selector string, key *rsa.PrivateKey) ([]byte, error) {
	var out bytes.Buffer
	err := dkim.Sign(&out, bytes.NewReader(raw), &dkim.SignOptions{
		Domain:                 domain,
		Selector:               selector,
		Signer:                 key,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys:             SignedHeaders,
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
