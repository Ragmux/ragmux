package rag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary double as the PDF worker: extractPDF runs
// os.Args[0] with "pdf-extract" when PDFWorker points at it.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "pdf-extract" {
		os.Exit(RunPDFWorker(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// assemblePDF serialises numbered objects (1-based) with a classic xref
// table. "%XREF%" in trailerExtra is replaced by the xref offset.
func assemblePDF(objs []string, trailerExtra string) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.5\n")
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
	trailerExtra = strings.ReplaceAll(trailerExtra, "%XREF%", fmt.Sprint(xref))
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R %s>>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, trailerExtra, xref)
	return buf.Bytes()
}

// pageObjs is a one-page document (objects 1..5) around the given /Pages
// dictionary; the page is object 4 with its own resources.
func pageObjs(pagesDict string) []string {
	content := "BT /F1 12 Tf 72 720 Td (Hello) Tj ET"
	return []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		pagesDict,
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
}

// xrefStreamPDF writes a PDF 1.5 file whose cross-reference is an
// uncompressed xref stream (W [1 4 2]) covering objects 0..len(entries)-1.
// entries[i] is the raw 7-byte entry for object i; nil means "at its
// offset in the file" for objects in objs and "free" otherwise.
func xrefStreamPDF(objs map[int]string, entries [][]byte, rootRef string) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.5\n")
	offsets := map[int]int{}
	for id, obj := range objs {
		offsets[id] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", id, obj)
	}
	var data bytes.Buffer
	for i, e := range entries {
		if e == nil {
			e = make([]byte, 7)
			if off, ok := offsets[i]; ok {
				e[0] = 1
				e[1], e[2], e[3], e[4] = byte(off>>24), byte(off>>16), byte(off>>8), byte(off)
			}
		}
		data.Write(e)
	}
	xrefID := len(entries)
	xrefOff := buf.Len()
	fmt.Fprintf(&buf, "%d 0 obj\n<< /Type /XRef /Size %d /W [1 4 2] /Root %s /Length %d >>\nstream\n", xrefID, xrefID, rootRef, data.Len())
	buf.Write(data.Bytes())
	fmt.Fprintf(&buf, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xrefOff)
	return buf.Bytes()
}

// inStream is an xref stream entry placing an object at index idx of
// object stream strm.
func inStream(strm, idx int) []byte {
	return []byte{2, byte(strm >> 24), byte(strm >> 16), byte(strm >> 8), byte(strm), byte(idx >> 8), byte(idx)}
}

// objStmSelfPDF declares that object 6 (an object stream) lives inside
// object stream 6, so resolving anything in it recurses without end.
func objStmSelfPDF() []byte {
	body := "1 0 << /Type /Catalog /Pages 2 0 R >>"
	objs := map[int]string{
		2: "<< /Type /Pages /Kids [] /Count 0 >>",
		6: fmt.Sprintf("<< /Type /ObjStm /N 1 /First 4 /Length %d >>\nstream\n%s\nendstream", len(body), body),
	}
	entries := make([][]byte, 7)
	entries[1] = inStream(6, 0)
	entries[6] = inStream(6, 1)
	return xrefStreamPDF(objs, entries, "1 0 R")
}

// objStmExtendsLoopPDF claims object 1 is in object stream 6, which does
// not hold it and whose /Extends points back to itself.
func objStmExtendsLoopPDF() []byte {
	body := "9 0 << /Foo /Bar >>"
	objs := map[int]string{
		2: "<< /Type /Pages /Kids [] /Count 0 >>",
		6: fmt.Sprintf("<< /Type /ObjStm /N 1 /First 4 /Extends 6 0 R /Length %d >>\nstream\n%s\nendstream", len(body), body),
	}
	entries := make([][]byte, 7)
	entries[1] = inStream(6, 0)
	return xrefStreamPDF(objs, entries, "1 0 R")
}

