package settings

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aaryanDhakal22/netpr/internal/store"
)

var env = Values{
	PrinterURI: "ipp://10.0.0.5/ipp/print", Copies: 1, Duplex: true,
	PaperSize: "letter", ConfirmSheets: 30, TonerLowPct: 15,
}

func open(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "netpr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOverridesPersistAndReset(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	s, ignored, err := Load(ctx, st, env)
	if err != nil || len(ignored) > 0 {
		t.Fatal(err, ignored)
	}

	v := s.Get()
	v.Duplex, v.PaperSize, v.ConfirmSheets = false, "a4", 0
	if err := s.SaveDefaults(ctx, "me", v); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrinterURI(ctx, "me", "ipp://10.0.0.9/ipp/print"); err != nil {
		t.Fatal(err)
	}

	// A fresh process sees the same values.
	s2, _, err := Load(ctx, st, env)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Get()
	want := Values{PrinterURI: "ipp://10.0.0.9/ipp/print", Copies: 1, Duplex: false,
		PaperSize: "a4", ConfirmSheets: 0, TonerLowPct: 15}
	if got != want {
		t.Errorf("reloaded = %+v, want %+v", got, want)
	}
	// Only values that differ from the environment are overrides.
	o := s2.Overridden()
	if !o[KeyDuplex] || !o[KeyPaperSize] || !o[KeyConfirmSheets] || !o[KeyPrinterURI] || o[KeyCopies] || o[KeyTonerLowPct] {
		t.Errorf("overrides = %v", o)
	}

	if err := s2.Reset(ctx, "me", DefaultKeys...); err != nil {
		t.Fatal(err)
	}
	if got := s2.Get(); got.Duplex != true || got.PaperSize != "letter" || got.PrinterURI != "ipp://10.0.0.9/ipp/print" {
		t.Errorf("after resetting defaults = %+v", got)
	}
	if err := s2.Reset(ctx, "me", KeyPrinterURI); err != nil {
		t.Fatal(err)
	}
	s3, _, _ := Load(ctx, st, env)
	if s3.Get() != env || len(s3.Overridden()) != 0 {
		t.Errorf("after full reset = %+v %v", s3.Get(), s3.Overridden())
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	s, _, _ := Load(ctx, open(t), env)
	for _, bad := range []Values{
		{Copies: 0, PaperSize: "letter"},
		{Copies: 1, PaperSize: "legal"},
		{Copies: 1, PaperSize: "a4", TonerLowPct: 101},
		{Copies: 1, PaperSize: "a4", ConfirmSheets: -1},
	} {
		if err := s.SaveDefaults(ctx, "me", bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if s.Get() != env {
		t.Errorf("rejected save changed values: %+v", s.Get())
	}
}

func TestBadSavedValueIgnored(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	st.SaveSettings(ctx, "me", map[string]string{KeyCopies: "lots", KeyTonerLowPct: "20"}, nil)
	s, ignored, err := Load(ctx, st, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(ignored) != 1 || s.Get().Copies != 1 || s.Get().TonerLowPct != 20 {
		t.Errorf("ignored=%v values=%+v", ignored, s.Get())
	}
}
