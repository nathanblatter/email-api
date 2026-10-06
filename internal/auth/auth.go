// Package auth resolves an API key (header X-API-Key or Bearer) to an actor
// name, so every send, read and token can say who did it. Keys are stored
// hashed in the inbox database; the EMAIL_API_KEY from the environment is
// always accepted as the actor "env" so a fresh deploy can bootstrap.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("key not found")

// Key is an API key as listed (never the secret itself).
type Key struct {
	ID        string
	Name      string
	CreatedAt time.Time
	LastUsed  *time.Time
	Revoked   bool
}

// Keys is the credential store contract.
type Keys interface {
	// Lookup resolves a raw key to its actor name. ok=false means unknown or revoked.
	Lookup(ctx context.Context, raw string) (actor string, ok bool)
}

type ctxKey struct{}

// WithActor / ActorFrom carry the resolved actor through the request.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, ctxKey{}, actor)
}

func ActorFrom(ctx context.Context) string {
	if a, _ := ctx.Value(ctxKey{}).(string); a != "" {
		return a
	}
	return "unknown"
}

func HashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Extract pulls the credential from a request: X-API-Key or Bearer.
func Extract(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// ── Static (env key only; also the fallback when there is no database) ────

type Static struct{ hash string }

func NewStatic(envKey string) *Static { return &Static{hash: HashKey(envKey)} }

func (s *Static) Lookup(_ context.Context, raw string) (string, bool) {
	if raw != "" && subtle.ConstantTimeCompare([]byte(HashKey(raw)), []byte(s.hash)) == 1 {
		return "env", true
	}
	return "", false
}

// ── Postgres: named keys + the env key ────────────────────────────────────

const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  key_hash     TEXT NOT NULL UNIQUE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ
);`

type PG struct {
	pool *pgxpool.Pool
	env  *Static
}

func OpenPG(ctx context.Context, pool *pgxpool.Pool, envKey string) (*PG, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("api_keys schema: %w", err)
	}
	return &PG{pool: pool, env: NewStatic(envKey)}, nil
}

func (p *PG) Lookup(ctx context.Context, raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	var name string
	err := p.pool.QueryRow(ctx, `UPDATE api_keys SET last_used_at = now() WHERE key_hash = $1 AND revoked_at IS NULL RETURNING name`, HashKey(raw)).Scan(&name)
	if err == nil {
		return name, true
	}
	return p.env.Lookup(ctx, raw)
}

// Create mints a key and returns the one-time secret.
func (p *PG) Create(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || strings.ContainsAny(name, " \t\n") {
		return "", errors.New("name must be 1–64 chars with no whitespace")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := "em_" + base64.RawURLEncoding.EncodeToString(b)
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	_, err := p.pool.Exec(ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ($1, $2, $3)`, hex.EncodeToString(idb), name, HashKey(secret))
	if err != nil {
		return "", fmt.Errorf("create key %q: %w", name, err)
	}
	return secret, nil
}

func (p *PG) List(ctx context.Context) ([]Key, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, name, created_at, last_used_at, revoked_at IS NOT NULL FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.Name, &k.CreatedAt, &k.LastUsed, &k.Revoked); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (p *PG) Revoke(ctx context.Context, name string) error {
	t, err := p.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE name = $1 AND revoked_at IS NULL`, name)
	if err != nil {
		return err
	}
	if t.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

var _ = pgx.ErrNoRows
