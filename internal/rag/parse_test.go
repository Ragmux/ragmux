package rag

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExtractMarkdownSections(t *testing.T) {
	md := "# Install\n\nIntro paragraph.\n\n## Docker\n\nRun the container.\n\nSecond docker paragraph.\n\n## Binary\n\n```\n# not a heading\n```\n\n# Usage\n\nCall the API."
	blocks, err := ExtractBlocks(context.Background(), "guide.md", []byte(md))
	if err != nil {
		t.Fatal(err)
	}
	want := []Block{
		{Text: "Intro paragraph.", Section: "Install"},
		{Text: "Run the container.", Section: "Install > Docker"},
		{Text: "Second docker paragraph.", Section: "Install > Docker"},
		{Text: "```\n# not a heading\n```", Section: "Install > Binary"},
		{Text: "Call the API.", Section: "Usage"},
	}
	if len(blocks) != len(want) {
		t.Fatalf("blocks = %+v", blocks)
	}
	for i := range want {
		if blocks[i] != want[i] {
			t.Errorf("block %d = %+v, want %+v", i, blocks[i], want[i])
		}
	}
	// Deep hierarchies keep only the last two levels.
	deep, _ := ExtractBlocks(context.Background(), "d.md", []byte("# A\n## B\n### C\n\ntext"))
	if len(deep) != 1 || deep[0].Section != "B > C" {
		t.Errorf("deep section: %+v", deep)
	}
	// ExtractText still returns the joined plain text.
	if txt, _ := ExtractText(context.Background(), "guide.md", []byte(md)); !strings.Contains(txt, "Intro paragraph.\n\nRun the container.") {
		t.Errorf("ExtractText: %q", txt)
	}
	if blocks, _ := ExtractBlocks(context.Background(), "notes.txt", []byte("one\n\ntwo")); len(blocks) != 2 || blocks[1].Text != "two" || blocks[0].Section != "" {
		t.Errorf("txt blocks: %+v", blocks)
	}
}

// buildDOCX assembles a minimal OOXML package around the given body XML.
func buildDOCX(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml":   `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body + `</w:body></w:document>`,
	}
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

