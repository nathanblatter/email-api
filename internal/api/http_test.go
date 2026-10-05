package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/quota"
	"github.com/nathanblatter/email-api/internal/service"
	"github.com/nathanblatter/email-api/internal/spool"
)

type fakeSender struct {
	fail error
	raws [][]byte
}

func (f *fakeSender) Send(_ context.Context, _ string, _ []string, raw []byte) error {
	if f.fail != nil {
		return f.fail
	}
	f.raws = append(f.raws, raw)
	return nil
}
func (f *fakeSender) Ping(context.Context) error { return f.fail }

func newServer(t *testing.T) (*httptest.Server, *fakeSender) {
	t.Helper()
	sp, _ := spool.Open(t.TempDir())
	snd := &fakeSender{}
	svc := service.New(snd, nil, sp, quota.New(100, ""), nil, slog.Default(), service.Options{
		Policy: mail.Policy{DefaultFrom: "noreply@nathanblatter.com", AllowedDomains: []string{"nathanblatter.com"}, MaxBytes: 1 << 20},
	})
	srv := httptest.NewServer(New(svc, "secret", 1<<20, slog.Default(), nil))
	t.Cleanup(srv.Close)
	return srv, snd
}

func do(t *testing.T, srv *httptest.Server, method, path, key, ctype string, body io.Reader) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, body)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestHealthIsOpen(t *testing.T) {
	srv, _ := newServer(t)
	if code, out := do(t, srv, "GET", "/health", "", "", nil); code != 200 || out["relay"] != "ok" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestAuth(t *testing.T) {
	srv, _ := newServer(t)
	body := `{"to":"a@x.com","subject":"s","text":"t"}`
	if code, _ := do(t, srv, "POST", "/send", "", "application/json", strings.NewReader(body)); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := do(t, srv, "POST", "/send", "wrong", "application/json", strings.NewReader(body)); code != 401 {
		t.Fatalf("wrong key: %d", code)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/send", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("bearer: %d", res.StatusCode)
	}
}

func TestSendJSON(t *testing.T) {
	srv, snd := newServer(t)
	body := `{"from":"Nate <nate@nathanblatter.com>","to":["a@x.com"],"cc":"c@x.com","bcc":["b@x.com"],"subject":"s","html":"<b>hi</b>",
	          "attachments":[{"filename":"a.txt","content":"aGVsbG8="}]}`
	code, out := do(t, srv, "POST", "/send", "secret", "application/json", strings.NewReader(body))
	if code != 200 || out["status"] != "sent" {
		t.Fatalf("%d %v", code, out)
	}
	raw := string(snd.raws[0])
	if !strings.Contains(raw, "From: \"Nate\" <nate@nathanblatter.com>") || !strings.Contains(raw, "aGVsbG8=") || strings.Contains(raw, "b@x.com") {
		t.Fatalf("raw:\n%s", raw)
	}
	if code, out := do(t, srv, "POST", "/send", "secret", "application/json", strings.NewReader(`{"to":"a@x.com","subject":"s","text":"t","bogus":1}`)); code != 400 || out["status"] != "rejected" {
		t.Fatalf("unknown field: %d %v", code, out)
	}
	if code, _ := do(t, srv, "POST", "/send", "secret", "application/json", strings.NewReader(`{"from":"x@gmail.com","to":"a@x.com","subject":"s","text":"t"}`)); code != 400 {
		t.Fatalf("bad from: %d", code)
	}
}

func TestSendMultipart(t *testing.T) {
	srv, snd := newServer(t)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("to", "a@x.com")
	_ = w.WriteField("to", "b@x.com")
	_ = w.WriteField("subject", "files")
	_ = w.WriteField("text", "see attached")
	fw, _ := w.CreateFormFile("file", "notes.txt")
	fw.Write([]byte("hello notes"))
	fw, _ = w.CreateFormFile("file", "data.csv")
	fw.Write([]byte("a,b\n1,2\n"))
	w.Close()
	code, out := do(t, srv, "POST", "/send", "secret", w.FormDataContentType(), &buf)
	if code != 200 || out["status"] != "sent" {
		t.Fatalf("%d %v", code, out)
	}
	rc := out["recipients"].([]any)
	if len(rc) != 2 {
		t.Fatalf("recipients %v", rc)
	}
	raw := string(snd.raws[0])
	if !strings.Contains(raw, `filename=notes.txt`) || !strings.Contains(raw, `filename=data.csv`) || !strings.Contains(raw, "text/csv") {
		t.Fatalf("raw:\n%s", raw)
	}
}

func TestQueuedIs202(t *testing.T) {
	srv, snd := newServer(t)
	snd.fail = errors.New("down")
	code, out := do(t, srv, "POST", "/send", "secret", "application/json", strings.NewReader(`{"to":"a@x.com","subject":"s","text":"t"}`))
	if code != 202 || out["status"] != "queued" || out["reason"] != "relay_down" || out["fallback"] != "none" {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := do(t, srv, "GET", "/queue", "secret", "", nil); code != 200 || out["count"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	snd.fail = nil
	if code, out := do(t, srv, "POST", "/queue/retry", "secret", "", nil); code != 200 || out["delivered"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
}

func TestBatch(t *testing.T) {
	srv, _ := newServer(t)
	body := `{"messages":[{"to":"a@x.com","subject":"1","text":"t"},{"to":"nope","subject":"2","text":"t"},{"to":"c@x.com","subject":"3","text":"t"}]}`
	code, out := do(t, srv, "POST", "/send/batch", "secret", "application/json", strings.NewReader(body))
	if code != 200 || out["status"] != "partial" || out["sent"].(float64) != 2 || out["rejected"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := do(t, srv, "POST", "/send/batch", "secret", "application/json", strings.NewReader(`{"messages":[]}`)); code != 400 {
		t.Fatalf("empty batch: %d", code)
	}
}
