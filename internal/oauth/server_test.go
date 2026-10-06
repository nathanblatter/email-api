package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nathanblatter/email-api/internal/auth"
)

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestFullFlowAndHostSplit(t *testing.T) {
	st := NewMemory()
	srv := New(st, "https://email.example.com", auth.NewStatic("the-key"), slog.Default())
	mcp := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("mcp ok")) })
	pub, host, err := PublicHandler(srv, "https://email.example.com", mcp)
	if err != nil || host != "email.example.com" {
		t.Fatal(err, host)
	}
	private := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("private")) })
	ts := httptest.NewServer(SplitByHost(host, pub, private))
	defer ts.Close()
	c := noRedirect()
	asPublic := func(req *http.Request) *http.Request { req.Host = "email.example.com"; return req }

	// Discovery.
	req, _ := http.NewRequest("GET", ts.URL+"/.well-known/oauth-authorization-server", nil)
	res, _ := c.Do(asPublic(req))
	var meta map[string]any
	_ = json.NewDecoder(res.Body).Decode(&meta)
	if res.StatusCode != 200 || meta["token_endpoint"] != "https://email.example.com/oauth/token" {
		t.Fatalf("metadata %d %v", res.StatusCode, meta)
	}

	// Private routes are invisible on the public host; private host sees them.
	for _, p := range []string{"/", "/ui/", "/inbox", "/send", "/health", "/healthz", "/queue", "/f/x/y", "/inbound"} {
		req, _ = http.NewRequest("GET", ts.URL+p, nil)
		if res, _ = c.Do(asPublic(req)); res.StatusCode != 404 {
			t.Fatalf("public %s should 404, got %d", p, res.StatusCode)
		}
	}
	req, _ = http.NewRequest("GET", ts.URL+"/inbox", nil)
	if res, _ = c.Do(req); res.StatusCode != 200 {
		t.Fatalf("private host should reach the private mux, got %d", res.StatusCode)
	}

	// Unauthenticated /mcp advertises the resource metadata.
	req, _ = http.NewRequest("POST", ts.URL+"/mcp", nil)
	res, _ = c.Do(asPublic(req))
	if res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), "oauth-protected-resource/mcp") {
		t.Fatalf("mcp 401: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	// A raw API key is refused on the public host.
	req, _ = http.NewRequest("POST", ts.URL+"/mcp", nil)
	req.Header.Set("X-API-Key", "the-key")
	if res, _ = c.Do(asPublic(req)); res.StatusCode != 401 {
		t.Fatalf("raw key must not work publicly, got %d", res.StatusCode)
	}

	// Register a public client.
	body := `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`
	req, _ = http.NewRequest("POST", ts.URL+"/oauth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, _ = c.Do(asPublic(req))
	var reg map[string]any
	_ = json.NewDecoder(res.Body).Decode(&reg)
	if res.StatusCode != 201 || reg["client_id"] == nil || reg["client_secret"] != nil {
		t.Fatalf("register %d %v", res.StatusCode, reg)
	}
	clientID := reg["client_id"].(string)

	// Authorize: GET renders the form; POST with the wrong key fails; right key redirects with a code.
	verifier := strings.Repeat("v", 64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{"client_id": {clientID}, "redirect_uri": {"https://claude.ai/api/mcp/auth_callback"}, "response_type": {"code"},
		"state": {"xyz"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	req, _ = http.NewRequest("GET", ts.URL+"/oauth/authorize?"+q.Encode(), nil)
	res, _ = c.Do(asPublic(req))
	page, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(page), "Claude") || !strings.Contains(string(page), `name="api_key"`) {
		t.Fatalf("authorize form %d", res.StatusCode)
	}
	form := url.Values{}
	for k, v := range q {
		form[k] = v
	}
	form.Set("api_key", "wrong")
	req, _ = http.NewRequest("POST", ts.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if res, _ = c.Do(asPublic(req)); res.StatusCode != 401 {
		t.Fatalf("wrong key should 401, got %d", res.StatusCode)
	}
	form.Set("api_key", "the-key")
	req, _ = http.NewRequest("POST", ts.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, _ = c.Do(asPublic(req))
	loc, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 302 || loc.Host != "claude.ai" || loc.Query().Get("state") != "xyz" || loc.Query().Get("code") == "" {
		t.Fatalf("authorize redirect %d %v", res.StatusCode, res.Header.Get("Location"))
	}
	code := loc.Query().Get("code")

	// Token exchange with PKCE; a wrong verifier fails; the code is single use.
	tok := func(v url.Values) (int, map[string]any) {
		req, _ := http.NewRequest("POST", ts.URL+"/oauth/token", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, _ := c.Do(asPublic(req))
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if code2, _ := tok(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {strings.Repeat("x", 64)}}); code2 != 400 {
		t.Fatalf("bad verifier should 400, got %d", code2)
	}
	// The failed attempt consumed the code (single use), so get a fresh one.
	req, _ = http.NewRequest("POST", ts.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, _ = c.Do(asPublic(req))
	loc, _ = url.Parse(res.Header.Get("Location"))
	code = loc.Query().Get("code")
	status, tr := tok(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier},
		"redirect_uri": {"https://claude.ai/api/mcp/auth_callback"}})
	if status != 200 || tr["access_token"] == nil || tr["refresh_token"] == nil || tr["scope"] != "mail" {
		t.Fatalf("token %d %v", status, tr)
	}
	if s2, _ := tok(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}}); s2 != 400 {
		t.Fatalf("code reuse should 400, got %d", s2)
	}

	// Bearer reaches /mcp on the public host.
	req, _ = http.NewRequest("POST", ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tr["access_token"].(string))
	res, _ = c.Do(asPublic(req))
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(b) != "mcp ok" {
		t.Fatalf("bearer mcp %d %q", res.StatusCode, b)
	}

	// Refresh rotates: new pair works, old refresh is dead, old access is dead.
	status, tr2 := tok(url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {tr["refresh_token"].(string)}})
	if status != 200 || tr2["access_token"] == tr["access_token"] {
		t.Fatalf("refresh %d %v", status, tr2)
	}
	if s3, _ := tok(url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {tr["refresh_token"].(string)}}); s3 != 400 {
		t.Fatalf("replayed refresh should 400, got %d", s3)
	}
	req, _ = http.NewRequest("POST", ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tr["access_token"].(string))
	if res, _ = c.Do(asPublic(req)); res.StatusCode != 401 {
		t.Fatalf("rotated-out access token should 401, got %d", res.StatusCode)
	}
	req, _ = http.NewRequest("POST", ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tr2["access_token"].(string))
	if res, _ = c.Do(asPublic(req)); res.StatusCode != 200 {
		t.Fatalf("new access token should work, got %d", res.StatusCode)
	}
}

func TestRedirectURIValidation(t *testing.T) {
	for uri, want := range map[string]bool{"https://claude.ai/cb": true, "http://localhost:3000/cb": true, "http://evil.com/cb": false, "https://x.com/cb#frag": false, "notaurl": false} {
		if validRedirectURI(uri) != want {
			t.Errorf("%s: want %v", uri, want)
		}
	}
	if !VerifyPKCE(strings.Repeat("a", 43), func() string {
		s := sha256.Sum256([]byte(strings.Repeat("a", 43)))
		return base64.RawURLEncoding.EncodeToString(s[:])
	}()) {
		t.Fatal("pkce")
	}
	if VerifyPKCE("short", "x") {
		t.Fatal("short verifier accepted")
	}
}
