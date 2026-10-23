package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// RoutedSender picks a Sender by the envelope sender's domain: domains in
// ByDomain go to their own relay (SES for byuisresearch.com), everything
// else to Default (postfix → Brevo for nathanblatter.com).
type RoutedSender struct {
	Default  Sender
	ByDomain map[string]Sender // lower-case domain → sender
}

// NewRoutedSender returns def unchanged when there is nothing to route, so
// the common single-relay setup pays nothing for the indirection.
func NewRoutedSender(def Sender, byDomain map[string]Sender) Sender {
	if len(byDomain) == 0 {
		return def
	}
	m := make(map[string]Sender, len(byDomain))
	for d, s := range byDomain {
		m[strings.ToLower(d)] = s
	}
	return &RoutedSender{Default: def, ByDomain: m}
}

func (r *RoutedSender) pick(from string) Sender {
	if at := strings.LastIndex(from, "@"); at >= 0 {
		if s, ok := r.ByDomain[strings.ToLower(from[at+1:])]; ok {
			return s
		}
	}
	return r.Default
}

func (r *RoutedSender) Send(ctx context.Context, from string, rcpts []string, raw []byte) error {
	return r.pick(from).Send(ctx, from, rcpts, raw)
}

// Ping checks every relay; one being down degrades health for all, which is
// what the pager should see.
func (r *RoutedSender) Ping(ctx context.Context) error {
	var errs []error
	if err := r.Default.Ping(ctx); err != nil {
		errs = append(errs, err)
	}
	for d, s := range r.ByDomain {
		if err := s.Ping(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d, err))
		}
	}
	return errors.Join(errs...)
}
