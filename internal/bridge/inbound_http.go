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

// HTTPPoster posts raw messages to email-api's POST /inbound on the public
// listener (reached internally as http://app:4501), authenticated with the
// same shared secret the Cloudflare Email Worker uses.
type HTTPPoster struct {
	BaseURL string // e.g. http://app:4501
	Secret  string
	Client  *http.Client
}

type inboundReply struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Suspicious bool   `json:"suspicious"`
	Error      string `json:"error"`
}

// Post maps the HTTP result onto an Outcome with the same rules as
// HTTPSender.Send: 2xx accepted, 401/403/429 retry (fixable server-side),
// other 4xx rejected, 5xx/transport retry.
func (h *HTTPPoster) Post(ctx context.Context, raw []byte, envFrom, envTo string) (Outcome, string, error) {
	url := strings.TrimRight(h.BaseURL, "/") + "/inbound"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return Retry, "", err
	}
	hr.Header.Set("Content-Type", "message/rfc822")
	hr.Header.Set("X-Inbound-Secret", h.Secret)
	hr.Header.Set("X-Envelope-From", envFrom)
	hr.Header.Set("X-Envelope-To", envTo)
	hr.Header.Set("User-Agent", "email-api-ses-bridge")

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := client.Do(hr)
	if err != nil {
		return Retry, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var rep inboundReply
	_ = json.Unmarshal(body, &rep)
	detail := fmt.Sprintf("http %d", resp.StatusCode)
	if rep.Status != "" {
		detail += " " + rep.Status
	}
	if rep.ID != "" {
		detail += " id=" + rep.ID
	}
	if rep.Suspicious {
		detail += " suspicious"
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
