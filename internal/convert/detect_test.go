package convert

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func zipWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	return b.Bytes()
}

func TestDetect(t *testing.T) {
	var pngBuf, jpgBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	png.Encode(&pngBuf, img)
	jpeg.Encode(&jpgBuf, img, nil)

	tests := []struct {
		name       string
		data       []byte
		clientName string
		kind       Kind
		ext        string
	}{
		{"pdf", []byte("%PDF-1.7\n..."), "x.bin", KindPDF, ".pdf"},
		{"png", pngBuf.Bytes(), "photo", KindImage, ".png"},
		{"jpeg named png", jpgBuf.Bytes(), "photo.png", KindImage, ".jpg"},
		{"text", []byte("hello\nworld\n"), "notes", KindText, ".txt"},
		{"docx", zipWith(t, map[string]string{"[Content_Types].xml": "", "word/document.xml": "<w/>"}), "a.zip", KindOffice, ".docx"},
		{"xlsx", zipWith(t, map[string]string{"xl/workbook.xml": "<x/>"}), "", KindOffice, ".xlsx"},
		{"pptx", zipWith(t, map[string]string{"ppt/presentation.xml": "<p/>"}), "", KindOffice, ".pptx"},
		{"odt", zipWith(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text"}), "", KindOffice, ".odt"},
		{"legacy doc", append(append([]byte{}, oleMagic...), make([]byte, 100)...), "Old.DOC", KindOffice, ".doc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Detect(write(t, tt.data), tt.clientName)
			if err != nil {
				t.Fatal(err)
			}
			if f.Kind != tt.kind || f.Ext != tt.ext {
				t.Errorf("got %s %s, want %s %s", f.Kind, f.Ext, tt.kind, tt.ext)
			}
		})
	}
}

func TestDetectRejects(t *testing.T) {
	for name, data := range map[string][]byte{
		"html":        []byte("<!doctype html><html><script>alert(1)</script>"),
		"plain zip":   zipWith(t, map[string]string{"readme.md": "hi"}),
		"ole unnamed": append(append([]byte{}, oleMagic...), make([]byte, 100)...),
		"binary":      {0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0},
	} {
		if _, err := Detect(write(t, data), "file"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
	if _, err := Detect(write(t, nil), "empty.pdf"); err == nil {
		t.Error("empty file should fail")
	}
}

// jpegWithOrientation builds a minimal JPEG header carrying an EXIF orientation.
func jpegWithOrientation(o uint16, bo binary.ByteOrder, mark string) []byte {
	tiff := new(bytes.Buffer)
	tiff.WriteString(mark)
	binary.Write(tiff, bo, uint16(42))
	binary.Write(tiff, bo, uint32(8)) // IFD0 offset
	binary.Write(tiff, bo, uint16(1)) // one entry
	binary.Write(tiff, bo, uint16(0x0112))
	binary.Write(tiff, bo, uint16(3)) // SHORT
	binary.Write(tiff, bo, uint32(1))
	binary.Write(tiff, bo, o)
	binary.Write(tiff, bo, uint16(0))
	binary.Write(tiff, bo, uint32(0)) // next IFD

	app1 := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(app1)+2))
	out = append(out, app1...)
	return append(out, 0xFF, 0xDA, 0, 2)
}

func TestJPEGOrientation(t *testing.T) {
	if got := jpegOrientation(bytes.NewReader(jpegWithOrientation(6, binary.LittleEndian, "II"))); got != 6 {
		t.Errorf("little endian: got %d", got)
	}
	if got := jpegOrientation(bytes.NewReader(jpegWithOrientation(8, binary.BigEndian, "MM"))); got != 8 {
		t.Errorf("big endian: got %d", got)
	}
	if got := jpegOrientation(bytes.NewReader([]byte{0xFF, 0xD8, 0xFF, 0xDA})); got != 1 {
		t.Errorf("no exif: got %d", got)
	}
	if got := jpegOrientation(bytes.NewReader([]byte("not a jpeg"))); got != 1 {
		t.Errorf("garbage: got %d", got)
	}
}
