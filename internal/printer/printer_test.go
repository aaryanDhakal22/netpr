package printer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	ipp "github.com/phin1x/go-ipp"
)

// fakePrinter is an in-process IPP endpoint that records requests and replies
// with whatever respond returns.
type fakePrinter struct {
	t       *testing.T
	mu      sync.Mutex
	reqs    []*ipp.Request
	docs    [][]byte
	respond func(req *ipp.Request) *ipp.Response
}

func (f *fakePrinter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != ipp.ContentTypeIPP {
		f.t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(r.Body)
	req, doc, err := parseRequest(body)
	if err != nil {
		f.t.Errorf("decode request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.docs = append(f.docs, doc)
	f.mu.Unlock()

	resp := f.respond(req)
	resp.RequestId = req.RequestId
	b, err := encodeResponse(resp)
	if err != nil {
		f.t.Errorf("encode response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ipp.ContentTypeIPP)
	w.Write(b)
}

// parseRequest decodes an IPP request per RFC 8010. go-ipp's own request
// decoder cannot read multi-valued attributes, so the fake uses this instead.
// Single values are stored as int/bool/string, multiple values as []any.
func parseRequest(b []byte) (*ipp.Request, []byte, error) {
	if len(b) < 9 {
		return nil, nil, errors.New("short request")
	}
	req := ipp.NewRequest(int16(binary.BigEndian.Uint16(b[2:4])), int32(binary.BigEndian.Uint32(b[4:8])))
	var group map[string]any
	last := ""
	for i := 8; i < len(b); {
		tag := b[i]
		i++
		switch {
		case tag == byte(ipp.TagDelimiterEnd):
			return req, b[i:], nil
		case tag == byte(ipp.TagDelimiterOperation):
			group = req.OperationAttributes
			continue
		case tag == byte(ipp.TagDelimiterJob):
			group = req.JobAttributes
			continue
		case tag == byte(ipp.TagDelimiterPrinter):
			group = req.PrinterAttributes
			continue
		case tag < 0x10:
			return nil, nil, fmt.Errorf("unsupported group 0x%02x", tag)
		}
		if i+2 > len(b) {
			return nil, nil, errors.New("truncated name length")
		}
		nl := int(binary.BigEndian.Uint16(b[i:]))
		i += 2
		name := string(b[i : i+nl])
		i += nl
		vl := int(binary.BigEndian.Uint16(b[i:]))
		i += 2
		raw := b[i : i+vl]
		i += vl
		var v any
		switch int8(tag) {
		case ipp.TagInteger, ipp.TagEnum:
			v = int(int32(binary.BigEndian.Uint32(raw)))
		case ipp.TagBoolean:
			v = raw[0] != 0
		default:
			v = string(raw)
		}
		if name == "" { // additional value of the previous attribute
			switch prev := group[last].(type) {
			case []any:
				group[last] = append(prev, v)
			default:
				group[last] = []any{prev, v}
			}
			continue
		}
		last = name
		group[name] = v
	}
	return nil, nil, errors.New("missing end-of-attributes tag")
}

// encodeResponse writes resp per RFC 8010 using each Attribute's own tag.
// go-ipp's encoder looks tags up by name and cannot write multiple values.
func encodeResponse(resp *ipp.Response) ([]byte, error) {
	var b bytes.Buffer
	b.Write([]byte{2, 0})
	binary.Write(&b, binary.BigEndian, resp.StatusCode)
	binary.Write(&b, binary.BigEndian, resp.RequestId)

	b.WriteByte(byte(ipp.TagDelimiterOperation))
	op := ipp.Attributes{
		"attributes-charset":          attr(ipp.TagCharset, "attributes-charset", "utf-8"),
		"attributes-natural-language": attr(ipp.TagLanguage, "attributes-natural-language", "en"),
	}
	maps.Copy(op, resp.OperationAttributes)
	if err := writeGroup(&b, op, "attributes-charset", "attributes-natural-language"); err != nil {
		return nil, err
	}
	for _, g := range resp.JobAttributes {
		b.WriteByte(byte(ipp.TagDelimiterJob))
		if err := writeGroup(&b, g); err != nil {
			return nil, err
		}
	}
	for _, g := range resp.PrinterAttributes {
		b.WriteByte(byte(ipp.TagDelimiterPrinter))
		if err := writeGroup(&b, g); err != nil {
			return nil, err
		}
	}
	b.WriteByte(byte(ipp.TagDelimiterEnd))
	return b.Bytes(), nil
}

func writeGroup(b *bytes.Buffer, attrs ipp.Attributes, first ...string) error {
	names := append([]string(nil), first...)
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		if !slices.Contains(first, name) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		for i, a := range attrs[name] {
			b.WriteByte(byte(a.Tag))
			n := name
			if i > 0 {
				n = "" // additional value
			}
			binary.Write(b, binary.BigEndian, uint16(len(n)))
			b.WriteString(n)
			switch v := a.Value.(type) {
			case int:
				binary.Write(b, binary.BigEndian, uint16(4))
				binary.Write(b, binary.BigEndian, int32(v))
			case bool:
				binary.Write(b, binary.BigEndian, uint16(1))
				if v {
					b.WriteByte(1)
				} else {
					b.WriteByte(0)
				}
			case string:
				binary.Write(b, binary.BigEndian, uint16(len(v)))
				b.WriteString(v)
			default:
				return fmt.Errorf("attribute %s: unsupported value %T", name, v)
			}
		}
	}
	return nil
}

func newFake(t *testing.T, respond func(*ipp.Request) *ipp.Response) (*fakePrinter, *Client) {
	t.Helper()
	f := &fakePrinter{t: t, respond: respond}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New("ipp://" + strings.TrimPrefix(srv.URL, "http://") + "/ipp/print")
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func attr(tag int8, name string, vals ...any) []ipp.Attribute {
	out := make([]ipp.Attribute, len(vals))
	for i, v := range vals {
		out[i] = ipp.Attribute{Tag: tag, Name: name, Value: v}
	}
	return out
}

func TestNewTranslatesURI(t *testing.T) {
	for in, want := range map[string]string{
		"ipp://10.0.0.5/ipp/print":      "http://10.0.0.5:631/ipp/print",
		"ipps://printer.lan/ipp/print":  "https://printer.lan:631/ipp/print",
		"ipp://10.0.0.5:8631/ipp/print": "http://10.0.0.5:8631/ipp/print",
	} {
		c, err := New(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if c.httpURL != want {
			t.Errorf("%s -> %s, want %s", in, c.httpURL, want)
		}
	}
	if _, err := New("http://10.0.0.5/"); err == nil {
		t.Error("http:// URI should be rejected")
	}
}

func TestPrintSendsDocumentAndOptions(t *testing.T) {
	f, c := newFake(t, func(req *ipp.Request) *ipp.Response {
		// Strict printers answer successful-ok-ignored-or-substituted-attributes;
		// that must still count as success.
		resp := ipp.NewResponse(ipp.StatusOkIgnoredOrSubstituted, 0)
		resp.JobAttributes = []ipp.Attributes{{"job-id": attr(ipp.TagInteger, "job-id", 42)}}
		return resp
	})

	pdf := []byte("%PDF-1.7 fake document")
	id, err := c.Print(context.Background(), bytes.NewReader(pdf), len(pdf), PrintOptions{
		JobName: "report.pdf", User: "aaryan", Copies: 3, Sides: SidesLongEdge,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("job id = %d, want 42", id)
	}

	req := f.reqs[0]
	if req.Operation != ipp.OperationPrintJob {
		t.Errorf("operation = %#x", req.Operation)
	}
	checks := map[string]any{
		"printer-uri":          c.printerURI,
		"requesting-user-name": "aaryan",
		"job-name":             "report.pdf",
		"document-format":      "application/pdf",
	}
	for k, want := range checks {
		if got := req.OperationAttributes[k]; got != want {
			t.Errorf("operation %s = %v, want %v", k, got, want)
		}
	}
	if got := req.JobAttributes["copies"]; got != 3 {
		t.Errorf("copies = %v", got)
	}
	if got := req.JobAttributes["sides"]; got != SidesLongEdge {
		t.Errorf("sides = %v", got)
	}
	if !bytes.Equal(f.docs[0], pdf) {
		t.Errorf("document = %q", f.docs[0])
	}
}

func TestPrintOmitsDefaults(t *testing.T) {
	f, c := newFake(t, func(*ipp.Request) *ipp.Response {
		resp := ipp.NewResponse(ipp.StatusOk, 0)
		resp.JobAttributes = []ipp.Attributes{{"job-id": attr(ipp.TagInteger, "job-id", 7)}}
		return resp
	})
	if _, err := c.Print(context.Background(), strings.NewReader("x"), 1, PrintOptions{Copies: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.reqs[0].JobAttributes["copies"]; ok {
		t.Error("copies=1 should not be sent")
	}
	if _, ok := f.reqs[0].JobAttributes["sides"]; ok {
		t.Error("empty sides should not be sent")
	}
	if got := f.reqs[0].OperationAttributes["requesting-user-name"]; got != "netpr" {
		t.Errorf("default user = %v", got)
	}
}

func TestPrintRejected(t *testing.T) {
	_, c := newFake(t, func(*ipp.Request) *ipp.Response {
		resp := ipp.NewResponse(ipp.StatusErrorDocumentFormatNotSupported, 0)
		resp.OperationAttributes["status-message"] = attr(ipp.TagText, "status-message", "PDF not supported")
		return resp
	})
	_, err := c.Print(context.Background(), strings.NewReader("x"), 1, PrintOptions{})
	var ie *IPPError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want IPPError", err)
	}
	if ie.Status != ipp.StatusErrorDocumentFormatNotSupported || ie.Message != "PDF not supported" {
		t.Errorf("got %+v", ie)
	}
}

func TestJobStatus(t *testing.T) {
	_, c := newFake(t, func(req *ipp.Request) *ipp.Response {
		if req.OperationAttributes["job-id"] == 404 {
			return ipp.NewResponse(ipp.StatusErrorNotFound, 0)
		}
		resp := ipp.NewResponse(ipp.StatusOk, 0)
		resp.JobAttributes = []ipp.Attributes{{
			"job-state":         attr(ipp.TagEnum, "job-state", int(ipp.JobStateProcessing)),
			"job-state-reasons": attr(ipp.TagKeyword, "job-state-reasons", "job-printing"),
		}}
		return resp
	})
	js, err := c.JobStatus(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if js.State != "processing" || len(js.Reasons) != 1 || js.Reasons[0] != "job-printing" {
		t.Errorf("got %+v", js)
	}
	if _, err := c.JobStatus(context.Background(), 404); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("err = %v, want ErrJobNotFound", err)
	}
}

func TestStatus(t *testing.T) {
	_, c := newFake(t, func(*ipp.Request) *ipp.Response {
		resp := ipp.NewResponse(ipp.StatusOk, 0)
		resp.PrinterAttributes = []ipp.Attributes{{
			"printer-state":             attr(ipp.TagEnum, "printer-state", int(ipp.PrinterStateIdle)),
			"printer-state-reasons":     attr(ipp.TagKeyword, "printer-state-reasons", "media-empty-error", "toner-low-warning"),
			"printer-is-accepting-jobs": attr(ipp.TagBoolean, "printer-is-accepting-jobs", true),
			"printer-make-and-model":    attr(ipp.TagText, "printer-make-and-model", "Canon LBP246"),
			"document-format-supported": attr(ipp.TagMimeType, "document-format-supported", "application/pdf", "image/urf"),
			"sides-supported":           attr(ipp.TagKeyword, "sides-supported", "one-sided", "two-sided-long-edge"),
			"marker-names":              attr(ipp.TagName, "marker-names", "Black Toner"),
			"marker-levels":             attr(ipp.TagInteger, "marker-levels", 12),
		}}
		return resp
	})
	s := c.Status(context.Background())
	if !s.Reachable {
		t.Fatalf("unreachable: %s", s.Error)
	}
	if s.State != "idle" || !s.Accepting || s.MakeModel != "Canon LBP246" {
		t.Errorf("got %+v", s)
	}
	if !s.SupportsPDF() || !s.SupportsDuplex() {
		t.Errorf("capabilities: formats=%v sides=%v", s.Formats, s.Sides)
	}
	if len(s.Markers) != 1 || s.Markers[0].Name != "Black Toner" || s.Markers[0].Level != 12 {
		t.Errorf("markers = %+v", s.Markers)
	}
	if len(s.Reasons) != 2 {
		t.Errorf("reasons = %v", s.Reasons)
	}
}

func TestStatusUnreachable(t *testing.T) {
	c, _ := New("ipp://127.0.0.1:1/ipp/print")
	s := c.Status(context.Background())
	if s.Reachable || s.Error == "" {
		t.Errorf("got %+v", s)
	}
}
