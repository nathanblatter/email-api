// Package mail turns an API-level Message into an RFC 5322 envelope + MIME
// body and validates it against the sender policy (allowed From domains).
package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	stdmail "net/mail"
	"net/textproto"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AddrList accepts either a JSON string ("a@x, B <b@y>") or an array of
// strings, so callers can pass whichever is convenient.
type AddrList []string

func (a *AddrList) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*a = nil
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			*a = nil
			return nil
		}
		*a = AddrList{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("expected a string or array of strings")
	}
	*a = AddrList(list)
	return nil
}

// Attachment content is base64 in JSON ([]byte marshals that way natively).
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Content     []byte `json:"content"`
	// ContentID makes the part inline (multipart/related) so HTML can
	// reference it as <img src="cid:...">.
	ContentID string `json:"content_id,omitempty"`
	// AsLink uploads the file and puts an expiring download link in the body
	// instead of attaching it. Also done automatically when the message would
	// exceed the size limit.
	AsLink bool `json:"as_link,omitempty"`
}

// EncodedSize estimates how many bytes the message will occupy once MIME
// encoded (base64 inflates attachments by 4/3 plus line breaks).
func (m *Message) EncodedSize() int64 {
	n := int64(len(m.Text)+len(m.HTML)) + 2048
	for _, a := range m.Attachments {
		n += int64(len(a.Content))*4/3 + int64(len(a.Content))/57*2 + 256
	}
	return n
}

