// Package secret seals data at rest, mints API tokens and makes IDs.
package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// Sealed values come in two formats:
//
//   - v1 (since v0.6): "omw\x01" || key id (4 bytes) || nonce (12) || ciphertext.
//     The first 8 bytes are authenticated as additional data, so a value
//     cannot be relabelled with another key id.
//   - legacy (v0.5 and earlier): nonce (12) || ciphertext, under the one key
//     there was.
//
// Open reads both, so values sealed before an upgrade stay readable;
// `openmailwire rotate-secrets` rewrites them in the v1 format.
var magic = []byte{'o', 'm', 'w', 1}

const (
	headerLen = 8 // magic + key id
	idLen     = 4
)

type sealKey struct {
	id   [idLen]byte
	aead cipher.AEAD
}

// Sealer encrypts with AES-256-GCM under a keyring. The first key seals new
// values; every key opens.
type Sealer struct{ keys []sealKey }

// NewSealer returns a Sealer with a single key.
func NewSealer(key []byte) (*Sealer, error) { return NewKeyring([][]byte{key}) }

// NewKeyring returns a Sealer that seals with keys[0] and opens values
// sealed with any of keys.
func NewKeyring(keys [][]byte) (*Sealer, error) {
	if len(keys) == 0 {
		return nil, errors.New("secret: no key")
	}
	s := &Sealer{}
	seen := map[[idLen]byte]bool{}
	for _, key := range keys {
		if len(key) != 32 {
			return nil, errors.New("secret: key must be 32 bytes")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		var id [idLen]byte
		copy(id[:], keyID(key))
		if seen[id] {
			return nil, fmt.Errorf("secret: key %x is listed twice", id)
		}
		seen[id] = true
		s.keys = append(s.keys, sealKey{id: id, aead: aead})
	}
	return s, nil
}

func keyID(key []byte) []byte { return Derive(key, "openmailwire/key-id")[:idLen] }

// KeyID returns the short public id of a key, as stored in sealed values
// and shown in logs: 8 hex characters.
func KeyID(key []byte) string { return hex.EncodeToString(keyID(key)) }

// KeyCheck returns a value that proves a key without revealing it. The
// control plane stores it and compares it at start.
func KeyCheck(key []byte) []byte { return Derive(key, "openmailwire/key-check") }

// PrimaryID is the id of the key that seals new values.
func (s *Sealer) PrimaryID() string { return hex.EncodeToString(s.keys[0].id[:]) }

// IDs lists the ids of every key, the sealing key first.
func (s *Sealer) IDs() []string {
	out := make([]string, len(s.keys))
	for i, k := range s.keys {
		out[i] = hex.EncodeToString(k.id[:])
	}
	return out
}

// PrimaryHeader is the prefix every value sealed by this Sealer starts
// with. Values without it need resealing after a rotation.
func (s *Sealer) PrimaryHeader() []byte {
	return append(append([]byte{}, magic...), s.keys[0].id[:]...)
}

func (s *Sealer) Seal(plain []byte) []byte {
	k := s.keys[0]
	out := make([]byte, headerLen+k.aead.NonceSize(), headerLen+k.aead.NonceSize()+len(plain)+k.aead.Overhead())
	copy(out, magic)
	copy(out[len(magic):], k.id[:])
	nonce := out[headerLen:]
	rand.Read(nonce)
	return k.aead.Seal(out, nonce, plain, out[:headerLen])
}

// Open decrypts a value sealed with any key of the ring, in either format.
func (s *Sealer) Open(sealed []byte) ([]byte, error) {
	if id, ok := v1ID(sealed); ok {
		for _, k := range s.keys {
			if k.id != id {
				continue
			}
			n := k.aead.NonceSize()
			if len(sealed) >= headerLen+n {
				if plain, err := k.aead.Open(nil, sealed[headerLen:headerLen+n], sealed[headerLen+n:], sealed[:headerLen]); err == nil {
					return plain, nil
				}
			}
		}
		// A legacy value whose random nonce happens to start with the
		// magic bytes is still tried below.
		if plain, err := s.openLegacy(sealed); err == nil {
			return plain, nil
		}
		if !s.has(id) {
			return nil, fmt.Errorf("secret: value is sealed with key %x, which is not configured; add that key to OMW_SECRET_KEYS", id)
		}
		return nil, errors.New("secret: sealed value is corrupt")
	}
	plain, err := s.openLegacy(sealed)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

func (s *Sealer) openLegacy(sealed []byte) ([]byte, error) {
	for _, k := range s.keys {
		n := k.aead.NonceSize()
		if len(sealed) < n {
			return nil, errors.New("secret: sealed value too short")
		}
		if plain, err := k.aead.Open(nil, sealed[:n], sealed[n:], nil); err == nil {
			return plain, nil
		}
	}
	return nil, errors.New("secret: no configured key opens this value (is OMW_SECRET_KEYS missing the key it was sealed with?)")
}

func (s *Sealer) has(id [idLen]byte) bool {
	for _, k := range s.keys {
		if k.id == id {
			return true
		}
	}
	return false
}

func v1ID(sealed []byte) ([idLen]byte, bool) {
	var id [idLen]byte
	if len(sealed) < headerLen || !bytes.Equal(sealed[:len(magic)], magic) {
		return id, false
	}
	copy(id[:], sealed[len(magic):headerLen])
	return id, true
}

// OpenedBy returns the id of the key that opens sealed, or "" when none
// does.
func (s *Sealer) OpenedBy(sealed []byte) string {
	for _, k := range s.keys {
		one := &Sealer{keys: []sealKey{k}}
		if _, err := one.Open(sealed); err == nil {
			return hex.EncodeToString(k.id[:])
		}
	}
	return ""
}

// NeedsReseal reports whether sealed is not yet in the v1 format under the
// sealing key.
func (s *Sealer) NeedsReseal(sealed []byte) bool {
	return !bytes.HasPrefix(sealed, s.PrimaryHeader())
}

// Reseal opens sealed with any key and seals it again with the sealing key.
func (s *Sealer) Reseal(sealed []byte) ([]byte, error) {
	plain, err := s.Open(sealed)
	if err != nil {
		return nil, err
	}
	return s.Seal(plain), nil
}

// NewKey returns a fresh base64 key for OMW_SECRET_KEY.
func NewKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// NewToken returns a random token with the given prefix and its hash. Only
// the hash is stored.
func NewToken(prefix string) (token string, hash []byte) {
	b := make([]byte, 24)
	rand.Read(b)
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, Hash(token)
}

func Hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Derive returns a subkey of key for one purpose, so one secret never serves
// two algorithms.
func Derive(key []byte, purpose string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(purpose))
	return m.Sum(nil)
}

// MAC returns a short hex HMAC of msg, used to sign bounce addresses.
func MAC(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))[:10]
}

// UUID returns a random version 4 UUID.
func UUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// RandomHex returns n random bytes as hex.
func RandomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
