//go:build integration

// Run against a real Gotenberg:
//
//	docker run --rm -p 127.0.0.1:3999:3000 gotenberg/gotenberg:8.37.0
//	NETPR_TEST_GOTENBERG=http://127.0.0.1:3999 go test -tags integration ./internal/convert
package convert

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gotenbergForTest(t *testing.T) *Gotenberg {
	url := os.Getenv("NETPR_TEST_GOTENBERG")
	if url == "" {
		t.Skip("NETPR_TEST_GOTENBERG not set")
	}
	g := NewGotenberg(url, "letter")
	if err := g.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g
}

func convertFile(t *testing.T, g *Gotenberg, name string, data []byte) (string, int) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "original")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Detect(src, name)
	if err != nil {
		t.Fatalf("detect %s: %v", name, err)
	}
	dst := filepath.Join(dir, "document.pdf")
	if err := g.ToPDF(context.Background(), src, f, dst); err != nil {
		t.Fatalf("convert %s: %v", name, err)
	}
	head := make([]byte, 5)
	fh, _ := os.Open(dst)
	fh.Read(head)
	fh.Close()
	if string(head) != "%PDF-" {
		t.Fatalf("%s: output is not a PDF: %q", name, head)
	}
	n, err := g.PageCount(context.Background(), dst)
	if err != nil {
		t.Fatalf("page count %s: %v", name, err)
	}
	return dst, n
}

func minimalDOCX(paragraphs int) []byte {
	var body strings.Builder
	for i := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t>Paragraph ` + string(rune('A'+i%26)) + ` of the netpr test document.</w:t></w:r></w:p>`)
	}
	files := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels":         `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":   `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body.String() + `</w:body></w:document>`,
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, content := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	return b.Bytes()
}

func TestGotenbergIntegration(t *testing.T) {
	g := gotenbergForTest(t)

	t.Run("text spans pages and splits", func(t *testing.T) {
		var txt strings.Builder
		for i := range 400 {
			txt.WriteString("line ")
			txt.WriteString(strings.Repeat("x", i%40))
			txt.WriteString("\n")
		}
		pdf, pages := convertFile(t, g, "long.txt", []byte(txt.String()))
		if pages < 4 {
			t.Fatalf("expected a multi-page PDF, got %d pages", pages)
		}
		out := filepath.Join(t.TempDir(), "selected.pdf")
		if err := g.Split(context.Background(), pdf, "1-2,4", out); err != nil {
			t.Fatal(err)
		}
		if n, err := g.PageCount(context.Background(), out); err != nil || n != 3 {
			t.Fatalf("split 1-2,4 -> %d pages, err %v; want 3", n, err)
		}
	})

	t.Run("docx", func(t *testing.T) {
		if _, pages := convertFile(t, g, "test.docx", minimalDOCX(5)); pages != 1 {
			t.Errorf("docx pages = %d, want 1", pages)
		}
	})

	t.Run("landscape image is one page", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 1600, 900))
		for x := range 1600 {
			for y := range 900 {
				img.Set(x, y, color.RGBA{uint8(x / 7), 148, 244, 255})
			}
		}
		var buf bytes.Buffer
		png.Encode(&buf, img)
		if _, pages := convertFile(t, g, "wide.png", buf.Bytes()); pages != 1 {
			t.Errorf("image pages = %d, want 1", pages)
		}
	})
}
