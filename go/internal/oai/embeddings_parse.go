package oai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/modelalias"
)

// The request parse of POST /v1/embeddings (design/02 §4.8; wave W02-09).
// Every top-level key falls into one class, like the chat parse: CTX (read
// by ctx — model, input, encoding_format, dimensions), DROP (user: named in
// X-Ctx-Ignored-Params, never sent — ctx sends nothing of the client's
// request but the texts) or REJECT (400 unsupported_parameter). A repeated
// key is 400 invalid_json (H-2, jsonkeys.go: the checked value is the sent
// value); a key that differs from a known one only in letter case is an
// unknown key, REJECT. The texts reach the wire as sent: no prefix, no
// rewrite (design/02 §4.8 "Präfix").
//
// The parse holds on the heap only what the request's reservation covers
// (FOLGE-E, the class of FOLGE-P hardenings 5–7 at this route): the model
// name is decoded only up to modelRawMax raw bytes (a longer one cannot name
// an alias and is resolved as no alias, probeNoAlias), and the inputs are
// sized before they are decoded — the exact decoded length of every string
// (decodedLen over strDecoder: an invalid UTF-8 byte becomes U+FFFD of three
// bytes, an escape one to four bytes) plus embedInputSlotBytes per input
// are booked through cover, then the strings are written into one
// allocation of that size. json.Unmarshal into a string held up to 24 × the decoded text on
// the way (lone surrogate escapes; 4.8 × for invalid bytes) — none of it
// booked. A refusal of the budget is errParseOverloaded: 503
// server_overloaded before the allocation. Every answer is the one the
// parse gave before (embed_parse_differential_test.go).

// The encoding_format values (design/02 §4.8): float ⇒ float32 JSON
// numbers, base64 ⇒ little-endian float32, standard base64 — the Python
// SDK's own default when the caller names none.
const (
	encodingFloat  = "float"
	encodingBase64 = "base64"
)

// embedRequest is the parsed client request.
type embedRequest struct {
	model string
	// modelOverlong: the raw model value is past modelRawMax — not
	// decoded, model stays empty and the route resolves no alias for it
	// (probeNoAlias, the answer of an unknown model).
	modelOverlong bool
	input    json.RawMessage // as sent, for the row's body policy
	inputs   []string
	dims     int // the client's dimensions, 0 = not sent
	base64   bool
	ignored  []string
	runes    []int // per input
	runesSum int
	bytesSum int
}

// embedInputSlotBytes is what the parse allocates per input beside its text:
// the string header in the inputs slice (16 bytes) and the input's rune
// count (8 bytes, embedRequest.runes). Booked with the texts, before both
// slices are made.
const embedInputSlotBytes = 16 + 8

// embedKeys are the CTX keys; embedDropKeys the DROP keys.
var (
	embedKeys     = map[string]bool{"model": true, "input": true, "encoding_format": true, "dimensions": true}
	embedDropKeys = map[string]bool{"user": true}
)

// embedNames and embedIndex are the named keys of the top level in sort
// order and their index: a key is classified by its index, a repeat of a
// named key is one bit (FOLGE-P levelKeys, as the chat parse's levels).
var embedNames, embedIndex = levelNames(slices.Collect(maps.Keys(embedKeys)), slices.Collect(maps.Keys(embedDropKeys)))

// embedWindow is how many members the parse reads: one more than the level
// names. Past them a body holds an unknown or a repeated key among them (six
// members, five names), so the answer is a 400 from them alone and the rest
// is never read — a body of a million keys costs the JSON check and six keys
// (hardening H-1, Fremd-Review 2 R2-1). A repeat or an unknown key past the
// window is not seen; the answer is a 400 either way, only the named param
// can differ from a parse of the whole object (Fremd-Review 3 F3-4).
var embedWindow = len(embedNames) + 1

