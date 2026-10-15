package raster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaryanDhakal22/netpr/internal/testpage"
)

type decodedPage struct {
	hdr []byte
	img *gray
}

func (p decodedPage) u32(off int) uint32 { return binary.BigEndian.Uint32(p.hdr[off:]) }
func (p decodedPage) str(off int) string { return strings.TrimRight(string(p.hdr[off:off+64]), "\x00") }

// decode is an independent reader for the PWG Raster stream.
func decode(b []byte) ([]decodedPage, error) {
	if !bytes.HasPrefix(b, []byte("RaS2")) {
		return nil, errors.New("missing sync word")
	}
	r := bytes.NewReader(b[4:])
	var pages []decodedPage
	for r.Len() > 0 {
		hdr := make([]byte, 1796)
		if _, err := r.Read(hdr); err != nil {
			return nil, err
		}
		p := decodedPage{hdr: hdr}
		w, h := int(p.u32(372)), int(p.u32(376))
		if int(p.u32(392)) != w {
			return nil, errors.New("bytes per line mismatch")
		}
		p.img = newGray(w, h)
		for y := 0; y < h; {
			rep, err := r.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", y, err)
			}
			line := p.img.pix[y*w : (y+1)*w]
			for x := 0; x < w; {
				c, err := r.ReadByte()
				if err != nil {
					return nil, err
				}
				switch {
				case c < 128:
					v, _ := r.ReadByte()
					for k := 0; k <= int(c); k++ {
						line[x] = v
						x++
					}
				case c > 128:
					n := 257 - int(c)
					if _, err := r.Read(line[x : x+n]); err != nil {
						return nil, err
					}
					x += n
				default:
					return nil, errors.New("reserved run byte 128")
				}
			}
			for k := 1; k <= int(rep); k++ {
				copy(p.img.pix[(y+k)*w:(y+k+1)*w], line)
			}
			y += int(rep) + 1
		}
		pages = append(pages, p)
	}
	return pages, nil
}

func TestEncodeRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	g := newGray(517, 300)
	for y := range g.h {
		for x := range g.w {
			switch {
			case y < 100: // noise: literal runs
				g.pix[y*g.w+x] = byte(rng.IntN(256))
			case y < 200: // stripes: repeats of every length
				g.pix[y*g.w+x] = byte((x / (1 + y%140)) % 3 * 100)
			default: // identical lines: line repeats beyond 256
				g.pix[y*g.w+x] = 255
			}
		}
	}
	var buf bytes.Buffer
	buf.WriteString("RaS2")
	h := header{width: g.w, height: g.h, dpi: 300, media: Letter}
	buf.Write(h.encode())
	w := bufio.NewWriter(&buf)
	encodeImage(w, g)
	w.Flush()

	pages, err := decode(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pages[0].img.pix, g.pix) {
		t.Fatal("round trip changed pixels")
	}
}

func TestHeader(t *testing.T) {
	p := decodedPage{hdr: header{width: 2550, height: 3300, dpi: 300, media: Letter, duplex: true, totalPages: 3}.encode()}
	checks := map[int]uint32{272: 1, 276: 300, 280: 300, 340: 1, 352: 612, 356: 792, 368: 0, 372: 2550, 376: 3300,
		384: 8, 388: 8, 392: 2550, 400: 18, 420: 1, 452: 3, 456: 1, 460: 1}
	for off, want := range checks {
		if got := p.u32(off); got != want {
			t.Errorf("offset %d = %d, want %d", off, got, want)
		}
	}
	if p.str(0) != "PwgRaster" || p.str(1732) != "na_letter_8.5x11in" {
		t.Errorf("strings: %q %q", p.str(0), p.str(1732))
	}
}

func TestRotateCCW(t *testing.T) {
	g := &gray{w: 3, h: 2, pix: []byte{1, 2, 3, 4, 5, 6}}
	r := g.rotateCCW()
	// 1 2 3      3 6
	// 4 5 6  ->  2 5
	//            1 4
	if r.w != 2 || r.h != 3 || !bytes.Equal(r.pix, []byte{3, 6, 2, 5, 1, 4}) {
		t.Errorf("got %dx%d %v", r.w, r.h, r.pix)
	}
}

// pdfPage writes a one-page PDF of the given size with a black square in
// its top-left corner.
func pdfPage(t *testing.T, w, h int) string {
	t.Helper()
	content := fmt.Sprintf("0 g 0 %d 72 72 re f\n", h-72)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Contents 4 0 R >>", w, h),
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	path := filepath.Join(t.TempDir(), "page.pdf")
	os.WriteFile(path, b.Bytes(), 0o600)
	return path
}

func needPoppler(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not installed")
	}
}

func dark(g *gray, x, y int) bool { return g.pix[y*g.w+x] < 128 }

