// Package printer talks IPP directly to a network printer.
//
// go-ipp provides the wire encoding; requests are sent here rather than through
// its HttpAdapter because the adapter treats every non-zero status as an error,
// including successful-ok-ignored-or-substituted-attributes, which strict
// printers return for perfectly good jobs.
package printer

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	ipp "github.com/phin1x/go-ipp"
)

type Client struct {
	printerURI string // ipp://host/path, sent inside requests
	httpURL    string // http(s)://host:631/path, where requests are POSTed
	http       *http.Client
	reqID      atomic.Int32
}

// New builds a client for an ipp:// or ipps:// printer URI.
func New(printerURI string) (*Client, error) {
	u, err := url.Parse(printerURI)
	if err != nil {
		return nil, err
	}
	h := *u
	switch u.Scheme {
	case "ipp":
		h.Scheme = "http"
	case "ipps":
		h.Scheme = "https"
	default:
		return nil, fmt.Errorf("unsupported printer URI scheme %q", u.Scheme)
	}
	if u.Port() == "" {
		h.Host = u.Hostname() + ":631"
	}
	return &Client{
		printerURI: printerURI,
		httpURL:    h.String(),
		http: &http.Client{Transport: &http.Transport{
			// Printers on a LAN almost always present self-signed certificates.
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			ResponseHeaderTimeout: 2 * time.Minute,
		}},
	}, nil
}

func (c *Client) URI() string { return c.printerURI }

// ErrTLSRequired means the printer refuses plain ipp:// and wants ipps://.
var ErrTLSRequired = errors.New("printer requires an encrypted connection (ipps://)")

// IPPError is a non-successful IPP status returned by the printer.
type IPPError struct {
	Status  int16
	Message string
}

func (e *IPPError) Error() string {
	return fmt.Sprintf("printer returned IPP status 0x%04x: %s", uint16(e.Status), e.Message)
}

func (c *Client) do(ctx context.Context, req *ipp.Request) (*ipp.Response, error) {
	payload, err := req.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode IPP request: %w", err)
	}
	body := io.Reader(bytes.NewReader(payload))
	size := int64(len(payload))
	if req.File != nil {
		body = io.MultiReader(body, req.File)
		size += int64(req.FileSize)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpURL, body)
	if err != nil {
		return nil, err
	}
	hreq.ContentLength = size
	hreq.Header.Set("Content-Type", ipp.ContentTypeIPP)

	hresp, err := c.http.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer hresp.Body.Close()
	if hresp.StatusCode == http.StatusUpgradeRequired {
		return nil, ErrTLSRequired
	}
	if hresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("printer HTTP status %s", hresp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	resp, err := ipp.NewResponseDecoder(bytes.NewReader(raw)).Decode(nil)
	if err != nil {
		return nil, fmt.Errorf("decode IPP response: %w", err)
	}
	// 0x0000-0x00ff are all successful-ok variants.
	if resp.StatusCode < 0 || resp.StatusCode > 0x00ff {
		e := &IPPError{Status: resp.StatusCode, Message: "no status message"}
		if m := stringVal(resp.OperationAttributes, "status-message"); m != "" {
			e.Message = m
		}
		return nil, e
	}
	return resp, nil
}

func (c *Client) newRequest(op int16, user string) *ipp.Request {
	req := ipp.NewRequest(op, c.reqID.Add(1))
	req.OperationAttributes[ipp.AttributePrinterURI] = c.printerURI
	if user == "" {
		user = "netpr"
	}
	req.OperationAttributes[ipp.AttributeRequestingUserName] = user
	return req
}

type PrintOptions struct {
	JobName string
	User    string
	Format  string // document-format; defaults to application/pdf
	Copies  int
	// Sides is an IPP sides keyword; empty omits the attribute.
	Sides string
}

const (
	SidesOneSided = "one-sided"
	SidesLongEdge = "two-sided-long-edge"
)

// Print sends a document with Print-Job and returns the printer's job id.
func (c *Client) Print(ctx context.Context, doc io.Reader, size int, o PrintOptions) (int, error) {
	req := c.newRequest(ipp.OperationPrintJob, o.User)
	req.OperationAttributes[ipp.AttributeJobName] = truncate(o.JobName, 255)
	if o.Format == "" {
		o.Format = FormatPDF
	}
	req.OperationAttributes[ipp.AttributeDocumentFormat] = o.Format
	if o.Copies > 1 {
		req.JobAttributes[ipp.AttributeCopies] = o.Copies
	}
	if o.Sides != "" {
		req.JobAttributes[ipp.AttributeSides] = o.Sides
	}
	req.File, req.FileSize = doc, size

	resp, err := c.do(ctx, req)
	if err != nil {
		return 0, err
	}
	if len(resp.JobAttributes) == 0 {
		return 0, errors.New("printer accepted job but returned no job attributes")
	}
	id := intVal(resp.JobAttributes[0], ipp.AttributeJobID, 0)
	if id == 0 {
		return 0, errors.New("printer accepted job but returned no job-id")
	}
	return id, nil
}

