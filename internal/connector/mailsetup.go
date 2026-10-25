package connector

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	squawk "github.com/nathanblatter/squawk/client"

	"github.com/nathanblatter/email-api/internal/maildev"
)

// MailSetup is the owner-only page that adds the mailbox to Apple Mail. The
// owner signs in through Squawk with the owner passkey (the same OAuth flow
// the phone connector uses, approved in the Squawk console); the page then
// mints a per-device credential and hands it over inside a configuration
// profile. The credential is never shown on screen.
type MailSetup struct {
	Devices  *maildev.Store
	ClientID string // public PKCE client registered at Squawk
	Secret   []byte // signs the setup cookies
	Address  string // nathan@nathanblatter.com
	Host     string // mail.nathanblatter.com
	FullName string
	Log      *slog.Logger

	mu       sync.Mutex
	profiles map[string]pendingProfile
}

type pendingProfile struct {
	body []byte
	name string
	exp  time.Time
}

const (
	oauthCookie   = "mail_oauth"
	sessionCookie = "mail_owner"
	sessionTTL    = 15 * time.Minute
)

func (m *MailSetup) mount(pm *http.ServeMux, origin, issuer string, v *squawk.Verifier) {
	m.profiles = map[string]pendingProfile{}
	h := &setupHandler{m: m, origin: origin, issuer: issuer, v: v}
	pm.HandleFunc("GET /mail", h.home)
	pm.HandleFunc("GET /mail/start", h.start)
	pm.HandleFunc("GET /mail/callback", h.callback)
	pm.HandleFunc("POST /mail/devices", h.create)
	pm.HandleFunc("POST /mail/devices/{id}/revoke", h.revoke)
	pm.HandleFunc("GET /mail/profile/{token}", h.profile)
	pm.HandleFunc("POST /mail/logout", h.logout)
}

