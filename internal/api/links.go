package api

import (
	"net/url"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Links in a body, for the require_approval has_links rule and the
// pre-send check (lane B).

var (
	bareURL = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"'()\[\]{}]+`)
	hrefURL = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// linkHosts returns the lowercase hosts that the text and HTML link to,
// sorted and without duplicates, and whether there is any web link at all
// (a relative href counts, though it has no host). mailto:, tel:, cid: and
// in-page (#) links are not web links.
func linkHosts(text, html string) (hosts []string, found bool) {
	seen := map[string]bool{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		low := strings.ToLower(raw)
		switch {
		case raw == "", strings.HasPrefix(raw, "#"), strings.HasPrefix(low, "mailto:"), strings.HasPrefix(low, "tel:"),
			strings.HasPrefix(low, "cid:"), strings.HasPrefix(low, "data:"), strings.HasPrefix(low, "sms:"):
			return
		case strings.HasPrefix(low, "www."):
			raw = "http://" + raw
		case strings.HasPrefix(raw, "//"):
			raw = "http:" + raw
		}
		found = true
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return
		}
		if h := strings.TrimSuffix(strings.ToLower(u.Hostname()), "."); h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	for _, s := range []string{text, html} {
		for _, m := range bareURL.FindAllString(s, -1) {
			add(strings.TrimRight(m, ".,;:!?"))
		}
	}
	for _, m := range hrefURL.FindAllStringSubmatch(html, -1) {
		add(m[1] + m[2] + m[3])
	}
	sort.Strings(hosts)
	return hosts, found
}

// siteOf returns a host's registrable domain (example.co.uk for
// mail.example.co.uk), or the host itself when it has none.
func siteOf(host string) string {
	if s, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return s
	}
	return host
}

// foreignHosts returns the hosts that are not on the same site as the
// sender's domain.
func foreignHosts(hosts []string, fromDomain string) []string {
	site := siteOf(fromDomain)
	var out []string
	for _, h := range hosts {
		if siteOf(h) != site {
			out = append(out, h)
		}
	}
	return out
}
