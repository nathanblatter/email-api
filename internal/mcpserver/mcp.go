// Package mcpserver exposes sending over the Model Context Protocol
// (streamable HTTP at /mcp) so agents can email directly, the same way they
// use flightdeck. Tools are thin wrappers over the service.
package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathanblatter/email-api/internal/inbox"
	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/service"
)

type attachmentIn struct {
	Filename      string `json:"filename" jsonschema:"file name as the recipient will see it"`
	ContentBase64 string `json:"content_base64" jsonschema:"file bytes, base64-encoded"`
	ContentType   string `json:"content_type,omitempty" jsonschema:"MIME type; inferred from the filename when omitted"`
	ContentID     string `json:"content_id,omitempty" jsonschema:"set to embed inline and reference from HTML as cid:<id>"`
	AsLink        bool   `json:"as_link,omitempty" jsonschema:"upload and send an expiring download link instead of attaching (automatic when the message would exceed the size limit)"`
}

type sendIn struct {
	From        string            `json:"from,omitempty" jsonschema:"sender, must be under an allowed domain, e.g. 'Nathan Blatter <nathan@nathanblatter.com>'; defaults to the configured noreply address"`
	To          []string          `json:"to" jsonschema:"recipient addresses"`
	Cc          []string          `json:"cc,omitempty"`
	Bcc         []string          `json:"bcc,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text,omitempty" jsonschema:"plain-text body (text and/or html required)"`
	HTML        string            `json:"html,omitempty" jsonschema:"HTML body"`
	Attachments []attachmentIn    `json:"attachments,omitempty"`
	Headers     map[string]string `json:"headers,omitempty" jsonschema:"extra headers such as X-Priority; core headers cannot be overridden"`
}

func (in sendIn) toMessage() (*mail.Message, error) {
	m := &mail.Message{
		From: in.From, To: in.To, Cc: in.Cc, Bcc: in.Bcc, ReplyTo: in.ReplyTo,
		Subject: in.Subject, Text: in.Text, HTML: in.HTML, Headers: in.Headers,
	}
	for _, a := range in.Attachments {
		var content []byte
		if err := json.Unmarshal([]byte(`"`+a.ContentBase64+`"`), &content); err != nil {
			return nil, &mail.ValidationError{Msg: "attachment " + a.Filename + ": content_base64 is not valid base64"}
		}
		m.Attachments = append(m.Attachments, mail.Attachment{Filename: a.Filename, ContentType: a.ContentType, Content: content, ContentID: a.ContentID, AsLink: a.AsLink})
	}
	return m, nil
}

type batchIn struct {
	Messages []sendIn `json:"messages" jsonschema:"messages to send, in order"`
}

type emptyIn struct{}

type handlers struct {
	svc *service.Service
	in  *inbox.Service // nil when receiving is not configured
}

func addTool[In, Out any](s *mcpsdk.Server, t *mcpsdk.Tool, fn func(context.Context, In) (Out, error)) {
	mcpsdk.AddTool(s, t, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in In) (*mcpsdk.CallToolResult, any, error) {
		out, err := fn(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		b, err := json.Marshal(out)
		if err != nil {
			return nil, nil, err
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(b)}}}, nil, nil
	})
}

// NewHandler builds the MCP server and returns its streamable-HTTP handler.
func NewHandler(svc *service.Service, in *inbox.Service, version string) http.Handler {
	h := &handlers{svc: svc, in: in}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "email", Version: version}, nil)

	addTool(server, &mcpsdk.Tool{Name: "send_email",
		Description: "Send one email from a nathanblatter.com address via the house relay. Returns status sent (delivered to relay) or queued (relay down or daily budget spent; retried automatically and paged to Nathan's phone)."},
		func(ctx context.Context, in sendIn) (service.Result, error) {
			m, err := in.toMessage()
			if err != nil {
				return service.Result{}, err
			}
			r, err := h.svc.Send(ctx, m)
			if err != nil && !service.IsValidation(err) {
				return r, err
			}
			return r, nil
		})

	addTool(server, &mcpsdk.Tool{Name: "send_batch_email",
		Description: "Send several emails in one call (each with its own recipients/subject/body). Results are per message; one bad message does not stop the others."},
		func(ctx context.Context, in batchIn) (any, error) {
			msgs := make([]*mail.Message, 0, len(in.Messages))
			results := make([]service.Result, 0, len(in.Messages))
			for _, s := range in.Messages {
				m, err := s.toMessage()
				if err != nil {
					results = append(results, service.Result{Status: "rejected", Error: err.Error()})
					continue
				}
				msgs = append(msgs, m)
			}
			results = append(results, h.svc.SendBatch(ctx, msgs)...)
			return results, nil
		})

	addTool(server, &mcpsdk.Tool{Name: "email_health",
		Description: "Relay/iMessage/quota status, today's sent count against the daily budget, and how many messages are queued."},
		func(ctx context.Context, _ emptyIn) (service.Health, error) { return h.svc.Health(ctx), nil })

	addTool(server, &mcpsdk.Tool{Name: "list_queued_emails",
		Description: "List emails waiting in the spool (relay down or over budget) with reason, attempts and last error."},
		func(ctx context.Context, _ emptyIn) ([]service.QueueEntry, error) { return h.svc.Queue() })

	addTool(server, &mcpsdk.Tool{Name: "retry_queued_emails",
		Description: "Run one retry pass over the spool now instead of waiting for the next scheduled pass."},
		func(ctx context.Context, _ emptyIn) (map[string]int, error) {
			d, r, dead := h.svc.RetryOnce(ctx)
			return map[string]int{"delivered": d, "remaining": r, "dead": dead}, nil
		})

	if in != nil {
		h.registerInbox(server)
	}
	return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
}
