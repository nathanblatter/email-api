package imapd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func (g *Gateway) newSMTPServer(tlsCfg *tls.Config) *smtp.Server {
	s := smtp.NewServer(&smtpBackend{g: g})
	s.Domain = g.cfg.Hostname
	s.TLSConfig = tlsCfg
	s.MaxMessageBytes = g.cfg.MaxBytes
	s.MaxRecipients = 50
	s.ReadTimeout = 2 * time.Minute
	s.WriteTimeout = 2 * time.Minute
	s.AllowInsecureAuth = false
	return s
}

type smtpBackend struct{ g *Gateway }

func (b *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{g: b.g, ip: ipOf(c.Conn().RemoteAddr())}, nil
}

type smtpSession struct {
	g      *Gateway
	ip     string
	device string
	from   string
	rcpts  []string
}

var _ smtp.AuthSession = (*smtpSession)(nil)

func (s *smtpSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		dev, err := s.g.authenticate(s.ip, username, password)
		if err != nil {
			return smtp.ErrAuthFailed
		}
		s.device = dev
		return nil
	}), nil
}

func (s *smtpSession) Mail(from string, opts *smtp.MailOptions) error {
	if s.device == "" {
		return smtp.ErrAuthRequired
	}
	if !strings.EqualFold(strings.TrimSpace(from), s.g.cfg.Address) {
		return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "This account may only send as " + s.g.cfg.Address}
	}
	s.from = s.g.cfg.Address
	return nil
}

func (s *smtpSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if s.device == "" {
		return smtp.ErrAuthRequired
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *smtpSession) Data(r io.Reader) error {
	if s.device == "" || s.from == "" || len(s.rcpts) == 0 {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Bad sequence of commands"}
	}
	raw, err := io.ReadAll(io.LimitReader(r, s.g.cfg.MaxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > s.g.cfg.MaxBytes {
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: "Message too big"}
	}
	status, msg, err := s.g.sendRaw(s.from, s.rcpts, raw)
	switch {
	case err != nil:
		s.g.log.Error("smtp submit: api unreachable", "device", s.device, "err", err)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary failure, try again"}
	case status == http.StatusOK || status == http.StatusAccepted:
		s.g.log.Info("smtp submit", "device", s.device, "rcpts", len(s.rcpts), "status", status)
		return nil
	case status >= 400 && status < 500 && status != http.StatusTooManyRequests:
		s.g.log.Warn("smtp submit rejected", "device", s.device, "status", status, "reason", msg)
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "Rejected: " + msg}
	default:
		s.g.log.Error("smtp submit failed", "device", s.device, "status", status, "reason", msg)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary failure, try again"}
	}
}

func (s *smtpSession) Reset() {
	s.from, s.rcpts = "", nil
}

func (s *smtpSession) Logout() error { return nil }

var apiClient = &http.Client{Timeout: 60 * time.Second}

// sendRaw hands a composed message to the API (budget, spool, pager, policy).
func (g *Gateway) sendRaw(from string, rcpts []string, raw []byte) (int, string, error) {
	body, err := json.Marshal(map[string]any{"from": from, "recipients": rcpts, "raw": raw})
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.SendURL+"/send/raw", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", g.cfg.SendKey)
	resp, err := apiClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var out struct {
		Error  string `json:"error"`
		Status string `json:"status"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(b, &out)
	msg := out.Error
	if msg == "" {
		msg = fmt.Sprintf("%s %s", resp.Status, out.Status)
	}
	return resp.StatusCode, msg, nil
}