type setupHandler struct {
	m      *MailSetup
	origin string
	issuer string
	v      *squawk.Verifier
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *setupHandler) sign(payload string) string {
	mac := hmac.New(sha256.New, h.m.Secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *setupHandler) verify(v string) (string, bool) {
	p, s, ok := strings.Cut(v, ".")
	if !ok {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, h.m.Secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	// payload = "<kind>|<unix exp>|<data>"
	parts := strings.SplitN(string(payload), "|", 3)
	if len(parts) != 3 {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return parts[0] + "|" + parts[2], true
}

func (h *setupHandler) setCookie(w http.ResponseWriter, name, kind, data string, ttl time.Duration) {
	exp := time.Now().Add(ttl)
	http.SetCookie(w, &http.Cookie{Name: name, Value: h.sign(kind + "|" + strconv.FormatInt(exp.Unix(), 10) + "|" + data),
		Path: "/mail", Expires: exp, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/mail", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func (h *setupHandler) owner(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	v, ok := h.verify(c.Value)
	return ok && v == "session|owner"
}

// sameOrigin blocks cross-site form posts (the cookie is also SameSite=Lax).
func (h *setupHandler) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == h.origin
}

func (h *setupHandler) home(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Address": h.m.Address, "Host": h.m.Host, "Owner": h.owner(r), "Error": r.URL.Query().Get("error")}
	if data["Owner"] == true {
		devs, err := h.m.Devices.List(r.Context())
		if err != nil {
			http.Error(w, "could not list devices", http.StatusInternalServerError)
			return
		}
		data["Devices"] = devs
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, data)
}

func (h *setupHandler) start(w http.ResponseWriter, r *http.Request) {
	state, verifier := randHex(16), randHex(32)
	h.setCookie(w, oauthCookie, "oauth", state+":"+verifier, 10*time.Minute)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {h.m.ClientID},
		"redirect_uri":          {h.origin + "/mail/callback"},
		"state":                 {state},
		"scope":                 {"write"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"resource":              {h.origin + "/mcp"},
	}
	http.Redirect(w, r, h.issuer+"/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (h *setupHandler) callback(w http.ResponseWriter, r *http.Request) {
	fail := func(reason string) {
		h.m.Log.Warn("mail setup sign-in failed", "reason", reason)
		http.Redirect(w, r, "/mail?error="+url.QueryEscape(reason), http.StatusFound)
	}
	c, err := r.Cookie(oauthCookie)
	if err != nil {
		fail("sign-in expired, start again")
		return
	}
	clearCookie(w, oauthCookie)
	v, ok := h.verify(c.Value)
	state, verifier, _ := strings.Cut(strings.TrimPrefix(v, "oauth|"), ":")
	if !ok || !strings.HasPrefix(v, "oauth|") || state == "" || r.URL.Query().Get("state") != state {
		fail("sign-in expired, start again")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		fail("sign-in was not approved")
		return
	}
	tok, err := h.exchange(r.Context(), r.URL.Query().Get("code"), verifier)
	if err != nil {
		fail("could not finish sign-in")
		h.m.Log.Error("mail setup token exchange", "err", err)
		return
	}
	claims, err := h.v.Verify(r.Context(), tok)
	if err != nil || claims.Subject != "owner" {
		fail("only the owner can add a mail device")
		return
	}
	h.setCookie(w, sessionCookie, "session", "owner", sessionTTL)
	h.m.Log.Info("mail setup: owner signed in")
	http.Redirect(w, r, "/mail", http.StatusFound)
}

func (h *setupHandler) exchange(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {h.origin + "/mail/callback"},
		"client_id":     {h.m.ClientID},
		"code_verifier": {verifier},
		"resource":      {h.origin + "/mcp"},
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("token endpoint %d %s", resp.StatusCode, out.Error)
	}
	return out.AccessToken, nil
}

func (h *setupHandler) create(w http.ResponseWriter, r *http.Request) {
	if !h.owner(r) || !h.sameOrigin(r) {
		http.Redirect(w, r, "/mail", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "iPhone"
	}
	id, secret, err := h.m.Devices.Create(r.Context(), name)
	if err != nil {
		http.Redirect(w, r, "/mail?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	body := h.m.profile(id, name, secret)
	tok := randHex(16)
	h.m.mu.Lock()
	for k, p := range h.m.profiles {
		if time.Now().After(p.exp) {
			delete(h.m.profiles, k)
		}
	}
	h.m.profiles[tok] = pendingProfile{body: body, name: name, exp: time.Now().Add(5 * time.Minute)}
	h.m.mu.Unlock()
	h.m.Log.Info("mail setup: device credential minted", "device", name, "id", id)
	http.Redirect(w, r, "/mail/profile/"+tok, http.StatusSeeOther)
}

func (h *setupHandler) profile(w http.ResponseWriter, r *http.Request) {
	if !h.owner(r) {
		http.Redirect(w, r, "/mail", http.StatusFound)
		return
	}
	tok := r.PathValue("token")
	h.m.mu.Lock()
	p, ok := h.m.profiles[tok]
	delete(h.m.profiles, tok)
	h.m.mu.Unlock()
	if !ok || time.Now().After(p.exp) {
		http.Redirect(w, r, "/mail?error="+url.QueryEscape("that profile link was already used; revoke the device and add it again"), http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Content-Disposition", `attachment; filename="nathanblatter-mail.mobileconfig"`)
	_, _ = w.Write(p.body)
}

func (h *setupHandler) revoke(w http.ResponseWriter, r *http.Request) {
	if !h.owner(r) || !h.sameOrigin(r) {
		http.Redirect(w, r, "/mail", http.StatusSeeOther)
		return
	}
	if err := h.m.Devices.Revoke(r.Context(), r.PathValue("id")); err != nil {
		http.Redirect(w, r, "/mail?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	h.m.Log.Info("mail setup: device revoked", "id", r.PathValue("id"))
	http.Redirect(w, r, "/mail", http.StatusSeeOther)
}

func (h *setupHandler) logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/mail", http.StatusSeeOther)
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// profile builds an iOS/macOS configuration profile with one IMAP account.
func (m *MailSetup) profile(id, device, secret string) []byte {
	e := xmlEscape
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadContent</key>
	<array>
		<dict>
			<key>EmailAccountDescription</key><string>` + e(m.Address) + `</string>
			<key>EmailAccountName</key><string>` + e(m.FullName) + `</string>
			<key>EmailAccountType</key><string>EmailTypeIMAP</string>
			<key>EmailAddress</key><string>` + e(m.Address) + `</string>
			<key>IncomingMailServerAuthentication</key><string>EmailAuthPassword</string>
			<key>IncomingMailServerHostName</key><string>` + e(m.Host) + `</string>
			<key>IncomingMailServerPortNumber</key><integer>993</integer>
			<key>IncomingMailServerUseSSL</key><true/>
			<key>IncomingMailServerUsername</key><string>` + e(m.Address) + `</string>
			<key>IncomingPassword</key><string>` + e(secret) + `</string>
			<key>OutgoingMailServerAuthentication</key><string>EmailAuthPassword</string>
			<key>OutgoingMailServerHostName</key><string>` + e(m.Host) + `</string>
			<key>OutgoingMailServerPortNumber</key><integer>465</integer>
			<key>OutgoingMailServerUseSSL</key><true/>
			<key>OutgoingMailServerUsername</key><string>` + e(m.Address) + `</string>
			<key>OutgoingPassword</key><string>` + e(secret) + `</string>
			<key>OutgoingPasswordSameAsIncomingPassword</key><true/>
			<key>PayloadDisplayName</key><string>` + e(m.Address) + `</string>
			<key>PayloadIdentifier</key><string>com.nathanblatter.mail.` + id + `.account</string>
			<key>PayloadType</key><string>com.apple.mail.managed</string>
			<key>PayloadUUID</key><string>` + uuid() + `</string>
			<key>PayloadVersion</key><integer>1</integer>
		</dict>
	</array>
	<key>PayloadDescription</key><string>Adds ` + e(m.Address) + ` to Mail on ` + e(device) + `.</string>
	<key>PayloadDisplayName</key><string>` + e(m.Address) + `</string>
	<key>PayloadIdentifier</key><string>com.nathanblatter.mail.` + id + `</string>
	<key>PayloadOrganization</key><string>nathanblatter.com</string>
	<key>PayloadRemovalDisallowed</key><false/>
	<key>PayloadType</key><string>Configuration</string>
	<key>PayloadUUID</key><string>` + uuid() + `</string>
	<key>PayloadVersion</key><integer>1</integer>
</dict>
</plist>
`)
}

var pageTmpl = template.Must(template.New("mail").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mail setup</title>
<style>
:root{--bg:#f6f6f4;--card:#fff;--ink:#1c1c1a;--muted:#6b6b66;--line:#e4e4df;--accent:#1f5fbf;--danger:#b3261e}
@media (prefers-color-scheme: dark){:root{--bg:#141413;--card:#1f1f1d;--ink:#ecece8;--muted:#9a9a94;--line:#33332f;--accent:#7aa7ff;--danger:#ff8a80}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:16px/1.45 -apple-system,BlinkMacSystemFont,"SF Pro Text",system-ui,sans-serif}
main{max-width:480px;margin:0 auto;padding:32px 16px}
h1{font-size:22px;margin:0 0 4px}p{color:var(--muted);margin:0 0 20px}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:18px;margin-bottom:16px}
.btn{display:block;width:100%;text-align:center;padding:13px;border-radius:11px;border:0;background:var(--accent);color:#fff;font:600 16px/1 inherit;text-decoration:none;cursor:pointer}
input{width:100%;padding:12px;border-radius:10px;border:1px solid var(--line);background:var(--bg);color:var(--ink);font:inherit;margin:6px 0 12px}
label{font-size:14px;color:var(--muted)}
.dev{display:flex;justify-content:space-between;align-items:center;padding:10px 0;border-top:1px solid var(--line)}
.dev:first-child{border-top:0}.dev small{display:block;color:var(--muted)}
.link{background:none;border:0;color:var(--danger);font:inherit;cursor:pointer;padding:0}
.err{color:var(--danger);margin-bottom:16px}.muted{color:var(--muted);font-size:14px}
</style></head><body><main>
<h1>Add {{.Address}} to Mail</h1>
<p>Signing in requires the owner passkey. Each device gets its own credential inside a profile, and you can revoke it here.</p>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
{{if .Owner}}
<div class="card"><form method="post" action="/mail/devices">
<label for="name">Device name</label><input id="name" name="name" value="iPhone" maxlength="64" autocomplete="off">
<button class="btn" type="submit">Download profile</button></form>
<p class="muted" style="margin:12px 0 0">After it downloads, open Settings, tap Profile Downloaded, then Install.</p></div>
<div class="card"><strong>Devices</strong>
{{range .Devices}}<div class="dev"><div>{{.Name}}<small>added {{.CreatedAt.Format "Jan 2, 2006"}}{{if .LastUsed}}, last used {{.LastUsed.Format "Jan 2 3:04 PM"}}{{end}}{{if .Revoked}}, revoked{{end}}</small></div>
{{if not .Revoked}}<form method="post" action="/mail/devices/{{.ID}}/revoke"><button class="link" type="submit">Revoke</button></form>{{end}}</div>
{{else}}<div class="muted" style="margin-top:8px">No devices yet.</div>{{end}}</div>
<form method="post" action="/mail/logout"><button class="link" style="color:var(--muted)" type="submit">Sign out</button></form>
{{else}}
<div class="card"><a class="btn" href="/mail/start">Continue with owner passkey</a>
<p class="muted" style="margin:12px 0 0">You will approve the sign-in in the Squawk console with Face ID.</p></div>
{{end}}
<p class="muted">Server {{.Host}}, IMAP 993 and SMTP 465, both SSL.</p>
</main></body></html>`))
