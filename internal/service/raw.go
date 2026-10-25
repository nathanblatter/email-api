package service

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	stdmail "net/mail"
	"strings"
	"time"

	"github.com/nathanblatter/email-api/internal/mail"
)

// SendRaw delivers a message a mail client already composed (SMTP submission
// from Apple Mail via the mail gateway). The bytes go to the relay unchanged
// except that any Bcc header is removed; the From domain and size limit are
// enforced exactly like Send, and the budget, spool and pager apply.
func (s *Service) SendRaw(ctx context.Context, from string, rcpts []string, raw []byte) (Result, error) {
	bad := func(format string, a ...any) (Result, error) {
		err := &mail.ValidationError{Msg: fmt.Sprintf(format, a...)}
		return Result{Status: "rejected", Error: err.Error()}, err
	}
	fa, err := stdmail.ParseAddress(from)
	if err != nil {
		return bad("invalid from address %q", from)
	}
	if !domainAllowed(fa.Address, s.opt.Policy.AllowedDomains, s.opt.UnmeteredDomains) {
		return bad("from domain not allowed: %s", fa.Address)
	}
	if len(rcpts) == 0 {
		return bad("no recipients")
	}
	var to []*stdmail.Address
	for _, r := range rcpts {
		a, err := stdmail.ParseAddress(r)
		if err != nil {
			return bad("invalid recipient %q", r)
		}
		to = append(to, a)
	}
	raw = StripBcc(raw)
	if max := s.opt.Policy.MaxBytes; max > 0 && int64(len(raw)) > max {
		return bad("message is %d bytes, over the %d byte limit", len(raw), max)
	}
	msg, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return bad("not an RFC 5322 message: %v", err)
	}
	hf, err := stdmail.ParseAddress(msg.Header.Get("From"))
	if err != nil || !strings.EqualFold(hf.Address, fa.Address) {
		return bad("From header must match the envelope sender %s", fa.Address)
	}
	subject := msg.Header.Get("Subject")
	if dec, err := new(mimeDecoder).DecodeHeader(subject); err == nil {
		subject = dec
	}
	id, err := rawID()
	if err != nil {
		return Result{Status: "rejected", Error: err.Error()}, err
	}
	p := &mail.Prepared{ID: id, From: fa, To: to, Subject: subject, Text: "(composed in a mail client)", Raw: raw, CreatedAt: time.Now()}
	return s.deliver(ctx, p)
}

func domainAllowed(addr string, lists ...[]string) bool {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return false
	}
	d := strings.ToLower(addr[at+1:])
	for _, l := range lists {
		for _, a := range l {
			if strings.EqualFold(a, d) {
				return true
			}
		}
	}
	return false
}

// StripBcc removes Bcc header fields (and their folded continuation lines)
// from the header block, leaving the body untouched.
func StripBcc(raw []byte) []byte {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	sep := 4
	if end < 0 {
		end = bytes.Index(raw, []byte("\n\n"))
		sep = 2
	}
	if end < 0 {
		return raw
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(raw[:end]))
	sc.Buffer(make([]byte, 0, 64*1024), len(raw)+1)
	skipping := false
	nl := "\r\n"
	if sep == 2 {
		nl = "\n"
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line != "" && (line[0] == ' ' || line[0] == '\t') {
			if !skipping {
				out.WriteString(line + nl)
			}
			continue
		}
		skipping = len(line) >= 4 && strings.EqualFold(line[:4], "bcc:")
		if !skipping {
			out.WriteString(line + nl)
		}
	}
	out.Write(raw[end+sep-len(nl):])
	return out.Bytes()
}
