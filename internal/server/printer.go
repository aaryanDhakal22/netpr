package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/aaryanDhakal22/netpr/internal/jobs"
	"github.com/aaryanDhakal22/netpr/internal/notify"
	"github.com/aaryanDhakal22/netpr/internal/printer"
	"github.com/aaryanDhakal22/netpr/internal/settings"
	"github.com/aaryanDhakal22/netpr/internal/store"
	"github.com/aaryanDhakal22/netpr/internal/testpage"
)

// The printer page: diagnostics, the printer's own queue, test tools, print
// defaults and the printer address.

type printerPage struct {
	pageData
	Diag     diagView
	Defaults defaultsView
	Address  addressView
}

func (s *Server) printerPage(c *echo.Context) error {
	ctx := c.Request().Context()
	d := printerPage{pageData: s.page(c), Diag: s.diag(ctx), Defaults: s.defaultsView(), Address: s.addressView()}
	d.Nav = "printer"
	return c.Render(http.StatusOK, "printer", d)
}

// ---- diagnostics ----

type diagView struct {
	printerView
	URI       string
	Error     string
	NeedsTLS  bool
	CheckedAt time.Time
	Latency   string
	State     string
	Message   string
	Accepting bool
	Reasons   []string // raw printer-state-reasons keywords
	Formats   []string
	SendsAs   string
	Sides     []string
	Gotenberg string // "" when healthy
	Pushover  bool
	DryRun    bool
}

func (s *Server) diag(ctx context.Context) diagView {
	st := s.monitor.Status()
	v := diagView{
		printerView: newPrinterView(st, s.set.Get().TonerLowPct),
		URI:         s.svc.Printer.Get().URI(),
		Error:       st.Error, NeedsTLS: st.NeedsTLS, CheckedAt: st.CheckedAt,
		State: st.State, Message: st.Message, Accepting: st.Accepting,
		Formats: st.Formats, SendsAs: st.SendsAs(), Sides: st.Sides,
		DryRun: s.cfg.DryRun,
	}
	if st.Latency > 0 {
		v.Latency = st.Latency.Round(time.Millisecond).String()
	}
	for _, r := range st.Reasons {
		if r != "none" {
			v.Reasons = append(v.Reasons, r)
		}
	}
	_, noop := s.svc.Notifier.(notify.Noop)
	v.Pushover = !noop
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.svc.Converter.Health(hctx); err != nil {
		v.Gotenberg = err.Error()
	}
	return v
}

func (s *Server) diagSection(c *echo.Context) error {
	return c.Render(http.StatusOK, "diag", s.diag(c.Request().Context()))
}

// checkPrinter polls the printer now instead of waiting for the next poll.
func (s *Server) checkPrinter(c *echo.Context) error {
	s.monitor.Refresh(c.Request().Context())
	return s.diagSection(c)
}

type attributesView struct {
	Attrs []printer.RawAttribute
	Error string
}

func (s *Server) printerAttributes(c *echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 15*time.Second)
	defer cancel()
	attrs, err := s.svc.Printer.Get().Attributes(ctx)
	v := attributesView{Attrs: attrs}
	if err != nil {
		v.Error = err.Error()
	}
	return c.Render(http.StatusOK, "attributes", v)
}

// ---- the printer's queue ----

type queueView struct {
	Jobs  []printer.QueuedJob
	Error string
	Note  string
}

func (s *Server) queue(c *echo.Context) error {
	return c.Render(http.StatusOK, "queue", s.queueView(c.Request().Context(), ""))
}

func (s *Server) queueView(ctx context.Context, note string) queueView {
	if st := s.monitor.Status(); !st.CheckedAt.IsZero() && !st.Reachable {
		// Don't wait out another timeout; Check now re-tests the printer.
		return queueView{Note: note, Error: "The printer is unreachable, so its queue can't be read."}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	js, err := s.svc.Printer.Get().Jobs(ctx)
	v := queueView{Jobs: js, Note: note}
	if err != nil {
		v.Error = "could not read the printer's queue: " + err.Error()
	}
	return v
}

func (s *Server) cancelJob(c *echo.Context) error {
	ctx := c.Request().Context()
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.ErrNotFound
	}
	pc := s.svc.Printer.Get()
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	queued, err := pc.Jobs(lctx)
	if err != nil {
		return c.Render(http.StatusOK, "queue", queueView{Error: "could not read the printer's queue: " + err.Error()})
	}
	// Without printer authentication only the job's owner may cancel it, so
	// ask as the owner the printer reported.
	owner := ""
	for _, j := range queued {
		if j.ID == id {
			owner = j.User
		}
	}
	if owner == "" {
		owner = user(c)
	}
	note := "Canceled job " + strconv.Itoa(id) + "."
	switch err := pc.CancelJob(lctx, id, owner); {
	case errors.Is(err, printer.ErrJobNotFound):
		note = "Job " + strconv.Itoa(id) + " had already finished."
	case err != nil:
		note = "Could not cancel job " + strconv.Itoa(id) + ": " + err.Error()
	default:
		s.log.Info("printer job canceled", "ipp_job", id, "owner", owner, "user", user(c))
	}
	return c.Render(http.StatusOK, "queue", s.queueView(ctx, note))
}

