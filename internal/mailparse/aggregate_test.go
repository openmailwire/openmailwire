package mailparse

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"strings"
	"testing"
)

const dmarcXML = `<?xml version="1.0" encoding="UTF-8" ?>
<feedback xmlns="urn:ietf:params:xml:ns:dmarc-2.0">
  <version>1.0</version>
  <report_metadata>
    <org_name>google.com</org_name>
    <email>noreply-dmarc-support@google.com</email>
    <report_id>13416812357125678543</report_id>
    <date_range><begin>1763337600</begin><end>1763423999</end></date_range>
    <generator>future-field</generator>
  </report_metadata>
  <policy_published>
    <domain>Acme.test</domain><adkim>r</adkim><aspf>r</aspf><p>none</p><sp>none</sp><pct>100</pct><np>reject</np>
  </policy_published>
  <record>
    <row>
      <source_ip>203.0.113.7</source_ip><count>42</count>
      <policy_evaluated><disposition>none</disposition><dkim>pass</dkim><spf>pass</spf></policy_evaluated>
    </row>
    <identifiers><header_from>acme.test</header_from><envelope_from>bounce.acme.test</envelope_from></identifiers>
    <auth_results>
      <dkim><domain>acme.test</domain><selector>omw202611ab</selector><result>pass</result></dkim>
      <spf><domain>bounce.acme.test</domain><scope>mfrom</scope><result>pass</result></spf>
    </auth_results>
  </record>
  <record>
    <row>
      <source_ip>::ffff:198.51.100.66</source_ip><count>3</count>
      <policy_evaluated><disposition>none</disposition><dkim>fail</dkim><spf>fail</spf>
        <reason><type>local_policy</type></reason></policy_evaluated>
    </row>
    <identifiers><header_from>acme.test</header_from></identifiers>
    <auth_results><spf><domain>spoof.test</domain><result>softfail</result></spf></auth_results>
  </record>
  <record><row><source_ip>not-an-ip</source_ip><count>1</count></row></record>
</feedback>`

const tlsJSON = `{
  "organization-name": "Company-X",
  "date-range": {"start-datetime": "2026-11-17T00:00:00Z", "end-datetime": "2026-11-17T23:59:59Z"},
  "contact-info": "sts-reporting@company-x.example",
  "report-id": "5065427c-23d3-47ca-b6e0-946ea0e8c4be",
  "policies": [{
    "policy": {"policy-type": "sts", "policy-string": ["version: STSv1", "mode: testing"], "policy-domain": "acme.test", "mx-host": ["*.mail.acme.test"]},
    "summary": {"total-successful-session-count": 5326, "total-failure-session-count": 303},
    "failure-details": [
      {"result-type": "certificate-expired", "sending-mta-ip": "2001:db8:abcd:0012::1", "receiving-mx-hostname": "mx1.mail.acme.test", "failed-session-count": 100},
      {"result-type": "starttls-not-supported", "sending-mta-ip": "2001:db8:abcd:0013::1", "receiving-mx-hostname": "mx2.mail.acme.test",
       "receiving-ip": "203.0.113.56", "failed-session-count": 200, "additional-information": "https://reports.company-x.example/report_info?id=5065427c-23d3"}
    ]
  }]
}`

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func zipped(name string, b []byte) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create(name)
	f.Write(b)
	w.Close()
	return buf.Bytes()
}

