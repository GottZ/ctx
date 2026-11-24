package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// The NUL guard (issue #519). A NUL character (U+0000) is a value PostgreSQL
// stores neither in text (22021) nor in JSON (22P05): every statement a
// request's value reached with one failed, and the write paths answered 500
// with an ERROR line (the MCP tools an error carrying the SQL text). The
// handlers decode their bodies inline, each its own way, so the one place
// every value passes is in front of them: the guard reads what a request
// carries — the path parameters, the query, the header values and the body
// — before the handler runs, and refuses a NUL in any of them with 400
// naming the field, without a line, before any statement. /api/manage keeps
// the refusal its dispatcher gives every action after the tier gate (#530);
// the MCP tools refuse it on their arguments (withNULRefusal, mcp_nul.go),
// since /mcp answers in JSON-RPC.

// NULBody is the kind of body a route's handler reads.
type NULBody int

const (
	// NULBodyNone: the guard does not read the body (the route reads none,
	// or /mcp, whose tools check their own arguments).
	NULBodyNone NULBody = iota
	// NULBodyJSON: a JSON body, read as JSON whatever its Content-Type
	// says (the handlers decode it regardless).
	NULBodyJSON
	// NULBodyForm: a form the handler reads through ParseForm/FormValue.
	NULBodyForm
)

// AuthFlowBodyCap is the body cap of the authorization flow's routes
// (/authorize, /token, /register, /auth/login): each handler caps its own
// body at it, and the guard in front of them reads no further.
const AuthFlowBodyCap = 8192

// nulBodySkip names, per route, the body fields the guard leaves to the
// handler: a NUL that is a legitimate value (bytes the route seals and stores
// as bytea — a secret's value), or one the handler answers in its own form
// (an ingest chunk fails on its own and the others are written, as every
// chunk-level failure of that route).
var nulBodySkip = map[string]func(path string) bool{
	http.MethodPut + " /api/secrets/{name}": func(p string) bool { return p == "value" },
	http.MethodPost + " /api/ingest":        func(p string) bool { return strings.HasPrefix(p, "chunks[") },
}

// nulBodyOwnCheck names the routes whose dispatcher refuses a NUL in the body
// itself, after its own gates and in its own form (/api/manage since #530).
var nulBodyOwnCheck = map[string]bool{
	http.MethodPost + " /api/manage": true,
}

// RefuseNUL is the guard in the house envelope: 400, code invalid_body.
// limit is the largest body the route accepts (its group's cap); the guard
// reads at most one byte more.
func RefuseNUL(body NULBody, limit int64) func(http.Handler) http.Handler {
	return nulGuard(body, limit, func(w http.ResponseWriter, msg string) {
		writeJSONReject(w, classInvalidBody.reject(msg))
	})
}

// RefuseNULOAuth is the guard in the OAuth error envelope (RFC 6749 §5.2,
// RFC 7591 §3.2.2): 400 with code as its error.
func RefuseNULOAuth(body NULBody, limit int64, code string) func(http.Handler) http.Handler {
	return nulGuard(body, limit, func(w http.ResponseWriter, msg string) {
		oauthError(w, code, msg)
	})
}

func nulGuard(body NULBody, limit int64, refuse func(http.ResponseWriter, string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			field, found := requestNULField(r)
			if !found && body != NULBodyNone {
				field, found = bodyNULField(r, body, limit)
			}
			if found {
				refuse(w, field+" "+nulFieldMessage)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requestNULField names the first value of the request line and headers that
// carries a NUL: a path parameter ("{name}"), a query parameter ("?name") or
// a header ("header Name"). Over the network a header cannot carry one (the
// server refuses the request); in-process callers can.
func requestNULField(r *http.Request) (string, bool) {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		for i, v := range rctx.URLParams.Values {
			if strings.IndexByte(v, 0) >= 0 && i < len(rctx.URLParams.Keys) {
				return "{" + rctx.URLParams.Keys[i] + "}", true
			}
		}
	}
	if q := r.URL.RawQuery; strings.Contains(q, "%00") || strings.IndexByte(q, 0) >= 0 {
		values, _ := url.ParseQuery(q) // what parses is what a handler reads
		if k, ok := valuesNULKey(values); ok {
			return "?" + k, true
		}
	}
	var names []string
	for k, vs := range r.Header {
		if slices.ContainsFunc(vs, func(v string) bool { return strings.IndexByte(v, 0) >= 0 }) {
			names = append(names, k)
		}
	}
	if len(names) > 0 {
		slices.Sort(names)
		return "header " + names[0], true
	}
	return "", false
}

// valuesNULKey names the first key (in key order) whose name or a value
// carries a NUL; a name that carries one is named by its position instead
// (the answer does not echo the character).
func valuesNULKey(values url.Values) (string, bool) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for i, k := range keys {
		if strings.IndexByte(k, 0) >= 0 {
			return "#" + strconv.Itoa(i+1) + " (its name)", true
		}
		if slices.ContainsFunc(values[k], func(v string) bool { return strings.IndexByte(v, 0) >= 0 }) {
			return k, true
		}
	}
	return "", false
}

// bodyNULField reads the body (at most limit+1 bytes), puts it back for the
// handler and names the field that carries a NUL. A body that ends in an
// error (its group's cap, a broken connection) or runs past limit is handed
// on as it came, unscanned: the handler refuses it for that reason, as before.
func bodyNULField(r *http.Request, body NULBody, limit int64) (string, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return "", false
	}
	if body == NULBodyForm && !formBodyParsed(r) {
		return "", false
	}
	key := r.Method + " " + routePattern(r)
	if nulBodyOwnCheck[key] {
		return "", false
	}
	raw, err := readUpTo(r.Body, limit, r.ContentLength)
	if err != nil || int64(len(raw)) > limit {
		r.Body = &replayBody{buf: raw, tail: err, rest: r.Body}
		return "", false
	}
	r.Body = &replayBody{buf: raw, rest: r.Body}
	if body == NULBodyForm {
		return formNULField(r, raw)
	}
	path, found := jsonNULPath(raw, nulBodySkip[key])
	if !found {
		return "", false
	}
	if path == "" {
		path = "body"
	}
	return path, true
}

