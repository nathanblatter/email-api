// Package oauth is the OAuth 2.1 authorization server behind the public MCP
// connector surface. claude.ai and the Claude mobile app add an MCP server by
// URL and can only authenticate with OAuth, so this is what lets a phone send
// and read mail through email-api.
//
// Same shape as flightdeck's: opaque tokens stored hashed, PKCE S256 only,
// dynamic client registration (RFC 7591), and a login page whose credential
// is the email-api key. There is one identity (Nathan), so a token simply
// means "someone who knew the key authorised this client".
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nathanblatter/email-api/internal/auth"
	"github.com/nathanblatter/email-api/internal/ratelimit"
)

const (
	codeTTL      = 10 * time.Minute
	accessTTL    = time.Hour
	refreshTTL   = 30 * 24 * time.Hour
	resourcePath = "/mcp"
	// Scope is the single scope this resource knows.
	Scope = "mail"
)

type Server struct {
	St     Store
	Issuer string
	// Keys resolves the API key pasted on the login page to an actor name;
	// tokens inherit that name so phone activity is attributed.
	Keys    auth.Keys
	limiter *ratelimit.IPLimiter
	log     *slog.Logger
}

func New(st Store, issuer string, keys auth.Keys, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{St: st, Issuer: strings.TrimRight(issuer, "/"), Keys: keys,
		limiter: ratelimit.New(0.5, 10), log: log}
}

func (s *Server) ResourceMetadataURL() string {
	return s.Issuer + "/.well-known/oauth-protected-resource" + resourcePath
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authServerMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/{rest...}", s.authServerMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/{rest...}", s.protectedResourceMetadata)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorizeForm)
	limited := func(h http.HandlerFunc) http.Handler {
		return s.limiter.Middleware(func(w http.ResponseWriter) {
			writeOAuthError(w, http.StatusTooManyRequests, "slow_down", "too many attempts, try again shortly")
		}, h)
	}
	mux.Handle("POST /oauth/authorize", limited(s.authorizeSubmit))
	mux.Handle("POST /oauth/token", limited(s.token))
	return mux
}

func (s *Server) authServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.Issuer,
		"authorization_endpoint":                s.Issuer + "/oauth/authorize",
		"token_endpoint":                        s.Issuer + "/oauth/token",
		"registration_endpoint":                 s.Issuer + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{Scope},
	})
}

func (s *Server) protectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.Issuer + resourcePath,
		"authorization_servers":    []string{s.Issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{Scope},
		"resource_name":            "email",
	})
}

type registerReq struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirectURI(u) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be absolute https URLs (http only for localhost)")
			return
		}
	}
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_basic"
	}
	switch method {
	case "none", "client_secret_post", "client_secret_basic":
	default:
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type "+g)
			return
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported response_type "+rt)
			return
		}
	}
	id, err := randomToken("emc_")
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "entropy unavailable")
		return
	}
	c := Client{ID: id, Name: strings.TrimSpace(req.ClientName), RedirectURIs: req.RedirectURIs, AuthMethod: method, CreatedAt: time.Now()}
	if len(c.Name) > 200 {
		c.Name = c.Name[:200]
	}
	var secret string
	if method != "none" {
		if secret, err = randomToken("emcs_"); err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "entropy unavailable")
			return
		}
		c.SecretHash = HashKey(secret)
	}
	if err := s.St.CreateClient(r.Context(), c); err != nil {
		s.log.Error("oauth register", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not store client")
		return
	}
	resp := map[string]any{
		"client_id": c.ID, "client_id_issued_at": c.CreatedAt.Unix(), "client_name": c.Name,
		"redirect_uris": c.RedirectURIs, "token_endpoint_auth_method": c.AuthMethod,
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, resp)
}

func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

type authzParams struct {
	ClientID, RedirectURI, ResponseType, State, Scope, Challenge, Method, Resource string
}