// malformedPDFs are crafted inputs that must yield an error or a bounded
// result: never a panic, a crash or work that outlives the deadline.
// Cases that hang or crash the library in-process are listed separately in
// unboundedPDFs.
func malformedPDFs(t *testing.T) map[string][]byte {
	t.Helper()
	good := buildPDF(t, "Hello page one", "Second page here")
	cut := func(marker string, keep int) []byte {
		at := bytes.LastIndex(good, []byte(marker)) + keep
		tail := good[bytes.LastIndex(good, []byte("trailer")):]
		return append(append([]byte{}, good[:at]...), tail...)
	}
	objs := pageObjs("<< /Type /Pages /Kids [4 0 R] /Count 1 >>")
	contentsDict := append([]string{}, objs...)
	contentsDict[3] = "<< /Type /Page /Parent 2 0 R /Contents 2 0 R >>"
	return map[string][]byte{
		"garbage":          []byte("%PDF-1.4 garbage"),
		"truncated-xref":   cut("xref", 30),
		"no-xref":          cut("xref", 0),
		"truncated-stream": cut("endstream", -5),
		"bad-startxref":    append(append([]byte{}, good[:bytes.LastIndex(good, []byte("startxref"))]...), "startxref\n99999999\n%%EOF\n"...),
		"zero-pages":       assemblePDF(pageObjs("<< /Type /Pages /Kids [] /Count 0 >>"), ""),
		"huge-count":       assemblePDF(pageObjs("<< /Type /Pages /Kids [4 0 R] /Count 2000000000 >>"), ""),
		"negative-count":   assemblePDF(pageObjs("<< /Type /Pages /Kids [4 0 R] /Count -5 >>"), ""),
		"kids-not-array":   assemblePDF(pageObjs("<< /Type /Pages /Kids 4 0 R /Count 1 >>"), ""),
		"contents-is-dict": assemblePDF(contentsDict, ""),
		"deep-array":       assemblePDF(pageObjs("<< /Type /Pages /Kids [4 0 R] /Count 1 /X "+strings.Repeat("[", 5000)+strings.Repeat("]", 5000)+" >>"), ""),
		"pages-self-only":  assemblePDF(pageObjs("<< /Type /Pages /Kids [2 0 R] /Count 1 >>"), ""),
		"pages-self-kid":   assemblePDF(pageObjs("<< /Type /Pages /Kids [2 0 R 4 0 R] /Count 2 >>"), ""),
		"pages-fanout":     assemblePDF(pageObjs("<< /Type /Pages /Kids ["+strings.Repeat("2 0 R ", 50)+"4 0 R] /Count 2 >>"), ""),
		"parent-cycle":     parentCyclePDF(),
	}
}

// parentCyclePDF is a page without /Resources whose /Parent chain loops.
func parentCyclePDF() []byte {
	objs := pageObjs("<< /Type /Pages /Kids [4 0 R] /Count 1 /Parent 4 0 R >>")
	objs[3] = "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R >>"
	return assemblePDF(objs, "")
}

// unboundedPDFs make the library recurse or loop without bound before any
// page is reached; only the worker process can contain them.
func unboundedPDFs() map[string][]byte {
	return map[string][]byte{
		"objstm-self":    objStmSelfPDF(),
		"objstm-extends": objStmExtendsLoopPDF(),
		"prev-loop":      assemblePDF(pageObjs("<< /Type /Pages /Kids [4 0 R] /Count 1 >>"), "/Prev %XREF% "),
	}
}

func TestExtractPDFMalformed(t *testing.T) {
	for name, data := range malformedPDFs(t) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			p, err := extractPDFPages(ctx, data)
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("took %s", took)
			}
			if err != nil {
				if ctx.Err() != nil {
					t.Errorf("hung until the deadline: %v", err)
				}
				return
			}
			if len(p.Blocks) == 0 || p.PageCount < 1 {
				t.Errorf("no error but nothing parsed: %+v", p)
			}
			if p.PageCount > maxPDFTreeNodes {
				t.Errorf("page_count %d not bounded", p.PageCount)
			}
			for _, b := range p.Blocks {
				if b.Page < 1 || b.Page > maxPDFPages {
					t.Errorf("block page %d out of range", b.Page)
				}
			}
		})
	}
}

func TestExtractPDFPageTree(t *testing.T) {
	ctx := context.Background()
	// /Count is not trusted: the tree has one page.
	p, err := extractPDFPages(ctx, assemblePDF(pageObjs("<< /Type /Pages /Kids [4 0 R] /Count 2000000000 >>"), ""))
	if err != nil || p.PageCount != 1 || len(p.Blocks) != 1 || p.Blocks[0].Page != 1 {
		t.Errorf("huge count: %+v %v", p, err)
	}
	// Nested /Pages nodes keep document order and page numbers.
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>",
		"<< /Type /Pages /Parent 2 0 R /Kids [4 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 3 0 R /Resources << /Font << /F1 8 0 R >> >> /Contents 5 0 R >>",
		"<< /Length 36 >>\nstream\nBT /F1 12 Tf 72 720 Td (One) Tj ET\nendstream",
		"<< /Type /Page /Parent 2 0 R /Contents 7 0 R >>", // resources inherited from the root
		"<< /Length 36 >>\nstream\nBT /F1 12 Tf 72 720 Td (Two) Tj ET\nendstream",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	objs[1] = "<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 /Resources << /Font << /F1 8 0 R >> >> >>"
	p, err = extractPDFPages(ctx, assemblePDF(objs, ""))
	if err != nil {
		t.Fatal(err)
	}
	if p.PageCount != 2 || len(p.Blocks) != 2 || p.Blocks[0].Text != "One" || p.Blocks[0].Page != 1 || p.Blocks[1].Text != "Two" || p.Blocks[1].Page != 2 {
		t.Errorf("nested tree: %+v", p)
	}
	// A page whose /Parent chain loops is skipped rather than walked forever.
	if _, err := extractPDFPages(ctx, parentCyclePDF()); err == nil || !strings.Contains(err.Error(), "no extractable text") {
		t.Errorf("parent cycle: %v", err)
	}
	// A page with more than maxPDFPageText of text is rejected.
	line := "BT /F1 12 Tf 72 720 Td (" + strings.Repeat("x", 4000) + ") Tj ET\n"
	content := strings.Repeat(line, maxPDFPageText/4000+2)
	objs = pageObjs("<< /Type /Pages /Kids [4 0 R] /Count 1 >>")
	objs[4] = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)
	if _, err := extractPDFPages(ctx, assemblePDF(objs, "")); err == nil || !strings.Contains(err.Error(), "more than 2 MiB of text") {
		t.Errorf("page text cap: %v", err)
	}
}

