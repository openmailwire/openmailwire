package mailparse

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Aggregate reports that receivers send to a domain's rua= addresses:
// DMARC aggregate reports (RFC 7489 appendix C, and DMARCbis) in XML, and
// SMTP TLS reports (TLS-RPT, RFC 8460) in JSON. Both usually arrive
// compressed (gzip, or zip for DMARC) as an attachment, or as the whole
// body.
//
// They come from anyone who can send mail, so every step is bounded: the
// compressed input, the decompressed size (whatever the archive claims),
// the zip entry count, the XML nesting and the number of rows kept.
const (
	// MaxReportSize is the most a report may decompress to.
	MaxReportSize = 16 << 20
	// MaxReportRows caps the rows kept from one report (DMARC records,
	// TLS-RPT policies and failure details each).
	MaxReportRows = 10000
	maxZipEntries = 16
	maxXMLDepth   = 32
)

// ErrNoReport means the message carries no report this parser can read.
var ErrNoReport = errors.New("no aggregate report found in the message")

// DMARCReport is a DMARC aggregate report.
type DMARCReport struct {
	OrgName   string
	Email     string
	ReportID  string
	Begin     time.Time
	End       time.Time
	Policy    DMARCPolicy
	Records   []DMARCRecord
	Truncated bool // more than MaxReportRows records; the rest were dropped
}

// DMARCPolicy is the policy the reporter found published.
type DMARCPolicy struct {
	Domain string `json:"domain"`
	ADKIM  string `json:"adkim,omitempty"`
	ASPF   string `json:"aspf,omitempty"`
	P      string `json:"p,omitempty"`
	SP     string `json:"sp,omitempty"`
	NP     string `json:"np,omitempty"`
	PCT    string `json:"pct,omitempty"`
}

// DMARCRecord is one row: messages from one source IP with one outcome.
type DMARCRecord struct {
	SourceIP     string
	Count        int64
	Disposition  string // none, quarantine, reject
	DKIM         string // aligned DKIM result as the reporter evaluated it: pass or fail
	SPF          string // aligned SPF result: pass or fail
	Reasons      []string
	HeaderFrom   string
	EnvelopeFrom string
	DKIMResults  []AuthResult // every signature the reporter checked
	SPFResults   []AuthResult
}

// AuthResult is one raw DKIM or SPF result in a DMARC record.
type AuthResult struct {
	Domain   string `json:"domain"`
	Selector string `json:"selector,omitempty"` // DKIM
	Scope    string `json:"scope,omitempty"`    // SPF: mfrom or helo
	Result   string `json:"result"`
}

// TLSReport is an SMTP TLS report.
type TLSReport struct {
	OrgName   string
	ReportID  string
	Contact   string
	Begin     time.Time
	End       time.Time
	Policies  []TLSPolicyResult
	Truncated bool
}

// TLSPolicyResult is the outcome of one policy the reporter applied.
type TLSPolicyResult struct {
	PolicyType   string // sts, tlsa or no-policy-found
	PolicyDomain string
	MXHosts      []string
	Successful   int64
	Failed       int64
	Failures     []TLSFailure
}

// TLSFailure is one failure detail.
type TLSFailure struct {
	ResultType          string `json:"result_type"`
	SendingMTAIP        string `json:"sending_mta_ip,omitempty"`
	ReceivingMXHostname string `json:"receiving_mx_hostname,omitempty"`
	ReceivingIP         string `json:"receiving_ip,omitempty"`
	FailedSessions      int64  `json:"failed_sessions"`
	Info                string `json:"additional_information,omitempty"`
	ReasonCode          string `json:"failure_reason_code,omitempty"`
}

