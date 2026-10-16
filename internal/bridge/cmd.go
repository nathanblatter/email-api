package bridge

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// Config is read from the environment by Main.
type Config struct {
	QueueURL string // SQS_QUEUE_URL; empty disables the bridge
	Region   string // AWS_REGION, default us-east-1
	APIURL   string // BRIDGE_API_URL, default http://app:4500
	APIKey   string // BRIDGE_API_KEY (required when enabled)
}

// ConfigFromEnv reads the bridge's settings. AWS credentials are deliberately
// not read here: the SDK's default chain picks them up from the environment.
func ConfigFromEnv() Config {
	return Config{
		QueueURL: strings.TrimSpace(os.Getenv("SQS_QUEUE_URL")),
		Region:   envOr("AWS_REGION", "us-east-1"),
		APIURL:   envOr("BRIDGE_API_URL", "http://app:4500"),
		APIKey:   strings.TrimSpace(os.Getenv("BRIDGE_API_KEY")),
	}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// Main is the `email-api sqs-bridge` subcommand. ctx is cancelled on
// SIGINT/SIGTERM; the return value is the process exit code.
//
// With SQS_QUEUE_URL unset the bridge is disabled but stays up (blocking on
// ctx) so `restart: unless-stopped` does not turn a missing setting into a
// restart loop.
func Main(ctx context.Context, log *slog.Logger, cfg Config) int {
	if cfg.QueueURL == "" {
		log.Info("bridge disabled: SQS_QUEUE_URL unset")
		<-ctx.Done()
		return 0
	}
	if cfg.APIKey == "" {
		log.Error("BRIDGE_API_KEY is required (mint one: email-api keygen auth-service)")
		return 1
	}
	q, err := OpenSQS(ctx, cfg.Region, cfg.QueueURL)
	if err != nil {
		log.Error("sqs", "err", err)
		return 1
	}
	b := &Bridge{
		Queue:  q,
		Sender: &HTTPSender{BaseURL: cfg.APIURL, APIKey: cfg.APIKey},
		Log:    log,
	}
	log.Info("sqs bridge polling", "queue", cfg.QueueURL, "region", cfg.Region, "api", cfg.APIURL)
	if err := b.Run(ctx); err != nil {
		log.Error("bridge", "err", err)
		return 1
	}
	log.Info("sqs bridge stopped")
	return 0
}
