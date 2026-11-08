package rerank

// The strict decoder of a rerank answer (FOLGE-R, RC-D: W02-10's stop finding
// S-W2 — an answer of the old 1-MiB read cap of malformed entries made
// json.Unmarshal allocate up to 126.7 MB; internal/embed raw_answer.go's
// pattern at the rerank wire). decodeAnswer walks the answer in place and
// allocates the scores and their coverage flags — nothing per entry, nothing
// per byte:
//
//   - The syntax of the whole answer is checked first, without an allocation
//     (checkAnswerSyntax: encoding/json's grammar under its v1 options —
//     invalid UTF-8 inside a string and repeated names allowed, 10 000
//     nested levels at most).
//   - The list that holds the entries is counted before an entry is read:
//     an entry past the documents is refused as such (errSurplusResults),
//     before it or any other entry is decoded.
//   - Every value the decode reads is checked against its Go type as
//     json.Unmarshal checks it into the base's response (a type error
//     anywhere is an error), unknown members — a document echo among them —
//     are skipped where they lie.
//   - The scores are read as json.Unmarshal reads them, bit for bit
//     (strconv.ParseFloat at 64 bits), with json.Unmarshal's member rules
//     (encoding/json over its v2 implementation with the v1 options): names
//     match case-insensitively (strings.EqualFold, escapes decoded), a
//     repeated member overwrites, null leaves an int as it was and empties a
//     pointer or a list, and a repeated "results" list decodes into the
//     entries the earlier one left (MergeWithLegacySemantics).
//
// TestInternalRerankBitIdenticalToBase and FuzzDecodeAnswerIsTheBase hold it
// to the base's decode (json.Unmarshal into rerankResponse at ab6b2415):
// the same scores bit for bit, the same prompt tokens, an error where the
// base errs, and the base's error text for every answer of at most n
// entries the re-alignment refuses.

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"unicode/utf8"
	"unsafe"
)

// maxAnswerDepth is the deepest nesting encoding/json reads (jsontext's
// maxNestingDepth): a value nested deeper is a syntax error.
const maxAnswerDepth = 10000

// The member names of the base's response (rerankResponse, rerankResult and
// its usage at ab6b2415 — their json tags).
var (
	nameResults        = []byte("results")
	nameData           = []byte("data")
	nameUsage          = []byte("usage")
	nameIndex          = []byte("index")
	nameRelevanceScore = []byte("relevance_score")
	namePromptTokens   = []byte("prompt_tokens")
	nameTotalTokens    = []byte("total_tokens")
)

var errAnswerEnded = errors.New("unexpected end of the answer")

// decodeAnswer reads a wire answer to n documents into one score per
// document in input order and the reported prompt tokens (see the file's
// comment). The container ("results", else Voyage's "data") is re-aligned
// strictly by each entry's index — the server sorts by score and may
// truncate to top_n, so array position is meaningless: any gap, repeat or
// out-of-range index is an error and the caller fails open.
func decodeAnswer(raw []byte, n int, scoreDomain string) ([]float64, int, error) {
	if err := checkAnswerSyntax(raw); err != nil {
		return nil, 0, fmt.Errorf("rerank: decode: %w", err)
	}
	start := skipSpace(raw, 0)
	top := raw[start:valueEnd(raw, start)]
	switch top[0] {
	case 'n':
		// null decodes into nothing: neither container.
		return nil, 0, neitherContainer(raw)
	case '{':
	default:
		return nil, 0, fmt.Errorf("rerank: decode: the answer is no object (%.24q)", top)
	}
	var a answerScan
	a.scan(top)
	list, fromData, err := a.container(n)
	if err != nil {
		return nil, 0, err
	}
	u, err := a.check(top, list, fromData)
	if err != nil {
		return nil, 0, fmt.Errorf("rerank: decode: %w", err)
	}
	if list == nil {
		return nil, 0, neitherContainer(raw)
	}
	isProbability := fromData
	switch scoreDomain {
	case DomainLogit:
		isProbability = false
	case DomainProbability:
		isProbability = true
	}
	scores, err := a.realign(top, list, n, fromData, isProbability)
	if err != nil {
		return nil, 0, err
	}

	// Voyage reports total_tokens; llama.cpp reports prompt_tokens.
	promptTokens := u.promptTokens
	if promptTokens <= 0 && u.totalTokens > 0 {
		promptTokens = u.totalTokens
		// This flips the MW22 meter semantics for total_tokens-only
		// backends from "uncharged" to a real measured charge — make
		// the switch findable in the log, once per process (rerank
		// runs per query; per-call INFO would be noise).
		totalTokensFallbackOnce.Do(func() {
			slog.Info("rerank: backend reports usage.total_tokens only; charging it as prompt tokens (previously uncharged)")
		})
	}
	return scores, promptTokens, nil
}