// parseEmbedTop is the alias-independent half of the parse: JSON, the class
// of every key (unknown ⇒ 400 before the model is resolved), the model, the
// shape of input (a string, or a list of non-empty strings; token arrays are
// refused) and the types of encoding_format and dimensions. The limits of
// the alias follow once it is known (checkEmbedPolicy).
//
// The order of the answers: no valid JSON object ⇒ invalid_json; a repeated
// key ⇒ invalid_json (H-2: the checked value is the sent value; the first
// repeat in body order); else an unknown key ⇒ unsupported_parameter, the
// smallest in sort order (levelKeys.reject); then the CTX values in the
// order model, input, encoding_format, dimensions. The keys are read with
// FOLGE-P's helpers (W02-09 rebase round 2): no key is decoded into a
// string — a named key up to the longest name (keyIndex), an unknown one
// into a digest (levelKeys.add) and, for a refusal, up to echoNameMax bytes
// (decodeKey, FOLGE-P H-9) — and no value is copied, the values are the
// body's own bytes. cover books the decoded inputs before they are
// allocated (readInput); nil books nothing (the parse outside a request).
func parseEmbedTop(body []byte, cover func(int64) bool) (*embedRequest, *paramError) {
	it, isObject := newMemberIter(body)
	if !isObject || !json.Valid(body) {
		return nil, notAnObject()
	}
	var vals [maxLevelKeys][]byte
	var keys levelKeys
	for range embedWindow {
		k, v, more := it.next()
		if !more {
			break
		}
		idx := keyIndex(k, embedIndex)
		if keys.add(k, idx) {
			return nil, repeated("", decodeKey(k))
		}
		if idx < 0 {
			keys.refuse(k)
			continue
		}
		vals[idx] = v
	}
	if keys.reject != nil {
		return nil, unsupportedParameter(decodeKey(keys.reject))
	}
	req := &embedRequest{}
	for i, k := range embedNames { // sorted, as the header names them
		if vals[i] != nil && embedDropKeys[k] {
			req.ignored = append(req.ignored, k)
		}
	}
	if perr := req.readModel(vals[embedIndex["model"]]); perr != nil {
		return nil, perr
	}
	if perr := req.readInput(vals[embedIndex["input"]], cover); perr != nil {
		return nil, perr
	}
	if perr := req.readEncoding(vals[embedIndex["encoding_format"]]); perr != nil {
		return nil, perr
	}
	if perr := req.readDimensions(vals[embedIndex["dimensions"]]); perr != nil {
		return nil, perr
	}
	return req, nil
}

func notAnObject() *paramError {
	return &paramError{CodeInvalidJSON, "", "The request body is not a valid JSON object."}
}

func (req *embedRequest) readModel(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("model")
	}
	// Past modelRawMax raw bytes no alias name can stand (hardening 5 of
	// FOLGE-P, B-1, at this route): not decoded — 8 MiB of invalid bytes
	// decoded to 24 MiB before the 404.
	if s, ok := stringContent(v); ok && len(s) > modelRawMax {
		req.modelOverlong = true
		return nil
	}
	if err := json.Unmarshal(v, &req.model); err != nil {
		return invalidType("model", "a string")
	}
	if req.model == "" {
		return missingParameter("model")
	}
	return nil
}

// readInput reads input: a string ⇒ one input; an array of strings ⇒ that
// many; an empty text, an empty array, a token array (int[] or int[][]) or
// any other shape is refused. Token ids presume one tokenizer over the whole
// chain; a failover between two builds makes that uncheckable (design/02
// §4.8). The texts are sized, booked through cover and decoded into one
// allocation of the booked size (FOLGE-E): the strings the route holds are
// the ones json.Unmarshal decoded, byte for byte, without its transients.
func (req *embedRequest) readInput(v json.RawMessage, cover func(int64) bool) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("input")
	}
	req.input = v
	switch kindOf(v) {
	case kindString:
		raw, _ := stringContent(v)
		size := decodedLen(raw)
		if cover != nil && !cover(int64(size)+embedInputSlotBytes) {
			return errParseOverloaded
		}
		var text strings.Builder
		text.Grow(size)
		writeDecoded(&text, raw)
		s := text.String()
		if s == "" {
			return unsupportedValue("input", "an input must not be empty")
		}
		req.inputs = []string{s}
	case kindArray:
		// The elements are counted before one of them is decoded (H-1): a
		// count past the code ceiling of embedding.max_inputs is refused at
		// the cost of a scan; the alias's own max_inputs follows once it is
		// resolved (checkEmbedPolicy).
		n, over := arrayElems(v, modelalias.CeilingEmbeddingMaxInputs)
		switch {
		case over:
			// The code ceiling, before any alias is known: the text names no
			// alias's limit (Fremd-Review 3 F3-5).
			return unsupportedValue("input", fmt.Sprintf("more inputs than any model accepts (%d)", modelalias.CeilingEmbeddingMaxInputs))
		case n == 0:
			return unsupportedValue("input", "at least one input is required")
		}
		// The size run: the decoded length of every string the decode below
		// reaches (it stops at the first element it refuses), booked with
		// the slots of all n inputs before anything is allocated.
		total := 0
		eachArrayElem(v, func(i int, e []byte) bool {
			if i == 0 && hasKind(e, kindNumber|kindArray) || kindOf(e) != kindString {
				return false
			}
			raw, _ := stringContent(e)
			total += decodedLen(raw)
			return len(raw) > 0
		})
		if cover != nil && !cover(int64(total)+int64(n)*embedInputSlotBytes) {
			return errParseOverloaded
		}
		req.inputs = make([]string, 0, n)
		var texts strings.Builder
		texts.Grow(total)
		var perr *paramError
		eachArrayElem(v, func(i int, e []byte) bool {
			if i == 0 && hasKind(e, kindNumber|kindArray) {
				perr = unsupportedValue("input", "token arrays are not supported; send the texts")
				return false
			}
			param := fmt.Sprintf("input[%d]", i)
			if kindOf(e) != kindString {
				perr = invalidType(param, "a string")
				return false
			}
			raw, _ := stringContent(e)
			from := texts.Len()
			writeDecoded(&texts, raw)
			s := texts.String()[from:] // the builder never grows past the booked size: no copy, no move
			if s == "" {
				perr = unsupportedValue(param, "an input must not be empty")
				return false
			}
			req.inputs = append(req.inputs, s)
			return true
		})
		if perr != nil {
			return perr
		}
	default:
		return invalidType("input", "a string or an array of strings")
	}
	req.runes = make([]int, len(req.inputs))
	for i, s := range req.inputs {
		req.runes[i] = utf8.RuneCountInString(s)
		req.runesSum += req.runes[i]
		req.bytesSum += len(s)
	}
	return nil
}

