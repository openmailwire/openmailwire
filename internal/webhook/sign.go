package webhook

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// NewSecret returns a signing secret in the "whsec_<base64>" form.
func NewSecret() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "whsec_" + base64.StdEncoding.EncodeToString(b)
}

func secretKey(whsec string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimPrefix(whsec, "whsec_"))
}

// Sign returns the "v1,<base64>" signature of body for message id and unix
// timestamp ts.
func Sign(whsec, id string, ts int64, body []byte) (string, error) {
	key, err := secretKey(whsec)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s.%d.", id, ts)
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil)), nil
}

// NewEd25519Key returns a new signing key: the 32-byte seed (kept sealed)
// and the public key.
func NewEd25519Key() (seed, public []byte) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err) // crypto/rand never fails
	}
	return priv.Seed(), pub
}

// SignEd25519 returns the Standard Webhooks asymmetric signature,
// "v1a,<base64>", of "<id>.<ts>.<body>".
func SignEd25519(seed []byte, id string, ts int64, body []byte) (string, error) {
	if len(seed) != ed25519.SeedSize {
		return "", errors.New("webhook: ed25519 seed must be 32 bytes")
	}
	msg := make([]byte, 0, len(id)+24+len(body))
	msg = fmt.Appendf(msg, "%s.%d.", id, ts)
	msg = append(msg, body...)
	return "v1a," + base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), msg)), nil
}

// PublicKeyString renders an Ed25519 public key as Standard Webhooks
// does: "whpk_<base64>".
func PublicKeyString(pub []byte) string {
	if len(pub) == 0 {
		return ""
	}
	return "whpk_" + base64.StdEncoding.EncodeToString(pub)
}
