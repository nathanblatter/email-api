// Package config loads the service configuration from environment variables.
// The surface is deliberately a flat .env: every knob is one variable with a
// sensible default, so the compose file and .env.example are the whole story.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr   string // listen address, e.g. ":4500"
	APIKey string // X-API-Key for /send, /send/batch, /queue, /mcp

	SMTPHost string
	SMTPPort int

	DefaultFrom        string   // used when a request omits "from"
	AllowedFromDomains []string // From must be under one of these domains

	// Daily send budget (recipients counted, matching how the relay counts).
	// Over budget, mail is spooled until the next UTC day instead of being
	// bounced by the relay.
	DailyBudget int
	RedisURL    string // counter store; empty or unreachable → in-memory fallback

	// Fallback paging via the iMessage API when the relay is down.
	IMessageURL       string
	IMessageKey       string
	IMessageRecipient string
	PageInterval      time.Duration // minimum gap between outage pages

	SpoolDir        string
	SpoolMaxAge     time.Duration
	RetryInterval   time.Duration
	MaxMessageBytes int64
}

func FromEnv() (Config, error) {
	c := Config{
		Addr:               env("EMAIL_ADDR", ":4500"),
		APIKey:             os.Getenv("EMAIL_API_KEY"),
		SMTPHost:           env("SMTP_HOST", "postfix"),
		DefaultFrom:        env("EMAIL_DEFAULT_FROM", "Nathan Blatter <noreply@nathanblatter.com>"),
		AllowedFromDomains: splitList(env("EMAIL_ALLOWED_FROM_DOMAINS", "nathanblatter.com")),
		RedisURL:           os.Getenv("REDIS_URL"),
		IMessageURL:        env("IMESSAGE_API_URL", "http://100.79.61.79:8899"),
		IMessageKey:        os.Getenv("IMESSAGE_API_KEY"),
		IMessageRecipient:  os.Getenv("IMESSAGE_FALLBACK_RECIPIENT"),
		SpoolDir:           env("EMAIL_SPOOL_DIR", "/data/spool"),
	}
	if c.APIKey == "" {
		return c, fmt.Errorf("EMAIL_API_KEY is required")
	}
	var err error
	if c.SMTPPort, err = envInt("SMTP_PORT", 25); err != nil {
		return c, err
	}
	if c.DailyBudget, err = envInt("EMAIL_DAILY_BUDGET", 250); err != nil {
		return c, err
	}
	if c.PageInterval, err = envDur("EMAIL_PAGE_INTERVAL", 10*time.Minute); err != nil {
		return c, err
	}
	if c.SpoolMaxAge, err = envDur("EMAIL_SPOOL_MAX_AGE", 48*time.Hour); err != nil {
		return c, err
	}
	if c.RetryInterval, err = envDur("EMAIL_RETRY_INTERVAL", time.Minute); err != nil {
		return c, err
	}
	mb, err := envInt("EMAIL_MAX_MESSAGE_MB", 10) // Brevo rejects messages over 10 MB
	if err != nil {
		return c, err
	}
	c.MaxMessageBytes = int64(mb) << 20
	if len(c.AllowedFromDomains) == 0 {
		return c, fmt.Errorf("EMAIL_ALLOWED_FROM_DOMAINS must list at least one domain")
	}
	return c, nil
}

func (c Config) SMTPAddr() string { return fmt.Sprintf("%s:%d", c.SMTPHost, c.SMTPPort) }

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func envDur(k string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}