func docxP(style, text string) string {
	ppr := ""
	if style != "" {
		ppr = `<w:pPr><w:pStyle w:val="` + style + `"/></w:pPr>`
	}
	return `<w:p>` + ppr + `<w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

func TestExtractDOCX(t *testing.T) {
	body := docxP("Title", "Handbook") +
		docxP("Heading1", "Install") +
		docxP("", "Intro ") + // one paragraph, two runs
		strings.Replace(docxP("", "para"), `<w:p>`, `<w:p><w:r><w:t>first run, </w:t></w:r>`, 1) +
		docxP("Heading2", "Docker") +
		docxP("", "Run the container.") +
		`<w:tbl><w:tr><w:tc>` + docxP("", "Flag") + `</w:tc><w:tc>` + docxP("", "Meaning") + `</w:tc></w:tr>` +
		`<w:tr><w:tc>` + docxP("", "-d") + `</w:tc><w:tc>` + docxP("", "detach") + docxP("", "from tty") + `</w:tc></w:tr></w:tbl>` +
		docxP("heading 1", "Usage") +
		docxP("", "Call it.")
	data := buildDOCX(t, body)
	if !SniffOK("h.docx", data) || SniffOK("h.docx", []byte("<html>")) {
		t.Error("docx sniffing")
	}
	blocks, err := ExtractBlocks(context.Background(), "h.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	want := []Block{
		{Text: "Intro", Section: "Handbook > Install"},
		{Text: "first run, para", Section: "Handbook > Install"},
		{Text: "Run the container.", Section: "Install > Docker"},
		{Text: "Flag | Meaning", Section: "Install > Docker"},
		{Text: "-d | detach from tty", Section: "Install > Docker"},
		{Text: "Call it.", Section: "Handbook > Usage"},
	}
	if len(blocks) != len(want) {
		t.Fatalf("blocks = %+v", blocks)
	}
	for i := range want {
		if blocks[i] != want[i] {
			t.Errorf("block %d = %+v, want %+v", i, blocks[i], want[i])
		}
	}
	if _, err := ExtractBlocks(context.Background(), "x.docx", []byte("not a zip")); err == nil {
		t.Error("expected error for non-zip docx")
	}
}

func TestExtractHTML(t *testing.T) {
	page := `<!doctype html><html><head><title> My  Page </title><style>p{color:red}</style><script>var x=1;</script></head>
	<body><nav><a href="/">Home</a><a href="/about">About</a></nav><header>Site header</header>
	<h1>Install</h1><p>Intro   paragraph<br>with break.</p>
	<h2>Docker</h2><div>Run the <b>container</b>.</div><ul><li>step one</li><li>step two</li></ul>
	<table><tr><td>Flag</td><td>Meaning</td></tr></table>
	<pre>line 1
line 2</pre><svg><text>icon</text></svg><noscript>enable js</noscript>
	<h1>Usage</h1><blockquote>Quoted.</blockquote><footer>footer text</footer></body></html>`
	p, err := Extract(context.Background(), "page.html", []byte(page))
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "My Page" {
		t.Errorf("title = %q", p.Title)
	}
	want := []Block{
		{Text: "Intro paragraph\nwith break.", Section: "Install"},
		{Text: "Run the container.", Section: "Install > Docker"},
		{Text: "step one", Section: "Install > Docker"},
		{Text: "step two", Section: "Install > Docker"},
		{Text: "Flag", Section: "Install > Docker"},
		{Text: "Meaning", Section: "Install > Docker"},
		{Text: "line 1\nline 2", Section: "Install > Docker"},
		{Text: "Quoted.", Section: "Usage"},
	}
	if len(p.Blocks) != len(want) {
		t.Fatalf("blocks = %+v", p.Blocks)
	}
	for i := range want {
		if p.Blocks[i] != want[i] {
			t.Errorf("block %d = %+v, want %+v", i, p.Blocks[i], want[i])
		}
	}
	joined := JoinBlocks(p.Blocks)
	for _, bad := range []string{"Home", "About", "Site header", "var x", "color:red", "icon", "enable js", "footer text"} {
		if strings.Contains(joined, bad) {
			t.Errorf("boilerplate %q leaked into blocks", bad)
		}
	}
	if !IsSupported("a.htm") || !IsSupported("A.DOCX") || IsSupported("a.exe") {
		t.Error("IsSupported")
	}
}

// buildPDF writes a minimal single-font PDF with one page per text.
func buildPDF(t *testing.T, pages ...string) []byte {
	t.Helper()
	var objs []string
	kids := ""
	for i := range pages {
		kids += fmt.Sprintf("%d 0 R ", 4+2*i)
	}
	objs = append(objs,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.TrimSpace(kids), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	)
	for i, text := range pages {
		content := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		)
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return buf.Bytes()
}

func TestExtractPDFPages(t *testing.T) {
	data := buildPDF(t, "Hello page one", "Second page here")
	blocks, err := ExtractBlocks(context.Background(), "doc.pdf", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[0].Page != 1 || !strings.Contains(blocks[0].Text, "Hello page one") {
		t.Errorf("page 1: %+v", blocks[0])
	}
	if blocks[1].Page != 2 || !strings.Contains(blocks[1].Text, "Second page here") {
		t.Errorf("page 2: %+v", blocks[1])
	}
	if _, err := ExtractBlocks(context.Background(), "x.pdf", []byte("%PDF-1.4 garbage")); err == nil {
		t.Error("expected error for a broken pdf")
	}
}

func TestExtractDOCXRejectsBombs(t *testing.T) {
	ctx := context.Background()
	// A header that declares a huge document.xml is rejected before reading.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "word/document.xml", Method: zip.Store,
		UncompressedSize64: 40 << 20, CompressedSize64: 5, CRC32: 0})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("<w:p>"))
	zw.Close()
	if _, err := ExtractBlocks(ctx, "big.docx", buf.Bytes()); err == nil || !strings.Contains(err.Error(), "limit is 32 MiB") {
		t.Errorf("declared size: err = %v", err)
	}
	// Highly compressible content (ratio > 100) is rejected too.
	body := docxP("", strings.Repeat("a", 2<<20))
	if _, err := ExtractBlocks(ctx, "ratio.docx", buildDOCX(t, body)); err == nil || !strings.Contains(err.Error(), "compression ratio") {
		t.Errorf("ratio: err = %v", err)
	}
	// A normal document still parses.
	if _, err := ExtractBlocks(ctx, "ok.docx", buildDOCX(t, docxP("", "hello world"))); err != nil {
		t.Errorf("normal: %v", err)
	}
}

func TestExtractTextCap(t *testing.T) {
	ctx := context.Background()
	big := make([]byte, maxExtractedText+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := ExtractBlocks(ctx, "big.txt", big); !errors.Is(err, ErrTooMuchText) {
		t.Errorf("txt: %v", err)
	}
	if _, err := ExtractBlocks(ctx, "big.md", big); !errors.Is(err, ErrTooMuchText) {
		t.Errorf("md: %v", err)
	}
	html := append([]byte("<p>"), big...)
	if _, err := ExtractBlocks(ctx, "big.html", html); !errors.Is(err, ErrTooMuchText) {
		t.Errorf("html: %v", err)
	}
}

func TestExtractPDFHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractBlocks(ctx, "doc.pdf", buildPDF(t, "Hello")); err == nil {
		t.Error("cancelled context must abort pdf parsing")
	}
}
