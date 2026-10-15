package convert

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Kind string

const (
	KindPDF    Kind = "pdf"
	KindImage  Kind = "image"
	KindOffice Kind = "office"
	KindText   Kind = "text"
)

// File describes an upload after content sniffing.
type File struct {
	Kind Kind
	Ext  string // extension Gotenberg should see, e.g. ".docx"
	MIME string
}

var ErrUnsupported = errors.New("unsupported file type")

var oleMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// Detect identifies a file by its contents. The client-supplied name is only
// consulted to tell legacy Office formats apart, which share one container.
func Detect(path, clientName string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return File{}, err
	}
	head = head[:n]
	if n == 0 {
		return File{}, errors.New("file is empty")
	}

	if bytes.HasPrefix(head, oleMagic) {
		switch ext := strings.ToLower(filepath.Ext(clientName)); ext {
		case ".doc", ".xls", ".ppt":
			return File{Kind: KindOffice, Ext: ext, MIME: "application/x-ole-storage"}, nil
		}
		return File{}, ErrUnsupported
	}

	mime := http.DetectContentType(head)
	switch {
	case mime == "application/pdf":
		return File{Kind: KindPDF, Ext: ".pdf", MIME: mime}, nil
	case mime == "image/png":
		return File{Kind: KindImage, Ext: ".png", MIME: mime}, nil
	case mime == "image/jpeg":
		return File{Kind: KindImage, Ext: ".jpg", MIME: mime}, nil
	case strings.HasPrefix(mime, "text/plain"):
		return File{Kind: KindText, Ext: ".txt", MIME: "text/plain"}, nil
	case mime == "application/zip":
		st, err := f.Stat()
		if err != nil {
			return File{}, err
		}
		return detectZip(f, st.Size())
	}
	return File{}, ErrUnsupported
}

// detectZip recognizes OOXML and OpenDocument containers.
func detectZip(r io.ReaderAt, size int64) (File, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return File{}, ErrUnsupported
	}
	for _, zf := range zr.File {
		switch zf.Name {
		case "word/document.xml":
			return File{Kind: KindOffice, Ext: ".docx", MIME: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}, nil
		case "xl/workbook.xml":
			return File{Kind: KindOffice, Ext: ".xlsx", MIME: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}, nil
		case "ppt/presentation.xml":
			return File{Kind: KindOffice, Ext: ".pptx", MIME: "application/vnd.openxmlformats-officedocument.presentationml.presentation"}, nil
		case "mimetype":
			rc, err := zf.Open()
			if err != nil {
				return File{}, ErrUnsupported
			}
			b, _ := io.ReadAll(io.LimitReader(rc, 100))
			rc.Close()
			switch string(b) {
			case "application/vnd.oasis.opendocument.text":
				return File{Kind: KindOffice, Ext: ".odt", MIME: string(b)}, nil
			case "application/vnd.oasis.opendocument.spreadsheet":
				return File{Kind: KindOffice, Ext: ".ods", MIME: string(b)}, nil
			case "application/vnd.oasis.opendocument.presentation":
				return File{Kind: KindOffice, Ext: ".odp", MIME: string(b)}, nil
			}
		}
	}
	return File{}, ErrUnsupported
}

// jpegOrientation returns the EXIF orientation (1-8) of a JPEG, or 1 when absent.
func jpegOrientation(r io.Reader) int {
	br := &byteReader{r: r}
	if br.u16() != 0xFFD8 {
		return 1
	}
	for br.err == nil {
		marker := br.u16()
		if marker&0xFF00 != 0xFF00 || marker == 0xFFDA { // start of scan: no more metadata
			return 1
		}
		segLen := int(br.u16()) - 2
		if segLen < 0 {
			return 1
		}
		seg := br.bytes(segLen)
		if marker == 0xFFE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
	}
	return 1
}

func tiffOrientation(t []byte) int {
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int(bo.Uint32(t[4:8]))
	if ifd+2 > len(t) {
		return 1
	}
	count := int(bo.Uint16(t[ifd:]))
	for i := range count {
		e := ifd + 2 + i*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

type byteReader struct {
	r   io.Reader
	err error
}

func (b *byteReader) bytes(n int) []byte {
	if b.err != nil {
		return nil
	}
	buf := make([]byte, n)
	_, b.err = io.ReadFull(b.r, buf)
	return buf
}

func (b *byteReader) u16() uint16 {
	buf := b.bytes(2)
	if b.err != nil {
		return 0
	}
	return binary.BigEndian.Uint16(buf)
}