func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		return rctx.RoutePattern()
	}
	return r.URL.Path
}

// formBodyParsed reports whether ParseForm/FormValue read this request's
// body as a form: POST, PUT or PATCH with a urlencoded or multipart body.
func formBodyParsed(r *http.Request) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return false
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data"
}

// formNULField names the form field that carries a NUL: a urlencoded body as
// ParseForm decodes it; a multipart body by its raw bytes (its parts carry
// their values unencoded).
func formNULField(r *http.Request, raw []byte) (string, bool) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		if bytes.IndexByte(raw, 0) >= 0 {
			return "body", true
		}
		return "", false
	}
	if !bytes.Contains(raw, []byte("%00")) && bytes.IndexByte(raw, 0) < 0 {
		return "", false
	}
	values, _ := url.ParseQuery(string(raw))
	return valuesNULKey(values)
}

// readUpTo reads rc up to limit+1 bytes. size is the declared length (-1
// unknown): a declared body is read into one buffer of its size.
func readUpTo(rc io.Reader, limit, size int64) ([]byte, error) {
	var b bytes.Buffer
	if size > 0 && size <= limit {
		b.Grow(int(size) + bytes.MinRead)
	}
	_, err := b.ReadFrom(io.LimitReader(rc, limit+1))
	return b.Bytes(), err
}

// replayBody hands the handler the bytes the guard read, then what the read
// ended with (its error), then the rest of the body. It drops its buffer once
// handed out, so a handler that copies the body into its own (a JSON
// decoder) does not hold two copies past the copy.
type replayBody struct {
	buf  []byte
	tail error
	rest io.ReadCloser
}

func (b *replayBody) Read(p []byte) (int, error) {
	if len(b.buf) > 0 {
		n := copy(p, b.buf)
		b.buf = b.buf[n:]
		if len(b.buf) == 0 {
			b.buf = nil
		}
		return n, nil
	}
	if b.tail != nil {
		return 0, b.tail
	}
	return b.rest.Read(p)
}

func (b *replayBody) Close() error { return b.rest.Close() }

// nulMaxDepth bounds the nesting jsonNULPath follows; encoding/json refuses
// deeper documents (its own limit), so the handler refuses them anyway.
const nulMaxDepth = 10000

type nulFrame struct {
	array bool
	index int
	key   []byte // the current key of an object, as it stands in the JSON text
}

// jsonNULPath walks the first JSON value of raw — the one a json.Decoder
// hands the handler — and returns the path of the first string (a value or
// an object's key) that carries a NUL character, its escape or the raw byte,
// whose path skip does not exempt. Paths read "metadata.source", "tags[0]",
// "items[0]: content" (an element of a list of objects, the batch form); a
// key that carries one is named by the object that holds it ("" for the top
// level), so the answer does not echo the character. Invalid JSON before a
// NUL ends the walk without one: the handler's decoder refuses that body.
// One pass, no allocation until a NUL is found.
func jsonNULPath(raw []byte, skip func(string) bool) (string, bool) {
	if bytes.IndexByte(raw, 0) < 0 && !jsonCarriesNUL(raw) {
		return "", false
	}
	sc := nulScanner{raw: raw, skip: skip}
	state := nulExpectValue
	for state != nulDone {
		state = sc.step(state)
	}
	return sc.path, sc.found
}

type nulState int

const (
	nulExpectValue nulState = iota
	nulExpectKey
	nulAfterValue
	nulDone
)

// nulScanner is jsonNULPath's walk: the text, the position, the open
// containers and, once found, the path.
type nulScanner struct {
	raw   []byte
	i     int
	stack []nulFrame
	skip  func(string) bool
	path  string
	found bool
}