func parseAuthz(v url.Values) authzParams {
	return authzParams{ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), ResponseType: v.Get("response_type"),
		State: v.Get("state"), Scope: v.Get("scope"), Challenge: v.Get("code_challenge"), Method: v.Get("code_challenge_method"), Resource: v.Get("resource")}
}

// loadClient validates client and redirect URI before anything may be
// redirected anywhere; failures render a page and never bounce the user.
func (s *Server) loadClient(ctx context.Context, p authzParams) (Client, string) {
	if p.ClientID == "" {
		return Client{}, "missing client_id"
	}
	c, err := s.St.GetClient(ctx, p.ClientID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Client{}, "unknown client_id"
		}
		return Client{}, "client lookup failed"
	}
	if p.RedirectURI == "" {
		return Client{}, "missing redirect_uri"
	}
	for _, u := range c.RedirectURIs {
		if u == p.RedirectURI {
			return c, ""
		}
	}
	return Client{}, "redirect_uri is not registered for this client"
}

func validateAuthz(p authzParams) (code, desc string) {
	if p.ResponseType != "code" {
		return "unsupported_response_type", "response_type must be code"
	}
	if p.Challenge == "" || p.Method != "S256" {
		return "invalid_request", "PKCE S256 code_challenge is required"
	}
	return "", ""
}

func (s *Server) authorizeForm(w http.ResponseWriter, r *http.Request) {
	p := parseAuthz(r.URL.Query())
	c, msg := s.loadClient(r.Context(), p)
	if msg != "" {
		renderPage(w, http.StatusBadRequest, pageData{Title: "Cannot continue", Error: msg})
		return
	}
	if code, desc := validateAuthz(p); code != "" {
		redirectError(w, r, p, code, desc)
		return
	}
	renderPage(w, http.StatusOK, pageData{Title: "Connect to your email", Client: clientLabel(c), Params: p})
}

func (s *Server) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		renderPage(w, http.StatusBadRequest, pageData{Title: "Cannot continue", Error: "malformed form"})
		return
	}
	p := parseAuthz(r.PostForm)
	c, msg := s.loadClient(r.Context(), p)
	if msg != "" {
		renderPage(w, http.StatusBadRequest, pageData{Title: "Cannot continue", Error: msg})
		return
	}
	if code, desc := validateAuthz(p); code != "" {
		redirectError(w, r, p, code, desc)
		return
	}
	retry := func(msg string) {
		renderPage(w, http.StatusUnauthorized, pageData{Title: "Connect to your email", Client: clientLabel(c), Params: p, Error: msg})
	}
	raw := strings.TrimSpace(r.PostForm.Get("api_key"))
	if raw == "" {
		retry("Paste the email-api key to continue.")
		return
	}
	actor, ok := s.Keys.Lookup(r.Context(), raw)
	if !ok {
		s.log.Warn("oauth: bad key on authorize", "ip", ratelimit.ClientIP(r), "client", clientLabel(c))
		retry("That key is not valid.")
		return
	}
	if sc := strings.TrimSpace(p.Scope); sc != "" && !strings.Contains(" "+sc+" ", " "+Scope+" ") {
		redirectError(w, r, p, "invalid_scope", "only the "+Scope+" scope exists")
		return
	}
	code, err := randomToken("emac_")
	if err != nil {
		retry("Could not issue a code; try again.")
		return
	}
	if err := s.St.CreateCode(r.Context(), HashKey(code), Code{ClientID: c.ID, RedirectURI: p.RedirectURI, Challenge: p.Challenge,
		Scope: Scope, Actor: actor, ExpiresAt: time.Now().Add(codeTTL)}); err != nil {
		s.log.Error("oauth store code", "err", err)
		retry("Could not issue a code; try again.")
		return
	}
	s.log.Info("oauth: authorized client", "client", clientLabel(c), "actor", actor)
	redirectWith(w, r, p.RedirectURI, url.Values{"code": {code}, "state": {p.State}})
}

