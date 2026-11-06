package oai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
)

// The request parse of POST /v1/chat/completions (design/02 §4.5, §4.5.1,
// L1/L2): every top-level key, every message key and every content-part key
// falls into exactly one class — PASS (upstream after a type and size check),
// CTX (read and set by ctx), DROP (removed, named in X-Ctx-Ignored-Params) or
// REJECT (400) — and an allowed value reaches the wire with its bytes as sent
// except insignificant whitespace (llm.BuildUpstreamBody compacts it, no
// escaping; a rewritten message gets sorted keys and role "system").
//
// Memory (FOLGE-P): the parse holds on the heap only what the request's
// reservation covers. It walks the body in place — json.Valid once, then
// memberIter/arrayIter over slices of the body: a key is classified before
// its value is looked at, an array is counted before it is checked, no value
// is copied and no object becomes a map. The one copy it makes, the message
// array with a rewritten message, is sized first and reserved through grow
// before it is allocated (parsePolicy). The model name is decoded for
// Resolve only while its raw form can still be an alias name (modelRawMax).
// Everything unknown is refused: an engine knob that is not listed here
// (SGLang custom_logit_processor, vLLM kv_transfer_params, a new one of the
// next engine release) cannot be smuggled past a list that names what is
// allowed.

// keyClass is the class of one request key.
type keyClass int

const (
	classReject keyClass = iota
	classPass
	classCTX
	classDrop
)

// jsonKind is the coarse JSON type of a PASS value (L1: "Typ-Grobprüfung").
type jsonKind int

const (
	kindNumber jsonKind = 1 << iota
	kindInteger
	kindString
	kindBool
	kindArray
	kindObject
)

// passKeys are the PASS keys of the top level and their JSON types
// (design/02 §4.5 table; the extensions top_k, min_p, repetition_penalty
// are what ctx-B itself sends and all three engines know — L7). messages is
// PASS too but parsed on its own (§4.5.1).
var passKeys = map[string]jsonKind{
	"temperature":         kindNumber,
	"top_p":               kindNumber,
	"stop":                kindString | kindArray,
	"presence_penalty":    kindNumber,
	"frequency_penalty":   kindNumber,
	"logit_bias":          kindObject,
	"logprobs":            kindBool,
	"top_logprobs":        kindInteger,
	"seed":                kindInteger,
	"response_format":     kindObject,
	"tools":               kindArray,
	"tool_choice":         kindString | kindObject,
	"parallel_tool_calls": kindBool,
	"top_k":               kindNumber,
	"min_p":               kindNumber,
	"repetition_penalty":  kindNumber,
}

// ctxKeys are read and overlaid by ctx (§4.5 class CTX): the client's value
// never reaches the wire as sent (llm.BuildUpstreamBody drops them and
// writes its frame). reasoning is the OpenRouter extension ctx-B sends on
// every stream (L7) — accepted, never forwarded; the alias policy decides
// (design/02 §3.2 reasoning, RC-B-INDEX Q15).
var ctxKeys = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true, "max_tokens": true,
	"max_completion_tokens": true, "n": true, "service_tier": true, "reasoning": true, "reasoning_effort": true,
}

// dropKeys are removed and named in X-Ctx-Ignored-Params (§4.5 class DROP):
// known client and vendor fields that must not break a client (LiteLLM sends
// priority, L7) and the engine internals of SGLang, vLLM and llama.cpp that
// would steer scheduling, caching, adapters or the request identity
// (design/02 §5 B1). modalities is dropped only as ["text"] (its default);
// anything else is REJECT (parseTopLevel).
var dropKeys = map[string]bool{
	"priority": true, "chat_template_kwargs": true, "user": true, "safety_identifier": true,
	"prompt_cache_key": true, "prompt_cache_retention": true, "prompt_cache_options": true,
	"metadata": true, "store": true, "verbosity": true, "moderation": true,
	"rid": true, "request_id": true, "id_slot": true, "cache_prompt": true, "lora": true, "lora_path": true,
	"return_hidden_states": true, "custom_logit_processor": true, "bootstrap_host": true,
	"bootstrap_port": true, "bootstrap_room": true, "kv_transfer_params": true, "vllm_xargs": true,
	"cache_salt": true, "session_params": true, "modalities": true,
}

// The message level (§4.5.1): PASS keys verbatim, DROP keys removed — earlier
// reasoning traces never go upstream.
var (
	messagePassKeys = map[string]bool{"role": true, "content": true, "name": true, "tool_calls": true, "tool_call_id": true, "refusal": true}
	messageDropKeys = map[string]bool{"reasoning": true, "reasoning_content": true}
	messageRoles    = []string{"system", "user", "assistant", "tool", "developer"}
)

// The named keys of every checked level in sort order and their index
// (FOLGE-P: a key is classified by its index, a repeat of a named key is one
// bit — levelKeys).
var (
	topLevelNames, topLevelIndex = levelNames(slices.Collect(maps.Keys(passKeys)), slices.Collect(maps.Keys(ctxKeys)),
		slices.Collect(maps.Keys(dropKeys)))
	messageNames, messageIndex = levelNames(slices.Collect(maps.Keys(messagePassKeys)), slices.Collect(maps.Keys(messageDropKeys)))
	_, partIndex               = levelNames([]string{"type", "text", "image_url"})
	_, imageIndex              = levelNames([]string{"url", "detail"})
)

// messageKeyCount is the number of named message keys (messageNames).
const messageKeyCount = 8

