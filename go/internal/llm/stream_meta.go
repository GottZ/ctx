package llm

import (
	"strconv"
	"unicode/utf8"
)

// walkChunkMeta is decodeChunkMeta's walk (FOLGE-S): one pass over payload
// that validates it as JSON (RFC 8259, strictly) and reads id, model, the
// first choice's finish_reason, usage and timings, allocating only the
// strings and objects it returns. It answers only when its answer is the
// base decode's with certainty; ok false hands the payload to the base
// decode (json.Unmarshal into streamChunk), which then answers — so the
// walk may refuse anything unusual, and it does:
//
//   - any syntax it does not know to be valid (a byte outside valid UTF-8
//     in a string, nesting deeper than metaMaxDepth), and a top level that
//     is no object;
//   - a key with an escape or a non-ASCII byte, a key that is no field of
//     its level but matches one under case folding or without '_' and '-'
//     (encoding/json matches struct fields case-insensitively), and a field
//     that occurs twice (the last would win);
//   - a value of the wrong kind for its field (the base's type error), a
//     string field (id, model, finish_reason) with an escape, an integer
//     that is no plain integer of at most 18 digits, a cost
//     strconv.ParseFloat refuses;
//   - an error object that is not null (the stream ends there; the base
//     decode reads it).
//
// The generated text — delta.content, delta.reasoning, a tool call's id,
// type, name and arguments — is validated and kind-checked as the base's
// types demand, never decoded or copied.
func walkChunkMeta(b []byte) (m chunkMeta, ok bool) {
	w := metaWalker{b: b}
	i := w.ws(0)
	if i >= len(b) || b[i] != '{' {
		return chunkMeta{}, false
	}
	end, ok := w.object(i, 1, &m, metaTop)
	if !ok || w.ws(end) != len(b) {
		return chunkMeta{}, false
	}
	return m, true
}

// metaMaxDepth bounds the nesting the walk follows; a deeper chunk takes
// the base decode (encoding/json's own limit is 10000).
const metaMaxDepth = 64

// metaLevel names the object levels the walk reads.
type metaLevel int

const (
	metaSkip     metaLevel = iota // an object no field of which is read
	metaTop                       // the chunk
	metaChoice                    // an element of choices
	metaDelta                     // a choice's delta
	metaToolCall                  // an element of delta.tool_calls
	metaFunction                  // a tool call's function
	metaUsage                     // usage
	metaTimings                   // timings
)

// metaFields are the fields of each level, as streamChunk names them.
var metaFields = [...][]string{
	metaSkip:     nil,
	metaTop:      {"id", "model", "choices", "usage", "timings", "error"},
	metaChoice:   {"delta", "finish_reason"},
	metaDelta:    {"content", "reasoning", "tool_calls"},
	metaToolCall: {"index", "id", "type", "function"},
	metaFunction: {"name", "arguments"},
	metaUsage:    {"prompt_tokens", "completion_tokens", "cost"},
	metaTimings:  {"prompt_n", "predicted_n", "draft_n", "draft_n_accepted"},
}

// metaWalker is the walk's state: the payload and the place of the first
// choice, whose finish_reason is the chunk's.
type metaWalker struct {
	b []byte
	// choice counts the elements of choices seen (the first is 1).
	choice int
}

