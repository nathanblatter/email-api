// Package inbox receives mail. Cloudflare Email Routing hands each inbound
// message to a small Email Worker, which POSTs the raw RFC 5322 bytes to the
// public listener; this package parses it and the store keeps the message
// in Postgres and its attachments (and the raw .eml) in MinIO.
package inbox

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	stdmail "net/mail"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Parsed is the structured form of one inbound message.
type Parsed struct {
	MessageID   string
	InReplyTo   string
	References  string
	From        *stdmail.Address
	To          []*stdmail.Address
	Cc          []*stdmail.Address
	ReplyTo     *stdmail.Address
	Subject     string
	Date        time.Time
	Text        string
	HTML        string
	Headers     map[string]string // selected headers, lower-cased names
	Attachments []Attachment
	// Authentication results as reported by the receiving edge (Cloudflare).
	SPF, DKIM, DMARC string
}

type Attachment struct {
	Filename    string
	ContentType string
	ContentID   string
	Inline      bool
	Content     []byte
}

// keptHeaders are stored verbatim (lower-cased keys) for forensics/threading.
var keptHeaders = []string{"message-id", "in-reply-to", "references", "date", "from", "to", "cc", "reply-to",
	"subject", "list-unsubscribe", "list-id", "authentication-results", "received-spf", "x-mailer", "user-agent",
	"x-priority", "importance", "auto-submitted", "precedence"}

// Parse decodes raw message bytes. It never fails on an odd body: a message
// that cannot be decoded still yields headers and an empty body.
func Parse(raw []byte) (*Parsed, error) {
	msg, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse headers: %w", err)
	}
	dec := mime.WordDecoder{CharsetReader: charsetReader}
	hdr := func(k string) string {
		v := msg.Header.Get(k)
		if d, err := dec.DecodeHeader(v); err == nil {
			return d
		}
		return v
	}
	p := &Parsed{
		MessageID:  strings.TrimSpace(msg.Header.Get("Message-Id")),
		InReplyTo:  strings.TrimSpace(msg.Header.Get("In-Reply-To")),
		References: strings.TrimSpace(msg.Header.Get("References")),
		Subject:    hdr("Subject"),
		Headers:    map[string]string{},
	}
	for _, k := range keptHeaders {
		if v := msg.Header.Get(k); v != "" {
			p.Headers[k] = hdr(k)
		}
	}
	p.From = parseOne(hdr("From"))
	p.ReplyTo = parseOne(hdr("Reply-To"))
	p.To = parseMany(hdr("To"))
	p.Cc = parseMany(hdr("Cc"))
	if d, err := msg.Header.Date(); err == nil {
		p.Date = d
	}
	p.SPF, p.DKIM, p.DMARC = authResults(msg.Header.Get("Authentication-Results"))

	walk(p, msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Header.Get("Content-Disposition"), msg.Header.Get("Content-ID"), msg.Body, 0)
	if p.Text == "" && p.HTML != "" {
		p.Text = htmlToText(p.HTML)
	}
	return p, nil
}

func parseOne(s string) *stdmail.Address {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	a, err := stdmail.ParseAddress(s)
	if err != nil {
		// Keep whatever we got so the inbox is never blind to the sender.
		return &stdmail.Address{Address: strings.TrimSpace(s)}
	}
	return a
}

func parseMany(s string) []*stdmail.Address {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	as, err := stdmail.ParseAddressList(s)
	if err != nil {
		return []*stdmail.Address{{Address: strings.TrimSpace(s)}}
	}
	return as
}

var authRe = regexp.MustCompile(`(?i)\b(spf|dkim|dmarc)=([a-z]+)`)

func authResults(h string) (spf, dkim, dmarc string) {
	for _, m := range authRe.FindAllStringSubmatch(h, -1) {
		switch strings.ToLower(m[1]) {
		case "spf":
			if spf == "" {
				spf = strings.ToLower(m[2])
			}
		case "dkim":
			if dkim == "" {
				dkim = strings.ToLower(m[2])
			}
		case "dmarc":
			if dmarc == "" {
				dmarc = strings.ToLower(m[2])
			}
		}
	}
	return
}

