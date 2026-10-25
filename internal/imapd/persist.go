package imapd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nathanblatter/email-api/internal/inbox"
)

// INBOX is the owned inbox (inbox_messages) filtered to one address; every
// other folder lives in imap_messages and exists only for mail clients.
const inboxUIDValidity = 1

const folderSchema = `
CREATE TABLE IF NOT EXISTS imap_folders (
  name         TEXT PRIMARY KEY,
  uid_validity BIGINT NOT NULL,
  special_use  TEXT NOT NULL DEFAULT '',
  subscribed   BOOLEAN NOT NULL DEFAULT true
);
CREATE TABLE IF NOT EXISTS imap_messages (
  uid           BIGSERIAL PRIMARY KEY,
  folder        TEXT NOT NULL REFERENCES imap_folders(name) ON UPDATE CASCADE ON DELETE CASCADE,
  flags         TEXT[] NOT NULL DEFAULT '{}',
  internal_date TIMESTAMPTZ NOT NULL DEFAULT now(),
  raw           BYTEA NOT NULL
);
CREATE INDEX IF NOT EXISTS imap_messages_folder_idx ON imap_messages (folder, uid);
INSERT INTO imap_folders (name, uid_validity, special_use) VALUES
  ('Sent', 2, '\Sent'), ('Drafts', 3, '\Drafts'), ('Trash', 4, '\Trash'),
  ('Archive', 5, '\Archive'), ('Junk', 6, '\Junk')
ON CONFLICT (name) DO NOTHING;
`

// specialFolders cannot be deleted or renamed: Mail relies on them.
var specialFolders = map[string]bool{"INBOX": true, "Sent": true, "Drafts": true, "Trash": true, "Archive": true, "Junk": true}

// addressMatch selects inbox rows delivered to the account address; the
// envelope_to column may hold a comma-separated list.
const addressMatch = `$1 = ANY(string_to_array(regexp_replace(lower(envelope_to), '\s', '', 'g'), ','))`

type persist struct {
	pool  *pgxpool.Pool
	inbox *inbox.PG
	addr  string // lower-case account address
	log   *slog.Logger
}

func ctxT() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}

