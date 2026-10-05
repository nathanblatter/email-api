package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/quota"
	"github.com/nathanblatter/email-api/internal/spool"
)

type fakeSender struct {
	mu   sync.Mutex
	fail error
	sent []string
}

func (f *fakeSender) Send(_ context.Context, from string, rcpts []string, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, strings.Join(rcpts, ","))
	return nil
}
func (f *fakeSender) Ping(context.Context) error { return f.fail }

type fakePager struct {
	mu    sync.Mutex
	pages []string
}

func (p *fakePager) Page(_ context.Context, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = append(p.pages, text)
	return nil
}
func (p *fakePager) Ping(context.Context) error { return nil }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T, budget int) (*Service, *fakeSender, *fakePager, *clock) {
	t.Helper()
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snd, pg := &fakeSender{}, &fakePager{}
	ck := &clock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	q := quota.New(budget, "")
	q.Now = ck.now
	svc := New(snd, pg, sp, q, slog.Default(), Options{
		Policy:       mail.Policy{DefaultFrom: "noreply@nathanblatter.com", AllowedDomains: []string{"nathanblatter.com"}},
		SpoolMaxAge:  time.Hour,
		PageInterval: 10 * time.Minute,
		Now:          ck.now,
	})
	return svc, snd, pg, ck
}

func msg(to ...string) *mail.Message {
	return &mail.Message{To: mail.AddrList(to), Subject: "hello", Text: "body text"}
}

func TestSendHappyPath(t *testing.T) {
	svc, snd, pg, _ := setup(t, 100)
	r, err := svc.Send(context.Background(), msg("a@x.com"))
	if err != nil || r.Status != "sent" || r.ID == "" {
		t.Fatalf("%+v %v", r, err)
	}
	if len(snd.sent) != 1 || len(pg.pages) != 0 {
		t.Fatalf("sent=%v pages=%v", snd.sent, pg.pages)
	}
}

