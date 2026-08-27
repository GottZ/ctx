package armsweep

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Streaming dump access (design/05 §6.1, wave M-W3d).
//
// A campaign dump of the 950-case gold set is ≈ 68 MB of JSONL, and `compare`
// holds FOUR of them at once (base, cond and the two halves of the replicate
// pair) — 0,5–1 GB RSS if they are read in. Every figure the comparison
// computes is paired PER QUERY, so nothing needs two records of one dump at the
// same time: the artefact is written compressed and read one record at a time.

// GzipSuffix is the extension that switches both the writer and the reader to
// gzip. Extension-driven rather than a flag on either side, so a dump cannot
// end up with a name that lies about its content.
const GzipSuffix = ".gz"

// streamBufferBytes is the read-ahead of one dump stream. Large enough that a
// 305-candidate record is one buffer refill, small enough that four open
// streams cost single-digit megabytes.
const streamBufferBytes = 1 << 20

// ErrDumpUnsorted refuses a dump whose case keys do not ascend.
//
// The merge below walks four streams in lockstep and advances the one holding
// the smallest key; that is only a pairing if every stream is sorted.
// WriteRecords sorts by case key (dump.go), so a dump that fails this check was
// not written by this instrument — and pairing it by position would compare
// case i of one run against a different case of another, silently.
var ErrDumpUnsorted = errors.New("Dump ist nicht nach Fall-Schlüssel sortiert")

// RecordStream reads a dump record by record, transparently un-gzipping.
type RecordStream struct {
	path string
	file *os.File
	gz   *gzip.Reader
	dec  *json.Decoder
	last string
	n    int
}

// OpenRecordStream opens a dump for streaming.
func OpenRecordStream(path string) (*RecordStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	s := &RecordStream{path: path, file: f}
	var r io.Reader = bufio.NewReaderSize(f, streamBufferBytes)
	if strings.HasSuffix(path, GzipSuffix) {
		zr, zerr := gzip.NewReader(r)
		if zerr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("%s: %w", path, zerr)
		}
		s.gz, r = zr, zr
	}
	s.dec = json.NewDecoder(r)
	return s, nil
}

// Next decodes the next record. The second return value is false at end of
// file — the caller distinguishes "done" from "failed" without comparing to
// io.EOF, which a json.Decoder can also return mid-value.
func (s *RecordStream) Next() (Record, bool, error) {
	var rec Record
	if err := s.dec.Decode(&rec); err != nil {
		if errors.Is(err, io.EOF) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("%s: Datensatz %d: %w", s.path, s.n+1, err)
	}
	key := rec.Key()
	if s.n > 0 && key <= s.last {
		return Record{}, false, fmt.Errorf("%w: %s: %q folgt auf %q", ErrDumpUnsorted, s.path, key, s.last)
	}
	s.last, s.n = key, s.n+1
	return rec, true, nil
}

// Count is the number of records handed out so far.
func (s *RecordStream) Count() int { return s.n }

// Close releases the decompressor and the file.
func (s *RecordStream) Close() error {
	if s.gz != nil {
		if err := s.gz.Close(); err != nil {
			_ = s.file.Close()
			return err
		}
	}
	return s.file.Close()
}

// readRecordsStreamed collects a compressed dump through the stream. The
// sortedness check applies here too: a dump the merge would refuse must not
// slip in through the whole-file door.
func readRecordsStreamed(path string) ([]Record, error) {
	s, err := OpenRecordStream(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	var out []Record
	for {
		rec, ok, err := s.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, rec)
	}
}

// RecordWriter appends records to an open sink, compressing when the artefact
// name carries GzipSuffix. It exists so a 290 000-record dump can be produced
// without holding 290 000 records — WriteRecords sorts and therefore cannot.
type RecordWriter struct {
	gz  *gzip.Writer
	buf *bufio.Writer
	enc *json.Encoder
	n   int
}

// NewRecordWriter wraps a sink; name decides whether the bytes are compressed.
func NewRecordWriter(w io.Writer, name string) (*RecordWriter, error) {
	out := &RecordWriter{}
	sink := w
	if strings.HasSuffix(name, GzipSuffix) {
		out.gz = gzip.NewWriter(w)
		sink = out.gz
	}
	out.buf = bufio.NewWriterSize(sink, streamBufferBytes)
	out.enc = json.NewEncoder(out.buf)
	return out, nil
}

// Write appends one record as a JSONL line.
func (w *RecordWriter) Write(rec Record) error {
	if err := w.enc.Encode(rec); err != nil {
		return fmt.Errorf("encode record %d: %w", w.n+1, err)
	}
	w.n++
	return nil
}

// Close flushes the buffer and finishes the gzip member. It does NOT close the
// underlying sink — the opener owns it.
func (w *RecordWriter) Close() error {
	if err := w.buf.Flush(); err != nil {
		return err
	}
	if w.gz != nil {
		return w.gz.Close()
	}
	return nil
}
