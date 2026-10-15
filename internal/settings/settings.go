// Package settings holds the options that can be changed from the printer
// page. Each starts from its environment variable; a value saved in the UI
// overrides it until reset.
package settings

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
)

type Values struct {
	PrinterURI    string
	Copies        int  // default for the print form
	Duplex        bool // default for the print form
	PaperSize     string
	ConfirmSheets int
	TonerLowPct   int
}

// Setting keys as stored in the database.
const (
	KeyPrinterURI    = "printer_uri"
	KeyCopies        = "default_copies"
	KeyDuplex        = "default_duplex"
	KeyPaperSize     = "paper_size"
	KeyConfirmSheets = "confirm_sheets"
	KeyTonerLowPct   = "toner_low_pct"
)

// DefaultKeys are the print defaults, saved and reset together.
var DefaultKeys = []string{KeyCopies, KeyDuplex, KeyPaperSize, KeyConfirmSheets, KeyTonerLowPct}

func (v Values) Validate() error {
	var errs []error
	if v.Copies < 1 || v.Copies > 99 {
		errs = append(errs, errors.New("default copies must be between 1 and 99"))
	}
	if v.PaperSize != "letter" && v.PaperSize != "a4" {
		errs = append(errs, errors.New("paper size must be letter or a4"))
	}
	if v.ConfirmSheets < 0 || v.ConfirmSheets > 10000 {
		errs = append(errs, errors.New("the big-job check must be between 0 (off) and 10000 sheets"))
	}
	if v.TonerLowPct < 0 || v.TonerLowPct > 100 {
		errs = append(errs, errors.New("the toner alert must be between 0 and 100%"))
	}
	return errors.Join(errs...)
}

func (v Values) encode() map[string]string {
	return map[string]string{
		KeyPrinterURI:    v.PrinterURI,
		KeyCopies:        strconv.Itoa(v.Copies),
		KeyDuplex:        strconv.FormatBool(v.Duplex),
		KeyPaperSize:     v.PaperSize,
		KeyConfirmSheets: strconv.Itoa(v.ConfirmSheets),
		KeyTonerLowPct:   strconv.Itoa(v.TonerLowPct),
	}
}

func (v *Values) set(key, val string) error {
	var err error
	switch key {
	case KeyPrinterURI:
		v.PrinterURI = val
	case KeyCopies:
		v.Copies, err = strconv.Atoi(val)
	case KeyDuplex:
		v.Duplex, err = strconv.ParseBool(val)
	case KeyPaperSize:
		v.PaperSize = val
	case KeyConfirmSheets:
		v.ConfirmSheets, err = strconv.Atoi(val)
	case KeyTonerLowPct:
		v.TonerLowPct, err = strconv.Atoi(val)
	default:
		err = errors.New("unknown setting")
	}
	return err
}

// Backend persists overrides; *store.Store implements it.
type Backend interface {
	Settings(ctx context.Context) (map[string]string, error)
	SaveSettings(ctx context.Context, user string, set map[string]string, del []string) error
}

type Settings struct {
	db  Backend
	env Values

	mu        sync.RWMutex
	cur       Values
	overrides map[string]bool
}

// Load applies saved overrides on top of env. A saved value that no longer
// parses is ignored, falling back to env.
func Load(ctx context.Context, db Backend, env Values) (*Settings, []error, error) {
	saved, err := db.Settings(ctx)
	if err != nil {
		return nil, nil, err
	}
	s := &Settings{db: db, env: env, cur: env, overrides: map[string]bool{}}
	var ignored []error
	for _, k := range slices.Sorted(maps.Keys(saved)) {
		next := s.cur
		if err := next.set(k, saved[k]); err != nil {
			ignored = append(ignored, fmt.Errorf("setting %s=%q: %w", k, saved[k], err))
			continue
		}
		if err := next.Validate(); err != nil {
			ignored = append(ignored, fmt.Errorf("setting %s=%q: %w", k, saved[k], err))
			continue
		}
		s.cur = next
		s.overrides[k] = true
	}
	return s, ignored, nil
}

func (s *Settings) Get() Values {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Env returns the values from environment variables, before overrides.
func (s *Settings) Env() Values { return s.env }

// Overridden reports which keys are set from the UI rather than the environment.
func (s *Settings) Overridden() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.overrides)
}

// SaveDefaults stores the print defaults from v. Values equal to the
// environment's are stored as "no override", so changing .env later applies.
func (s *Settings) SaveDefaults(ctx context.Context, user string, v Values) error {
	if err := v.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	next.Copies, next.Duplex, next.PaperSize = v.Copies, v.Duplex, v.PaperSize
	next.ConfirmSheets, next.TonerLowPct = v.ConfirmSheets, v.TonerLowPct
	return s.commit(ctx, user, next, DefaultKeys)
}

// SetPrinterURI switches the printer. The caller validates the address.
func (s *Settings) SetPrinterURI(ctx context.Context, user, uri string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	next.PrinterURI = uri
	return s.commit(ctx, user, next, []string{KeyPrinterURI})
}

// Reset drops the overrides for keys, returning them to the environment's values.
func (s *Settings) Reset(ctx context.Context, user string, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	envVals := s.env.encode()
	for _, k := range keys {
		if err := next.set(k, envVals[k]); err != nil {
			return err
		}
	}
	return s.commit(ctx, user, next, keys)
}

// commit stores keys of next and makes next current. Callers hold s.mu.
func (s *Settings) commit(ctx context.Context, user string, next Values, keys []string) error {
	nextVals, envVals := next.encode(), s.env.encode()
	set, overrides := map[string]string{}, maps.Clone(s.overrides)
	var del []string
	for _, k := range keys {
		if nextVals[k] == envVals[k] {
			del = append(del, k)
			delete(overrides, k)
		} else {
			set[k] = nextVals[k]
			overrides[k] = true
		}
	}
	if err := s.db.SaveSettings(ctx, user, set, del); err != nil {
		return err
	}
	s.cur, s.overrides = next, overrides
	return nil
}
