package service

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nathanblatter/email-api/internal/files"
	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/quota"
	"github.com/nathanblatter/email-api/internal/spool"
)

func setupLinks(t *testing.T, store files.Store, maxBytes int64) (*Service, *fakeSender) {
	t.Helper()
	sp, _ := spool.Open(t.TempDir())
	snd := &fakeSender{}
	svc := New(snd, nil, sp, quota.New(100, ""), store, slog.Default(), Options{
		Policy:       mail.Policy{DefaultFrom: "noreply@nathanblatter.com", AllowedDomains: []string{"nathanblatter.com"}, MaxBytes: maxBytes},
		MaxLinkBytes: 1 << 20,
	})
	return svc, snd
}

func memStore() *files.Memory {
	return &files.Memory{PublicURL: "https://file.nathanblatter.com", TTL: 24 * time.Hour, Now: func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }}
}

func TestExplicitAsLink(t *testing.T) {
	store := memStore()
	svc, snd := setupLinks(t, store, 1<<20)
	m := msg("a@x.com")
	m.HTML = "<p>hi</p>"
	m.Attachments = []mail.Attachment{{Filename: "big.bin", Content: []byte("data"), AsLink: true}, {Filename: "small.txt", Content: []byte("x")}}
	r, err := svc.Send(context.Background(), m)
	if err != nil || r.Status != "sent" || len(r.Links) != 1 || r.Links[0].Filename != "big.bin" {
		t.Fatalf("%+v %v", r, err)
	}
	raw := string(snd.raws[0])
	if !strings.Contains(raw, "filename=small.txt") || strings.Contains(raw, "filename=big.bin") {
		t.Fatalf("attachments wrong:\n%s", raw)
	}
	if !strings.Contains(raw, "/f/") || !strings.Contains(raw, "big.bin") {
		t.Fatalf("link missing from body:\n%s", raw)
	}
	if !strings.Contains(m.Text, "available until Oct 6, 2026") || !strings.Contains(m.HTML, "<a href=\"https://file.nathanblatter.com/f/") {
		t.Fatalf("bodies: %q %q", m.Text, m.HTML)
	}
	if len(store.Objects) != 1 {
		t.Fatal("expected one stored object")
	}
}

func TestOversizedAttachmentsBecomeLinksLargestFirst(t *testing.T) {
	store := memStore()
	svc, snd := setupLinks(t, store, 64<<10) // 64 KB message limit
	m := msg("a@x.com")
	m.Attachments = []mail.Attachment{
		{Filename: "huge.pdf", Content: bytes.Repeat([]byte("h"), 100<<10)},
		{Filename: "mid.png", Content: bytes.Repeat([]byte("m"), 20<<10)},
		{Filename: "tiny.txt", Content: []byte("tiny")},
	}
	r, err := svc.Send(context.Background(), m)
	if err != nil || r.Status != "sent" {
		t.Fatalf("%+v %v", r, err)
	}
	if len(r.Links) != 1 || r.Links[0].Filename != "huge.pdf" {
		t.Fatalf("links %+v", r.Links)
	}
	raw := string(snd.raws[0])
	if !strings.Contains(raw, "filename=mid.png") || !strings.Contains(raw, "filename=tiny.txt") {
		t.Fatalf("small attachments should stay attached")
	}
}

func TestOversizedWithoutStoreIsRejected(t *testing.T) {
	svc, _ := setupLinks(t, nil, 1<<10)
	m := msg("a@x.com")
	m.Attachments = []mail.Attachment{{Filename: "big.bin", Content: bytes.Repeat([]byte("b"), 4<<10)}}
	r, err := svc.Send(context.Background(), m)
	if !IsValidation(err) || r.Status != "rejected" || !strings.Contains(err.Error(), "download links are not configured") {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestLinkUploadCap(t *testing.T) {
	svc, _ := setupLinks(t, memStore(), 1<<20)
	m := msg("a@x.com")
	m.Attachments = []mail.Attachment{{Filename: "giant.iso", Content: bytes.Repeat([]byte("g"), 2<<20), AsLink: true}}
	if _, err := svc.Send(context.Background(), m); !IsValidation(err) || !strings.Contains(err.Error(), "upload limit") {
		t.Fatalf("%v", err)
	}
}

func TestInlineImagesNeverBecomeLinks(t *testing.T) {
	svc, _ := setupLinks(t, memStore(), 1<<10)
	m := msg("a@x.com")
	m.HTML = `<img src="cid:logo">`
	m.Attachments = []mail.Attachment{{Filename: "logo.png", Content: bytes.Repeat([]byte("p"), 4<<10), ContentID: "logo"}}
	if _, err := svc.Send(context.Background(), m); !IsValidation(err) || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size rejection, got %v", err)
	}
}
