package mail

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"time"
)

// Sender delivers a raw message to an envelope. The production implementation
// talks plain SMTP to the local postfix relay; tests substitute fakes.
type Sender interface {
	Send(ctx context.Context, from string, rcpts []string, raw []byte) error
	// Ping reports whether the relay accepts connections right now.
	Ping(ctx context.Context) error
}

// SMTPSender speaks unauthenticated SMTP to a trusted relay (postfix with
// permit_mynetworks). No TLS: the hop is inside the docker network.
type SMTPSender struct {
	Addr    string
	Timeout time.Duration
}

func (s *SMTPSender) timeout() time.Duration {
	if s.Timeout <= 0 {
		return 60 * time.Second
	}
	return s.Timeout
}

func (s *SMTPSender) Send(ctx context.Context, from string, rcpts []string, raw []byte) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", s.Addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(s.timeout()))
	host, _, _ := net.SplitHostPort(s.Addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer c.Close()
	if err := c.Hello("email-api"); err != nil {
		return fmt.Errorf("smtp helo: %w", err)
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp end of data: %w", err)
	}
	return c.Quit()
}

func (s *SMTPSender) Ping(ctx context.Context) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	host, _, _ := net.SplitHostPort(s.Addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	_ = c.Quit()
	return nil
}
