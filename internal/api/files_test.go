package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nathanblatter/email-api/internal/files"
)

func TestFilesDownload(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	store := &files.Memory{PublicURL: "https://file.nathanblatter.com", TTL: time.Hour, Now: func() time.Time { return now }}
	link, _ := store.Put(context.Background(), "../evil/report q.pdf", "application/pdf", []byte("%PDF-1.4"))
	if !strings.HasPrefix(link.URL, "https://file.nathanblatter.com/f/") || !strings.HasSuffix(link.URL, "/report%20q.pdf") {
		t.Fatalf("url %s", link.URL)
	}
	srv := httptest.NewServer(Files(store, slog.Default()))
	defer srv.Close()

	path := strings.TrimPrefix(link.URL, "https://file.nathanblatter.com")
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(body) != "%PDF-1.4" || res.Header.Get("Content-Type") != "application/pdf" ||
		!strings.Contains(res.Header.Get("Content-Disposition"), "report q.pdf") {
		t.Fatalf("%d %s %v", res.StatusCode, body, res.Header)
	}
	for _, p := range []string{"/f/deadbeef/x", "/f/" + strings.Repeat("0", 32) + "/nope", "/", "/health", "/send"} {
		if r, _ := http.Get(srv.URL + p); r.StatusCode != 404 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	now = now.Add(2 * time.Hour)
	if r, _ := http.Get(srv.URL + path); r.StatusCode != 404 {
		t.Fatalf("expired link should 404, got %d", r.StatusCode)
	}
}
