// Package testpage draws a one-page PDF for checking the printer, without
// needing Gotenberg.
package testpage

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

type Info struct {
	User      string
	Printer   string // make and model, if known
	URI       string
	PaperSize string // letter | a4
	Time      time.Time
}

// PDF returns a single-page PDF with the details in info, a border to check
// margins and a row of grey boxes to check toner coverage.
func PDF(info Info) []byte {
	w, h := 612.0, 792.0 // letter, in points
	if info.PaperSize == "a4" {
		w, h = 595.0, 842.0
	}
	var c strings.Builder
	// Border half an inch from each edge.
	fmt.Fprintf(&c, "0 G 1.5 w 36 36 %.0f %.0f re S\n", w-72, h-72)

	y := h - 120
	text(&c, "F2", 28, 72, y, "NetPr test page")
	y -= 40
	text(&c, "F1", 12, 72, y, "If you can read this, printing from NetPr works.")
	y -= 36
	model := info.Printer
	if model == "" {
		model = "unknown model"
	}
	for _, l := range [][2]string{
		{"Printed", info.Time.Format("Mon Jan 2, 2006 at 15:04 MST")},
		{"By", info.User},
		{"Printer", model},
		{"Address", info.URI},
		{"Paper", strings.ToUpper(info.PaperSize)},
	} {
		text(&c, "F2", 11, 72, y, l[0])
		text(&c, "F1", 11, 150, y, l[1])
		y -= 20
	}

	y -= 40
	text(&c, "F2", 11, 72, y, "Toner check: each box should be evenly shaded, with no streaks.")
	y -= 80
	box := (w - 144 - 4*12) / 5
	for i, gray := range []float64{0.9, 0.7, 0.5, 0.25, 0} {
		x := 72 + float64(i)*(box+12)
		fmt.Fprintf(&c, "%.2f g %.1f %.1f %.1f 60 re f 0 g\n", gray, x, y, box)
		text(&c, "F1", 9, x, y-14, fmt.Sprintf("%d%%", int((1-gray)*100+0.5)))
	}

	y -= 60
	text(&c, "F1", 10, 72, y, "Thin lines: each should be crisp and unbroken.")
	for i := range 4 {
		ly := y - 20 - float64(i)*10
		fmt.Fprintf(&c, "%.2f w 72 %.1f m %.1f %.1f l S\n", 0.25*float64(i+1), ly, w-72, ly)
	}
	return build(w, h, c.String())
}

func text(c *strings.Builder, font string, size, x, y float64, s string) {
	fmt.Fprintf(c, "BT /%s %.0f Tf %.1f %.1f Td (%s) Tj ET\n", font, size, x, y, escape(s))
}

// escape keeps printable ASCII, which the standard fonts can always show.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '(' || r == ')':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

func build(w, h float64, content string) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.0f %.0f] "+
			"/Resources << /Font << /F1 4 0 R /F2 5 0 R >> >> /Contents 6 0 R >>", w, h),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}