var totalTokensFallbackOnce sync.Once

// neitherContainer: neither container decoded any entries — an unknown
// backend dialect, a gateway error object behind HTTP 200, or a renamed
// results field. It names the schema break and echoes a body snippet:
// "count mismatch" would misdiagnose this as a counting problem (the exact
// failure mode that masked the Voyage incident).
func neitherContainer(raw []byte) error {
	return fmt.Errorf("rerank: response contains neither \"results\" nor \"data\" entries (unknown backend dialect?); body: %.256s", raw)
}

// answerScan is what the walk of the answer's top-level members found: the
// last "results" and "data" members' values (a repeated member overwrites)
// and the "results" lists the last one decodes into.
type answerScan struct {
	results []byte // the last "results" member's value; nil: none
	data    []byte // the last "data" member's value; nil: none
	reset   int    // the member number of the last "results" null or []; -1: none
	lists   int    // "results" lists with entries after the last reset
}

// scan walks the members of the object top (valid JSON).
func (a *answerScan) scan(top []byte) {
	a.reset = -1
	k := 0
	eachMember(top, func(name, v []byte) bool {
		switch {
		case nameIs(name, nameResults):
			a.results = v
			switch {
			case v[0] == 'n' || (v[0] == '[' && emptyList(v)):
				// null empties the list, [] makes it a new empty one: the
				// entries an earlier list left are gone.
				a.reset, a.lists = k, 0
			case v[0] == '[':
				a.lists++
			}
		case nameIs(name, nameData):
			a.data = v
		}
		k++
		return true
	})
}

// container is the list whose entries the answer gives the documents —
// "results" when its last value has entries, else "data" (Voyage) —
// counted before an entry is read: more entries than documents is
// errSurplusResults. nil: neither container has entries.
func (a *answerScan) container(n int) (list []byte, fromData bool, err error) {
	if r := a.results; r != nil {
		switch {
		case r[0] == '[' && !emptyList(r):
			if longerThan(r, n) {
				return nil, false, errSurplusResults
			}
			return r, false, nil
		case r[0] != '[' && r[0] != 'n':
			return nil, false, fmt.Errorf("rerank: decode: %q is no list (%.24q)", "results", r)
		}
	}
	d := a.data
	switch {
	case d == nil || d[0] == 'n' || (d[0] == '[' && emptyList(d)):
		return nil, false, nil
	case d[0] != '[':
		return nil, false, fmt.Errorf("rerank: decode data: %q is no list (%.24q)", "data", d)
	case longerThan(d, n):
		return nil, false, errSurplusResults
	}
	return d, true, nil
}

// answerUsage is the token accounting of the answer's "usage" members.
type answerUsage struct {
	promptTokens, totalTokens int
}

// check is the type check of everything json.Unmarshal decodes into the
// base's response — every "results" list and its entries, every "usage"
// object, and the "data" list when it is the container (list, fromData) —
// and returns the usage as the members leave it. It allocates nothing.
func (a *answerScan) check(top, list []byte, fromData bool) (answerUsage, error) {
	var u answerUsage
	var err error
	eachMember(top, func(name, v []byte) bool {
		switch {
		case nameIs(name, nameResults):
			err = checkEntries("results", v)
		case nameIs(name, nameUsage):
			err = u.read(v)
		}
		return err == nil
	})
	if err == nil && fromData {
		err = checkEntries("data", list)
	}
	return u, err
}

