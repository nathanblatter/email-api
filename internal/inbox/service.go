package inbox

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nathanblatter/email-api/internal/fallback"
)

// Service accepts inbound mail and exposes the inbox. Thin: parse → store →
// optional iMessage notification.
type Service struct {
	Store  Store
	Secret string         // shared with the Email Worker (X-Inbound-Secret)
	Pager  fallback.Pager // nil = no notifications
	Notify bool
	Log    *slog.Logger
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Authorized checks the worker's shared secret in constant time.
func (s *Service) Authorized(secret string) bool {
	return s.Secret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(s.Secret)) == 1
}

// Receive stores one raw message. envFrom/envTo come from the SMTP envelope
// the edge saw (the Worker forwards them as headers).
func (s *Service) Receive(ctx context.Context, raw []byte, envFrom, envTo string) (*Message, error) {
	p, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	m, err := s.Store.Save(ctx, p, envFrom, envTo, raw)
	if err != nil {
		return nil, err
	}
	s.log().Info("inbound stored", "id", m.ID, "from", m.FromAddr, "to", envTo, "subject", m.Subject,
		"attachments", len(m.Attachments), "suspicious", m.Suspicious, "injection", m.Injection, "reasons", m.Reasons, "size", m.Size)
	if s.Notify && s.Pager != nil && !m.Suspicious {
		who := m.FromAddr
		if m.FromName != "" {
			who = fmt.Sprintf("%s <%s>", m.FromName, m.FromAddr)
		}
		preview := strings.TrimSpace(m.Text)
		if len(preview) > 280 {
			preview = preview[:280] + "…"
		}
		att := ""
		if n := len(m.Attachments); n > 0 {
			att = fmt.Sprintf(" [%d attachment(s)]", n)
		}
		text := fmt.Sprintf("📥 %s → %s%s\nSubject: %s\n\n%s", who, envTo, att, m.Subject, preview)
		if err := s.Pager.Page(ctx, text); err != nil {
			s.log().Warn("inbox notification failed", "err", err)
		}
	}
	return m, nil
}
