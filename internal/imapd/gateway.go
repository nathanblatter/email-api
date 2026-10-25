// Package imapd is the mail gateway that lets a normal mail client (Apple
// Mail) use the owned inbox: IMAP over implicit TLS for reading and SMTP
// submission over implicit TLS for sending. It runs as its own single
// process (compose service `mailgw`, `email-api mail-gateway`).
//
//   - One account: MAIL_ADDRESS (nathan@nathanblatter.com). INBOX is
//     inbox_messages filtered to that address; Sent/Drafts/Trash/Archive/Junk
//     and any folders Mail creates live in imap_messages.
//   - Passwords are per-device credentials from internal/maildev, minted only
//     by the owner-passkey setup page. No human-chosen password exists.
//   - Outgoing mail is handed to the API's POST /send/raw with this gateway's
//     key, so the budget, spool, pager and From policy all apply.
//   - The public edge (Hetzner) forwards raw TCP with a PROXY v2 header; TLS
//     terminates here with a Let's Encrypt certificate (DNS-01 via Cloudflare),
//     so the edge never sees plaintext.
package imapd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/pires/go-proxyproto"

	"github.com/nathanblatter/email-api/internal/inbox"
	"github.com/nathanblatter/email-api/internal/maildev"
)

type Config struct {
	Address       string // account address, e.g. nathan@nathanblatter.com
	Hostname      string // public name in the certificate, e.g. mail.nathanblatter.com
	IMAPAddr      string // listen address for IMAPS (behind the edge)
	SMTPAddr      string // listen address for SMTPS submission
	ProxyProtocol bool   // require a PROXY header (the Hetzner edge sends v2)
	SendURL       string // API base, e.g. http://app:4500
	SendKey       string // API key for /send/raw
	CertDir       string
	CertFile      string // optional: static certificate instead of ACME (local testing)
	KeyFile       string
	ACMEEmail     string
	CloudflareTok string
	MaxBytes      int64
	SyncEvery     time.Duration

	DatabaseURL, MinIOEndpoint, MinIOAccessKey, MinIOSecretKey, Bucket string
	MinIOSecure                                                        bool
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func ConfigFromEnv() Config {
	return Config{
		Address:        strings.ToLower(env("MAIL_ADDRESS", "nathan@nathanblatter.com")),
		Hostname:       env("MAIL_HOSTNAME", "mail.nathanblatter.com"),
		IMAPAddr:       env("MAIL_IMAP_ADDR", ":1993"),
		SMTPAddr:       env("MAIL_SMTP_ADDR", ":1465"),
		ProxyProtocol:  env("MAIL_PROXY_PROTOCOL", "true") != "false",
		SendURL:        strings.TrimRight(env("MAIL_SEND_URL", "http://email-api-app-1:4500"), "/"),
		SendKey:        os.Getenv("MAIL_SEND_KEY"),
		CertDir:        env("MAIL_CERT_DIR", "/data/certmagic"),
		CertFile:       os.Getenv("MAIL_TLS_CERT"),
		KeyFile:        os.Getenv("MAIL_TLS_KEY"),
		ACMEEmail:      env("MAIL_ACME_EMAIL", "nathan@nathanblatter.com"),
		CloudflareTok:  os.Getenv("CLOUDFLARE_API_TOKEN"),
		MaxBytes:       int64(25) << 20,
		SyncEvery:      10 * time.Second,
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		MinIOEndpoint:  os.Getenv("MINIO_ENDPOINT"),
		MinIOAccessKey: os.Getenv("MINIO_ACCESS_KEY"),
		MinIOSecretKey: os.Getenv("MINIO_SECRET_KEY"),
		MinIOSecure:    os.Getenv("MINIO_SECURE") == "true",
		Bucket:         env("EMAIL_INBOX_BUCKET", "email-inbox"),
	}
}

// Gateway holds the account and the auth state shared by IMAP and SMTP.
type Gateway struct {
	cfg  Config
	log  *slog.Logger
	p    *persist
	user *User
	dev  *maildev.Store
	lim  *limiter
}

func newTracker(n int) *imapserver.MailboxTracker { return imapserver.NewMailboxTracker(uint32(n)) }

func (p *persist) folderUIDNext() imap.UID {
	ctx, cancel := ctxT()
	defer cancel()
	n, err := seqNext(ctx, p.pool, "imap_messages_uid_seq")
	if err != nil {
		return 1
	}
	return n
}

// Main runs the gateway until ctx is cancelled; returns a process exit code.
func Main(ctx context.Context, log *slog.Logger, cfg Config) int {
	if cfg.DatabaseURL == "" || cfg.SendKey == "" || (cfg.CloudflareTok == "" && cfg.CertFile == "") {
		log.Error("mail gateway needs DATABASE_URL, MAIL_SEND_KEY and CLOUDFLARE_API_TOKEN")
		return 1
	}
	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	pg, err := inbox.OpenPG(initCtx, inbox.PGConfig{DatabaseURL: cfg.DatabaseURL, MinIOEndpoint: cfg.MinIOEndpoint,
		MinIOAccessKey: cfg.MinIOAccessKey, MinIOSecretKey: cfg.MinIOSecretKey, MinIOSecure: cfg.MinIOSecure, Bucket: cfg.Bucket})
	if err != nil {
		cancel()
		log.Error("inbox store", "err", err)
		return 1
	}
	defer pg.Close()
	g := &Gateway{cfg: cfg, log: log, lim: newLimiter(10, 15*time.Minute)}
	g.p = &persist{pool: pg.Pool(), inbox: pg, addr: cfg.Address, log: log}
	if err := g.p.migrate(initCtx); err != nil {
		cancel()
		log.Error("imap schema", "err", err)
		return 1
	}
	if g.dev, err = maildev.Open(initCtx, pg.Pool()); err != nil {
		cancel()
		log.Error("mail devices", "err", err)
		return 1
	}
	if err := g.load(initCtx); err != nil {
		cancel()
		log.Error("load mailboxes", "err", err)
		return 1
	}
	cancel()

	tlsCfg, err := certificate(ctx, cfg, log)
	if err != nil {
		log.Error("tls certificate", "err", err)
		return 1
	}

	imapLn, err := g.listen(cfg.IMAPAddr, tlsCfg)
	if err != nil {
		log.Error("imap listen", "err", err)
		return 1
	}
	smtpLn, err := g.listen(cfg.SMTPAddr, tlsCfg)
	if err != nil {
		log.Error("smtp listen", "err", err)
		return 1
	}

	imapSrv := imapserver.New(&imapserver.Options{
		NewSession: g.newIMAPSession,
		Caps:       imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		Logger:     slogAdapter{log},
	})
	smtpSrv := g.newSMTPServer(tlsCfg)

	go g.syncLoop(ctx)
	errc := make(chan error, 2)
	go func() { errc <- imapSrv.Serve(imapLn) }()
	go func() { errc <- smtpSrv.Serve(smtpLn) }()
	log.Info("mail gateway up", "address", cfg.Address, "host", cfg.Hostname, "imap", cfg.IMAPAddr, "smtp", cfg.SMTPAddr, "proxy_protocol", cfg.ProxyProtocol)

	select {
	case <-ctx.Done():
		_ = imapSrv.Close()
		_ = smtpSrv.Close()
		return 0
	case err := <-errc:
		log.Error("mail gateway listener stopped", "err", err)
		return 1
	}
}

func (g *Gateway) listen(addr string, tlsCfg *tls.Config) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if g.cfg.ProxyProtocol {
		ln = &proxyproto.Listener{Listener: ln, ReadHeaderTimeout: 10 * time.Second,
			Policy: func(net.Addr) (proxyproto.Policy, error) { return proxyproto.REQUIRE, nil }}
	}
	return tls.NewListener(ln, tlsCfg), nil
}

