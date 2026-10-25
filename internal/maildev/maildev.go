// Package maildev holds the per-device credentials Apple Mail uses for IMAP
// and SMTP submission. Nobody chooses or types them: the only way to mint one
// is the setup page on the public hostname, which requires the Squawk owner
// passkey, and the secret goes straight into a configuration profile. Each
// device is revocable on its own. Secrets are stored as SHA-256 hashes.
package maildev

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("device not found")

const schema = `
CREATE TABLE IF NOT EXISTS mail_devices (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  secret_hash  TEXT NOT NULL UNIQUE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ
);`

type Device struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used_at,omitempty"`
	Revoked   bool       `json:"revoked"`
}

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("mail_devices schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Create mints a device credential and returns its id and one-time secret.
func (s *Store) Create(ctx context.Context, name string) (id, secret string, err error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", "", errors.New("device name must be 1-64 characters")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	secret = "md_" + base64.RawURLEncoding.EncodeToString(b)
	idb := make([]byte, 6)
	if _, err := rand.Read(idb); err != nil {
		return "", "", err
	}
	id = hex.EncodeToString(idb)
	if _, err := s.pool.Exec(ctx, `INSERT INTO mail_devices (id, name, secret_hash) VALUES ($1, $2, $3)`, id, name, hash(secret)); err != nil {
		return "", "", err
	}
	return id, secret, nil
}

// Verify returns the device name for a live secret.
func (s *Store) Verify(ctx context.Context, secret string) (string, bool) {
	if !strings.HasPrefix(secret, "md_") {
		return "", false
	}
	var name string
	err := s.pool.QueryRow(ctx, `UPDATE mail_devices SET last_used_at = now() WHERE secret_hash = $1 AND revoked_at IS NULL RETURNING name`, hash(secret)).Scan(&name)
	return name, err == nil
}

func (s *Store) List(ctx context.Context) ([]Device, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, created_at, last_used_at, revoked_at IS NOT NULL FROM mail_devices ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.CreatedAt, &d.LastUsed, &d.Revoked); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) Revoke(ctx context.Context, id string) error {
	t, err := s.pool.Exec(ctx, `UPDATE mail_devices SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if t.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
