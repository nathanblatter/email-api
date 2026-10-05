// Package spool is the on-disk queue for mail the relay could not take:
// relay down, or daily budget exhausted. One JSON file per message so a
// restart loses nothing and an operator can inspect/delete with plain tools.
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reason records why a message was queued rather than sent.
type Reason string

const (
	ReasonRelayDown Reason = "relay_down"
	ReasonBudget    Reason = "daily_budget"
)

type Entry struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	Rcpts     []string  `json:"rcpts"`
	Subject   string    `json:"subject"`
	Preview   string    `json:"preview"` // first lines of the text body, for pages/listing
	Raw       []byte    `json:"raw"`
	Reason    Reason    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	LastTry   time.Time `json:"last_try,omitempty"`
}

type Spool struct {
	dir string
	mu  sync.Mutex
}

func Open(dir string) (*Spool, error) {
	for _, d := range []string{dir, filepath.Join(dir, "dead")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("spool dir %s: %w", d, err)
		}
	}
	return &Spool{dir: dir}, nil
}

func (s *Spool) path(e *Entry) string {
	return filepath.Join(s.dir, fmt.Sprintf("%d-%s.json", e.CreatedAt.UnixNano(), e.ID))
}

// Put writes (or rewrites) an entry atomically.
func (s *Spool) Put(e *Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(s.path(e), e)
}

func (s *Spool) write(path string, e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List returns queued entries oldest first.
func (s *Spool) List() ([]*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names) // filenames start with a nanosecond timestamp
	var out []*Entry
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			// A corrupt file must not wedge the whole queue; park it.
			_ = os.Rename(n, filepath.Join(s.dir, "dead", filepath.Base(n)+".corrupt"))
			continue
		}
		out = append(out, &e)
	}
	return out, nil
}

func (s *Spool) Count() int {
	es, _ := s.List()
	return len(es)
}

func (s *Spool) Delete(e *Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(e))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Dead moves an entry out of the retry set (gave up) but keeps the bytes.
func (s *Spool) Dead(e *Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.path(e)
	return os.Rename(p, filepath.Join(s.dir, "dead", filepath.Base(p)))
}

// Preview trims a text body to a short, single-paragraph summary.
func Preview(text string, max int) string {
	t := strings.TrimSpace(text)
	if len(t) > max {
		t = t[:max] + "…"
	}
	return t
}