func TestConvertPortraitLandscapeAndOversize(t *testing.T) {
	needPoppler(t)
	ctx := context.Background()
	o := Options{DPI: 100, Media: Letter, Duplex: true}

	cases := []struct {
		name       string
		w, h       int
		squareAt   [2]int // a pixel expected inside the black square
		whiteAt    [2]int
		marginLeft bool // page narrower than paper: white column at x=0
	}{
		// Letter portrait: square at top-left, full bleed.
		{"letter", 612, 792, [2]int{20, 20}, [2]int{200, 200}, false},
		// Letter landscape is turned counter-clockwise: top-left goes to bottom-left.
		{"landscape", 792, 612, [2]int{20, 1080}, [2]int{20, 20}, false},
		// A small page is centered, not enlarged.
		{"small", 306, 396, [2]int{230, 300}, [2]int{20, 20}, true},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		n, err := Convert(ctx, pdfPage(t, c.w, c.h), &buf, o)
		if err != nil || n != 1 {
			t.Fatalf("%s: %d pages, %v", c.name, n, err)
		}
		pages, err := decode(buf.Bytes())
		if err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		img := pages[0].img
		if img.w != 850 || img.h != 1100 || pages[0].u32(272) != 1 {
			t.Fatalf("%s: %dx%d duplex=%d", c.name, img.w, img.h, pages[0].u32(272))
		}
		if !dark(img, c.squareAt[0], c.squareAt[1]) {
			t.Errorf("%s: expected black at %v", c.name, c.squareAt)
		}
		if dark(img, c.whiteAt[0], c.whiteAt[1]) {
			t.Errorf("%s: expected white at %v", c.name, c.whiteAt)
		}
	}

	// A3 is shrunk to fit letter, keeping its shape.
	var buf bytes.Buffer
	if _, err := Convert(ctx, pdfPage(t, 842, 1191), &buf, o); err != nil {
		t.Fatal(err)
	}
	pages, _ := decode(buf.Bytes())
	img := pages[0].img
	// Scale = 792/1191; the page is 560pt wide, so a 26pt white margin each side.
	if dark(img, 10, 50) || !dark(img, 45, 50) {
		t.Error("A3 page not centered after shrinking")
	}
}

func TestConvertTestPage(t *testing.T) {
	needPoppler(t)
	pdf := testpage.PDF(testpage.Info{User: "me", URI: "ipps://printer/ipp/print", PaperSize: "letter"})
	path := filepath.Join(t.TempDir(), "test.pdf")
	os.WriteFile(path, pdf, 0o600)
	var buf bytes.Buffer
	if _, err := Convert(context.Background(), path, &buf, Options{DPI: 300, Media: Letter}); err != nil {
		t.Fatal(err)
	}
	pages, err := decode(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if pages[0].img.w != 2550 || pages[0].img.h != 3300 {
		t.Errorf("size %dx%d", pages[0].img.w, pages[0].img.h)
	}
	raw := 2550 * 3300
	t.Logf("test page: %d bytes of PWG raster (%.1f%% of raw)", buf.Len(), 100*float64(buf.Len())/float64(raw))
	if out := os.Getenv("RASTER_DUMP"); out != "" {
		os.WriteFile(out, buf.Bytes(), 0o644)
	}
}

// pdfPages writes an n-page letter PDF; page k has a black square k inches
// down the left edge, so pages can be told apart after rendering.
func pdfPages(t *testing.T, n int) string {
	t.Helper()
	objs := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	var kids []string
	for k := range n {
		content := fmt.Sprintf("0 g 0 %d 72 72 re f\n", 792-72*(k+1))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R >>", len(objs)))
		kids = append(kids, fmt.Sprintf("%d 0 R", len(objs)))
	}
	objs[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n)
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	path := filepath.Join(t.TempDir(), "pages.pdf")
	os.WriteFile(path, b.Bytes(), 0o600)
	return path
}

// pageMark returns which page a rendered image is (1-based, from its square),
// or 0 for a blank page.
func pageMark(img *gray, dpi int) int {
	for k := 1; k <= 10; k++ {
		if dark(img, dpi/4, dpi*(k-1)+dpi/2) {
			return k
		}
	}
	return 0
}

func TestConvertCopies(t *testing.T) {
	needPoppler(t)
	cases := []struct {
		name   string
		pages  int
		duplex bool
		want   []int // page marks in stream order; 0 = blank
	}{
		{"odd pages double-sided: blank back between copies", 3, true, []int{1, 2, 3, 0, 1, 2, 3}},
		{"odd pages single-sided: no blanks", 3, false, []int{1, 2, 3, 1, 2, 3}},
		{"one page double-sided", 1, true, []int{1, 0, 1}},
		{"even pages double-sided", 2, true, []int{1, 2, 1, 2}},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		n, err := Convert(context.Background(), pdfPages(t, c.pages), &buf,
			Options{DPI: 50, Media: Letter, Duplex: c.duplex, Copies: 2, TempDir: t.TempDir()})
		if err != nil || n != c.pages {
			t.Fatalf("%s: n=%d err=%v", c.name, n, err)
		}
		pages, err := decode(buf.Bytes())
		if err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		var got []int
		for _, p := range pages {
			got = append(got, pageMark(p.img, 50))
			if int(p.u32(452)) != len(c.want) {
				t.Errorf("%s: TotalPageCount = %d, want %d", c.name, p.u32(452), len(c.want))
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: pages %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConvertRemovesSpool(t *testing.T) {
	needPoppler(t)
	dir := t.TempDir()
	var buf bytes.Buffer
	if _, err := Convert(context.Background(), pdfPages(t, 2), &buf, Options{DPI: 50, Media: Letter, Copies: 3, TempDir: dir}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("spool file left behind: %v", entries)
	}
	pages, _ := decode(buf.Bytes())
	if len(pages) != 6 {
		t.Errorf("3 copies of 2 pages = %d pages", len(pages))
	}
}
