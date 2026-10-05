package inbox

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests.
type Memory struct {
	mu   sync.Mutex
	msgs map[string]*Message
	blob map[string][]byte
	Now  func() time.Time
}

func NewMemory() *Memory {
	return &Memory{msgs: map[string]*Message{}, blob: map[string][]byte{}, Now: time.Now}
}

func (m *Memory) Save(_ context.Context, p *Parsed, envFrom, envTo string, raw []byte) (*Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, _ := newID()
	msg := fromParsed(id, p, envFrom, envTo, len(raw), m.Now())
	msg.RawKey = id + "/raw.eml"
	m.blob[msg.RawKey] = raw
	for i, a := range p.Attachments {
		aid, _ := newID()
		key := id + "/" + string(rune('0'+i)) + "-" + a.Filename
		m.blob[key] = a.Content
		msg.Attachments = append(msg.Attachments, AttachmentMeta{ID: aid, Filename: a.Filename, ContentType: a.ContentType, Size: len(a.Content), ContentID: a.ContentID, Inline: a.Inline, ObjectKey: key})
	}
	m.msgs[id] = msg
	return msg, nil
}

func (m *Memory) List(_ context.Context, o ListOptions) ([]Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []*Message
	for _, msg := range m.msgs {
		if o.Unread && msg.Read {
			continue
		}
		if o.Suspicious != nil && msg.Suspicious != *o.Suspicious {
			continue
		}
		if o.Query != "" {
			q := strings.ToLower(o.Query)
			if !strings.Contains(strings.ToLower(msg.FromAddr+" "+msg.FromName+" "+msg.Subject+" "+msg.Text), q) {
				continue
			}
		}
		all = append(all, msg)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ReceivedAt.After(all[j].ReceivedAt) })
	if o.Limit > 0 && len(all) > o.Limit {
		all = all[:o.Limit]
	}
	out := []Summary{}
	for _, msg := range all {
		prev := msg.Text
		if len(prev) > 200 {
			prev = prev[:200]
		}
		out = append(out, Summary{ID: msg.ID, ReceivedAt: msg.ReceivedAt, FromName: msg.FromName, FromAddr: msg.FromAddr, To: msg.To,
			Subject: msg.Subject, Preview: strings.TrimSpace(prev), Read: msg.Read, Suspicious: msg.Suspicious, Attachments: len(msg.Attachments)})
	}
	return out, nil
}

func (m *Memory) Get(_ context.Context, id string) (*Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.msgs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *msg
	return &cp, nil
}

func (m *Memory) Attachment(_ context.Context, id, attID string) (*AttachmentMeta, io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.msgs[id]
	if !ok {
		return nil, nil, ErrNotFound
	}
	for _, a := range msg.Attachments {
		if a.ID == attID {
			a := a
			return &a, io.NopCloser(strings.NewReader(string(m.blob[a.ObjectKey]))), nil
		}
	}
	return nil, nil, ErrNotFound
}

func (m *Memory) MarkRead(_ context.Context, id string, read bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.msgs[id]
	if !ok {
		return ErrNotFound
	}
	msg.Read = read
	return nil
}

func (m *Memory) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.msgs[id]; !ok {
		return ErrNotFound
	}
	delete(m.msgs, id)
	return nil
}

func (m *Memory) Unread(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, msg := range m.msgs {
		if !msg.Read && !msg.Suspicious {
			n++
		}
	}
	return n, nil
}
