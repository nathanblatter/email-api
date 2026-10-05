package inbox

import (
	"context"
	"strings"
	"testing"
)

type pager struct{ pages []string }

func (p *pager) Page(_ context.Context, t string) error { p.pages = append(p.pages, t); return nil }
func (p *pager) Ping(context.Context) error             { return nil }

func TestReceiveStoresAndNotifies(t *testing.T) {
	st := NewMemory()
	pg := &pager{}
	svc := &Service{Store: st, Secret: "s", Pager: pg, Notify: true}
	if svc.Authorized("x") || !svc.Authorized("s") || (&Service{}).Authorized("") {
		t.Fatal("auth")
	}
	m, err := svc.Receive(context.Background(), []byte(sample), "bounce@example.com", "hello@nathanblatter.com")
	if err != nil {
		t.Fatal(err)
	}
	if m.FromAddr != "jane@example.com" || m.EnvTo != "hello@nathanblatter.com" || len(m.Attachments) != 2 || m.Suspicious {
		t.Fatalf("%+v", m)
	}
	if len(pg.pages) != 1 || !strings.Contains(pg.pages[0], "Jane Doe") || !strings.Contains(pg.pages[0], "[2 attachment(s)]") {
		t.Fatalf("pages %v", pg.pages)
	}
	got, err := st.Get(context.Background(), m.ID)
	if err != nil || got.Subject != "Café meeting" {
		t.Fatalf("%+v %v", got, err)
	}
	meta, body, err := st.Attachment(context.Background(), m.ID, got.Attachments[0].ID)
	if err != nil || meta.Filename != "notes.pdf" {
		t.Fatalf("%+v %v", meta, err)
	}
	body.Close()
	list, _ := st.List(context.Background(), ListOptions{Unread: true})
	if len(list) != 1 || list[0].Attachments != 2 || list[0].Read {
		t.Fatalf("list %+v", list)
	}
	if n, _ := st.Unread(context.Background()); n != 1 {
		t.Fatal("unread")
	}
	_ = st.MarkRead(context.Background(), m.ID, true)
	if n, _ := st.Unread(context.Background()); n != 0 {
		t.Fatal("unread after mark")
	}

	// Suspicious mail is stored but not paged, and hidden when filtering.
	m2, _ := svc.Receive(context.Background(), []byte("From: spam@evil.test\r\nSubject: buy\r\n\r\nx"), "", "nathan@nathanblatter.com")
	if !m2.Suspicious || len(pg.pages) != 1 {
		t.Fatalf("suspicious handling: %+v pages=%d", m2, len(pg.pages))
	}
	f := false
	list, _ = st.List(context.Background(), ListOptions{Suspicious: &f})
	if len(list) != 1 {
		t.Fatalf("filtered list %+v", list)
	}
	if err := st.Delete(context.Background(), m2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(context.Background(), m2.ID); err != ErrNotFound {
		t.Fatal("expected gone")
	}
}