// read folds a "usage" value into u: null changes nothing, an object sets
// each counter it carries (null leaves a counter as it was).
func (u *answerUsage) read(v []byte) error {
	switch v[0] {
	case 'n':
		return nil
	case '{':
	default:
		return fmt.Errorf("%q is no object (%.24q)", "usage", v)
	}
	var err error
	eachMember(v, func(name, c []byte) bool {
		switch {
		case nameIs(name, namePromptTokens):
			err = readInt(c, &u.promptTokens)
		case nameIs(name, nameTotalTokens):
			err = readInt(c, &u.totalTokens)
		}
		return err == nil
	})
	return err
}

// checkEntries checks a list's value: null, or a list of entries each null
// or an object whose index is null or an integer and whose relevance_score
// is null or a number a float64 holds.
func checkEntries(key string, v []byte) error {
	switch v[0] {
	case 'n':
		return nil
	case '[':
	default:
		return fmt.Errorf("%q is no list (%.24q)", key, v)
	}
	var err error
	eachElem(v, func(e []byte) bool {
		var st resultEntry
		err = st.read(e)
		return err == nil
	})
	return err
}

// resultEntry is an entry as json.Unmarshal leaves rerankResult: its index
// and its score, if it has one.
type resultEntry struct {
	index    int
	score    float64
	hasScore bool
}

// read folds the entry value e into st: null changes nothing, an object sets
// what its members carry — the index (null leaves it), the score (null
// removes it).
func (st *resultEntry) read(e []byte) error {
	switch e[0] {
	case 'n':
		return nil
	case '{':
	default:
		return fmt.Errorf("an entry is no object (%.24q)", e)
	}
	var err error
	eachMember(e, func(name, v []byte) bool {
		switch {
		case nameIs(name, nameIndex):
			err = readInt(v, &st.index)
		case nameIs(name, nameRelevanceScore):
			if v[0] == 'n' {
				st.hasScore = false
				return true
			}
			if v[0] != '-' && (v[0] < '0' || v[0] > '9') {
				err = fmt.Errorf("relevance_score is no number (%.24q)", v)
				return false
			}
			// A view of the answer for the call: strconv keeps no reference
			// (an error carries a clone), and a copy of a long literal would
			// be an allocation of its size.
			f, perr := strconv.ParseFloat(unsafe.String(&v[0], len(v)), 64) //nolint:gosec // G103: read-only view of the answer for the duration of the call; strconv clones on error
			if perr != nil {
				err = fmt.Errorf("relevance_score %.24q does not fit a float64", v)
				return false
			}
			st.score, st.hasScore = f, true
		}
		return err == nil
	})
	return err
}

// readInt reads an int member as json.Unmarshal reads it: null leaves *dst,
// an integer literal that fits sets it, anything else is an error.
func readInt(v []byte, dst *int) error {
	if v[0] == 'n' {
		return nil
	}
	if v[0] != '-' && (v[0] < '0' || v[0] > '9') {
		return fmt.Errorf("%.24q is no integer", v)
	}
	x, err := strconv.ParseInt(unsafe.String(&v[0], len(v)), 10, 64) //nolint:gosec // G103: read-only view of the answer for the duration of the call; strconv clones on error
	if err != nil {
		return fmt.Errorf("%.24q is no integer that fits an int", v)
	}
	*dst = int(x)
	return nil
}

