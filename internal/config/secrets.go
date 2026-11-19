package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultWebhooksPerProject is how many webhooks a project may have unless
// OMW_WEBHOOKS_PER_PROJECT says otherwise.
const DefaultWebhooksPerProject = 10

// SecretKeys reads the keys that seal data at rest.
//
//   - OMW_SECRET_KEYS: a comma-separated list of base64 keys. The first seals
//     new values; the others only open values sealed before a rotation.
//   - OMW_SECRET_KEY: one key, as before v0.6. When both are set, it must be
//     one of OMW_SECRET_KEYS.
func SecretKeys() ([][]byte, error) {
	list := strings.TrimSpace(os.Getenv("OMW_SECRET_KEYS"))
	single := strings.TrimSpace(os.Getenv("OMW_SECRET_KEY"))
	if list == "" && single == "" {
		return nil, errors.New("OMW_SECRET_KEY is not set; generate one with `openmailwire keygen`")
	}
	if list == "" {
		k, err := decodeKey(single)
		if err != nil {
			return nil, fmt.Errorf("OMW_SECRET_KEY %w", err)
		}
		return [][]byte{k}, nil
	}
	var keys [][]byte
	for i, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, err := decodeKey(part)
		if err != nil {
			return nil, fmt.Errorf("OMW_SECRET_KEYS entry %d %w", i+1, err)
		}
		for _, prev := range keys {
			if bytes.Equal(prev, k) {
				return nil, fmt.Errorf("OMW_SECRET_KEYS lists entry %d twice; list each key once, the newest first", i+1)
			}
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("OMW_SECRET_KEYS is empty; list the keys, the newest first")
	}
	if single != "" {
		k, err := decodeKey(single)
		if err != nil {
			return nil, fmt.Errorf("OMW_SECRET_KEY %w", err)
		}
		found := false
		for _, have := range keys {
			found = found || bytes.Equal(have, k)
		}
		if !found {
			return nil, errors.New("OMW_SECRET_KEY is not one of OMW_SECRET_KEYS; add it to the list (after the new key) or unset it")
		}
	}
	return keys, nil
}

func decodeKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(k) != 32 {
		return nil, errors.New("must be 32 bytes, base64 encoded; generate one with `openmailwire keygen`")
	}
	return k, nil
}

// loadPlatform reads the lane E settings of the control plane.
func (c *Control) loadPlatform() error {
	keys, err := SecretKeys()
	if err != nil {
		return err
	}
	c.SecretKeys = keys
	c.SecretKey = keys[0]
	if c.WebhooksPerProject, err = envInt("OMW_WEBHOOKS_PER_PROJECT", DefaultWebhooksPerProject); err != nil {
		return err
	}
	return nil
}
