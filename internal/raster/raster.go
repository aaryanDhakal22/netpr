// Package raster turns PDF pages into PWG Raster (PWG 5102.4), the page
// image format IPP Everywhere printers accept when they cannot print PDF.
//
// Pages are rendered with poppler's pdftoppm and pdfinfo, scaled down to fit
// the paper if needed, turned to portrait, centered, and run-length encoded.
package raster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// Media is a paper size in points.
type Media struct {
	Name          string // PWG self-describing media name
	Width, Height float64
}

var (
	Letter = Media{Name: "na_letter_8.5x11in", Width: 612, Height: 792}
	A4     = Media{Name: "iso_a4_210x297mm", Width: 595.28, Height: 841.89}
)

// MediaFor returns the paper for a netpr paper-size setting.
func MediaFor(size string) Media {
	if size == "a4" {
		return A4
	}
	return Letter
}

type Options struct {
	DPI    int
	Media  Media
	Duplex bool
	Tumble bool // duplex flips on the short edge
}

// Convert renders every page of the PDF at src as 8-bit grayscale PWG Raster.
func Convert(ctx context.Context, src string, dst io.Writer, o Options) (pages int, err error) {
	sizes, err := pageSizes(ctx, src)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriterSize(dst, 256<<10)
	w.WriteString("RaS2")

	dpi := float64(o.DPI)
	cw := int(math.Round(o.Media.Width / 72 * dpi))
	ch := int(math.Round(o.Media.Height / 72 * dpi))
	canvas := newGray(cw, ch)
	for i, sz := range sizes {
		pw, ph := sz[0], sz[1]
		landscape := pw > ph
		if landscape {
			pw, ph = ph, pw
		}
		// Shrink pages bigger than the paper; never enlarge small ones.
		scale := min(1, o.Media.Width/pw, o.Media.Height/ph)
		img, err := renderPage(ctx, src, i+1, dpi*scale)
		if err != nil {
			return i, fmt.Errorf("page %d: %w", i+1, err)
		}
		if landscape {
			img = img.rotateCCW()
		}
		canvas.fill(0xff)
		canvas.drawCentered(img)

		h := header{
			width: cw, height: ch, dpi: o.DPI, media: o.Media, duplex: o.Duplex, tumble: o.Tumble,
			totalPages: len(sizes),
		}
		w.Write(h.encode())
		encodeImage(w, canvas)
	}
	return len(sizes), w.Flush()
}

var (
	sizeLine = regexp.MustCompile(`(?m)^Page\s+(\d+) size:\s+([\d.]+) x ([\d.]+) pts`)
	rotLine  = regexp.MustCompile(`(?m)^Page\s+(\d+) rot:\s+(\d+)`)
)

// pageSizes returns each page's displayed size in points, after /Rotate.
func pageSizes(ctx context.Context, src string) ([][2]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pdfinfo", "-f", "1", "-l", "100000", src).Output()
	if err != nil {
		return nil, fmt.Errorf("pdfinfo: %w", execErr(err))
	}
	var sizes [][2]float64
	for _, m := range sizeLine.FindAllSubmatch(out, -1) {
		w, _ := strconv.ParseFloat(string(m[2]), 64)
		h, _ := strconv.ParseFloat(string(m[3]), 64)
		sizes = append(sizes, [2]float64{w, h})
	}
	for _, m := range rotLine.FindAllSubmatch(out, -1) {
		n, _ := strconv.Atoi(string(m[1]))
		rot, _ := strconv.Atoi(string(m[2]))
		if n >= 1 && n <= len(sizes) && rot%180 == 90 {
			sizes[n-1][0], sizes[n-1][1] = sizes[n-1][1], sizes[n-1][0]
		}
	}
	if len(sizes) == 0 {
		return nil, errors.New("pdfinfo found no pages")
	}
	return sizes, nil
}

func renderPage(ctx context.Context, src string, page int, dpi float64) (*gray, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	n := strconv.Itoa(page)
	cmd := exec.CommandContext(ctx, "pdftoppm", "-gray", "-r", strconv.FormatFloat(dpi, 'f', 2, 64),
		"-f", n, "-l", n, "-singlefile", src)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pdftoppm: %w", execErr(err))
	}
	return parsePGM(out)
}

func execErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
	}
	return err
}

// ---- images ----

type gray struct {
	w, h int
	pix  []byte // 0 black, 255 white, like sGray
}

func newGray(w, h int) *gray { return &gray{w: w, h: h, pix: make([]byte, w*h)} }

func (g *gray) fill(v byte) {
	for i := range g.pix {
		g.pix[i] = v
	}
}

// rotateCCW turns the image a quarter turn counter-clockwise, the IPP
// convention for printing landscape pages on portrait paper.
func (g *gray) rotateCCW() *gray {
	r := newGray(g.h, g.w)
	for y := range g.h {
		row := g.pix[y*g.w : (y+1)*g.w]
		for x, v := range row {
			r.pix[(g.w-1-x)*r.w+y] = v
		}
	}
	return r
}