// realign reads the entries of list (at most n, type-checked) into one score
// per document: each entry's index in [0, n) once, a score present, a
// probability in [0, 1] when isProbability (mapped to its logit), and n
// entries in all — the base's checks and texts, in the base's order. An
// entry of a "results" list is what every list since the last reset left at
// its place (answerScan.lists > 1: folded first, one resultEntry per entry).
func (a *answerScan) realign(top, list []byte, n int, fromData, isProbability bool) ([]float64, error) {
	scores := make([]float64, n)
	seen := make([]bool, n)
	got := 0
	var err error
	place := func(st resultEntry) bool {
		if st.index < 0 || st.index >= n {
			err = fmt.Errorf("rerank: result index %d out of range [0,%d)", st.index, n)
			return false
		}
		if seen[st.index] {
			err = fmt.Errorf("rerank: duplicate result index %d", st.index)
			return false
		}
		seen[st.index] = true
		if !st.hasScore {
			err = fmt.Errorf("rerank: result index %d missing relevance_score (unknown score field name?)", st.index)
			return false
		}
		score := st.score
		if isProbability {
			// A calibrated [0,1] relevance would be sigmoided a second
			// time by the caller (rrf treats every score as a logit),
			// compressing the signal into [0.5,0.73] and letting RRF
			// outvote the reranker at blend_weight < 1. Map the
			// probability to its logit here so the downstream sigmoid
			// reconstructs it exactly. A score outside [0,1] breaks the
			// probability-domain contract — error → caller fails open.
			if score < 0 || score > 1 {
				err = fmt.Errorf("rerank: score %g at index %d outside [0,1] (probability score domain)", score, st.index)
				return false
			}
			score = probabilityToLogit(score)
		}
		scores[st.index] = score
		got++
		return true
	}
	if fromData || a.lists <= 1 {
		eachElem(list, func(e []byte) bool {
			var st resultEntry
			_ = st.read(e) // type-checked
			return place(st)
		})
	} else {
		for _, st := range a.fold(top, countElems(list)) {
			if !place(st) {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if got != n {
		return nil, fmt.Errorf("rerank: result count mismatch: got %d scores for %d documents", got, n)
	}
	return scores, nil
}

// fold is the l entries of the last "results" list as json.Unmarshal leaves
// them when the answer repeats the list: every list since the last reset
// decodes into the entries the one before left (a list keeps its capacity,
// an entry is not zeroed first), so an entry is the fold of every list's
// entry at its place. Places past l cannot reach the result.
func (a *answerScan) fold(top []byte, l int) []resultEntry {
	out := make([]resultEntry, l)
	k := 0
	eachMember(top, func(name, v []byte) bool {
		if k > a.reset && v[0] == '[' && nameIs(name, nameResults) {
			p := 0
			eachElem(v, func(e []byte) bool {
				_ = out[p].read(e) // type-checked
				p++
				return p < l
			})
		}
		k++
		return true
	})
	return out
}

// nameIs reports whether the member name (a JSON string, quotes included)
// matches field as json.Unmarshal matches a struct field's name: its
// escapes decoded, equal under Unicode case folding (strings.EqualFold —
// encoding/json v2 with MatchCaseInsensitiveNames and
// MatchCaseSensitiveDelimiter, the v1 options). A name that decodes to more
// than 28 bytes cannot match: a field folds from at most two bytes per
// ASCII letter (U+017F for s, U+212A for k take two and three).
func nameIs(name, field []byte) bool {
	s := name[1 : len(name)-1]
	if bytes.IndexByte(s, '\\') < 0 {
		return bytes.EqualFold(s, field)
	}
	var buf [32]byte
	u := buf[:0]
	for i := 0; i < len(s); {
		if len(u) > len(buf)-utf8.UTFMax {
			return false
		}
		c := s[i]
		if c != '\\' {
			u = append(u, c)
			i++
			continue
		}
		switch e := s[i+1]; e {
		case 'b':
			u = append(u, '\b')
		case 'f':
			u = append(u, '\f')
		case 'n':
			u = append(u, '\n')
		case 'r':
			u = append(u, '\r')
		case 't':
			u = append(u, '\t')
		case 'u':
			var r rune
			for _, h := range s[i+2 : i+6] {
				r = r<<4 | rune(hexValue(h))
			}
			if utf8.ValidRune(r) {
				u = utf8.AppendRune(u, r)
			} else {
				// A surrogate half: decoded alone or in a pair it is no
				// rune that folds to a field's letters.
				u = utf8.AppendRune(u, utf8.RuneError)
			}
			i += 6
			continue
		default: // " \ /
			u = append(u, e)
		}
		i += 2
	}
	return bytes.EqualFold(u, field)
}

// checkAnswerSyntax reports whether raw is one JSON value, surrounded by
// whitespace at most, as encoding/json's Unmarshal and Valid read it under
// the v1 options: strings without control characters and with valid
// escapes (any other byte, invalid UTF-8 included, as it is), numbers of
// JSON's grammar, the three literals, names repeated freely, at most
// maxAnswerDepth nested levels. It allocates nothing: a bit per level
// tells an object from a list.
func checkAnswerSyntax(b []byte) error {
	var s syntaxState
	i := 0
	for {
		next, opened, err := s.value(b, i)
		if err != nil {
			return err
		}
		if opened {
			i = next
			continue
		}
		var done bool
		if i, done, err = s.after(b, next); err != nil || done {
			return err
		}
	}
}

// syntaxState is the nesting of checkAnswerSyntax: its depth and a bit per
// level, set for an object.
type syntaxState struct {
	objects [(maxAnswerDepth + 63) / 64]uint64
	depth   int
}

// value scans the value that starts at b[i] (after whitespace). opened: it
// opened a list or an object with a member — the next value follows (an
// object's name and colon scanned).
func (s *syntaxState) value(b []byte, i int) (next int, opened bool, err error) {
	i = skipSpace(b, i)
	if i >= len(b) {
		return i, false, errAnswerEnded
	}
	switch c := b[i]; c {
	case '{', '[':
		if s.depth == maxAnswerDepth {
			return i, false, fmt.Errorf("the answer nests deeper than %d levels", maxAnswerDepth)
		}
		i = skipSpace(b, i+1)
		if i < len(b) && b[i] == c+2 { // '}' and ']' are '{'+2 and '['+2
			return i + 1, false, nil
		}
		bit := uint64(1) << (s.depth % 64)
		if c == '{' {
			s.objects[s.depth/64] |= bit
		} else {
			s.objects[s.depth/64] &^= bit
		}
		s.depth++
		if c == '{' {
			i, err = scanName(b, i)
		}
		return i, err == nil, err
	case '"':
		i, err = scanString(b, i)
	case 't':
		i, err = scanLiteral(b, i, "true")
	case 'f':
		i, err = scanLiteral(b, i, "false")
	case 'n':
		i, err = scanLiteral(b, i, "null")
	default:
		i, err = scanNumber(b, i)
	}
	return i, false, err
}

// after scans what follows a value at b[i]: the levels that close there,
// then a comma (and an object's next name) before the next value — or the
// end of the answer (done).
func (s *syntaxState) after(b []byte, i int) (next int, done bool, err error) {
	for {
		i = skipSpace(b, i)
		if s.depth == 0 {
			if i < len(b) {
				return i, false, fmt.Errorf("invalid character %q after the answer", b[i])
			}
			return i, true, nil
		}
		if i >= len(b) {
			return i, false, errAnswerEnded
		}
		object := s.objects[(s.depth-1)/64]>>((s.depth-1)%64)&1 == 1
		switch c := b[i]; {
		case c == ',' && object:
			i, err = scanName(b, skipSpace(b, i+1))
			return i, false, err
		case c == ',':
			return i + 1, false, nil
		case (object && c == '}') || (!object && c == ']'):
			s.depth--
			i++
		default:
			return i, false, fmt.Errorf("invalid character %q in the answer", c)
		}
	}
}

// scanName scans a member name at b[i] and its colon; it returns the
// index after the colon.
func scanName(b []byte, i int) (int, error) {
	if i >= len(b) {
		return i, errAnswerEnded
	}
	if b[i] != '"' {
		return i, fmt.Errorf("invalid character %q where a name begins", b[i])
	}
	i, err := scanString(b, i)
	if err != nil {
		return i, err
	}
	i = skipSpace(b, i)
	if i >= len(b) {
		return i, errAnswerEnded
	}
	if b[i] != ':' {
		return i, fmt.Errorf("invalid character %q after a name", b[i])
	}
	return i + 1, nil
}

// scanString scans the JSON string at b[i] (a quote) and returns the index
// after its closing quote.
func scanString(b []byte, i int) (int, error) {
	for j := i + 1; j < len(b); j++ {
		switch c := b[j]; {
		case c == '"':
			return j + 1, nil
		case c == '\\':
			if j+1 >= len(b) {
				return j, errAnswerEnded
			}
			switch b[j+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				j++
			case 'u':
				if j+6 > len(b) {
					return j, errAnswerEnded
				}
				for _, h := range b[j+2 : j+6] {
					if !isHex(h) {
						return j, fmt.Errorf("invalid escape %q in a string", b[j:j+6])
					}
				}
				j += 5
			default:
				return j, fmt.Errorf("invalid escape %q in a string", b[j:j+2])
			}
		case c < ' ':
			return j, fmt.Errorf("invalid control character %q in a string", c)
		}
	}
	return len(b), errAnswerEnded
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// hexValue is the value of the hex digit c (isHex).
func hexValue(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	}
	return c - 'a' + 10
}

// scanLiteral scans the literal lit at b[i].
func scanLiteral(b []byte, i int, lit string) (int, error) {
	for k := 0; k < len(lit); k++ {
		if i+k >= len(b) {
			return i, errAnswerEnded
		}
		if b[i+k] != lit[k] {
			return i, fmt.Errorf("invalid literal at %.8q", b[i:])
		}
	}
	return i + len(lit), nil
}

// scanNumber scans a JSON number at b[i]: -?(0|[1-9][0-9]*)(.[0-9]+)?([eE][+-]?[0-9]+)?.
func scanNumber(b []byte, i int) (int, error) {
	j := i
	if j < len(b) && b[j] == '-' {
		j++
	}
	switch {
	case j >= len(b):
		return j, errAnswerEnded
	case b[j] == '0':
		j++
	case '1' <= b[j] && b[j] <= '9':
		j = digits(b, j)
	default:
		return j, fmt.Errorf("invalid character %q where a value begins", b[j])
	}
	if j < len(b) && b[j] == '.' {
		j++
		if j >= len(b) {
			return j, errAnswerEnded
		}
		if b[j] < '0' || b[j] > '9' {
			return j, fmt.Errorf("invalid character %q in a number's fraction", b[j])
		}
		j = digits(b, j)
	}
	if j < len(b) && (b[j] == 'e' || b[j] == 'E') {
		j++
		if j < len(b) && (b[j] == '+' || b[j] == '-') {
			j++
		}
		if j >= len(b) {
			return j, errAnswerEnded
		}
		if b[j] < '0' || b[j] > '9' {
			return j, fmt.Errorf("invalid character %q in a number's exponent", b[j])
		}
		j = digits(b, j)
	}
	return j, nil
}

func digits(b []byte, j int) int {
	for j < len(b) && '0' <= b[j] && b[j] <= '9' {
		j++
	}
	return j
}

// The walk of an answer checkAnswerSyntax admitted: every function below
// reads valid JSON and indexes without a bound check of its grammar.

// eachMember calls fn with the raw name (quotes included) and the raw value
// of each member of the object b, in order, until fn returns false.
func eachMember(b []byte, fn func(name, value []byte) bool) {
	i := skipSpace(b, 1)
	if b[i] == '}' {
		return
	}
	for {
		ne := stringEnd(b, i)
		name := b[i:ne]
		i = skipSpace(b, skipSpace(b, ne)+1)
		ve := valueEnd(b, i)
		if !fn(name, b[i:ve]) {
			return
		}
		i = skipSpace(b, ve)
		if b[i] == '}' {
			return
		}
		i = skipSpace(b, i+1)
	}
}

// eachElem calls fn with the raw bytes of each element of the list b, in
// order, until fn returns false.
func eachElem(b []byte, fn func(elem []byte) bool) {
	i := skipSpace(b, 1)
	if b[i] == ']' {
		return
	}
	for {
		end := valueEnd(b, i)
		if !fn(b[i:end]) {
			return
		}
		i = skipSpace(b, end)
		if b[i] == ']' {
			return
		}
		i = skipSpace(b, i+1)
	}
}

// emptyList reports whether the list b has no element.
func emptyList(b []byte) bool { return b[skipSpace(b, 1)] == ']' }

// countElems is the number of elements of the list b.
func countElems(b []byte) int {
	n := 0
	eachElem(b, func([]byte) bool { n++; return true })
	return n
}

// longerThan reports whether the list b has more than n elements; it stops
// at the element past n, unread.
func longerThan(b []byte, n int) bool {
	count := 0
	eachElem(b, func([]byte) bool { count++; return count <= n })
	return count > n
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

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
