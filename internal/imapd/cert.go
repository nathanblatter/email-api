package imapd

import (
	"context"
	"crypto/tls"
	"log/slog"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

// certificate obtains and renews the Let's Encrypt certificate for the public
// mail hostname with a DNS-01 challenge in Cloudflare (the edge only forwards
// TCP, so HTTP challenges cannot reach this process).
func certificate(ctx context.Context, cfg Config, log *slog.Logger) (*tls.Config, error) {
	if cfg.CertFile != "" {
		c, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}, nil
	}
	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.Email = cfg.ACMEEmail
	certmagic.DefaultACME.DisableHTTPChallenge = true
	certmagic.DefaultACME.DisableTLSALPNChallenge = true
	certmagic.DefaultACME.DNS01Solver = &certmagic.DNS01Solver{
		DNSManager: certmagic.DNSManager{DNSProvider: &cloudflare.Provider{APIToken: cfg.CloudflareTok}},
	}
	certmagic.Default.Storage = &certmagic.FileStorage{Path: cfg.CertDir}
	cm := certmagic.NewDefault()
	if err := cm.ManageSync(ctx, []string{cfg.Hostname}); err != nil {
		return nil, err
	}
	tc := cm.TLSConfig()
	tc.NextProtos = nil
	tc.MinVersion = tls.VersionTLS12
	log.Info("tls certificate ready", "host", cfg.Hostname)
	return tc, nil
}
