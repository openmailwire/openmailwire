package mailauth

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// SPF results (RFC 7208 section 2.6).
const (
	None      = "none"
	Neutral   = "neutral"
	Pass      = "pass"
	Fail      = "fail"
	SoftFail  = "softfail"
	TempError = "temperror"
	PermError = "permerror"
)

// RFC 7208 section 4.6.4: at most 10 terms that cause DNS lookups, at most
// 2 lookups that find nothing, at most 10 MX or PTR names per term.
const (
	maxDNSTerms   = 10
	maxVoid       = 2
	maxNames      = 10
	maxRecordSize = 4096
)

var (
	errPerm = errors.New(PermError)
	errTemp = errors.New(TempError)
)

// SPFResult is the outcome of one SPF check.
type SPFResult struct {
	Result string `json:"result"`
	Domain string `json:"domain,omitempty"` // the domain checked
	Reason string `json:"reason,omitempty"`
}

// spfCheck is one evaluation (check_host and everything it recurses
// into), carrying the shared lookup budget.
type spfCheck struct {
	r      Resolver
	ip     netip.Addr
	sender string // the full sender address (local@domain)
	helo   string
	terms  int
	voids  int
}

// CheckSPF evaluates the SPF policy of domain for a message from ip,
// with sender the RFC5321.MailFrom address ("postmaster@<helo>" when it
// was empty) and helo the client's HELO name.
func CheckSPF(ctx context.Context, r Resolver, ip net.IP, domain, sender, helo string) SPFResult {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return SPFResult{Result: None, Domain: domain, Reason: "no client address"}
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if !validDomain(domain) {
		return SPFResult{Result: None, Domain: domain, Reason: "not a domain name"}
	}
	c := &spfCheck{r: r, ip: addr.Unmap(), sender: sender, helo: helo}
	res, reason := c.checkHost(ctx, domain, 0)
	return SPFResult{Result: res, Domain: domain, Reason: reason}
}

func (c *spfCheck) checkHost(ctx context.Context, domain string, depth int) (string, string) {
	if depth > maxDNSTerms {
		return PermError, "include or redirect loop"
	}
	rec, res, reason := c.record(ctx, domain)
	if res != "" {
		return res, reason
	}
	terms := strings.Fields(rec)[1:]
	var redirect string
	for _, term := range terms {
		if name, value, ok := modifier(term); ok {
			if name == "redirect" {
				if redirect != "" {
					return PermError, "two redirect modifiers"
				}
				redirect = value
			}
			continue
		}
		qual, mech := Pass, term
		switch term[0] {
		case '+':
			mech = term[1:]
		case '-':
			qual, mech = Fail, term[1:]
		case '~':
			qual, mech = SoftFail, term[1:]
		case '?':
			qual, mech = Neutral, term[1:]
		}
		match, err := c.mechanism(ctx, domain, strings.ToLower(mech), mech, depth)
		if err != nil {
			if errors.Is(err, errTemp) {
				return TempError, err.Error()
			}
			return PermError, err.Error()
		}
		if match {
			return qual, "matched " + truncate(term, 80)
		}
	}
	if redirect != "" {
		if err := c.count(); err != nil {
			return PermError, err.Error()
		}
		target, err := c.expand(ctx, redirect, domain)
		if err != nil {
			return PermError, "redirect: " + err.Error()
		}
		res, reason := c.checkHost(ctx, target, depth+1)
		if res == None {
			return PermError, "redirect to " + target + " which has no SPF record"
		}
		return res, reason
	}
	return Neutral, "no mechanism matched"
}

// record returns the domain's one SPF record, or a final result.
func (c *spfCheck) record(ctx context.Context, domain string) (rec, res, reason string) {
	txts, err := c.r.LookupTXT(ctx, domain)
	if err != nil {
		if notFound(err) {
			return "", None, "no SPF record at " + domain
		}
		return "", TempError, "TXT lookup for " + domain + " failed"
	}
	var found []string
	for _, t := range txts {
		l := strings.ToLower(t)
		if l == "v=spf1" || strings.HasPrefix(l, "v=spf1 ") {
			found = append(found, t)
		}
	}
	switch {
	case len(found) == 0:
		return "", None, "no SPF record at " + domain
	case len(found) > 1:
		return "", PermError, "more than one SPF record at " + domain
	case len(found[0]) > maxRecordSize:
		return "", PermError, "SPF record too long"
	}
	return found[0], "", ""
}

// modifier recognizes name=value terms. A mechanism never has '=' before
// any ':' or '/'.
func modifier(term string) (name, value string, ok bool) {
	i := strings.IndexAny(term, "=:/")
	if i <= 0 || term[i] != '=' {
		return "", "", false
	}
	return strings.ToLower(term[:i]), term[i+1:], true
}

func (c *spfCheck) count() error {
	c.terms++
	if c.terms > maxDNSTerms {
		return errors.New("more than 10 DNS lookups")
	}
	return nil
}

func (c *spfCheck) void() error {
	c.voids++
	if c.voids > maxVoid {
		return errors.New("more than 2 lookups found nothing")
	}
	return nil
}

