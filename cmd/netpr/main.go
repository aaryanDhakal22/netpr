// Command netpr serves the print web UI and sends jobs to an IPP printer.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/aaryanDhakal22/netpr/internal/config"
	"github.com/aaryanDhakal22/netpr/internal/convert"
	"github.com/aaryanDhakal22/netpr/internal/jobs"
	"github.com/aaryanDhakal22/netpr/internal/metrics"
	"github.com/aaryanDhakal22/netpr/internal/notify"
	"github.com/aaryanDhakal22/netpr/internal/printer"
	"github.com/aaryanDhakal22/netpr/internal/server"
	"github.com/aaryanDhakal22/netpr/internal/settings"
	"github.com/aaryanDhakal22/netpr/internal/staging"
	"github.com/aaryanDhakal22/netpr/internal/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config:\n%w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "netpr.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	stg, err := staging.New(filepath.Join(cfg.DataDir, "staging"), cfg.StagingTTL, log)
	if err != nil {
		return err
	}
	// The printer page can override these; saved values win over the environment.
	set, ignored, err := settings.Load(ctx, st, settings.Values{
		PrinterURI: cfg.PrinterURI, Copies: 1, Duplex: true, PaperSize: cfg.PaperSize,
		ConfirmSheets: cfg.ConfirmSheets, TonerLowPct: cfg.TonerLowPct,
	})
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	for _, err := range ignored {
		log.Warn("ignoring saved setting", "err", err)
	}
	current := set.Get()
	pc, err := printer.New(current.PrinterURI)
	if err != nil {
		return err
	}
	active := printer.NewActive(pc)
	conv := convert.NewGotenberg(cfg.GotenbergURL, current.PaperSize)
	met := metrics.New()
	notifier := notify.New(cfg.PushoverToken, cfg.PushoverUser)

	var alertMu sync.Mutex
	alerter := &printer.Alerter{}
	monitor := printer.NewMonitor(active, cfg.PrinterPoll, func(s printer.Status) {
		levels, trays := map[string]int{}, map[string]int{}
		for _, m := range s.Markers {
			levels[m.Name] = m.Level
		}
		for _, t := range s.Trays {
			trays[t.Name] = t.Level
		}
		met.Printer(s.Reachable, levels, trays)

		alertMu.Lock()
		alerter.TonerLowPct = set.Get().TonerLowPct
		alerts := alerter.Observe(s)
		alertMu.Unlock()
		for _, a := range alerts {
			log.Warn("printer alert", "title", a.Title, "message", a.Message)
			go func() {
				nctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if err := notifier.Notify(nctx, a.Title, a.Message, notify.PriorityNormal); err != nil {
					log.Warn("notify", "err", err)
				}
			}()
		}
	})

	svc := &jobs.Service{
		Store: st, Staging: stg, Converter: conv, Printer: active, Monitor: monitor,
		Notifier: notifier, Metrics: met, Log: log,
		MaxUploadBytes: cfg.MaxUploadBytes, DryRun: cfg.DryRun,
	}
	svc.Init()

	app, err := server.New(cfg, set, svc, log)
	if err != nil {
		return err
	}

	go func() {
		s := monitor.Refresh(ctx)
		logCapabilities(log, s)
		monitor.Run(ctx)
	}()
	go stg.Run(ctx)
	go svc.Run(ctx)

	internal := &http.Server{
		Addr:              cfg.InternalAddr,
		Handler:           internalMux(st, conv, monitor, met),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := internal.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("internal listener", "err", err)
			stop()
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		internal.Shutdown(sctx)
	}()

	if current.PrinterURI != cfg.PrinterURI {
		log.Info("using printer address set on the printer page instead of NETPR_PRINTER_URI",
			"uri", current.PrinterURI, "env", cfg.PrinterURI)
	}
	log.Info("netpr starting", "addr", cfg.Addr, "internal", cfg.InternalAddr, "printer", current.PrinterURI,
		"gotenberg", cfg.GotenbergURL, "dry_run", cfg.DryRun, "dev_user", cfg.DevUser != "",
		"pushover", cfg.PushoverToken != "")
	sc := echo.StartConfig{
		Address:         cfg.Addr,
		HideBanner:      true,
		HidePort:        true,
		GracefulTimeout: 15 * time.Second,
		BeforeServeFunc: func(s *http.Server) error {
			s.ReadHeaderTimeout = 10 * time.Second
			s.IdleTimeout = 2 * time.Minute
			return nil
		},
	}
	if err := sc.Start(ctx, app); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("netpr stopped")
	return nil
}

func logCapabilities(log *slog.Logger, s printer.Status) {
	if !s.Reachable {
		log.Warn("printer not reachable at startup; will keep retrying", "uri", s.URI, "err", s.Error)
		return
	}
	log.Info("printer found", "model", s.MakeModel, "state", s.State, "sends_as", s.SendsAs(),
		"formats", s.Formats, "sides", s.Sides)
	if s.SendsAs() == "" {
		log.Error("printer accepts neither PDF nor grayscale PWG Raster; jobs will be rejected", "formats", s.Formats)
	}
}

func internalMux(st *store.Store, conv *convert.Gotenberg, monitor *printer.Monitor, met *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	// readyz reports dependencies; it is for humans and dashboards, not the
	// container healthcheck, since a powered-off printer is not a netpr fault.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		checks := map[string]string{"database": "ok", "gotenberg": "ok", "printer": "ok"}
		ok := true
		if err := st.Ping(ctx); err != nil {
			checks["database"], ok = err.Error(), false
		}
		if err := conv.Health(ctx); err != nil {
			checks["gotenberg"], ok = err.Error(), false
		}
		if s := monitor.Status(); !s.Reachable {
			checks["printer"], ok = "unreachable: "+s.Error, false
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(checks)
	})
	mux.Handle("GET /metrics", met)
	return mux
}

// healthcheck is the container HEALTHCHECK, so the image needs no curl.
func healthcheck() int {
	addr := os.Getenv("NETPR_INTERNAL_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthz:", resp.Status)
		return 1
	}
	return 0
}
