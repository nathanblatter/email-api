package mail

import (
	"context"
	"errors"
	"testing"
)

type recSender struct {
	calls int
	err   error
}

func (r *recSender) Send(context.Context, string, []string, []byte) error { r.calls++; return r.err }
func (r *recSender) Ping(context.Context) error                           { return r.err }

func TestRoutedSenderPicksByDomain(t *testing.T) {
	def, ses := &recSender{}, &recSender{}
	s := NewRoutedSender(def, map[string]Sender{"ByuIsResearch.com": ses})
	ctx := context.Background()
	for _, from := range []string{"a@byuisresearch.com", "B@BYUISRESEARCH.COM"} {
		if err := s.Send(ctx, from, []string{"x@example.com"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, from := range []string{"n@nathanblatter.com", "nodomain"} {
		if err := s.Send(ctx, from, []string{"x@example.com"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if ses.calls != 2 || def.calls != 2 {
		t.Fatalf("ses=%d default=%d, want 2/2", ses.calls, def.calls)
	}
}

func TestRoutedSenderNoRoutesReturnsDefault(t *testing.T) {
	def := &recSender{}
	if s := NewRoutedSender(def, nil); s != Sender(def) {
		t.Fatalf("expected the default sender back, got %T", s)
	}
}

func TestRoutedSenderPingJoinsErrors(t *testing.T) {
	down := errors.New("ses down")
	s := NewRoutedSender(&recSender{}, map[string]Sender{"byuisresearch.com": &recSender{err: down}})
	err := s.Ping(context.Background())
	if !errors.Is(err, down) {
		t.Fatalf("ping = %v, want wrapped %v", err, down)
	}
	if err.Error() != "byuisresearch.com: ses down" {
		t.Fatalf("ping message = %q", err.Error())
	}
}