// The indexes of the named message, part and image_url keys.
var (
	msgContent, msgName, msgReasoning, msgReasoningContent = messageIndex["content"], messageIndex["name"], messageIndex["reasoning"], messageIndex["reasoning_content"]
	msgRefusal, msgRole, msgToolCallID, msgToolCalls       = messageIndex["refusal"], messageIndex["role"], messageIndex["tool_call_id"], messageIndex["tool_calls"]
	partType, partText, partImage                          = partIndex["type"], partIndex["text"], partIndex["image_url"]
	imageURL, imageDetail                                  = imageIndex["url"], imageIndex["detail"]
)

func init() {
	if len(messageNames) != messageKeyCount {
		panic("oai: messageKeyCount does not match the message keys")
	}
}

// serviceTiers are the service_tier values of the OpenAI spec; the planner
// reads them (flex lowers the lane to bulk, design/03 §4.4).
var serviceTiers = map[string]bool{"auto": true, "default": true, "flex": true, "scale": true, "priority": true}

// requestTierOf is the service tier the raw JSON string content s names, ""
// for none — matched in its decoded form, never decoded into a string.
func requestTierOf(s []byte) string {
	for t := range serviceTiers {
		if cmpKeyName(s, t) == 0 {
			return t
		}
	}
	return ""
}

// responseFormatTypes are the response_format.type values PASS admits
// (§4.5: text, json_object, json_schema).
var responseFormatTypes = map[string]bool{"text": true, "json_object": true, "json_schema": true}

// visionPrefixes are the only image_url.url forms (§4.5.1, B18): a data URI
// of one of four image types, lowercase, without parameters. Engines fetch
// every other form server-side — http(s) into the LAN, file:// and bare
// paths from the serving host (SGLang load_image) — so nothing else passes.
var visionPrefixes = []string{
	"data:image/png;base64,", "data:image/jpeg;base64,", "data:image/webp;base64,", "data:image/gif;base64,",
}

// topLogprobsMax is the spec's bound of top_logprobs (OpenAI: 0..20).
const topLogprobsMax = 20

// paramError is one 400 of the parse: a code of the vocabulary, the param
// path the client sent and a ctx-own message (it echoes no value).
type paramError struct {
	code, param, message string
}

func unsupportedParameter(param string) *paramError {
	return &paramError{CodeUnsupportedParameter, param, fmt.Sprintf("Unrecognized request argument supplied: %s.", param)}
}

func unsupportedValue(param, why string) *paramError {
	return &paramError{CodeUnsupportedValue, param, fmt.Sprintf("Unsupported value for %s: %s.", param, why)}
}

func invalidType(param, want string) *paramError {
	return &paramError{CodeInvalidType, param, fmt.Sprintf("Invalid type for %s: expected %s.", param, want)}
}

func missingParameter(param string) *paramError {
	return &paramError{CodeMissingRequiredParameter, param, fmt.Sprintf("Missing required parameter: %s.", param)}
}

// chatRequest is the parsed client request: the PASS keys for the wire, the
// rewritten messages, the CTX values ctx reads and the DROP names.
type chatRequest struct {
	model        string
	pass         map[string]json.RawMessage
	messages     json.RawMessage
	stream       bool
	n            int
	maxTokens    int // the client's max_tokens / max_completion_tokens, 0 = not sent
	serviceTier  string
	ignored      []string
	messageCount int

	// modelOverlong: the raw model value is past modelRawMax — not
	// decoded, model stays empty and Resolve finds no alias for it.
	modelOverlong bool
}

// parseTopLevel is the alias-independent half of the parse: JSON, the class
// of every top-level key (unknown ⇒ 400 before the model is resolved, so the
// answer says nothing about any alias), the model name and the CTX values.
// The message and policy checks follow once the alias is known (parsePolicy).
//
// The order of the answers: no valid JSON object ⇒ invalid_json; a repeated
// key ⇒ invalid_json (H-2: the checked value is the sent value; the first
// repeat in body order — see pairwiseKeys for unknown keys); else the keys in
// sort order, the first refused one wins — the smallest unknown key is held
// in O(1) (levelKeys.reject), the named ones are checked in topLevelNames
// order up to it. No key and no value is copied (FOLGE-P).
func parseTopLevel(body []byte) (*chatRequest, *paramError) {
	it, isObject := newMemberIter(body)
	if !isObject || !json.Valid(body) {
		return nil, &paramError{CodeInvalidJSON, "", "The request body is not a valid JSON object."}
	}
	var vals [maxLevelKeys][]byte
	var keys levelKeys
	for {
		k, v, more := it.next()
		if !more {
			break
		}
		idx := keyIndex(k, topLevelIndex)
		if keys.add(k, idx) {
			return nil, repeated("", decodeKey(k)) // H-2: the checked value is the sent value
		}
		if idx < 0 {
			keys.refuse(k)
			continue
		}
		vals[idx] = v
	}
	req := &chatRequest{pass: make(map[string]json.RawMessage)}
	for i, k := range topLevelNames { // sorted: the first refused key is deterministic
		v := json.RawMessage(vals[i])
		if v == nil {
			continue
		}
		if keys.reject != nil && cmpKeyName(keys.reject, k) < 0 {
			break // an unknown key sorts before k
		}
		switch classOf(k) {
		case classReject:
			return nil, unsupportedParameter(k)
		case classDrop:
			if k == "modalities" && !textOnlyModalities(v) {
				return nil, unsupportedParameter(k) // REJECT unless ["text"] (§4.5)
			}
			req.ignored = append(req.ignored, k)
		case classPass:
			if isNull(v) {
				continue // null = the default; nothing to forward
			}
			if !hasKind(v, passKeys[k]) {
				return nil, invalidType(k, kindName(passKeys[k]))
			}
			req.pass[k] = v
		case classCTX:
			if perr := req.readCTX(k, v); perr != nil {
				return nil, perr
			}
		}
	}
	if keys.reject != nil {
		return nil, unsupportedParameter(decodeKey(keys.reject))
	}
	if req.model == "" && !req.modelOverlong {
		return nil, missingParameter("model")
	}
	return req, nil
}

