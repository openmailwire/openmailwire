package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/openmailwire/openmailwire/internal/mailparse"
	"github.com/openmailwire/openmailwire/internal/store"
)

// reportAddresses are where a domain's DMARC and TLS-RPT aggregate reports
// should go: dmarc@ and tls-rpt@ its return-path domain, which already has
// an MX pointing to the inbound nodes.
type reportAddresses struct {
	DMARC        string `json:"dmarc_rua"`
	TLS          string `json:"tls_rpt_rua"`
	DMARCExample string `json:"dmarc_record_example"`
	TLSRecord    struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"tls_rpt_record"`
}

func (s *Server) reportAddresses(d store.Domain) *reportAddresses {
	if d.ReturnPath == "" {
		return nil
	}
	a := &reportAddresses{
		DMARC: "mailto:" + dmarcReportLocal + "@" + d.ReturnPath,
		TLS:   "mailto:" + tlsReportLocal + "@" + d.ReturnPath,
	}
	a.DMARCExample = "v=DMARC1; p=none; rua=" + a.DMARC
	a.TLSRecord.Name = "_smtp._tls." + d.Name
	a.TLSRecord.Value = "v=TLSRPTv1; rua=" + a.TLS
	return a
}

type dmarcSourceView struct {
	SourceIP      string   `json:"source_ip"`
	Messages      int64    `json:"messages"`
	Pass          int64    `json:"pass"`
	Fail          int64    `json:"fail"`
	PassRate      float64  `json:"pass_rate"`
	DKIMPass      int64    `json:"dkim_pass"`
	SPFPass       int64    `json:"spf_pass"`
	Worker        bool     `json:"openmailwire_worker"`
	Organizations []string `json:"organizations"`
	HeaderFrom    []string `json:"header_from"`
}

func rate(pass, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(int64(float64(pass)/float64(total)*10000)) / 10000
}

// domainReports summarizes the DMARC aggregate and TLS-RPT reports a
// domain received: GET /domains/{id}/reports?days=30 (1–180).
func (s *Server) domainReports(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 180 {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "days must be between 1 and 180")
			return
		}
		days = n
	}
	ctx := r.Context()
	rep, err := s.Store.ReportsSince(ctx, d.ProjectID, d.ID, time.Now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		s.internal(w, "domain reports", err)
		return
	}
	workers, err := s.Store.WorkerIPs(ctx)
	if err != nil {
		s.internal(w, "worker ips", err)
		return
	}
	isWorker := map[string]bool{}
	for _, ip := range workers {
		isWorker[ip] = true
	}

	var total, pass int64
	sources := []dmarcSourceView{}
	unknown := []dmarcSourceView{}
	for _, x := range rep.Sources {
		v := dmarcSourceView{SourceIP: x.SourceIP, Messages: x.Messages, Pass: x.Pass, Fail: x.Messages - x.Pass,
			PassRate: rate(x.Pass, x.Messages), DKIMPass: x.DKIMPass, SPFPass: x.SPFPass, Worker: isWorker[x.SourceIP],
			Organizations: nonEmpty(x.Organizations), HeaderFrom: nonEmpty(x.HeaderFroms)}
		total += x.Messages
		pass += x.Pass
		sources = append(sources, v)
		// A source none of whose mail passes, and that is not one of our
		// workers, sends as the domain without authorization: a spoofer,
		// or a service that still needs SPF or DKIM set up.
		if x.Pass == 0 && !v.Worker {
			unknown = append(unknown, v)
		}
	}
	orgs := []map[string]any{}
	for _, o := range rep.Orgs {
		orgs = append(orgs, map[string]any{"organization": o.OrgName, "reports": o.Reports, "messages": o.Messages,
			"pass": o.Pass, "fail": o.Messages - o.Pass, "pass_rate": rate(o.Pass, o.Messages)})
	}

	var tlsOK, tlsFailed int64
	tlsOrgs := []map[string]any{}
	failures := []map[string]any{}
	for _, t := range rep.TLS {
		tlsOK += t.Successful
		tlsFailed += t.Failed
		tlsOrgs = append(tlsOrgs, map[string]any{"organization": t.OrgName, "policy_type": t.PolicyType, "policy_domain": t.PolicyDomain,
			"successful_sessions": t.Successful, "failed_sessions": t.Failed})
		for _, raw := range t.Failures {
			var fs []mailparse.TLSFailure
			json.Unmarshal(raw, &fs)
			for _, f := range fs {
				if len(failures) == 200 {
					break
				}
				failures = append(failures, map[string]any{"organization": t.OrgName, "policy_type": t.PolicyType,
					"result_type": f.ResultType, "sending_mta_ip": f.SendingMTAIP, "receiving_mx_hostname": f.ReceivingMXHostname,
					"receiving_ip": f.ReceivingIP, "failed_sessions": f.FailedSessions, "additional_information": f.Info,
					"failure_reason_code": f.ReasonCode})
			}
		}
	}

	reports := []map[string]any{}
	for _, x := range rep.Recent {
		reports = append(reports, map[string]any{"id": x.ID, "kind": x.Kind, "organization": x.OrgName, "report_id": x.ReportID,
			"begin": x.Begin, "end": x.End, "messages": x.Messages, "failed": x.Failed, "truncated": x.Truncated, "received_at": x.ReceivedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "domain_reports", "domain_id": d.ID, "domain": d.Name, "days": days,
		"addresses": s.reportAddresses(d),
		"dmarc": map[string]any{
			"messages": total, "pass": pass, "fail": total - pass, "pass_rate": rate(pass, total),
			"by_organization": orgs, "by_source": sources, "unknown_senders": unknown,
		},
		"tls": map[string]any{
			"successful_sessions": tlsOK, "failed_sessions": tlsFailed, "by_organization": tlsOrgs, "failures": failures,
		},
		"reports": reports,
	})
}

func nonEmpty(l []string) []string {
	out := []string{}
	for _, v := range l {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