func TestExtractPDFTestdata(t *testing.T) {
	files, err := filepath.Glob("testdata/*.pdf")
	if err != nil || len(files) < 2 {
		t.Fatalf("testdata: %v %v", files, err)
	}
	want := map[string][]string{
		"mixed-fonts.pdf":     {"Page one heading in Helvetica", "café naïve", "SELECT id, name FROM users", "- alpha - beta - gamma"},
		"macos-texttopdf.pdf": {"Plain page one: alpha beta gamma.", "Plain page two: delta epsilon.", "Plain page three: zeta eta theta."},
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Extract(context.Background(), f, data)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if p.PageCount != 3 {
			t.Errorf("%s: page_count = %d", f, p.PageCount)
		}
		joined := JoinBlocks(p.Blocks)
		for _, s := range want[filepath.Base(f)] {
			if !strings.Contains(joined, s) {
				t.Errorf("%s: missing %q in %q", f, s, joined)
			}
		}
		if last := p.Blocks[len(p.Blocks)-1].Page; last != 3 {
			t.Errorf("%s: last block on page %d", f, last)
		}
	}
}

// withTestWorker points PDFWorker at the test binary for the test's duration.
func withTestWorker(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path:", err)
	}
	PDFWorker = []string{exe, "pdf-extract"}
	t.Cleanup(func() { PDFWorker = nil })
}

func TestExtractPDFWorker(t *testing.T) {
	withTestWorker(t)
	ctx := context.Background()
	// A good document round-trips through the worker with page numbers.
	p, err := Extract(ctx, "doc.pdf", buildPDF(t, "Hello page one", "Second page here"))
	if err != nil {
		t.Fatal(err)
	}
	if p.PageCount != 2 || len(p.Blocks) != 2 || p.Blocks[1].Page != 2 || p.Blocks[1].Text != "Second page here" {
		t.Errorf("worker result: %+v", p)
	}
	// Parse errors come back as errors, not crashes.
	if _, err := Extract(ctx, "x.pdf", []byte("%PDF-1.4 garbage")); err == nil || !strings.Contains(err.Error(), "open pdf") {
		t.Errorf("garbage: %v", err)
	}
	// A library stack overflow kills only the worker.
	if _, err := Extract(ctx, "x.pdf", objStmSelfPDF()); err == nil || !strings.Contains(err.Error(), "pdf parser crashed") {
		t.Errorf("objstm-self: %v", err)
	}
	// A cancelled context ends the worker.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Extract(cctx, "doc.pdf", buildPDF(t, "Hello")); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	// A missing worker binary is an error, not a fallback that hides it.
	PDFWorker = []string{filepath.Join(t.TempDir(), "missing"), "pdf-extract"}
	if _, err := Extract(ctx, "doc.pdf", buildPDF(t, "Hello")); err == nil {
		t.Error("missing worker: expected error")
	}
}

// TestExtractPDFWorkerDeadline checks that the worker is killed when the
// parse deadline passes on an input that loops forever in the library.
func TestExtractPDFWorkerDeadline(t *testing.T) {
	withTestWorker(t)
	for name, data := range unboundedPDFs() {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			_, err := extractPDF(ctx, data)
			if err == nil {
				t.Fatal("expected an error")
			}
			if time.Since(start) > 4*time.Second {
				t.Errorf("worker outlived the deadline: %v", err)
			}
			t.Logf("%s: %v", name, err)
		})
	}
}

// FuzzExtractPDF runs the in-process parser on mutated PDFs: it must return
// a result or an error, never let a panic escape. Run with
// go test ./internal/rag -run '^$' -fuzz FuzzExtractPDF -fuzztime 60s
func FuzzExtractPDF(f *testing.F) {
	t := &testing.T{}
	f.Add(buildPDF(t, "Hello page one", "Second page here"))
	f.Add(buildPDF(t))
	for _, data := range malformedPDFs(t) {
		f.Add(data)
	}
	files, _ := filepath.Glob("testdata/*.pdf")
	for _, file := range files {
		if data, err := os.ReadFile(file); err == nil {
			f.Add(data)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := extractPDFPages(ctx, data)
		if err != nil {
			return
		}
		if len(p.Blocks) == 0 || p.PageCount < 1 || p.PageCount > maxPDFTreeNodes {
			t.Errorf("inconsistent result: %d blocks, page_count %d", len(p.Blocks), p.PageCount)
		}
		total := 0
		for _, b := range p.Blocks {
			total += len(b.Text)
			if b.Page < 1 || b.Page > maxPDFPages || len(b.Text) > maxPDFPageText {
				t.Errorf("block out of bounds: page %d, %d bytes", b.Page, len(b.Text))
			}
		}
		if total > maxExtractedText {
			t.Errorf("total text %d exceeds the cap", total)
		}
	})
}