// aliasNameMaxBytes is the longest alias name: migration 162's CHECK
// ^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$ on every alias row the registry loads
// (handler modelAliasNameRE refuses a longer one before the write) — ASCII
// only, one byte per character. Pinned to the migrations by
// TestAliasNameBoundIsTheMigrationCheck.
const aliasNameMaxBytes = 128

// modelRawMax is the longest raw JSON form of an alias name: every character
// written as a \u escape of 6 bytes. A raw model value past it cannot decode
// to an alias name — fewer than 6 raw bytes per character leave more than
// aliasNameMaxBytes characters, a non-ASCII character is no alias character —
// so the parse does not decode it (hardening 5, B-1: the decode turned each
// invalid UTF-8 byte into a U+FFFD of 3 bytes, a 256 KiB name allocated
// 3.9 MB) and Resolve looks up the empty name: the answer of an unknown
// model, byte for byte, with its one grant probe.
const modelRawMax = aliasNameMaxBytes * len(`\u0000`)

func classOf(k string) keyClass {
	switch {
	case ctxKeys[k]:
		return classCTX
	case k == "messages":
		return classCTX
	case passKeys[k] != 0:
		return classPass
	case dropKeys[k]:
		return classDrop
	}
	return classReject
}

// readCTX reads one CTX key. stream: true and n > 1 are refused here (E4:
// n > 1 is RC-F; streaming is W02-08) — before any alias is resolved.
func (req *chatRequest) readCTX(k string, v json.RawMessage) *paramError {
	if isNull(v) {
		return nil
	}
	switch k {
	case "model":
		s, ok := stringContent(v)
		if !ok {
			return invalidType(k, "a string")
		}
		if len(s) > modelRawMax {
			req.modelOverlong = true
			return nil
		}
		if err := json.Unmarshal(v, &req.model); err != nil {
			return invalidType(k, "a string")
		}
	case "messages":
		req.messages = v
	case "stream":
		if err := json.Unmarshal(v, &req.stream); err != nil {
			return invalidType(k, "a boolean")
		}
		if req.stream {
			return unsupportedValue(k, "streaming is not supported on this endpoint yet")
		}
	case "stream_options", "reasoning":
		if !hasKind(v, kindObject) {
			return invalidType(k, "an object")
		}
	case "reasoning_effort":
		if !hasKind(v, kindString) {
			return invalidType(k, "a string")
		}
	case "service_tier":
		s, ok := stringContent(v)
		if !ok {
			return invalidType(k, "a string")
		}
		if req.serviceTier = requestTierOf(s); req.serviceTier == "" {
			return unsupportedValue(k, "unknown service tier")
		}
	case "n":
		n, ok := intLiteral(v)
		if !ok {
			return invalidType(k, "an integer")
		}
		req.n = n
		if req.n != 1 {
			return unsupportedValue(k, "only n = 1 is supported")
		}
	case "max_tokens", "max_completion_tokens":
		return req.readMaxTokens(k, v)
	}
	return nil
}

// readMaxTokens maps max_completion_tokens onto max_tokens; both set with
// different values is a 400 (§4.5.2 stage 3).
func (req *chatRequest) readMaxTokens(k string, v json.RawMessage) *paramError {
	n, ok := intLiteral(v)
	if !ok {
		return invalidType(k, "an integer")
	}
	if n < 1 {
		return unsupportedValue(k, "must be at least 1")
	}
	if req.maxTokens != 0 && req.maxTokens != n {
		return unsupportedValue(k, "max_tokens and max_completion_tokens differ")
	}
	req.maxTokens = n
	return nil
}

// intLiteral reads a JSON integer literal the way a decode into an int did —
// false for a number with fraction or exponent and for one past the int64
// range — without copying it (FOLGE-P: the decode copied an 8 MiB literal
// twice). A JSON integer has no leading zeros, so one of more than 20
// characters ("-9223372036854775808") is out of range unread.
func intLiteral(v json.RawMessage) (int, bool) {
	t := bytes.TrimSpace(v)
	if !hasKind(t, kindInteger) || len(t) > 20 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(t), 10, 64)
	return int(n), err == nil
}

// textOnlyModalities reports modalities = ["text"] (what a decode into a
// []string of length one holding "text" accepted), without decoding the
// array.
func textOnlyModalities(v json.RawMessage) bool {
	it, ok := newArrayIter(v)
	if !ok {
		return false
	}
	first, ok := it.next()
	if !ok {
		return false
	}
	if _, more := it.next(); more {
		return false
	}
	s, ok := stringContent(first)
	return ok && cmpKeyName(s, "text") == 0
}

// policyLimits are the size caps of one request (design/02 §3.2): the alias
// policy, its default for an unset key, and for a tenant alias the tenant
// ceiling over both (W02-04 handover (1): the registry clamps set keys only).
type policyLimits struct {
	toolsMaxCount, toolsMaxBytes, responseFormatMaxBytes int
	logitBiasMaxEntries, stopMaxEntries                  int
	vision                                               bool
	visionMaxBytes                                       int
}

func optOr(o modelalias.Opt[int], def int) int {
	if o.Set {
		return o.V
	}
	return def
}

