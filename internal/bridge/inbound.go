package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Inbound is the receiving twin of Bridge, for domains whose DNS lives in
// Route 53 rather than Cloudflare (byuisresearch.com). SES receives the mail,
// stores the raw message in S3 and publishes its "Received" notification via
// SNS to an SQS queue; this loop fetches the object and replays it as
// POST /inbound on the public listener, exactly as the Cloudflare Email Worker
// does for nathanblatter.com. Pull-based: while the Mini is down the mail
// simply waits in SQS (14-day retention) instead of being tempfailed.
type Inbound struct {
	Queue   Queue
	Objects ObjectStore
	Poster  InboundPoster
	Log     *slog.Logger
	// ErrBackoff is how long to wait after a Receive error before polling
	// again. Defaults to 5s.
	ErrBackoff time.Duration
}

// ObjectStore is the part of S3 the inbound loop uses.
type ObjectStore interface {
	Get(ctx context.Context, bucket, key string) ([]byte, error)
}

// InboundPoster delivers one raw message to email-api's POST /inbound.
// detail is for logs only (status text), never the message.
type InboundPoster interface {
	Post(ctx context.Context, raw []byte, envFrom, envTo string) (outcome Outcome, detail string, err error)
}

// sesNotification is the subset of SES's receipt notification the loop reads
// (https://docs.aws.amazon.com/ses/latest/dg/receiving-email-notifications-contents.html).
type sesNotification struct {
	NotificationType string `json:"notificationType"`
	Mail             struct {
		Source      string   `json:"source"`
		Destination []string `json:"destination"`
		MessageID   string   `json:"messageId"`
	} `json:"mail"`
	Receipt struct {
		SPFVerdict   verdict `json:"spfVerdict"`
		DKIMVerdict  verdict `json:"dkimVerdict"`
		DMARCVerdict verdict `json:"dmarcVerdict"`
		SpamVerdict  verdict `json:"spamVerdict"`
		VirusVerdict verdict `json:"virusVerdict"`
		Action       struct {
			Type       string `json:"type"`
			BucketName string `json:"bucketName"`
			ObjectKey  string `json:"objectKey"`
		} `json:"action"`
	} `json:"receipt"`
}

type verdict struct {
	Status string `json:"status"` // PASS | FAIL | GRAY | PROCESSING_FAILED
}

// snsEnvelope is what arrives when the SNS→SQS subscription does not use raw
// message delivery: the SES JSON is a string inside Message.
type snsEnvelope struct {
	Type    string `json:"Type"`
	Message string `json:"Message"`
}

// errSkip marks a queue entry that is not a mail receipt (SNS subscription
// confirmations, non-Received notifications) and is just deleted.
var errSkip = errors.New("not a received-mail notification")

// parseNotification accepts either the bare SES notification or an SNS
// envelope wrapping it.
func parseNotification(body string) (*sesNotification, error) {
	var env snsEnvelope
	if err := json.Unmarshal([]byte(body), &env); err == nil && env.Type != "" {
		if env.Type != "Notification" {
			return nil, fmt.Errorf("%w: sns %s", errSkip, env.Type)
		}
		body = env.Message
	}
	var n sesNotification
	if err := json.Unmarshal([]byte(body), &n); err != nil {
		return nil, err
	}
	if n.NotificationType != "Received" {
		return nil, fmt.Errorf("%w: %q", errSkip, n.NotificationType)
	}
	if n.Receipt.Action.BucketName == "" || n.Receipt.Action.ObjectKey == "" {
		return nil, errors.New("notification has no S3 action")
	}
	return &n, nil
}

// authHeader renders SES's verdicts as the Authentication-Results header the
// inbox parser already understands (the Cloudflare path gets one from
// Cloudflare's MTA). Prepended to the raw message so it is the first such
// header and wins in inbox.Parse.
func authHeader(n *sesNotification) []byte {
	v := func(s string) string {
		switch strings.ToUpper(s) {
		case "PASS":
			return "pass"
		case "FAIL":
			return "fail"
		case "GRAY":
			return "neutral"
		default:
			return "none"
		}
	}
	return []byte(fmt.Sprintf("Authentication-Results: amazonses.com; spf=%s; dkim=%s; dmarc=%s\r\n",
		v(n.Receipt.SPFVerdict.Status), v(n.Receipt.DKIMVerdict.Status), v(n.Receipt.DMARCVerdict.Status)))
}

// Run polls until ctx is cancelled. It returns nil on a clean shutdown.
func (b *Inbound) Run(ctx context.Context) error {
	log := b.logger()
	backoff := b.ErrBackoff
	if backoff <= 0 {
		backoff = 5 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		msgs, err := b.Queue.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error("sqs receive failed", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		for _, m := range msgs {
			if ctx.Err() != nil {
				return nil
			}
			b.handle(ctx, m)
		}
	}
}

// handle processes one notification. Like Bridge.handle it never returns an
// error: the only decision is whether to delete the queue message.
func (b *Inbound) handle(ctx context.Context, m Message) {
	log := b.logger().With("sqs_id", m.ID)
	n, err := parseNotification(m.Body)
	if err != nil {
		if errors.Is(err, errSkip) {
			log.Info("skipping queue message", "reason", err)
		} else {
			log.Error("malformed notification; dropping", "err", err)
		}
		b.delete(ctx, log, m)
		return
	}
	envTo := strings.Join(n.Mail.Destination, ", ")
	log = log.With("ses_id", n.Mail.MessageID, "from", redactAddr(n.Mail.Source), "to", envTo,
		"s3", n.Receipt.Action.BucketName+"/"+n.Receipt.Action.ObjectKey)

	raw, err := b.Objects.Get(ctx, n.Receipt.Action.BucketName, n.Receipt.Action.ObjectKey)
	if err != nil {
		// S3 is far more likely to be a transient/permissions problem than a
		// genuinely missing object; leave the message so it is retried and
		// ends in the DLQ if it never works.
		log.Warn("s3 get failed; leaving message for redelivery", "err", err)
		return
	}
	raw = append(authHeader(n), raw...)

	outcome, detail, err := b.Poster.Post(ctx, raw, n.Mail.Source, envTo)
	switch outcome {
	case Accepted:
		log.Info("relayed to /inbound", "result", detail, "size", len(raw))
		b.delete(ctx, log, m)
	case Rejected:
		log.Error("email-api rejected message; dropping (raw stays in S3)", "result", detail)
		b.delete(ctx, log, m)
	default:
		if err == nil {
			err = errors.New(detail)
		}
		log.Warn("email-api unavailable; leaving message for redelivery", "err", err)
	}
}

func (b *Inbound) delete(ctx context.Context, log *slog.Logger, m Message) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := b.Queue.Delete(dctx, m.Handle); err != nil {
		log.Error("sqs delete failed; message may be redelivered", "err", err)
	}
}

func (b *Inbound) logger() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}
