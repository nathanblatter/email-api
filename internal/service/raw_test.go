package service

import (
	"strings"
	"testing"
)

func TestStripBcc(t *testing.T) {
	in := "From: a@nathanblatter.com\r\nBcc: hidden@example.com,\r\n other@example.com\r\nTo: b@example.com\r\nSubject: hi\r\n\r\nBcc: this line is body\r\n"
	out := string(StripBcc([]byte(in)))
	if strings.Contains(out, "hidden@") || strings.Contains(out, "other@") {
		t.Fatalf("bcc header kept: %q", out)
	}
	want := "From: a@nathanblatter.com\r\nTo: b@example.com\r\nSubject: hi\r\n\r\nBcc: this line is body\r\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}