// ---- tools ----

type toolResult struct {
	Tone string // ok, warn, error
	Text string
	Jobs []store.Job
}

func (s *Server) testPage(c *echo.Context) error {
	ctx := c.Request().Context()
	pdf := testpage.PDF(testpage.Info{
		User: user(c), Printer: s.monitor.Status().MakeModel, URI: s.svc.Printer.Get().URI(),
		PaperSize: s.set.Get().PaperSize, Time: time.Now(),
	})
	submitted, err := s.svc.SubmitPDF(ctx, user(c), "NetPr test page", pdf, 1, jobs.Options{Copies: 1})
	if err != nil {
		return c.Render(http.StatusOK, "tool-result", toolResult{Tone: "error", Text: "Could not queue the test page: " + err.Error()})
	}
	return c.Render(http.StatusOK, "tool-result", toolResult{Tone: "ok", Text: "Test page sent.", Jobs: submitted})
}

func (s *Server) testNotify(c *echo.Context) error {
	if _, noop := s.svc.Notifier.(notify.Noop); noop {
		return c.Render(http.StatusOK, "tool-result", toolResult{Tone: "warn",
			Text: "Pushover isn't set up. Add PUSHOVER_TOKEN and PUSHOVER_USER to .env and restart netpr."})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 20*time.Second)
	defer cancel()
	err := s.svc.Notifier.Notify(ctx, "NetPr test", "Notifications from NetPr are working. Sent by "+user(c)+".",
		notify.PriorityNormal)
	if err != nil {
		return c.Render(http.StatusOK, "tool-result", toolResult{Tone: "error", Text: "Pushover failed: " + err.Error()})
	}
	return c.Render(http.StatusOK, "tool-result", toolResult{Tone: "ok", Text: "Test notification sent. Check your phone."})
}

// ---- print defaults ----

type defaultsView struct {
	Values     settings.Values
	Env        settings.Values
	Changed    map[string]bool
	AnyChanged bool
	Error      string
	Saved      string
}

func (s *Server) defaultsView() defaultsView {
	v := defaultsView{Values: s.set.Get(), Env: s.set.Env(), Changed: s.set.Overridden()}
	for _, k := range settings.DefaultKeys {
		v.AnyChanged = v.AnyChanged || v.Changed[k]
	}
	return v
}

func (s *Server) saveDefaults(c *echo.Context) error {
	r := c.Request()
	if err := r.ParseForm(); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	f := r.PostForm
	v := s.set.Get()
	var bad []string
	num := func(field string, dst *int) {
		n, err := strconv.Atoi(strings.TrimSpace(f.Get(field)))
		if err != nil {
			bad = append(bad, field)
			return
		}
		*dst = n
	}
	num("copies", &v.Copies)
	num("confirm_sheets", &v.ConfirmSheets)
	num("toner_low_pct", &v.TonerLowPct)
	v.Duplex = f.Get("duplex") == "on"
	v.PaperSize = f.Get("paper_size")

	view := s.defaultsView()
	view.Values = v
	if len(bad) > 0 {
		view.Error = "Enter whole numbers for copies, the big-job check and the toner alert."
		return c.Render(http.StatusOK, "defaults", view)
	}
	if err := s.set.SaveDefaults(r.Context(), user(c), v); err != nil {
		view.Error = strings.ReplaceAll(err.Error(), "\n", ". ")
		return c.Render(http.StatusOK, "defaults", view)
	}
	s.applyDefaults()
	s.log.Info("print defaults saved", "user", user(c), "copies", v.Copies, "duplex", v.Duplex,
		"paper", v.PaperSize, "confirm_sheets", v.ConfirmSheets, "toner_low_pct", v.TonerLowPct)
	view = s.defaultsView()
	view.Saved = "Saved."
	return c.Render(http.StatusOK, "defaults", view)
}

