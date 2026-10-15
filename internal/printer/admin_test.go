package printer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ipp "github.com/phin1x/go-ipp"
)

func TestParseAddress(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.50":                      "ipp://192.168.1.50/ipp/print",
		" 192.168.1.50 ":                    "ipp://192.168.1.50/ipp/print",
		"printer.lan:8631":                  "ipp://printer.lan:8631/ipp/print",
		"ipp://192.168.1.50":                "ipp://192.168.1.50/ipp/print",
		"ipps://192.168.1.50/ipp/print":     "ipps://192.168.1.50/ipp/print",
		"ipp://192.168.1.50/ipp/printer/q1": "ipp://192.168.1.50/ipp/printer/q1",
	} {
		got, err := ParseAddress(in)
		if err != nil || got != want {
			t.Errorf("ParseAddress(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "http://192.168.1.50/", "ipp://", "ipp://u:p@host/", "ipp://host/?x=1"} {
		if _, err := ParseAddress(bad); err == nil {
			t.Errorf("ParseAddress(%q) accepted", bad)
		}
	}
}

func TestJobsAndCancel(t *testing.T) {
	f, c := newFake(t, func(req *ipp.Request) *ipp.Response {
		switch req.Operation {
		case ipp.OperationGetJobs:
			resp := ipp.NewResponse(ipp.StatusOk, 0)
			resp.JobAttributes = []ipp.Attributes{
				{
					"job-id":                    attr(ipp.TagInteger, "job-id", 12),
					"job-name":                  attr(ipp.TagName, "job-name", "report.pdf"),
					"job-originating-user-name": attr(ipp.TagName, "job-originating-user-name", "aaryan"),
					"job-state":                 attr(ipp.TagEnum, "job-state", int(ipp.JobStateProcessing)),
					"job-state-reasons":         attr(ipp.TagKeyword, "job-state-reasons", "job-printing"),
				},
				{
					"job-id":    attr(ipp.TagInteger, "job-id", 13),
					"job-state": attr(ipp.TagEnum, "job-state", int(ipp.JobStateHeld)),
				},
			}
			return resp
		case ipp.OperationCancelJob:
			if req.OperationAttributes["job-id"] == 99 {
				return ipp.NewResponse(ipp.StatusErrorNotFound, 0)
			}
			if req.OperationAttributes["requesting-user-name"] != "aaryan" {
				return ipp.NewResponse(ipp.StatusErrorNotAuthorized, 0)
			}
			return ipp.NewResponse(ipp.StatusOk, 0)
		}
		return ipp.NewResponse(ipp.StatusErrorOperationNotSupported, 0)
	})
	ctx := context.Background()

	jobs, err := c.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != 12 || jobs[0].Name != "report.pdf" || jobs[0].User != "aaryan" ||
		jobs[0].State != "processing" || jobs[1].State != "pending" {
		t.Errorf("jobs = %+v", jobs)
	}
	if got := f.reqs[0].OperationAttributes["which-jobs"]; got != "not-completed" {
		t.Errorf("which-jobs = %v", got)
	}

	if err := c.CancelJob(ctx, 12, "aaryan"); err != nil {
		t.Errorf("cancel as owner: %v", err)
	}
	var ie *IPPError
	if err := c.CancelJob(ctx, 12, "someone"); !errors.As(err, &ie) || ie.Status != ipp.StatusErrorNotAuthorized {
		t.Errorf("cancel as other user: %v", err)
	}
	if err := c.CancelJob(ctx, 99, "aaryan"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("cancel finished job: %v", err)
	}
}

func TestAttributes(t *testing.T) {
	_, c := newFake(t, func(req *ipp.Request) *ipp.Response {
		resp := ipp.NewResponse(ipp.StatusOk, 0)
		resp.PrinterAttributes = []ipp.Attributes{{
			"printer-name":     attr(ipp.TagName, "printer-name", "Canon\x00LBP"),
			"copies-supported": attr(ipp.TagInteger, "copies-supported", 1),
			"sides-supported":  attr(ipp.TagKeyword, "sides-supported", "one-sided", "two-sided-long-edge"),
		}}
		return resp
	})
	attrs, err := c.Attributes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, a := range attrs {
		got[a.Name] = a.Value
	}
	if got["printer-name"] != "CanonLBP" || got["sides-supported"] != "one-sided, two-sided-long-edge" || got["copies-supported"] != "1" {
		t.Errorf("attrs = %v", got)
	}
	if attrs[0].Name != "copies-supported" {
		t.Error("attributes not sorted")
	}
}

func TestFormatCollection(t *testing.T) {
	col := ipp.Collection{
		"media-size-name": {{Tag: ipp.TagKeyword, Value: "na_letter_8.5x11in"}},
		"media-type":      {{Tag: ipp.TagKeyword, Value: "stationery"}},
	}
	if got := formatValue(col); got != "{media-size-name=na_letter_8.5x11in media-type=stationery}" {
		t.Errorf("got %q", got)
	}
}

func TestMonitorIgnoresStalePoll(t *testing.T) {
	release := make(chan struct{})
	_, slow := newFake(t, func(*ipp.Request) *ipp.Response {
		<-release
		resp := ipp.NewResponse(ipp.StatusOk, 0)
		resp.PrinterAttributes = []ipp.Attributes{{"printer-make-and-model": attr(ipp.TagText, "printer-make-and-model", "Old")}}
		return resp
	})
	_, fast := newFake(t, func(*ipp.Request) *ipp.Response {
		resp := ipp.NewResponse(ipp.StatusOk, 0)
		resp.PrinterAttributes = []ipp.Attributes{{"printer-make-and-model": attr(ipp.TagText, "printer-make-and-model", "New")}}
		return resp
	})
	active := NewActive(slow)
	m := NewMonitor(active, 0, nil)
	done := make(chan struct{})
	go func() { m.Refresh(context.Background()); close(done) }()

	active.Set(fast)
	if got := m.Refresh(context.Background()).MakeModel; got != "New" {
		t.Fatalf("refresh after switch = %q", got)
	}
	close(release)
	<-done
	if got := m.Status().MakeModel; got != "New" {
		t.Errorf("slow poll of the old printer overwrote status: %q", got)
	}
	if !strings.Contains(m.Status().URI, strings.TrimPrefix(fast.URI(), "ipp://")) {
		t.Errorf("status URI = %q", m.Status().URI)
	}
}

func TestTLSRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Upgrade", "TLS/1.2, HTTP/1.1")
		w.WriteHeader(http.StatusUpgradeRequired)
	}))
	t.Cleanup(srv.Close)
	c, _ := New("ipp://" + strings.TrimPrefix(srv.URL, "http://") + "/ipp/print")
	s := c.Status(context.Background())
	if s.Reachable || !s.NeedsTLS || !strings.Contains(s.Error, "ipps://") {
		t.Errorf("got %+v", s)
	}
}

