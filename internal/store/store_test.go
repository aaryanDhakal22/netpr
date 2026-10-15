package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "netpr.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	a := Job{User: "aaryan", Filename: "a.pdf", Kind: "pdf", Pages: 3, Copies: 1, Duplex: true, Sheets: 2, State: StateQueued}
	b := Job{User: "guest", Filename: "b.docx", Kind: "office", Pages: 10, Copies: 2, PageRange: "1-5", Sheets: 10, State: StateQueued}
	for _, j := range []*Job{&a, &b} {
		if err := s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if a.ID == 0 || b.ID <= a.ID {
		t.Fatalf("ids: %d %d", a.ID, b.ID)
	}

	if err := s.UpdateState(ctx, a.ID, StatePending, "", 77); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateState(ctx, a.ID, StateCompleted, "done", 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCompleted || got.IPPJobID != 77 || got.Message != "done" || !got.Duplex {
		t.Errorf("got %+v", got) // a zero ippJobID must not erase the earlier one
	}

	list, err := s.List(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b.ID || list[1].PageRange != "" || list[0].PageRange != "1-5" {
		t.Errorf("list newest first: %+v", list)
	}
	older, _ := s.List(ctx, 10, b.ID)
	if len(older) != 1 || older[0].ID != a.ID {
		t.Errorf("pagination: %+v", older)
	}

	unfinished, _ := s.Unfinished(ctx)
	if len(unfinished) != 1 || unfinished[0].ID != b.ID {
		t.Errorf("unfinished: %+v", unfinished)
	}

	if _, err := s.Get(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing job: %v", err)
	}

	// Reopening must not re-run migrations or lose data.
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if list, _ := s.List(ctx, 10, 0); len(list) != 2 {
		t.Errorf("after reopen: %d jobs", len(list))
	}
}