// encodingRawMax is the longest raw JSON form of an encoding_format value
// the route accepts: base64, every character written as a \u escape. A
// longer raw value cannot decode to float or base64 and is refused without
// being decoded (FOLGE-E, the class gate: 8 MiB of invalid bytes decoded
// into 24 MiB, 120 MB allocated on the way).
const encodingRawMax = len(encodingBase64) * len(`\u0000`)

// jsonIntMax is the longest JSON integer an int holds: -9223372036854775808.
// A longer dimensions value (JSON has no leading zeros) overflows it and is
// refused without being decoded (FOLGE-E, the class gate: an 8 MiB integer
// allocated 16 MB in json.Unmarshal).
const jsonIntMax = len("-9223372036854775808")

func (req *embedRequest) readEncoding(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return nil // float, the API's default
	}
	if raw, ok := stringContent(v); ok && len(raw) > encodingRawMax {
		return unsupportedValue("encoding_format", "use float or base64")
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return invalidType("encoding_format", "a string")
	}
	switch s {
	case encodingFloat:
	case encodingBase64:
		req.base64 = true
	default:
		return unsupportedValue("encoding_format", "use float or base64")
	}
	return nil
}

func (req *embedRequest) readDimensions(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return nil
	}
	if !hasKind(v, kindInteger) {
		return invalidType("dimensions", "an integer")
	}
	if len(bytes.TrimSpace(v)) > jsonIntMax {
		return unsupportedValue("dimensions", "must be a positive integer")
	}
	var d int
	if err := json.Unmarshal(v, &d); err != nil || d < 1 {
		return unsupportedValue("dimensions", "must be a positive integer")
	}
	req.dims = d
	return nil
}

// embedLimits are the alias's embedding policy as the route reads it
// (design/02 §3.2): unset keys at their defaults, a tenant alias clamped to
// gateway.tenant_alias_ceiling.embedding_max_inputs.
type embedLimits struct {
	maxInputs     int
	subBatch      int
	nativeDims    int // 0 = unknown (learned from the first block's answer)
	matryoshkaMin int // 0 = no cut allowed
	maxInputRunes int // 0 = no rune limit: the backend judges an input's length
}

func embedLimitsOf(a modelalias.Alias, s Settings) embedLimits {
	p := a.Policy
	l := embedLimits{
		maxInputs:     optOr(p.EmbeddingMaxInputs, modelalias.DefaultEmbeddingMaxInputs),
		subBatch:      optOr(p.EmbeddingSubBatch, modelalias.DefaultEmbeddingSubBatch),
		nativeDims:    optOr(p.EmbeddingNativeDims, 0),
		matryoshkaMin: optOr(p.EmbeddingMatryoshkaMin, 0),
		maxInputRunes: optOr(p.EmbeddingMaxInputRunes, 0),
	}
	if tenantAlias(a) {
		l.maxInputs = min(l.maxInputs, max(s.TenantAliasCeiling.EmbeddingMaxInputs, 0))
	}
	return l
}