// reportMail wraps an attachment the way Google and Microsoft send them.
func reportMail(contentType, filename string, data []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(data)
	var lines []string
	for len(enc) > 76 {
		lines = append(lines, enc[:76])
		enc = enc[76:]
	}
	lines = append(lines, enc)
	return []byte("From: noreply-dmarc-support@google.com\r\nTo: dmarc@bounce.acme.test\r\n" +
		"Subject: Report domain: acme.test Submitter: google.com\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"R\"\r\n\r\n" +
		"--R\r\nContent-Type: text/plain\r\n\r\nThis is an aggregate report.\r\n" +
		"--R\r\nContent-Type: " + contentType + "; name=\"" + filename + "\"\r\nContent-Disposition: attachment; filename=\"" + filename + "\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + strings.Join(lines, "\r\n") + "\r\n--R--\r\n")
}

func TestParseDMARCReport(t *testing.T) {
	for name, msg := range map[string][]byte{
		"gzip":  reportMail("application/gzip", "google.com!acme.test!1763337600!1763423999.xml.gz", gz([]byte(dmarcXML))),
		"zip":   reportMail("application/zip", "report.zip", zipped("report.xml", []byte(dmarcXML))),
		"plain": reportMail("text/xml", "report.xml", []byte(dmarcXML)),
		// The whole body is the attachment, labelled with the wrong type.
		"body": []byte("Subject: r\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			base64.StdEncoding.EncodeToString(gz([]byte(dmarcXML))) + "\r\n"),
	} {
		r, err := ParseDMARCReport(msg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.OrgName != "google.com" || r.ReportID != "13416812357125678543" || r.Policy.Domain != "acme.test" || r.Policy.P != "none" ||
			r.Policy.NP != "reject" || r.Begin.Unix() != 1763337600 || r.End.Unix() != 1763423999 {
			t.Fatalf("%s: report %+v", name, r)
		}
		if len(r.Records) != 2 {
			t.Fatalf("%s: records %+v", name, r.Records)
		}
		a, b := r.Records[0], r.Records[1]
		if a.SourceIP != "203.0.113.7" || a.Count != 42 || a.DKIM != "pass" || a.SPF != "pass" || a.EnvelopeFrom != "bounce.acme.test" ||
			len(a.DKIMResults) != 1 || a.DKIMResults[0].Selector != "omw202611ab" || a.SPFResults[0].Scope != "mfrom" {
			t.Fatalf("%s: first record %+v", name, a)
		}
		if b.SourceIP != "198.51.100.66" || b.Count != 3 || b.DKIM != "fail" || b.Reasons[0] != "local_policy" || b.SPFResults[0].Result != "softfail" {
			t.Fatalf("%s: second record %+v", name, b)
		}
	}
}

func TestParseTLSReport(t *testing.T) {
	for name, msg := range map[string][]byte{
		"gzip": reportMail("application/tlsrpt+gzip", "company-x.example!acme.test!1763337600!1763423999!001.json.gz", gz([]byte(tlsJSON))),
		"json": reportMail("application/tlsrpt+json", "report.json", []byte(tlsJSON)),
	} {
		r, err := ParseTLSReport(msg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.OrgName != "Company-X" || r.ReportID != "5065427c-23d3-47ca-b6e0-946ea0e8c4be" || r.Begin.IsZero() || len(r.Policies) != 1 {
			t.Fatalf("%s: %+v", name, r)
		}
		p := r.Policies[0]
		if p.PolicyType != "sts" || p.PolicyDomain != "acme.test" || p.Successful != 5326 || p.Failed != 303 || len(p.Failures) != 2 ||
			p.Failures[1].ResultType != "starttls-not-supported" || p.Failures[1].FailedSessions != 200 || p.Failures[1].ReceivingIP != "203.0.113.56" {
			t.Fatalf("%s: policy %+v", name, p)
		}
	}
}

// A small gzip or zip that expands past MaxReportSize is refused without
// holding more than the limit in memory, whatever the zip entry claims.
func TestReportBombs(t *testing.T) {
	huge := bytes.Repeat([]byte{' '}, MaxReportSize+1024)
	bomb := gz(huge)
	if len(bomb) > 64<<10 {
		t.Fatalf("test bomb is %d bytes", len(bomb))
	}
	if _, err := ParseDMARCReport(reportMail("application/gzip", "r.xml.gz", bomb)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("gzip bomb: %v", err)
	}
	if _, err := ParseTLSReport(reportMail("application/zip", "r.zip", zipped("r.json", huge))); err == nil {
		t.Fatal("zip bomb accepted")
	}
	// Many zip entries.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for i := 0; i < maxZipEntries+1; i++ {
		f, _ := w.Create(strings.Repeat("x", i+1))
		f.Write([]byte("<feedback/>"))
	}
	w.Close()
	if _, err := ParseDMARCReport(reportMail("application/zip", "r.zip", buf.Bytes())); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("many entries: %v", err)
	}
	// Deep nesting.
	deep := "<feedback>" + strings.Repeat("<a>", 10000) + strings.Repeat("</a>", 10000) + "</feedback>"
	if _, err := ParseDMARCReport(reportMail("text/xml", "r.xml", []byte(deep))); err == nil || !strings.Contains(err.Error(), "deeply") {
		t.Fatalf("deep XML: %v", err)
	}
	// Entities are not expanded.
	lol := `<?xml version="1.0"?><!DOCTYPE l [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;&a;&a;">]>` +
		`<feedback><report_metadata><report_id>&b;</report_id></report_metadata></feedback>`
	if _, err := ParseDMARCReport(reportMail("text/xml", "r.xml", []byte(lol))); err == nil {
		t.Fatal("entity expansion accepted")
	}
	if _, err := ParseDMARCReport([]byte("Subject: hi\r\n\r\njust text\r\n")); err == nil {
		t.Fatal("a plain message parsed as a report")
	}
}

func TestDMARCRecordsCapped(t *testing.T) {
	rec := `<record><row><source_ip>192.0.2.1</source_ip><count>1</count><policy_evaluated><dkim>pass</dkim><spf>pass</spf></policy_evaluated></row></record>`
	doc := `<feedback><report_metadata><org_name>x</org_name><report_id>r</report_id></report_metadata>` +
		`<policy_published><domain>acme.test</domain></policy_published>` + strings.Repeat(rec, MaxReportRows+5) + `</feedback>`
	r, err := ParseDMARCReport(reportMail("application/gzip", "r.xml.gz", gz([]byte(doc))))
	if err != nil || len(r.Records) != MaxReportRows || !r.Truncated {
		t.Fatalf("capped report: %v, %d records", err, len(r.Records))
	}
}

func FuzzAggregateReports(f *testing.F) {
	f.Add([]byte(dmarcXML))
	f.Add([]byte(tlsJSON))
	f.Add(gz([]byte(dmarcXML)))
	f.Add(zipped("r.xml", []byte(dmarcXML)))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 64<<10 {
			return
		}
		msg := append([]byte("Content-Type: application/octet-stream\r\n\r\n"), b...)
		ParseDMARCReport(msg)
		ParseTLSReport(msg)
	})
}

func FuzzHTMLToText(f *testing.F) {
	f.Add(`<p>a<br>b</p><script>x</script><div style="display:none">h</div>`)
	f.Add(`<head><title>x</title><body><p hidden>y</p>z`)
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 64<<10 {
			return
		}
		out := HTMLToText(s)
		if strings.Contains(out, "\n\n") {
			t.Fatalf("empty line kept: %q", out)
		}
	})
}
