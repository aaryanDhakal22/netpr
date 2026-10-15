package staging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "staging"), time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateEnforcesLimit(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create("big.pdf", strings.NewReader(strings.Repeat("x", 11)), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	entries, _ := os.ReadDir(s.dir)
	if len(entries) != 0 {
		t.Errorf("rejected upload left %d entries behind", len(entries))
	}
	it, err := s.Create("ok.pdf", strings.NewReader(strings.Repeat("x", 10)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(it.ID); ok {
		t.Error("item visible before Add")
	}
}

func TestTakeIsOnce(t *testing.T) {
	s := newStore(t)
	it, _ := s.Create("a.pdf", strings.NewReader("data"), 100)
	s.Add(it)
	if _, ok := s.Take(it.ID); !ok {
		t.Fatal("first take failed")
	}
	if _, ok := s.Take(it.ID); ok {
		t.Fatal("second take succeeded: a double submit would print twice")
	}
}

func TestSweepExpires(t *testing.T) {
	s := newStore(t)
	old, _ := s.Create("old.pdf", strings.NewReader("x"), 100)
	old.CreatedAt = time.Now().Add(-2 * time.Hour)
	s.Add(old)
	fresh, _ := s.Create("new.pdf", strings.NewReader("x"), 100)
	s.Add(fresh)

	s.sweep(time.Now())
	if _, ok := s.Get(old.ID); ok {
		t.Error("expired item still present")
	}
	if _, err := os.Stat(old.Dir); !os.IsNotExist(err) {
		t.Error("expired files not deleted")
	}
	if _, ok := s.Get(fresh.ID); !ok {
		t.Error("fresh item swept")
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":             "report.pdf",
		"../../etc/passwd":       "passwd",
		`C:\Users\me\scan.jpg`:   "scan.jpg",
		"bad\x00name\n.txt":      "badname.txt",
		"":                       "document",
		"   ":                    "document",
		strings.Repeat("é", 200): strings.Repeat("é", 120),
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
