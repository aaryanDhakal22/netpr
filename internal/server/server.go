// Package server is the web UI.
package server

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/aaryanDhakal22/netpr/internal/config"
	"github.com/aaryanDhakal22/netpr/internal/jobs"
	"github.com/aaryanDhakal22/netpr/internal/printer"
	"github.com/aaryanDhakal22/netpr/internal/settings"
	"github.com/aaryanDhakal22/netpr/internal/staging"
	"github.com/aaryanDhakal22/netpr/internal/store"
	"github.com/aaryanDhakal22/netpr/web"
)

type Server struct {
	cfg     *config.Config
	set     *settings.Settings
	svc     *jobs.Service
	monitor *printer.Monitor
	store   *store.Store
	log     *slog.Logger
	tmpl    *renderer
}

func New(cfg *config.Config, set *settings.Settings, svc *jobs.Service, log *slog.Logger) (*echo.Echo, error) {
	tmpl, err := newRenderer()
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, set: set, svc: svc, monitor: svc.Monitor, store: svc.Store, log: log, tmpl: tmpl}

	e := echo.NewWithConfig(echo.Config{Logger: log})
	e.Renderer = tmpl
	e.HTTPErrorHandler = s.errorHandler
	e.Use(middleware.Recover())
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true, LogURI: true, LogMethod: true, LogLatency: true, HandleError: true,
		Skipper: func(c *echo.Context) bool {
			p := c.Request().URL.Path
			// Polling endpoints would drown out everything else.
			return strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/jobs/") ||
				(c.Request().Method == http.MethodGet && strings.HasPrefix(p, "/printer/"))
		},
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			log.Info("request", "method", v.Method, "uri", v.URI, "status", v.Status,
				"latency_ms", v.Latency.Milliseconds(), "user", user(c), "err", v.Error)
			return nil
		},
	}))
	e.Use(middleware.SecureWithConfig(middleware.SecureConfig{
		XSSProtection: "0", ContentTypeNosniff: "nosniff", XFrameOptions: "DENY",
		ReferrerPolicy: "same-origin",
		ContentSecurityPolicy: "default-src 'self'; img-src 'self' blob: data:; style-src 'self' 'unsafe-inline'; " +
			"script-src 'self'; worker-src 'self' blob:; frame-ancestors 'none'",
	}))
	e.Use(s.requireUser)
	e.Use(middleware.CSRFWithConfig(middleware.CSRFConfig{
		TokenLookup:    "header:X-CSRF-Token,form:_csrf",
		CookieSecure:   true,
		CookieHTTPOnly: true,
		CookieSameSite: http.SameSiteStrictMode,
		CookiePath:     "/",
	}))

	static := echo.MustSubFS(web.Static, "static")
	e.GET("/static/*", echo.StaticDirectoryHandler(static, false), cacheStatic)
	e.GET("/sw.js", s.serviceWorker)
	e.GET("/manifest.webmanifest", s.manifest)

	e.GET("/", s.index)
	e.POST("/upload", s.upload)
	e.POST("/share", s.share)
	e.GET("/staged/:id/pdf", s.stagedPDF)
	e.DELETE("/staged/:id", s.removeStaged)
	e.POST("/print", s.print)
	e.GET("/jobs", s.history)
	e.GET("/jobs/:id", s.job)
	e.GET("/printer/status", s.printerStatus)

	e.GET("/printer", s.printerPage)
	e.GET("/printer/diag", s.diagSection)
	e.POST("/printer/check", s.checkPrinter)
	e.GET("/printer/attributes", s.printerAttributes)
	e.GET("/printer/queue", s.queue)
	e.POST("/printer/queue/:id/cancel", s.cancelJob)
	e.POST("/printer/test-page", s.testPage)
	e.POST("/printer/test-notify", s.testNotify)
	e.POST("/printer/defaults", s.saveDefaults)
	e.POST("/printer/defaults/reset", s.resetDefaults)
	e.POST("/printer/address/test", s.testAddress)
	e.POST("/printer/address", s.saveAddress)
	e.POST("/printer/address/reset", s.resetAddress)
	return e, nil
}

const userKey = "user"

