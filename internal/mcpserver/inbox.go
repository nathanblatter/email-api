package mcpserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathanblatter/email-api/internal/inbox"
)

type listInboxIn struct {
	Limit      int    `json:"limit,omitempty" jsonschema:"max messages to return (default 50)"`
	Unread     bool   `json:"unread,omitempty" jsonschema:"only unread messages"`
	Query      string `json:"query,omitempty" jsonschema:"substring to match in sender, subject or body"`
	Suspicious string `json:"suspicious,omitempty" jsonschema:"'hide' (default) hides mail that failed both SPF and DKIM; 'only' shows just those; 'all' shows everything"`
}

type idIn struct {
	ID string `json:"id" jsonschema:"message id from list_inbox"`
}

type markReadIn struct {
	ID   string `json:"id" jsonschema:"message id"`
	Read *bool  `json:"read,omitempty" jsonschema:"true (default) marks read, false marks unread"`
}

type getAttachmentIn struct {
	ID           string `json:"id" jsonschema:"message id"`
	AttachmentID string `json:"attachment_id" jsonschema:"attachment id from read_email"`
}

type attachmentOut struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	Size          int    `json:"size"`
	ContentBase64 string `json:"content_base64,omitempty"`
	Note          string `json:"note,omitempty"`
}

const maxInlineAttachment = 5 << 20

func (h *handlers) registerInbox(server *mcpsdk.Server) {
	addTool(server, &mcpsdk.Tool{Name: "list_inbox",
		Description: "List received email for @nathanblatter.com (newest first) with sender, subject, preview, read state and attachment count. Mail is received by Cloudflare Email Routing and stored here; nothing lives in a third-party mailbox."},
		func(ctx context.Context, in listInboxIn) (any, error) {
			o := inbox.ListOptions{Limit: in.Limit, Unread: in.Unread, Query: in.Query}
			switch in.Suspicious {
			case "all":
			case "only":
				t := true
				o.Suspicious = &t
			default:
				f := false
				o.Suspicious = &f
			}
			list, err := h.in.Store.List(ctx, o)
			if err != nil {
				return nil, err
			}
			unread, _ := h.in.Store.Unread(ctx)
			return map[string]any{"unread": unread, "count": len(list), "messages": list}, nil
		})

	addTool(server, &mcpsdk.Tool{Name: "read_email",
		Description: "Fetch one received email in full: headers, text and HTML bodies, threading ids (use message_id as In-Reply-To when replying via send_email) and attachment metadata."},
		func(ctx context.Context, in idIn) (*inbox.Message, error) { return h.in.Store.Get(ctx, in.ID) })

	addTool(server, &mcpsdk.Tool{Name: "get_email_attachment",
		Description: "Return an attachment's bytes (base64) for a received email. Files over 5 MB are not inlined; fetch them over HTTP from GET /inbox/{id}/attachments/{attachment_id} instead."},
		func(ctx context.Context, in getAttachmentIn) (attachmentOut, error) {
			meta, body, err := h.in.Store.Attachment(ctx, in.ID, in.AttachmentID)
			if err != nil {
				return attachmentOut{}, err
			}
			defer body.Close()
			out := attachmentOut{Filename: meta.Filename, ContentType: meta.ContentType, Size: meta.Size}
			if meta.Size > maxInlineAttachment {
				out.Note = fmt.Sprintf("too large to inline; GET /inbox/%s/attachments/%s", in.ID, in.AttachmentID)
				return out, nil
			}
			b, err := io.ReadAll(body)
			if err != nil {
				return out, err
			}
			out.ContentBase64 = base64.StdEncoding.EncodeToString(b)
			return out, nil
		})

	addTool(server, &mcpsdk.Tool{Name: "mark_email_read", Description: "Mark a received email read (or unread)."},
		func(ctx context.Context, in markReadIn) (okOut, error) {
			read := in.Read == nil || *in.Read
			return okOut{OK: true}, h.in.Store.MarkRead(ctx, in.ID, read)
		})

	addTool(server, &mcpsdk.Tool{Name: "delete_email", Description: "Delete a received email and its stored attachments. Irreversible."},
		func(ctx context.Context, in idIn) (okOut, error) {
			return okOut{OK: true}, h.in.Store.Delete(ctx, in.ID)
		})
}

type okOut struct {
	OK bool `json:"ok"`
}
