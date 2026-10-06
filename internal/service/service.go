// Package service is the delivery policy: try the relay; if it refuses or
// the daily budget is spent, spool the message, keep retrying, and page the
// phone so nothing silently waits. This is where the robustness lives; the
// HTTP and MCP layers are thin.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nathanblatter/email-api/internal/auth"
	"github.com/nathanblatter/email-api/internal/fallback"
	"github.com/nathanblatter/email-api/internal/files"
	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/quota"
	"github.com/nathanblatter/email-api/internal/spool"
)

type Options struct {
	Policy       mail.Policy
	MaxLinkBytes int64 // largest single file that may become a link (0 = unlimited)
	SpoolMaxAge  time.Duration
	PageInterval time.Duration
	Now          func() time.Time
}

type Service struct {
	opt    Options
	files  files.Store
	sender mail.Sender
	pager  fallback.Pager
	spool  *spool.Spool
	quota  *quota.Counter
	log    *slog.Logger

	// Paging is throttled: during an outage every queued message would
	// otherwise fire a text. First failure pages immediately; later ones
	// within PageInterval are folded into the next page as a count.
	mu         sync.Mutex
	lastPage   time.Time
	suppressed int
}

func New(sender mail.Sender, pager fallback.Pager, sp *spool.Spool, q *quota.Counter, fs files.Store, log *slog.Logger, opt Options) *Service {
	if fs == nil {
		fs = files.Nop{}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.PageInterval <= 0 {
		opt.PageInterval = 10 * time.Minute
	}
	if opt.SpoolMaxAge <= 0 {
		opt.SpoolMaxAge = 48 * time.Hour
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{opt: opt, files: fs, sender: sender, pager: pager, spool: sp, quota: q, log: log}
}

// Result is what every send reports, for HTTP and MCP alike.
type Result struct {
	ID         string   `json:"id,omitempty"`
	Status     string   `json:"status"` // sent | queued | rejected
	Recipients []string `json:"recipients,omitempty"`
	Reason     string   `json:"reason,omitempty"`   // for queued: relay_down | daily_budget
	Fallback   string   `json:"fallback,omitempty"` // for queued: imessage | none
	RetryAfter string   `json:"retry_after,omitempty"`
	Error      string   `json:"error,omitempty"`
	// Links are attachments that were uploaded and linked instead of attached.
	Links []files.Link `json:"links,omitempty"`
}

// Send validates and delivers one message. A *mail.ValidationError means the
// caller's input was bad; any other error is internal. A queued message is
// not an error: it is accepted and will be retried.
func (s *Service) Send(ctx context.Context, m *mail.Message) (Result, error) {
	links, err := s.linkify(ctx, m)
	if err != nil {
		return Result{Status: "rejected", Error: err.Error()}, err
	}
	p, err := mail.Prepare(m, s.opt.Policy)
	if err != nil {
		return Result{Status: "rejected", Error: err.Error()}, err
	}
	res, err := s.deliver(ctx, p)
	res.Links = links
	return res, err
}

// linkify uploads attachments flagged as_link, then (largest first) any
// others needed to bring the message under the size limit, and appends a
// download section to the body. m is modified in place.
func (s *Service) linkify(ctx context.Context, m *mail.Message) ([]files.Link, error) {
	if m == nil || len(m.Attachments) == 0 {
		return nil, nil
	}
	limit := s.opt.Policy.MaxBytes
	var keep []mail.Attachment
	var toLink []mail.Attachment
	for _, a := range m.Attachments {
		if a.AsLink && a.ContentID == "" {
			toLink = append(toLink, a)
		} else {
			keep = append(keep, a)
		}
	}
	m.Attachments = keep
	if limit > 0 {
		for m.EncodedSize() > limit && len(m.Attachments) > 0 {
			// Pick the largest non-inline attachment to convert.
			idx := -1
			for i, a := range m.Attachments {
				if a.ContentID == "" && (idx < 0 || len(a.Content) > len(m.Attachments[idx].Content)) {
					idx = i
				}
			}
			if idx < 0 {
				break // only inline images left; Prepare will report the size error
			}
			toLink = append(toLink, m.Attachments[idx])
			m.Attachments = append(m.Attachments[:idx:idx], m.Attachments[idx+1:]...)
		}
	}
	if len(toLink) == 0 {
		return nil, nil
	}
	if !s.files.Configured() {
		return nil, &mail.ValidationError{Msg: fmt.Sprintf("attachments exceed the %d MB message limit and download links are not configured", limit>>20)}
	}
	var links []files.Link
	for _, a := range toLink {
		if int64(len(a.Content)) > s.opt.MaxLinkBytes && s.opt.MaxLinkBytes > 0 {
			return nil, &mail.ValidationError{Msg: fmt.Sprintf("%s is %d MB, over the %d MB upload limit", a.Filename, len(a.Content)>>20, s.opt.MaxLinkBytes>>20)}
		}
		l, err := s.files.Put(ctx, a.Filename, a.ContentType, a.Content)
		if err != nil {
			return nil, fmt.Errorf("upload %s: %w", a.Filename, err)
		}
		links = append(links, l)
		s.log.Info("attachment linked", "file", l.Filename, "size", l.Size, "expires", l.Expires)
	}
	appendLinks(m, links)
	return links, nil
}

func appendLinks(m *mail.Message, links []files.Link) {
	var txt, htm strings.Builder
	txt.WriteString("\n\n---\nAttachments (download links):\n")
	htm.WriteString(`<hr style="margin-top:24px;border:0;border-top:1px solid #e5e7eb"><p style="font-size:13px;color:#374151"><strong>Attachments</strong> (download links)</p><ul style="font-size:13px">`)
	for _, l := range links {
		exp := l.Expires.UTC().Format("Jan 2, 2006")
		fmt.Fprintf(&txt, "  %s (%s, available until %s)\n  %s\n", l.Filename, humanSize(l.Size), exp, l.URL)
		fmt.Fprintf(&htm, `<li><a href="%s">%s</a> <span style="color:#6b7280">(%s, available until %s)</span></li>`, l.URL, htmlEscape(l.Filename), humanSize(l.Size), exp)
	}
	htm.WriteString("</ul>")
	if strings.TrimSpace(m.Text) != "" || strings.TrimSpace(m.HTML) == "" {
		m.Text += txt.String()
	}
	if strings.TrimSpace(m.HTML) != "" {
		m.HTML += htm.String()
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func (s *Service) deliver(ctx context.Context, p *mail.Prepared) (Result, error) {
	rcpts := p.Recipients()
	entry := &spool.Entry{
		ID: p.ID, From: p.From.Address, Rcpts: rcpts, Subject: p.Subject,
		Preview: spool.Preview(p.Text, 400), Raw: p.Raw, CreatedAt: s.opt.Now(),
	}

	if ok, used := s.quota.Reserve(ctx, len(rcpts)); !ok {
		entry.Reason = spool.ReasonBudget
		reset := quota.NextReset(s.opt.Now())
		if err := s.spool.Put(entry); err != nil {
			return Result{Status: "rejected", Error: "spool: " + err.Error()}, err
		}
		s.log.Warn("daily budget reached; queued", "id", p.ID, "used", used, "budget", s.quota.Budget, "retry_after", reset)
		fb := s.page(ctx, fmt.Sprintf("📧 email-api: daily send budget reached (%d/%d). Queued until %s UTC:\n\nFrom: %s\nTo: %s\nSubject: %s",
			used, s.quota.Budget, reset.Format("Jan 2 15:04"), p.From.Address, strings.Join(rcpts, ", "), p.Subject))
		return Result{ID: p.ID, Status: "queued", Recipients: rcpts, Reason: string(entry.Reason), Fallback: fb, RetryAfter: reset.Format(time.RFC3339)}, nil
	}

	if err := s.sender.Send(ctx, p.From.Address, rcpts, p.Raw); err != nil {
		entry.Reason = spool.ReasonRelayDown
		entry.Attempts, entry.LastError, entry.LastTry = 1, err.Error(), s.opt.Now()
		if perr := s.spool.Put(entry); perr != nil {
			return Result{Status: "rejected", Error: "relay failed and spool failed: " + perr.Error()}, perr
		}
		s.log.Error("relay failed; queued", "id", p.ID, "err", err)
		fb := s.page(ctx, fmt.Sprintf("📧 email-api: mail relay DOWN (%s). Queued, will retry for %s:\n\nFrom: %s\nTo: %s\nSubject: %s\n\n%s",
			shortErr(err), s.opt.SpoolMaxAge, p.From.Address, strings.Join(rcpts, ", "), p.Subject, entry.Preview))
		return Result{ID: p.ID, Status: "queued", Recipients: rcpts, Reason: string(entry.Reason), Fallback: fb, Error: err.Error()}, nil
	}
	s.log.Info("sent", "id", p.ID, "actor", auth.ActorFrom(ctx), "from", p.From.Address, "to", rcpts, "subject", p.Subject)
	return Result{ID: p.ID, Status: "sent", Recipients: rcpts}, nil
}

// SendBatch sends messages in order. One bad message does not stop the rest;
// its result carries the error.
func (s *Service) SendBatch(ctx context.Context, msgs []*mail.Message) []Result {
	out := make([]Result, len(msgs))
	for i, m := range msgs {
		r, _ := s.Send(ctx, m)
		out[i] = r
	}
	return out
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// page sends an iMessage, throttled. Returns "imessage" if a text went out,
// "none" otherwise (throttled, unconfigured, or failed).
func (s *Service) page(ctx context.Context, text string) string {
	s.mu.Lock()
	now := s.opt.Now()
	if !s.lastPage.IsZero() && now.Sub(s.lastPage) < s.opt.PageInterval {
		s.suppressed++
		s.mu.Unlock()
		return "none"
	}
	if s.suppressed > 0 {
		text += fmt.Sprintf("\n\n(+%d more queued since last page)", s.suppressed)
	}
	s.lastPage, s.suppressed = now, 0
	s.mu.Unlock()

	if s.pager == nil {
		return "none"
	}
	if err := s.pager.Page(ctx, text); err != nil {
		s.log.Error("imessage page failed", "err", err)
		return "none"
	}
	return "imessage"
}

// RunRetries drains the spool every interval until ctx ends.
func (s *Service) RunRetries(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RetryOnce(ctx)
		}
	}
}

// RetryOnce makes one pass over the spool. Exported for tests and for the
// manual POST /queue/retry endpoint.
func (s *Service) RetryOnce(ctx context.Context) (delivered, remaining, dead int) {
	entries, err := s.spool.List()
	if err != nil {
		s.log.Error("spool list failed", "err", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	now := s.opt.Now()
	for _, e := range entries {
		if now.Sub(e.CreatedAt) > s.opt.SpoolMaxAge {
			if err := s.spool.Dead(e); err == nil {
				dead++
				s.log.Error("gave up on queued message", "id", e.ID, "subject", e.Subject, "last_error", e.LastError)
			}
			continue
		}
		if e.Reason == spool.ReasonBudget {
			if ok, _ := s.quota.Reserve(ctx, len(e.Rcpts)); !ok {
				remaining++
				continue
			}
			// Budget is reserved; from here it behaves like a normal send.
			e.Reason = spool.ReasonRelayDown
		}
		e.Attempts++
		e.LastTry = now
		if err := s.sender.Send(ctx, e.From, e.Rcpts, e.Raw); err != nil {
			e.LastError = err.Error()
			_ = s.spool.Put(e)
			remaining++
			continue
		}
		_ = s.spool.Delete(e)
		delivered++
		s.log.Info("delivered from spool", "id", e.ID, "attempts", e.Attempts)
	}

	if dead > 0 {
		s.page(ctx, fmt.Sprintf("📧 email-api: gave up on %d queued email(s) older than %s. They are kept in the spool's dead/ folder.", dead, s.opt.SpoolMaxAge))
	}
	if delivered > 0 && remaining == 0 {
		s.pageNow(ctx, fmt.Sprintf("📧 email-api: mail relay back. %d queued email(s) delivered.", delivered))
	}
	return
}

// pageNow bypasses the throttle for recovery notices (they are rare and the
// one message you always want).
func (s *Service) pageNow(ctx context.Context, text string) {
	s.mu.Lock()
	s.lastPage, s.suppressed = s.opt.Now(), 0
	s.mu.Unlock()
	if s.pager == nil {
		return
	}
	if err := s.pager.Page(ctx, text); err != nil {
		s.log.Error("imessage page failed", "err", err)
	}
}

// QueueEntry is the operator view of a spooled message (no raw bytes).
type QueueEntry struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	Rcpts     []string  `json:"rcpts"`
	Subject   string    `json:"subject"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
}

func (s *Service) Queue() ([]QueueEntry, error) {
	es, err := s.spool.List()
	if err != nil {
		return nil, err
	}
	out := make([]QueueEntry, 0, len(es))
	for _, e := range es {
		out = append(out, QueueEntry{ID: e.ID, From: e.From, Rcpts: e.Rcpts, Subject: e.Subject, Reason: string(e.Reason),
			CreatedAt: e.CreatedAt, Attempts: e.Attempts, LastError: e.LastError})
	}
	return out, nil
}

type Health struct {
	Status      string `json:"status"` // ok | degraded
	Relay       string `json:"relay"`  // ok | down
	IMessage    string `json:"imessage"`
	Quota       string `json:"quota"` // redis | memory | redis-unreachable
	Files       string `json:"files"` // minio | unconfigured
	Queued      int    `json:"queued"`
	SentToday   int    `json:"sent_today"`
	DailyBudget int    `json:"daily_budget"`
	RelayError  string `json:"relay_error,omitempty"`
}

func (s *Service) Health(ctx context.Context) Health {
	h := Health{Status: "ok", Relay: "ok", IMessage: "ok", Quota: s.quota.Backend(), Files: "unconfigured",
		Queued: s.spool.Count(), SentToday: s.quota.Used(ctx), DailyBudget: s.quota.Budget}
	if err := s.sender.Ping(ctx); err != nil {
		h.Relay, h.RelayError, h.Status = "down", err.Error(), "degraded"
	}
	if s.files.Configured() {
		h.Files = "minio"
	}
	if s.pager == nil {
		h.IMessage = "unconfigured"
	} else if err := s.pager.Ping(ctx); err != nil {
		h.IMessage = "down"
	}
	if err := s.quota.Ping(ctx); err != nil {
		h.Quota, h.Status = "redis-unreachable", "degraded"
	}
	if h.Queued > 0 {
		h.Status = "degraded"
	}
	return h
}

// IsValidation reports whether err came from request validation.
func IsValidation(err error) bool {
	var ve *mail.ValidationError
	return errors.As(err, &ve)
}
