package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nathanblatter/email-api/internal/auth"
	"github.com/nathanblatter/email-api/internal/inbox"
	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/quota"
	"github.com/nathanblatter/email-api/internal/service"
	"github.com/nathanblatter/email-api/internal/spool"
)

const rawMail = "From: Jane <jane@example.com>\r\nTo: hello@nathanblatter.com\r\nSubject: Hello\r\n" +
	"Authentication-Results: mx; spf=pass; dkim=pass\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: text/plain\r\n\r\nhi there\r\n--b\r\nContent-Type: text/csv; name=a.csv\r\nContent-Disposition: attachment; filename=a.csv\r\n\r\nx,y\r\n--b--\r\n"

func TestInboundAndInboxRoutes(t *testing.T) {
	in := &inbox.Service{Store: inbox.NewMemory(), Secret: "hook"}
	pub := httptest.NewServer(Files(nil, in, slog.Default()))
	defer pub.Close()
	sp, _ := spool.Open(t.TempDir())
	svc := service.New(&fakeSender{}, nil, sp, quota.New(100, ""), nil, slog.Default(), service.Options{
		Policy: mail.Policy{DefaultFrom: "noreply@nathanblatter.com", AllowedDomains: []string{"nathanblatter.com"}}})
	api := httptest.NewServer(New(svc, auth.NewStatic("secret"), 1<<20, slog.Default(), nil, in))
	defer api.Close()

	post := func(secret string, body string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", pub.URL+"/inbound", strings.NewReader(body))
		req.Header.Set("X-Inbound-Secret", secret)
		req.Header.Set("X-Envelope-From", "jane@example.com")
		req.Header.Set("X-Envelope-To", "hello@nathanblatter.com")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = jsonDecode(res.Body, &out)
		return res.StatusCode, out
	}
	if code, _ := post("wrong", rawMail); code != 401 {
		t.Fatalf("bad secret: %d", code)
	}
	if code, _ := post("hook", ""); code != 400 {
		t.Fatalf("empty: %d", code)
	}
	code, out := post("hook", rawMail)
	if code != 200 || out["status"] != "stored" || out["attachments"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	id := out["id"].(string)

	// The public listener must not expose the inbox itself.
	if r, _ := http.Get(pub.URL + "/inbox"); r.StatusCode != 404 {
		t.Fatalf("public /inbox should 404, got %d", r.StatusCode)
	}

	c, o := do(t, api, "GET", "/inbox", "secret", "", nil)
	if c != 200 || o["count"].(float64) != 1 || o["unread"].(float64) != 1 {
		t.Fatalf("%d %v", c, o)
	}
	if c, _ := do(t, api, "GET", "/inbox", "", "", nil); c != 401 {
		t.Fatalf("inbox needs key: %d", c)
	}
	c, o = do(t, api, "GET", "/inbox/"+id, "secret", "", nil)
	if c != 200 || o["subject"] != "Hello" || !strings.Contains(o["text"].(string), "hi there") {
		t.Fatalf("%d %v", c, o)
	}
	aid := o["attachments"].([]any)[0].(map[string]any)["id"].(string)
	req, _ := http.NewRequest("GET", api.URL+"/inbox/"+id+"/attachments/"+aid, nil)
	req.Header.Set("X-API-Key", "secret")
	res, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(b) != "x,y" || !strings.Contains(res.Header.Get("Content-Disposition"), "a.csv") {
		t.Fatalf("%d %q %v", res.StatusCode, b, res.Header)
	}
	if c, _ := do(t, api, "POST", "/inbox/"+id+"/read", "secret", "", nil); c != 200 {
		t.Fatalf("mark read %d", c)
	}
	if _, o := do(t, api, "GET", "/inbox", "secret", "", nil); o["unread"].(float64) != 0 {
		t.Fatalf("unread after read: %v", o)
	}
	if c, _ := do(t, api, "DELETE", "/inbox/"+id, "secret", "", nil); c != 200 {
		t.Fatalf("delete %d", c)
	}
	if c, _ := do(t, api, "GET", "/inbox/"+id, "secret", "", nil); c != 404 {
		t.Fatalf("after delete %d", c)
	}
	_ = context.Background()
}

func jsonDecode(r io.Reader, v any) error {
	return jsonNewDecoder(r).Decode(v)
}