// limitsOf resolves the caps of a for this request.
func limitsOf(a modelalias.Alias, s Settings) policyLimits {
	p := a.Policy
	l := policyLimits{
		toolsMaxCount:          optOr(p.ToolsMaxCount, modelalias.DefaultToolsMaxCount),
		toolsMaxBytes:          optOr(p.ToolsMaxBytes, modelalias.DefaultToolsMaxBytes),
		responseFormatMaxBytes: optOr(p.ResponseFormatMaxBytes, modelalias.DefaultResponseFormatMaxBytes),
		logitBiasMaxEntries:    optOr(p.LogitBiasMaxEntries, modelalias.DefaultLogitBiasMaxEntries),
		stopMaxEntries:         optOr(p.StopMaxEntries, modelalias.DefaultStopMaxEntries),
		vision:                 p.Vision.Set && p.Vision.V,
		visionMaxBytes:         optOr(p.VisionMaxBytes, modelalias.DefaultVisionMaxBytes),
	}
	if s.MaxRequestBytes > 0 && int64(l.visionMaxBytes) > s.MaxRequestBytes {
		l.visionMaxBytes = int(s.MaxRequestBytes)
	}
	if tenantAlias(a) {
		l.toolsMaxCount = min(l.toolsMaxCount, max(s.TenantAliasCeiling.ToolsMaxCount, 0))
	}
	return l
}

// tenantAlias reports a tenant-scoped alias (the tenant ceiling and the
// export filter apply to it).
func tenantAlias(a modelalias.Alias) bool { return a.Scope != backends.GlobalScope && a.Scope != "" }

// effectiveMaxTokens is stage 3's caller ceiling (§4.5.2, B10): the client's
// value, else the alias default, else gateway.default_max_tokens — capped by
// the alias's max_output_tokens, gateway.max_output_tokens_ceiling and, for
// a tenant alias, tenant_alias_ceiling.max_output_tokens. The cap holds when
// neither the alias nor the row sets one (a row without limits is 0 = no
// cap); the row caps lower it further in llm.BuildUpstreamBody.
func effectiveMaxTokens(client int, a modelalias.Alias, s Settings) int {
	v := optOr(a.Policy.DefaultMaxTokens, s.DefaultMaxTokens)
	if client > 0 {
		v = client
	}
	caps := []int{s.MaxOutputTokensCeiling}
	if a.Policy.MaxOutputTokens.Set {
		caps = append(caps, a.Policy.MaxOutputTokens.V)
	}
	if tenantAlias(a) {
		caps = append(caps, s.TenantAliasCeiling.MaxOutputTokens)
	}
	for _, c := range caps {
		if c > 0 && c < v {
			v = c
		}
	}
	return v
}

// errParseOverloaded is parsePolicy's answer when grow refused the copy of a
// rewritten message array: the in-flight budget's 503 (§4.10.2), before the
// copy exists.
var errParseOverloaded = &paramError{code: CodeServerOverloaded}

// parsePolicy is the alias-dependent half: the PASS caps of the alias policy
// and the messages (§4.5.1), developer rewritten to system, the DROP keys of
// every message removed, every content part checked — the image parts
// before any upstream contact (B18). A rewritten message array is a copy:
// its size is offered to grow before it is allocated (FOLGE-P; nil grow
// reserves nothing), and a refusal is errParseOverloaded.
func (req *chatRequest) parsePolicy(l policyLimits, grow func(n int64) bool) *paramError {
	if perr := checkPassCaps(req.pass, l); perr != nil {
		return perr
	}
	if req.messages == nil {
		return missingParameter("messages")
	}
	msgs, dropped, perr := parseMessages(req.messages, l)
	if perr != nil {
		return perr
	}
	if msgs.size > 0 {
		if grow != nil && !grow(int64(msgs.size)) {
			return errParseOverloaded
		}
		req.messages = rewriteMessages(req.messages, msgs.size)
	}
	req.messageCount = msgs.count
	req.ignored = append(req.ignored, dropped...)
	slices.Sort(req.ignored)
	req.ignored = slices.Compact(req.ignored)
	return nil
}

// checkPassCaps applies the size caps of §3.2 to the PASS keys.
func checkPassCaps(pass map[string]json.RawMessage, l policyLimits) *paramError {
	if v, ok := pass["tools"]; ok {
		if len(v) > l.toolsMaxBytes {
			return unsupportedValue("tools", "the tool definitions exceed the size limit of this model")
		}
		if perr := checkTools(v, l.toolsMaxCount); perr != nil {
			return perr
		}
	}
	if v, ok := pass["response_format"]; ok {
		if len(v) > l.responseFormatMaxBytes {
			return unsupportedValue("response_format", "the response format exceeds the size limit of this model")
		}
		if perr := checkResponseFormat(v); perr != nil {
			return perr
		}
	}
	if v, ok := pass["tool_choice"]; ok {
		var vals [2][]byte
		if _, perr := checkHull("tool_choice", -1, v, toolHullKeys, vals[:]); perr != nil {
			return perr
		}
	}
	if v, ok := pass["logit_bias"]; ok {
		if perr := checkLogitBias(v, l.logitBiasMaxEntries); perr != nil {
			return perr
		}
	}
	if v, ok := pass["stop"]; ok && hasKind(v, kindArray) {
		if perr := checkStop(v, l.stopMaxEntries); perr != nil {
			return perr
		}
	}
	if v, ok := pass["top_logprobs"]; ok {
		// 0..20 has at most two characters ("-0" included): a longer
		// integer is out of range without being copied into a string.
		t, n, err := bytes.TrimSpace(v), 0, strconv.ErrRange
		if len(t) <= 2 {
			n, err = strconv.Atoi(string(t))
		}
		if err != nil || n < 0 || n > topLogprobsMax {
			return unsupportedValue("top_logprobs", fmt.Sprintf("must be between 0 and %d", topLogprobsMax))
		}
	}
	return nil
}

