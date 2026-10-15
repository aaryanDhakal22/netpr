package printer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Monitor polls printer status and keeps the latest snapshot.
type Monitor struct {
	printer  *Active
	interval time.Duration
	onUpdate func(Status)

	mu  sync.RWMutex
	cur Status
}

func NewMonitor(p *Active, interval time.Duration, onUpdate func(Status)) *Monitor {
	return &Monitor{printer: p, interval: interval, onUpdate: onUpdate}
}

func (m *Monitor) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur
}

// Refresh polls the printer now and returns the new snapshot.
func (m *Monitor) Refresh(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := m.printer.Get()
	st := c.Status(ctx)
	m.mu.Lock()
	if m.printer.Get() != c {
		// The printer was switched while this poll ran; its result is stale.
		st = m.cur
		m.mu.Unlock()
		return st
	}
	m.cur = st
	m.mu.Unlock()
	if m.onUpdate != nil {
		m.onUpdate(st)
	}
	return st
}

func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Refresh(ctx)
		}
	}
}

// urgentWarnings are reasons some printers (e.g. Canon) report only as
// warnings even though nothing prints until someone acts.
var urgentWarnings = map[string]bool{"media-empty-warning": true, "media-needed-warning": true}

func IsUrgentWarning(reason string) bool { return urgentWarnings[reason] }

// AttentionReasons returns printer-state-reasons that need a human, keyed by
// reason without its severity suffix. Per RFC 8011 a reason with no suffix is
// an error. Low toner is handled separately through marker levels.
func AttentionReasons(s Status) []string {
	if !s.Reachable {
		return []string{"unreachable"}
	}
	var out []string
	for _, r := range s.Reasons {
		if urgentWarnings[r] {
			r = strings.TrimSuffix(r, "-warning")
		} else if r == "none" || strings.HasSuffix(r, "-report") || strings.HasSuffix(r, "-warning") {
			continue
		}
		base := strings.TrimSuffix(r, "-error")
		if base == "toner-low" || base == "marker-supply-low" {
			continue
		}
		if !slices.Contains(out, base) {
			out = append(out, base)
		}
	}
	return out
}

var reasonLabels = map[string]string{
	"unreachable":          "Printer is unreachable",
	"media-empty":          "Out of paper",
	"media-needed":         "Load paper",
	"media-jam":            "Paper jam",
	"door-open":            "Door open",
	"cover-open":           "Cover open",
	"input-tray-missing":   "Paper tray missing",
	"toner-empty":          "Toner empty",
	"marker-supply-empty":  "Toner empty",
	"offline":              "Printer offline",
	"paused":               "Printer paused",
	"shutdown":             "Printer shutting down",
	"connecting-to-device": "Connecting to printer",
	"output-area-full":     "Output tray full",
}

// ReasonLabel turns an IPP reason keyword into something readable.
func ReasonLabel(reason string) string {
	if l, ok := reasonLabels[reason]; ok {
		return l
	}
	return strings.ReplaceAll(reason, "-", " ")
}

type Alert struct {
	Title   string
	Message string
}

// Alerter turns successive status snapshots into edge-triggered alerts: a
// condition alerts once when it appears and again only after it has cleared.
type Alerter struct {
	TonerLowPct int

	attention map[string]bool
	tonerLow  map[string]bool
}

func (a *Alerter) Observe(s Status) []Alert {
	if a.attention == nil {
		a.attention, a.tonerLow = map[string]bool{}, map[string]bool{}
	}
	var alerts []Alert

	now := map[string]bool{}
	for _, r := range AttentionReasons(s) {
		now[r] = true
		if !a.attention[r] {
			msg := ReasonLabel(r)
			if r == "unreachable" && s.Error != "" {
				msg += ": " + s.Error
			} else if s.Message != "" {
				msg += ": " + s.Message
			}
			alerts = append(alerts, Alert{Title: "Printer needs attention", Message: msg})
		}
	}
	a.attention = now

	// Marker levels are unknown while unreachable; keep the previous toner state.
	if s.Reachable {
		for _, m := range s.Markers {
			if m.Level < 0 {
				continue
			}
			low := m.Level < a.TonerLowPct
			if low && !a.tonerLow[m.Name] {
				alerts = append(alerts, Alert{
					Title:   "Toner low",
					Message: fmt.Sprintf("%s is at %d%%", m.Name, m.Level),
				})
			}
			a.tonerLow[m.Name] = low
		}
	}
	return alerts
}