func clientLabel(c Client) string {
	if c.Name != "" {
		return c.Name
	}
	return c.ID
}

func redirectError(w http.ResponseWriter, r *http.Request, p authzParams, code, desc string) {
	redirectWith(w, r, p.RedirectURI, url.Values{"error": {code}, "error_description": {desc}, "state": {p.State}})
}

func redirectWith(w http.ResponseWriter, r *http.Request, redirectURI string, params url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			if v != "" {
				q.Set(k, v)
			}
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form")
		return
	}
	c, ok := s.authenticateClient(w, r)
	if !ok {
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, c)
	case "refresh_token":
		s.refresh(w, r, c)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Server) authenticateClient(w http.ResponseWriter, r *http.Request) (Client, bool) {
	id, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if bu, bp, ok := r.BasicAuth(); ok {
		id, secret = bu, bp
	}
	if id == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client_id is required")
		return Client{}, false
	}
	c, err := s.St.GetClient(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
			return Client{}, false
		}
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "client lookup failed")
		return Client{}, false
	}
	if c.AuthMethod != "none" {
		if c.SecretHash == "" || secret == "" || !EqualHash(HashKey(secret), c.SecretHash) {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return Client{}, false
		}
	}
	return c, true
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, c Client) {
	code, verifier, redirectURI := r.PostForm.Get("code"), r.PostForm.Get("code_verifier"), r.PostForm.Get("redirect_uri")
	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}
	row, err := s.St.ConsumeCode(r.Context(), HashKey(code))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code is invalid, expired, or already used")
			return
		}
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "code lookup failed")
		return
	}
	if row.ClientID != c.ID || (redirectURI != "" && redirectURI != row.RedirectURI) || !VerifyPKCE(verifier, row.Challenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code does not match this client, redirect_uri, or code_verifier")
		return
	}
	s.mint(w, r, c.ID, row.Scope, row.Actor)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, c Client) {
	rt := r.PostForm.Get("refresh_token")
	if rt == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	row, err := s.St.ConsumeRefresh(r.Context(), HashKey(rt))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid, expired, or already used")
			return
		}
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "token lookup failed")
		return
	}
	if row.ClientID != c.ID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token belongs to another client")
		return
	}
	s.mint(w, r, c.ID, row.Scope, row.Actor)
}

func (s *Server) mint(w http.ResponseWriter, r *http.Request, clientID, scope, actor string) {
	access, err1 := randomToken("emat_")
	refresh, err2 := randomToken("emrt_")
	id, err3 := randomToken("emtk_")
	if err1 != nil || err2 != nil || err3 != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "entropy unavailable")
		return
	}
	now := time.Now()
	if err := s.St.CreateToken(r.Context(), HashKey(access), HashKey(refresh), Token{ID: id, ClientID: clientID, Scope: scope, Actor: actor,
		AccessExp: now.Add(accessTTL), RefreshExp: now.Add(refreshTTL)}); err != nil {
		s.log.Error("oauth store token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not store token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(accessTTL.Seconds()),
		"refresh_token": refresh, "scope": scope})
}

// Bearer guards the public /mcp: a valid access token or a 401 that points
// at the resource metadata, per the MCP authorization spec. Raw API keys are
// refused here on purpose: a leaked key must not be replayable through the
// tunnel.
func (s *Server) Bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if strings.HasPrefix(h, "Bearer ") {
			if t, err := s.St.GetTokenByAccess(r.Context(), HashKey(strings.TrimPrefix(h, "Bearer "))); err == nil {
				next.ServeHTTP(w, r.WithContext(auth.WithActor(r.Context(), t.Actor)))
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.ResourceMetadataURL()+`"`)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "a valid bearer token is required")
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

type pageData struct {
	Title  string
	Client string
	Error  string
	Params authzParams
}

var page = template.Must(template.New("authorize").Parse(authorizeHTML))

func renderPage(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = page.Execute(w, d)
}