// The known keys of the checked hulls whose other keys pass verbatim (H-2):
// a tool and tool_choice ({type, function}), response_format and its
// json_schema hull. Their free payloads (function, schema) keep their keys.
var (
	toolHullKeys           = []string{"type", "function"}
	responseFormatKeys     = []string{"type", "json_schema"}
	responseFormatJSONKeys = []string{"name", "description", "schema", "strict"}
)

// checkTools counts the tools against the cap — before any hull is looked at
// — and checks every tool hull.
func checkTools(v json.RawMessage, maxCount int) *paramError {
	it, ok := newArrayIter(v)
	if !ok {
		return nil // the PASS type check let an array through; nothing to count
	}
	n := 0
	for _, more := it.next(); more; _, more = it.next() {
		n++
	}
	if n > maxCount {
		return unsupportedValue("tools", fmt.Sprintf("at most %d tools are allowed for this model", maxCount))
	}
	it, _ = newArrayIter(v)
	var vals [2][]byte
	for i := 0; ; i++ {
		t, more := it.next()
		if !more {
			return nil
		}
		if _, perr := checkHull("tools", i, t, toolHullKeys, vals[:]); perr != nil {
			return perr
		}
	}
}

// checkStop is the stop array's check: every element a string (or null, as
// a decode into a []string read it), then the count against the cap — the
// order of the decode it replaces, without decoding (P-2).
func checkStop(v json.RawMessage, maxEntries int) *paramError {
	it, _ := newArrayIter(v)
	n, typed := 0, true
	for e, more := it.next(); more; e, more = it.next() {
		n++
		typed = typed && (hasKind(e, kindString) || isNull(e))
	}
	if !typed {
		return invalidType("stop", "a string or an array of strings")
	}
	if n > maxEntries {
		return unsupportedValue("stop", fmt.Sprintf("at most %d stop sequences are allowed", maxEntries))
	}
	return nil
}

// checkLogitBias counts the entries against the cap before anything else is
// held (P-2). A repeated token id would reach the wire as often as sent
// (H-2): within the cap every repeat is found (pairwise, then sorted over at
// most maxEntries keys — memory bounded by the policy, not the body); past
// the cap only one levelKeys finds (pairwiseKeys), else the cap answers.
func checkLogitBias(v json.RawMessage, maxEntries int) *paramError {
	n := objectMembers(v)
	if n < 0 {
		return invalidType("logit_bias", "an object")
	}
	it, _ := newMemberIter(v)
	var keys levelKeys
	for k, _, more := it.next(); more; k, _, more = it.next() {
		if keys.add(k, -1) {
			return repeated("logit_bias", decodeKey(k))
		}
		if keys.n == pairwiseKeys && n > pairwiseKeys && n <= maxEntries {
			break // the sorted check below sees every key
		}
	}
	if n > maxEntries {
		return unsupportedValue("logit_bias", fmt.Sprintf("at most %d entries are allowed", maxEntries))
	}
	if n <= pairwiseKeys {
		return nil
	}
	if k := firstRepeat(v, n); k != nil {
		return repeated("logit_bias", decodeKey(k))
	}
	return nil
}

// firstRepeat is the key of the object raw (n members) whose second
// occurrence comes first in body order, nil when no key repeats.
func firstRepeat(raw []byte, n int) []byte {
	type ref struct {
		k []byte
		i int
	}
	refs := make([]ref, 0, n)
	it, _ := newMemberIter(raw)
	for k, _, more := it.next(); more; k, _, more = it.next() {
		refs = append(refs, ref{k, len(refs)})
	}
	slices.SortFunc(refs, func(a, b ref) int {
		if c := cmpKeys(a.k, b.k); c != 0 {
			return c
		}
		return a.i - b.i
	})
	best := -1
	for j := 1; j < len(refs); j++ {
		second := cmpKeys(refs[j-1].k, refs[j].k) == 0 && (j < 2 || cmpKeys(refs[j-2].k, refs[j].k) != 0)
		if second && (best < 0 || refs[j].i < refs[best].i) {
			best = j
		}
	}
	if best < 0 {
		return nil
	}
	return refs[best].k
}

// checkResponseFormat reads response_format.type from the exact key "type"
// (never a case-insensitive struct match) and checks the json_schema hull.
func checkResponseFormat(v json.RawMessage) *paramError {
	var vals [2][]byte // type, json_schema
	isObject, perr := checkHull("response_format", -1, v, responseFormatKeys, vals[:])
	if perr != nil {
		return perr
	}
	var typ string
	if !isObject || vals[0] == nil || json.Unmarshal(vals[0], &typ) != nil || !responseFormatTypes[typ] {
		return unsupportedValue("response_format.type", "must be text, json_object or json_schema")
	}
	if vals[1] != nil {
		var hull [4][]byte
		if _, perr := checkHull("response_format.json_schema", -1, vals[1], responseFormatJSONKeys, hull[:]); perr != nil {
			return perr
		}
	}
	return nil
}

// parsedMessages is what the check of the message array leaves: the number
// of messages and, when a message has to change upstream, the exact size of
// the rewritten array (0: the array goes upstream as sent).
type parsedMessages struct {
	count int
	size  int
}

