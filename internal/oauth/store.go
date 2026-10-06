package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

type Client struct {
	ID           string
	SecretHash   string // empty for public clients
	Name         string
	RedirectURIs []string
	AuthMethod   string
	CreatedAt    time.Time
}

type Code struct {
	ClientID, RedirectURI, Challenge, Scope, Actor string
	ExpiresAt                                      time.Time
}

type Token struct {
	ID         string
	ClientID   string
	Scope      string
	Actor      string // name of the API key the user signed in with
	AccessExp  time.Time
	RefreshExp time.Time
}

// Store persists OAuth clients, one-time codes and token pairs. Everything
// secret is stored as a SHA-256 hash.
type Store interface {
	CreateClient(ctx context.Context, c Client) error
	GetClient(ctx context.Context, id string) (Client, error)
	CreateCode(ctx context.Context, codeHash string, c Code) error
	ConsumeCode(ctx context.Context, codeHash string) (Code, error)
	CreateToken(ctx context.Context, accessHash, refreshHash string, t Token) error
	GetTokenByAccess(ctx context.Context, accessHash string) (Token, error)
	ConsumeRefresh(ctx context.Context, refreshHash string) (Token, error)
}

func HashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func EqualHash(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// ── Postgres ───────────────────────────────────────────────────────────────

const schema = `
CREATE TABLE IF NOT EXISTS oauth_clients (
  id            TEXT PRIMARY KEY,
  secret_hash   TEXT NOT NULL DEFAULT '',
  name          TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT[] NOT NULL,
  auth_method   TEXT NOT NULL DEFAULT 'none',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS oauth_codes (
  code_hash      TEXT PRIMARY KEY,
  client_id      TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  redirect_uri   TEXT NOT NULL,
  code_challenge TEXT NOT NULL,
  scope          TEXT NOT NULL DEFAULT '',
  expires_at     TIMESTAMPTZ NOT NULL,
  used           BOOLEAN NOT NULL DEFAULT false
);
ALTER TABLE oauth_codes ADD COLUMN IF NOT EXISTS actor TEXT NOT NULL DEFAULT 'env';
ALTER TABLE oauth_tokens ADD COLUMN IF NOT EXISTS actor TEXT NOT NULL DEFAULT 'env';
CREATE TABLE IF NOT EXISTS oauth_tokens (
  id                 TEXT PRIMARY KEY,
  client_id          TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  access_hash        TEXT NOT NULL UNIQUE,
  refresh_hash       TEXT NOT NULL UNIQUE,
  scope              TEXT NOT NULL DEFAULT '',
  access_expires_at  TIMESTAMPTZ NOT NULL,
  refresh_expires_at TIMESTAMPTZ NOT NULL,
  revoked            BOOLEAN NOT NULL DEFAULT false,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at       TIMESTAMPTZ
);`

type PG struct{ pool *pgxpool.Pool }

func OpenPG(ctx context.Context, pool *pgxpool.Pool) (*PG, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("oauth schema: %w", err)
	}
	return &PG{pool: pool}, nil
}

