package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	ipp "github.com/phin1x/go-ipp"

	"github.com/aaryanDhakal22/netpr/internal/config"
	"github.com/aaryanDhakal22/netpr/internal/convert"
	"github.com/aaryanDhakal22/netpr/internal/jobs"
	"github.com/aaryanDhakal22/netpr/internal/metrics"
	"github.com/aaryanDhakal22/netpr/internal/notify"
	"github.com/aaryanDhakal22/netpr/internal/printer"
	"github.com/aaryanDhakal22/netpr/internal/settings"
	"github.com/aaryanDhakal22/netpr/internal/staging"
	"github.com/aaryanDhakal22/netpr/internal/store"
	"github.com/aaryanDhakal22/netpr/internal/testpage"
)

// fakeGotenberg returns canned PDFs and reports pages page count.
func fakeGotenberg(t *testing.T, pages int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/forms/pdfengines/metadata/read":
			w.Write([]byte(`{"document.pdf":{"PageCount":` + strconv.Itoa(pages) + `}}`))
		case "/forms/libreoffice/convert", "/forms/chromium/convert/html":
			w.Write([]byte("%PDF-1.7 converted"))
		case "/forms/pdfengines/split":
			w.Write([]byte("%PDF-1.7 split"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type testApp struct {
	e     *echo.Echo
	store *store.Store
	set   *settings.Settings
	svc   *jobs.Service
}

func newTestApp(t *testing.T, pages int, mutate func(*config.Config)) *testApp {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()

	cfg := &config.Config{
		PrinterURI:     "ipp://127.0.0.1:1/ipp/print", // unreachable; dry run never sends
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		DryRun:         true,
		MaxUploadBytes: 1 << 20,
		ConfirmSheets:  30,
		TonerLowPct:    15,
	}
	if mutate != nil {
		mutate(cfg)
	}
	st, err := store.Open(filepath.Join(dir, "netpr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	stg, err := staging.New(filepath.Join(dir, "staging"), time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	set, _, err := settings.Load(ctx, st, settings.Values{
		PrinterURI: cfg.PrinterURI, Copies: 1, Duplex: true, PaperSize: "letter",
		ConfirmSheets: cfg.ConfirmSheets, TonerLowPct: cfg.TonerLowPct,
	})
	if err != nil {
		t.Fatal(err)
	}
	pc, _ := printer.New(cfg.PrinterURI)
	active := printer.NewActive(pc)
	monitor := printer.NewMonitor(active, time.Hour, nil)
	svc := &jobs.Service{
		Store: st, Staging: stg, Converter: convert.NewGotenberg(fakeGotenberg(t, pages).URL, "letter"),
		Printer: active, Monitor: monitor, Notifier: notify.Noop{}, Metrics: metrics.New(), Log: log,
		MaxUploadBytes: cfg.MaxUploadBytes, DryRun: cfg.DryRun,
		PollInterval: 10 * time.Millisecond,
	}
	svc.Init()
	go svc.Run(ctx)

	e, err := New(cfg, set, svc, log)
	if err != nil {
		t.Fatal(err)
	}
	return &testApp{e: e, store: st, set: set, svc: svc}
}

// do sends a request as if it came through Traefik with Authelia's header.
func (a *testApp) do(req *http.Request) *httptest.ResponseRecorder {
	if req.Header.Get("Remote-User") == "" {
		req.Header.Set("Remote-User", "aaryan")
	}
	if req.Header.Get("Sec-Fetch-Site") == "" {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	a.e.ServeHTTP(rec, req)
	return rec
}

func uploadReq(t *testing.T, files map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, content := range files {
		w, _ := mw.CreateFormFile("files", name)
		w.Write([]byte(content))
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func printReq(form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/print", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

var idRe = regexp.MustCompile(`name="id" value="([0-9a-f]+)"`)

func TestAuth(t *testing.T) {
	a := newTestApp(t, 1, nil)

	trusted := httptest.NewRequest(http.MethodGet, "/", nil)
	trusted.RemoteAddr = "192.0.2.10:5555"
	if rec := a.do(trusted); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Signed in as aaryan") {
		t.Errorf("trusted proxy: %d", rec.Code)
	}

	// Someone reaching netpr directly can't claim to be a user.
	spoofed := httptest.NewRequest(http.MethodGet, "/", nil)
	spoofed.RemoteAddr = "203.0.113.9:5555"
	if rec := a.do(spoofed); rec.Code != http.StatusForbidden {
		t.Errorf("untrusted peer with Remote-User: %d, want 403", rec.Code)
	}

	noHeader := httptest.NewRequest(http.MethodGet, "/", nil)
	noHeader.RemoteAddr = "192.0.2.10:5555"
	noHeader.Header.Set("Remote-User", " ")
	if rec := a.do(noHeader); rec.Code != http.StatusForbidden {
		t.Errorf("trusted proxy without user: %d, want 403", rec.Code)
	}

	dev := newTestApp(t, 1, func(c *config.Config) { c.TrustedProxies = nil; c.DevUser = "dev" })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if rec := dev.do(req); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Signed in as dev") {
		t.Errorf("dev user: %d", rec.Code)
	}
}

func TestCrossSitePostBlocked(t *testing.T) {
	a := newTestApp(t, 1, nil)
	req := printReq(url.Values{"copies": {"1"}})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := a.do(req); rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "cross-site") {
		t.Errorf("cross-site POST was not blocked: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUploadAndDryRunPrint(t *testing.T) {
	a := newTestApp(t, 4, nil)

	rec := a.do(uploadReq(t, map[string]string{"notes.pdf": "%PDF-1.7 hello", "virus.exe": "MZ\x90\x00\x03"}))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, body)
	}
	if !strings.Contains(body, "notes.pdf") || !strings.Contains(body, "4 pages") {
		t.Errorf("staged card missing: %s", body)
	}
	if !strings.Contains(body, "unsupported file type") {
		t.Errorf("exe should be rejected per file: %s", body)
	}
	m := idRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no staged id")
	}
	id := m[1]

	// Preview is served while staged.
	if rec := a.do(httptest.NewRequest(http.MethodGet, "/staged/"+id+"/pdf", nil)); rec.Code != http.StatusOK ||
		rec.Header().Get("Content-Type") != "application/pdf" {
		t.Errorf("preview: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	// Bad range is reported, not printed.
	rec = a.do(printReq(url.Values{"id": {id}, "copies": {"1"}, "range": {"9"}}))
	if !strings.Contains(rec.Body.String(), "outside 1-4") {
		t.Errorf("bad range: %s", rec.Body.String())
	}

	// 4 pages x 20 copies single-sided = 80 sheets: needs confirmation.
	form := url.Values{"id": {id}, "copies": {"20"}, "range": {"1-3,4"}}
	rec = a.do(printReq(form))
	if !strings.Contains(rec.Body.String(), "80 sheets") {
		t.Fatalf("expected confirmation: %s", rec.Body.String())
	}
	if list, _ := a.store.List(context.Background(), 10, 0); len(list) != 0 {
		t.Fatal("job created before confirmation")
	}

	form.Set("confirm", "1")
	form.Set("duplex", "on")
	rec = a.do(printReq(form))
	if !strings.Contains(rec.Body.String(), "Sent 1 job") {
		t.Fatalf("submit: %s", rec.Body.String())
	}

	// Submitting the same form again must not print twice.
	rec = a.do(printReq(form))
	if !strings.Contains(rec.Body.String(), "expired or was already printed") {
		t.Errorf("double submit: %s", rec.Body.String())
	}

	var job store.Job
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := a.store.List(context.Background(), 10, 0)
		if len(list) == 1 && list[0].Terminal() {
			job = list[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != store.StateDryRun {
		t.Fatalf("job state = %q", job.State)
	}
	if job.User != "aaryan" || job.Copies != 20 || !job.Duplex || job.Pages != 4 || job.Sheets != 40 || job.PageRange != "" {
		t.Errorf("job = %+v", job)
	}
	if rec := a.do(httptest.NewRequest(http.MethodGet, "/staged/"+id+"/pdf", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("staged file still served after printing: %d", rec.Code)
	}

	rec = a.do(httptest.NewRequest(http.MethodGet, "/jobs", nil))
	if !strings.Contains(rec.Body.String(), "notes.pdf") || !strings.Contains(rec.Body.String(), "Dry run") {
		t.Errorf("history page missing job")
	}
}

func TestRangeOnlyForSingleFile(t *testing.T) {
	a := newTestApp(t, 2, nil)
	body := a.do(uploadReq(t, map[string]string{"a.pdf": "%PDF-1.7 a", "b.pdf": "%PDF-1.7 b"})).Body.String()
	ids := idRe.FindAllStringSubmatch(body, -1)
	if len(ids) != 2 {
		t.Fatalf("staged %d files", len(ids))
	}
	rec := a.do(printReq(url.Values{"id": {ids[0][1], ids[1][1]}, "copies": {"1"}, "range": {"1"}}))
	if !strings.Contains(rec.Body.String(), "single file") {
		t.Errorf("range with two files: %s", rec.Body.String())
	}
}

func TestStaticAndPWA(t *testing.T) {
	a := newTestApp(t, 1, nil)
	for path, ctype := range map[string]string{
		"/static/css/app.css":              "text/css",
		"/static/vendor/pdfjs/pdf.min.mjs": "javascript",
		"/static/img/icon-192.png":         "image/png",
		"/sw.js":                           "javascript",
		"/manifest.webmanifest":            "application/manifest+json",
	} {
		rec := a.do(httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := a.do(httptest.NewRequest(http.MethodGet, "/sw.js", nil)); rec.Header().Get("Service-Worker-Allowed") != "/" {
		t.Error("sw.js missing Service-Worker-Allowed")
	}
}

// fakePrinter answers every IPP request with the same printer attributes,
// which is all the printer page needs. go-ipp's encoder doesn't know these
// attribute names, so the response is written by hand per RFC 8010.
func fakePrinter(t *testing.T, model string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b bytes.Buffer
		b.Write([]byte{2, 0, 0, 0}) // version 2.0, successful-ok
		b.Write(body[4:8])          // request id
		put := func(tag int8, name string, value []byte) {
			b.WriteByte(byte(tag))
			binary.Write(&b, binary.BigEndian, uint16(len(name)))
			b.WriteString(name)
			binary.Write(&b, binary.BigEndian, uint16(len(value)))
			b.Write(value)
		}
		b.WriteByte(byte(ipp.TagDelimiterOperation))
		put(ipp.TagCharset, "attributes-charset", []byte("utf-8"))
		put(ipp.TagLanguage, "attributes-natural-language", []byte("en"))
		b.WriteByte(byte(ipp.TagDelimiterPrinter))
		put(ipp.TagEnum, "printer-state", []byte{0, 0, 0, byte(ipp.PrinterStateIdle)})
		put(ipp.TagText, "printer-make-and-model", []byte(model))
		put(ipp.TagMimeType, "document-format-supported", []byte("application/pdf"))
		put(ipp.TagKeyword, "sides-supported", []byte("two-sided-long-edge"))
		b.WriteByte(byte(ipp.TagDelimiterEnd))
		w.Header().Set("Content-Type", ipp.ContentTypeIPP)
		w.Write(b.Bytes())
	}))
	t.Cleanup(srv.Close)
	return "ipp://" + strings.TrimPrefix(srv.URL, "http://") + "/ipp/print"
}

func formReq(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestPrinterPage(t *testing.T) {
	a := newTestApp(t, 1, nil)
	rec := a.do(httptest.NewRequest(http.MethodGet, "/printer", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("printer page: %d %s", rec.Code, body)
	}
	for _, want := range []string{"Printer queue", "Print test page", "Print defaults", "Printer address",
		"ipp://127.0.0.1:1/ipp/print", `aria-current="page">Printer`} {
		if !strings.Contains(body, want) {
			t.Errorf("printer page missing %q", want)
		}
	}
	for _, path := range []string{"/printer/status", "/printer/status?view=card", "/printer/diag", "/printer/queue", "/printer/attributes"} {
		if rec := a.do(httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	// The unreachable test printer's queue shows an error, not a crash.
	if body := a.do(httptest.NewRequest(http.MethodGet, "/printer/queue", nil)).Body.String(); !strings.Contains(body, "could not read") {
		t.Errorf("queue error: %s", body)
	}
	// Once the monitor knows it's unreachable, the queue says so without asking.
	a.svc.Monitor.Refresh(context.Background())
	if body := a.do(httptest.NewRequest(http.MethodGet, "/printer/queue", nil)).Body.String(); !strings.Contains(body, "unreachable") {
		t.Errorf("queue while offline: %s", body)
	}
}

func TestDefaultsApplyToPrintForm(t *testing.T) {
	a := newTestApp(t, 4, nil)
	rec := a.do(formReq("/printer/defaults", url.Values{
		"copies": {"2"}, "paper_size": {"a4"}, "confirm_sheets": {"5"}, "toner_low_pct": {"20"},
	}))
	if !strings.Contains(rec.Body.String(), "Saved.") {
		t.Fatalf("save: %s", rec.Body.String())
	}
	index := a.do(httptest.NewRequest(http.MethodGet, "/", nil)).Body.String()
	if !strings.Contains(index, `name="copies" min="1" max="99" value="2"`) {
		t.Error("print form ignores default copies")
	}
	if strings.Contains(index, `name="duplex" checked`) {
		t.Error("print form ignores single-sided default")
	}

	// The new 5-sheet limit applies: 4 pages x 2 copies = 8 sheets.
	id := idRe.FindStringSubmatch(a.do(uploadReq(t, map[string]string{"a.pdf": "%PDF-1.7 a"})).Body.String())[1]
	if body := a.do(printReq(url.Values{"id": {id}, "copies": {"2"}})).Body.String(); !strings.Contains(body, "more than the 5-sheet check") {
		t.Errorf("confirm threshold not applied: %s", body)
	}

	if body := a.do(formReq("/printer/defaults", url.Values{
		"copies": {"0"}, "paper_size": {"a4"}, "confirm_sheets": {"5"}, "toner_low_pct": {"20"},
	})).Body.String(); !strings.Contains(body, "between 1 and 99") {
		t.Errorf("invalid copies accepted: %s", body)
	}

	if body := a.do(formReq("/printer/defaults/reset", nil)).Body.String(); !strings.Contains(body, `value="1"`) {
		t.Errorf("reset: %s", body)
	}
}

func TestChangePrinterAddress(t *testing.T) {
	a := newTestApp(t, 1, nil)
	uri := fakePrinter(t, "Canon LBP246")
	host := strings.TrimSuffix(strings.TrimPrefix(uri, "ipp://"), "/ipp/print")

	// Testing a bare host:port reports the printer without switching to it.
	body := a.do(formReq("/printer/address/test", url.Values{"address": {host}})).Body.String()
	if !strings.Contains(body, "Canon LBP246") || !strings.Contains(body, "netpr will send: PDF") || !strings.Contains(body, "Use this printer") {
		t.Fatalf("probe: %s", body)
	}
	if a.set.Get().PrinterURI == uri {
		t.Fatal("testing switched the printer")
	}

	body = a.do(formReq("/printer/address/test", url.Values{"address": {"127.0.0.1:1"}})).Body.String()
	if !strings.Contains(body, "No IPP printer answered") {
		t.Errorf("unreachable probe: %s", body)
	}
	// Saving an unreachable address is refused.
	a.do(formReq("/printer/address", url.Values{"uri": {"ipp://127.0.0.1:2/ipp/print"}}))
	if a.set.Get().PrinterURI != "ipp://127.0.0.1:1/ipp/print" {
		t.Error("unreachable address was saved")
	}

	rec := a.do(formReq("/printer/address", url.Values{"uri": {uri}}))
	if !strings.Contains(rec.Body.String(), "Now printing to") || rec.Header().Get("HX-Trigger") != "printer-changed" {
		t.Fatalf("save: %s", rec.Body.String())
	}
	if a.set.Get().PrinterURI != uri || a.svc.Printer.Get().URI() != uri {
		t.Error("printer not switched")
	}
	if got := a.svc.Monitor.Status().MakeModel; got != "Canon LBP246" {
		t.Errorf("monitor not refreshed: %q", got)
	}

	a.do(formReq("/printer/address/reset", nil))
	if a.svc.Printer.Get().URI() != "ipp://127.0.0.1:1/ipp/print" {
		t.Error("reset did not switch back")
	}
}

func TestTestPageAndNotify(t *testing.T) {
	a := newTestApp(t, 1, nil)
	body := a.do(formReq("/printer/test-page", nil)).Body.String()
	if !strings.Contains(body, "Test page sent") || !strings.Contains(body, "NetPr test page") {
		t.Fatalf("test page: %s", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if list, _ := a.store.List(context.Background(), 1, 0); len(list) == 1 && list[0].Terminal() {
			if list[0].State != store.StateDryRun || list[0].Pages != 1 || list[0].Duplex {
				t.Errorf("test page job = %+v", list[0])
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if body := a.do(formReq("/printer/test-notify", nil)).Body.String(); !strings.Contains(body, "set up. Add PUSHOVER_TOKEN") {
		t.Errorf("notify without pushover: %s", body)
	}
}

// rasterPrinter is an AirPrint-style printer that takes PWG Raster but not
// PDF, like the Canon LBP246dw. It records each document it is sent.
type rasterPrinter struct {
	mu   sync.Mutex
	docs [][]byte
}

func (p *rasterPrinter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	op := binary.BigEndian.Uint16(body[2:4])
	var b bytes.Buffer
	b.Write([]byte{2, 0, 0, 0})
	b.Write(body[4:8])
	put := func(tag int8, name string, value []byte) {
		b.WriteByte(byte(tag))
		binary.Write(&b, binary.BigEndian, uint16(len(name)))
		b.WriteString(name)
		binary.Write(&b, binary.BigEndian, uint16(len(value)))
		b.Write(value)
	}
	u32 := func(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }
	b.WriteByte(byte(ipp.TagDelimiterOperation))
	put(ipp.TagCharset, "attributes-charset", []byte("utf-8"))
	put(ipp.TagLanguage, "attributes-natural-language", []byte("en"))
	switch int16(op) {
	case ipp.OperationGetPrinterAttributes:
		b.WriteByte(byte(ipp.TagDelimiterPrinter))
		put(ipp.TagEnum, "printer-state", u32(uint32(ipp.PrinterStateIdle)))
		put(ipp.TagText, "printer-make-and-model", []byte("Canon LBP245/246/248"))
		put(ipp.TagMimeType, "document-format-supported", []byte("application/octet-stream"))
		put(ipp.TagMimeType, "", []byte("image/pwg-raster"))
		put(ipp.TagKeyword, "sides-supported", []byte("one-sided"))
		put(ipp.TagKeyword, "", []byte("two-sided-long-edge"))
		put(ipp.TagResolution, "pwg-raster-document-resolution-supported", append(append(u32(300), u32(300)...), 3))
		put(ipp.TagKeyword, "pwg-raster-document-type-supported", []byte("srgb_8"))
		put(ipp.TagKeyword, "", []byte("sgray_8"))
		put(ipp.TagKeyword, "pwg-raster-document-sheet-back", []byte("normal"))
	case ipp.OperationPrintJob:
		p.mu.Lock()
		p.docs = append(p.docs, body)
		p.mu.Unlock()
		b.WriteByte(byte(ipp.TagDelimiterJob))
		put(ipp.TagInteger, "job-id", u32(7))
	case ipp.OperationGetJobAttributes:
		b.WriteByte(byte(ipp.TagDelimiterJob))
		put(ipp.TagEnum, "job-state", u32(uint32(ipp.JobStateCompleted)))
	}
	b.WriteByte(byte(ipp.TagDelimiterEnd))
	w.Header().Set("Content-Type", ipp.ContentTypeIPP)
	w.Write(b.Bytes())
}

func TestPrintsRasterWhenPrinterLacksPDF(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not installed")
	}
	fake := &rasterPrinter{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	a := newTestApp(t, 1, func(c *config.Config) {
		c.PrinterURI = "ipp://" + strings.TrimPrefix(srv.URL, "http://") + "/ipp/print"
		c.DryRun = false
	})
	a.svc.Monitor.Refresh(context.Background())

	pdf := string(testpage.PDF(testpage.Info{User: "me", PaperSize: "letter"}))
	id := idRe.FindStringSubmatch(a.do(uploadReq(t, map[string]string{"page.pdf": pdf})).Body.String())[1]
	a.do(printReq(url.Values{"id": {id}, "copies": {"2"}, "duplex": {"on"}}))

	var job store.Job
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if list, _ := a.store.List(context.Background(), 1, 0); len(list) == 1 && list[0].Terminal() {
			job = list[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != store.StateCompleted || job.IPPJobID != 7 {
		t.Fatalf("job = %+v", job)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	req := fake.docs[0]
	if !bytes.Contains(req, []byte("image/pwg-raster")) || bytes.Contains(req, []byte("application/pdf")) {
		t.Error("document-format is not image/pwg-raster")
	}
	i := bytes.Index(req, []byte("RaS2"))
	if i < 0 {
		t.Fatal("no PWG Raster in the request")
	}
	hdr := req[i+4:]
	if !bytes.HasPrefix(hdr, []byte("PwgRaster")) || binary.BigEndian.Uint32(hdr[272:]) != 1 ||
		binary.BigEndian.Uint32(hdr[276:]) != 300 || binary.BigEndian.Uint32(hdr[372:]) != 2550 {
		t.Error("unexpected PWG page header")
	}
	if !bytes.Contains(req, []byte("two-sided-long-edge")) {
		t.Error("sides not sent")
	}
	// The printer ignores IPP copies for raster, so the copies are in the
	// document: page, blank back, page. The attribute itself is left out.
	if got := bytes.Count(req[i:], []byte("PwgRaster")); got != 3 {
		t.Errorf("%d raster pages, want 3 (2 copies of 1 page, blank back between)", got)
	}
	if bytes.Contains(req[:i], []byte("copies")) {
		t.Error("copies attribute sent as well as copies in the document")
	}
}

func TestJobFinishedToast(t *testing.T) {
	a := newTestApp(t, 2, nil)
	id := idRe.FindStringSubmatch(a.do(uploadReq(t, map[string]string{"résumé.pdf": "%PDF-1.7 x"})).Body.String())[1]
	body := a.do(printReq(url.Values{"id": {id}, "copies": {"1"}})).Body.String()
	if !strings.Contains(body, `class="jobbar"`) || !strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Errorf("submitted job has no progress bar or polling: %s", body)
	}
	var rec *httptest.ResponseRecorder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec = a.do(httptest.NewRequest(http.MethodGet, "/jobs/1", nil))
		if rec.Header().Get("HX-Trigger") != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := rec.Header().Get("HX-Trigger")
	for _, r := range got {
		if r >= 0x80 {
			t.Fatalf("HX-Trigger is not ASCII: %s", got)
		}
	}
	var ev struct {
		Finished struct {
			Title, Name string
			OK          bool
		} `json:"job-finished"`
	}
	if err := json.Unmarshal([]byte(got), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Finished.Name != "r\u00e9sum\u00e9.pdf" || !ev.Finished.OK || ev.Finished.Title != "Printed (dry run)" {
		t.Errorf("event = %+v", ev.Finished)
	}
	if strings.Contains(rec.Body.String(), "jobbar") {
		t.Error("finished job still shows a progress bar")
	}
}

func TestStaticAssetsAreFingerprinted(t *testing.T) {
	a := newTestApp(t, 1, nil)
	page := a.do(httptest.NewRequest(http.MethodGet, "/", nil)).Body.String()
	m := regexp.MustCompile(`href="(/static/css/app\.css\?v=[0-9a-f]{10})"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("stylesheet link not fingerprinted")
	}
	rec := a.do(httptest.NewRequest(http.MethodGet, m[1], nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("fingerprinted CSS: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rec.Body.String(), `--display: "Montserrat"`) {
		t.Error("headings are not set in Montserrat")
	}
	rec = a.do(httptest.NewRequest(http.MethodGet, "/static/fonts/montserrat-latin-wght-normal.woff2", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("font: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