func TestValidationErrorIsRejected(t *testing.T) {
	svc, _, _, _ := setup(t, 100)
	r, err := svc.Send(context.Background(), &mail.Message{From: "x@gmail.com", To: mail.AddrList{"a@x.com"}, Subject: "s", Text: "t"})
	if !IsValidation(err) || r.Status != "rejected" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRelayDownQueuesPagesAndRecovers(t *testing.T) {
	svc, snd, pg, ck := setup(t, 100)
	snd.fail = errors.New("connection refused")

	r, err := svc.Send(context.Background(), msg("a@x.com"))
	if err != nil || r.Status != "queued" || r.Reason != "relay_down" || r.Fallback != "imessage" {
		t.Fatalf("%+v %v", r, err)
	}
	if len(pg.pages) != 1 || !strings.Contains(pg.pages[0], "relay DOWN") || !strings.Contains(pg.pages[0], "body text") {
		t.Fatalf("pages %v", pg.pages)
	}
	// Second failure inside the page interval is folded, not paged.
	ck.t = ck.t.Add(time.Minute)
	if r, _ = svc.Send(context.Background(), msg("b@x.com")); r.Fallback != "none" || len(pg.pages) != 1 {
		t.Fatalf("expected throttled page, got %+v pages=%d", r, len(pg.pages))
	}
	// Still down on retry: nothing delivered, both remain.
	if d, rem, _ := svc.RetryOnce(context.Background()); d != 0 || rem != 2 {
		t.Fatalf("retry while down: delivered=%d remaining=%d", d, rem)
	}
	// Relay recovers: both drain, one recovery page.
	snd.fail = nil
	if d, rem, _ := svc.RetryOnce(context.Background()); d != 2 || rem != 0 {
		t.Fatalf("retry after recovery: delivered=%d remaining=%d", d, rem)
	}
	if len(snd.sent) != 2 || len(pg.pages) != 2 || !strings.Contains(pg.pages[1], "relay back. 2 queued") {
		t.Fatalf("sent=%v pages=%v", snd.sent, pg.pages)
	}
	// Nothing left; a further pass is silent.
	svc.RetryOnce(context.Background())
	if len(pg.pages) != 2 {
		t.Fatalf("unexpected extra page: %v", pg.pages)
	}
}

func TestThrottledPagesCarryCount(t *testing.T) {
	svc, snd, pg, ck := setup(t, 100)
	snd.fail = errors.New("down")
	svc.Send(context.Background(), msg("a@x.com"))
	svc.Send(context.Background(), msg("b@x.com"))
	svc.Send(context.Background(), msg("c@x.com"))
	ck.t = ck.t.Add(11 * time.Minute)
	svc.Send(context.Background(), msg("d@x.com"))
	if len(pg.pages) != 2 || !strings.Contains(pg.pages[1], "+2 more queued") {
		t.Fatalf("pages %v", pg.pages)
	}
}

func TestDailyBudgetSpoolsUntilReset(t *testing.T) {
	svc, snd, pg, ck := setup(t, 2)
	if r, _ := svc.Send(context.Background(), msg("a@x.com", "b@x.com")); r.Status != "sent" {
		t.Fatalf("%+v", r)
	}
	r, err := svc.Send(context.Background(), msg("c@x.com"))
	if err != nil || r.Status != "queued" || r.Reason != "daily_budget" || !strings.HasPrefix(r.RetryAfter, "2026-10-06T00:00:00") {
		t.Fatalf("%+v %v", r, err)
	}
	if len(pg.pages) != 1 || !strings.Contains(pg.pages[0], "budget reached (2/2)") {
		t.Fatalf("pages %v", pg.pages)
	}
	if d, rem, _ := svc.RetryOnce(context.Background()); d != 0 || rem != 1 {
		t.Fatalf("should still be over budget: %d %d", d, rem)
	}
	svc.opt.SpoolMaxAge = 48 * time.Hour
	ck.t = ck.t.Add(13 * time.Hour) // past UTC midnight
	if d, rem, _ := svc.RetryOnce(context.Background()); d != 1 || rem != 0 {
		t.Fatalf("after reset: delivered=%d remaining=%d", d, rem)
	}
	if len(snd.sent) != 2 {
		t.Fatalf("sent %v", snd.sent)
	}
}

func TestGiveUpAfterMaxAge(t *testing.T) {
	svc, snd, pg, ck := setup(t, 100)
	snd.fail = errors.New("down")
	svc.Send(context.Background(), msg("a@x.com"))
	ck.t = ck.t.Add(2 * time.Hour)
	if _, rem, dead := svc.RetryOnce(context.Background()); dead != 1 || rem != 0 {
		t.Fatalf("dead=%d rem=%d", dead, rem)
	}
	if q, _ := svc.Queue(); len(q) != 0 {
		t.Fatalf("queue should be empty: %v", q)
	}
	if !strings.Contains(pg.pages[len(pg.pages)-1], "gave up on 1") {
		t.Fatalf("pages %v", pg.pages)
	}
}

func TestBatchIsIndependent(t *testing.T) {
	svc, snd, _, _ := setup(t, 100)
	rs := svc.SendBatch(context.Background(), []*mail.Message{msg("a@x.com"), {To: mail.AddrList{"x"}, Subject: "s", Text: "t"}, msg("b@x.com")})
	if rs[0].Status != "sent" || rs[1].Status != "rejected" || rs[2].Status != "sent" || len(snd.sent) != 2 {
		t.Fatalf("%+v", rs)
	}
}

func TestHealth(t *testing.T) {
	svc, snd, _, _ := setup(t, 100)
	svc.Send(context.Background(), msg("a@x.com", "b@x.com"))
	h := svc.Health(context.Background())
	if h.Status != "ok" || h.SentToday != 2 || h.DailyBudget != 100 || h.Queued != 0 {
		t.Fatalf("%+v", h)
	}
	snd.fail = errors.New("down")
	svc.Send(context.Background(), msg("c@x.com"))
	if h = svc.Health(context.Background()); h.Status != "degraded" || h.Relay != "down" || h.Queued != 1 {
		t.Fatalf("%+v", h)
	}
}