type JobStatus struct {
	State   string // pending, processing, completed, canceled, aborted
	Reasons []string
	Message string
}

var ErrJobNotFound = errors.New("job not found on printer")

func (c *Client) JobStatus(ctx context.Context, jobID int) (JobStatus, error) {
	req := c.newRequest(ipp.OperationGetJobAttributes, "")
	req.OperationAttributes[ipp.AttributeJobID] = jobID
	req.OperationAttributes[ipp.AttributeRequestedAttributes] = []string{
		ipp.AttributeJobState, ipp.AttributeJobStateReasons, ipp.AttributeJobStateMessage,
	}
	resp, err := c.do(ctx, req)
	if e := (*IPPError)(nil); errors.As(err, &e) && e.Status == ipp.StatusErrorNotFound {
		return JobStatus{}, ErrJobNotFound
	}
	if err != nil {
		return JobStatus{}, err
	}
	if len(resp.JobAttributes) == 0 {
		return JobStatus{}, ErrJobNotFound
	}
	a := resp.JobAttributes[0]
	return JobStatus{
		State:   jobStateName(intVal(a, ipp.AttributeJobState, 0)),
		Reasons: stringVals(a, ipp.AttributeJobStateReasons),
		Message: stringVal(a, ipp.AttributeJobStateMessage),
	}, nil
}

func jobStateName(s int) string {
	switch int8(s) {
	case ipp.JobStatePending, ipp.JobStateHeld:
		return "pending"
	case ipp.JobStateProcessing, ipp.JobStateStopped:
		return "processing"
	case ipp.JobStateCanceled:
		return "canceled"
	case ipp.JobStateAborted:
		return "aborted"
	case ipp.JobStateCompleted:
		return "completed"
	}
	return "unknown"
}

type Marker struct {
	Name  string
	Color string
	Type  string
	Level int // percent; negative values are IPP "unknown" sentinels
}

type Status struct {
	URI       string
	Reachable bool
	Error     string
	NeedsTLS  bool // the printer refused plain ipp:// and wants ipps://
	CheckedAt time.Time
	Latency   time.Duration // round trip of the status request

	State     string // idle, processing, stopped
	Reasons   []string
	Message   string
	Accepting bool
	MakeModel string
	Formats   []string
	Sides     []string
	Markers   []Marker
	Trays     []Tray

	// PWG Raster capabilities, for printers that can't print PDF.
	RasterDPIs      []int
	RasterTypes     []string
	RasterSheetBack string
}

const (
	FormatPDF       = "application/pdf"
	FormatPWGRaster = "image/pwg-raster"
)

func (s Status) SupportsPDF() bool    { return slices.Contains(s.Formats, FormatPDF) }
func (s Status) SupportsDuplex() bool { return slices.Contains(s.Sides, SidesLongEdge) }

// SendsAs describes how netpr will send documents to this printer.
func (s Status) SendsAs() string {
	switch {
	case s.SupportsPDF():
		return "PDF"
	case s.SupportsRaster():
		return fmt.Sprintf("Page images (PWG Raster, %d dpi)", s.RasterDPI())
	}
	return ""
}

// SupportsRaster reports whether netpr can send 8-bit grayscale PWG Raster.
func (s Status) SupportsRaster() bool {
	return slices.Contains(s.Formats, FormatPWGRaster) && slices.Contains(s.RasterTypes, "sgray_8") && len(s.RasterDPIs) > 0
}