func (g *Gateway) load(ctx context.Context) error {
	folders, err := g.p.loadFolders(ctx)
	if err != nil {
		return err
	}
	in := newMailbox("INBOX", inboxUIDValidity, g.p, true)
	rows, err := g.p.inboxRows(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		raw, err := g.p.inbox.Raw(ctx, r.id)
		if err != nil {
			g.log.Warn("imap: raw message unavailable; skipped", "id", r.id, "err", err)
			continue
		}
		in.l = append(in.l, r.message(raw))
	}
	if in.uidNext, err = seqNext(ctx, g.p.pool, "inbox_messages_imap_uid_seq"); err != nil {
		return err
	}
	in.tracker = newTracker(len(in.l))
	folders["INBOX"] = in
	g.user = &User{p: g.p, mailboxes: folders, inbox: in}
	g.log.Info("imap mailboxes loaded", "inbox", len(in.l), "folders", len(folders))
	return nil
}

// syncLoop folds changes made outside IMAP into the inbox: new deliveries,
// and reads/deletes from the web inbox or MCP tools.
func (g *Gateway) syncLoop(ctx context.Context) {
	t := time.NewTicker(g.cfg.SyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := g.syncInbox(ctx); err != nil {
				g.log.Warn("imap inbox sync", "err", err)
			}
		}
	}
}

