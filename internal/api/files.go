package api

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"

	"github.com/nathanblatter/email-api/internal/files"
	"github.com/nathanblatter/email-api/internal/inbox"
)

// Files is the public listener: no API key, two routes (downloads and the
// secret-guarded inbound hook), nothing else. It is what the Cloudflare
// tunnel points at; the keyed API never leaves the tailnet.
func Files(store files.Store, in *inbox.Service, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	if in != nil {
		mux.Handle("POST /inbound", Inbound(in, log))
	}
	mux.HandleFunc("GET /f/{token}/{name}", func(w http.ResponseWriter, r *http.Request) {
		obj, err := store.Get(r.Context(), r.PathValue("token"), r.PathValue("name"))
		if err != nil {
			if !errors.Is(err, files.ErrNotFound) {
				log.Error("file fetch failed", "err", err)
			}
			http.Error(w, "This download link is invalid or has expired.", http.StatusNotFound)
			return
		}
		defer obj.Body.Close()
		ct := obj.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": r.PathValue("name")}))
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !obj.Expires.IsZero() {
			w.Header().Set("Expires", obj.Expires.UTC().Format(http.TimeFormat))
		}
		_, _ = io.Copy(w, obj.Body)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "not found", http.StatusNotFound) })
	return mux
}