// requireUser trusts Remote-User only from the reverse proxy, which sets it
// after Authelia authenticates the request (and overwrites any client copy).
func (s *Server) requireUser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if u := strings.TrimSpace(c.Request().Header.Get("Remote-User")); u != "" && s.fromTrustedProxy(c.Request()) {
			c.Set(userKey, u)
			return next(c)
		}
		if s.cfg.DevUser != "" {
			c.Set(userKey, s.cfg.DevUser)
			return next(c)
		}
		return echo.NewHTTPError(http.StatusForbidden, "not authenticated: requests must come through Authelia")
	}
}

func (s *Server) fromTrustedProxy(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	addr := ap.Addr().Unmap()
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func user(c *echo.Context) string {
	u, _ := c.Get(userKey).(string)
	return u
}

func csrfToken(c *echo.Context) string {
	t, _ := c.Get("csrf").(string)
	return t
}

// cacheStatic lets browsers keep fingerprinted URLs (?v=<hash>, see asset)
// forever; anything else is rechecked after an hour.
func cacheStatic(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		cc := "public, max-age=3600"
		if c.QueryParam("v") != "" {
			cc = "public, max-age=31536000, immutable"
		}
		c.Response().Header().Set("Cache-Control", cc)
		return next(c)
	}
}

func (s *Server) errorHandler(c *echo.Context, err error) {
	if r, _ := echo.UnwrapResponse(c.Response()); r != nil && r.Committed {
		return
	}
	code := echo.StatusCode(err)
	if code == 0 {
		code = http.StatusInternalServerError
	}
	msg := http.StatusText(code)
	var he *echo.HTTPError
	if errors.As(err, &he) && code < 500 {
		msg = he.Message
	}
	if code >= 500 {
		s.log.Error("handler error", "uri", c.Request().RequestURI, "err", err)
	}
	if isHTMX(c) {
		// app.js configures htmx to swap error responses; show them in place.
		c.Response().Header().Set("HX-Reswap", "innerHTML")
		c.Response().Header().Set("HX-Retarget", "#result")
		c.Render(code, "error", msg)
		return
	}
	c.String(code, msg)
}

func isHTMX(c *echo.Context) bool { return c.Request().Header.Get("HX-Request") == "true" }

// ---- pages ----

type pageData struct {
	User        string
	CSRF        string
	Nav         string // current section, for the header
	Printer     printerView
	Settings    settings.Values
	Jobs        []store.Job
	Staged      []stagedView
	MaxUploadMB int64
	DryRun      bool
	NextBefore  int64
	Accept      string
}

func (s *Server) page(c *echo.Context) pageData {
	v := s.set.Get()
	return pageData{
		User: user(c), CSRF: csrfToken(c), Printer: newPrinterView(s.monitor.Status(), v.TonerLowPct),
		Settings: v, MaxUploadMB: s.cfg.MaxUploadBytes >> 20, DryRun: s.cfg.DryRun, Accept: acceptTypes,
	}
}

const acceptTypes = "application/pdf,image/jpeg,image/png,text/plain,.txt,.doc,.docx,.xls,.xlsx,.ppt,.pptx,.odt,.ods,.odp"

func (s *Server) index(c *echo.Context) error {
	d := s.page(c)
	d.Nav = "print"
	var err error
	if d.Jobs, err = s.store.List(c.Request().Context(), 5, 0); err != nil {
		return err
	}
	return c.Render(http.StatusOK, "index", d)
}

func (s *Server) history(c *echo.Context) error {
	const pageSize = 50
	before, _ := strconv.ParseInt(c.QueryParam("before"), 10, 64)
	d := s.page(c)
	d.Nav = "history"
	var err error
	if d.Jobs, err = s.store.List(c.Request().Context(), pageSize, before); err != nil {
		return err
	}
	if len(d.Jobs) == pageSize {
		d.NextBefore = d.Jobs[len(d.Jobs)-1].ID
	}
	return c.Render(http.StatusOK, "history", d)
}

func (s *Server) job(c *echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.ErrNotFound
	}
	j, err := s.store.Get(c.Request().Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	// Only unfinished jobs poll, so a finished one here has just finished.
	if j.Terminal() && isHTMX(c) {
		c.Response().Header().Set("HX-Trigger", jobFinishedEvent(j))
	}
	return c.Render(http.StatusOK, "job", j)
}

// printerStatus serves the header pill, or the detailed card with ?view=card.
func (s *Server) printerStatus(c *echo.Context) error {
	name := "printer-pill"
	if c.QueryParam("view") == "card" {
		name = "printer-card"
	}
	return c.Render(http.StatusOK, name, newPrinterView(s.monitor.Status(), s.set.Get().TonerLowPct))
}

