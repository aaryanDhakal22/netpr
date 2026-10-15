package printer

import (
	"reflect"
	"testing"
)

func TestAttentionReasons(t *testing.T) {
	s := Status{Reachable: true, Reasons: []string{
		"none", "media-empty-error", "media-jam", "toner-low-warning", "toner-low",
		"other-report", "door-open-error", "media-low-warning",
	}}
	got := AttentionReasons(s)
	want := []string{"media-empty", "media-jam", "door-open"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := AttentionReasons(Status{}); !reflect.DeepEqual(got, []string{"unreachable"}) {
		t.Errorf("unreachable: got %v", got)
	}
}

func titles(alerts []Alert) []string {
	var out []string
	for _, a := range alerts {
		out = append(out, a.Title+": "+a.Message)
	}
	return out
}

func TestAlerterIsEdgeTriggered(t *testing.T) {
	a := &Alerter{TonerLowPct: 15}
	ok := Status{Reachable: true, Markers: []Marker{{Name: "Black", Level: 40}}}
	jam := Status{Reachable: true, Reasons: []string{"media-jam-error"}, Markers: []Marker{{Name: "Black", Level: 40}}}

	if got := a.Observe(ok); len(got) != 0 {
		t.Fatalf("healthy printer alerted: %v", titles(got))
	}
	if got := a.Observe(jam); len(got) != 1 || got[0].Message != "Paper jam" {
		t.Fatalf("jam: %v", titles(got))
	}
	if got := a.Observe(jam); len(got) != 0 {
		t.Fatalf("repeated jam alerted again: %v", titles(got))
	}
	a.Observe(ok)
	if got := a.Observe(jam); len(got) != 1 {
		t.Fatalf("jam after clearing should alert again: %v", titles(got))
	}
}

func TestAlerterToner(t *testing.T) {
	a := &Alerter{TonerLowPct: 15}
	level := func(n int) Status {
		return Status{Reachable: true, Markers: []Marker{{Name: "Black", Level: n}}}
	}
	if got := a.Observe(level(20)); len(got) != 0 {
		t.Fatalf("20%%: %v", titles(got))
	}
	if got := a.Observe(level(14)); len(got) != 1 || got[0].Message != "Black is at 14%" {
		t.Fatalf("14%%: %v", titles(got))
	}
	if got := a.Observe(level(10)); len(got) != 0 {
		t.Fatalf("still low should not re-alert: %v", titles(got))
	}
	// Going offline and back must not re-announce low toner.
	if got := a.Observe(Status{Error: "timeout"}); len(got) != 1 || got[0].Title != "Printer needs attention" {
		t.Fatalf("offline: %v", titles(got))
	}
	if got := a.Observe(level(9)); len(got) != 0 {
		t.Fatalf("back online: %v", titles(got))
	}
	a.Observe(level(100)) // new cartridge
	if got := a.Observe(level(5)); len(got) != 1 {
		t.Fatalf("low again after replacement: %v", titles(got))
	}
	if got := a.Observe(level(-2)); len(got) != 0 {
		t.Fatalf("unknown level alerted: %v", titles(got))
	}
}

func TestOutOfPaperWarningNeedsAttention(t *testing.T) {
	// What the Canon LBP246dw reported while holding a job for paper.
	s := Status{Reachable: true, State: "idle", Reasons: []string{"media-empty-report", "media-needed-warning"}}
	if got := AttentionReasons(s); !reflect.DeepEqual(got, []string{"media-needed"}) {
		t.Errorf("got %v", got)
	}
	a := &Alerter{}
	if alerts := a.Observe(s); len(alerts) != 1 || alerts[0].Message != "Load paper" {
		t.Errorf("alerts = %v", titles(alerts))
	}
}