func (sc *nulScanner) step(state nulState) nulState {
	sc.skipWS()
	if sc.i >= len(sc.raw) {
		return nulDone
	}
	switch state {
	case nulExpectValue:
		return sc.value()
	case nulExpectKey:
		return sc.key()
	default:
		return sc.after()
	}
}

func (sc *nulScanner) skipWS() {
	for sc.i < len(sc.raw) {
		switch sc.raw[sc.i] {
		case ' ', '\t', '\n', '\r':
			sc.i++
		default:
			return
		}
	}
}

// hit records a NUL at the path of frames unless skip exempts it; it
// reports whether the walk ends.
func (sc *nulScanner) hit(frames []nulFrame) bool {
	p := renderNULPath(frames)
	if sc.skip != nil && sc.skip(p) {
		return false
	}
	sc.path, sc.found = p, true
	return true
}

func (sc *nulScanner) value() nulState {
	switch c := sc.raw[sc.i]; c {
	case '{', '[':
		if len(sc.stack) >= nulMaxDepth {
			return nulDone
		}
		sc.stack = append(sc.stack, nulFrame{array: c == '['})
		sc.i++
		sc.skipWS()
		if sc.i < len(sc.raw) && (sc.raw[sc.i] == ']' || sc.raw[sc.i] == '}') {
			return nulAfterValue // the closer is checked against its opener there
		}
		if c == '[' {
			return nulExpectValue
		}
		return nulExpectKey
	case '"':
		end, nul, ok := scanJSONString(sc.raw, sc.i)
		if !ok || (nul && sc.hit(sc.stack)) {
			return nulDone
		}
		sc.i = end
		return nulAfterValue
	default:
		for sc.i < len(sc.raw) && !jsonScalarEnd(sc.raw[sc.i]) {
			sc.i++
		}
		return nulAfterValue
	}
}

// jsonScalarEnd reports whether c ends a number or a literal.
func jsonScalarEnd(c byte) bool {
	switch c {
	case ',', ']', '}', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

func (sc *nulScanner) key() nulState {
	if sc.raw[sc.i] != '"' {
		return nulDone
	}
	end, nul, ok := scanJSONString(sc.raw, sc.i)
	if !ok || (nul && sc.hit(sc.stack[:len(sc.stack)-1])) {
		return nulDone
	}
	sc.stack[len(sc.stack)-1].key = sc.raw[sc.i+1 : end-1]
	sc.i = end
	sc.skipWS()
	if sc.i >= len(sc.raw) || sc.raw[sc.i] != ':' {
		return nulDone
	}
	sc.i++
	return nulExpectValue
}

func (sc *nulScanner) after() nulState {
	if len(sc.stack) == 0 {
		return nulDone // the first value is complete
	}
	top := &sc.stack[len(sc.stack)-1]
	switch c := sc.raw[sc.i]; {
	case c == ',':
		sc.i++
		if top.array {
			top.index++
			return nulExpectValue
		}
		return nulExpectKey
	case (top.array && c == ']') || (!top.array && c == '}'):
		sc.i++
		sc.stack = sc.stack[:len(sc.stack)-1]
		return nulAfterValue
	}
	return nulDone
}

// scanJSONString scans the JSON string that starts at raw[i] (a quote) and
// returns the index after its closing quote and whether it carries a NUL —
// the escape backslash-u-0000, or the raw byte (invalid JSON, which a decoder
// refuses, but no less a NUL).
func scanJSONString(raw []byte, i int) (end int, nul, ok bool) {
	n := len(raw)
	for j := i + 1; j < n; {
		switch c := raw[j]; c {
		case '"':
			return j + 1, nul, true
		case '\\':
			if j+1 >= n {
				return 0, false, false
			}
			if raw[j+1] == 'u' {
				if j+6 > n {
					return 0, false, false
				}
				if string(raw[j+2:j+6]) == "0000" {
					nul = true
				}
				j += 6
				continue
			}
			j += 2
		case 0:
			nul = true
			j++
		default:
			j++
		}
	}
	return 0, false, false
}

// renderNULPath renders the frames as a field path: keys joined by ".", an
// index as "[i]", and a key inside an element of a list as "[i]: key".
func renderNULPath(stack []nulFrame) string {
	var b strings.Builder
	for k, f := range stack {
		if f.array {
			b.WriteString("[" + strconv.Itoa(f.index) + "]")
			continue
		}
		if f.key == nil {
			continue
		}
		switch {
		case b.Len() == 0:
		case k > 0 && stack[k-1].array:
			b.WriteString(": ")
		default:
			b.WriteString(".")
		}
		b.WriteString(jsonKeyText(f.key))
	}
	return b.String()
}

// jsonKeyText decodes a key as it stands in the JSON text (its escapes).
func jsonKeyText(raw []byte) string {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw)
	}
	var s string
	if json.Unmarshal(append(append([]byte{'"'}, raw...), '"'), &s) != nil {
		return string(raw)
	}
	return s
}
