package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const devDeliverUsage = `usage: openmailwire dev-deliver --to ADDRESS [--to ADDRESS]... [--from ADDRESS] [--subject TEXT]
                                [--text TEXT] [--attach FILE]... [--in-reply-to MESSAGE-ID] [--inbound HOST:PORT]

Sends one realistic test message over SMTP to an inbound node (default 127.0.0.1:2525,
the inbound service of docker-compose.yml), the way a mail server on the internet
would, so receiving mail can be tried without the internet. --in-reply-to takes the
Message-ID of an email you sent (<id@domain>), to try threading and wait_for_reply.
Development only.`

// devMessage is what dev-deliver sends.
type devMessage struct {
	From      string
	To        []string
	Subject   string
	Text      string
	InReplyTo string
	Attach    []string // file paths
	Date      time.Time
}

func devDeliver(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dev-deliver", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var to, attach patterns
	fs.Var(&to, "to", "")
	fs.Var(&attach, "attach", "")
	from := fs.String("from", "Alex Sender <alex@sender.test>", "")
	subject := fs.String("subject", "Hello from dev-deliver", "")
	text := fs.String("text", "", "")
	inReplyTo := fs.String("in-reply-to", "", "")
	addr := fs.String("inbound", "127.0.0.1:2525", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New(devDeliverUsage)
	}
	if len(to) == 0 {
		return errors.New("--to is required\n\n" + devDeliverUsage)
	}
	if _, _, err := net.SplitHostPort(*addr); err != nil {
		return fmt.Errorf("--inbound must be host:port, like 127.0.0.1:2525")
	}
	m := devMessage{From: *from, To: to, Subject: *subject, Text: *text, InReplyTo: *inReplyTo, Attach: attach, Date: time.Now()}
	if m.Text == "" {
		m.Text = "This is a test message from openmailwire dev-deliver, sent " + m.Date.UTC().Format(time.RFC1123) +
			".\r\nIf you can read it in an inbox, receiving mail works.\r\n"
	}
	raw, msgID, sender, rcpts, err := buildDevMessage(m)
	if err != nil {
		return err
	}
	if err := sendDev(ctx, *addr, sender, rcpts, raw); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s accepted Message-ID %s from %s for %s (%d bytes)\n", *addr, msgID, sender, strings.Join(rcpts, ", "), len(raw))
	return nil
}

// buildDevMessage renders m as a MIME message with CRLF line endings. It
// returns the message, its Message-ID, the envelope sender and recipients.
func buildDevMessage(m devMessage) (raw []byte, msgID, sender string, rcpts []string, err error) {
	fromAddr, err := mail.ParseAddress(m.From)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("--from %q is not an address: %v", m.From, err)
	}
	var toList []string
	for _, t := range m.To {
		a, err := mail.ParseAddress(t)
		if err != nil {
			return nil, "", "", nil, fmt.Errorf("--to %q is not an address: %v", t, err)
		}
		rcpts = append(rcpts, a.Address)
		toList = append(toList, a.String())
	}
	_, domain, _ := strings.Cut(fromAddr.Address, "@")
	id := make([]byte, 12)
	rand.Read(id)
	msgID = "<" + hex.EncodeToString(id) + ".dev-deliver@" + domain + ">"

	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", fromAddr.String())
	h("To", strings.Join(toList, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", m.Date.Format(time.RFC1123Z))
	h("Message-ID", msgID)
	if m.InReplyTo != "" {
		ref := strings.TrimSpace(m.InReplyTo)
		if !strings.HasPrefix(ref, "<") {
			ref = "<" + strings.Trim(ref, "<>") + ">"
		}
		h("In-Reply-To", ref)
		h("References", ref)
	}
	h("MIME-Version", "1.0")

	textPart := func(w io.Writer) error {
		qp := quotedprintable.NewWriter(w)
		if _, err := io.WriteString(qp, strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")); err != nil {
			return err
		}
		return qp.Close()
	}
	if len(m.Attach) == 0 {
		h("Content-Type", "text/plain; charset=utf-8")
		h("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := textPart(&b); err != nil {
			return nil, "", "", nil, err
		}
		return crlf(b.Bytes()), msgID, fromAddr.Address, rcpts, nil
	}
	mw := multipart.NewWriter(&b)
	h("Content-Type", `multipart/mixed; boundary="`+mw.Boundary()+`"`)
	b.WriteString("\r\nThis is a multi-part message in MIME format.\r\n")
	pw, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"text/plain; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
	if err != nil {
		return nil, "", "", nil, err
	}
	if err := textPart(pw); err != nil {
		return nil, "", "", nil, err
	}
	for _, path := range m.Attach {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", "", nil, fmt.Errorf("--attach: %w", err)
		}
		name := filepath.Base(path)
		ctype := mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {mime.FormatMediaType(ctype, map[string]string{"name": name})},
			"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": name})},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return nil, "", "", nil, err
		}
		enc := base64.StdEncoding.EncodeToString(data)
		for len(enc) > 76 {
			io.WriteString(pw, enc[:76]+"\r\n")
			enc = enc[76:]
		}
		io.WriteString(pw, enc+"\r\n")
	}
	if err := mw.Close(); err != nil {
		return nil, "", "", nil, err
	}
	return crlf(b.Bytes()), msgID, fromAddr.Address, rcpts, nil
}

// crlf turns lone LFs into CRLF, as SMTP needs.
func crlf(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
}

// sendDev hands the message to the inbound node at addr in one SMTP
// transaction, with STARTTLS when the node offers it.
func sendDev(ctx context.Context, addr, from string, rcpts []string, msg []byte) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("cannot reach an inbound node at %s (%v); start one (docker compose --profile inbound up -d, or openmailwire inbound) or pass --inbound", addr, err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Minute))
	host, _, _ := net.SplitHostPort(addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: %w", addr, err)
	}
	defer c.Close()
	if err := c.Hello("dev-deliver.localhost"); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		// Development only: local nodes use self-signed certificates.
		if err := c.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: true}); err != nil { //nolint:gosec
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("the inbound node refused the sender %s: %w", from, err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			hint := ""
			var te *textproto.Error
			if errors.As(err, &te) && te.Code == 550 {
				hint = fmt.Sprintf("; create the inbox first (POST /inboxes {\"address\": %q}) on a domain of the project", r)
			}
			return fmt.Errorf("the inbound node refused %s: %w%s", r, err, hint)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the inbound node did not accept the message: %w", err)
	}
	return c.Quit()
}
