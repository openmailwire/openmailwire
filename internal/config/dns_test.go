package config

import (
	"strings"
	"testing"
)

func TestCloudflareAPIBase(t *testing.T) {
	for raw, ok := range map[string]bool{
		"":                                       true,
		"https://api.cloudflare.com/client/v4/":  true,
		"http://127.0.0.1:8787/client/v4":        true,
		"http://localhost:8787":                  true,
		"http://[::1]:8787":                      true,
		"http://api.cloudflare.com/client/v4":    false, // a token in plain text over the network
		"https://user:pass@api.cloudflare.com":   false,
		"https://api.cloudflare.com/client/v4?x": false,
		"ftp://api.cloudflare.com":               false,
		"api.cloudflare.com":                     false,
	} {
		got, err := CloudflareAPIBase(raw)
		if (err == nil) != ok {
			t.Errorf("%q: %v", raw, err)
		}
		if ok && strings.HasSuffix(got, "/") {
			t.Errorf("%q: trailing slash kept: %q", raw, got)
		}
	}
}

func TestLoadDNSAutomation(t *testing.T) {
	t.Setenv("OMW_CLOUDFLARE_API_BASE", "")
	t.Setenv("OMW_CLOUDFLARE_TOKEN", "")
	if d, err := LoadDNSAutomation(""); err != nil || d.CloudflareToken != "" {
		t.Fatalf("unset: %+v %v", d, err)
	}
	t.Setenv("OMW_CLOUDFLARE_TOKEN", "abcdefghijklmnopqrstuvwxyz0123456789ABCD")
	if _, err := LoadDNSAutomation(""); err == nil || !strings.Contains(err.Error(), "OMW_SPF_INCLUDE") {
		t.Fatalf("token without an include: %v", err)
	}
	if d, err := LoadDNSAutomation("_spf.mail.example.net"); err != nil || d.CloudflareToken == "" {
		t.Fatalf("token and include: %+v %v", d, err)
	}
	t.Setenv("OMW_CLOUDFLARE_TOKEN", "two words")
	if _, err := LoadDNSAutomation("_spf.mail.example.net"); err == nil || strings.Contains(err.Error(), "two words") {
		t.Fatalf("malformed token: %v", err)
	}
	t.Setenv("OMW_CLOUDFLARE_TOKEN", "")
	t.Setenv("OMW_CLOUDFLARE_API_BASE", "http://example.com")
	if _, err := LoadDNSAutomation(""); err == nil {
		t.Fatal("plain http to a remote host accepted")
	}
}