func (c *spfCheck) mechanism(ctx context.Context, domain, lower, orig string, depth int) (bool, error) {
	name, arg, hasArg := strings.Cut(lower, ":")
	_, origArg, _ := strings.Cut(orig, ":")
	// a and mx take an optional /cidr after the name or the domain.
	cidrPart := ""
	if name == "a" || name == "mx" || strings.HasPrefix(name, "a/") || strings.HasPrefix(name, "mx/") {
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name, cidrPart = name[:i], name[i:]
		} else if hasArg {
			if i := strings.IndexByte(origArg, '/'); i >= 0 {
				origArg, cidrPart = origArg[:i], origArg[i:]
			}
		}
	}
	switch name {
	case "all":
		if hasArg {
			return false, errPermf("bad mechanism %q", orig)
		}
		return true, nil
	case "ip4", "ip6":
		if !hasArg {
			return false, errPermf("%s needs an address", name)
		}
		p, err := parsePrefix(arg, name == "ip6")
		if err != nil {
			return false, err
		}
		return p.Contains(c.ip), nil
	case "include":
		if err := c.count(); err != nil {
			return false, err
		}
		if !hasArg {
			return false, errPermf("include needs a domain")
		}
		target, err := c.expand(ctx, origArg, domain)
		if err != nil {
			return false, err
		}
		res, _ := c.checkHost(ctx, target, depth+1)
		switch res {
		case Pass:
			return true, nil
		case Fail, SoftFail, Neutral:
			return false, nil
		case TempError:
			return false, errTemp
		}
		return false, errPermf("include:%s has no usable SPF record", target)
	case "a", "mx":
		if err := c.count(); err != nil {
			return false, err
		}
		target := domain
		if hasArg {
			var err error
			if target, err = c.expand(ctx, origArg, domain); err != nil {
				return false, err
			}
		}
		bits4, bits6, err := dualCIDR(cidrPart)
		if err != nil {
			return false, err
		}
		hosts := []string{target}
		if name == "mx" {
			mxs, err := c.r.LookupMX(ctx, target)
			if err != nil && !notFound(err) {
				return false, errTemp
			}
			if len(mxs) == 0 {
				return false, c.void()
			}
			if len(mxs) > maxNames {
				return false, errPermf("more than 10 MX records at %s", target)
			}
			hosts = hosts[:0]
			for _, mx := range mxs {
				hosts = append(hosts, strings.TrimSuffix(mx.Host, "."))
			}
		}
		for _, h := range hosts {
			ips, err := c.r.LookupIPAddr(ctx, h)
			if err != nil && !notFound(err) {
				return false, errTemp
			}
			if len(ips) == 0 && name == "a" {
				if err := c.void(); err != nil {
					return false, err
				}
			}
			for _, ip := range ips {
				if inCIDR(ip.IP, c.ip, bits4, bits6) {
					return true, nil
				}
			}
		}
		return false, nil
	case "exists":
		if err := c.count(); err != nil {
			return false, err
		}
		if !hasArg {
			return false, errPermf("exists needs a domain")
		}
		target, err := c.expand(ctx, origArg, domain)
		if err != nil {
			return false, err
		}
		ips, err := c.r.LookupIPAddr(ctx, target)
		if err != nil && !notFound(err) {
			return false, errTemp
		}
		for _, ip := range ips {
			if ip.IP.To4() != nil {
				return true, nil
			}
		}
		return false, c.void()
	case "ptr":
		if err := c.count(); err != nil {
			return false, err
		}
		target := domain
		if hasArg {
			var err error
			if target, err = c.expand(ctx, origArg, domain); err != nil {
				return false, err
			}
		}
		return c.validatedPTR(ctx, target), nil
	}
	return false, errPermf("unknown mechanism %q", truncate(orig, 40))
}

// validatedPTR reports whether the client's address has a PTR name, in
// target or below it, whose own addresses include the client's.
func (c *spfCheck) validatedPTR(ctx context.Context, target string) bool {
	names, err := c.r.LookupAddr(ctx, c.ip.String())
	if err != nil {
		return false
	}
	for i, n := range names {
		if i == maxNames {
			break
		}
		n = strings.ToLower(strings.TrimSuffix(n, "."))
		if n != target && !strings.HasSuffix(n, "."+target) {
			continue
		}
		ips, err := c.r.LookupIPAddr(ctx, n)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip.IP); ok && a.Unmap() == c.ip {
				return true
			}
		}
	}
	return false
}

func parsePrefix(s string, v6 bool) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Is4() == v6 && !a.Is4In6() {
			return netip.Prefix{}, errPermf("bad address %q", truncate(s, 60))
		}
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || p.Addr().Is4() == v6 {
		return netip.Prefix{}, errPermf("bad network %q", truncate(s, 60))
	}
	return p.Masked(), nil
}

