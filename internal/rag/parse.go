// Package rag implements document ingestion (parse, chunk, embed) and
// retrieval for the gateway's vector stores.
package rag

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
)

// SupportedExtensions lists accepted upload types.
var SupportedExtensions = map[string]bool{
	".pdf": true, ".txt": true, ".md": true, ".markdown": true,
	".docx": true, ".html": true, ".htm": true,
}

// IsSupported reports whether a filename can be ingested.
func IsSupported(filename string) bool {
	return SupportedExtensions[strings.ToLower(filepath.Ext(filename))]
}

// Block is one unit of extracted text: a paragraph, list item, table row
// or page fragment together with the heading path it sits under and the
// page it came from (PDF only; 0 otherwise).
type Block struct {
	Text    string
	Section string
	Page    int
}

// Parsed is the result of extracting a document.
type Parsed struct {
	Blocks []Block
	// Title is the document title when the format carries one (HTML
	// <title>); empty otherwise.
	Title string
	// PageCount is the number of pages of a PDF (before the page cap); 0
	// for formats without pages.
	PageCount int
}

// Extraction limits. Uploads are bounded by MAX_UPLOAD_MB, but a small file
// can expand into far more text (zip bombs, PDFs with thousands of pages),
// so the parsers cap what they produce as well.
const (
	// maxExtractedText caps the total text of one document, every format.
	maxExtractedText = 20 << 20
	// maxPDFPages is the number of pages read from a PDF; the rest is ignored.
	maxPDFPages = 2000
	// pdfTimeout bounds PDF parsing, which runs in library code that has no
	// cancellation hook of its own.
	pdfTimeout = 60 * time.Second
	// maxPDFPageText caps the text of a single page. A dense real page holds
	// a few KiB; far more means a crafted content stream.
	maxPDFPageText = 2 << 20
	// maxPDFTreeNodes bounds the page-tree walk in pdfPages: at most this
	// many nodes are visited, however many the tree references. Every visit
	// re-reads the object through the xref, so the budget is what keeps a
	// crafted fan-out (one node listed thousands of times) to about a
	// second; a real 2000-page document has a few thousand nodes.
	maxPDFTreeNodes = 10_000
	// maxPDFTreeDepth bounds /Pages nesting and the /Parent chain a page's
	// inherited attributes are looked up through.
	maxPDFTreeDepth = 64
	// maxDocxXML caps the size of word/document.xml inside a DOCX.
	maxDocxXML = 32 << 20
	// maxDocxRatio is the highest uncompressed/compressed ratio accepted for
	// word/document.xml; real documents sit far below it.
	maxDocxRatio = 100
)

// ErrTooMuchText reports a document whose extracted text exceeds the cap.
var ErrTooMuchText = fmt.Errorf("document text exceeds %d MiB after extraction", maxExtractedText>>20)

// ExtractBlocks parses an uploaded document into blocks. The file type is
// taken from the filename extension.
func ExtractBlocks(ctx context.Context, filename string, data []byte) ([]Block, error) {
	p, err := Extract(ctx, filename, data)
	if err != nil {
		return nil, err
	}
	return p.Blocks, nil
}

// Extract parses an uploaded document into blocks plus document metadata.
// ctx bounds the parse (PDF parsing also has its own timeout).
func Extract(ctx context.Context, filename string, data []byte) (*Parsed, error) {
	var (
		p   *Parsed
		err error
	)
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		p, err = extractPDF(ctx, data)
	case ".txt":
		if len(data) > maxExtractedText {
			return nil, ErrTooMuchText
		}
		p = &Parsed{Blocks: paragraphBlocks(normalizeText(string(data)), "", 0)}
	case ".md", ".markdown":
		if len(data) > maxExtractedText {
			return nil, ErrTooMuchText
		}
		p = &Parsed{Blocks: extractMarkdown(normalizeText(string(data)))}
	case ".docx":
		p, err = extractDOCX(data)
	case ".html", ".htm":
		p, err = extractHTML(data)
	default:
		return nil, fmt.Errorf("unsupported file type %q", filepath.Ext(filename))
	}
	if err != nil {
		return nil, err
	}
	total := 0
	for _, b := range p.Blocks {
		total += len(b.Text)
		if total > maxExtractedText {
			return nil, ErrTooMuchText
		}
	}
	return p, nil
}