// The DROP keys a message carried (parseMessage's drop bits).
const (
	dropReasoning uint8 = 1 << iota
	dropReasoningContent
)

// parseMessages checks every message (§4.5.1) without holding one: the
// array is walked in place, each message by its members (FOLGE-P). It sizes
// the upstream form — verbatim when nothing changes (no developer role, no
// DROP key), else rewriteMessages' array — and names the DROP keys it saw.
func parseMessages(raw json.RawMessage, l policyLimits) (parsedMessages, []string, *paramError) {
	it, ok := newArrayIter(raw)
	if !ok {
		return parsedMessages{}, nil, invalidType("messages", "an array of message objects")
	}
	var pm parsedMessages
	var drop uint8
	size, changed := 2, false // the brackets
	for {
		m, more := it.next()
		if !more {
			break
		}
		if pm.count > 0 {
			size++
		}
		rewritten, d, perr := parseMessage(pm.count, m, l)
		if perr != nil {
			return parsedMessages{}, nil, perr
		}
		drop |= d
		if rewritten > 0 {
			size += rewritten
			changed = true
		} else {
			size += len(m)
		}
		pm.count++
	}
	if pm.count == 0 {
		return parsedMessages{}, nil, unsupportedValue("messages", "at least one message is required")
	}
	if changed {
		pm.size = size
	}
	var dropped []string
	if drop&dropReasoning != 0 {
		dropped = append(dropped, "messages[].reasoning")
	}
	if drop&dropReasoningContent != 0 {
		dropped = append(dropped, "messages[].reasoning_content")
	}
	return pm, dropped, nil
}

// msgPath is the param of message i; partPath of its content part j — built
// only for a refusal (a string per message would be heap in the number of
// messages).
func msgPath(i int) string     { return fmt.Sprintf("messages[%d]", i) }
func partPath(i, j int) string { return fmt.Sprintf("messages[%d].content[%d]", i, j) }

// parseMessage checks message i and returns the size of its upstream form
// when it has to change (developer ⇒ system, DROP keys removed; 0 when it
// goes as sent) and the DROP keys it carried.
func parseMessage(i int, raw []byte, l policyLimits) (rewritten int, drop uint8, perr *paramError) {
	vals, perr := messageMembers(i, raw)
	if perr != nil {
		return 0, 0, perr
	}
	role, perr := messageRole(i, vals[msgRole])
	if perr != nil {
		return 0, 0, perr
	}
	if perr := checkMessageFields(i, role, &vals); perr != nil {
		return 0, 0, perr
	}
	if perr := checkContent(i, role, vals[msgContent], l); perr != nil {
		return 0, 0, perr
	}
	if vals[msgReasoning] != nil {
		drop |= dropReasoning
	}
	if vals[msgReasoningContent] != nil {
		drop |= dropReasoningContent
	}
	if role != "developer" && drop == 0 {
		return 0, 0, nil // an unchanged message goes upstream byte for byte
	}
	return rewrittenSize(&vals, role == "developer"), drop, nil
}

// messageMembers reads the members of message i into the slots of
// messageNames: not an object, a repeated key and a key outside the
// allowlist (the smallest in sort order) are refused.
func messageMembers(i int, raw []byte) (vals [messageKeyCount][]byte, perr *paramError) {
	it, ok := newMemberIter(raw)
	if !ok {
		return vals, invalidType(msgPath(i), "a message object")
	}
	var keys levelKeys
	for {
		k, v, more := it.next()
		if !more {
			break
		}
		idx := keyIndex(k, messageIndex)
		if keys.add(k, idx) {
			return vals, repeated(msgPath(i), decodeKey(k)) // an unchanged message goes upstream byte for byte
		}
		if idx < 0 {
			keys.refuse(k)
			continue
		}
		vals[idx] = v
	}
	if keys.reject != nil {
		return vals, unsupportedParameter(msgPath(i) + "." + decodeKey(keys.reject))
	}
	return vals, nil
}

// systemRole is the upstream role of a developer message.
var systemRole = []byte(`"system"`)

// messagePass marks the PASS keys among messageNames (the rewritten form
// keeps them, in this order).
var messagePass = func() (p [messageKeyCount]bool) {
	for i, k := range messageNames {
		p[i] = messagePassKeys[k]
	}
	return p
}()

// rewrittenSize is the length appendRewritten writes for the message.
func rewrittenSize(vals *[messageKeyCount][]byte, developer bool) int {
	n, first := 2, true // the braces
	for i, k := range messageNames {
		v := vals[i]
		if v == nil || !messagePass[i] {
			continue
		}
		if i == msgRole && developer {
			v = systemRole
		}
		if !first {
			n++
		}
		first = false
		n += len(k) + 3 + len(v) // "k":v
	}
	return n
}

// appendRewritten appends the upstream form of a rewritten message: its PASS
// keys in sort order (the base parse marshalled the message as a map), each
// value as sent but role "system" for developer; the DROP keys left out.
func appendRewritten(dst []byte, vals *[messageKeyCount][]byte, developer bool) []byte {
	dst = append(dst, '{')
	first := true
	for i, k := range messageNames {
		v := vals[i]
		if v == nil || !messagePass[i] {
			continue
		}
		if i == msgRole && developer {
			v = systemRole
		}
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = append(dst, k...)
		dst = append(dst, '"', ':')
		dst = append(dst, v...)
	}
	return append(dst, '}')
}

