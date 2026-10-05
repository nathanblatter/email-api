// Package api is the HTTP surface. It mirrors imessage-api: X-API-Key auth,
// GET /health open, everything else keyed. JSON in, JSON out, plus a
// multipart form for sending files straight from curl.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/service"
)

type Server struct {
	svc      *service.Service
	apiKey   string
	maxBytes int64
	log      *slog.Logger
}

func New(svc *service.Service, apiKey string, maxBytes int64, log *slog.Logger, mcp http.Handler) http.Handler {
	s := &Server{svc: svc, apiKey: apiKey, maxBytes: maxBytes, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.Handle("POST /send", s.auth(http.HandlerFunc(s.send)))
	mux.Handle("POST /send/batch", s.auth(http.HandlerFunc(s.sendBatch)))
	mux.Handle("GET /queue", s.auth(http.HandlerFunc(s.queue)))
	mux.Handle("POST /queue/retry", s.auth(http.HandlerFunc(s.retry)))
	if mcp != nil {
		mux.Handle("/mcp", s.auth(mcp))
		mux.Handle("/mcp/", s.auth(mcp))
	}
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				key = strings.TrimPrefix(h, "Bearer ")
			}
		}
		if key == "" || key != s.apiKey {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Health(r.Context()))
}

// parseMessage accepts application/json or multipart/form-data. In the form
// variant, every file field becomes an attachment and the other fields map
// 1:1 to the JSON names (to/cc/bcc may repeat or be comma-separated).
func (s *Server) parseMessage(r *http.Request) (*mail.Message, error) {
	ct := r.Header.Get("Content-Type")
	// Base64 inflates attachments by ~4/3; allow that plus headroom.
	r.Body = http.MaxBytesReader(nil, r.Body, s.maxBytes*3/2+1<<20)
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, &mail.ValidationError{Msg: "bad multipart form: " + err.Error()}
		}
		f := r.MultipartForm
		m := &mail.Message{
			From:    first(f.Value["from"]),
			To:      mail.AddrList(f.Value["to"]),
			Cc:      mail.AddrList(f.Value["cc"]),
			Bcc:     mail.AddrList(f.Value["bcc"]),
			ReplyTo: first(f.Value["reply_to"]),
			Subject: first(f.Value["subject"]),
			Text:    first(f.Value["text"]),
			HTML:    first(f.Value["html"]),
		}
		for _, fhs := range f.File {
			for _, fh := range fhs {
				file, err := fh.Open()
				if err != nil {
					return nil, err
				}
				b, err := io.ReadAll(file)
				file.Close()
				if err != nil {
					return nil, err
				}
				ctype := fh.Header.Get("Content-Type")
				if ctype == "application/octet-stream" {
					ctype = "" // generic client default; let the filename decide
				}
				m.Attachments = append(m.Attachments, mail.Attachment{Filename: fh.Filename, ContentType: ctype, Content: b})
			}
		}
		return m, nil
	}
	var m mail.Message
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &mail.ValidationError{Msg: "request body too large"}
		}
		return nil, &mail.ValidationError{Msg: "invalid JSON body: " + err.Error()}
	}
	return &m, nil
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	m, err := s.parseMessage(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.svc.Send(r.Context(), m)
	if err != nil {
		s.fail(w, err)
		return
	}
	code := http.StatusOK
	if res.Status == "queued" {
		code = http.StatusAccepted
	}
	writeJSON(w, code, res)
}

type batchRequest struct {
	Messages []*mail.Message `json:"messages"`
}

type batchResponse struct {
	Status   string           `json:"status"` // sent | partial | queued | rejected
	Sent     int              `json:"sent"`
	Queued   int              `json:"queued"`
	Rejected int              `json:"rejected"`
	Results  []service.Result `json:"results"`
}

func (s *Server) sendBatch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(nil, r.Body, s.maxBytes*3+1<<20)
	var req batchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.fail(w, &mail.ValidationError{Msg: "invalid JSON body: " + err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		s.fail(w, &mail.ValidationError{Msg: "messages must be a non-empty array"})
		return
	}
	writeJSON(w, http.StatusOK, Summarize(s.svc.SendBatch(r.Context(), req.Messages)))
}

// Summarize rolls per-message results into one batch verdict.
func Summarize(results []service.Result) batchResponse {
	b := batchResponse{Results: results}
	for _, r := range results {
		switch r.Status {
		case "sent":
			b.Sent++
		case "queued":
			b.Queued++
		default:
			b.Rejected++
		}
	}
	switch {
	case b.Rejected == len(results):
		b.Status = "rejected"
	case b.Sent == len(results):
		b.Status = "sent"
	case b.Rejected == 0 && b.Sent == 0:
		b.Status = "queued"
	default:
		b.Status = "partial"
	}
	return b
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	q, err := s.svc.Queue()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(q), "queued": q})
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	d, rem, dead := s.svc.RetryOnce(r.Context())
	writeJSON(w, http.StatusOK, map[string]int{"delivered": d, "remaining": rem, "dead": dead})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if service.IsValidation(err) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "rejected", "error": err.Error()})
		return
	}
	s.log.Error("request failed", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "failed", "error": err.Error()})
}
