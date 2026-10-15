// Package config loads netpr settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr         string // public app listener, routed by Traefik
	InternalAddr string // /healthz, /readyz, /metrics; never routed by Traefik
	DataDir      string

	PrinterURI     string
	GotenbergURL   string
	TrustedProxies []netip.Prefix
	DevUser        string // bypasses Remote-User check; local development only
	DryRun         bool

	MaxUploadBytes int64
	ConfirmSheets  int
	TonerLowPct    int
	PaperSize      string // letter | a4, used when rendering images to PDF
	StagingTTL     time.Duration
	PrinterPoll    time.Duration

	PushoverToken string
	PushoverUser  string
}

func Load() (*Config, error) {
	c := &Config{
		Addr:          env("NETPR_ADDR", ":8080"),
		InternalAddr:  env("NETPR_INTERNAL_ADDR", ":9090"),
		DataDir:       env("NETPR_DATA_DIR", "/data"),
		PrinterURI:    os.Getenv("NETPR_PRINTER_URI"),
		GotenbergURL:  strings.TrimRight(env("NETPR_GOTENBERG_URL", "http://gotenberg:3000"), "/"),
		DevUser:       os.Getenv("NETPR_DEV_USER"),
		PaperSize:     strings.ToLower(env("NETPR_PAPER_SIZE", "letter")),
		PushoverToken: os.Getenv("PUSHOVER_TOKEN"),
		PushoverUser:  os.Getenv("PUSHOVER_USER"),
	}

	var errs []error
	c.DryRun = envBool("NETPR_DRY_RUN", &errs)
	c.MaxUploadBytes = int64(envInt("NETPR_MAX_UPLOAD_MB", 50, &errs)) << 20
	c.ConfirmSheets = envInt("NETPR_CONFIRM_SHEETS", 30, &errs)
	c.TonerLowPct = envInt("NETPR_TONER_LOW_PCT", 15, &errs)
	c.StagingTTL = envDuration("NETPR_STAGING_TTL", time.Hour, &errs)
	c.PrinterPoll = envDuration("NETPR_PRINTER_POLL", time.Minute, &errs)

	for s := range strings.SplitSeq(env("NETPR_TRUSTED_PROXIES", ""), ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("NETPR_TRUSTED_PROXIES: %w", err))
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, p.Masked())
	}

	if c.PrinterURI == "" {
		errs = append(errs, errors.New("NETPR_PRINTER_URI is required (e.g. ipp://192.168.1.50/ipp/print)"))
	} else if u, err := url.Parse(c.PrinterURI); err != nil || (u.Scheme != "ipp" && u.Scheme != "ipps") || u.Host == "" {
		errs = append(errs, fmt.Errorf("NETPR_PRINTER_URI %q must be an ipp:// or ipps:// URI", c.PrinterURI))
	}
	if len(c.TrustedProxies) == 0 && c.DevUser == "" {
		errs = append(errs, errors.New("set NETPR_TRUSTED_PROXIES (Traefik network CIDR) or NETPR_DEV_USER for local development"))
	}
	if c.PaperSize != "letter" && c.PaperSize != "a4" {
		errs = append(errs, fmt.Errorf("NETPR_PAPER_SIZE must be letter or a4, got %q", c.PaperSize))
	}
	if (c.PushoverToken == "") != (c.PushoverUser == "") {
		errs = append(errs, errors.New("set both PUSHOVER_TOKEN and PUSHOVER_USER, or neither"))
	}
	return c, errors.Join(errs...)
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, errs *[]error) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a non-negative integer", key, v))
		return def
	}
	return n
}

func envBool(key string, errs *[]error) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a boolean", key, v))
	}
	return b
}

func envDuration(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a positive duration", key, v))
		return def
	}
	return d
}
