package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrNotFound = errors.New("message not found")

// Message is the stored form. Attachments are listed without content.
type Message struct {
	ID          string            `json:"id"`
	ReceivedAt  time.Time         `json:"received_at"`
	Date        *time.Time        `json:"date,omitempty"`
	EnvFrom     string            `json:"envelope_from"`
	EnvTo       string            `json:"envelope_to"`
	FromName    string            `json:"from_name,omitempty"`
	FromAddr    string            `json:"from"`
	To          []string          `json:"to"`
	Cc          []string          `json:"cc,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text,omitempty"`
	HTML        string            `json:"html,omitempty"`
	MessageID   string            `json:"message_id,omitempty"`
	InReplyTo   string            `json:"in_reply_to,omitempty"`
	References  string            `json:"references,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	SPF         string            `json:"spf,omitempty"`
	DKIM        string            `json:"dkim,omitempty"`
	DMARC       string            `json:"dmarc,omitempty"`
	Suspicious  bool              `json:"suspicious"`
	Read        bool              `json:"read"`
	Size        int               `json:"size"`
	RawKey      string            `json:"-"`
	Attachments []AttachmentMeta  `json:"attachments"`
}

type AttachmentMeta struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	ContentID   string `json:"content_id,omitempty"`
	Inline      bool   `json:"inline"`
	ObjectKey   string `json:"-"`
}

// Summary is the list-view row (no bodies).
type Summary struct {
	ID          string    `json:"id"`
	ReceivedAt  time.Time `json:"received_at"`
	FromName    string    `json:"from_name,omitempty"`
	FromAddr    string    `json:"from"`
	To          []string  `json:"to"`
	Subject     string    `json:"subject"`
	Preview     string    `json:"preview"`
	Read        bool      `json:"read"`
	Suspicious  bool      `json:"suspicious"`
	Attachments int       `json:"attachments"`
}

type ListOptions struct {
	Limit      int
	Unread     bool
	Suspicious *bool  // nil = all; false = hide suspicious (default in handlers)
	Query      string // substring match on from/subject/text
	Before     time.Time
}

// Store is the inbox persistence contract; Postgres+MinIO in prod, memory in tests.
type Store interface {
	Save(ctx context.Context, p *Parsed, envFrom, envTo string, raw []byte) (*Message, error)
	List(ctx context.Context, o ListOptions) ([]Summary, error)
	Get(ctx context.Context, id string) (*Message, error)
	Attachment(ctx context.Context, id, attID string) (*AttachmentMeta, io.ReadCloser, error)
	MarkRead(ctx context.Context, id string, read bool) error
	Delete(ctx context.Context, id string) error
	Unread(ctx context.Context) (int, error)
}

// ── Postgres + MinIO ────────────────────────────────────────────────────────

type PG struct {
	pool   *pgxpool.Pool
	s3     *minio.Client
	bucket string
	now    func() time.Time
}

type PGConfig struct {
	DatabaseURL                                   string // the inbox database; created if missing
	MinIOEndpoint, MinIOAccessKey, MinIOSecretKey string
	MinIOSecure                                   bool
	Bucket                                        string
}

const schema = `
CREATE TABLE IF NOT EXISTS inbox_messages (
  id            TEXT PRIMARY KEY,
  received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  date          TIMESTAMPTZ,
  envelope_from TEXT NOT NULL DEFAULT '',
  envelope_to   TEXT NOT NULL DEFAULT '',
  from_name     TEXT NOT NULL DEFAULT '',
  from_addr     TEXT NOT NULL DEFAULT '',
  to_addrs      TEXT[] NOT NULL DEFAULT '{}',
  cc_addrs      TEXT[] NOT NULL DEFAULT '{}',
  reply_to      TEXT NOT NULL DEFAULT '',
  subject       TEXT NOT NULL DEFAULT '',
  text_body     TEXT NOT NULL DEFAULT '',
  html_body     TEXT NOT NULL DEFAULT '',
  message_id    TEXT NOT NULL DEFAULT '',
  in_reply_to   TEXT NOT NULL DEFAULT '',
  refs          TEXT NOT NULL DEFAULT '',
  headers       JSONB NOT NULL DEFAULT '{}',
  spf           TEXT NOT NULL DEFAULT '',
  dkim          TEXT NOT NULL DEFAULT '',
  dmarc         TEXT NOT NULL DEFAULT '',
  suspicious    BOOLEAN NOT NULL DEFAULT false,
  read          BOOLEAN NOT NULL DEFAULT false,
  size          INTEGER NOT NULL DEFAULT 0,
  raw_key       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS inbox_messages_received_idx ON inbox_messages (received_at DESC);
CREATE INDEX IF NOT EXISTS inbox_messages_message_id_idx ON inbox_messages (message_id);
CREATE TABLE IF NOT EXISTS inbox_attachments (
  id           TEXT PRIMARY KEY,
  message_id   TEXT NOT NULL REFERENCES inbox_messages(id) ON DELETE CASCADE,
  filename     TEXT NOT NULL,
  content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  size         INTEGER NOT NULL DEFAULT 0,
  content_id   TEXT NOT NULL DEFAULT '',
  inline       BOOLEAN NOT NULL DEFAULT false,
  object_key   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS inbox_attachments_message_idx ON inbox_attachments (message_id);
`

// OpenPG connects, creating the database and tables if they do not exist.
func OpenPG(ctx context.Context, cfg PGConfig) (*PG, error) {
	if err := ensureDatabase(ctx, cfg.DatabaseURL); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("inbox db: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("inbox schema: %w", err)
	}
	s3, err := minio.New(cfg.MinIOEndpoint, &minio.Options{
		Creds: credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""), Secure: cfg.MinIOSecure,
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("inbox minio: %w", err)
	}
	if ok, err := s3.BucketExists(ctx, cfg.Bucket); err != nil {
		pool.Close()
		return nil, fmt.Errorf("inbox bucket: %w", err)
	} else if !ok {
		if err := s3.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			pool.Close()
			return nil, fmt.Errorf("inbox make bucket: %w", err)
		}
	}
	return &PG{pool: pool, s3: s3, bucket: cfg.Bucket, now: time.Now}, nil
}

// ensureDatabase creates the target database by connecting to the server's
// maintenance database, so a fresh deploy needs no manual createdb.
func ensureDatabase(ctx context.Context, dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return errors.New("DATABASE_URL has no database name")
	}
	admin := *u
	admin.Path = "/postgres"
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{name}.Sanitize())); err != nil {
			return fmt.Errorf("create database %s: %w", name, err)
		}
	}
	return nil
}

func (s *PG) Close() { s.pool.Close() }

func (s *PG) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *PG) Save(ctx context.Context, p *Parsed, envFrom, envTo string, raw []byte) (*Message, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	m := fromParsed(id, p, envFrom, envTo, len(raw), s.now())
	m.RawKey = id + "/raw.eml"
	if _, err := s.s3.PutObject(ctx, s.bucket, m.RawKey, bytes.NewReader(raw), int64(len(raw)), minio.PutObjectOptions{ContentType: "message/rfc822"}); err != nil {
		return nil, fmt.Errorf("store raw: %w", err)
	}
	for i, a := range p.Attachments {
		aid, _ := newID()
		key := fmt.Sprintf("%s/%d-%s", id, i, safeName(a.Filename))
		if _, err := s.s3.PutObject(ctx, s.bucket, key, bytes.NewReader(a.Content), int64(len(a.Content)), minio.PutObjectOptions{ContentType: a.ContentType}); err != nil {
			return nil, fmt.Errorf("store attachment %s: %w", a.Filename, err)
		}
		m.Attachments = append(m.Attachments, AttachmentMeta{ID: aid, Filename: a.Filename, ContentType: a.ContentType,
			Size: len(a.Content), ContentID: a.ContentID, Inline: a.Inline, ObjectKey: key})
	}
	hdr, _ := json.Marshal(m.Headers)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO inbox_messages (id, received_at, date, envelope_from, envelope_to, from_name, from_addr, to_addrs, cc_addrs,
		reply_to, subject, text_body, html_body, message_id, in_reply_to, refs, headers, spf, dkim, dmarc, suspicious, size, raw_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		m.ID, m.ReceivedAt, m.Date, m.EnvFrom, m.EnvTo, m.FromName, m.FromAddr, m.To, m.Cc, m.ReplyTo, m.Subject, m.Text, m.HTML,
		m.MessageID, m.InReplyTo, m.References, hdr, m.SPF, m.DKIM, m.DMARC, m.Suspicious, m.Size, m.RawKey)
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	for _, a := range m.Attachments {
		if _, err := tx.Exec(ctx, `INSERT INTO inbox_attachments (id, message_id, filename, content_type, size, content_id, inline, object_key)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, a.ID, m.ID, a.Filename, a.ContentType, a.Size, a.ContentID, a.Inline, a.ObjectKey); err != nil {
			return nil, fmt.Errorf("insert attachment: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PG) List(ctx context.Context, o ListOptions) ([]Summary, error) {
	if o.Limit <= 0 || o.Limit > 200 {
		o.Limit = 50
	}
	q := `SELECT m.id, m.received_at, m.from_name, m.from_addr, m.to_addrs, m.subject, left(m.text_body, 200), m.read, m.suspicious,
	        (SELECT count(*) FROM inbox_attachments a WHERE a.message_id = m.id)
	      FROM inbox_messages m WHERE 1=1`
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(" AND "+cond, len(args))
	}
	if o.Unread {
		q += " AND NOT m.read"
	}
	if o.Suspicious != nil {
		add("m.suspicious = $%d", *o.Suspicious)
	}
	if o.Query != "" {
		add("(m.from_addr ILIKE $%[1]d OR m.from_name ILIKE $%[1]d OR m.subject ILIKE $%[1]d OR m.text_body ILIKE $%[1]d)", "%"+o.Query+"%")
	}
	if !o.Before.IsZero() {
		add("m.received_at < $%d", o.Before)
	}
	args = append(args, o.Limit)
	q += fmt.Sprintf(" ORDER BY m.received_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var r Summary
		if err := rows.Scan(&r.ID, &r.ReceivedAt, &r.FromName, &r.FromAddr, &r.To, &r.Subject, &r.Preview, &r.Read, &r.Suspicious, &r.Attachments); err != nil {
			return nil, err
		}
		r.Preview = strings.TrimSpace(r.Preview)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PG) Get(ctx context.Context, id string) (*Message, error) {
	m := &Message{}
	var hdr []byte
	err := s.pool.QueryRow(ctx, `SELECT id, received_at, date, envelope_from, envelope_to, from_name, from_addr, to_addrs, cc_addrs, reply_to,
		subject, text_body, html_body, message_id, in_reply_to, refs, headers, spf, dkim, dmarc, suspicious, read, size, raw_key
		FROM inbox_messages WHERE id=$1`, id).Scan(&m.ID, &m.ReceivedAt, &m.Date, &m.EnvFrom, &m.EnvTo, &m.FromName, &m.FromAddr, &m.To, &m.Cc,
		&m.ReplyTo, &m.Subject, &m.Text, &m.HTML, &m.MessageID, &m.InReplyTo, &m.References, &hdr, &m.SPF, &m.DKIM, &m.DMARC, &m.Suspicious,
		&m.Read, &m.Size, &m.RawKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(hdr, &m.Headers)
	rows, err := s.pool.Query(ctx, `SELECT id, filename, content_type, size, content_id, inline, object_key FROM inbox_attachments WHERE message_id=$1 ORDER BY object_key`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m.Attachments = []AttachmentMeta{}
	for rows.Next() {
		var a AttachmentMeta
		if err := rows.Scan(&a.ID, &a.Filename, &a.ContentType, &a.Size, &a.ContentID, &a.Inline, &a.ObjectKey); err != nil {
			return nil, err
		}
		m.Attachments = append(m.Attachments, a)
	}
	return m, rows.Err()
}

func (s *PG) Attachment(ctx context.Context, id, attID string) (*AttachmentMeta, io.ReadCloser, error) {
	var a AttachmentMeta
	err := s.pool.QueryRow(ctx, `SELECT id, filename, content_type, size, content_id, inline, object_key FROM inbox_attachments WHERE message_id=$1 AND id=$2`, id, attID).
		Scan(&a.ID, &a.Filename, &a.ContentType, &a.Size, &a.ContentID, &a.Inline, &a.ObjectKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	obj, err := s.s3.GetObject(ctx, s.bucket, a.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, nil, err
	}
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, nil, ErrNotFound
	}
	return &a, obj, nil
}

func (s *PG) MarkRead(ctx context.Context, id string, read bool) error {
	t, err := s.pool.Exec(ctx, `UPDATE inbox_messages SET read=$2 WHERE id=$1`, id, read)
	if err != nil {
		return err
	}
	if t.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PG) Delete(ctx context.Context, id string) error {
	m, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	for _, a := range m.Attachments {
		_ = s.s3.RemoveObject(ctx, s.bucket, a.ObjectKey, minio.RemoveObjectOptions{})
	}
	if m.RawKey != "" {
		_ = s.s3.RemoveObject(ctx, s.bucket, m.RawKey, minio.RemoveObjectOptions{})
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM inbox_messages WHERE id=$1`, id)
	return err
}

func (s *PG) Unread(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE NOT read AND NOT suspicious`).Scan(&n)
	return n, err
}

// ── shared helpers ──────────────────────────────────────────────────────────

func fromParsed(id string, p *Parsed, envFrom, envTo string, size int, now time.Time) *Message {
	m := &Message{ID: id, ReceivedAt: now, EnvFrom: envFrom, EnvTo: envTo, Subject: p.Subject, Text: p.Text, HTML: p.HTML,
		MessageID: p.MessageID, InReplyTo: p.InReplyTo, References: p.References, Headers: p.Headers,
		SPF: p.SPF, DKIM: p.DKIM, DMARC: p.DMARC, Suspicious: p.Suspicious(), Size: size,
		// Non-nil slices: pgx encodes a nil []string as NULL, which the NOT NULL columns reject.
		To: []string{}, Cc: []string{}, Attachments: []AttachmentMeta{}}
	if !p.Date.IsZero() {
		d := p.Date
		m.Date = &d
	}
	if p.From != nil {
		m.FromName, m.FromAddr = p.From.Name, p.From.Address
	}
	if p.ReplyTo != nil {
		m.ReplyTo = p.ReplyTo.Address
	}
	for _, a := range p.To {
		m.To = append(m.To, a.Address)
	}
	for _, a := range p.Cc {
		m.Cc = append(m.Cc, a.Address)
	}
	if m.Subject == "" {
		m.Subject = "(no subject)"
	}
	return m
}

func safeName(name string) string {
	name = strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(strings.TrimSpace(name))
	if name == "" {
		return "file"
	}
	return name
}
