package openmailwire

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrWaitTimeout is returned (wrapped) by WaitForDelivery when its timeout
// passes while a recipient is still queued, sending or deferred.
var ErrWaitTimeout = errors.New("openmailwire: timed out waiting for delivery")

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// wait returns how long to sleep before the next poll, or false when the
// deadline has passed. A zero deadline never passes.
func (c *Client) wait(deadline time.Time) (time.Duration, bool) {
	d := c.pollInterval
	if deadline.IsZero() {
		return d, true
	}
	left := time.Until(deadline)
	if left <= 0 {
		return 0, false
	}
	if left < d {
		d = left
	}
	return d, true
}

// WaitForDelivery polls Emails.Get until no recipient is queued, sending or
// deferred, and returns that email. Every recipient then has a final status
// (delivered, bounced, expired, suppressed, unknown or canceled): check each
// one.
//
// After timeout it returns the last state seen and an error wrapping
// ErrWaitTimeout. A timeout of 0 or less waits until ctx is done.
func (c *Client) WaitForDelivery(ctx context.Context, emailID string, timeout time.Duration) (*Email, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		email, err := c.Emails.Get(ctx, emailID)
		if err != nil {
			return nil, err
		}
		var pending []string
		for _, r := range email.Recipients {
			if !r.Final() {
				pending = append(pending, r.Status)
			}
		}
		if len(pending) == 0 {
			return email, nil
		}
		d, ok := c.wait(deadline)
		if !ok {
			return email, fmt.Errorf("%w: email %s: %d recipient(s) still %s after %s",
				ErrWaitTimeout, emailID, len(pending), strings.Join(pending, ", "), timeout)
		}
		if err := sleep(ctx, d); err != nil {
			return email, err
		}
	}
}

// WaitForReply polls the inboxes until received messages answer the sent
// email emailID (their InReplyToEmailID), and returns them, newest first.
// It returns an empty slice and no error if none arrived within timeout.
//
// It looks at the 50 newest messages of each inbox, or only of inboxIDs when
// given. A timeout of 0 or less waits until ctx is done.
func (c *Client) WaitForReply(ctx context.Context, emailID string, timeout time.Duration, inboxIDs ...string) ([]MessageSummary, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		ids := inboxIDs
		if len(ids) == 0 {
			list, err := c.Inboxes.List(ctx)
			if err != nil {
				return nil, err
			}
			for _, in := range list.Data {
				ids = append(ids, in.ID)
			}
		}
		found := []MessageSummary{}
		for _, id := range ids {
			page, err := c.Inboxes.ListMessages(ctx, id, &ListMessagesParams{Limit: 50})
			if err != nil {
				return nil, err
			}
			for _, m := range page.Data {
				if m.InReplyToEmailID == emailID {
					found = append(found, m)
				}
			}
		}
		if len(found) > 0 {
			sort.SliceStable(found, func(i, j int) bool { return found[i].ReceivedAt.After(found[j].ReceivedAt) })
			return found, nil
		}
		d, ok := c.wait(deadline)
		if !ok {
			return found, nil
		}
		if err := sleep(ctx, d); err != nil {
			return found, err
		}
	}
}
