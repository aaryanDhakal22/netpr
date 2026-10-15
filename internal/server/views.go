package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/labstack/echo/v5"

	"github.com/aaryanDhakal22/netpr/internal/printer"
	"github.com/aaryanDhakal22/netpr/internal/store"
	"github.com/aaryanDhakal22/netpr/web"
)

// renderer executes full pages through the layout and fragments directly.
// Each page is parsed into its own clone of the shared set so every page can
// define the same "content" block.
type renderer struct {
	pages     map[string]*template.Template
	fragments *template.Template
}

func newRenderer() (*renderer, error) {
	versions, err := assetVersions()
	if err != nil {
		return nil, err
	}
	funcs["asset"] = func(name string) string {
		if v, ok := versions[name]; ok {
			return "/static/" + name + "?v=" + v
		}
		return "/static/" + name
	}
	base, err := template.New("").Funcs(funcs).ParseFS(web.Templates, "templates/layout.html", "templates/partials/*.html")
	if err != nil {
		return nil, err
	}
	pageFiles, err := fs.Glob(web.Templates, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	r := &renderer{pages: map[string]*template.Template{}, fragments: base}
	for _, f := range pageFiles {
		t, err := template.Must(base.Clone()).ParseFS(web.Templates, f)
		if err != nil {
			return nil, err
		}
		r.pages[strings.TrimSuffix(path.Base(f), ".html")] = t
	}
	return r, nil
}

// assetVersions fingerprints every static file, so pages can link to
// /static/css/app.css?v=<hash> and browsers fetch a file again exactly when
// its contents change.
func assetVersions() (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(web.Static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(web.Static, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:5])
		return nil
	})
	return out, err
}

func (r *renderer) Render(_ *echo.Context, w io.Writer, name string, data any) error {
	if t, ok := r.pages[name]; ok {
		return t.ExecuteTemplate(w, "layout", data)
	}
	return r.fragments.ExecuteTemplate(w, name, data)
}

var funcs = template.FuncMap{
	"stateLabel": stateLabel,
	"kindLabel":  kindLabel,
	"ago":        ago,
	"stamp":      func(t time.Time) string { return t.Format("Jan 2, 2006 15:04") },
	"plural": func(n int, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return fmt.Sprintf("%d %ss", n, word)
	},
	"terminal": store.Terminal,
	"join":     strings.Join,
	"progress": progress,
}

func stateLabel(state string) string {
	switch state {
	case store.StateQueued:
		return "Queued"
	case store.StatePreparing:
		return "Preparing"
	case store.StateSending:
		return "Sending"
	case store.StatePending:
		return "Waiting"
	case store.StateProcessing:
		return "Printing"
	case store.StateCompleted:
		return "Printed"
	case store.StateCanceled:
		return "Canceled"
	case store.StateAborted:
		return "Aborted"
	case store.StateFailed:
		return "Failed"
	case store.StateDryRun:
		return "Dry run"
	case store.StateUnknown:
		return "Unknown"
	}
	return state
}

// progress is a rough percentage for each stage of an unfinished job.
func progress(state string) int {
	switch state {
	case store.StateQueued:
		return 10
	case store.StatePreparing:
		return 30
	case store.StateSending:
		return 50
	case store.StatePending:
		return 65
	case store.StateProcessing:
		return 85
	}
	return 100
}

func kindLabel(kind string) string {
	switch kind {
	case "pdf":
		return "PDF"
	case "image":
		return "Image"
	case "office":
		return "Document"
	case "text":
		return "Text"
	}
	return kind
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("Jan 2")
}

type markerView struct {
	Name  string
	Level int
	Known bool
	Low   bool
}

type paperView struct {
	Name    string
	Size    string // Letter, A4, ...; "" when unknown
	Label   string // Empty, Has paper, 120 of 250 sheets, ...
	Tone    string // ok, warn, error, muted
	Bar     bool
	Percent int
}

type printerView struct {
	Checked   bool
	Reachable bool
	Label     string // Ready, Printing, Stopped, Offline, or the one problem
	Tone      string // ok, busy, warn, error
	Model     string
	Attention []string
	Warnings  []string
	Markers   []markerView
	Paper     []paperView
	Duplex    bool
}

