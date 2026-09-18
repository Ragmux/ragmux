package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime/debug"
	"strings"
)

// PDFWorker is the command that parses PDFs out of process: the gateway's
// own executable followed by the pdf-extract subcommand. The main package
// sets it at startup; when it is empty (tests, library use) PDFs are parsed
// in-process.
//
// The PDF library recurses without bound on some crafted inputs (an object
// stream whose cross-reference entry says it lives inside itself), and a Go
// stack overflow is fatal rather than a recoverable panic. It also loops
// forever on others, past any timeout, because a goroutine cannot be
// interrupted. In a child process both become an error: the parent kills
// the child when the deadline passes and reports a crash as a failed parse.
var PDFWorker []string

// pdfWorkerOutput is the JSON the worker writes on success.
type pdfWorkerOutput struct {
	Blocks    []Block `json:"blocks"`
	PageCount int     `json:"page_count"`
}

// pdfWorkerMaxStack bounds the worker's goroutine stacks so runaway
// recursion fails fast instead of growing towards the 1 GB default.
const pdfWorkerMaxStack = 256 << 20

// RunPDFWorker is the body of the pdf-extract subcommand: it reads a PDF
// from stdin, writes the extracted blocks as JSON to stdout and returns the
// exit code (1 with the message on stderr when the parse fails).
func RunPDFWorker(stdin io.Reader, stdout, stderr io.Writer) int {
	debug.SetMaxStack(pdfWorkerMaxStack)
	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return fail(fmt.Errorf("read stdin: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), pdfTimeout)
	defer cancel()
	p, err := extractPDFPages(ctx, data)
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(stdout).Encode(pdfWorkerOutput{Blocks: p.Blocks, PageCount: p.PageCount}); err != nil {
		return fail(fmt.Errorf("encode: %w", err))
	}
	return 0
}

// extractPDFExternal runs PDFWorker on data and decodes its output. ctx
// already carries the parse deadline; when it passes the child is killed.
func extractPDFExternal(ctx context.Context, data []byte) (*Parsed, error) {
	cmd := exec.CommandContext(ctx, PDFWorker[0], PDFWorker[1:]...) // #nosec G204 -- PDFWorker is set by main to the gateway's own executable, not by input
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("pdf parse exceeded %s", pdfTimeout)
		}
		return nil, ctx.Err()
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 512 {
			msg = msg[:512]
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("pdf parser crashed (%v): %s", err, firstLine(msg))
	}
	var out pdfWorkerOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("pdf parser output: %w", err)
	}
	return &Parsed{Blocks: out.Blocks, PageCount: out.PageCount}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