// ExtractText returns the plain text of an uploaded document: all blocks
// joined by blank lines. Kept for callers that do not need structure.
func ExtractText(ctx context.Context, filename string, data []byte) (string, error) {
	blocks, err := ExtractBlocks(ctx, filename, data)
	if err != nil {
		return "", err
	}
	return JoinBlocks(blocks), nil
}

// JoinBlocks concatenates block texts with blank lines between them.
func JoinBlocks(blocks []Block) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n")
}

// SniffOK checks that the bytes plausibly match the extension. Only DOCX is
// checked (it must be a zip archive); everything else is accepted.
func SniffOK(filename string, data []byte) bool {
	if strings.ToLower(filepath.Ext(filename)) == ".docx" {
		return len(data) >= 2 && data[0] == 'P' && data[1] == 'K'
	}
	return true
}

// paragraphBlocks splits text on blank lines.
func paragraphBlocks(text, section string, page int) []Block {
	var out []Block
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, Block{Text: p, Section: section, Page: page})
	}
	return out
}

// sectionPath tracks a heading hierarchy and renders the last two levels
// as "Parent > Child".
type sectionPath struct {
	levels [10]string // index = heading level 1..9; 0 is a document title
}

func (sp *sectionPath) set(level int, title string) {
	if level < 0 || level > 9 {
		return
	}
	sp.levels[level] = strings.TrimSpace(title)
	for i := level + 1; i < len(sp.levels); i++ {
		sp.levels[i] = ""
	}
}

func (sp *sectionPath) String() string {
	var parts []string
	for _, l := range sp.levels {
		if l != "" {
			parts = append(parts, l)
		}
	}
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, " > ")
}

// extractMarkdown tracks ATX headings and yields paragraphs under them.
// Fenced code blocks are kept intact and never mistaken for headings.
func extractMarkdown(text string) []Block {
	var (
		out   []Block
		sp    sectionPath
		para  []string
		fence bool
	)
	flush := func() {
		if p := strings.TrimSpace(strings.Join(para, "\n")); p != "" {
			out = append(out, Block{Text: p, Section: sp.String()})
		}
		para = para[:0]
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = !fence
			para = append(para, line)
			continue
		}
		if fence {
			para = append(para, line)
			continue
		}
		if level, title, ok := atxHeading(trimmed); ok {
			flush()
			sp.set(level, title)
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		para = append(para, line)
	}
	flush()
	return out
}

func atxHeading(line string) (int, string, bool) {
	if !strings.HasPrefix(line, "#") {
		return 0, "", false
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level > 6 || level == len(line) || line[level] != ' ' {
		return 0, "", false
	}
	title := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[level:]), "#"))
	if title == "" {
		return 0, "", false
	}
	return level, title, true
}

// extractPDF parses at most maxPDFPages pages within pdfTimeout, in the
// PDFWorker child process when one is configured (see pdfworker.go).
//
// In-process, the PDF library can loop or crawl on crafted content and
// offers no way to interrupt a page, so the work runs in a goroutine: on
// timeout the caller returns an error and the goroutine is abandoned. It
// exits on its own when the current page finishes because it checks ctx
// between pages, and the buffered result channel means it never blocks on
// a departed receiver.
func extractPDF(ctx context.Context, data []byte) (*Parsed, error) {
	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()
	if len(PDFWorker) > 0 {
		return extractPDFExternal(ctx, data)
	}
	type result struct {
		out *Parsed
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := extractPDFPages(ctx, data)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("pdf parse exceeded %s", pdfTimeout)
		}
		return nil, ctx.Err()
	}
}

// extractPDFPages does the parsing. It must run under the recover below:
// the library panics on many malformed inputs (dangling references, object
// streams that are not streams, bad filters) and the panic would otherwise
// take the ingester down with it.
func extractPDFPages(ctx context.Context, data []byte) (out *Parsed, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("pdf parse failed: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open pdf: %w", err)
	}
	pages, count := pdfPages(r)
	out = &Parsed{PageCount: count}
	total := 0
	for i, v := range pages {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !pdfParentChainOK(v) {
			continue
		}
		s, perr := pdf.Page{V: v}.GetPlainText(nil)
		if perr != nil {
			continue
		}
		if len(s) > maxPDFPageText {
			return nil, fmt.Errorf("pdf page %d yields more than %d MiB of text", i+1, maxPDFPageText>>20)
		}
		total += len(s)
		if total > maxExtractedText {
			return nil, ErrTooMuchText
		}
		out.Blocks = append(out.Blocks, paragraphBlocks(normalizeText(s), "", i+1)...)
	}
	if len(out.Blocks) == 0 {
		return nil, fmt.Errorf("pdf contains no extractable text (scanned image?)")
	}
	return out, nil
}

