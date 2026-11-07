package embed

// The decode of a gateway sub-block's answer (EmbedRaw; W02-09 hardening 6
// after the bot reviews of PR #157). The read cap bounds the bytes of an
// answer, not what they decode into: two bytes "{}" became a 32-byte entry,
// "[]" a 24-byte vector header, and an answer of 8 MiB of them allocated
// ≈ 85 MiB past the route's decode share. This decode reads the answer's
// object into one buffer that grows towards the cap (rawAnswerBuffer) and
// walks its entries in place: the first entry past the inputs is refused
// before it is decoded (E-1), an entry without index — missing or null — is
// refused (E-2), a null component is refused before it could read as 0
// (E-3), and a vector is allocated once, at its exact length, after its
// entry's index and its length were checked. The internal path keeps its
// decode (TestInternalEmbedBitIdenticalToBase pins its vectors and error
// texts, an index-less answer included).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"unsafe"
)

// errSurplusEmbeddings: the answer has an entry past the inputs.
var errSurplusEmbeddings = errors.New("more embeddings than inputs")

// maxIndexLiteral is the longest integer literal an index can be: an int64
// (encoding/json reads an int at 64 bits); a longer literal is no index.
const maxIndexLiteral = len("-9223372036854775808")

// rawAnswerMinStep is the smallest buffer step of an answer whose length the
// header does not name: steps are the read cap halved down to here.
const rawAnswerMinStep = 4 << 10

// rawAnswer is how a wire call reads its answer: strict is EmbedRaw's
// contract (this file), the zero value the internal path's decode; dims > 0
// is the expected vector length, 0 lets the first vector set it.
type rawAnswer struct {
	strict bool
	dims   int
}

// openAIUsage is the token accounting of an OpenAI-wire answer.
type openAIUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// decodeRawOpenAI reads the OpenAI-wire answer to n inputs: n entries, each
// index in [0, n) once, every vector of one length (dims, or the first
// vector's), in input order.
func decodeRawOpenAI[T floatElem](resp *http.Response, r io.Reader, limit int64, n, dims int) ([][]T, openAIUsage, error) {
	var a struct {
		Data  rawEntries[T] `json:"data"`
		Usage openAIUsage   `json:"usage"`
	}
	a.Data.list = rawList[T]{n: n, dims: dims}
	if err := decodeRawAnswer(resp, r, limit, &a); err != nil {
		return nil, openAIUsage{}, err
	}
	if err := a.Data.list.complete(); err != nil {
		return nil, openAIUsage{}, err
	}
	return a.Data.list.vecs, a.Usage, nil
}

// decodeRawOllama reads the /api/embed answer to n inputs: n vectors in
// input order (the wire carries no index), every one of one length.
func decodeRawOllama[T floatElem](resp *http.Response, r io.Reader, limit int64, n, dims int) ([][]T, int, error) {
	var a struct {
		Embeddings      rawVectors[T] `json:"embeddings"`
		PromptEvalCount int           `json:"prompt_eval_count"`
	}
	a.Embeddings.list = rawList[T]{n: n, dims: dims}
	if err := decodeRawAnswer(resp, r, limit, &a); err != nil {
		return nil, 0, err
	}
	if err := a.Embeddings.list.complete(); err != nil {
		return nil, 0, err
	}
	return a.Embeddings.list.vecs, a.PromptEvalCount, nil
}

// decodeRawAnswer reads the answer's object and decodes it into v. A
// refusal of the walk keeps its text; a read or JSON error is a decode
// error. Both are malformed answers for the failover classifier.
func decodeRawAnswer(resp *http.Response, r io.Reader, limit int64, v any) error {
	doc, err := rawAnswerBuffer(r, resp.ContentLength, limit)
	if err != nil {
		return fmt.Errorf("embed: decode: %w", err)
	}
	if err := json.Unmarshal(doc, v); err != nil {
		return fmt.Errorf("embed: decode: %w", err)
	}
	return nil
}