// ---- uploads ----

type stagedView struct {
	ID       string
	Filename string
	Kind     string
	Pages    int
	Error    string
}

// stageAll streams every file part of a multipart request into staging,
// so large uploads never sit in memory or in a second temp copy.
func (s *Server) stageAll(c *echo.Context) ([]stagedView, error) {
	r := c.Request()
	r.Body = http.MaxBytesReader(c.Response(), r.Body, s.cfg.MaxUploadBytes*20+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "expected a file upload")
	}
	var out []stagedView
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, echo.NewHTTPError(http.StatusBadRequest, "upload interrupted or too large")
		}
		if part.FormName() != "files" || part.FileName() == "" {
			part.Close()
			continue
		}
		name := part.FileName()
		it, err := s.svc.Stage(r.Context(), name, part)
		part.Close()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return out, err
			}
			out = append(out, stagedView{Filename: staging.CleanName(name), Error: err.Error()})
			continue
		}
		out = append(out, stagedView{ID: it.ID, Filename: it.Filename, Kind: it.Kind, Pages: it.Pages})
	}
	if len(out) == 0 {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "no files received")
	}
	return out, nil
}

func (s *Server) upload(c *echo.Context) error {
	staged, err := s.stageAll(c)
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "staged", staged)
}

// share is the no-service-worker fallback for the PWA share target.
func (s *Server) share(c *echo.Context) error {
	staged, err := s.stageAll(c)
	if err != nil {
		return err
	}
	d := s.page(c)
	d.Staged = staged
	if d.Jobs, err = s.store.List(c.Request().Context(), 5, 0); err != nil {
		return err
	}
	return c.Render(http.StatusOK, "index", d)
}

func (s *Server) stagedPDF(c *echo.Context) error {
	it, ok := s.svc.Staging.Get(c.Param("id"))
	if !ok {
		return echo.ErrNotFound
	}
	f, err := os.Open(it.PDFPath())
	if err != nil {
		return echo.ErrNotFound
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := c.Response().Header()
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Type", "application/pdf")
	// ServeContent handles Range requests, which pdf.js uses for big files.
	http.ServeContent(c.Response(), c.Request(), "document.pdf", st.ModTime(), f)
	return nil
}

func (s *Server) removeStaged(c *echo.Context) error {
	s.svc.Staging.Remove(c.Param("id"))
	return c.NoContent(http.StatusOK)
}

// ---- printing ----

type confirmView struct {
	Sheets    int
	Pages     int
	Threshold int
}

func (s *Server) print(c *echo.Context) error {
	r := c.Request()
	if err := r.ParseForm(); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	copies, err := strconv.Atoi(strings.TrimSpace(r.PostForm.Get("copies")))
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "copies must be a number")
	}
	opts := jobs.Options{
		Copies:    copies,
		Duplex:    r.PostForm.Get("duplex") == "on",
		PageRange: r.PostForm.Get("range"),
	}
	plan, err := s.svc.Plan(r.PostForm["id"], opts)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}
	if limit := s.set.Get().ConfirmSheets; limit > 0 && plan.Sheets > limit && r.PostForm.Get("confirm") != "1" {
		return c.Render(http.StatusOK, "confirm", confirmView{Sheets: plan.Sheets, Pages: plan.Pages, Threshold: limit})
	}
	submitted, err := s.svc.Submit(r.Context(), user(c), plan, opts)
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return c.Render(http.StatusOK, "submitted", submitted)
}

// ---- PWA ----

func (s *Server) serviceWorker(c *echo.Context) error {
	b, err := fs.ReadFile(web.Static, "static/js/sw.js")
	if err != nil {
		return err
	}
	h := c.Response().Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("Service-Worker-Allowed", "/")
	return c.Blob(http.StatusOK, "text/javascript; charset=utf-8", b)
}

func (s *Server) manifest(c *echo.Context) error {
	b, err := fs.ReadFile(web.Static, "static/manifest.webmanifest")
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "no-cache")
	return c.Blob(http.StatusOK, "application/manifest+json", b)
}

func init() {
	// Make sure ES modules are served with a JavaScript type everywhere.
	mime.AddExtensionType(".mjs", "text/javascript")
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
}
