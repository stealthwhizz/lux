package connectors

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"strings"
)

// DocxText reads the paragraphs of a Word document; ok is false when it is
// not one or has no text.
func DocxText(r io.ReaderAt, size int64) (string, bool) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", false
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" || f.UncompressedSize64 > 50<<20 {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", false
		}
		defer rc.Close()
		var b strings.Builder
		dec := xml.NewDecoder(io.LimitReader(rc, 50<<20))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.CharData:
				b.Write(t)
			case xml.EndElement:
				if t.Name.Local == "p" {
					b.WriteByte('\n')
				}
			}
		}
		text := strings.TrimSpace(b.String())
		return text, text != ""
	}
	return "", false
}
