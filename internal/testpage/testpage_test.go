package testpage

import (
	"bytes"
	"regexp"
	"strconv"
	"testing"
	"time"
)

func TestPDFStructure(t *testing.T) {
	pdf := PDF(Info{User: "me (admin)", URI: "ipp://10.0.0.5/ipp/print", PaperSize: "a4", Time: time.Now()})
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Fatal("missing header or trailer")
	}
	if !bytes.Contains(pdf, []byte("/MediaBox [0 0 595 842]")) {
		t.Error("a4 media box")
	}
	if !bytes.Contains(pdf, []byte(`me \(admin\)`)) {
		t.Error("parentheses not escaped")
	}
	// Every xref offset must point at its object.
	m := regexp.MustCompile(`startxref\n(\d+)`).FindSubmatch(pdf)
	xref, _ := strconv.Atoi(string(m[1]))
	if !bytes.HasPrefix(pdf[xref:], []byte("xref\n")) {
		t.Fatal("startxref does not point at xref")
	}
	entries := regexp.MustCompile(`(\d{10}) 00000 n`).FindAllSubmatch(pdf[xref:], -1)
	if len(entries) != 6 {
		t.Fatalf("%d xref entries", len(entries))
	}
	for i, e := range entries {
		off, _ := strconv.Atoi(string(e[1]))
		if want := strconv.Itoa(i+1) + " 0 obj"; !bytes.HasPrefix(pdf[off:], []byte(want)) {
			t.Errorf("object %d offset wrong", i+1)
		}
	}
	// The declared stream length must match.
	sm := regexp.MustCompile(`(?s)/Length (\d+) >>\nstream\n(.*)endstream`).FindSubmatch(pdf)
	if n, _ := strconv.Atoi(string(sm[1])); n != len(sm[2]) {
		t.Errorf("stream length %d, actual %d", n, len(sm[2]))
	}
}

func TestEscapeNonASCII(t *testing.T) {
	if got := escape(`a\b(c)é`); got != `a\\b\(c\)?` {
		t.Errorf("got %q", got)
	}
}
