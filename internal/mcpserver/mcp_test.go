package mcpserver

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/quota"
	"github.com/nathanblatter/email-api/internal/service"
	"github.com/nathanblatter/email-api/internal/spool"
)

type okSender struct{ n int }

func (s *okSender) Send(context.Context, string, []string, []byte) error { s.n++; return nil }
func (s *okSender) Ping(context.Context) error                           { return nil }

func TestToolsOverStreamableHTTP(t *testing.T) {
	sp, _ := spool.Open(t.TempDir())
	snd := &okSender{}
	svc := service.New(snd, nil, sp, quota.New(100, ""), nil, slog.Default(), service.Options{
		Policy: mail.Policy{DefaultFrom: "noreply@nathanblatter.com", AllowedDomains: []string{"nathanblatter.com"}},
	})
	srv := httptest.NewServer(NewHandler(svc, nil, "test"))
	defer srv.Close()

	ctx := context.Background()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil)
	sess, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	for _, want := range []string{"send_email", "send_batch_email", "email_health", "list_queued_emails", "retry_queued_emails"} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Fatalf("missing tool %s in %v", want, names)
		}
	}

	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "send_email", Arguments: map[string]any{
		"to": []string{"a@x.com"}, "subject": "hi", "text": "body",
		"attachments": []map[string]any{{"filename": "a.txt", "content_base64": "aGVsbG8="}},
	}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	txt := res.Content[0].(*mcpsdk.TextContent).Text
	if !strings.Contains(txt, `"status":"sent"`) || snd.n != 1 {
		t.Fatalf("%s", txt)
	}

	// Validation errors come back as a normal rejected result, not a protocol error.
	res, err = sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "send_email", Arguments: map[string]any{
		"from": "x@gmail.com", "to": []string{"a@x.com"}, "subject": "hi", "text": "body"}})
	if err != nil {
		t.Fatal(err)
	}
	if txt = res.Content[0].(*mcpsdk.TextContent).Text; !strings.Contains(txt, "rejected") {
		t.Fatalf("%s", txt)
	}

	res, _ = sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "email_health", Arguments: map[string]any{}})
	if txt = res.Content[0].(*mcpsdk.TextContent).Text; !strings.Contains(txt, `"sent_today":1`) {
		t.Fatalf("%s", txt)
	}
}
