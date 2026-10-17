package connector

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nathanblatter/email-api/internal/auth"
)

// fakeSquawk serves a JWKS and signs ES256 tokens, the way Squawk does.
type fakeSquawk struct {
	key *ecdsa.PrivateKey
	srv *httptest.Server
}

func newSquawk(t *testing.T) *fakeSquawk {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSquawk{key: k}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		pt, _ := k.PublicKey.Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "EC", "crv": "P-256", "kid": "k1", "use": "sig", "alg": "ES256",
			"x": base64.RawURLEncoding.EncodeToString(pt[1:33]), "y": base64.RawURLEncoding.EncodeToString(pt[33:])}}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSquawk) token(t *testing.T, aud, scope string) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "at+jwt", "kid": "k1"})
	now := time.Now()
	body, _ := json.Marshal(map[string]any{"iss": f.srv.URL, "sub": "usr:owner", "aud": aud, "scope": scope, "client_id": "nbc_x",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "jti": "j1"})
	input := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, f.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestHandler(t *testing.T) {
	sq := newSquawk(t)
	var gotActor string
	mcp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor = auth.ActorFrom(r.Context())
		w.WriteHeader(200)
	})
	h, host, err := Handler(Config{PublicURL: "https://email.example.test", Issuer: sq.srv.URL, ServiceID: "svc_mail"}, mcp)
	if err != nil || host != "email.example.test" {
		t.Fatalf("handler: %v host=%q", err, host)
	}
	if _, _, err := Handler(Config{PublicURL: "http://nope", Issuer: sq.srv.URL, ServiceID: "x"}, mcp); err == nil {
		t.Fatal("http public url accepted")
	}
	if _, _, err := Handler(Config{PublicURL: "https://email.example.test"}, mcp); err == nil {
		t.Fatal("missing issuer/service id accepted")
	}

	do := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// Metadata names Squawk and the resource.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/.well-known/oauth-protected-resource/mcp", nil))
	var meta map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &meta)
	if rec.Code != 200 || meta["resource"] != "https://email.example.test/mcp" || meta["authorization_servers"].([]any)[0] != sq.srv.URL {
		t.Fatalf("metadata: %d %s", rec.Code, rec.Body.String())
	}
	// No token → 401 with the resource_metadata challenge.
	if rec := do("/mcp", ""); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// Wrong audience, missing scope, then success.
	if rec := do("/mcp", sq.token(t, "svc_other", "write")); rec.Code != 401 {
		t.Fatalf("wrong aud: %d", rec.Code)
	}
	if rec := do("/mcp", sq.token(t, "svc_mail", "read")); rec.Code != 403 {
		t.Fatalf("read-only token: %d", rec.Code)
	}
	if rec := do("/mcp", sq.token(t, "svc_mail", "read write")); rec.Code != 200 || gotActor != Actor {
		t.Fatalf("valid token: %d actor=%q", rec.Code, gotActor)
	}
	// Nothing else exists on the public host.
	for _, p := range []string{"/", "/health", "/send", "/oauth/token", "/.well-known/oauth-authorization-server"} {
		if rec := do(p, sq.token(t, "svc_mail", "write")); rec.Code != 404 {
			t.Fatalf("%s on public host: %d", p, rec.Code)
		}
	}
	// Host split.
	private := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	split := SplitByHost(host, h, private)
	for _, c := range []struct {
		host string
		want int
	}{{"email.example.test", 401}, {"EMAIL.example.test:443", 401}, {"100.79.61.79:4500", 204}} {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Host = c.host
		rec := httptest.NewRecorder()
		split.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("host %s: %d want %d", c.host, rec.Code, c.want)
		}
	}
}