// ParseDMARCReport finds and parses the DMARC aggregate report in a
// message.
func ParseDMARCReport(raw []byte) (*DMARCReport, error) {
	var firstErr error
	for _, data := range reportPayloads(raw) {
		doc, err := decompress(data)
		if err == nil {
			var r *DMARCReport
			if r, err = parseDMARCXML(doc); err == nil {
				return r, nil
			}
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = ErrNoReport
	}
	return nil, firstErr
}

// ParseTLSReport finds and parses the TLS-RPT report in a message.
func ParseTLSReport(raw []byte) (*TLSReport, error) {
	var firstErr error
	for _, data := range reportPayloads(raw) {
		doc, err := decompress(data)
		if err == nil {
			var r *TLSReport
			if r, err = parseTLSJSON(doc); err == nil {
				return r, nil
			}
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = ErrNoReport
	}
	return nil, firstErr
}

// reportPayloads lists the parts that may hold a report: attachments and
// non-text bodies first, then XML or JSON text parts.
func reportPayloads(raw []byte) [][]byte {
	var first, later [][]byte
	walk(raw, func(p part) {
		if len(p.body) == 0 {
			return
		}
		mt := p.mediaType
		switch {
		case mt == "text/plain" || mt == "text/html":
			// Usually the human-readable cover note, but some reporters
			// send the XML itself as text/plain.
			if t := bytes.TrimSpace(p.body); len(t) > 0 && (t[0] == '<' || t[0] == '{') {
				later = append(later, p.body)
			}
		case strings.HasPrefix(mt, "multipart/"), strings.HasPrefix(mt, "message/"):
		default:
			first = append(first, p.body)
		}
	})
	return append(first, later...)
}

// decompress undoes gzip or zip (detected from the content, not the
// declared type, which reporters get wrong), refusing anything that would
// grow past MaxReportSize.
func decompress(data []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		return readLimited(zr)
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("zip: %w", err)
		}
		if len(zr.File) > maxZipEntries {
			return nil, fmt.Errorf("zip: %d entries; a report has one", len(zr.File))
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			if f.UncompressedSize64 > MaxReportSize {
				return nil, fmt.Errorf("zip: entry says it is %d bytes; the limit is %d", f.UncompressedSize64, MaxReportSize)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("zip: %w", err)
			}
			b, err := readLimited(rc)
			rc.Close()
			return b, err
		}
		return nil, errors.New("zip: no file inside")
	}
	if len(data) > MaxReportSize {
		return nil, fmt.Errorf("report is larger than %d bytes", MaxReportSize)
	}
	return data, nil
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxReportSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxReportSize {
		return nil, fmt.Errorf("report decompresses to more than %d bytes", MaxReportSize)
	}
	return b, nil
}

// ---- DMARC XML ----

type xmlMetadata struct {
	OrgName   string `xml:"org_name"`
	Email     string `xml:"email"`
	ReportID  string `xml:"report_id"`
	DateRange struct {
		Begin string `xml:"begin"`
		End   string `xml:"end"`
	} `xml:"date_range"`
}

type xmlPolicy struct {
	Domain string `xml:"domain"`
	ADKIM  string `xml:"adkim"`
	ASPF   string `xml:"aspf"`
	P      string `xml:"p"`
	SP     string `xml:"sp"`
	NP     string `xml:"np"`
	PCT    string `xml:"pct"`
}

type xmlRecord struct {
	Row struct {
		SourceIP string `xml:"source_ip"`
		Count    string `xml:"count"`
		Policy   struct {
			Disposition string `xml:"disposition"`
			DKIM        string `xml:"dkim"`
			SPF         string `xml:"spf"`
			Reasons     []struct {
				Type string `xml:"type"`
			} `xml:"reason"`
		} `xml:"policy_evaluated"`
	} `xml:"row"`
	Identifiers struct {
		HeaderFrom   string `xml:"header_from"`
		EnvelopeFrom string `xml:"envelope_from"`
	} `xml:"identifiers"`
	AuthResults struct {
		DKIM []AuthResult `xml:"dkim"`
		SPF  []AuthResult `xml:"spf"`
	} `xml:"auth_results"`
}

// AuthResult doubles as the XML shape of <dkim> and <spf> results.
func (a *AuthResult) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var v struct {
		Domain   string `xml:"domain"`
		Selector string `xml:"selector"`
		Scope    string `xml:"scope"`
		Result   string `xml:"result"`
	}
	if err := d.DecodeElement(&v, &start); err != nil {
		return err
	}
	*a = AuthResult{Domain: cleanField(v.Domain, 253), Selector: cleanField(v.Selector, 63), Scope: cleanField(v.Scope, 10), Result: cleanField(v.Result, 20)}
	return nil
}