func (g *Gateway) syncInbox(ctx context.Context) error {
	rows, err := g.p.inboxRows(ctx)
	if err != nil {
		return err
	}
	mbox := g.user.inbox
	byUID := make(map[imap.UID]inboxRow, len(rows))
	for _, r := range rows {
		byUID[r.uid] = r
	}

	mbox.mutex.Lock()
	have := make(map[imap.UID]bool, len(mbox.l))
	gone := make(map[*message]struct{})
	seen := canonicalFlag(imap.FlagSeen)
	for i, msg := range mbox.l {
		have[msg.uid] = true
		r, ok := byUID[msg.uid]
		if !ok {
			gone[msg] = struct{}{}
			continue
		}
		if _, isSeen := msg.flags[seen]; isSeen != r.read {
			if r.read {
				msg.flags[seen] = struct{}{}
			} else {
				delete(msg.flags, seen)
			}
			mbox.tracker.QueueMessageFlags(uint32(i)+1, msg.uid, msg.flagList(), nil)
		}
	}
	mbox.expungeLocked(gone, false)
	next := mbox.uidNext
	mbox.mutex.Unlock()

	for _, r := range rows {
		if have[r.uid] {
			continue
		}
		if r.uid < next {
			// Committed out of order: give it a UID clients have not passed yet.
			if r.uid, err = g.p.renumber(ctx, r.id); err != nil {
				return err
			}
		}
		raw, err := g.p.inbox.Raw(ctx, r.id)
		if err != nil {
			g.log.Warn("imap: raw message unavailable", "id", r.id, "err", err)
			continue
		}
		mbox.mutex.Lock()
		mbox.insertLocked(r.message(raw))
		next = mbox.uidNext
		mbox.mutex.Unlock()
		g.log.Info("imap: new inbox message", "id", r.id, "uid", r.uid)
	}
	return nil
}

// authenticate checks a device credential for the account address.
func (g *Gateway) authenticate(ip, username, password string) (string, error) {
	if g.lim.blocked(ip) {
		time.Sleep(2 * time.Second)
		return "", errAuth
	}
	if !strings.EqualFold(strings.TrimSpace(username), g.cfg.Address) {
		g.lim.fail(ip)
		return "", errAuth
	}
	ctx, cancel := ctxT()
	defer cancel()
	name, ok := g.dev.Verify(ctx, password)
	if !ok {
		g.lim.fail(ip)
		g.log.Warn("mail login rejected", "ip", ip)
		time.Sleep(1 * time.Second)
		return "", errAuth
	}
	return name, nil
}

var errAuth = errors.New("authentication failed")

type imapSession struct {
	*UserSession
	g      *Gateway
	ip     string
	device string
}

func ipOf(a net.Addr) string {
	if a == nil {
		return ""
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}

func (g *Gateway) newIMAPSession(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
	return &imapSession{g: g, ip: ipOf(c.NetConn().RemoteAddr())}, nil, nil
}

func (s *imapSession) Login(username, password string) error {
	dev, err := s.g.authenticate(s.ip, username, password)
	if err != nil {
		return imapserver.ErrAuthFailed
	}
	s.device = dev
	s.UserSession = &UserSession{user: s.g.user}
	s.g.log.Info("imap login", "device", dev, "ip", s.ip)
	return nil
}

func (s *imapSession) Close() error {
	if s.UserSession != nil {
		return s.UserSession.Close()
	}
	return nil
}

// limiter blocks an address after too many failed logins in a window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *limiter) prune(ip string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.fails[ip] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = keep
	}
	return keep
}

func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip, time.Now())) >= l.max
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.fails[ip] = append(l.prune(ip, now), now)
}

type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Printf(format string, v ...any) {
	a.l.Warn("imap", "msg", fmt.Sprintf(format, v...))
}

var _ imapserver.SessionIMAP4rev2 = (*imapSession)(nil)
