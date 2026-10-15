// Package convert normalizes uploads to PDF and manipulates PDFs via Gotenberg.
package convert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type Gotenberg struct {
	base      string
	http      *http.Client
	paperSize atomic.Value // string: letter | a4, for images
}

func NewGotenberg(baseURL, paperSize string) *Gotenberg {
	g := &Gotenberg{base: baseURL, http: &http.Client{Timeout: 3 * time.Minute}}
	g.SetPaperSize(paperSize)
	return g
}

// SetPaperSize changes the page size used for images from now on.
func (g *Gotenberg) SetPaperSize(size string) { g.paperSize.Store(size) }

// PaperSize is the current paper size: letter or a4.
func (g *Gotenberg) PaperSize() string { return g.paperSize.Load().(string) }

func (g *Gotenberg) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gotenberg health: %s", resp.Status)
	}
	return nil
}

// ToPDF converts src (already identified as f) to a PDF written at dst.
// PDFs are copied through unchanged.
func (g *Gotenberg) ToPDF(ctx context.Context, src string, f File, dst string) error {
	switch f.Kind {
	case KindPDF:
		return copyFile(src, dst)
	case KindOffice, KindText:
		return g.post(ctx, "/forms/libreoffice/convert", nil, []part{{name: "document" + f.Ext, path: src}}, dst)
	case KindImage:
		return g.imageToPDF(ctx, src, f, dst)
	}
	return ErrUnsupported
}

var imagePage = template.Must(template.New("").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><style>
html, body { margin: 0; height: 100%; }
body { display: flex; align-items: center; justify-content: center; }
img { max-width: 100%; max-height: 99vh; object-fit: contain; image-orientation: from-image; }
</style></head><body><img src="{{.}}"></body></html>`))

// imageToPDF renders the image on a single page via Chromium, choosing
// landscape when the image (after EXIF rotation) is wider than tall.
func (g *Gotenberg) imageToPDF(ctx context.Context, src string, f File, dst string) error {
	fh, err := os.Open(src)
	if err != nil {
		return err
	}
	cfg, _, err := image.DecodeConfig(fh)
	if err != nil {
		fh.Close()
		return fmt.Errorf("read image: %w", err)
	}
	w, h := cfg.Width, cfg.Height
	if f.Ext == ".jpg" {
		if _, err := fh.Seek(0, io.SeekStart); err == nil {
			if o := jpegOrientation(fh); o >= 5 { // orientations 5-8 swap axes
				w, h = h, w
			}
		}
	}
	fh.Close()

	var html bytes.Buffer
	name := "image" + f.Ext
	if err := imagePage.Execute(&html, name); err != nil {
		return err
	}
	pw, ph := "8.5", "11"
	if g.PaperSize() == "a4" {
		pw, ph = "8.27", "11.7"
	}
	fields := map[string]string{
		"paperWidth": pw, "paperHeight": ph,
		"marginTop": "0.25", "marginBottom": "0.25", "marginLeft": "0.25", "marginRight": "0.25",
		"landscape": fmt.Sprint(w > h),
	}
	return g.post(ctx, "/forms/chromium/convert/html", fields, []part{
		{name: "index.html", data: html.Bytes()},
		{name: name, path: src},
	}, dst)
}

// Split writes the pages selected by span (e.g. "1-3,5") of src into dst.
func (g *Gotenberg) Split(ctx context.Context, src, span, dst string) error {
	fields := map[string]string{"splitMode": "pages", "splitSpan": span, "splitUnify": "true"}
	return g.post(ctx, "/forms/pdfengines/split", fields, []part{{name: "document.pdf", path: src}}, dst)
}

// PageCount reads the PDF's page count from Gotenberg's metadata endpoint.
func (g *Gotenberg) PageCount(ctx context.Context, src string) (int, error) {
	var out bytes.Buffer
	if err := g.do(ctx, "/forms/pdfengines/metadata/read", nil, []part{{name: "document.pdf", path: src}}, &out); err != nil {
		return 0, err
	}
	var meta map[string]map[string]any
	if err := json.Unmarshal(out.Bytes(), &meta); err != nil {
		return 0, fmt.Errorf("decode metadata: %w", err)
	}
	for _, m := range meta {
		if n, ok := m["PageCount"].(float64); ok && n > 0 {
			return int(n), nil
		}
	}
	return 0, errors.New("metadata has no PageCount")
}

type part struct {
	name string
	path string // read from disk when data is nil
	data []byte
}

func (g *Gotenberg) post(ctx context.Context, route string, fields map[string]string, files []part, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	err = g.do(ctx, route, fields, files, out)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

func (g *Gotenberg) do(ctx context.Context, route string, fields map[string]string, files []part, w io.Writer) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(writeForm(mw, fields, files))
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+route, pr)
	if err != nil {
		pr.Close()
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := g.http.Do(req)
	if err != nil {
		pr.Close()
		return fmt.Errorf("gotenberg %s: %w", route, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("gotenberg %s: %s: %s", route, resp.Status, strings.TrimSpace(string(msg)))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func writeForm(mw *multipart.Writer, fields map[string]string, files []part) error {
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	for _, p := range files {
		fw, err := mw.CreateFormFile("files", p.name)
		if err != nil {
			return err
		}
		if p.data != nil {
			if _, err := fw.Write(p.data); err != nil {
				return err
			}
			continue
		}
		f, err := os.Open(p.path)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return mw.Close()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
