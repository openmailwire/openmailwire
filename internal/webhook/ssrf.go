package webhook

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// Address policy for webhook endpoints. Only global unicast addresses are
// dialed, minus every range in the IANA special-purpose registries
// (iana-ipv4-special-registry and iana-ipv6-special-registry), including
// the few listed there as globally reachable: no webhook needs them.
// Addresses that embed an IPv4 address are judged by that address.
var (
	blockedV4 = prefixes(
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // shared address space (CGNAT)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link local, cloud metadata
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.31.196.0/24", // AS112-v4
		"192.52.193.0/24", // AMT
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"192.168.0.0/16",  // private
		"192.175.48.0/24", // direct delegation AS112
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, and broadcast
	)
	// Global unicast IPv6 is 2000::/3. Everything outside it (loopback,
	// unspecified, IPv4-compatible, unique local, link local, multicast,
	// discard, SRv6 SIDs …) is refused, except the NAT64 well-known prefix.
	globalV6  = netip.MustParsePrefix("2000::/3")
	blockedV6 = prefixes(
		"2001::/23",         // IETF protocol assignments: Teredo, benchmarking, ORCHID, AMT, AS112 …
		"2001:db8::/32",     // documentation
		"2002::/16",         // 6to4: tunnels to an IPv4 address
		"2620:4f:8000::/48", // direct delegation AS112
		"3fff::/20",         // documentation
	)
	// NAT64 (RFC 6052): the well-known prefix carries an IPv4 address in its
	// last 32 bits and may only carry global ones. The local-use prefix
	// (RFC 8215) is not globally reachable at all.
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Public reports whether a webhook may be sent to a.
func Public(a netip.Addr) bool {
	a = a.Unmap() // ::ffff:a.b.c.d is a.b.c.d
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	if a.Is4() {
		return !inAny(a, blockedV4)
	}
	if nat64WellKnown.Contains(a) {
		b := a.As16()
		return Public(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	return globalV6.Contains(a) && !inAny(a, blockedV6)
}

// internal reports whether ip must not be dialed.
func internal(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return !ok || !Public(a)
}

var errBlocked = errors.New("webhook endpoint resolves to an address that is not public (loopback, private or reserved)")

// Timeouts and connection limits of webhook requests. An endpoint has to
// start answering within ResponseHeaderTimeout; process webhooks
// asynchronously and answer 2xx at once.
const (
	DialTimeout           = 5 * time.Second
	TLSHandshakeTimeout   = 5 * time.Second
	ResponseHeaderTimeout = 10 * time.Second // time to first byte, once the request is sent
	RequestTimeout        = 15 * time.Second // everything, body included
	IdleConnTimeout       = 30 * time.Second
	MaxIdleConns          = 64
	MaxIdleConnsPerHost   = 2
	MaxConnsPerHost       = 8
)

// NewHTTPClient returns a client that refuses to connect to addresses that
// are not public (see Public) unless allowPrivate is set. The check runs on
// the address actually dialed, after DNS, so a host name that resolves to
// an internal IP is refused too. Redirects are not followed.
func NewHTTPClient(allowPrivate bool) *http.Client {
	return newHTTPClient(allowPrivate, IdleConnTimeout)
}

func newHTTPClient(allowPrivate bool, idle time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: DialTimeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || internal(ip) {
				return errBlocked
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: RequestTimeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:    TLSHandshakeTimeout,
			ResponseHeaderTimeout:  ResponseHeaderTimeout,
			IdleConnTimeout:        idle,
			MaxIdleConns:           MaxIdleConns,
			MaxIdleConnsPerHost:    MaxIdleConnsPerHost,
			MaxConnsPerHost:        MaxConnsPerHost,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