func (s *PG) CreateClient(ctx context.Context, c Client) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO oauth_clients (id, secret_hash, name, redirect_uris, auth_method) VALUES ($1,$2,$3,$4,$5)`,
		c.ID, c.SecretHash, c.Name, c.RedirectURIs, c.AuthMethod)
	return err
}

func (s *PG) GetClient(ctx context.Context, id string) (Client, error) {
	var c Client
	err := s.pool.QueryRow(ctx, `SELECT id, secret_hash, name, redirect_uris, auth_method, created_at FROM oauth_clients WHERE id=$1`, id).
		Scan(&c.ID, &c.SecretHash, &c.Name, &c.RedirectURIs, &c.AuthMethod, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *PG) CreateCode(ctx context.Context, codeHash string, c Code) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO oauth_codes (code_hash, client_id, redirect_uri, code_challenge, scope, expires_at, actor) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		codeHash, c.ClientID, c.RedirectURI, c.Challenge, c.Scope, c.ExpiresAt, c.Actor)
	return err
}

// ConsumeCode marks the code used and returns it only if it was unused and
// unexpired: single use is enforced by the UPDATE's row count.
func (s *PG) ConsumeCode(ctx context.Context, codeHash string) (Code, error) {
	var c Code
	err := s.pool.QueryRow(ctx, `UPDATE oauth_codes SET used=true WHERE code_hash=$1 AND NOT used AND expires_at > now()
		RETURNING client_id, redirect_uri, code_challenge, scope, expires_at, actor`, codeHash).
		Scan(&c.ClientID, &c.RedirectURI, &c.Challenge, &c.Scope, &c.ExpiresAt, &c.Actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *PG) CreateToken(ctx context.Context, accessHash, refreshHash string, t Token) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO oauth_tokens (id, client_id, access_hash, refresh_hash, scope, access_expires_at, refresh_expires_at, actor)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, t.ID, t.ClientID, accessHash, refreshHash, t.Scope, t.AccessExp, t.RefreshExp, t.Actor)
	return err
}

func (s *PG) GetTokenByAccess(ctx context.Context, accessHash string) (Token, error) {
	var t Token
	err := s.pool.QueryRow(ctx, `UPDATE oauth_tokens SET last_used_at=now() WHERE access_hash=$1 AND NOT revoked AND access_expires_at > now()
		RETURNING id, client_id, scope, access_expires_at, refresh_expires_at, actor`, accessHash).
		Scan(&t.ID, &t.ClientID, &t.Scope, &t.AccessExp, &t.RefreshExp, &t.Actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// ConsumeRefresh revokes the pair and returns it (rotation): a replayed
// refresh token finds the row already revoked.
func (s *PG) ConsumeRefresh(ctx context.Context, refreshHash string) (Token, error) {
	var t Token
	err := s.pool.QueryRow(ctx, `UPDATE oauth_tokens SET revoked=true WHERE refresh_hash=$1 AND NOT revoked AND refresh_expires_at > now()
		RETURNING id, client_id, scope, access_expires_at, refresh_expires_at, actor`, refreshHash).
		Scan(&t.ID, &t.ClientID, &t.Scope, &t.AccessExp, &t.RefreshExp, &t.Actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// ── Memory (tests) ─────────────────────────────────────────────────────────

type Memory struct {
	mu      sync.Mutex
	clients map[string]Client
	codes   map[string]*memCode
	tokens  map[string]*memToken // by id
	Now     func() time.Time
}

type memCode struct {
	Code
	used bool
}
type memToken struct {
	Token
	access, refresh string
	revoked         bool
}

func NewMemory() *Memory {
	return &Memory{clients: map[string]Client{}, codes: map[string]*memCode{}, tokens: map[string]*memToken{}, Now: time.Now}
}

func (m *Memory) CreateClient(_ context.Context, c Client) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[c.ID] = c
	return nil
}
func (m *Memory) GetClient(_ context.Context, id string) (Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[id]
	if !ok {
		return c, ErrNotFound
	}
	return c, nil
}
func (m *Memory) CreateCode(_ context.Context, h string, c Code) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[h] = &memCode{Code: c}
	return nil
}
func (m *Memory) ConsumeCode(_ context.Context, h string) (Code, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.codes[h]
	if !ok || c.used || m.Now().After(c.ExpiresAt) {
		return Code{}, ErrNotFound
	}
	c.used = true
	return c.Code, nil
}
func (m *Memory) CreateToken(_ context.Context, a, r string, t Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[t.ID] = &memToken{Token: t, access: a, refresh: r}
	return nil
}
func (m *Memory) GetTokenByAccess(_ context.Context, a string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if t.access == a && !t.revoked && m.Now().Before(t.AccessExp) {
			return t.Token, nil
		}
	}
	return Token{}, ErrNotFound
}
func (m *Memory) ConsumeRefresh(_ context.Context, r string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if t.refresh == r && !t.revoked && m.Now().Before(t.RefreshExp) {
			t.revoked = true
			return t.Token, nil
		}
	}
	return Token{}, ErrNotFound
}
