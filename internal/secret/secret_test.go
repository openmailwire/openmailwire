package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// legacySeal seals the way v0.5 did: nonce || ciphertext, no key id.
func legacySeal(t *testing.T, k, plain []byte) []byte {
	t.Helper()
	block, _ := aes.NewCipher(k)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, nil)
}

func TestSealCarriesKeyID(t *testing.T) {
	s, err := NewKeyring([][]byte{key('a'), key('b')})
	if err != nil {
		t.Fatal(err)
	}
	sealed := s.Seal([]byte("dkim private key"))
	if !bytes.HasPrefix(sealed, s.PrimaryHeader()) || s.PrimaryID() != KeyID(key('a')) || len(s.PrimaryID()) != 8 {
		t.Fatalf("sealed value does not start with the sealing key's header: %x", sealed[:8])
	}
	if got, err := s.Open(sealed); err != nil || string(got) != "dkim private key" {
		t.Fatalf("open: %q %v", got, err)
	}
	if s.NeedsReseal(sealed) {
		t.Fatal("a value sealed with the sealing key needs no reseal")
	}
	if s.OpenedBy(sealed) != KeyID(key('a')) {
		t.Fatal("OpenedBy")
	}
}

func TestRotationKeepsOldValuesReadable(t *testing.T) {
	old, _ := NewSealer(key('a'))
	v1 := old.Seal([]byte("sealed with a"))
	legacy := legacySeal(t, key('a'), []byte("sealed by v0.5"))

	// The new key first, the old one after it.
	ring, err := NewKeyring([][]byte{key('b'), key('a')})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range [][]byte{v1, legacy} {
		if _, err := ring.Open(v); err != nil {
			t.Fatalf("the ring cannot open an old value: %v", err)
		}
		if !ring.NeedsReseal(v) {
			t.Fatal("an old value needs resealing")
		}
		re, err := ring.Reseal(v)
		if err != nil || ring.NeedsReseal(re) {
			t.Fatalf("reseal: %v", err)
		}
		onlyNew, _ := NewSealer(key('b'))
		if _, err := onlyNew.Open(re); err != nil {
			t.Fatalf("a resealed value must open with the new key alone: %v", err)
		}
	}

	// Without the old key, the error names the missing key id.
	onlyNew, _ := NewSealer(key('b'))
	if _, err := onlyNew.Open(v1); err == nil || !strings.Contains(err.Error(), KeyID(key('a'))) {
		t.Fatalf("want an error naming key %s, got %v", KeyID(key('a')), err)
	}
	if _, err := onlyNew.Open(legacy); err == nil {
		t.Fatal("a legacy value opened with the wrong key")
	}
}

func TestSealedHeaderIsAuthenticated(t *testing.T) {
	ring, _ := NewKeyring([][]byte{key('a'), key('b')})
	sealed := ring.Seal([]byte("x"))
	// Relabel the value with the other key's id: it must not open.
	forged := append([]byte{}, sealed...)
	copy(forged[4:8], []byte(mustHex(t, KeyID(key('b')))))
	if _, err := ring.Open(forged); err == nil {
		t.Fatal("a relabelled value opened")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := ring.Open(sealed); err == nil {
		t.Fatal("a tampered value opened")
	}
}

func mustHex(t *testing.T, s string) string {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestKeyringRefusesBadKeys(t *testing.T) {
	if _, err := NewKeyring(nil); err == nil {
		t.Fatal("empty keyring accepted")
	}
	if _, err := NewKeyring([][]byte{key('a'), []byte("short")}); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := NewKeyring([][]byte{key('a'), key('a')}); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if bytes.Equal(KeyCheck(key('a')), KeyCheck(key('b'))) || KeyID(key('a')) == KeyID(key('b')) {
		t.Fatal("different keys must have different ids and check values")
	}
}
