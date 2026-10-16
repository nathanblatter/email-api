package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeQueue hands out its messages once, then blocks until ctx is cancelled
// (like a long poll on an empty queue).
type fakeQueue struct {
	mu       sync.Mutex
	msgs     []Message
	deleted  []string
	received int
	recvErr  error // returned once on the first Receive
}

func (q *fakeQueue) Receive(ctx context.Context) ([]Message, error) {
	q.mu.Lock()
	q.received++
	if q.recvErr != nil {
		err := q.recvErr
		q.recvErr = nil
		q.mu.Unlock()
		return nil, err
	}
	if len(q.msgs) > 0 {
		out := q.msgs
		q.msgs = nil
		q.mu.Unlock()
		return out, nil
	}
	q.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (q *fakeQueue) Delete(_ context.Context, handle string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deleted = append(q.deleted, handle)
	return nil
}

func (q *fakeQueue) deletedHandles() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.deleted...)
}

type fakeSender struct {
	mu      sync.Mutex
	outcome Outcome
	err     error
	got     []Request
}

func (s *fakeSender) Send(_ context.Context, req Request) (Outcome, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, req)
	return s.outcome, s.outcome.String(), s.err
}

func (s *fakeSender) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// runOnce drives the loop with the given messages until the queue is drained,
// then cancels. The queue's Receive blocks on an empty queue, so cancelling
// after a short settle is how we stop.
func runOnce(t *testing.T, q *fakeQueue, s Sender) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bridge{Queue: q, Sender: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ErrBackoff: time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	// Wait until the queue has been polled at least twice (drained, then blocking).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		n := q.received
		q.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

const goodBody = `{"to":"user@example.com","subject":"Verify","text":"code 1234"}`

func TestAcceptedDeletes(t *testing.T) {
	q := &fakeQueue{msgs: []Message{{ID: "1", Handle: "h1", Body: goodBody}}}
	s := &fakeSender{outcome: Accepted}
	runOnce(t, q, s)
	if got := q.deletedHandles(); len(got) != 1 || got[0] != "h1" {
		t.Fatalf("deleted = %v, want [h1]", got)
	}
	if s.calls() != 1 || s.got[0].To != "user@example.com" || s.got[0].Subject != "Verify" || s.got[0].Text != "code 1234" || s.got[0].From != "" {
		t.Fatalf("sender got %+v", s.got)
	}
}

func TestRejectedDeletes(t *testing.T) {
	q := &fakeQueue{msgs: []Message{{ID: "1", Handle: "h1", Body: goodBody}}}
	s := &fakeSender{outcome: Rejected}
	runOnce(t, q, s)
	if got := q.deletedHandles(); len(got) != 1 {
		t.Fatalf("rejected message should be deleted; deleted = %v", got)
	}
}

func TestRetryKeeps(t *testing.T) {
	q := &fakeQueue{msgs: []Message{{ID: "1", Handle: "h1", Body: goodBody}, {ID: "2", Handle: "h2", Body: goodBody}}}
	s := &fakeSender{outcome: Retry, err: errors.New("dial tcp: connection refused")}
	runOnce(t, q, s)
	if got := q.deletedHandles(); len(got) != 0 {
		t.Fatalf("transient failure must not delete; deleted = %v", got)
	}
	if s.calls() != 2 {
		t.Fatalf("sender calls = %d, want 2", s.calls())
	}
}

func TestBadJSONDeletesWithoutSending(t *testing.T) {
	q := &fakeQueue{msgs: []Message{
		{ID: "1", Handle: "h1", Body: `{not json`},
		{ID: "2", Handle: "h2", Body: `{"to":"a@b.c","subject":"s","text":"t","bogus":1}`},
	}}
	s := &fakeSender{outcome: Accepted}
	runOnce(t, q, s)
	if got := q.deletedHandles(); len(got) != 2 {
		t.Fatalf("malformed messages should be deleted; deleted = %v", got)
	}
	if s.calls() != 0 {
		t.Fatalf("malformed messages must not reach the sender; calls = %d", s.calls())
	}
}

func TestReceiveErrorBacksOffAndContinues(t *testing.T) {
	q := &fakeQueue{recvErr: errors.New("sqs down"), msgs: []Message{{ID: "1", Handle: "h1", Body: goodBody}}}
	s := &fakeSender{outcome: Accepted}
	runOnce(t, q, s)
	if got := q.deletedHandles(); len(got) != 1 {
		t.Fatalf("loop should survive a receive error; deleted = %v", got)
	}
}

func TestShutdownWhileBlockedOnReceive(t *testing.T) {
	q := &fakeQueue{}
	b := &Bridge{Queue: q, Sender: &fakeSender{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on shutdown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRedactAddr(t *testing.T) {
	got := redactAddr("User Name <User@Example.com>")
	if len(got) != 8+1+len("Example.com") || got[8:] != "@Example.com" {
		t.Fatalf("redactAddr = %q", got)
	}
	if got == "user@example.com" {
		t.Fatal("local part leaked")
	}
}

// TestHTTPSenderMapping checks the status-code → Outcome table and the exact
// body shape POSTed to /send.
func TestHTTPSenderMapping(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want Outcome
	}{
		{"sent", 200, `{"id":"abc","status":"sent"}`, Accepted},
		{"queued", 202, `{"id":"abc","status":"queued"}`, Accepted},
		{"rejected", 400, `{"status":"rejected","error":"invalid to address"}`, Rejected},
		{"unauthorized", 401, `{"error":"unauthorized"}`, Retry},
		{"rate limited", 429, ``, Retry},
		{"failed", 500, `{"status":"failed","error":"relay"}`, Retry},
		{"bad gateway", 502, `<html>`, Retry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotKey string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/send" || r.Method != http.MethodPost {
					t.Errorf("got %s %s", r.Method, r.URL.Path)
				}
				gotKey = r.Header.Get("X-API-Key")
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			h := &HTTPSender{BaseURL: srv.URL + "/", APIKey: "k"}
			out, _, _ := h.Send(context.Background(), Request{To: "u@x.com", Subject: "s", Text: "t"})
			if out != tc.want {
				t.Fatalf("outcome = %v, want %v", out, tc.want)
			}
			if gotKey != "k" {
				t.Fatalf("X-API-Key = %q", gotKey)
			}
			if _, has := gotBody["from"]; has {
				t.Fatalf("empty from must be omitted; body = %v", gotBody)
			}
			if gotBody["to"] != "u@x.com" || gotBody["subject"] != "s" || gotBody["text"] != "t" {
				t.Fatalf("body = %v", gotBody)
			}
		})
	}
}

func TestHTTPSenderFromPassedThrough(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"status":"sent"}`)
	}))
	defer srv.Close()
	h := &HTTPSender{BaseURL: srv.URL, APIKey: "k"}
	if out, _, _ := h.Send(context.Background(), Request{To: "u@x.com", Subject: "s", Text: "t", From: "Auth <auth@nathanblatter.com>"}); out != Accepted {
		t.Fatalf("outcome = %v", out)
	}
	if gotBody["from"] != "Auth <auth@nathanblatter.com>" {
		t.Fatalf("from = %v", gotBody["from"])
	}
}

func TestHTTPSenderNetworkErrorRetries(t *testing.T) {
	h := &HTTPSender{BaseURL: "http://127.0.0.1:1", APIKey: "k", Client: &http.Client{Timeout: time.Second}}
	out, _, err := h.Send(context.Background(), Request{To: "u@x.com", Subject: "s", Text: "t"})
	if out != Retry || err == nil {
		t.Fatalf("outcome = %v err = %v", out, err)
	}
}
