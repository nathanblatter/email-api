package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPSender posts to email-api's keyed POST /send.
type HTTPSender struct {
	BaseURL string // e.g. http://app:4500
	APIKey  string
	Client  *http.Client
}

// sendReply is the part of email-api's /send response the bridge reads.
type sendReply struct {
	ID     string `json:"id"`
	Status string `json:"status"` // sent | queued | rejected | failed
	Error  string `json:"error"`
}

// Send maps the HTTP result onto an Outcome:
//
//	200/202 (status sent/queued) → Accepted
//	other 2xx                    → Accepted (be liberal; the server took it)
//	429                          → Retry   (budget/rate limit; SQS redelivers)
//	401/403                      → Retry   (key problem: fixable server-side,
//	                                        dropping mail would hide it)
//	other 4xx                    → Rejected
//	5xx / transport error        → Retry
func (h *HTTPSender) Send(ctx context.Context, req Request) (Outcome, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Rejected, "encode: " + err.Error(), err
	}
	url := strings.TrimRight(h.BaseURL, "/") + "/send"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Retry, "", err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("X-API-Key", h.APIKey)
	hr.Header.Set("User-Agent", "email-api-sqs-bridge")

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(hr)
	if err != nil {
		return Retry, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var rep sendReply
	_ = json.Unmarshal(raw, &rep)
	detail := fmt.Sprintf("http %d", resp.StatusCode)
	if rep.Status != "" {
		detail += " " + rep.Status
	}
	if rep.ID != "" {
		detail += " id=" + rep.ID
	}
	if rep.Error != "" {
		detail += ": " + rep.Error
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Accepted, detail, nil
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return Retry, detail, fmt.Errorf("email-api: %s", detail)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return Rejected, detail, nil
	default:
		return Retry, detail, fmt.Errorf("email-api: %s", detail)
	}
}
