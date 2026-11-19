package apikey

import (
	"fmt"
	"sort"
)

// Safety is the part of a key's policy about approvals (v0.6, agent
// safety). It is embedded in Policy, so its fields sit at the top level of
// the policy JSON.
type Safety struct {
	// RequireApproval holds the key's emails for a person (or a reviewing
	// agent) to approve when any of its rules matches.
	RequireApproval *ApprovalRules `json:"require_approval,omitempty"`
	// CanApprove lets the key approve and reject held emails of other keys
	// (POST /emails/{id}/approve and /reject). Full-access keys always can.
	CanApprove bool `json:"can_approve,omitempty"`
}

// ApprovalRules says which sends are held. Any matching rule holds the
// whole email.
type ApprovalRules struct {
	Always         bool `json:"always,omitempty"`          // every email
	FirstContact   bool `json:"first_contact,omitempty"`   // a recipient the project has never queued mail to
	HasAttachments bool `json:"has_attachments,omitempty"` // any attachment, inline ones included
	HasLinks       bool `json:"has_links,omitempty"`       // a web link in the text or HTML
	RecipientsOver *int `json:"recipients_over,omitempty"` // more than N recipients (to, cc and bcc together)
}

// Approval rule names, as reported in hold reasons.
const (
	RuleAlways         = "always"
	RuleFirstContact   = "first_contact"
	RuleHasAttachments = "has_attachments"
	RuleHasLinks       = "has_links"
	RuleRecipientsOver = "recipients_over"
)

func (s *Safety) normalize() error {
	r := s.RequireApproval
	if r == nil {
		return nil
	}
	if r.RecipientsOver != nil && (*r.RecipientsOver < 0 || *r.RecipientsOver > 1000) {
		return fmt.Errorf("policy.require_approval.recipients_over must be between 0 and 1000 (0 holds every email)")
	}
	if !r.Always && !r.FirstContact && !r.HasAttachments && !r.HasLinks && r.RecipientsOver == nil {
		return fmt.Errorf("policy.require_approval has no rule; set always, first_contact, has_attachments, has_links or recipients_over to true (or a number), or leave require_approval out")
	}
	return nil
}

// Facts describe one send for the approval rules. FirstContact is asked
// only when that rule is on, because it needs a lookup.
type Facts struct {
	Recipients     int
	HasAttachments bool
	HasLinks       bool
	FirstContact   func() (bool, error)
}

// HoldReasons returns the rules that hold this send, sorted, or none.
func (p Policy) HoldReasons(f Facts) ([]string, error) {
	r := p.RequireApproval
	if r == nil {
		return nil, nil
	}
	var out []string
	if r.Always {
		out = append(out, RuleAlways)
	}
	if r.HasAttachments && f.HasAttachments {
		out = append(out, RuleHasAttachments)
	}
	if r.HasLinks && f.HasLinks {
		out = append(out, RuleHasLinks)
	}
	if r.RecipientsOver != nil && f.Recipients > *r.RecipientsOver {
		out = append(out, RuleRecipientsOver)
	}
	if r.FirstContact && f.FirstContact != nil {
		first, err := f.FirstContact()
		if err != nil {
			return nil, err
		}
		if first {
			out = append(out, RuleFirstContact)
		}
	}
	sort.Strings(out)
	return out, nil
}
