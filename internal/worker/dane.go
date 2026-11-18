package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// DANE for SMTP (RFC 7672): a receiver publishes, in DNSSEC-signed TLSA
// records, which certificate or key its MX host presents. A sender that can
// trust the answer requires TLS and checks the certificate against them.

// TLSA usages, selectors and matching types the worker honours.
const (
	DANETA = 2 // DANE-TA: a trust anchor in the server's chain
	DANEEE = 3 // DANE-EE: the server's own certificate or key

	SelectorCert = 0 // the whole certificate
	SelectorSPKI = 1 // the SubjectPublicKeyInfo

	MatchExact  = 0
	MatchSHA256 = 1
	MatchSHA512 = 2
)

// TLSARecord is one usable TLSA record.
type TLSARecord struct {
	Usage, Selector, MatchingType uint8
	Data                          []byte
}

func (r TLSARecord) String() string {
	return fmt.Sprintf("%d %d %d %x", r.Usage, r.Selector, r.MatchingType, r.Data)
}

// Usable reports whether the worker can check this record (RFC 7672
// section 3.1: PKIX usages 0 and 1 are not used for SMTP).
func (r TLSARecord) Usable() bool {
	if r.Usage != DANETA && r.Usage != DANEEE {
		return false
	}
	if r.Selector != SelectorCert && r.Selector != SelectorSPKI {
		return false
	}
	switch r.MatchingType {
	case MatchExact:
		return len(r.Data) > 0
	case MatchSHA256:
		return len(r.Data) == sha256.Size
	case MatchSHA512:
		return len(r.Data) == sha512.Size
	}
	return false
}

// Matches reports whether cert is what the record describes.
func (r TLSARecord) Matches(cert *x509.Certificate) bool {
	var in []byte
	switch r.Selector {
	case SelectorCert:
		in = cert.Raw
	case SelectorSPKI:
		in = cert.RawSubjectPublicKeyInfo
	default:
		return false
	}
	switch r.MatchingType {
	case MatchExact:
		return bytes.Equal(in, r.Data)
	case MatchSHA256:
		h := sha256.Sum256(in)
		return bytes.Equal(h[:], r.Data)
	case MatchSHA512:
		h := sha512.Sum512(in)
		return bytes.Equal(h[:], r.Data)
	}
	return false
}

// VerifyDANE checks a server's certificate chain (leaf first) against
// usable TLSA records.
//
//   - DANE-EE (3): the leaf matches. Names and dates are not checked
//     (RFC 7672 section 3.1.1).
//   - DANE-TA (2): a certificate in the chain matches, and the leaf chains
//     to it and is valid for one of names: the MX host, then the
//     recipient domain (section 3.2.2).
func VerifyDANE(chain []*x509.Certificate, recs []TLSARecord, names []string) error {
	if len(chain) == 0 {
		return errors.New("the server presented no certificate")
	}
	for _, r := range recs {
		if r.Usage == DANEEE && r.Matches(chain[0]) {
			return nil
		}
	}
	var lastErr error
	for _, r := range recs {
		if r.Usage != DANETA {
			continue
		}
		for _, ta := range chain {
			if !r.Matches(ta) {
				continue
			}
			roots := x509.NewCertPool()
			roots.AddCert(ta)
			inter := x509.NewCertPool()
			for _, c := range chain[1:] {
				inter.AddCert(c)
			}
			for _, name := range names {
				_, err := chain[0].Verify(x509.VerifyOptions{
					DNSName: name, Roots: roots, Intermediates: inter,
					KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
				})
				if err == nil {
					return nil
				}
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return fmt.Errorf("the certificate chains to a DANE-TA trust anchor but does not verify: %w", lastErr)
	}
	return errors.New("the certificate matches none of the TLSA records")
}

// TLSASource looks up the usable TLSA records of an MX host. No records
// (nil, nil) means DANE does not apply to the host. An error means the
// answer could not be trusted either way, so the host must not be used.
type TLSASource interface {
	TLSA(ctx context.Context, host string) ([]TLSARecord, error)
}

// DANEResolver asks a validating resolver for TLSA records over DNS and
// trusts only answers it marks authenticated (the AD bit). Run it only
// against a resolver you trust, over a path an attacker cannot touch
// (normally one on the same host): the AD bit itself is not protected.
type DANEResolver struct {
	Server  string // host:port, OMW_DNSSEC_RESOLVER
	Timeout time.Duration
}

// TLSA returns host's usable TLSA records for SMTP (_25._tcp.<host>).
//
// RFC 7672 section 2.2 applies DANE only to MX hosts whose address records
// are DNSSEC-secure, so the address lookup is checked first: an unsigned
// host is never asked for TLSA records, which also avoids the broken name
// servers that fail TLSA queries for unsigned zones.
func (r *DANEResolver) TLSA(ctx context.Context, host string) ([]TLSARecord, error) {
	host = dns.Fqdn(strings.ToLower(host))
	a, err := r.query(ctx, host, dns.TypeA)
	if err != nil {
		return nil, fmt.Errorf("address lookup for %s: %w", strings.TrimSuffix(host, "."), err)
	}
	if !a.AuthenticatedData {
		return nil, nil
	}
	name := "_25._tcp." + host
	m, err := r.query(ctx, name, dns.TypeTLSA)
	if err != nil {
		return nil, fmt.Errorf("TLSA lookup for %s: %w", strings.TrimSuffix(name, "."), err)
	}
	if !m.AuthenticatedData || m.Rcode == dns.RcodeNameError {
		return nil, nil
	}
	var out []TLSARecord
	for _, rr := range m.Answer {
		t, ok := rr.(*dns.TLSA)
		if !ok {
			continue
		}
		data, err := hex.DecodeString(t.Certificate)
		if err != nil {
			continue
		}
		rec := TLSARecord{Usage: t.Usage, Selector: t.Selector, MatchingType: t.MatchingType, Data: data}
		if rec.Usable() {
			out = append(out, rec)
		}
	}
	return out, nil
}

// query asks the resolver with the DO bit set (and AD, to say the answer's
// AD bit is wanted, RFC 6840 section 5.7). Only NOERROR and NXDOMAIN are
// answers; SERVFAIL is what a validating resolver says about bogus data.
func (r *DANEResolver) query(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	m.RecursionDesired = true
	m.AuthenticatedData = true
	m.SetEdns0(4096, true)
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	c := &dns.Client{Net: "udp", Timeout: timeout}
	in, _, err := c.ExchangeContext(ctx, m, r.Server)
	if err == nil && in.Truncated {
		c.Net = "tcp"
		in, _, err = c.ExchangeContext(ctx, m, r.Server)
	}
	if err != nil {
		return nil, err
	}
	if in.Rcode != dns.RcodeSuccess && in.Rcode != dns.RcodeNameError {
		return nil, fmt.Errorf("resolver answered %s", dns.RcodeToString[in.Rcode])
	}
	return in, nil
}
