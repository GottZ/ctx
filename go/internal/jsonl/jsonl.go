// Package jsonl reads JSON-Lines documents: one JSON value per line, every
// error addressed by its 1-based line number.
//
// Twelve readers in goldset, armsweep and goldbench used to carry their own
// copy of the same loop — read, split on "\n", skip the blank ones, unmarshal,
// prefix the failure with "<file>:<line>: ". Twelve copies of a loop are twelve
// places where a rule can drift apart without a test noticing, so the loop
// lives here once and the callers keep only what actually differs between them:
// which type a line becomes, and what they do with it.
//
// The core takes an io.Reader rather than a path because three of those callers
// do not have a file to hand: goldset.AssertSheetBlind checks a sheet that has
// just been rendered and never written, goldset.ParseJudgements has already
// read the bytes to sniff Markdown against JSONL, and armsweep reads its dumps
// through a gzip.Reader.
//
// # Boundary rule
//
// jsonl reads LINES and addresses errors by line number. Whoever addresses
// RECORDS (json.Decoder, multi-line values), carries a byte offset, or has to
// tolerate a torn final line is in the wrong place here. The two exceptions in
// this tree are named, so the next sweep need not re-derive them:
// internal/armsweep/stream.go (record counting, gzip, sortedness gate) and
// internal/goldbench/runner.go loadDumpDone (byte-offset resume).
package jsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrLineTooLong reports a line beyond the MaxLine cap.
//
// It IS bufio.ErrTooLong, deliberately: goldbench.LoadCases capped its lines
// with a bufio.Scanner before this package existed and reported the scanner's
// wording. Reusing the sentinel keeps that operator-visible text byte-identical
// instead of inventing a second phrase for the same refusal.
var ErrLineTooLong = bufio.ErrTooLong

// LineError names the line a value could not be parsed from.
//
// Name is what the caller calls the document — a path, usually. It is only ever
// part of the text; the reader does not open, stat or otherwise touch it. A
// caller that has no meaningful name (a freshly rendered buffer) passes "" and
// gets the short form.
type LineError struct {
	Name string
	Line int
	Err  error
}

// Error renders "<name>:<line>: <err>", or "Zeile <line>: <err>" without a name.
func (e *LineError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("Zeile %d: %s", e.Line, e.Err)
	}
	return fmt.Sprintf("%s:%d: %s", e.Name, e.Line, e.Err)
}

// Unwrap exposes the underlying json error to errors.Is and errors.As.
func (e *LineError) Unwrap() error { return e.Err }

// opts is the resolved option set of one read.
type opts struct {
	trimBlank bool
	trimCR    bool
	maxLine   int
}

// Opt tunes a read. The zero configuration is the one eleven of the twelve
// call sites want, because it is what strings.Split(string(b), "\n") gave
// them: skip whitespace-only lines, hand the rest on untouched, no length cap.
type Opt func(*opts)

// SkipBlank picks which lines count as blank and are handed to no one.
//
// true (the default) skips a line that is empty after TrimSpace; false skips
// only the truly empty line and lets a whitespace-only one reach the parser,
// where it fails. That is not a detail: goldbench's case files are strict on
// purpose, and quietly widening the skip would turn one of its errors into a
// success.
func SkipBlank(trim bool) Opt { return func(o *opts) { o.trimBlank = trim } }

// TrimCR drops a carriage return in front of the line terminator, the way
// bufio.ScanLines does.
//
// false (the default) is strings.Split(…, "\n") behaviour and hands the line
// on exactly as it stood in the document — a stray CR included. That matters
// twice over: a line consisting only of CR is blank either way (TrimSpace sees
// to it), but a MALFORMED line whose last byte is a CR gets a different
// complaint out of encoding/json depending on whether the CR is still there.
// Eleven call sites split by hand and keep it; goldbench.LoadCases read
// through a bufio.Scanner and wants true.
func TrimCR(trim bool) Opt { return func(o *opts) { o.trimCR = trim } }

