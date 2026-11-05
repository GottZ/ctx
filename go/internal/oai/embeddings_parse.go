package oai

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/promptguard"
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

// The encoding_format values (design/02 §4.8): float ⇒ float32 JSON
// numbers, base64 ⇒ little-endian float32, standard base64 — the Python
// SDK's own default when the caller names none.
const (
	encodingFloat  = "float"
	encodingBase64 = "base64"
)

// embedRequest is the parsed client request.
type embedRequest struct {
	model    string
	input    json.RawMessage // as sent, for the row's body policy
	inputs   []string
	dims     int // the client's dimensions, 0 = not sent
	base64   bool
	ignored  []string
	runes    []int // per input
	runesSum int
	bytesSum int
}

// embedKeys are the CTX keys; embedDropKeys the DROP keys.
var (
	embedKeys     = map[string]bool{"model": true, "input": true, "encoding_format": true, "dimensions": true}
	embedDropKeys = map[string]bool{"user": true}
)

// parseEmbedTop is the alias-independent half of the parse: JSON, the class
// of every key (unknown ⇒ 400 before the model is resolved), the model, the
// shape of input (a string, or a list of non-empty strings; token arrays are
// refused) and the types of encoding_format and dimensions. The limits of
// the alias follow once it is known (checkEmbedPolicy).
func parseEmbedTop(body []byte) (*embedRequest, *paramError) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, &paramError{CodeInvalidJSON, "", "The request body is not a valid JSON object."}
	}
	if perr := uniqueMembers("", body, raw); perr != nil {
		return nil, perr
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the first refused key is deterministic
	req := &embedRequest{}
	for _, k := range keys {
		switch {
		case embedDropKeys[k]:
			req.ignored = append(req.ignored, k)
		case !embedKeys[k]:
			return nil, unsupportedParameter(k)
		}
	}
	if perr := req.readModel(raw["model"]); perr != nil {
		return nil, perr
	}
	if perr := req.readInput(raw["input"]); perr != nil {
		return nil, perr
	}
	if perr := req.readEncoding(raw["encoding_format"]); perr != nil {
		return nil, perr
	}
	if perr := req.readDimensions(raw["dimensions"]); perr != nil {
		return nil, perr
	}
	return req, nil
}

func (req *embedRequest) readModel(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("model")
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
// §4.8).
func (req *embedRequest) readInput(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("input")
	}
	req.input = v
	switch kindOf(v) {
	case kindString:
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return invalidType("input", "a string or an array of strings")
		}
		if s == "" {
			return unsupportedValue("input", "an input must not be empty")
		}
		req.inputs = []string{s}
	case kindArray:
		var elems []json.RawMessage
		if err := json.Unmarshal(v, &elems); err != nil {
			return invalidType("input", "a string or an array of strings")
		}
		if len(elems) == 0 {
			return unsupportedValue("input", "at least one input is required")
		}
		if hasKind(elems[0], kindNumber|kindArray) {
			return unsupportedValue("input", "token arrays are not supported; send the texts")
		}
		req.inputs = make([]string, len(elems))
		for i, e := range elems {
			param := fmt.Sprintf("input[%d]", i)
			if kindOf(e) != kindString {
				return invalidType(param, "a string")
			}
			if err := json.Unmarshal(e, &req.inputs[i]); err != nil {
				return invalidType(param, "a string")
			}
			if req.inputs[i] == "" {
				return unsupportedValue(param, "an input must not be empty")
			}
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

func (req *embedRequest) readEncoding(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return nil // float, the API's default
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
	maxInputRunes int // 0 = the chain's window decides (embedRuneLimit)
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
// single string is input[0] as well — the wire sends it as one).
func checkInputRunes(req *embedRequest, limit int) *paramError {
	for i, n := range req.runes {
		if n > limit {
			return unsupportedValue(fmt.Sprintf("input[%d]", i), fmt.Sprintf("an input may hold at most %d characters", limit))
		}
	}
	return nil
}

// embedFallbackWindow is the token window the rune limit of an input
// assumes when neither the alias (embedding.max_input_runes) nor a row of
// the chain names one (design/02 §3.2: "ohne bekanntes Fenster 3686" =
// promptguard.RuneBudget of 2048 tokens, the live embedding window). Data of
// the design, like modelalias.Default*: an operator sets the alias key.
const embedFallbackWindow = 2048

// embedRuneLimit is the per-input rune limit (design/02 §3.2
// embedding.max_input_runes): the alias's value, else the 9/5 rune budget
// (promptguard.RuneBudget, the measured densest tokenization) of the
// smallest known window of the chain, else of embedFallbackWindow — capped
// by the code ceiling. The guard heuristic of the promptguard census reads
// per file: this file builds no prompt, it only borrows the ratio.
func embedRuneLimit(l embedLimits, chain []backends.Backend) int {
	if l.maxInputRunes > 0 {
		return l.maxInputRunes
	}
	window := 0
	for i := range chain {
		if n := chain[i].NumCtx; n > 0 && (window == 0 || n < window) {
			window = n
		}
	}
	if window == 0 {
		window = embedFallbackWindow
	}
	return min(promptguard.RuneBudget(window), modelalias.CeilingEmbeddingMaxInputRunes)
}
