package config

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func b64key(c byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{c}, 32)) }

func TestSecretKeys(t *testing.T) {
	a, b := b64key('a'), b64key('b')
	for _, tc := range []struct {
		name, keys, key string
		want            []byte // first key's first byte; 0 for an error
		n               int
		err             string
	}{
		{name: "none", err: "OMW_SECRET_KEY is not set"},
		{name: "one key", key: a, want: []byte{'a'}, n: 1},
		{name: "list", keys: b + ", " + a, want: []byte{'b'}, n: 2},
		{name: "list with the old single key", keys: b + "," + a, key: a, want: []byte{'b'}, n: 2},
		{name: "single key not in list", keys: b, key: a, err: "not one of OMW_SECRET_KEYS"},
		{name: "duplicate", keys: a + "," + a, err: "twice"},
		{name: "bad entry", keys: a + ",nope", err: "entry 2 must be 32 bytes"},
		{name: "bad single", key: "short", err: "OMW_SECRET_KEY must be 32 bytes"},
		{name: "empty list", keys: " , ", err: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OMW_SECRET_KEYS", tc.keys)
			t.Setenv("OMW_SECRET_KEY", tc.key)
			keys, err := SecretKeys()
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want an error with %q, got %v", tc.err, err)
				}
				return
			}
			if err != nil || len(keys) != tc.n || keys[0][0] != tc.want[0] {
				t.Fatalf("got %d keys, %v", len(keys), err)
			}
		})
	}
}

func TestLoadControlPlatform(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("OMW_SECRET_KEY", "")
	t.Setenv("OMW_SECRET_KEYS", b64key('n')+","+b64key('o'))
	c, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SecretKeys) != 2 || c.SecretKey[0] != 'n' || c.WebhooksPerProject != DefaultWebhooksPerProject {
		t.Fatalf("%d keys, sealing key %q, webhooks per project %d", len(c.SecretKeys), c.SecretKey[:1], c.WebhooksPerProject)
	}
	t.Setenv("OMW_WEBHOOKS_PER_PROJECT", "0")
	if _, err := LoadControl(); err == nil {
		t.Fatal("OMW_WEBHOOKS_PER_PROJECT=0 accepted")
	}
}