func (p *persist) migrate(ctx context.Context) error {
	var exists bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='inbox_messages' AND column_name='imap_uid')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		// One-time: number existing rows oldest first, then new rows take the
		// next value of the sequence, so UIDs only ever grow.
		if _, err := p.pool.Exec(ctx, `ALTER TABLE inbox_messages ADD COLUMN imap_uid BIGINT;
			CREATE SEQUENCE IF NOT EXISTS inbox_messages_imap_uid_seq OWNED BY inbox_messages.imap_uid;
			UPDATE inbox_messages m SET imap_uid = s.n FROM (SELECT id, nextval('inbox_messages_imap_uid_seq') AS n FROM (SELECT id FROM inbox_messages ORDER BY received_at, id) o) s WHERE m.id = s.id;
			ALTER TABLE inbox_messages ALTER COLUMN imap_uid SET DEFAULT nextval('inbox_messages_imap_uid_seq');
			ALTER TABLE inbox_messages ALTER COLUMN imap_uid SET NOT NULL;`); err != nil {
			return fmt.Errorf("imap_uid: %w", err)
		}
	}
	if _, err := p.pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS inbox_messages_imap_uid_idx ON inbox_messages (imap_uid);
		ALTER TABLE inbox_messages ADD COLUMN IF NOT EXISTS imap_flags TEXT[] NOT NULL DEFAULT '{}';`+folderSchema); err != nil {
		return fmt.Errorf("imap schema: %w", err)
	}
	return nil
}

func seqNext(ctx context.Context, pool *pgxpool.Pool, seq string) (imap.UID, error) {
	var last int64
	var called bool
	if err := pool.QueryRow(ctx, `SELECT last_value, is_called FROM `+seq).Scan(&last, &called); err != nil {
		return 0, err
	}
	if !called {
		return imap.UID(last), nil
	}
	return imap.UID(last + 1), nil
}

func flagsFrom(list []string) map[imap.Flag]struct{} {
	m := make(map[imap.Flag]struct{}, len(list))
	for _, f := range list {
		if f != "" {
			m[canonicalFlag(imap.Flag(f))] = struct{}{}
		}
	}
	return m
}

// inboxExtraFlags are the flags stored beside the read column.
func inboxExtraFlags(msg *message) (read bool, extra []string) {
	extra = []string{}
	for f := range msg.flags {
		if f == canonicalFlag(imap.FlagSeen) {
			read = true
			continue
		}
		extra = append(extra, string(f))
	}
	return read, extra
}

func allFlags(msg *message) []string {
	out := []string{}
	for f := range msg.flags {
		out = append(out, string(f))
	}
	return out
}

func (p *persist) append(mbox *Mailbox, msg *message) (imap.UID, string, error) {
	ctx, cancel := ctxT()
	defer cancel()
	if mbox.inbox {
		// Moving a message back to INBOX (Mail's undo) re-files it in the
		// owned inbox as if it were delivered to the account address.
		parsed, err := inbox.Parse(msg.buf)
		if err != nil {
			return 0, "", &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Message could not be parsed"}
		}
		m, err := p.inbox.Save(ctx, parsed, "", p.addr, msg.buf)
		if err != nil {
			return 0, "", err
		}
		read, extra := inboxExtraFlags(msg)
		var uid int64
		if err := p.pool.QueryRow(ctx, `UPDATE inbox_messages SET read=$2, imap_flags=$3 WHERE id=$1 RETURNING imap_uid`, m.ID, read, extra).Scan(&uid); err != nil {
			return 0, "", err
		}
		return imap.UID(uid), m.ID, nil
	}
	var uid int64
	if err := p.pool.QueryRow(ctx, `INSERT INTO imap_messages (folder, flags, internal_date, raw) VALUES ($1, $2, $3, $4) RETURNING uid`,
		mbox.name, allFlags(msg), msg.t, msg.buf).Scan(&uid); err != nil {
		return 0, "", err
	}
	return imap.UID(uid), strconv.FormatInt(uid, 10), nil
}

func (p *persist) setFlags(mbox *Mailbox, msg *message) error {
	ctx, cancel := ctxT()
	defer cancel()
	if mbox.inbox {
		read, extra := inboxExtraFlags(msg)
		_, err := p.pool.Exec(ctx, `UPDATE inbox_messages SET read=$2, imap_flags=$3 WHERE id=$1`, msg.id, read, extra)
		return err
	}
	_, err := p.pool.Exec(ctx, `UPDATE imap_messages SET flags=$2 WHERE uid=$1`, int64(msg.uid), allFlags(msg))
	return err
}

func (p *persist) remove(mbox *Mailbox, msg *message) error {
	ctx, cancel := ctxT()
	defer cancel()
	if mbox.inbox {
		err := p.inbox.Delete(ctx, msg.id)
		if errors.Is(err, inbox.ErrNotFound) {
			return nil
		}
		return err
	}
	_, err := p.pool.Exec(ctx, `DELETE FROM imap_messages WHERE uid=$1`, int64(msg.uid))
	return err
}

// ── folders ────────────────────────────────────────────────────────────────

func specialAttr(s string) []imap.MailboxAttr {
	if s == "" {
		return nil
	}
	return []imap.MailboxAttr{imap.MailboxAttr(s)}
}

func (p *persist) loadFolders(ctx context.Context) (map[string]*Mailbox, error) {
	out := map[string]*Mailbox{}
	rows, err := p.pool.Query(ctx, `SELECT name, uid_validity, special_use, subscribed FROM imap_folders`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, special string
		var uv int64
		var sub bool
		if err := rows.Scan(&name, &uv, &special, &sub); err != nil {
			rows.Close()
			return nil, err
		}
		mb := newMailbox(name, uint32(uv), p, false)
		mb.specialUse, mb.subscribed = specialAttr(special), sub
		out[name] = mb
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	next, err := seqNext(ctx, p.pool, "imap_messages_uid_seq")
	if err != nil {
		return nil, err
	}
	mrows, err := p.pool.Query(ctx, `SELECT uid, folder, flags, internal_date, raw FROM imap_messages ORDER BY uid`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var uid int64
		var folder string
		var flags []string
		var t time.Time
		var raw []byte
		if err := mrows.Scan(&uid, &folder, &flags, &t, &raw); err != nil {
			return nil, err
		}
		mb := out[folder]
		if mb == nil {
			continue
		}
		mb.l = append(mb.l, &message{uid: imap.UID(uid), id: strconv.FormatInt(uid, 10), buf: raw, t: t, flags: flagsFrom(flags)})
	}
	for _, mb := range out {
		mb.uidNext = next
		mb.tracker = newTracker(len(mb.l))
	}
	return out, mrows.Err()
}

func (p *persist) createFolder(name string) (uint32, error) {
	ctx, cancel := ctxT()
	defer cancel()
	uv := uint32(time.Now().Unix())
	_, err := p.pool.Exec(ctx, `INSERT INTO imap_folders (name, uid_validity) VALUES ($1, $2)`, name, int64(uv))
	return uv, err
}

func (p *persist) deleteFolder(name string) error {
	ctx, cancel := ctxT()
	defer cancel()
	_, err := p.pool.Exec(ctx, `DELETE FROM imap_folders WHERE name=$1`, name)
	return err
}

func (p *persist) renameFolder(oldName, newName string) error {
	ctx, cancel := ctxT()
	defer cancel()
	_, err := p.pool.Exec(ctx, `UPDATE imap_folders SET name=$2 WHERE name=$1`, oldName, newName)
	return err
}

func (p *persist) setSubscribed(name string, sub bool) error {
	if strings.EqualFold(name, "INBOX") {
		return nil
	}
	ctx, cancel := ctxT()
	defer cancel()
	_, err := p.pool.Exec(ctx, `UPDATE imap_folders SET subscribed=$2 WHERE name=$1`, name, sub)
	return err
}

// ── inbox ──────────────────────────────────────────────────────────────────

type inboxRow struct {
	id   string
	uid  imap.UID
	read bool
	tags []string
	t    time.Time
}

func (p *persist) inboxRows(ctx context.Context) ([]inboxRow, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, imap_uid, read, imap_flags, received_at FROM inbox_messages WHERE `+addressMatch+` ORDER BY imap_uid`, p.addr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []inboxRow
	for rows.Next() {
		var r inboxRow
		var uid int64
		if err := rows.Scan(&r.id, &uid, &r.read, &r.tags, &r.t); err != nil {
			return nil, err
		}
		r.uid = imap.UID(uid)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (r inboxRow) message(raw []byte) *message {
	f := flagsFrom(r.tags)
	if r.read {
		f[canonicalFlag(imap.FlagSeen)] = struct{}{}
	}
	return &message{uid: r.uid, id: r.id, buf: raw, t: r.t, flags: f}
}

// renumber gives a row a fresh UID; used when a row turns up below UIDNEXT
// (inserted out of order), which IMAP clients would never notice otherwise.
func (p *persist) renumber(ctx context.Context, id string) (imap.UID, error) {
	var uid int64
	err := p.pool.QueryRow(ctx, `UPDATE inbox_messages SET imap_uid = nextval('inbox_messages_imap_uid_seq') WHERE id=$1 RETURNING imap_uid`, id).Scan(&uid)
	return imap.UID(uid), err
}
