package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
)

// DNSAutomation configures publishing DNS records through a DNS host's API
// (lane G).
type DNSAutomation struct {
	// CloudflareAPIBase is Cloudflare's API v4 base URL
	// (OMW_CLOUDFLARE_API_BASE). Empty means the real one; tests point it at
	// a fake.
	CloudflareAPIBase string
	// CloudflareToken (OMW_CLOUDFLARE_TOKEN) is the operator's own token.
	// With OMW_SPF_INCLUDE it keeps that include's TXT record listing the
	// live workers' IPs. It is never used for customers' domains: those
	// need their owner's token.
	CloudflareToken string
}

// LoadDNSAutomation reads OMW_CLOUDFLARE_API_BASE and OMW_CLOUDFLARE_TOKEN.
// spfInclude is OMW_SPF_INCLUDE, which the token needs.
func LoadDNSAutomation(spfInclude string) (DNSAutomation, error) {
	d := DNSAutomation{CloudflareToken: strings.TrimSpace(os.Getenv("OMW_CLOUDFLARE_TOKEN"))}
	var err error
	if d.CloudflareAPIBase, err = CloudflareAPIBase(os.Getenv("OMW_CLOUDFLARE_API_BASE")); err != nil {
		return d, err
	}
	if d.CloudflareToken != "" {
		if strings.ContainsAny(d.CloudflareToken, " \t\r\n") || len(d.CloudflareToken) > 512 {
			return d, errors.New("OMW_CLOUDFLARE_TOKEN is not a Cloudflare API token; copy it again from Cloudflare (My Profile > API Tokens)")
		}
		if spfInclude == "" {
			return d, errors.New("OMW_CLOUDFLARE_TOKEN is set but OMW_SPF_INCLUDE is not: the token only keeps the SPF include " +
				"(e.g. _spf.mail.example.net) in sync with the workers' IPs; set OMW_SPF_INCLUDE, or remove the token")
		}
	}
	return d, nil
}

// CloudflareAPIBase checks OMW_CLOUDFLARE_API_BASE. Tokens travel to it, so
// it must use https, except to a loopback address (a local fake).
func CloudflareAPIBase(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	bad := errors.New("OMW_CLOUDFLARE_API_BASE must be an https URL without credentials or a query, like https://api.cloudflare.com/client/v4 " +
		"(plain http only to a loopback address, for tests)")
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", bad
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", bad
		}
	default:
		return "", bad
	}
	return raw, nil
}
