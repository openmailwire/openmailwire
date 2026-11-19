package worker

import (
	"context"
	"fmt"
	"net"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// SelfCheckResolver is what the self-check looks up; *net.Resolver has it.
type SelfCheckResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// SelfChecker checks, when a direct worker starts, what receivers check
// about it: Gmail and Outlook refuse or junk mail from an IP whose reverse
// DNS does not name a host that points back to it, and nothing is delivered
// where port 25 is blocked.
type SelfChecker struct {
	EHLO     string // OMW_EHLO
	PublicIP string // OMW_PUBLIC_IP
	SourceIP string // OMW_SOURCE_IP, to test port 25 from the right address
	Resolver SelfCheckResolver
	// SMTP is the host:port the port 25 check connects to; empty means the
	// first MX of ProbeDomain on port 25.
	SMTP        string
	ProbeDomain string        // default gmail.com
	Timeout     time.Duration // per check; default 15 seconds
}

var hostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Run performs every check. OK is true when all passed.
func (s *SelfChecker) Run(ctx context.Context) protocol.SelfCheck {
	if s.Timeout == 0 {
		s.Timeout = 15 * time.Second
	}
	if s.ProbeDomain == "" {
		s.ProbeDomain = "gmail.com"
	}
	out := protocol.SelfCheck{OK: true, CheckedAt: time.Now().UTC()}
	add := func(name string, err error) {
		item := protocol.CheckItem{Name: name, OK: err == nil}
		if err != nil {
			item.Detail = err.Error()
			out.OK = false
		}
		out.Checks = append(out.Checks, item)
	}
	ehlo := strings.TrimSuffix(strings.ToLower(s.EHLO), ".")
	add("ehlo_fqdn", s.checkFQDN(ehlo))
	add("ehlo_a_record", s.checkA(ctx, ehlo))
	add("reverse_dns", s.checkPTR(ctx, ehlo))
	add("port25_egress", s.checkPort25(ctx))
	return out
}

func (s *SelfChecker) checkFQDN(ehlo string) error {
	if ehlo == "" || ehlo == "localhost" || net.ParseIP(ehlo) != nil || !hostnameRE.MatchString(ehlo) {
		return fmt.Errorf("OMW_EHLO %q is not a fully qualified host name; set it to the name the sending IP's PTR record points to, e.g. mail1.example.net", s.EHLO)
	}
	return nil
}

func (s *SelfChecker) checkA(ctx context.Context, ehlo string) error {
	if s.PublicIP == "" {
		return fmt.Errorf("OMW_PUBLIC_IP is not set; set it to the IP this worker sends from")
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	ips, err := s.Resolver.LookupIPAddr(ctx, ehlo)
	if err != nil {
		return fmt.Errorf("looking up %s: %v; publish an A record %s -> %s", ehlo, err, ehlo, s.PublicIP)
	}
	want := net.ParseIP(s.PublicIP)
	var got []string
	for _, ip := range ips {
		if ip.IP.Equal(want) {
			return nil
		}
		got = append(got, ip.IP.String())
	}
	return fmt.Errorf("%s points to %s, not to OMW_PUBLIC_IP %s; fix the A record or OMW_PUBLIC_IP", ehlo, strings.Join(got, ", "), s.PublicIP)
}

func (s *SelfChecker) checkPTR(ctx context.Context, ehlo string) error {
	if s.PublicIP == "" {
		return fmt.Errorf("OMW_PUBLIC_IP is not set")
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	names, err := s.Resolver.LookupAddr(ctx, s.PublicIP)
	if err != nil || len(names) == 0 {
		return fmt.Errorf("%s has no reverse DNS (PTR) record; ask the IP's provider to set it to %s", s.PublicIP, ehlo)
	}
	for _, n := range names {
		if strings.EqualFold(strings.TrimSuffix(n, "."), ehlo) {
			return nil // and the A check confirms it points back
		}
	}
	return fmt.Errorf("the PTR record of %s names %s, not OMW_EHLO %s; set OMW_EHLO to that name (with an A record pointing back), or change the PTR",
		s.PublicIP, strings.TrimSuffix(names[0], "."), ehlo)
}

func (s *SelfChecker) checkPort25(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	addr := s.SMTP
	if addr == "" {
		mxs, err := s.Resolver.LookupMX(ctx, s.ProbeDomain)
		if err != nil || len(mxs) == 0 {
			return fmt.Errorf("could not find a server to test port 25 with (MX of %s): %v", s.ProbeDomain, err)
		}
		addr = net.JoinHostPort(strings.TrimSuffix(mxs[0].Host, "."), "25")
	}
	conn, err := dial(ctx, s.SourceIP, addr, s.Timeout)
	if err != nil {
		return fmt.Errorf("cannot connect to %s: %v; outbound port 25 is probably blocked by the provider or a firewall", addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	tp := textproto.NewConn(conn)
	code, _, err := tp.ReadResponse(0)
	if err != nil {
		return fmt.Errorf("%s accepted the connection but sent no SMTP greeting (%v); something between this host and the internet intercepts port 25", addr, err)
	}
	tp.PrintfLine("QUIT")
	if code != 220 {
		return fmt.Errorf("%s greeted with %d instead of 220; check that this IP is not on a blocklist", addr, code)
	}
	return nil
}
