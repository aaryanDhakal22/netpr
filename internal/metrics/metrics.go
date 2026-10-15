// Package metrics exposes a few counters in the Prometheus text format.
package metrics

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type Metrics struct {
	mu sync.Mutex

	jobs          map[string]uint64 // terminal jobs by state
	pages, sheets uint64
	convCount     uint64
	convFailures  uint64
	convSeconds   float64
	printerUp     bool
	markers       map[string]int
	trays         map[string]int
}

func New() *Metrics {
	return &Metrics{jobs: map[string]uint64{}, markers: map[string]int{}, trays: map[string]int{}}
}

func (m *Metrics) JobFinished(state string, pages, sheets int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[state]++
	if state == "completed" {
		m.pages += uint64(pages)
		m.sheets += uint64(sheets)
	}
}

func (m *Metrics) Converted(d time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.convCount++
	m.convSeconds += d.Seconds()
	if err != nil {
		m.convFailures++
	}
}

// Printer records reachability, marker (toner) levels and paper tray levels
// by name.
func (m *Metrics) Printer(up bool, markers, trays map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.printerUp = up
	if up {
		m.markers = maps.Clone(markers)
		m.trays = maps.Clone(trays)
	}
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder

	b.WriteString("# HELP netpr_jobs_total Print jobs that reached a final state.\n# TYPE netpr_jobs_total counter\n")
	for _, state := range slices.Sorted(maps.Keys(m.jobs)) {
		fmt.Fprintf(&b, "netpr_jobs_total{state=%q} %d\n", state, m.jobs[state])
	}
	b.WriteString("# HELP netpr_pages_printed_total Pages in completed jobs, per copy.\n# TYPE netpr_pages_printed_total counter\n")
	fmt.Fprintf(&b, "netpr_pages_printed_total %d\n", m.pages)
	b.WriteString("# HELP netpr_sheets_printed_total Estimated sheets used by completed jobs.\n# TYPE netpr_sheets_printed_total counter\n")
	fmt.Fprintf(&b, "netpr_sheets_printed_total %d\n", m.sheets)
	b.WriteString("# HELP netpr_conversions_total Document conversions attempted.\n# TYPE netpr_conversions_total counter\n")
	fmt.Fprintf(&b, "netpr_conversions_total %d\n", m.convCount)
	b.WriteString("# HELP netpr_conversion_failures_total Document conversions that failed.\n# TYPE netpr_conversion_failures_total counter\n")
	fmt.Fprintf(&b, "netpr_conversion_failures_total %d\n", m.convFailures)
	b.WriteString("# HELP netpr_conversion_seconds_total Time spent converting documents.\n# TYPE netpr_conversion_seconds_total counter\n")
	fmt.Fprintf(&b, "netpr_conversion_seconds_total %g\n", m.convSeconds)
	b.WriteString("# HELP netpr_printer_up Whether the last printer status poll succeeded.\n# TYPE netpr_printer_up gauge\n")
	up := 0
	if m.printerUp {
		up = 1
	}
	fmt.Fprintf(&b, "netpr_printer_up %d\n", up)
	b.WriteString("# HELP netpr_printer_marker_level_percent Toner/ink level reported by the printer (-1 unknown).\n# TYPE netpr_printer_marker_level_percent gauge\n")
	for _, name := range slices.Sorted(maps.Keys(m.markers)) {
		fmt.Fprintf(&b, "netpr_printer_marker_level_percent{marker=%q} %d\n", name, m.markers[name])
	}
	b.WriteString("# HELP netpr_printer_tray_level Sheets in each paper tray (0 empty, -2 unknown, -3 some).\n# TYPE netpr_printer_tray_level gauge\n")
	for _, name := range slices.Sorted(maps.Keys(m.trays)) {
		fmt.Fprintf(&b, "netpr_printer_tray_level{tray=%q} %d\n", name, m.trays[name])
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}