// dualCIDR reads "/24", "//64" or "/24//64".
func dualCIDR(s string) (v4, v6 int, err error) {
	v4, v6 = 32, 128
	if s == "" {
		return
	}
	four, six, hasSix := strings.Cut(s[1:], "//")
	if strings.HasPrefix(s, "//") {
		four, six, hasSix = "", s[2:], true
	}
	if four != "" {
		if v4, err = strconv.Atoi(four); err != nil || v4 < 0 || v4 > 32 {
			return 0, 0, errPermf("bad cidr %q", s)
		}
	}
	if hasSix {
		if v6, err = strconv.Atoi(six); err != nil || v6 < 0 || v6 > 128 {
			return 0, 0, errPermf("bad cidr %q", s)
		}
	}
	return v4, v6, nil
}

func inCIDR(ip net.IP, client netip.Addr, bits4, bits6 int) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if a.Is4() != client.Is4() {
		return false
	}
	bits := bits6
	if a.Is4() {
		bits = bits4
	}
	p, err := a.Prefix(bits)
	return err == nil && p.Contains(client)
}

// expand applies SPF macros (RFC 7208 section 7) to a domain-spec and
// checks the result is a domain name, shortening it from the left when it
// is longer than 253 characters.
func (c *spfCheck) expand(_ context.Context, spec, domain string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(spec); i++ {
		ch := spec[i]
		if ch != '%' {
			b.WriteByte(ch)
			continue
		}
		if i+1 >= len(spec) {
			return "", errPermf("bad macro in %q", truncate(spec, 60))
		}
		i++
		switch spec[i] {
		case '%':
			b.WriteByte('%')
		case '_':
			b.WriteByte(' ')
		case '-':
			b.WriteString("%20")
		case '{':
			end := strings.IndexByte(spec[i:], '}')
			if end < 0 {
				return "", errPermf("bad macro in %q", truncate(spec, 60))
			}
			v, err := c.macro(spec[i+1:i+end], domain)
			if err != nil {
				return "", err
			}
			b.WriteString(v)
			i += end
		default:
			return "", errPermf("bad macro in %q", truncate(spec, 60))
		}
	}
	out := strings.ToLower(strings.TrimSuffix(b.String(), "."))
	for len(out) > 253 {
		_, rest, ok := strings.Cut(out, ".")
		if !ok {
			break
		}
		out = rest
	}
	if !validDomain(out) {
		return "", errPermf("%q is not a domain name", truncate(out, 60))
	}
	return out, nil
}

func (c *spfCheck) macro(m, domain string) (string, error) {
	if m == "" {
		return "", errPermf("empty macro")
	}
	letter := m[0] | 0x20
	rest := m[1:]
	var v string
	local, senderDomain, _ := strings.Cut(c.sender, "@")
	switch letter {
	case 's':
		v = c.sender
	case 'l':
		v = local
	case 'o':
		v = senderDomain
	case 'd':
		v = domain
	case 'i':
		if c.ip.Is4() {
			v = c.ip.String()
		} else {
			b := c.ip.As16()
			parts := make([]string, 0, 32)
			for _, x := range b {
				parts = append(parts, strconv.FormatUint(uint64(x>>4), 16), strconv.FormatUint(uint64(x&15), 16))
			}
			v = strings.Join(parts, ".")
		}
	case 'v':
		v = "in-addr"
		if !c.ip.Is4() {
			v = "ip6"
		}
	case 'h':
		v = c.helo
	case 'p':
		v = "unknown"
	default:
		return "", errPermf("unknown macro letter %q", string(m[0]))
	}
	// Transformers: digits, then 'r'; then delimiters.
	n := 0
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		n = n*10 + int(rest[j]-'0')
		if n > 128 {
			return "", errPermf("macro keeps too many labels")
		}
		j++
	}
	reverse := false
	if j < len(rest) && rest[j]|0x20 == 'r' {
		reverse = true
		j++
	}
	delims := rest[j:]
	if strings.Trim(delims, ".-+,/_=") != "" {
		return "", errPermf("bad macro delimiter in %q", m)
	}
	if delims == "" {
		delims = "."
	}
	parts := strings.FieldsFunc(v, func(r rune) bool { return strings.ContainsRune(delims, r) })
	if reverse {
		for a, b := 0, len(parts)-1; a < b; a, b = a+1, b-1 {
			parts[a], parts[b] = parts[b], parts[a]
		}
	}
	if j > 0 && n == 0 && !reverse {
		return "", errPermf("macro keeps zero labels")
	}
	if n > 0 && n < len(parts) {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, "."), nil
}

func errPermf(format string, args ...any) error {
	return &permError{msg: sprintf(format, args...)}
}

type permError struct{ msg string }

func (e *permError) Error() string { return e.msg }
func (e *permError) Is(t error) bool {
	return t == errPerm
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// validDomain checks a DNS name loosely: labels of 1–63 bytes, at least
// two of them, no empty label, total at most 253.
func validDomain(d string) bool {
	if d == "" || len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for _, l := range strings.Split(d, ".") {
		if l == "" || len(l) > 63 {
			return false
		}
		for i := 0; i < len(l); i++ {
			if c := l[i]; c <= ' ' || c >= 0x7f {
				return false
			}
		}
	}
	return true
}
