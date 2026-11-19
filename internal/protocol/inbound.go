package protocol

// What an inbound recipient address is for.
const (
	InboundBounce    = "bounce"    // a signed VERP address: delivery status notifications (and complaints)
	InboundComplaint = "complaint" // the feedback-loop address (OMW_FBL_ADDRESS): ARF complaints
	InboundInbox     = "inbox"     // an inbox address
	// Lane D (v0.6): aggregate reports and the RFC 5321 role addresses.
	InboundReport     = "report"     // dmarc@ or tls-rpt@ a return-path domain: DMARC and TLS-RPT aggregate reports
	InboundPostmaster = "postmaster" // postmaster@ or abuse@ a domain the inbound nodes serve
)

// InboundCheckRequest asks whether the control plane accepts mail for one
// RCPT TO address.
type InboundCheckRequest struct {
	Rcpt string `json:"rcpt"`
}

type InboundCheckResponse struct {
	Accept bool   `json:"accept"`
	Kind   string `json:"kind,omitempty"`
}

// InboundMessage is one message accepted by an inbound node, posted after
// DATA. Raw holds the message exactly as received (CRLF line endings), with
// the node's Received header on top.
type InboundMessage struct {
	MailFrom string   `json:"mail_from"`
	Rcpts    []string `json:"rcpts"`
	Raw      []byte   `json:"raw"`
	RemoteIP string   `json:"remote_ip"`
	HELO     string   `json:"helo"`
	TLS      bool     `json:"tls"`
}

// InboundResult says what the control plane did for one recipient.
type InboundResult struct {
	Rcpt string `json:"rcpt"`
	Kind string `json:"kind,omitempty"`
	// stored, duplicate, bounced, complained, ignored, logged, rejected, or
	// deferred (the node answers 451 so the sender retries later).
	Outcome string `json:"outcome"`
	ID      string `json:"id,omitempty"`
}

type InboundResponse struct {
	Results []InboundResult `json:"results"`
}
