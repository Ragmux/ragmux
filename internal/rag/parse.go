// Package rag implements document ingestion (parse, chunk, embed) and
// retrieval for the gateway's vector stores.
package rag

import (
	"bytes"
	"fmt"
	"os"
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

// ExtractText reads a file and returns its plain text.
func ExtractText(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return extractPDF(path)
	case ".txt", ".md", ".markdown":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return normalizeText(string(b)), nil
	}
	return "", fmt.Errorf("unsupported file type %q", filepath.Ext(path))
}

func extractPDF(path string) (text string, err error) {
	defer func() {
		// The PDF library panics on some malformed inputs.
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf parse failed: %v", r)
		}
	}()
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}
	defer f.Close()
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
