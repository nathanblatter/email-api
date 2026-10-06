// email-api: HTTP + MCP gateway for sending mail from nathanblatter.com
// through the house postfix relay, with on-disk spooling and iMessage paging
// when the relay is down.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nathanblatter/email-api/internal/api"
	"github.com/nathanblatter/email-api/internal/auth"
	"github.com/nathanblatter/email-api/internal/config"
	"github.com/nathanblatter/email-api/internal/fallback"
	"github.com/nathanblatter/email-api/internal/files"
	"github.com/nathanblatter/email-api/internal/inbox"
	"github.com/nathanblatter/email-api/internal/mail"
	"github.com/nathanblatter/email-api/internal/mcpserver"
	"github.com/nathanblatter/email-api/internal/oauth"
	"github.com/nathanblatter/email-api/internal/quota"
	"github.com/nathanblatter/email-api/internal/service"
	"github.com/nathanblatter/email-api/internal/spool"
)

var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println(Version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 {
		os.Exit(keysCLI(cfg, os.Args[1:]))
	}
	sp, err := spool.Open(cfg.SpoolDir)
	if err != nil {
		log.Error("spool", "err", err)
		os.Exit(1)
	}

	sender := &mail.SMTPSender{Addr: cfg.SMTPAddr()}
	var pager fallback.Pager
	im := &fallback.IMessage{URL: cfg.IMessageURL, APIKey: cfg.IMessageKey, Recipient: cfg.IMessageRecipient}
	if im.Configured() {
		pager = im
	} else {
		log.Warn("iMessage fallback not configured; outages will only be logged")
	}
	q := quota.New(cfg.DailyBudget, cfg.RedisURL)

	var store files.Store = files.Nop{}
	if cfg.FilesEnabled() {
		m, err := files.New(files.Config{Endpoint: cfg.MinIOEndpoint, AccessKey: cfg.MinIOAccessKey, SecretKey: cfg.MinIOSecretKey,
			Secure: cfg.MinIOSecure, Bucket: cfg.FilesBucket, PublicURL: cfg.FilesPublicURL, TTL: cfg.FilesTTL})
		if err != nil {
			log.Error("files store", "err", err)
			os.Exit(1)
		}
		initCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := m.Init(initCtx); err != nil {
			// Not fatal: mail still sends; oversized attachments are rejected until MinIO is back.
			log.Error("files store init failed; download links disabled until restart", "err", err)
		} else {
			store = m
		}
		cancel()
	} else {
		log.Warn("download links not configured (MINIO_* / EMAIL_FILES_PUBLIC_URL); attachments over the limit will be rejected")
	}

	svc := service.New(sender, pager, sp, q, store, log, service.Options{
		Policy:       mail.Policy{DefaultFrom: cfg.DefaultFrom, AllowedDomains: cfg.AllowedFromDomains, MaxBytes: cfg.MaxMessageBytes},
		MaxLinkBytes: cfg.MaxUploadBytes,
		SpoolMaxAge:  cfg.SpoolMaxAge,
		PageInterval: cfg.PageInterval,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go svc.RunRetries(ctx, cfg.RetryInterval)

	var in *inbox.Service
	var pg *inbox.PG
	var keys auth.Keys = auth.NewStatic(cfg.APIKey)
	if cfg.InboxEnabled() {
		initCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var err error
		pg, err = inbox.OpenPG(initCtx, inbox.PGConfig{DatabaseURL: cfg.DatabaseURL, MinIOEndpoint: cfg.MinIOEndpoint,
			MinIOAccessKey: cfg.MinIOAccessKey, MinIOSecretKey: cfg.MinIOSecretKey, MinIOSecure: cfg.MinIOSecure, Bucket: cfg.InboxBucket})
		cancel()
		if err != nil {
			// Receiving is a hard dependency once configured: a silent inbox is
			// worse than a loud restart loop.
			log.Error("inbox store", "err", err)
			os.Exit(1)
		}
		defer pg.Close()
		kctx, kcancel := context.WithTimeout(context.Background(), 10*time.Second)
		pk, err := auth.OpenPG(kctx, pg.Pool(), cfg.APIKey)
		kcancel()
		if err != nil {
			log.Error("api keys", "err", err)
			os.Exit(1)
		}
		keys = pk
		in = &inbox.Service{Store: pg, Secret: cfg.InboundSecret, Pager: pager, Notify: cfg.InboxNotify, Log: log}
	} else {
		log.Warn("inbox not configured (EMAIL_INBOUND_SECRET / DATABASE_URL / MINIO_*); receiving disabled")
	}

	mcpHandler := mcpserver.NewHandler(svc, in, Version)
	var handler http.Handler = api.New(svc, keys, cfg.MaxUploadBytes, log, mcpHandler, in)

	// Public connector (phone / claude.ai): OAuth + bearer-only /mcp on the
	// public hostname; everything else stays Tailscale-only. Enforced in-app
	// by Host, so a misconfigured tunnel cannot widen exposure.
	if cfg.PublicURL != "" {
		if pg == nil {
			log.Error("EMAIL_PUBLIC_URL needs the inbox database (DATABASE_URL) for OAuth token storage")
			os.Exit(1)
		}
		initCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		ost, err := oauth.OpenPG(initCtx, pg.Pool())
		cancel()
		if err != nil {
			log.Error("oauth store", "err", err)
			os.Exit(1)
		}
		osrv := oauth.New(ost, cfg.PublicURL, keys, log)
		pub, host, err := oauth.PublicHandler(osrv, cfg.PublicURL, mcpHandler)
		if err != nil {
			log.Error("public connector", "err", err)
			os.Exit(1)
		}
		handler = oauth.SplitByHost(host, pub, handler)
		log.Info("public connector enabled (OAuth + bearer /mcp only)", "host", host)
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	pub := &http.Server{Addr: cfg.FilesAddr, Handler: api.Files(store, in, log), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		_ = pub.Shutdown(shutdown)
	}()
	go func() {
		if err := pub.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("files server", "err", err)
			os.Exit(1)
		}
	}()

	log.Info("email-api listening", "addr", cfg.Addr, "version", Version, "relay", cfg.SMTPAddr(),
		"default_from", cfg.DefaultFrom, "allowed_domains", cfg.AllowedFromDomains,
		"daily_budget", cfg.DailyBudget, "quota", q.Backend(), "spool", cfg.SpoolDir, "imessage_fallback", pager != nil,
		"files", store.Configured(), "files_addr", cfg.FilesAddr, "files_url", cfg.FilesPublicURL, "inbox", in != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
