// Package connector is the public surface for Claude's phone/web connector:
// an RFC 9728 protected resource whose authorization server is Squawk
// (https://squawk.nathanblatter.com). Squawk runs the OAuth 2.1 flow and
// Nathan approves each connector there with his passkey; this package only
// verifies the ES256 tokens Squawk mints for this service and attributes the
// resulting sends to a fixed actor.
//
// Served only when the request's Host is the public hostname (the tunnel
// preserves it); everything else on that hostname answers 404, whatever the
// tunnel chooses to forward.
package connector

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	squawk "github.com/nathanblatter/squawk/client"

	"github.com/nathanblatter/email-api/internal/auth"
)

// Actor is how connector traffic shows up on sends and in logs.
const Actor = "claude-connector"

// Config is what the public mux needs.
type Config struct {
	PublicURL string // https origin the tunnel forwards, e.g. https://email.nathanblatter.com
	Issuer    string // Squawk, e.g. https://squawk.nathanblatter.com
	ServiceID string // this service's id in Squawk (the token audience)
}

// Handler builds the public mux and returns it with the public hostname.
func Handler(cfg Config, mcp http.Handler) (http.Handler, string, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, "", fmt.Errorf("EMAIL_PUBLIC_URL must be an https origin with no path, got %q", cfg.PublicURL)
	}
	if cfg.Issuer == "" || cfg.ServiceID == "" {
		return nil, "", fmt.Errorf("EMAIL_AUTH_ISSUER and EMAIL_AUTH_SERVICE_ID are required with EMAIL_PUBLIC_URL")
	}
	origin := u.Scheme + "://" + u.Host
	v := squawk.NewVerifier(cfg.Issuer, cfg.ServiceID, squawk.WithResource(origin+"/mcp"))
	guard := func(next http.Handler) http.Handler {
		return squawk.RequireToken(v)(squawk.RequireScope("write")(withActor(next)))
	}
	pm := http.NewServeMux()
	pm.Handle("GET /.well-known/oauth-protected-resource/mcp", squawk.ProtectedResourceMetadata(v, "email-api", []string{"read", "write"}))
	pm.Handle("/mcp", guard(mcp))
	pm.Handle("/mcp/", guard(mcp))
	return pm, u.Hostname(), nil
}

// withActor stamps connector requests with the fixed actor name.
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithActor(r.Context(), Actor)))
	})
}

// SplitByHost routes requests for publicHost to pub and everything else to
// private. A direct Tailscale request carries the IP (or a *.ts.net name) and
// stays on the private side.
func SplitByHost(publicHost string, pub, private http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if strings.EqualFold(host, publicHost) {
			pub.ServeHTTP(w, r)
			return
		}
		private.ServeHTTP(w, r)
	})
}