// rewriteMessages builds the upstream message array of size bytes
// (parseMessages sized it, parsePolicy reserved it): every message as sent,
// a rewritten one by appendRewritten.
func rewriteMessages(raw json.RawMessage, size int) json.RawMessage {
	out := make([]byte, 0, size)
	out = append(out, '[')
	it, _ := newArrayIter(raw)
	for i := 0; ; i++ {
		m, more := it.next()
		if !more {
			break
		}
		if i > 0 {
			out = append(out, ',')
		}
		vals, _ := messageMembers(i, m) // checked by parseMessages
		developer := isDeveloper(vals[msgRole])
		if !developer && vals[msgReasoning] == nil && vals[msgReasoningContent] == nil {
			out = append(out, m...)
			continue
		}
		out = appendRewritten(out, &vals, developer)
	}
	return append(out, ']')
}

// isDeveloper reports a checked role value "developer".
func isDeveloper(v []byte) bool {
	s, ok := stringContent(v)
	return ok && cmpKeyName(s, "developer") == 0
}

// messageRole reads the role the way a decode into a string did: absent ⇒
// missing, null ⇒ "" (not a role), not a string ⇒ invalid_type.
func messageRole(i int, v json.RawMessage) (string, *paramError) {
	if v == nil {
		return "", missingParameter(msgPath(i) + ".role")
	}
	s, isString := stringContent(v)
	if !isString && !isNull(v) {
		return "", invalidType(msgPath(i)+".role", "a string")
	}
	for _, r := range messageRoles {
		if isString && cmpKeyName(s, r) == 0 {
			return r, nil
		}
	}
	return "", unsupportedValue(msgPath(i)+".role", "must be system, developer, user, assistant or tool")
}

// messageFields are the PASS keys besides role and content and their types,
// in sort order (the base parse ranged over a map literal per message: of
// two mistyped fields it named either).
var messageFields = [...]struct {
	idx  *int
	name string
	want jsonKind
}{{&msgName, "name", kindString}, {&msgRefusal, "refusal", kindString}, {&msgToolCallID, "tool_call_id", kindString}, {&msgToolCalls, "tool_calls", kindArray}}

// checkMessageFields type-checks the PASS keys besides role and content.
func checkMessageFields(i int, role string, vals *[messageKeyCount][]byte) *paramError {
	for _, f := range messageFields {
		if v := vals[*f.idx]; v != nil && !isNull(v) && !hasKind(v, f.want) {
			return invalidType(msgPath(i)+"."+f.name, kindName(f.want))
		}
	}
	if vals[msgToolCalls] != nil && role != "assistant" {
		return unsupportedParameter(msgPath(i) + ".tool_calls")
	}
	return nil
}

// checkContent checks content over every role (§4.5.1): a string, an array
// of parts, or — for an assistant message carrying tool calls — null.
func checkContent(i int, role string, v json.RawMessage, l policyLimits) *paramError {
	switch {
	case v == nil || isNull(v):
		if role == "assistant" {
			return nil
		}
		return missingParameter(msgPath(i) + ".content")
	case hasKind(v, kindString):
		return nil
	case hasKind(v, kindArray):
	default:
		return invalidType(msgPath(i)+".content", "a string or an array of content parts")
	}
	it, _ := newArrayIter(v)
	for j := 0; ; j++ {
		p, more := it.next()
		if !more {
			return nil
		}
		if perr := checkPart(i, j, p, l); perr != nil {
			return perr
		}
	}
}

// partTypeOf reads a part's type the way a decode into a string did: absent
// or not a string ⇒ ok false (missing), null ⇒ "" (an unsupported type).
func partTypeOf(v []byte) (typ string, ok bool) {
	if v == nil {
		return "", false
	}
	if isNull(v) {
		return "", true
	}
	s, ok := stringContent(v)
	switch {
	case !ok:
		return "", false
	case cmpKeyName(s, "text") == 0:
		return "text", true
	case cmpKeyName(s, "image_url") == 0:
		return "image_url", true
	}
	return "other", true
}

// checkPart checks one content part: text, or image_url under the vision
// rule. Every other part type (input_audio, file, video_url, …) is refused,
// and so is a key outside the type's allowlist — the smallest in sort order
// (the base parse ranged over a map: of several it named either).
func checkPart(i, j int, raw []byte, l policyLimits) *paramError {
	it, ok := newMemberIter(raw)
	if !ok {
		return invalidType(partPath(i, j), "a content part object")
	}
	var vals [3][]byte
	var keys levelKeys
	for {
		k, v, more := it.next()
		if !more {
			break
		}
		idx := keyIndex(k, partIndex)
		if keys.add(k, idx) {
			return repeated(partPath(i, j), decodeKey(k))
		}
		if idx < 0 {
			keys.refuse(k)
			continue
		}
		vals[idx] = v
	}
	typ, ok := partTypeOf(vals[partType])
	if !ok {
		return missingParameter(partPath(i, j) + ".type")
	}
	other, refused := "", false // the key refused (it may be "")
	switch typ {
	case "text":
		if !hasKind(vals[partText], kindString) {
			return invalidType(partPath(i, j)+".text", "a string")
		}
		if vals[partImage] != nil {
			other, refused = "image_url", true
		}
	case "image_url":
		if !l.vision {
			return unsupportedValue(partPath(i, j)+".image_url.url", "this model does not accept images")
		}
		if perr := checkImageURL(i, j, vals[partImage], l.visionMaxBytes); perr != nil {
			return perr
		}
		if vals[partText] != nil {
			other, refused = "text", true
		}
	default:
		return unsupportedValue(partPath(i, j)+".type", "unsupported content part type")
	}
	if keys.reject != nil {
		if !refused || cmpKeyName(keys.reject, other) < 0 {
			other, refused = decodeKey(keys.reject), true
		}
	}
	if refused {
		return unsupportedParameter(partPath(i, j) + "." + other)
	}
	return nil
}

