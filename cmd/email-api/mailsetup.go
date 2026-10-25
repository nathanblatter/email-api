package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nathanblatter/email-api/internal/config"
	"github.com/nathanblatter/email-api/internal/connector"
	"github.com/nathanblatter/email-api/internal/inbox"
	"github.com/nathanblatter/email-api/internal/maildev"
)

// mailSetup wires the owner-only Apple Mail setup page when its Squawk client
// is configured (EMAIL_MAIL_CLIENT_ID) and the inbox database is available.
func mailSetup(cfg config.Config, pg *inbox.PG, log *slog.Logger) *connector.MailSetup {
	clientID := strings.TrimSpace(os.Getenv("EMAIL_MAIL_CLIENT_ID"))
	if clientID == "" || pg == nil {
		return nil
	}
	secret := []byte(os.Getenv("EMAIL_MAIL_SETUP_SECRET"))
	if len(secret) == 0 {
		mac := hmac.New(sha256.New, []byte(cfg.InboundSecret))
		mac.Write([]byte("mail-setup-cookie"))
		secret = mac.Sum(nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devs, err := maildev.Open(ctx, pg.Pool())
	if err != nil {
		log.Error("mail devices; setup page disabled", "err", err)
		return nil
	}
	return &connector.MailSetup{
		Devices:  devs,
		ClientID: clientID,
		Secret:   secret,
		Address:  strings.ToLower(envOr("MAIL_ADDRESS", "nathan@nathanblatter.com")),
		Host:     envOr("MAIL_HOSTNAME", "mail.nathanblatter.com"),
		FullName: envOr("MAIL_FULL_NAME", "Nathan Blatter"),
		Log:      log,
	}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