func TestRasterDPI(t *testing.T) {
	for _, c := range []struct {
		dpis []int
		want int
	}{{[]int{300}, 300}, {[]int{600, 300}, 300}, {[]int{200, 400}, 400}, {[]int{600}, 600}} {
		if got := (Status{RasterDPIs: c.dpis}).RasterDPI(); got != c.want {
			t.Errorf("%v -> %d, want %d", c.dpis, got, c.want)
		}
	}
}

func TestParseTrays(t *testing.T) {
	// What the Canon LBP246dw reports while out of paper.
	trays := parseTrays([]string{
		"type=other;mediafeed=0;mediaxfeed=0;maxcapacity=100;level=0;status=3;name=auto;",
		"type=sheetFeedAutoNonRemovableTray;mediafeed=0;mediaxfeed=0;maxcapacity=100;level=0;status=3;name=Stack Bypass;",
		"type=sheetFeedAutoRemovableTray;mediafeed=27940;mediaxfeed=21590;maxcapacity=250;level=-3;status=0;name=Drawer1;",
	})
	if len(trays) != 2 {
		t.Fatalf("trays = %+v", trays)
	}
	if b := trays[0]; b.Name != "Stack Bypass" || !b.Bypass() || b.Level != 0 || b.Capacity != 100 {
		t.Errorf("bypass = %+v", b)
	}
	if d := trays[1]; d.Name != "Drawer1" || d.Bypass() || d.Level != TrayLevelSome || d.Capacity != 250 ||
		d.Feed != 27940 || d.CrossFeed != 21590 {
		t.Errorf("drawer = %+v", d)
	}
}
