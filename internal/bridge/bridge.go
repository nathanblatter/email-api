// Package bridge is the SQS outbox consumer: it long-polls an SQS queue that
// auth-service (an AWS Lambda, unable to reach the Tailscale-only keyed port)
// writes mail requests to, and replays each one as POST /send against the
// email-api edge with a named key. Every send therefore still goes through
// email-api's policy, budget, spool and actor logging.
//
// The loop is built on two small interfaces (Queue, Sender) so it can be
// exercised with fakes; the AWS and HTTP adapters live in sqs.go and http.go.
package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
)

// Request is the message contract auth-service puts on the queue. It is also
// the body POSTed to /send: the field names are email-api's own (mail.Message
// to/subject/text/from), and From is omitted when empty so EMAIL_DEFAULT_FROM
// applies.
type Request struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	From    string `json:"from,omitempty"`
}

// Message is one received queue entry. Handle is what DeleteMessage needs.
type Message struct {
	ID     string
	Handle string
	Body   string
}

// Queue is the part of SQS the bridge uses.
type Queue interface {
	// Receive long-polls and returns zero or more messages. It must honour ctx.
	Receive(ctx context.Context) ([]Message, error)
	Delete(ctx context.Context, handle string) error
}

// Outcome classifies a /send attempt so the loop can decide whether the
// queue message is finished (delete) or should be redelivered (keep).
type Outcome int

const (
	// Accepted: email-api took the message (sent or queued/spooled).
	Accepted Outcome = iota
	// Rejected: email-api says the message itself is bad (4xx). Retrying
	// cannot fix it, so the queue message is dropped.
	Rejected
	// Retry: transient failure (5xx, 429, network). Leave it on the queue.
	Retry
)

func (o Outcome) String() string {
	switch o {
	case Accepted:
		return "accepted"
	case Rejected:
		return "rejected"
	default:
		return "retry"
	}
}

// Sender delivers one request to email-api. detail is for logs only (the
// response status/error text), never the message body.
type Sender interface {
	Send(ctx context.Context, req Request) (outcome Outcome, detail string, err error)
}

// Bridge is the consume loop.
type Bridge struct {
	Queue  Queue
	Sender Sender
	Log    *slog.Logger
	// ErrBackoff is how long to wait after a Receive error before polling
	// again, so a broken queue config cannot hot-loop. Defaults to 5s.
	ErrBackoff time.Duration
}

// Run polls until ctx is cancelled. It returns nil on a clean shutdown.
func (b *Bridge) Run(ctx context.Context) error {
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
				// Not processed: visibility timeout expires and SQS redelivers.
				return nil
			}
			b.handle(ctx, m)
		}
	}
}

// handle processes one queue message. It never returns an error: the only
// decision is whether to delete the message, and that is made here.
func (b *Bridge) handle(ctx context.Context, m Message) {
	log := b.logger().With("sqs_id", m.ID)
	var req Request
	dec := json.NewDecoder(strings.NewReader(m.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		log.Error("malformed queue message; dropping", "err", err)
		b.delete(ctx, log, m)
		return
	}
	log = log.With("to", redactAddr(req.To), "subject_len", len(req.Subject), "text_len", len(req.Text))

	outcome, detail, err := b.Sender.Send(ctx, req)
	switch outcome {
	case Accepted:
		log.Info("relayed to email-api", "result", detail)
		b.delete(ctx, log, m)
	case Rejected:
		log.Error("email-api rejected message; dropping", "result", detail)
		b.delete(ctx, log, m)
	default:
		if err == nil {
			err = errors.New(detail)
		}
		log.Warn("email-api unavailable; leaving message for redelivery", "err", err)
	}
}

func (b *Bridge) delete(ctx context.Context, log *slog.Logger, m Message) {
	// Use a short detached timeout so a shutdown that lands between the send
	// and the delete still removes the message instead of causing a duplicate.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := b.Queue.Delete(dctx, m.Handle); err != nil {
		log.Error("sqs delete failed; message may be redelivered", "err", err)
	}
}

func (b *Bridge) logger() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}

// redactAddr logs a recipient as a short hash plus its domain, so a log
// line identifies "which user" without being a mailing list.
func redactAddr(raw string) string {
	addr := strings.TrimSpace(raw)
	if a, err := mail.ParseAddress(addr); err == nil {
		addr = a.Address
	}
	sum := sha256.Sum256([]byte(strings.ToLower(addr)))
	h := hex.EncodeToString(sum[:4])
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		return fmt.Sprintf("%s@%s", h, addr[at+1:])
	}
	return h
}