// checkEmbedPolicy is the alias-dependent half of the parse: the number of
// inputs and dimensions (design/02 §4.8, E-02-10). dimensions needs a known
// native dimension (the policy's embedding.native_dims — learned only from
// the answer, too late for a check before any work) and lies in
// [matryoshka_min, native_dims]; matryoshka_min 0 allows no cut, so only
// the native dimension itself.
func checkEmbedPolicy(req *embedRequest, l embedLimits) *paramError {
	if n := len(req.inputs); n > l.maxInputs {
		return unsupportedValue("input", fmt.Sprintf("at most %d inputs per request", l.maxInputs))
	}
	if req.dims == 0 {
		return nil
	}
	switch {
	case l.nativeDims <= 0:
		return unsupportedValue("dimensions", "this model does not declare its native dimension")
	case l.matryoshkaMin <= 0 && req.dims != l.nativeDims:
		return unsupportedValue("dimensions", fmt.Sprintf("this model supports only %d", l.nativeDims))
	case l.matryoshkaMin > 0 && (req.dims < l.matryoshkaMin || req.dims > l.nativeDims):
		return unsupportedValue("dimensions", fmt.Sprintf("must be between %d and %d", l.matryoshkaMin, l.nativeDims))
	}
	return nil
}

// checkInputRunes refuses an input above limit runes (param input[i]; a
// single string is input[0] as well — the wire sends it as one). limit is
// the alias's embedding.max_input_runes; without it the route checks no
// length (FOLGE-EMBLIMIT): the backend judges the input, and its refusal is
// the client's 400 at every sub-block (embedCall.outcome). No estimate of
// the route — a rune budget of a row's window — stands in for the backend's
// judgment; the body cap and the in-flight budget bound what reaches it.
func checkInputRunes(req *embedRequest, limit int) *paramError {
	for i, n := range req.runes {
		if n > limit {
			return unsupportedValue(fmt.Sprintf("input[%d]", i), fmt.Sprintf("an input may hold at most %d characters", limit))
		}
	}
	return nil
}

// embedDecodeChunk is the stack buffer the size run and the decode of an
// input read the decoded text through (strDecoder.read).
const embedDecodeChunk = 512

// decodedLen is the length of the string json.Unmarshal decodes from the raw
// JSON string content raw — strDecoder, FOLGE-P's decoder of a key (escapes
// resolved, an unpaired surrogate escape and every invalid UTF-8 byte
// U+FFFD) — counted without allocating.
func decodedLen(raw []byte) int {
	if !needsDecode(raw) {
		return len(raw)
	}
	var buf [embedDecodeChunk]byte
	d := strDecoder{raw: raw}
	total := 0
	for n := d.read(buf[:]); n > 0; n = d.read(buf[:]) {
		total += n
	}
	return total
}

// writeDecoded writes the string json.Unmarshal decodes from raw to b — no
// more than decodedLen(raw) bytes, so a builder grown to the booked size
// never grows again. Pinned against json.Unmarshal by
// TestEmbedInputDecodeIsUnmarshal and its fuzzer.
func writeDecoded(b *strings.Builder, raw []byte) {
	if !needsDecode(raw) {
		b.Write(raw)
		return
	}
	var buf [embedDecodeChunk]byte
	d := strDecoder{raw: raw}
	for n := d.read(buf[:]); n > 0; n = d.read(buf[:]) {
		b.Write(buf[:n])
	}
}

// arrayElems counts the elements of the JSON array raw without decoding one
// (raw must be valid JSON — the body passed json.Valid); over reports more
// than limit elements, and the count stops there.
func arrayElems(raw []byte, limit int) (n int, over bool) {
	eachArrayElem(raw, func(int, []byte) bool {
		n++
		return n <= limit
	})
	return min(n, limit), n > limit
}

// eachArrayElem hands every element of the JSON array raw, as its raw bytes,
// to fn until fn returns false; it allocates nothing.
func eachArrayElem(raw []byte, fn func(i int, elem []byte) bool) {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return
	}
	i = skipSpace(raw, i+1)
	for n := 0; i < len(raw) && raw[i] != ']'; n++ {
		end := skipValue(raw, i)
		if !fn(n, raw[i:end]) {
			return
		}
		i = skipSpace(raw, end)
		if i < len(raw) && raw[i] == ',' {
			i = skipSpace(raw, i+1)
		}
	}
}
