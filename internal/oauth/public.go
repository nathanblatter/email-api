package oauth

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// PublicHandler is the complete surface served under the public hostname:
// OAuth, its discovery documents, and a bearer-only /mcp. Everything else
// answers 404 there, whatever the tunnel in front chooses to forward.
func PublicHandler(srv *Server, publicURL string, mcpHandler http.Handler) (http.Handler, string, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, "", fmt.Errorf("EMAIL_PUBLIC_URL must be an https origin with no path, got %q", publicURL)
	}
	pm := http.NewServeMux()
	pm.Handle("/.well-known/", srv.Routes())
	pm.Handle("/oauth/", srv.Routes())
	pm.Handle("/mcp", srv.Bearer(mcpHandler))
	pm.Handle("/mcp/", srv.Bearer(mcpHandler))
	// Nothing else: no health probe, no UI, no keyed API. Discovery, client
	// registration and the login page are the minimum an OAuth client needs
	// before it holds a credential; everything past them requires the key.
	return pm, u.Hostname(), nil
}

// SplitByHost routes requests for publicHost to pub and everything else to
// private. The tunnel preserves the original Host header, so a request that
// came through it is recognizable by hostname alone.
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
