// Package staging holds uploaded files between upload and print.
package staging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Item struct {
	ID        string
	Filename  string // display name, sanitized
	Kind      string
	Pages     int
	CreatedAt time.Time
	Dir       string
}

func (i *Item) OriginalPath() string { return filepath.Join(i.Dir, "original") }
func (i *Item) PDFPath() string      { return filepath.Join(i.Dir, "document.pdf") }

type Store struct {
	dir string
	ttl time.Duration
	log *slog.Logger

	mu    sync.Mutex
	items map[string]*Item
}

// New prepares dir, discarding anything left from a previous run: staged
// files are never meant to outlive the process.
func New(dir string, ttl time.Duration, log *slog.Logger) (*Store, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, ttl: ttl, log: log, items: map[string]*Item{}}, nil
}

var ErrTooLarge = errors.New("file too large")

// Create writes src to a new staging directory, refusing more than maxBytes.
// The item is not visible to Get until Add is called.
func (s *Store) Create(filename string, src io.Reader, maxBytes int64) (*Item, error) {
	id := newID()
	it := &Item{ID: id, Filename: CleanName(filename), CreatedAt: time.Now(), Dir: filepath.Join(s.dir, id)}
	if err := os.Mkdir(it.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Create(it.OriginalPath())
	if err != nil {
		s.Discard(it)
		return nil, err
	}
	n, err := io.Copy(f, io.LimitReader(src, maxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxBytes {
		err = fmt.Errorf("%w: limit is %d MB", ErrTooLarge, maxBytes>>20)
	}
	if err != nil {
		s.Discard(it)
		return nil, err
	}
	return it, nil
}

// Add makes a fully prepared item available for printing.
func (s *Store) Add(it *Item) {
	s.mu.Lock()
	s.items[it.ID] = it
	s.mu.Unlock()
}

func (s *Store) Get(id string) (*Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	return it, ok
}

// Take removes an item from the index so it cannot be printed twice. The
// caller owns its files afterwards and must Discard them.
func (s *Store) Take(id string) (*Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	delete(s.items, id)
	return it, ok
}

// Remove drops an item and deletes its files.
func (s *Store) Remove(id string) {
	if it, ok := s.Take(id); ok {
		s.Discard(it)
	}
}

// Discard deletes an item's files.
func (s *Store) Discard(it *Item) {
	if err := os.RemoveAll(it.Dir); err != nil {
		s.log.Warn("remove staged files", "id", it.ID, "err", err)
	}
}

func (s *Store) sweep(now time.Time) {
	s.mu.Lock()
	var expired []*Item
	for id, it := range s.items {
		if now.Sub(it.CreatedAt) > s.ttl {
			expired = append(expired, it)
			delete(s.items, id)
		}
	}
	s.mu.Unlock()
	for _, it := range expired {
		s.Discard(it)
	}
}

func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sweep(now)
		}
	}
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CleanName reduces a client-supplied filename to a safe display name.
func CleanName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 120 {
		name = string(r[:120])
	}
	if name == "" || name == "." || name == "/" {
		name = "document"
	}
	return name
}