// Message is the wire format for POST /send and the send_email MCP tool.
type Message struct {
	From        string            `json:"from,omitempty"`
	To          AddrList          `json:"to"`
	Cc          AddrList          `json:"cc,omitempty"`
	Bcc         AddrList          `json:"bcc,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text,omitempty"`
	HTML        string            `json:"html,omitempty"`
	Attachments []Attachment      `json:"attachments,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// Policy is what Prepare validates against.
type Policy struct {
	DefaultFrom    string
	AllowedDomains []string // lower-case
	MaxBytes       int64    // 0 = unlimited
}

// Prepared is a validated message with its SMTP envelope and raw bytes.
type Prepared struct {
	ID        string
	From      *stdmail.Address
	To        []*stdmail.Address
	Cc        []*stdmail.Address
	Bcc       []*stdmail.Address
	Subject   string
	Text      string // kept for the iMessage fallback summary
	Raw       []byte
	CreatedAt time.Time
}

// Recipients is the deduplicated envelope recipient list (To + Cc + Bcc).
func (p *Prepared) Recipients() []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range [][]*stdmail.Address{p.To, p.Cc, p.Bcc} {
		for _, a := range group {
			k := strings.ToLower(a.Address)
			if !seen[k] {
				seen[k] = true
				out = append(out, a.Address)
			}
		}
	}
	return out
}

// ValidationError is a caller mistake (HTTP 400), as opposed to a delivery
// failure.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// reservedHeaders cannot be overridden through Message.Headers; they are
// derived from the structured fields so the envelope and headers never
// disagree.
var reservedHeaders = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "subject": true,
	"reply-to": true, "date": true, "message-id": true, "mime-version": true,
	"content-type": true, "content-transfer-encoding": true, "content-disposition": true,
}

// Prepare validates m against pol and builds the raw RFC 5322 message.
func Prepare(m *Message, pol Policy) (*Prepared, error) {
	if m == nil {
		return nil, invalid("empty message")
	}
	fromStr := strings.TrimSpace(m.From)
	if fromStr == "" {
		fromStr = pol.DefaultFrom
	}
	from, err := stdmail.ParseAddress(fromStr)
	if err != nil {
		return nil, invalid("invalid from %q: %v", m.From, err)
	}
	if !domainAllowed(from.Address, pol.AllowedDomains) {
		return nil, invalid("from %q must be under an allowed domain (%s)", from.Address, strings.Join(pol.AllowedDomains, ", "))
	}
	to, err := parseList(m.To, "to")
	if err != nil {
		return nil, err
	}
	cc, err := parseList(m.Cc, "cc")
	if err != nil {
		return nil, err
	}
	bcc, err := parseList(m.Bcc, "bcc")
	if err != nil {
		return nil, err
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return nil, invalid("at least one recipient (to, cc or bcc) is required")
	}
	var replyTo *stdmail.Address
	if rt := strings.TrimSpace(m.ReplyTo); rt != "" {
		if replyTo, err = stdmail.ParseAddress(rt); err != nil {
			return nil, invalid("invalid reply_to %q: %v", m.ReplyTo, err)
		}
	}
	if strings.TrimSpace(m.Subject) == "" {
		return nil, invalid("subject is required")
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return nil, invalid("subject must be a single line")
	}
	if strings.TrimSpace(m.Text) == "" && strings.TrimSpace(m.HTML) == "" {
		return nil, invalid("text or html body is required")
	}
	for i, a := range m.Attachments {
		if strings.TrimSpace(a.Filename) == "" {
			return nil, invalid("attachments[%d]: filename is required", i)
		}
		if len(a.Content) == 0 {
			return nil, invalid("attachments[%d] (%s): content is empty", i, a.Filename)
		}
	}
	for k := range m.Headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" || reservedHeaders[lk] {
			return nil, invalid("header %q cannot be set directly", k)
		}
		if strings.ContainsAny(k+m.Headers[k], "\r\n") {
			return nil, invalid("header %q must be a single line", k)
		}
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	p := &Prepared{
		ID: id, From: from, To: to, Cc: cc, Bcc: bcc,
		Subject: m.Subject, Text: m.Text, CreatedAt: now,
	}
	p.Raw = build(m, p, replyTo, now)
	if pol.MaxBytes > 0 && int64(len(p.Raw)) > pol.MaxBytes {
		return nil, invalid("message is %d bytes, over the %d byte limit", len(p.Raw), pol.MaxBytes)
	}
	return p, nil
}

func domainAllowed(addr string, allowed []string) bool {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return false
	}
	d := strings.ToLower(addr[at+1:])
	for _, a := range allowed {
		if d == a {
			return true
		}
	}
	return false
}

func parseList(list AddrList, field string) ([]*stdmail.Address, error) {
	var out []*stdmail.Address
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		addrs, err := stdmail.ParseAddressList(raw)
		if err != nil {
			return nil, invalid("invalid %s address %q: %v", field, raw, err)
		}
		out = append(out, addrs...)
	}
	return out, nil
}

func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ── MIME construction ───────────────────────────────────────────────────────

type part struct {
	header textproto.MIMEHeader
	body   []byte
}

func build(m *Message, p *Prepared, replyTo *stdmail.Address, now time.Time) []byte {
	// Body tree: alternative(text, html) → related(+inline) → mixed(+files).
	var bodies []part
	if strings.TrimSpace(m.Text) != "" {
		bodies = append(bodies, textPart("text/plain", m.Text))
	}
	if strings.TrimSpace(m.HTML) != "" {
		bodies = append(bodies, textPart("text/html", m.HTML))
	}
	body := bodies[0]
	if len(bodies) == 2 {
		body = container("multipart/alternative", bodies)
	}

	var inline, files []part
	for _, a := range m.Attachments {
		if a.ContentID != "" {
			inline = append(inline, attachmentPart(a, true))
		} else {
			files = append(files, attachmentPart(a, false))
		}
	}
	if len(inline) > 0 {
		body = container("multipart/related", append([]part{body}, inline...))
	}
	if len(files) > 0 {
		body = container("multipart/mixed", append([]part{body}, files...))
	}

	domain := "localhost"
	if at := strings.LastIndex(p.From.Address, "@"); at >= 0 {
		domain = p.From.Address[at+1:]
	}

	h := textproto.MIMEHeader{}
	h.Set("From", p.From.String())
	if len(p.To) > 0 {
		h.Set("To", joinAddrs(p.To))
	}
	if len(p.Cc) > 0 {
		h.Set("Cc", joinAddrs(p.Cc))
	}
	if replyTo != nil {
		h.Set("Reply-To", replyTo.String())
	}
	h.Set("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h.Set("Date", now.Format(time.RFC1123Z))
	h.Set("Message-ID", fmt.Sprintf("<%s@%s>", p.ID, domain))
	h.Set("MIME-Version", "1.0")
	h.Set("X-Mailer", "email-api")
	for k, v := range m.Headers {
		h.Set(k, v)
	}
	for k, vs := range body.header {
		h[k] = vs
	}

	var buf bytes.Buffer
	writeHeader(&buf, h)
	buf.WriteString("\r\n")
	buf.Write(body.body)
	return buf.Bytes()
}

func joinAddrs(as []*stdmail.Address) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

func textPart(ctype, text string) part {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", ctype+"; charset=utf-8")
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	return part{header: h, body: buf.Bytes()}
}

func attachmentPart(a Attachment, inline bool) part {
	ctype := strings.TrimSpace(a.ContentType)
	if ctype == "" {
		ctype = mime.TypeByExtension(strings.ToLower(filepath.Ext(a.Filename)))
	}
	if ctype == "" {
		ctype = http.DetectContentType(a.Content)
	}
	// Both lookups may return parameters ("text/plain; charset=utf-8");
	// split them so the name parameter can be added alongside.
	mediaType, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mediaType, params = "application/octet-stream", map[string]string{}
	}
	params["name"] = a.Filename
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	h.Set("Content-Transfer-Encoding", "base64")
	disp := "attachment"
	if inline {
		disp = "inline"
		h.Set("Content-ID", "<"+strings.Trim(a.ContentID, "<>")+">")
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": a.Filename}))
	return part{header: h, body: base64Lines(a.Content)}
}

// container wraps parts in a multipart body of the given subtype.
func container(ctype string, parts []part) part {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		w, _ := mw.CreatePart(p.header)
		_, _ = w.Write(p.body)
	}
	_ = mw.Close()
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", fmt.Sprintf("%s; boundary=%q", ctype, mw.Boundary()))
	return part{header: h, body: buf.Bytes()}
}

func writeHeader(buf *bytes.Buffer, h textproto.MIMEHeader) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(buf, "%s: %s\r\n", k, v)
		}
	}
}

func base64Lines(b []byte) []byte {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out bytes.Buffer
	var line [76]byte
	n := 0
	emit := func(c byte) {
		line[n] = c
		n++
		if n == 76 {
			out.Write(line[:])
			out.WriteString("\r\n")
			n = 0
		}
	}
	for i := 0; i < len(b); i += 3 {
		var v uint32
		rem := len(b) - i
		v = uint32(b[i]) << 16
		if rem > 1 {
			v |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			v |= uint32(b[i+2])
		}
		emit(enc[v>>18&63])
		emit(enc[v>>12&63])
		if rem > 1 {
			emit(enc[v>>6&63])
		} else {
			emit('=')
		}
		if rem > 2 {
			emit(enc[v&63])
		} else {
			emit('=')
		}
	}
	if n > 0 {
		out.Write(line[:n])
		out.WriteString("\r\n")
	}
	return out.Bytes()
}
