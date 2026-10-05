// Package files turns large attachments into expiring download links. Objects
// live in the house MinIO; the public listener (see api.Files) streams them
// out behind a Cloudflare tunnel, so the bucket itself never faces the
// internet and the keyed API stays on the tailnet.
package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// Link is what the email (and the API response) carries.
type Link struct {
	Filename string    `json:"filename"`
	URL      string    `json:"url"`
	Size     int64     `json:"size"`
	Expires  time.Time `json:"expires"`
}

// Object is a stored file opened for download.
type Object struct {
	Body        io.ReadCloser
	Size        int64
	ContentType string
	Expires     time.Time
}

var ErrNotFound = errors.New("file not found or expired")

// Store is the contract the service and the public handler use; tests use an
// in-memory implementation.
type Store interface {
	Put(ctx context.Context, filename, contentType string, data []byte) (Link, error)
	Get(ctx context.Context, token, filename string) (*Object, error)
	Configured() bool
}

type Config struct {
	Endpoint  string // host:port, no scheme
	AccessKey string
	SecretKey string
	Secure    bool
	Bucket    string
	PublicURL string // e.g. https://file.nathanblatter.com
	TTL       time.Duration
	Now       func() time.Time
}

type MinIO struct {
	cfg    Config
	client *minio.Client
}

// Nop is the store used when MinIO is not configured: links are refused and
// oversized mail is rejected with a clear message.
type Nop struct{}

func (Nop) Put(context.Context, string, string, []byte) (Link, error) {
	return Link{}, errors.New("download links are not configured (MINIO_* / EMAIL_FILES_PUBLIC_URL)")
}
func (Nop) Get(context.Context, string, string) (*Object, error) { return nil, ErrNotFound }
func (Nop) Configured() bool                                     { return false }

func New(cfg Config) (*MinIO, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * 24 * time.Hour
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	return &MinIO{cfg: cfg, client: c}, nil
}

func (m *MinIO) Configured() bool { return true }

// Init creates the bucket if needed and sets a lifecycle rule so MinIO
// deletes objects itself after the TTL (the handler also refuses expired
// objects, so a lagging lifecycle sweep can't leak a stale link).
func (m *MinIO) Init(ctx context.Context) error {
	ok, err := m.client.BucketExists(ctx, m.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("bucket exists: %w", err)
	}
	if !ok {
		if err := m.client.MakeBucket(ctx, m.cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("make bucket: %w", err)
		}
	}
	days := int(m.cfg.TTL.Hours()/24) + 1
	lc := lifecycle.NewConfiguration()
	lc.Rules = []lifecycle.Rule{{
		ID:         "expire-email-files",
		Status:     "Enabled",
		Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(days)},
	}}
	if err := m.client.SetBucketLifecycle(ctx, m.cfg.Bucket, lc); err != nil {
		return fmt.Errorf("bucket lifecycle: %w", err)
	}
	return nil
}

func (m *MinIO) Put(ctx context.Context, filename, contentType string, data []byte) (Link, error) {
	tok, err := newToken()
	if err != nil {
		return Link{}, err
	}
	name := safeName(filename)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	expires := m.cfg.Now().Add(m.cfg.TTL)
	_, err = m.client.PutObject(ctx, m.cfg.Bucket, tok+"/"+name, strings.NewReader(string(data)), int64(len(data)), minio.PutObjectOptions{
		ContentType:  contentType,
		UserMetadata: map[string]string{"expires": expires.UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return Link{}, fmt.Errorf("upload %s: %w", name, err)
	}
	return Link{Filename: name, URL: PublicURL(m.cfg.PublicURL, tok, name), Size: int64(len(data)), Expires: expires}, nil
}

func (m *MinIO) Get(ctx context.Context, token, filename string) (*Object, error) {
	if !validToken(token) {
		return nil, ErrNotFound
	}
	key := token + "/" + safeName(filename)
	obj, err := m.client.GetObject(ctx, m.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ErrNotFound
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, ErrNotFound
	}
	var expires time.Time
	if s := st.UserMetadata["Expires"]; s != "" {
		expires, _ = time.Parse(time.RFC3339, s)
	}
	if !expires.IsZero() && m.cfg.Now().After(expires) {
		obj.Close()
		return nil, ErrNotFound
	}
	return &Object{Body: obj, Size: st.Size, ContentType: st.ContentType, Expires: expires}, nil
}

// PublicURL builds the link the email carries.
func PublicURL(base, token, name string) string {
	return strings.TrimRight(base, "/") + "/f/" + token + "/" + url.PathEscape(name)
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validToken(t string) bool {
	if len(t) != 32 {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

// safeName strips directories and control characters from a filename.
func safeName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" {
		return "file"
	}
	return name
}

// Memory is an in-memory Store for tests.
type Memory struct {
	PublicURL string
	TTL       time.Duration
	Now       func() time.Time
	Objects   map[string]memObj
}

type memObj struct {
	data    []byte
	ctype   string
	expires time.Time
}

func (m *Memory) Configured() bool { return true }

func (m *Memory) Put(_ context.Context, filename, contentType string, data []byte) (Link, error) {
	if m.Objects == nil {
		m.Objects = map[string]memObj{}
	}
	tok, _ := newToken()
	name := safeName(filename)
	exp := m.Now().Add(m.TTL)
	m.Objects[tok+"/"+name] = memObj{data: data, ctype: contentType, expires: exp}
	return Link{Filename: name, URL: PublicURL(m.PublicURL, tok, name), Size: int64(len(data)), Expires: exp}, nil
}

func (m *Memory) Get(_ context.Context, token, filename string) (*Object, error) {
	o, ok := m.Objects[token+"/"+safeName(filename)]
	if !ok || m.Now().After(o.expires) {
		return nil, ErrNotFound
	}
	return &Object{Body: io.NopCloser(strings.NewReader(string(o.data))), Size: int64(len(o.data)), ContentType: o.ctype, Expires: o.expires}, nil
}
