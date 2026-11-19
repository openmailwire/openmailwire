package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// APILimits protect the control plane's HTTP API (lane A, v0.6).
type APILimits struct {
	// KeyRate is how many requests per second one API key may make on
	// average, and KeyBurst how many it may make at once. A KeyRate of 0
	// turns the limit off. Each control plane counts on its own.
	KeyRate  float64
	KeyBurst int
	// LargeRequests is how many requests with a body over 1 MB are handled
	// at once; more wait for a slot.
	LargeRequests int
	// TrustedProxies are the reverse proxies whose X-Forwarded-For header is
	// believed when working out a client's address (API key allowed_ips).
	TrustedProxies []netip.Prefix
}

// Defaults for APILimits.
const (
	DefaultKeyRate       = 20
	DefaultLargeRequests = 4
)

func loadAPILimits() (APILimits, error) {
	l := APILimits{KeyRate: DefaultKeyRate}
	if v := strings.TrimSpace(os.Getenv("OMW_RATE_LIMIT")); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r < 0 || r > 1e6 {
			return l, fmt.Errorf("OMW_RATE_LIMIT must be requests per second per API key, like 20 (0 turns the limit off)")
		}
		l.KeyRate = r
	}
	burst := max(int(2*l.KeyRate), 1)
	var err error
	if l.KeyBurst, err = envInt("OMW_RATE_LIMIT_BURST", burst); err != nil {
		return l, err
	}
	if l.LargeRequests, err = envInt("OMW_MAX_LARGE_REQUESTS", DefaultLargeRequests); err != nil {
		return l, err
	}
	for _, f := range strings.FieldsFunc(os.Getenv("OMW_TRUSTED_PROXIES"), func(r rune) bool { return r == ',' || r == ' ' }) {
		p, err := netip.ParsePrefix(f)
		if err != nil {
			a, aerr := netip.ParseAddr(f)
			if aerr != nil {
				return l, fmt.Errorf("OMW_TRUSTED_PROXIES: %q is not an address or CIDR prefix, like 127.0.0.1 or 10.0.0.0/8", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		l.TrustedProxies = append(l.TrustedProxies, p.Masked())
	}
	return l, nil
}