func parseDMARCXML(doc []byte) (*DMARCReport, error) {
	d := xml.NewDecoder(bytes.NewReader(doc))
	d.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		return charsetReader(label, in)
	}
	r := &DMARCReport{}
	depth, root, seenRecords := 0, false, 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("dmarc report XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxXMLDepth {
				return nil, errors.New("dmarc report XML is nested too deeply")
			}
			if depth == 1 {
				if t.Name.Local != "feedback" {
					return nil, fmt.Errorf("dmarc report XML: root element is <%s>, not <feedback>", t.Name.Local)
				}
				root = true
				continue
			}
			if depth != 2 {
				continue
			}
			switch t.Name.Local {
			case "report_metadata":
				var m xmlMetadata
				if err := d.DecodeElement(&m, &t); err != nil {
					return nil, fmt.Errorf("dmarc report_metadata: %w", err)
				}
				depth--
				r.OrgName, r.Email, r.ReportID = cleanField(m.OrgName, 200), cleanField(m.Email, 254), cleanField(m.ReportID, 200)
				r.Begin, r.End = unixField(m.DateRange.Begin), unixField(m.DateRange.End)
			case "policy_published":
				var p xmlPolicy
				if err := d.DecodeElement(&p, &t); err != nil {
					return nil, fmt.Errorf("dmarc policy_published: %w", err)
				}
				depth--
				r.Policy = DMARCPolicy{Domain: strings.ToLower(cleanField(p.Domain, 253)), ADKIM: cleanField(p.ADKIM, 5),
					ASPF: cleanField(p.ASPF, 5), P: cleanField(p.P, 20), SP: cleanField(p.SP, 20), NP: cleanField(p.NP, 20), PCT: cleanField(p.PCT, 5)}
			case "record":
				seenRecords++
				if len(r.Records) >= MaxReportRows {
					r.Truncated = true
					if err := d.Skip(); err != nil {
						return nil, err
					}
					depth--
					continue
				}
				var x xmlRecord
				if err := d.DecodeElement(&x, &t); err != nil {
					return nil, fmt.Errorf("dmarc record %d: %w", seenRecords, err)
				}
				depth--
				if rec, ok := dmarcRecord(x); ok {
					r.Records = append(r.Records, rec)
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	if !root {
		return nil, errors.New("dmarc report XML has no <feedback> element")
	}
	if r.ReportID == "" || r.Policy.Domain == "" {
		return nil, errors.New("dmarc report lacks report_id or policy_published/domain")
	}
	return r, nil
}

func dmarcRecord(x xmlRecord) (DMARCRecord, bool) {
	ip, err := netip.ParseAddr(strings.TrimSpace(x.Row.SourceIP))
	if err != nil {
		return DMARCRecord{}, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(x.Row.Count), 10, 64)
	if err != nil || n < 0 {
		return DMARCRecord{}, false
	}
	rec := DMARCRecord{
		SourceIP: ip.Unmap().String(), Count: min(n, 1<<40),
		Disposition:  strings.ToLower(cleanField(x.Row.Policy.Disposition, 20)),
		DKIM:         strings.ToLower(cleanField(x.Row.Policy.DKIM, 20)),
		SPF:          strings.ToLower(cleanField(x.Row.Policy.SPF, 20)),
		HeaderFrom:   strings.ToLower(cleanField(x.Identifiers.HeaderFrom, 253)),
		EnvelopeFrom: strings.ToLower(cleanField(x.Identifiers.EnvelopeFrom, 253)),
	}
	for i, reason := range x.Row.Policy.Reasons {
		if i == 10 {
			break
		}
		rec.Reasons = append(rec.Reasons, cleanField(reason.Type, 40))
	}
	if len(x.AuthResults.DKIM) > 10 {
		x.AuthResults.DKIM = x.AuthResults.DKIM[:10]
	}
	if len(x.AuthResults.SPF) > 10 {
		x.AuthResults.SPF = x.AuthResults.SPF[:10]
	}
	rec.DKIMResults, rec.SPFResults = x.AuthResults.DKIM, x.AuthResults.SPF
	return rec, true
}

// ---- TLS-RPT JSON ----

type jsonTLSReport struct {
	OrganizationName string `json:"organization-name"`
	DateRange        struct {
		Start string `json:"start-datetime"`
		End   string `json:"end-datetime"`
	} `json:"date-range"`
	ContactInfo string `json:"contact-info"`
	ReportID    string `json:"report-id"`
	Policies    []struct {
		Policy struct {
			Type    string   `json:"policy-type"`
			Domain  string   `json:"policy-domain"`
			MXHosts []string `json:"mx-host"`
		} `json:"policy"`
		Summary struct {
			Successful int64 `json:"total-successful-session-count"`
			Failed     int64 `json:"total-failure-session-count"`
		} `json:"summary"`
		FailureDetails []struct {
			ResultType          string `json:"result-type"`
			SendingMTAIP        string `json:"sending-mta-ip"`
			ReceivingMXHostname string `json:"receiving-mx-hostname"`
			ReceivingIP         string `json:"receiving-ip"`
			FailedSessions      int64  `json:"failed-session-count"`
			AdditionalInfo      string `json:"additional-information"`
			ReasonCode          string `json:"failure-reason-code"`
		} `json:"failure-details"`
	} `json:"policies"`
}

func parseTLSJSON(doc []byte) (*TLSReport, error) {
	var j jsonTLSReport
	if err := json.Unmarshal(doc, &j); err != nil {
		return nil, fmt.Errorf("tls report JSON: %w", err)
	}
	if j.ReportID == "" || len(j.Policies) == 0 {
		return nil, errors.New("tls report lacks report-id or policies")
	}
	r := &TLSReport{
		OrgName: cleanField(j.OrganizationName, 200), ReportID: cleanField(j.ReportID, 200), Contact: cleanField(j.ContactInfo, 254),
		Begin: timeField(j.DateRange.Start), End: timeField(j.DateRange.End),
	}
	rows := 0
	for _, p := range j.Policies {
		if rows >= MaxReportRows {
			r.Truncated = true
			break
		}
		rows++
		pr := TLSPolicyResult{
			PolicyType: strings.ToLower(cleanField(p.Policy.Type, 30)), PolicyDomain: strings.ToLower(cleanField(p.Policy.Domain, 253)),
			Successful: max(p.Summary.Successful, 0), Failed: max(p.Summary.Failed, 0),
		}
		for i, h := range p.Policy.MXHosts {
			if i == 20 {
				break
			}
			pr.MXHosts = append(pr.MXHosts, cleanField(h, 253))
		}
		for _, f := range p.FailureDetails {
			if rows >= MaxReportRows {
				r.Truncated = true
				break
			}
			rows++
			pr.Failures = append(pr.Failures, TLSFailure{
				ResultType: strings.ToLower(cleanField(f.ResultType, 60)), SendingMTAIP: cleanField(f.SendingMTAIP, 45),
				ReceivingMXHostname: cleanField(f.ReceivingMXHostname, 253), ReceivingIP: cleanField(f.ReceivingIP, 45),
				FailedSessions: max(f.FailedSessions, 0), Info: cleanField(f.AdditionalInfo, 300), ReasonCode: cleanField(f.ReasonCode, 100),
			})
		}
		r.Policies = append(r.Policies, pr)
	}
	return r, nil
}

// ---- helpers ----

// cleanField trims a value from a report, removes control characters and
// cuts it to n bytes on a character boundary.
func cleanField(s string, n int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f || isBidiControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "")))
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

func unixField(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 || n > 1<<35 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

func timeField(s string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// charsetReader decodes report XML declared in another charset.
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	l := strings.ToLower(strings.TrimSpace(label))
	if l == "utf-8" || l == "utf8" || l == "us-ascii" || l == "ascii" {
		return in, nil
	}
	return wordDecoder.CharsetReader(l, in)
}