// MaxLine caps how much one line may cost, terminator included; 0 means no cap.
//
// The cap is what a bufio.Scanner would have been given as its maximum token
// size, and that buffer had to hold the newline too — so a line of exactly n
// bytes is ALREADY too long, refused with ErrLineTooLong. The read stops at
// the cap rather than after assembling the whole line.
func MaxLine(n int) Opt { return func(o *opts) { o.maxLine = n } }

// EachReader is the core: it reads r line by line, parses every line that is
// not skipped into T and calls fn with the 1-based line number.
//
// Blank lines are counted even though they are not parsed, so the number in an
// error is the number an editor shows. fn's error ends the read and is returned
// unchanged — a caller that wants its own wording around a row simply writes
// it. A line that will not parse yields a *LineError, which reads exactly like
// the fmt.Errorf("%s:%d: %w", …) the call sites used to write by hand.
func EachReader[T any](r io.Reader, name string, fn func(line int, v T) error, o ...Opt) error {
	cfg := opts{trimBlank: true}
	for _, apply := range o {
		apply(&cfg)
	}
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, last, err := readLine(br, cfg)
		if err != nil {
			return err
		}
		// Ein Dokument mit Schluss-Newline endet auf einer leeren letzten
		// Zeile, die es gar nicht gibt — die wird nicht mitgezählt.
		if last && len(line) == 0 {
			return nil
		}
		if !blank(line, cfg.trimBlank) {
			var v T
			if uerr := json.Unmarshal(line, &v); uerr != nil {
				return &LineError{Name: name, Line: n, Err: uerr}
			}
			if ferr := fn(n, v); ferr != nil {
				return ferr
			}
		}
		if last {
			return nil
		}
	}
}

// Each opens path and runs EachReader over it, naming errors after the path.
func Each[T any](path string, fn func(line int, v T) error, o ...Opt) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return EachReader(f, path, fn, o...)
}

// All reads path into a slice. It returns a nil slice for a document without a
// single parsed line, the way a hand-written append loop does.
func All[T any](path string, o ...Opt) ([]T, error) {
	var out []T
	if err := Each(path, func(_ int, v T) error {
		out = append(out, v)
		return nil
	}, o...); err != nil {
		return nil, err
	}
	return out, nil
}

// blank decides whether a line is skipped.
func blank(line []byte, trim bool) bool {
	if trim {
		return len(bytes.TrimSpace(line)) == 0
	}
	return len(line) == 0
}

// readLine returns the next line with its "\n" removed — and, under TrimCR,
// a "\r" in front of it as well, at the end of the document too. The returned
// slice may point into the reader's buffer and is only valid until the next
// call — every caller here unmarshals it before reading on, and encoding/json
// copies what it keeps.
//
// The second result says whether the reader hit the end of the document on
// this line. The end is not an error here, so it does not travel as one: only
// ErrLineTooLong and whatever r itself reported come back in the third.
//
// The cap is measured BEFORE the CR is dropped, because that is the byte count
// a bufio.Scanner had to fit into its buffer.
func readLine(br *bufio.Reader, cfg opts) ([]byte, bool, error) {
	frag, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		// Longer than one buffer: assemble, but stop as soon as the cap is
		// reached rather than once the whole line is in memory. Everything
		// gathered here is free of "\n" — that is what ErrBufferFull means.
		buf := append([]byte(nil), frag...)
		for errors.Is(err, bufio.ErrBufferFull) {
			if cfg.maxLine > 0 && len(buf) >= cfg.maxLine {
				return nil, false, ErrLineTooLong
			}
			frag, err = br.ReadSlice('\n')
			buf = append(buf, frag...)
		}
		frag = buf
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	line := trimSuffixByte(frag, '\n')
	if cfg.maxLine > 0 && len(line) >= cfg.maxLine {
		return nil, false, ErrLineTooLong
	}
	if cfg.trimCR {
		line = trimSuffixByte(line, '\r')
	}
	return line, err != nil, nil
}

// trimSuffixByte drops c from the end of b if it is there.
func trimSuffixByte(b []byte, c byte) []byte {
	if n := len(b); n > 0 && b[n-1] == c {
		return b[:n-1]
	}
	return b
}