func (w *metaWalker) ws(i int) int {
	for i < len(w.b) {
		switch w.b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// object walks the object at w.b[i] ('{') of level lv; m receives what the
// level reads (the chunk's meta, usage and timings through m's pointers).
func (w *metaWalker) object(i, depth int, m *chunkMeta, lv metaLevel) (int, bool) {
	if depth > metaMaxDepth {
		return 0, false
	}
	fields := metaFields[lv]
	var seen uint8
	i = w.ws(i + 1)
	if i < len(w.b) && w.b[i] == '}' {
		return i + 1, true
	}
	for {
		if i >= len(w.b) || w.b[i] != '"' {
			return 0, false
		}
		kend, esc, ok := w.str(i)
		if !ok || esc && lv != metaSkip {
			return 0, false // an escaped key of a level read may name a field
		}
		key := w.b[i+1 : kend-1]
		f := metaField(key, fields)
		if f == -2 {
			return 0, false // a case or delimiter variant of a field
		}
		if f >= 0 {
			if seen&(1<<f) != 0 {
				return 0, false // a repeated field: the last would win
			}
			seen |= 1 << f
		}
		i = w.ws(kend)
		if i >= len(w.b) || w.b[i] != ':' {
			return 0, false
		}
		i = w.ws(i + 1)
		if f < 0 {
			i, ok = w.value(i, depth+1)
		} else {
			i, ok = w.field(i, depth+1, m, lv, fields[f])
		}
		if !ok {
			return 0, false
		}
		i = w.ws(i)
		if i >= len(w.b) {
			return 0, false
		}
		switch w.b[i] {
		case ',':
			i = w.ws(i + 1)
		case '}':
			return i + 1, true
		default:
			return 0, false
		}
	}
}

// field walks the value of field name of level lv at w.b[i].
func (w *metaWalker) field(i, depth int, m *chunkMeta, lv metaLevel, name string) (int, bool) {
	if i >= len(w.b) {
		return 0, false
	}
	switch lv {
	case metaTop:
		return w.topField(i, depth, m, name)
	case metaChoice:
		if name == "finish_reason" {
			if w.choice == 1 {
				return w.text(i, &m.FinishReason)
			}
			var discard string
			return w.text(i, &discard)
		}
		return w.child(i, depth, m, metaDelta)
	case metaDelta:
		if name == "tool_calls" {
			if w.b[i] == 'n' {
				return w.value(i, depth)
			}
			return w.array(i, depth, m, metaToolCall)
		}
		return w.kindString(i, depth)
	case metaToolCall:
		switch name {
		case "index":
			_, end, ok := w.integer(i)
			return end, ok
		case "function":
			return w.child(i, depth, m, metaFunction)
		}
		return w.kindString(i, depth)
	case metaFunction:
		if name == "name" {
			return w.kindString(i, depth)
		}
		return w.value(i, depth) // arguments: a json.RawMessage, any value
	case metaUsage:
		return w.usageField(i, m.Usage, name)
	case metaTimings:
		return w.timingsField(i, m.Timings, name)
	}
	return 0, false
}

// topField walks a field of the chunk itself.
func (w *metaWalker) topField(i, depth int, m *chunkMeta, name string) (int, bool) {
	null := w.b[i] == 'n'
	switch {
	case name == "id":
		return w.text(i, &m.ID)
	case name == "model":
		return w.text(i, &m.Model)
	case null:
		return w.value(i, depth) // choices, usage, timings, error: null passes
	case name == "choices":
		return w.array(i, depth, m, metaChoice)
	case name == "error" || w.b[i] != '{':
		return 0, false // an error object is the base decode's; a wrong kind its type error
	case name == "usage":
		if m.Usage == nil {
			m.Usage = &usageBody{}
		}
		return w.object(i, depth, m, metaUsage)
	}
	if m.Timings == nil {
		m.Timings = &timingsBody{}
	}
	return w.object(i, depth, m, metaTimings)
}

// usageField walks a field of usage.
func (w *metaWalker) usageField(i int, u *usageBody, name string) (int, bool) {
	switch name {
	case "prompt_tokens":
		return w.intField(i, &u.PromptTokens)
	case "completion_tokens":
		return w.intField(i, &u.CompletionTokens)
	}
	return w.cost(i, u)
}

// timingsField walks a field of timings.
func (w *metaWalker) timingsField(i int, t *timingsBody, name string) (int, bool) {
	switch name {
	case "prompt_n":
		return w.intField(i, &t.PromptN)
	case "predicted_n":
		return w.intField(i, &t.PredictedN)
	case "draft_n":
		return w.intField(i, &t.DraftN)
	}
	return w.intField(i, &t.DraftNAccepted)
}

// child walks an object field of level lv (null passes).
func (w *metaWalker) child(i, depth int, m *chunkMeta, lv metaLevel) (int, bool) {
	switch w.b[i] {
	case 'n':
		return w.value(i, depth)
	case '{':
		return w.object(i, depth, m, lv)
	}
	return 0, false
}

// array walks an array field whose elements are objects of level lv or
// null.
func (w *metaWalker) array(i, depth int, m *chunkMeta, lv metaLevel) (int, bool) {
	if w.b[i] != '[' || depth > metaMaxDepth {
		return 0, false
	}
	i = w.ws(i + 1)
	if i < len(w.b) && w.b[i] == ']' {
		return i + 1, true
	}
	for {
		if i >= len(w.b) {
			return 0, false
		}
		if lv == metaChoice {
			w.choice++
		}
		var ok bool
		if lv == metaSkip {
			i, ok = w.value(i, depth+1)
		} else {
			i, ok = w.child(i, depth+1, m, lv)
		}
		if !ok {
			return 0, false
		}
		i = w.ws(i)
		if i >= len(w.b) {
			return 0, false
		}
		switch w.b[i] {
		case ',':
			i = w.ws(i + 1)
		case ']':
			return i + 1, true
		default:
			return 0, false
		}
	}
}

// text reads a string field into dst: a string without an escape (its
// bytes are its value), or null (dst stays).
func (w *metaWalker) text(i int, dst *string) (int, bool) {
	switch w.b[i] {
	case 'n':
		return w.value(i, 0)
	case '"':
		end, esc, ok := w.str(i)
		if !ok || esc {
			return 0, false
		}
		*dst = string(w.b[i+1 : end-1])
		return end, true
	}
	return 0, false
}

// kindString checks a string field that is not read: a string or null.
func (w *metaWalker) kindString(i, depth int) (int, bool) {
	switch w.b[i] {
	case 'n', '"':
		return w.value(i, depth)
	}
	return 0, false
}

// intField reads an int field (null: dst stays).
func (w *metaWalker) intField(i int, dst *int) (int, bool) {
	v, end, ok := w.integer(i)
	if ok && w.b[i] != 'n' {
		*dst = v
	}
	return end, ok
}

// integer reads a plain integer of at most 18 digits, or null.
func (w *metaWalker) integer(i int) (int, int, bool) {
	if w.b[i] == 'n' {
		end, ok := w.value(i, 0)
		return 0, end, ok
	}
	end, ok := w.number(i)
	if !ok {
		return 0, 0, false
	}
	lit := w.b[i:end]
	neg := lit[0] == '-'
	if neg {
		lit = lit[1:]
	}
	if len(lit) > 18 {
		return 0, 0, false
	}
	v := 0
	for _, c := range lit {
		if c < '0' || c > '9' {
			return 0, 0, false // a fraction or an exponent: no Go int
		}
		v = v*10 + int(c-'0')
	}
	if neg {
		v = -v
	}
	return v, end, true
}

// cost reads usage.cost: a number strconv.ParseFloat takes, or null.
func (w *metaWalker) cost(i int, u *usageBody) (int, bool) {
	if w.b[i] == 'n' {
		return w.value(i, 0)
	}
	end, ok := w.number(i)
	if !ok || end-i > 32 {
		return 0, false
	}
	var buf [32]byte
	f, err := strconv.ParseFloat(string(append(buf[:0], w.b[i:end]...)), 64)
	if err != nil {
		return 0, false
	}
	u.Cost = &f
	return end, true
}

// value validates the JSON value at w.b[i] and returns its end.
func (w *metaWalker) value(i, depth int) (int, bool) {
	if i >= len(w.b) || depth > metaMaxDepth {
		return 0, false
	}
	switch c := w.b[i]; c {
	case '"':
		end, _, ok := w.str(i)
		return end, ok
	case '{':
		return w.object(i, depth, nil, metaSkip)
	case '[':
		return w.array(i, depth, nil, metaSkip)
	case 't':
		return w.literal(i, "true")
	case 'f':
		return w.literal(i, "false")
	case 'n':
		return w.literal(i, "null")
	}
	return w.number(i)
}

func (w *metaWalker) literal(i int, lit string) (int, bool) {
	if len(w.b)-i < len(lit) || string(w.b[i:i+len(lit)]) != lit {
		return 0, false
	}
	return i + len(lit), true
}

// number validates the number at w.b[i]: -?(0|[1-9][0-9]*)(.[0-9]+)?([eE][+-]?[0-9]+)?
func (w *metaWalker) number(i int) (int, bool) {
	b := w.b
	if i < len(b) && b[i] == '-' {
		i++
	}
	switch {
	case i < len(b) && b[i] == '0':
		i++
	case i < len(b) && b[i] >= '1' && b[i] <= '9':
		i = digits(b, i)
	default:
		return 0, false
	}
	if i < len(b) && b[i] == '.' {
		j := digits(b, i+1)
		if j == i+1 {
			return 0, false
		}
		i = j
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		j := digits(b, i)
		if j == i {
			return 0, false
		}
		i = j
	}
	return i, true
}

func digits(b []byte, i int) int {
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		i++
	}
	return i
}

// strPlain marks the bytes a string's content can hold as they are.
var strPlain = func() (t [256]bool) {
	for c := 0x20; c < utf8.RuneSelf; c++ {
		t[c] = c != '"' && c != '\\'
	}
	return t
}()

// str validates the string at w.b[i] ('"') and returns its end and whether
// it holds an escape; a byte outside valid UTF-8 is refused (the base
// decode takes it).
func (w *metaWalker) str(i int) (end int, esc, ok bool) {
	b := w.b
	i++
	for i < len(b) {
		c := b[i]
		if strPlain[c] {
			i++
			continue
		}
		switch {
		case c == '"':
			return i + 1, esc, true
		case c == '\\':
			esc = true
			if i+1 >= len(b) {
				return 0, false, false
			}
			switch b[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if len(b)-i < 6 || !isHex(b[i+2]) || !isHex(b[i+3]) || !isHex(b[i+4]) || !isHex(b[i+5]) {
					return 0, false, false
				}
				i += 6
			default:
				return 0, false, false
			}
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				return 0, false, false
			}
			i += size
		default:
			return 0, false, false // a control byte
		}
	}
	return 0, false, false
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// metaField is the index of key among fields, -1 for a key that is no
// field, -2 for a key encoding/json might match to a field all the same:
// equal under ASCII case folding with '_' and '-' left out (key is ASCII).
func metaField(key []byte, fields []string) int {
	if len(fields) == 0 {
		return -1
	}
	for f, name := range fields {
		if string(key) == name {
			return f
		}
	}
	for _, name := range fields {
		if foldEqual(key, name) {
			return -2
		}
	}
	for _, c := range key {
		if c >= utf8.RuneSelf {
			return -2 // Unicode folding ("ſ", "K") is the base decode's
		}
	}
	return -1
}

// foldEqual compares a and b ignoring ASCII case, '_' and '-'.
func foldEqual(a []byte, b string) bool {
	i, j := 0, 0
	for {
		for i < len(a) && (a[i] == '_' || a[i] == '-') {
			i++
		}
		for j < len(b) && (b[j] == '_' || b[j] == '-') {
			j++
		}
		if i == len(a) || j == len(b) {
			return i == len(a) && j == len(b)
		}
		x, y := a[i], b[j]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
		i++
		j++
	}
}
