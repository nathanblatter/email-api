// Package fallback pages the on-call phone through the local iMessage API
// (the same bridge ai-therapist and the uptime check use) when mail cannot
// go out. An email cannot be rerouted to an arbitrary recipient over iMessage,
// so the fallback delivers the *content* to Nathan and the mail itself waits
// in the spool.
package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Pager is the minimal iMessage bridge contract.
type Pager interface {
	Page(ctx context.Context, text string) error
	Ping(ctx context.Context) error
}

type IMessage struct {
	URL       string
	APIKey    string
	Recipient string
	Client    *http.Client
}

func (p *IMessage) Configured() bool {
	return p != nil && p.URL != "" && p.APIKey != "" && p.Recipient != ""
}

func (p *IMessage) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (p *IMessage) Page(ctx context.Context, text string) error {
	if !p.Configured() {
		return errors.New("imessage fallback not configured (IMESSAGE_API_KEY / IMESSAGE_FALLBACK_RECIPIENT)")
	}
	body, _ := json.Marshal(map[string]string{"recipient": p.Recipient, "message": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", p.APIKey)
	res, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("imessage api %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func (p *IMessage) Ping(ctx context.Context) error {
	if p == nil || p.URL == "" {
		return errors.New("imessage fallback not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.URL, "/")+"/health", nil)
	if err != nil {
		return err
	}
	c := p.client()
	c.Timeout = 5 * time.Second
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("imessage api health %d", res.StatusCode)
	}
	return nil
}