// rawAnswerBuffer reads the JSON object that opens r into one buffer and
// stops at its closing brace, as json.Decoder stops after the first value;
// bytes after it are not read. The buffer takes the header's length when it
// names one within the cap (the transport then ends the body there), else
// it grows by doubling from the cap divided by 2^k, rounded up, so that the
// last doubling meets the cap and is clamped to it: at most twice the cap
// in all (the rounding adds less than 2^k, which under the 16-MiB transport
// cap is at most the first step), never past it. A step rounded down would
// end below the cap and take one more buffer of the whole cap for the last
// bytes (Delta-Review 7 F-1: 3.86 × the cap with strconv's clone). An answer
// longer than the cap fails in r (httpx.CapBody). A document that is no
// object, or ends before its object closes, is an error.
func rawAnswerBuffer(r io.Reader, hint, limit int64) ([]byte, error) {
	size := limit
	if hint > 0 && hint <= limit {
		size = hint
	} else {
		k := 0
		for size/2 >= rawAnswerMinStep {
			size /= 2
			k++
		}
		size = (limit + int64(1)<<k - 1) >> k
	}
	buf := make([]byte, 0, size)
	var sc objectScan
	for {
		if len(buf) == cap(buf) {
			if int64(cap(buf)) >= limit || (hint > 0 && hint <= limit) {
				// At the cap or the header's length: one more read tells an
				// answer that goes on (r's cap error) from one that ended.
				var probe [1]byte
				n, err := r.Read(probe[:])
				if n == 0 {
					return nil, endOfAnswer(err, len(buf))
				}
				return nil, fmt.Errorf("the answer is longer than its %d-byte length", cap(buf))
			}
			grown := make([]byte, len(buf), min(2*int64(cap(buf)), limit))
			copy(grown, buf)
			buf = grown
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		from := len(buf)
		buf = buf[:from+n]
		end, serr := sc.feed(buf, from)
		if serr != nil {
			return nil, serr
		}
		if end > 0 {
			return buf[:end], nil
		}
		if err != nil {
			return nil, endOfAnswer(err, len(buf))
		}
	}
}

// endOfAnswer is the error of a read that ended before the object closed:
// io.EOF for an empty body, io.ErrUnexpectedEOF after a part (the texts of
// json.Decoder), else the reader's error (httpx.ErrResponseTooLarge).
func endOfAnswer(err error, read int) error {
	if err == nil || errors.Is(err, io.EOF) {
		if read == 0 {
			return io.EOF
		}
		return io.ErrUnexpectedEOF
	}
	return err
}

// objectScan finds the end of the JSON object that opens a document: the
// nesting depth outside strings. It does not validate — json.Unmarshal
// checks the bytes it returns.
type objectScan struct {
	started, inString, escaped bool
	depth                      int
}

// feed scans b[from:] and returns the length of the document up to the
// object's closing brace, or 0 while it is open.
func (s *objectScan) feed(b []byte, from int) (int, error) {
	for i := from; i < len(b); i++ {
		c := b[i]
		switch {
		case !s.started:
			if isSpace(c) {
				continue
			}
			if c != '{' {
				return 0, fmt.Errorf("the answer is not a JSON object (%q)", c)
			}
			s.started, s.depth = true, 1
		case s.inString:
			switch {
			case s.escaped:
				s.escaped = false
			case c == '\\':
				s.escaped = true
			case c == '"':
				s.inString = false
			}
		case c == '"':
			s.inString = true
		case c == '{' || c == '[':
			s.depth++
		case c == '}' || c == ']':
			s.depth--
			if s.depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// rawList is the vectors of one answer as they are walked: at most n, one
// length.
type rawList[T floatElem] struct {
	n, dims int
	seen    bool // the list key appeared once
	count   int
	vecs    [][]T
}

// open starts a walk of the list b (raw JSON); false: b is null.
func (l *rawList[T]) open(b []byte, key string) (bool, error) {
	if l.seen {
		return false, fmt.Errorf("the answer repeats %q", key)
	}
	l.seen = true
	if string(b) == "null" {
		return false, nil
	}
	if b[0] != '[' {
		return false, fmt.Errorf("%q is not a list", key)
	}
	l.vecs = make([][]T, l.n)
	return true, nil
}

// next admits entry number l.count: an entry past the inputs is refused
// before it is decoded (E-1).
func (l *rawList[T]) next() error {
	if l.count >= l.n {
		return errSurplusEmbeddings
	}
	l.count++
	return nil
}

// complete is the count check after the walk: n vectors.
func (l *rawList[T]) complete() error {
	if l.count == 0 {
		return errors.New("embed: empty embedding returned")
	}
	if l.count != l.n {
		return fmt.Errorf("embed: %d embeddings returned for %d inputs", l.count, l.n)
	}
	return nil
}

// rawEntries is the data list of an OpenAI-wire answer.
type rawEntries[T floatElem] struct{ list rawList[T] }

// UnmarshalJSON walks the entries in place. json.Unmarshal hands it a
// subslice of the answer's buffer, and every entry's fields keep subslices
// of it until the walk returns — the buffer outlives the walk, so nothing
// is copied (the Unmarshaler contract's copy rule guards a decoder's
// reused buffer; rawAnswerBuffer's is not reused).
func (d *rawEntries[T]) UnmarshalJSON(b []byte) error {
	l := &d.list
	if ok, err := l.open(b, "data"); !ok {
		return err
	}
	return eachElem(b, func(elem []byte) error {
		if err := l.next(); err != nil {
			return err
		}
		var e struct {
			Index     rawIndex `json:"index"`
			Embedding rawField `json:"embedding"`
		}
		if err := json.Unmarshal(elem, &e); err != nil {
			return err
		}
		i := l.count - 1
		if !e.Index.set {
			return fmt.Errorf("entry %d has no index (missing or null)", i) // E-2
		}
		idx := e.Index.v
		if idx < 0 || idx >= int64(l.n) || l.vecs[idx] != nil {
			return fmt.Errorf("embedding index %d outside [0, %d) or repeated", idx, l.n)
		}
		v, err := parseVector[T](e.Embedding.b, &l.dims, i)
		if err != nil {
			return err
		}
		l.vecs[idx] = v
		return nil
	})
}

// rawVectors is the embeddings list of an /api/embed answer.
type rawVectors[T floatElem] struct{ list rawList[T] }

// UnmarshalJSON walks the vectors in place, in input order (see rawEntries).
func (d *rawVectors[T]) UnmarshalJSON(b []byte) error {
	l := &d.list
	if ok, err := l.open(b, "embeddings"); !ok {
		return err
	}
	return eachElem(b, func(elem []byte) error {
		if err := l.next(); err != nil {
			return err
		}
		i := l.count - 1
		v, err := parseVector[T](elem, &l.dims, i)
		if err != nil {
			return err
		}
		l.vecs[i] = v
		return nil
	})
}

// rawIndex is an entry's index: an integer, present and not null (E-2).
type rawIndex struct {
	v         int64
	set, seen bool
}

// UnmarshalJSON reads the index as encoding/json reads an int (a base-10
// integer literal; 1.0, 1e0 or "1" are refused); null leaves it unset, as a
// missing index does (the walk refuses both), and a second index is refused.
func (x *rawIndex) UnmarshalJSON(b []byte) error {
	if x.seen {
		return errors.New("an entry repeats its index")
	}
	x.seen = true
	if string(b) == "null" {
		return nil
	}
	if len(b) > maxIndexLiteral || (b[0] != '-' && (b[0] < '0' || b[0] > '9')) {
		return fmt.Errorf("an entry's index is no integer (%.24s)", b)
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("an entry's index is no integer (%s)", b)
	}
	x.v, x.set = v, true
	return nil
}

// rawField keeps the raw JSON of a field (a subslice of the answer's
// buffer, see rawEntries) and refuses a second one.
type rawField struct {
	b    []byte
	seen bool
}

func (f *rawField) UnmarshalJSON(b []byte) error {
	if f.seen {
		return errors.New("an entry repeats its embedding")
	}
	f.b, f.seen = b, true
	return nil
}

// parseVector reads one vector (raw JSON) of entry i: a list of numbers —
// a null component is refused before it could read as 0 (E-3) — whose
// length is *dims (or sets it when 0), allocated once after both checks.
// Each number is read as encoding/json reads it into T (strconv.ParseFloat
// at T's size; out of range is refused).
func parseVector[T floatElem](b []byte, dims *int, i int) ([]T, error) {
	if len(b) == 0 || string(b) == "null" {
		return nil, fmt.Errorf("embedding %d is empty", i)
	}
	if b[0] != '[' {
		return nil, fmt.Errorf("embedding %d is not a list", i)
	}
	n := 0
	err := eachElem(b, func(lit []byte) error {
		switch c := lit[0]; {
		case c == '-' || '0' <= c && c <= '9':
			n++
			return nil
		case c == 'n':
			return fmt.Errorf("embedding %d has a null component at %d", i, n) // E-3
		default:
			return fmt.Errorf("embedding %d has a component that is no number at %d", i, n)
		}
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("embedding %d is empty", i)
	}
	if *dims > 0 && n != *dims {
		return nil, fmt.Errorf("embedding %d has %d dimensions, want %d", i, n, *dims)
	}
	*dims = n
	bits := reflect.TypeFor[T]().Bits()
	v := make([]T, 0, n)
	err = eachElem(b, func(lit []byte) error {
		// A view of the buffer for the call: strconv keeps no reference to
		// it (an error carries a clone), and a copy of a long literal would
		// be one more allocation of the answer's size.
		f, err := strconv.ParseFloat(unsafe.String(&lit[0], len(lit)), bits) //nolint:gosec // G103: read-only view of the answer buffer for the duration of the call; strconv clones on error
		if err != nil {
			return fmt.Errorf("embedding %d: component %d does not fit a %d-bit float", i, len(v), bits)
		}
		v = append(v, T(f))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// eachElem calls fn with the raw bytes of each element of the JSON list b,
// in order, and stops at fn's first error. b is valid JSON (json.Unmarshal
// checked the whole answer before it handed b over).
func eachElem(b []byte, fn func(elem []byte) error) error {
	i := skipSpace(b, 1)
	if i < len(b) && b[i] == ']' {
		return nil
	}
	for i < len(b) {
		end := valueEnd(b, i)
		if err := fn(b[i:end]); err != nil {
			return err
		}
		i = skipSpace(b, end)
		if i >= len(b) || b[i] == ']' {
			return nil
		}
		i = skipSpace(b, i+1)
	}
	return nil
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

// valueEnd is the index after the JSON value that starts at b[i].
func valueEnd(b []byte, i int) int {
	switch b[i] {
	case '"':
		return stringEnd(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				j = stringEnd(b, j) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return len(b)
	}
	j := i
	for j < len(b) && b[j] != ',' && b[j] != ']' && b[j] != '}' && !isSpace(b[j]) {
		j++
	}
	return j
}

// stringEnd is the index after the JSON string that starts at b[i].
func stringEnd(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(b)
}
