package mail

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	stdmail "net/mail"
	"strings"
	"testing"
)

var pol = Policy{DefaultFrom: "Nathan Blatter <noreply@nathanblatter.com>", AllowedDomains: []string{"nathanblatter.com"}, MaxBytes: 1 << 20}

func TestAddrListAcceptsStringOrArray(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"to":"a@x.com, B <b@y.com>","cc":["c@z.com"]}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.To) != 1 || len(m.Cc) != 1 {
		t.Fatalf("got %+v", m)
	}
	p, err := Prepare(&Message{To: m.To, Cc: m.Cc, Subject: "s", Text: "t"}, pol)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Recipients(); len(got) != 3 || got[1] != "b@y.com" {
		t.Fatalf("recipients %v", got)
	}
}

func TestFromPolicy(t *testing.T) {
	base := Message{To: AddrList{"a@x.com"}, Subject: "hi", Text: "body"}

	p, err := Prepare(&base, pol)
	if err != nil {
		t.Fatal(err)
	}
	if p.From.Address != "noreply@nathanblatter.com" || p.From.Name != "Nathan Blatter" {
		t.Fatalf("default from = %v", p.From)
	}

	m := base
	m.From = "Nate <nate@nathanblatter.com>"
	if p, err = Prepare(&m, pol); err != nil || p.From.Address != "nate@nathanblatter.com" {
		t.Fatalf("custom from: %v %v", p, err)
	}

	m.From = "evil@gmail.com"
	_, err = Prepare(&m, pol)
	var ve *ValidationError
	if err == nil || !strings.Contains(err.Error(), "allowed domain") {
		t.Fatalf("expected domain rejection, got %v", err)
	}
	if !asValidation(err, &ve) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
}

func asValidation(err error, ve **ValidationError) bool {
	v, ok := err.(*ValidationError)
	if ok {
		*ve = v
	}
	return ok
}

func TestValidationRules(t *testing.T) {
	cases := map[string]Message{
		"no recipients": {Subject: "s", Text: "t"},
		"no subject":    {To: AddrList{"a@x.com"}, Text: "t"},
		"no body":       {To: AddrList{"a@x.com"}, Subject: "s"},
		"bad address":   {To: AddrList{"not an address"}, Subject: "s", Text: "t"},
		"reserved hdr":  {To: AddrList{"a@x.com"}, Subject: "s", Text: "t", Headers: map[string]string{"Bcc": "x@y.com"}},
		"hdr injection": {To: AddrList{"a@x.com"}, Subject: "s", Text: "t", Headers: map[string]string{"X-A": "v\r\nBcc: x@y.com"}},
		"subject nl":    {To: AddrList{"a@x.com"}, Subject: "s\nBcc: x@y.com", Text: "t"},
		"empty attach":  {To: AddrList{"a@x.com"}, Subject: "s", Text: "t", Attachments: []Attachment{{Filename: "a.txt"}}},
	}
	for name, m := range cases {
		if _, err := Prepare(&m, pol); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBuildsMIMEWithAlternativesInlineAndAttachments(t *testing.T) {
	m := &Message{
		From: "Nathan <nathan@nathanblatter.com>", To: AddrList{"A <a@x.com>"}, Cc: AddrList{"c@x.com"}, Bcc: AddrList{"secret@x.com"},
		ReplyTo: "reply@nathanblatter.com", Subject: "Hällo wörld", Text: "plain ünïcode", HTML: `<p><img src="cid:logo"></p>`,
		Attachments: []Attachment{
			{Filename: "logo.png", Content: []byte("\x89PNG....."), ContentID: "logo"},
			{Filename: "report.pdf", Content: bytes.Repeat([]byte("x"), 200)},
		},
		Headers: map[string]string{"X-Priority": "1", "In-Reply-To": "<abc@x.com>"},
	}
	p, err := Prepare(m, pol)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(p.Raw)
	if strings.Contains(raw, "secret@x.com") {
		t.Fatal("bcc leaked into headers")
	}
	if !strings.Contains(raw, "X-Priority: 1") || !strings.Contains(raw, "In-Reply-To: <abc@x.com>") {
		t.Fatal("custom headers missing")
	}
	msg, err := stdmail.ReadMessage(bytes.NewReader(p.Raw))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	if s, _ := dec.DecodeHeader(msg.Header.Get("Subject")); s != "Hällo wörld" {
		t.Fatalf("subject %q", s)
	}
	if msg.Header.Get("Reply-To") != "<reply@nathanblatter.com>" {
		t.Fatalf("reply-to %q", msg.Header.Get("Reply-To"))
	}
	if !strings.HasPrefix(msg.Header.Get("Message-Id"), "<"+p.ID+"@nathanblatter.com>") {
		t.Fatalf("message-id %q", msg.Header.Get("Message-Id"))
	}

	// Walk the tree: mixed → [related → [alternative → [text, html], inline png], pdf]
	kinds := walk(t, msg.Header.Get("Content-Type"), msg.Body)
	want := []string{"multipart/mixed", "multipart/related", "multipart/alternative", "text/plain", "text/html", "image/png", "application/pdf"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("tree = %v", kinds)
	}
}

func walk(t *testing.T, ctype string, body io.Reader) []string {
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{mt}
	if !strings.HasPrefix(mt, "multipart/") {
		return out
	}
	mr := multipart.NewReader(body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, walk(t, part.Header.Get("Content-Type"), part)...)
	}
}

func TestSizeLimit(t *testing.T) {
	m := &Message{To: AddrList{"a@x.com"}, Subject: "s", Text: "t",
		Attachments: []Attachment{{Filename: "big.bin", Content: bytes.Repeat([]byte{1}, 1<<20)}}}
	if _, err := Prepare(m, pol); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size error, got %v", err)
	}
}

func TestBase64Lines(t *testing.T) {
	for n := 0; n < 200; n++ {
		in := bytes.Repeat([]byte{byte(n)}, n)
		got := string(base64Lines(in))
		for _, line := range strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n") {
			if len(line) > 76 {
				t.Fatalf("n=%d line too long", n)
			}
		}
		var back []byte
		if err := json.Unmarshal([]byte(`"`+strings.ReplaceAll(got, "\r\n", "")+`"`), &back); err != nil || !bytes.Equal(back, in) {
			t.Fatalf("n=%d roundtrip failed: %v", n, err)
		}
	}
}
