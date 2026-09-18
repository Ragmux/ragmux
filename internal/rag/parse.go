// Package rag implements document ingestion (parse, chunk, embed) and
// retrieval for the gateway's vector stores.
package rag

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// SupportedExtensions lists accepted upload types.
var SupportedExtensions = map[string]bool{".pdf": true, ".txt": true, ".md": true, ".markdown": true}

// IsSupported reports whether a filename can be ingested.
func IsSupported(filename string) bool {
	return SupportedExtensions[strings.ToLower(filepath.Ext(filename))]
}

// ExtractText returns the plain text of an uploaded document. The file type
// is taken from the filename extension.
func ExtractText(filename string, data []byte) (string, error) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		return extractPDF(data)
	case ".txt", ".md", ".markdown":
		return normalizeText(string(data)), nil
	}
	return "", fmt.Errorf("unsupported file type %q", filepath.Ext(filename))
}

func extractPDF(data []byte) (text string, err error) {
	defer func() {
		// The PDF library panics on some malformed inputs.
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf parse failed: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}
	var buf bytes.Buffer
	n := r.NumPage()
	for i := 1; i <= n; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		s, perr := p.GetPlainText(nil)
		if perr != nil {
			continue
		}
		buf.WriteString(s)
		buf.WriteString("\n\n")
	}
	out := normalizeText(buf.String())
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("pdf contains no extractable text (scanned image?)")
	}
	return out, nil
}

// normalizeText fixes invalid UTF-8, line endings and excessive blank lines.
func normalizeText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\x00", "")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}