// RasterDPI picks the resolution to render at: 300 dpi if offered, otherwise
// the closest one, preferring higher.
func (s Status) RasterDPI() int {
	best := 0
	for _, d := range s.RasterDPIs {
		if best == 0 || abs(d-300) < abs(best-300) || (abs(d-300) == abs(best-300) && d > best) {
			best = d
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

var printerAttrs = []string{
	"printer-state", "printer-state-reasons", "printer-state-message", "printer-is-accepting-jobs",
	"printer-make-and-model", "document-format-supported", "sides-supported",
	"marker-names", "marker-colors", "marker-types", "marker-levels", "printer-input-tray",
	"pwg-raster-document-resolution-supported", "pwg-raster-document-type-supported",
	"pwg-raster-document-sheet-back",
}

// Status runs Get-Printer-Attributes. Transport failures come back as an
// unreachable Status rather than an error so callers can render them.
func (c *Client) Status(ctx context.Context) Status {
	st := Status{URI: c.printerURI, CheckedAt: time.Now()}
	req := c.newRequest(ipp.OperationGetPrinterAttributes, "")
	req.OperationAttributes[ipp.AttributeRequestedAttributes] = printerAttrs
	resp, err := c.do(ctx, req)
	st.Latency = time.Since(st.CheckedAt)
	if err != nil {
		st.Error = err.Error()
		st.NeedsTLS = errors.Is(err, ErrTLSRequired)
		return st
	}
	if len(resp.PrinterAttributes) == 0 {
		st.Error = "printer returned no attributes"
		return st
	}
	a := resp.PrinterAttributes[0]
	st.Reachable = true
	switch int8(intVal(a, "printer-state", 0)) {
	case ipp.PrinterStateIdle:
		st.State = "idle"
	case ipp.PrinterStateProcessing:
		st.State = "processing"
	case ipp.PrinterStateStopped:
		st.State = "stopped"
	default:
		st.State = "unknown"
	}
	st.Reasons = stringVals(a, "printer-state-reasons")
	st.Message = stringVal(a, "printer-state-message")
	if v := a["printer-is-accepting-jobs"]; len(v) > 0 {
		st.Accepting, _ = v[0].Value.(bool)
	}
	st.MakeModel = stringVal(a, "printer-make-and-model")
	st.Formats = stringVals(a, "document-format-supported")
	st.Sides = stringVals(a, "sides-supported")
	for _, v := range a["pwg-raster-document-resolution-supported"] {
		// Only square resolutions; netpr renders with one DPI for both axes.
		if r, ok := v.Value.(ipp.Resolution); ok && r.Height == r.Width && r.Width > 0 {
			st.RasterDPIs = append(st.RasterDPIs, int(r.Width))
		}
	}
	st.Trays = parseTrays(stringVals(a, "printer-input-tray"))
	st.RasterTypes = stringVals(a, "pwg-raster-document-type-supported")
	st.RasterSheetBack = stringVal(a, "pwg-raster-document-sheet-back")

	names, colors, types := stringVals(a, "marker-names"), stringVals(a, "marker-colors"), stringVals(a, "marker-types")
	levels := a["marker-levels"]
	for i, name := range names {
		m := Marker{Name: name, Level: -1}
		if i < len(colors) {
			m.Color = colors[i]
		}
		if i < len(types) {
			m.Type = types[i]
		}
		if i < len(levels) {
			m.Level, _ = levels[i].Value.(int)
		}
		st.Markers = append(st.Markers, m)
	}
	return st
}

func stringVal(a ipp.Attributes, name string) string {
	if v := a[name]; len(v) > 0 {
		s, _ := v[0].Value.(string)
		return s
	}
	return ""
}

func stringVals(a ipp.Attributes, name string) []string {
	var out []string
	for _, v := range a[name] {
		if s, ok := v.Value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intVal(a ipp.Attributes, name string, def int) int {
	if v := a[name]; len(v) > 0 {
		if n, ok := v[0].Value.(int); ok {
			return n
		}
	}
	return def
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Tray is one paper source from printer-input-tray (PWG 5100.13).
type Tray struct {
	Name     string
	Type     string // e.g. sheetFeedAutoRemovableTray
	Capacity int    // sheets; -2 unknown
	// Level is sheets left, or a sentinel: 0 empty, -2 unknown, -3 "some".
	Level int
	// Media size in hundredths of a millimetre; zero when not set.
	Feed, CrossFeed int
}

const (
	TrayLevelUnknown = -2
	TrayLevelSome    = -3
)

// Bypass reports whether the tray is a manual-feed slot, which is normally
// empty and doesn't mean the printer is out of paper.
func (t Tray) Bypass() bool {
	n := strings.ToLower(t.Name)
	return strings.Contains(n, "bypass") || strings.Contains(n, "manual") || strings.Contains(n, "multi") ||
		t.Type == "sheetFeedAutoNonRemovableTray" || t.Type == "sheetFeedManual"
}

// parseTrays reads printer-input-tray values like
// "type=sheetFeedAutoRemovableTray;mediafeed=27940;...;level=0;name=Drawer1;".
// The pseudo-tray for automatic selection is skipped.
func parseTrays(vals []string) []Tray {
	var out []Tray
	for _, v := range vals {
		t := Tray{Capacity: TrayLevelUnknown, Level: TrayLevelUnknown}
		for kv := range strings.SplitSeq(v, ";") {
			k, val, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, err := strconv.Atoi(val)
			switch k {
			case "name":
				t.Name = val
			case "type":
				t.Type = val
			case "maxcapacity":
				if err == nil {
					t.Capacity = n
				}
			case "level":
				if err == nil {
					t.Level = n
				}
			case "mediafeed":
				t.Feed = n
			case "mediaxfeed":
				t.CrossFeed = n
			}
		}
		if t.Name == "" || t.Type == "other" || strings.EqualFold(t.Name, "auto") {
			continue
		}
		out = append(out, t)
	}
	return out
}
