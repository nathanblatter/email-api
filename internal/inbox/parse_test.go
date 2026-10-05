package inbox

import (
	"strings"
	"testing"
)

const sample = "From: \"Jane Doe\" <jane@example.com>\r\n" +
	"To: Nathan <nathan@nathanblatter.com>, hello@nathanblatter.com\r\n" +
	"Cc: cc@example.com\r\n" +
	"Subject: =?utf-8?q?Caf=C3=A9_meeting?=\r\n" +
	"Date: Mon, 05 Oct 2026 10:00:00 -0600\r\n" +
	"Message-ID: <abc123@example.com>\r\n" +
	"In-Reply-To: <prev@nathanblatter.com>\r\n" +
	"Authentication-Results: mx.cloudflare.net; dkim=pass header.d=example.com; spf=pass smtp.mailfrom=example.com; dmarc=pass\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"Hi Nathan, see the caf=C3=A9 notes attached.\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>Hi Nathan, see the <b>café</b> notes attached.</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"notes.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"Content-Disposition: attachment; filename=\"notes.pdf\"\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\nJWVuZG9i\r\n" +
	"--outer\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"Content-ID: <logo>\r\n" +
	"Content-Disposition: inline\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--outer--\r\n"

func TestParseMultipart(t *testing.T) {
	p, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if p.From.Name != "Jane Doe" || p.From.Address != "jane@example.com" {
		t.Fatalf("from %+v", p.From)
	}
	if len(p.To) != 2 || p.To[1].Address != "hello@nathanblatter.com" || len(p.Cc) != 1 {
		t.Fatalf("to %v cc %v", p.To, p.Cc)
	}
	if p.Subject != "Café meeting" || p.MessageID != "<abc123@example.com>" || p.InReplyTo != "<prev@nathanblatter.com>" {
		t.Fatalf("hdrs %q %q %q", p.Subject, p.MessageID, p.InReplyTo)
	}
	if p.Date.IsZero() || p.Date.Year() != 2026 {
		t.Fatalf("date %v", p.Date)
	}
	if !strings.Contains(p.Text, "café notes") || !strings.Contains(p.HTML, "<b>café</b>") {
		t.Fatalf("bodies %q / %q", p.Text, p.HTML)
	}
	if p.SPF != "pass" || p.DKIM != "pass" || p.DMARC != "pass" || p.Suspicious() {
		t.Fatalf("auth %s %s %s", p.SPF, p.DKIM, p.DMARC)
	}
	if len(p.Attachments) != 2 {
		t.Fatalf("attachments %+v", p.Attachments)
	}
	pdf, logo := p.Attachments[0], p.Attachments[1]
	if pdf.Filename != "notes.pdf" || pdf.ContentType != "application/pdf" || string(pdf.Content) != "%PDF-1.4\n%endob" || pdf.Inline {
		t.Fatalf("pdf %+v %q", pdf, pdf.Content)
	}
	if logo.ContentID != "logo" || !logo.Inline || logo.Filename != "attachment.png" || len(logo.Content) != 8 {
		t.Fatalf("logo %+v", logo)
	}
	if p.Headers["authentication-results"] == "" || p.Headers["from"] == "" {
		t.Fatalf("kept headers %v", p.Headers)
	}
}

func TestParsePlainAndHTMLOnly(t *testing.T) {
	p, err := Parse([]byte("From: a@b.c\r\nSubject: plain\r\n\r\njust text\r\n"))
	if err != nil || p.Text != "just text\r\n" || p.HTML != "" || len(p.Attachments) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if !p.Suspicious() {
		t.Fatal("no auth results should be suspicious")
	}
	p, _ = Parse([]byte("From: a@b.c\r\nContent-Type: text/html\r\n\r\n<html><style>x{}</style><p>Hello<br>World &amp; more</p></html>"))
	if p.Text != "Hello\nWorld & more" {
		t.Fatalf("html fallback %q", p.Text)
	}
}

func TestParseLatin1AndBadAddresses(t *testing.T) {
	raw := "From: Bad Sender\r\nTo: nathan@nathanblatter.com\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\ncaf\xe9\r\n"
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.From == nil || p.From.Address != "Bad Sender" {
		t.Fatalf("from %+v", p.From)
	}
	if !strings.HasPrefix(p.Text, "café") {
		t.Fatalf("latin1 %q", p.Text)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("no headers here\x00")); err == nil {
		t.Fatal("expected error")
	}
}