// drawCentered copies src into the middle of g, clipping any overhang.
func (g *gray) drawCentered(src *gray) {
	ox, oy := (g.w-src.w)/2, (g.h-src.h)/2
	for sy := range src.h {
		dy := sy + oy
		if dy < 0 || dy >= g.h {
			continue
		}
		sx0, dx0 := 0, ox
		if dx0 < 0 {
			sx0, dx0 = -dx0, 0
		}
		n := min(src.w-sx0, g.w-dx0)
		if n > 0 {
			copy(g.pix[dy*g.w+dx0:dy*g.w+dx0+n], src.pix[sy*src.w+sx0:])
		}
	}
}

// parsePGM reads a binary 8-bit PGM (P5), as written by pdftoppm -gray.
func parsePGM(b []byte) (*gray, error) {
	var vals [3]int
	i := 2
	if !bytes.HasPrefix(b, []byte("P5")) {
		return nil, errors.New("pdftoppm did not return a PGM image")
	}
	for k := range vals {
		for i < len(b) {
			if b[i] == '#' {
				for i < len(b) && b[i] != '\n' {
					i++
				}
			} else if b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r' {
				i++
			} else {
				break
			}
		}
		start := i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
		n, err := strconv.Atoi(string(b[start:i]))
		if err != nil {
			return nil, errors.New("bad PGM header")
		}
		vals[k] = n
	}
	i++ // single whitespace before the pixels
	w, h, maxv := vals[0], vals[1], vals[2]
	if maxv != 255 || w <= 0 || h <= 0 || len(b)-i < w*h {
		return nil, fmt.Errorf("unsupported PGM %dx%d max %d", w, h, maxv)
	}
	return &gray{w: w, h: h, pix: b[i : i+w*h]}, nil
}

// ---- PWG Raster ----

type header struct {
	width, height, dpi int
	media              Media
	duplex, tumble     bool
	totalPages         int
}

// encode lays out the 1796-byte PWG page header (PWG 5102.4 section 4.3).
func (h header) encode() []byte {
	b := make([]byte, 1796)
	str := func(off int, s string) { copy(b[off:off+63], s) }
	u32 := func(off int, v uint32) { binary.BigEndian.PutUint32(b[off:], v) }
	flag := func(v bool) uint32 {
		if v {
			return 1
		}
		return 0
	}
	str(0, "PwgRaster")
	u32(272, flag(h.duplex))
	u32(276, uint32(h.dpi))
	u32(280, uint32(h.dpi))
	u32(340, 1) // NumCopies: copies come from the IPP job
	u32(352, uint32(math.Round(h.media.Width)))
	u32(356, uint32(math.Round(h.media.Height)))
	u32(368, flag(h.tumble))
	u32(372, uint32(h.width))
	u32(376, uint32(h.height))
	u32(384, 8)               // BitsPerColor
	u32(388, 8)               // BitsPerPixel
	u32(392, uint32(h.width)) // BytesPerLine
	u32(400, 18)              // ColorSpace: sgray
	u32(420, 1)               // NumColors
	u32(452, uint32(h.totalPages))
	u32(456, 1) // CrossFeedTransform
	u32(460, 1) // FeedTransform
	str(1732, h.media.Name)
	return b
}

// encodeImage writes the PWG run-length encoding: each line starts with a
// repeat count for identical following lines, then runs of pixels where
// 0-127 means "repeat the next pixel n+1 times" and 129-255 means "257-n
// literal pixels follow".
func encodeImage(w *bufio.Writer, g *gray) {
	for y := 0; y < g.h; {
		line := g.pix[y*g.w : (y+1)*g.w]
		rep := 0
		for y+rep+1 < g.h && rep < 255 && bytes.Equal(line, g.pix[(y+rep+1)*g.w:(y+rep+2)*g.w]) {
			rep++
		}
		w.WriteByte(byte(rep))
		encodeLine(w, line)
		y += rep + 1
	}
}

func encodeLine(w *bufio.Writer, line []byte) {
	n := len(line)
	for i := 0; i < n; {
		j := i + 1
		for j < n && j-i < 128 && line[j] == line[i] {
			j++
		}
		if j-i >= 2 {
			w.WriteByte(byte(j - i - 1))
			w.WriteByte(line[i])
			i = j
			continue
		}
		// Literal run up to the start of the next repeat.
		j = i + 1
		for j < n && j-i < 128 && !(j+1 < n && line[j] == line[j+1]) {
			j++
		}
		if j-i == 1 {
			w.WriteByte(0)
			w.WriteByte(line[i])
		} else {
			w.WriteByte(byte(257 - (j - i)))
			w.Write(line[i:j])
		}
		i = j
	}
}