// pdfPages walks the page tree from the catalog and returns the first
// maxPDFPages leaf pages in document order plus the number of leaves seen.
// Reader.Page is not used: it follows /Kids and trusts /Count with no cycle
// guard, so a node that lists itself keeps it spinning past the timeout
// (the goroutine cannot be interrupted), and a huge /Count is reported as
// the page count. The walk is bounded by depth and by a node budget.
func pdfPages(r *pdf.Reader) ([]pdf.Value, int) {
	type node struct {
		v     pdf.Value
		depth int
	}
	stack := []node{{v: r.Trailer().Key("Root").Key("Pages")}}
	budget := maxPDFTreeNodes
	var pages []pdf.Value
	count := 0
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch n.v.Key("Type").Name() {
		case "Page":
			count++
			if len(pages) < maxPDFPages {
				pages = append(pages, n.v)
			}
		case "Pages":
			if n.depth >= maxPDFTreeDepth {
				continue
			}
			kids := n.v.Key("Kids")
			// Pushed in reverse so the first kid is popped first.
			for i := kids.Len() - 1; i >= 0 && budget > 0; i-- {
				budget--
				stack = append(stack, node{kids.Index(i), n.depth + 1})
			}
		}
	}
	return pages, count
}

// pdfParentChainOK reports whether the page carries /Resources itself or
// its /Parent chain ends within maxPDFTreeDepth hops. The library resolves
// inherited resources by walking that chain with no cycle guard, so a page
// whose ancestry loops back on itself is skipped rather than parsed.
func pdfParentChainOK(page pdf.Value) bool {
	v := page
	for i := 0; i < maxPDFTreeDepth && !v.IsNull(); i++ {
		if !v.Key("Resources").IsNull() {
			return true
		}
		v = v.Key("Parent")
	}
	return v.IsNull()
}

// ---- DOCX ----

// extractDOCX reads word/document.xml from the OOXML archive. Paragraphs
// styled Heading1..9 (or Title) form the section hierarchy; table rows
// become one block per row with cells joined by " | ".
func extractDOCX(data []byte) (*Parsed, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open docx: %w", err)
	}
	var docXML []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			// The header sizes are checked first (a bomb declares them
			// honestly or lies; both are caught: an honest header fails
			// here, a lying one hits the LimitReader below).
			if f.UncompressedSize64 > maxDocxXML {
				return nil, fmt.Errorf("docx document.xml is %d MiB; the limit is %d MiB", f.UncompressedSize64>>20, maxDocxXML>>20)
			}
			if f.UncompressedSize64 > maxDocxRatio*max(f.CompressedSize64, 1) {
				return nil, errors.New("docx document.xml has an implausible compression ratio")
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("open docx: %w", err)
			}
			docXML, err = io.ReadAll(io.LimitReader(rc, maxDocxXML+1))
			_ = rc.Close()
			if err != nil {
				return nil, fmt.Errorf("read docx: %w", err)
			}
			if len(docXML) > maxDocxXML {
				return nil, fmt.Errorf("docx document.xml exceeds %d MiB", maxDocxXML>>20)
			}
			break
		}
	}
	if docXML == nil {
		return nil, fmt.Errorf("docx has no word/document.xml")
	}

	var (
		out          Parsed
		sp           sectionPath
		para         strings.Builder // current w:p text
		style        string          // current w:p style
		inPara       bool
		cells        []string // finished cells of the current row
		cell         strings.Builder
		inCell       bool
		depth        int // nesting inside w:tbl
		cellHasParas bool
	)
	dec := xml.NewDecoder(bytes.NewReader(docXML))
	emitPara := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if level, ok := docxHeadingLevel(style); ok {
			sp.set(level, text)
			return
		}
		out.Blocks = append(out.Blocks, Block{Text: text, Section: sp.String()})
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse docx: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tbl":
				depth++
			case "tr":
				cells = cells[:0]
			case "tc":
				inCell = true
				cell.Reset()
				cellHasParas = false
			case "p":
				inPara = true
				para.Reset()
				style = ""
			case "pStyle":
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						style = a.Value
					}
				}
			case "tab":
				if inPara {
					para.WriteByte('\t')
				}
			case "br", "cr":
				if inPara {
					para.WriteByte('\n')
				}
			case "t":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, fmt.Errorf("parse docx: %w", err)
				}
				if inPara {
					para.WriteString(s)
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				inPara = false
				if inCell {
					if cellHasParas {
						cell.WriteByte(' ')
					}
					cell.WriteString(strings.TrimSpace(para.String()))
					cellHasParas = true
				} else {
					emitPara(para.String())
				}
			case "tc":
				inCell = false
				cells = append(cells, strings.TrimSpace(cell.String()))
			case "tr":
				if depth > 0 {
					row := strings.TrimSpace(strings.Join(cells, " | "))
					if strings.Trim(row, " |") != "" {
						out.Blocks = append(out.Blocks, Block{Text: row, Section: sp.String()})
					}
				}
			case "tbl":
				if depth > 0 {
					depth--
				}
			}
		}
	}
	if len(out.Blocks) == 0 {
		return nil, fmt.Errorf("docx contains no text")
	}
	return &out, nil
}

