package server

import (
	"time"

	"testing"

	"github.com/aaryanDhakal22/netpr/internal/printer"
)

func TestPaperView(t *testing.T) {
	drawer := func(level int) printer.Tray {
		return printer.Tray{Name: "Drawer1", Type: "sheetFeedAutoRemovableTray", Capacity: 250, Level: level, Feed: 27940, CrossFeed: 21590}
	}
	bypass := printer.Tray{Name: "Stack Bypass", Type: "sheetFeedAutoNonRemovableTray", Capacity: 100, Level: 0}

	cases := []struct {
		name   string
		status printer.Status
		want   []paperView
	}{
		{"out of paper, as the LBP246dw reports it",
			printer.Status{Reasons: []string{"media-empty-report", "media-needed-warning"}, Trays: []printer.Tray{bypass, drawer(0)}},
			[]paperView{
				{Name: "Stack Bypass", Label: "Empty", Tone: "muted", Bar: true},
				{Name: "Drawer1", Size: "Letter", Label: "Empty", Tone: "error", Bar: true},
			}},
		{"some paper",
			printer.Status{Reasons: []string{"none"}, Trays: []printer.Tray{drawer(printer.TrayLevelSome)}},
			[]paperView{{Name: "Drawer1", Size: "Letter", Label: "Has paper", Tone: "ok"}}},
		{"counted and low",
			printer.Status{Trays: []printer.Tray{drawer(20)}},
			[]paperView{{Name: "Drawer1", Size: "Letter", Label: "20 of 250 sheets", Tone: "warn", Bar: true, Percent: 8}}},
		{"media-low with some paper",
			printer.Status{Reasons: []string{"media-low-warning"}, Trays: []printer.Tray{drawer(printer.TrayLevelSome)}},
			[]paperView{{Name: "Drawer1", Size: "Letter", Label: "Running low", Tone: "warn"}}},
		{"no tray info, out of paper",
			printer.Status{Reasons: []string{"media-empty-error"}},
			[]paperView{{Name: "Paper", Label: "Empty", Tone: "error"}}},
		{"no tray info, fine",
			printer.Status{Reasons: []string{"none"}}, nil},
	}
	for _, c := range cases {
		got := newPaperView(c.status)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %+v", c.name, got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: row %d = %+v, want %+v", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestPillNamesSingleProblem(t *testing.T) {
	v := newPrinterView(printer.Status{Reachable: true, CheckedAt: timeNow(), State: "idle",
		Reasons: []string{"media-empty-report", "media-needed-warning"}}, 15)
	if v.Label != "Load paper" || v.Tone != "error" {
		t.Errorf("label = %q tone = %q", v.Label, v.Tone)
	}
}

func TestPaperSize(t *testing.T) {
	for in, want := range map[[2]int]string{{27940, 21590}: "Letter", {29700, 21000}: "A4", {21590, 27940}: "Letter", {20000, 10000}: "100 × 200 mm", {0, 0}: ""} {
		if got := paperSize(in[0], in[1]); got != want {
			t.Errorf("%v = %q, want %q", in, got, want)
		}
	}
}

func timeNow() time.Time { return time.Now() }