func newPrinterView(s printer.Status, tonerLowPct int) printerView {
	v := printerView{Checked: !s.CheckedAt.IsZero(), Reachable: s.Reachable, Model: s.MakeModel, Duplex: true}
	if !v.Checked {
		v.Label, v.Tone = "Checking…", "busy"
		return v
	}
	if !s.Reachable {
		v.Label, v.Tone = "Offline", "error"
		v.Attention = []string{printer.ReasonLabel("unreachable")}
		return v
	}
	v.Duplex = s.SupportsDuplex()
	for _, r := range printer.AttentionReasons(s) {
		v.Attention = append(v.Attention, printer.ReasonLabel(r))
	}
	for _, r := range s.Reasons {
		if strings.HasSuffix(r, "-warning") && !printer.IsUrgentWarning(r) {
			v.Warnings = append(v.Warnings, printer.ReasonLabel(strings.TrimSuffix(r, "-warning")))
		}
	}
	for _, m := range s.Markers {
		mv := markerView{Name: m.Name, Level: m.Level, Known: m.Level >= 0}
		mv.Low = mv.Known && m.Level < tonerLowPct
		v.Markers = append(v.Markers, mv)
	}
	v.Paper = newPaperView(s)
	switch {
	case len(v.Attention) == 1:
		v.Label, v.Tone = v.Attention[0], "error"
	case len(v.Attention) > 0:
		v.Label, v.Tone = "Needs attention", "error"
	case s.State == "processing":
		v.Label, v.Tone = "Printing", "busy"
	case s.State == "stopped":
		v.Label, v.Tone = "Stopped", "warn"
	case len(v.Warnings) > 0:
		v.Label, v.Tone = "Ready", "warn"
	default:
		v.Label, v.Tone = "Ready", "ok"
	}
	return v
}

// jobFinishedEvent is the HX-Trigger header announcing a finished job, which
// app.js shows as a toast. Non-ASCII is escaped because header values are
// read as Latin-1 by the browser.
func jobFinishedEvent(j store.Job) string {
	title := "Printed"
	switch j.State {
	case store.StateDryRun:
		title = "Printed (dry run)"
	case store.StateCompleted:
	case store.StateUnknown:
		title = "Lost track of print"
	default:
		title = "Print " + strings.ToLower(stateLabel(j.State))
	}
	b, _ := json.Marshal(map[string]any{"job-finished": map[string]any{
		"title": title, "name": j.Filename, "ok": j.State == store.StateCompleted || j.State == store.StateDryRun,
	}})
	var out strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r > 0xffff:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&out, `\u%04x\u%04x`, r1, r2)
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.String()
}

// paperReasons sums up printer-state-reasons about paper, at any severity.
func paperReasons(reasons []string) (empty, low bool) {
	for _, r := range reasons {
		base := r
		for _, suffix := range []string{"-error", "-warning", "-report"} {
			base = strings.TrimSuffix(base, suffix)
		}
		switch base {
		case "media-empty", "media-needed":
			// A -report alone usually means one tray (often the bypass) is empty.
			if !strings.HasSuffix(r, "-report") {
				empty = true
			}
		case "media-low":
			low = true
		}
	}
	return empty, low
}

// newPaperView describes each paper source. Printers that don't report
// trays get a single row when their state reasons mention paper.
func newPaperView(s printer.Status) []paperView {
	empty, low := paperReasons(s.Reasons)
	if len(s.Trays) == 0 {
		switch {
		case empty:
			return []paperView{{Name: "Paper", Label: "Empty", Tone: "error"}}
		case low:
			return []paperView{{Name: "Paper", Label: "Low", Tone: "warn"}}
		}
		return nil
	}
	var out []paperView
	for _, t := range s.Trays {
		p := paperView{Name: t.Name, Size: paperSize(t.Feed, t.CrossFeed)}
		switch {
		case t.Level == 0:
			p.Label, p.Tone = "Empty", "error"
			if t.Bypass() {
				p.Tone = "muted" // the manual slot is normally empty
			}
			p.Bar = t.Capacity > 0
		case t.Level > 0:
			p.Label, p.Tone = fmt.Sprintf("%d sheets", t.Level), "ok"
			if t.Capacity > 0 {
				p.Label = fmt.Sprintf("%d of %d sheets", t.Level, t.Capacity)
				p.Bar, p.Percent = true, min(100, t.Level*100/t.Capacity)
			}
		case t.Level == printer.TrayLevelSome:
			p.Label, p.Tone = "Has paper", "ok"
			if low && !t.Bypass() {
				p.Label, p.Tone = "Running low", "warn"
			}
		default:
			p.Label, p.Tone = "Unknown", "muted"
			if empty && !t.Bypass() {
				p.Label, p.Tone = "Probably empty", "error"
			}
		}
		if p.Bar && p.Percent < 10 && p.Tone == "ok" {
			p.Tone = "warn"
		}
		out = append(out, p)
	}
	return out
}

// paperSize names a media size given in hundredths of a millimetre.
func paperSize(feed, xfeed int) string {
	if feed <= 0 || xfeed <= 0 {
		return ""
	}
	near := func(a, b int) bool { return a-b < 150 && b-a < 150 }
	for _, s := range []struct {
		name string
		w, h int
	}{{"Letter", 21590, 27940}, {"A4", 21000, 29700}, {"Legal", 21590, 35560}, {"A5", 14800, 21000}} {
		if (near(xfeed, s.w) && near(feed, s.h)) || (near(xfeed, s.h) && near(feed, s.w)) {
			return s.name
		}
	}
	return fmt.Sprintf("%.0f × %.0f mm", float64(xfeed)/100, float64(feed)/100)
}
