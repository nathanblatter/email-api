package api

import (
	"encoding/json"
	"net/http"

	"github.com/nathanblatter/email-api/internal/mail"
)

// rawRequest is a message already composed by a mail client: the envelope
// plus the exact RFC 5322 bytes (base64 in JSON). Used by the IMAP/SMTP mail
// gateway for Apple Mail's outgoing mail.
type rawRequest struct {
	From       string   `json:"from"`
	Recipients []string `json:"recipients"`
	Raw        []byte   `json:"raw"`
}

func (s *Server) sendRaw(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(nil, r.Body, s.maxBytes*4/3+1<<20)
	var req rawRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.fail(w, &mail.ValidationError{Msg: "invalid JSON body: " + err.Error()})
		return
	}
	res, err := s.svc.SendRaw(r.Context(), req.From, req.Recipients, req.Raw)
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