// docxHeadingLevel maps paragraph styles like "Heading1" or "heading 2" to
// a heading level; "Title" sits above all headings (level 0).
func docxHeadingLevel(style string) (int, bool) {
	s := strings.ToLower(strings.ReplaceAll(style, " ", ""))
	if s == "title" {
		return 0, true
	}
	if !strings.HasPrefix(s, "heading") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "heading"))
	if err != nil || n < 1 || n > 9 {
		return 0, false
	}
	return n, true
}

// ---- HTML ----

var htmlSkip = map[string]bool{"script": true, "style": true, "noscript": true, "nav": true,
	"header": true, "footer": true, "svg": true, "template": true, "head": true}

var htmlBlockTags = map[string]bool{"p": true, "li": true, "td": true, "th": true, "pre": true,
	"blockquote": true, "div": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"section": true, "article": true, "main": true, "tr": true, "ul": true, "ol": true, "table": true, "body": true}

// extractHTML walks the DOM: h1..h3 set the section hierarchy, block
// elements with direct text become blocks and boilerplate elements are
// dropped. The <title> element becomes the document title.
func extractHTML(data []byte) (*Parsed, error) {
	root, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	var (
		out Parsed
		sp  sectionPath
		cur strings.Builder
	)
	flush := func() {
		if t := collapseSpace(cur.String()); t != "" {
			out.Blocks = append(out.Blocks, Block{Text: t, Section: sp.String()})
		}
		cur.Reset()
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			cur.WriteString(n.Data)
			return
		case html.ElementNode:
			tag := strings.ToLower(n.Data)
			if tag == "head" {
				if t := findElement(n, "title"); t != nil {
					out.Title = collapseSpace(nodeText(t))
				}
				return
			}
			if tag == "title" {
				return
			}
			if htmlSkip[tag] {
				return
			}
			if tag == "h1" || tag == "h2" || tag == "h3" {
				flush()
				sp.set(int(tag[1]-'0'), collapseSpace(nodeText(n)))
				return
			}
			if tag == "br" {
				cur.WriteByte('\n')
				return
			}
			if htmlBlockTags[tag] {
				flush()
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				flush()
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	flush()
	if len(out.Blocks) == 0 {
		return nil, fmt.Errorf("html contains no text")
	}
	return &out, nil
}

func findElement(n *html.Node, tag string) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.ToLower(c.Data) == tag {
			return c
		}
		if f := findElement(c, tag); f != nil {
			return f
		}
	}
	return nil
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// collapseSpace squeezes runs of whitespace into single spaces, keeping
// newlines that separate lines (as in <pre> or <br>).
func collapseSpace(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l != "" {
			out = append(out, l)
		}
	}
	return normalizeText(strings.Join(out, "\n"))
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