// Suspicious reports whether the edge saw neither SPF nor DKIM pass. Such mail
// is kept but flagged so list views can hide it by default.
func (p *Parsed) Suspicious() bool {
	return p.SPF != "pass" && p.DKIM != "pass"
}

const maxDepth = 12

func walk(p *Parsed, ctype, cte, cdisp, cid string, body io.Reader, depth int) {
	if depth > maxDepth {
		return
	}
	if ctype == "" {
		ctype = "text/plain; charset=us-ascii"
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt, params = "application/octet-stream", map[string]string{}
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			walk(p, part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"),
				part.Header.Get("Content-Disposition"), part.Header.Get("Content-ID"), part, depth+1)
		}
	}
	data, err := io.ReadAll(decodeCTE(body, cte))
	if err != nil && len(data) == 0 {
		return
	}
	disp, dparams, _ := mime.ParseMediaType(cdisp)
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	if d, err := (&mime.WordDecoder{CharsetReader: charsetReader}).DecodeHeader(filename); err == nil {
		filename = d
	}
	isText := mt == "text/plain" || mt == "text/html"
	if isText && disp != "attachment" && filename == "" {
		s := decodeCharset(data, params["charset"])
		if mt == "text/plain" {
			if p.Text == "" {
				p.Text = s
			}
		} else if p.HTML == "" {
			p.HTML = s
		}
		return
	}
	if mt == "message/rfc822" && filename == "" {
		filename = "forwarded.eml"
	}
	if filename == "" {
		filename = "attachment" + extFor(mt)
	}
	p.Attachments = append(p.Attachments, Attachment{
		Filename:    filename,
		ContentType: mt,
		ContentID:   strings.Trim(cid, "<>"),
		Inline:      disp == "inline" || cid != "",
		Content:     data,
	})
}

func extFor(mt string) string {
	if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

func decodeCTE(r io.Reader, cte string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &b64Cleaner{r: r})
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// b64Cleaner strips whitespace so line-wrapped base64 decodes cleanly.
type b64Cleaner struct {
	r   io.Reader
	buf []byte
}

func (c *b64Cleaner) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		tmp := make([]byte, 4096)
		n, err := c.r.Read(tmp)
		for _, b := range tmp[:n] {
			if b != '\r' && b != '\n' && b != ' ' && b != '\t' {
				c.buf = append(c.buf, b)
			}
		}
		if err != nil {
			if len(c.buf) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "utf-8", "us-ascii", "ascii", "":
		return input, nil
	case "iso-8859-1", "latin1", "windows-1252", "cp1252":
		return transform.NewReader(input, charmap.Windows1252.NewDecoder()), nil
	case "iso-8859-15":
		return transform.NewReader(input, charmap.ISO8859_15.NewDecoder()), nil
	case "utf-16", "utf-16le":
		return transform.NewReader(input, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()), nil
	case "utf-16be":
		return transform.NewReader(input, unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder()), nil
	}
	return input, nil // unknown charset: pass bytes through rather than fail
}

func decodeCharset(data []byte, charset string) string {
	r, _ := charsetReader(charset, bytes.NewReader(data))
	out, err := io.ReadAll(r)
	if err != nil {
		return string(data)
	}
	return strings.ToValidUTF8(string(out), "�")
}

var (
	tagRe    = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</\s*(script|style)\s*>`)
	brRe     = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/tr|/li|/h[1-6])\s*/?>`)
	anyTagRe = regexp.MustCompile(`<[^>]+>`)
	spaceRe  = regexp.MustCompile(`[ \t]+\n`)
	nlRe     = regexp.MustCompile(`\n{3,}`)
)

// htmlToText is a rough fallback for HTML-only mail so search and previews
// have something to work with.
func htmlToText(h string) string {
	s := tagRe.ReplaceAllString(h, "")
	s = brRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	s = spaceRe.ReplaceAllString(s, "\n")
	s = nlRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
