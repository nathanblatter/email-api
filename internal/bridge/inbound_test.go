package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const sesReceived = `{"notificationType":"Received",
 "mail":{"source":"alice@example.com","destination":["nathan@byuisresearch.com"],"messageId":"abc123"},
 "receipt":{"spfVerdict":{"status":"PASS"},"dkimVerdict":{"status":"FAIL"},"dmarcVerdict":{"status":"GRAY"},
  "action":{"type":"S3","bucketName":"bkt","objectKey":"abc123"}}}`

type fakeObjects struct {
	data map[string][]byte
	err  error
}

func (f *fakeObjects) Get(_ context.Context, bucket, key string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.data[bucket+"/"+key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return b, nil
}

type fakePoster struct {
	mu      sync.Mutex
	outcome Outcome
	got     [][]byte
	envTo   []string
}

func (p *fakePoster) Post(_ context.Context, raw []byte, _, envTo string) (Outcome, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, raw)
	p.envTo = append(p.envTo, envTo)
	if p.outcome == Retry {
		return Retry, "http 503", errors.New("down")
	}
	return p.outcome, "http 200", nil
}

func runInbound(t *testing.T, q *fakeQueue, o ObjectStore, p InboundPoster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b := &Inbound{Queue: q, Objects: o, Poster: p, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ErrBackoff: time.Millisecond}
	done := make(chan struct{})
	go func() { _ = b.Run(ctx); close(done) }()
	deadline := time.After(time.Second)
	for {
		q.mu.Lock()
		n := q.received
		q.mu.Unlock()
		if n >= 2 { // first call drained the messages, second is blocking on an empty queue
			break
		}
		select {
		case <-deadline:
			t.Fatal("bridge never drained the queue")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestInboundRelaysAndPrependsAuthResults(t *testing.T) {
	q := &fakeQueue{msgs: []Message{{ID: "1", Handle: "h1", Body: sesReceived}}}
	o := &fakeObjects{data: map[string][]byte{"bkt/abc123": []byte("Subject: hi\r\n\r\nbody\r\n")}}
	p := &fakePoster{outcome: Accepted}
	runInbound(t, q, o, p)
	if got := q.deletedHandles(); len(got) != 1 || got[0] != "h1" {
		t.Fatalf("deleted = %v, want [h1]", got)
	}
	if len(p.got) != 1 {
		t.Fatalf("posted %d messages, want 1", len(p.got))
	}
	want := "Authentication-Results: amazonses.com; spf=pass; dkim=fail; dmarc=neutral\r\nSubject: hi\r\n\r\nbody\r\n"
	if string(p.got[0]) != want {
		t.Fatalf("raw =\n%q\nwant\n%q", p.got[0], want)
	}
	if p.envTo[0] != "nathan@byuisresearch.com" {
		t.Fatalf("envelope to = %q", p.envTo[0])
	}
}

func TestInboundAcceptsSNSEnvelope(t *testing.T) {
	env, _ := json.Marshal(map[string]string{"Type": "Notification", "Message": sesReceived})
	q := &fakeQueue{msgs: []Message{{ID: "1", Handle: "h1", Body: string(env)}}}
	o := &fakeObjects{data: map[string][]byte{"bkt/abc123": []byte("x")}}
	p := &fakePoster{outcome: Accepted}
	runInbound(t, q, o, p)
	if len(p.got) != 1 || len(q.deletedHandles()) != 1 {
		t.Fatalf("posted=%d deleted=%d", len(p.got), len(q.deletedHandles()))
	}
}

func TestInboundSkipsNonReceipts(t *testing.T) {
	q := &fakeQueue{msgs: []Message{
		{ID: "1", Handle: "sub", Body: `{"Type":"SubscriptionConfirmation","Message":"x"}`},
		{ID: "2", Handle: "bounce", Body: `{"notificationType":"Bounce"}`},
		{ID: "3", Handle: "junk", Body: `not json`},
	}}
	p := &fakePoster{outcome: Accepted}
	runInbound(t, q, &fakeObjects{}, p)
	if len(p.got) != 0 {
		t.Fatalf("posted %d, want 0", len(p.got))
	}
	if got := q.deletedHandles(); len(got) != 3 {
		t.Fatalf("deleted = %v, want all three", got)
	}
}

func TestInboundKeepsMessageWhenS3OrAPIIsDown(t *testing.T) {
	q := &fakeQueue{msgs: []Message{{ID: "1", Handle: "h1", Body: sesReceived}}}
	runInbound(t, q, &fakeObjects{err: errors.New("s3 down")}, &fakePoster{outcome: Accepted})
	if got := q.deletedHandles(); len(got) != 0 {
		t.Fatalf("deleted %v after s3 failure, want none", got)
	}

	q = &fakeQueue{msgs: []Message{{ID: "1", Handle: "h1", Body: sesReceived}}}
	o := &fakeObjects{data: map[string][]byte{"bkt/abc123": []byte("x")}}
	runInbound(t, q, o, &fakePoster{outcome: Retry})
	if got := q.deletedHandles(); len(got) != 0 {
		t.Fatalf("deleted %v after api failure, want none", got)
	}

	q = &fakeQueue{msgs: []Message{{ID: "1", Handle: "h1", Body: sesReceived}}}
	runInbound(t, q, o, &fakePoster{outcome: Rejected})
	if got := q.deletedHandles(); len(got) != 1 {
		t.Fatalf("deleted %v after rejection, want [h1]", got)
	}
}

func TestHTTPPosterOutcomes(t *testing.T) {
	var gotHeaders http.Header
	var gotBody []byte
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inbound" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"stored","id":"m1","suspicious":true}`))
	}))
	defer srv.Close()
	p := &HTTPPoster{BaseURL: srv.URL, Secret: "s3cret"}

	out, detail, err := p.Post(context.Background(), []byte("raw"), "a@x", "b@y")
	if out != Accepted || err != nil || !strings.Contains(detail, "id=m1") || !strings.Contains(detail, "suspicious") {
		t.Fatalf("200 → %v %q %v", out, detail, err)
	}
	if gotHeaders.Get("X-Inbound-Secret") != "s3cret" || gotHeaders.Get("X-Envelope-From") != "a@x" ||
		gotHeaders.Get("X-Envelope-To") != "b@y" || gotHeaders.Get("Content-Type") != "message/rfc822" || !bytes.Equal(gotBody, []byte("raw")) {
		t.Fatalf("request headers/body wrong: %v %q", gotHeaders, gotBody)
	}
	for code, want := range map[int]Outcome{401: Retry, 413: Rejected, 400: Rejected, 429: Retry, 500: Retry} {
		status = code
		if out, _, _ := p.Post(context.Background(), []byte("raw"), "", ""); out != want {
			t.Errorf("%d → %v, want %v", code, out, want)
		}
	}
}