func (s *Server) resetDefaults(c *echo.Context) error {
	if err := s.set.Reset(c.Request().Context(), user(c), settings.DefaultKeys...); err != nil {
		return err
	}
	s.applyDefaults()
	s.log.Info("print defaults reset to environment", "user", user(c))
	view := s.defaultsView()
	view.Saved = "Back to the values from .env."
	return c.Render(http.StatusOK, "defaults", view)
}

// applyDefaults pushes settings that other components cache.
func (s *Server) applyDefaults() {
	s.svc.Converter.SetPaperSize(s.set.Get().PaperSize)
}

// ---- printer address ----

type addressView struct {
	URI     string
	EnvURI  string
	Changed bool
	Input   string
	Error   string
	Saved   string
}

func (s *Server) addressView() addressView {
	return addressView{
		URI: s.set.Get().PrinterURI, EnvURI: s.set.Env().PrinterURI,
		Changed: s.set.Overridden()[settings.KeyPrinterURI],
	}
}

type probeView struct {
	URI     string
	Status  printer.Status
	Latency string
	Current bool
	Error   string
}

// probe checks a printer address with a read-only Get-Printer-Attributes.
// A printer that refuses plain ipp:// is retried as ipps:// on port 443,
// where printers that want encryption usually serve IPP.
func probe(ctx context.Context, input string) (probeView, *printer.Client) {
	uri, err := printer.ParseAddress(input)
	if err != nil {
		return probeView{Error: err.Error()}, nil
	}
	pc, err := printer.New(uri)
	if err != nil {
		return probeView{URI: uri, Error: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st := pc.Status(ctx)
	if u, _ := url.Parse(uri); st.NeedsTLS && u.Scheme == "ipp" {
		u.Scheme, u.Host = "ipps", net.JoinHostPort(u.Hostname(), "443")
		if tls, err := printer.New(u.String()); err == nil {
			if tst := tls.Status(ctx); tst.Reachable {
				uri, pc, st = u.String(), tls, tst
			}
		}
	}
	v := probeView{URI: uri, Status: st, Latency: st.Latency.Round(time.Millisecond).String()}
	if !st.Reachable {
		v.Error = "No IPP printer answered at " + uri + ": " + st.Error
		return v, nil
	}
	return v, pc
}

func (s *Server) testAddress(c *echo.Context) error {
	v, _ := probe(c.Request().Context(), c.FormValue("address"))
	v.Current = v.URI == s.set.Get().PrinterURI
	return c.Render(http.StatusOK, "probe", v)
}

func (s *Server) saveAddress(c *echo.Context) error {
	ctx := c.Request().Context()
	view := s.addressView()
	p, pc := probe(ctx, c.FormValue("uri"))
	if pc == nil {
		view.Input, view.Error = c.FormValue("uri"), p.Error
		return c.Render(http.StatusOK, "address", view)
	}
	if err := s.set.SetPrinterURI(ctx, user(c), p.URI); err != nil {
		return err
	}
	s.switchPrinter(ctx, pc)
	s.log.Info("printer address changed", "user", user(c), "uri", p.URI, "model", p.Status.MakeModel)
	view = s.addressView()
	view.Saved = "Now printing to " + p.URI + "."
	c.Response().Header().Set("HX-Trigger", "printer-changed")
	return c.Render(http.StatusOK, "address", view)
}

func (s *Server) resetAddress(c *echo.Context) error {
	ctx := c.Request().Context()
	if err := s.set.Reset(ctx, user(c), settings.KeyPrinterURI); err != nil {
		return err
	}
	pc, err := printer.New(s.set.Get().PrinterURI)
	if err != nil {
		return err
	}
	s.switchPrinter(ctx, pc)
	s.log.Info("printer address reset to environment", "user", user(c), "uri", pc.URI())
	view := s.addressView()
	view.Saved = "Back to the address from .env."
	c.Response().Header().Set("HX-Trigger", "printer-changed")
	return c.Render(http.StatusOK, "address", view)
}

// switchPrinter makes pc the printer for new jobs and refreshes its status.
// Jobs already sent keep being followed on the printer they went to.
func (s *Server) switchPrinter(ctx context.Context, pc *printer.Client) {
	s.svc.Printer.Set(pc)
	s.monitor.Refresh(ctx)
}