// checkImageURL is the vision rule (§4.5.1, B18): url exactly one of the
// four data-URI prefixes, the payload valid base64 and its decoded size ≤
// the policy's vision_max_bytes; detail PASS. The url is checked in its
// decoded form without being decoded into a string (FOLGE-P: an escaped
// "\/" — what PHP's json_encode writes — must not cost a copy of the image).
func checkImageURL(i, j int, raw []byte, maxBytes int) *paramError {
	at := func() string { return partPath(i, j) + ".image_url" }
	it, ok := newMemberIter(raw)
	if !ok {
		return invalidType(at(), "an object")
	}
	var vals [2][]byte
	var keys levelKeys
	for {
		k, v, more := it.next()
		if !more {
			break
		}
		idx := keyIndex(k, imageIndex)
		if keys.add(k, idx) {
			return repeated(at(), decodeKey(k)) // two urls: the checked one and the one an engine reads may differ (B18)
		}
		if idx < 0 {
			keys.refuse(k)
			continue
		}
		vals[idx] = v
	}
	if keys.reject != nil {
		return unsupportedParameter(at() + "." + decodeKey(keys.reject))
	}
	if d := vals[imageDetail]; d != nil && !hasKind(d, kindString) {
		return invalidType(at()+".detail", "a string")
	}
	u := vals[imageURL]
	s, isString := stringContent(u)
	if !isString && (u == nil || !isNull(u)) {
		return invalidType(at()+".url", "a string")
	}
	n, prefix, valid := dataImageSize(s)
	switch {
	case !prefix:
		return unsupportedValue(at()+".url", "only base64 data URIs of png, jpeg, webp or gif images are accepted")
	case !valid:
		return unsupportedValue(at()+".url", "the image data is not valid base64")
	case n > maxBytes:
		return unsupportedValue(at()+".url", "the image exceeds the size limit of this model")
	}
	return nil
}

// visionPrefixMax is the longest of visionPrefixes.
const visionPrefixMax = len("data:image/jpeg;base64,")

// dataImageSize reads the decoded url (raw JSON string content s) as one of
// visionPrefixes and a standard padded base64 payload: the payload's decoded
// size, whether a prefix matched and whether the payload is valid. No
// prefix is the prefix of another, so the first full match is the one.
func dataImageSize(s []byte) (n int, prefix, valid bool) {
	d := strDecoder{raw: s}
	var head [visionPrefixMax]byte
	h := 0
	var b64 base64Check
	for c, ok := d.next(); ok; c, ok = d.next() {
		if prefix {
			b64.add(c)
			continue
		}
		head[h] = c
		h++
		for _, p := range visionPrefixes {
			if len(p) == h && string(head[:h]) == p {
				prefix = true
			}
		}
		if !prefix && h == len(head) {
			return 0, false, false
		}
	}
	if !prefix {
		return 0, false, false
	}
	n, valid = b64.size()
	return n, true, valid
}

// strictBase64 is the strict decoder of the last quantum (one value: Strict
// returns a fresh Encoding per call).
var strictBase64 = base64.StdEncoding.Strict()

// base64Check validates a byte stream as standard padded base64 without
// holding it: the length a multiple of 4, every byte of the alphabet except a
// trailing run of at most two '=', and the last quantum canonical (its
// padding bits zero, as a strict decoder demands).
type base64Check struct {
	n, pad int
	bad    bool
	last   [4]byte
}

func (b *base64Check) add(c byte) {
	b.last[b.n&3] = c
	b.n++
	switch {
	case c == '=':
		b.pad++
	case isBase64Char(c) && b.pad == 0:
	default:
		b.bad = true
	}
}

// size is the decoded length of a valid stream.
func (b *base64Check) size() (int, bool) {
	if b.bad || b.n == 0 || b.n%4 != 0 || b.pad > 2 {
		return 0, false
	}
	var dst [3]byte
	if _, err := strictBase64.Decode(dst[:], b.last[:]); err != nil {
		return 0, false
	}
	return b.n/4*3 - b.pad, true
}

// isBase64Char reports a character of the standard base64 alphabet.
func isBase64Char(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
}

// --- JSON kind helpers ---.

func isNull(v json.RawMessage) bool { return string(bytes.TrimSpace(v)) == "null" }

// kindOf is the coarse type of a raw JSON value by its first byte; an integer
// is a number literal without fraction or exponent.
func kindOf(v json.RawMessage) jsonKind {
	t := bytes.TrimSpace(v)
	if len(t) == 0 {
		return 0
	}
	switch c := t[0]; {
	case c == '"':
		return kindString
	case c == '{':
		return kindObject
	case c == '[':
		return kindArray
	case c == 't' || c == 'f':
		return kindBool
	case c == '-' || c >= '0' && c <= '9':
		if bytes.ContainsAny(t, ".eE") {
			return kindNumber
		}
		return kindNumber | kindInteger
	}
	return 0
}

func hasKind(v json.RawMessage, want jsonKind) bool { return kindOf(v)&want != 0 }

func kindName(k jsonKind) string {
	var names []string
	for _, kn := range []struct {
		k    jsonKind
		name string
	}{{kindInteger, "an integer"}, {kindNumber, "a number"}, {kindString, "a string"}, {kindBool, "a boolean"}, {kindArray, "an array"}, {kindObject, "an object"}} {
		if k&kn.k != 0 && (kn.k != kindNumber || k&kindInteger == 0) {
			names = append(names, kn.name)
		}
	}
	return strings.Join(names, " or ")
}
