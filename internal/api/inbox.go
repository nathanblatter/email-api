package api

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/nathanblatter/email-api/internal/inbox"
)

// MaxInboundBytes caps a single inbound message. Cloudflare Email Routing
// itself stops at 25 MB.
const MaxInboundBytes = 30 << 20

// Inbound is the public-listener route the Email Worker posts raw messages
// to. Auth is a shared secret header; the body is the RFC 5322 message.
func Inbound(svc *inbox.Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !svc.Authorized(r.Header.Get("X-Inbound-Secret")) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxInboundBytes))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "message too large"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
			return
		}
		if len(raw) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty body"})
			return
		}
		m, err := svc.Receive(r.Context(), raw, r.Header.Get("X-Envelope-From"), r.Header.Get("X-Envelope-To"))
		if err != nil {
			// 5xx makes the Worker throw, so Cloudflare tempfails and the
			// sending MTA retries instead of the mail being lost.
			log.Error("inbound failed", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "stored", "id": m.ID, "suspicious": m.Suspicious, "attachments": len(m.Attachments)})
	}
}

// mountInbox adds the keyed inbox routes to the API mux.
func (s *Server) mountInbox(mux *http.ServeMux, svc *inbox.Service) {
	if svc == nil {
		return
	}
	mux.Handle("GET /inbox", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		o := inbox.ListOptions{Query: q.Get("q"), Unread: q.Get("unread") == "true"}
		if n, err := strconv.Atoi(q.Get("limit")); err == nil {
			o.Limit = n
		}
		if q.Get("suspicious") != "all" {
			show := q.Get("suspicious") == "true"
			o.Suspicious = &show
		}
		if b := q.Get("before"); b != "" {
			if t, err := time.Parse(time.RFC3339, b); err == nil {
				o.Before = t
			}
		}
		list, err := svc.Store.List(r.Context(), o)
		if err != nil {
			s.fail(w, err)
			return
		}
		unread, _ := svc.Store.Unread(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"count": len(list), "unread": unread, "messages": list})
	})))
	mux.Handle("GET /inbox/{id}", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, err := svc.Store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			s.inboxErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	})))
	mux.Handle("GET /inbox/{id}/attachments/{aid}", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta, body, err := svc.Store.Attachment(r.Context(), r.PathValue("id"), r.PathValue("aid"))
		if err != nil {
			s.inboxErr(w, err)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", meta.ContentType)
		w.Header().Set("Content-Length", strconv.Itoa(meta.Size))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": meta.Filename}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.Copy(w, body)
	})))
	mux.Handle("POST /inbox/{id}/read", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := svc.Store.MarkRead(r.Context(), r.PathValue("id"), r.URL.Query().Get("read") != "false"); err != nil {
			s.inboxErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})))
	mux.Handle("DELETE /inbox/{id}", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := svc.Store.Delete(r.Context(), r.PathValue("id")); err != nil {
			s.inboxErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})))
}

func (s *Server) inboxErr(w http.ResponseWriter, err error) {
	if errors.Is(err, inbox.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	s.fail(w, err)
}
