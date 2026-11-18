package protocol

// What an inbound recipient address is for.
const (
	InboundBounce    = "bounce"    // a signed VERP address: delivery status notifications (and complaints)
	InboundComplaint = "complaint" // the feedback-loop address (OMW_FBL_ADDRESS): ARF complaints
	InboundInbox     = "inbox"     // an inbox address
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
	Rcpt    string `json:"rcpt"`
	Kind    string `json:"kind,omitempty"`
	Outcome string `json:"outcome"` // stored, duplicate, bounced, complained, ignored or rejected
	ID      string `json:"id,omitempty"`
}

type InboundResponse struct {
	Results []InboundResult `json:"results"`
}
