package oai

// The parse of POST /v1/chat/completions as it stood on 4839a958 (W02-07 +
// H-2, before FOLGE-P), kept verbatim as the test oracle of the FOLGE-P
// differential (chat_parse_differential_test.go): every top-level identifier
// carries the prefix "base", nothing else changed. The shared vocabulary
// (paramError and its constructors, policyLimits) is the production one.
// Not production code; never edit it except to rename — and for the one
// hunk the parse took besides FOLGE-P: W02-08's readCTX (8e00a12e..49289c7b:
// stream true accepted, stream_options.include_usage read), applied
// verbatim in the merge of the two waves, so the oracle stays the parse
// FOLGE-P replaced.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// baseKeyClass is the class of one request key.
type baseKeyClass int

const (
	baseClassReject baseKeyClass = iota
	baseClassPass
	baseClassCTX
	baseClassDrop
)

// baseJSONKind is the coarse JSON type of a PASS value (L1: "Typ-Grobprüfung").
type baseJSONKind int

const (
	baseKindNumber baseJSONKind = 1 << iota
	baseKindInteger
	baseKindString
	baseKindBool
	baseKindArray
	baseKindObject
)

// basePassKeys are the PASS keys of the top level and their JSON types
// (design/02 §4.5 table; the extensions top_k, min_p, repetition_penalty
// are what ctx-B itself sends and all three engines know — L7). messages is
// PASS too but parsed on its own (§4.5.1).
var basePassKeys = map[string]baseJSONKind{
	"temperature":         baseKindNumber,
	"top_p":               baseKindNumber,
	"stop":                baseKindString | baseKindArray,
	"presence_penalty":    baseKindNumber,
	"frequency_penalty":   baseKindNumber,
	"logit_bias":          baseKindObject,
	"logprobs":            baseKindBool,
	"top_logprobs":        baseKindInteger,
	"seed":                baseKindInteger,
	"response_format":     baseKindObject,
	"tools":               baseKindArray,
	"tool_choice":         baseKindString | baseKindObject,
	"parallel_tool_calls": baseKindBool,
	"top_k":               baseKindNumber,
	"min_p":               baseKindNumber,
	"repetition_penalty":  baseKindNumber,
}

// baseCtxKeys are read and overlaid by ctx (§4.5 class CTX): the client's value
// never reaches the wire as sent (llm.BuildUpstreamBody drops them and
// writes its frame). reasoning is the OpenRouter extension ctx-B sends on
// every stream (L7) — accepted, never forwarded; the alias policy decides
// (design/02 §3.2 reasoning, RC-B-INDEX Q15).
var baseCtxKeys = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true, "max_tokens": true,
	"max_completion_tokens": true, "n": true, "service_tier": true, "reasoning": true, "reasoning_effort": true,
}

// baseDropKeys are removed and named in X-Ctx-Ignored-Params (§4.5 class DROP):
// known client and vendor fields that must not break a client (LiteLLM sends
// priority, L7) and the engine internals of SGLang, vLLM and llama.cpp that
// would steer scheduling, caching, adapters or the request identity
// (design/02 §5 B1). modalities is dropped only as ["text"] (its default);
// anything else is REJECT (baseParseTopLevel).
var baseDropKeys = map[string]bool{
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
	baseMessagePassKeys = map[string]bool{"role": true, "content": true, "name": true, "tool_calls": true, "tool_call_id": true, "refusal": true}
	baseMessageDropKeys = map[string]bool{"reasoning": true, "reasoning_content": true}
	baseMessageRoles    = map[string]bool{"system": true, "user": true, "assistant": true, "tool": true, "developer": true}
)

// baseServiceTiers are the service_tier values of the OpenAI spec; the planner
// reads them (flex lowers the lane to bulk, design/03 §4.4).
var baseServiceTiers = map[string]bool{"auto": true, "default": true, "flex": true, "scale": true, "priority": true}

// baseResponseFormatTypes are the response_format.type values PASS admits
// (§4.5: text, json_object, json_schema).
var baseResponseFormatTypes = map[string]bool{"text": true, "json_object": true, "json_schema": true}

// baseVisionPrefixes are the only image_url.url forms (§4.5.1, B18): a data URI
// of one of four image types, lowercase, without parameters. Engines fetch
// every other form server-side — http(s) into the LAN, file:// and bare
// paths from the serving host (SGLang load_image) — so nothing else passes.
var baseVisionPrefixes = []string{
	"data:image/png;base64,", "data:image/jpeg;base64,", "data:image/webp;base64,", "data:image/gif;base64,",
}

// baseTopLogprobsMax is the spec's bound of top_logprobs (OpenAI: 0..20).
const baseTopLogprobsMax = 20

// baseChatRequest is the parsed client request: the PASS keys for the wire, the
// rewritten messages, the CTX values ctx reads and the DROP names.
type baseChatRequest struct {
	raw          map[string]json.RawMessage
	model        string
	pass         map[string]json.RawMessage
	messages     json.RawMessage
	stream       bool
	includeUsage bool // stream_options.include_usage (W02-08: the usage chunk reaches the client only then)
	n            int
	maxTokens    int // the client's max_tokens / max_completion_tokens, 0 = not sent
	serviceTier  string
	ignored      []string
	messageCount int
}

// baseParseTopLevel is the alias-independent half of the parse: JSON, the class
// of every top-level key (unknown ⇒ 400 before the model is resolved, so the
// answer says nothing about any alias), the model name and the CTX values.
// The message and policy checks follow once the alias is known (parsePolicy).
func baseParseTopLevel(body []byte) (*baseChatRequest, *paramError) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, &paramError{CodeInvalidJSON, "", "The request body is not a valid JSON object."}
	}
	if perr := baseUniqueMembers("", body, raw); perr != nil {
		return nil, perr // H-2: the checked value is the sent value
	}
	req := &baseChatRequest{raw: raw, pass: make(map[string]json.RawMessage, len(raw))}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the first refused key is deterministic
	for _, k := range keys {
		v := raw[k]
		switch baseClassOf(k) {
		case baseClassReject:
			return nil, unsupportedParameter(k)
		case baseClassDrop:
			if k == "modalities" && !baseTextOnlyModalities(v) {
				return nil, unsupportedParameter(k) // REJECT unless ["text"] (§4.5)
			}
			req.ignored = append(req.ignored, k)
		case baseClassPass:
			if baseIsNull(v) {
				continue // null = the default; nothing to forward
			}
			if !baseHasKind(v, basePassKeys[k]) {
				return nil, invalidType(k, baseKindName(basePassKeys[k]))
			}
			req.pass[k] = v
		case baseClassCTX:
			if perr := req.readCTX(k, v); perr != nil {
				return nil, perr
			}
		}
	}
	if req.model == "" {
		return nil, missingParameter("model")
	}
	return req, nil
}

func baseClassOf(k string) baseKeyClass {
	switch {
	case baseCtxKeys[k]:
		return baseClassCTX
	case k == "messages":
		return baseClassCTX
	case basePassKeys[k] != 0:
		return baseClassPass
	case baseDropKeys[k]:
		return baseClassDrop
	}
	return baseClassReject
}

// readCTX reads one CTX key. n > 1 is refused here (E4: RC-F) — before any
// alias is resolved. stream and stream_options.include_usage are read (W02-08);
// upstream, a stream always asks for usage (llm.BuildUpstreamBody).
func (req *baseChatRequest) readCTX(k string, v json.RawMessage) *paramError {
	if baseIsNull(v) {
		return nil
	}
	switch k {
	case "model":
		if err := json.Unmarshal(v, &req.model); err != nil {
			return invalidType(k, "a string")
		}
	case "messages":
		req.messages = v
	case "stream":
		if err := json.Unmarshal(v, &req.stream); err != nil {
			return invalidType(k, "a boolean")
		}
	case "stream_options":
		if !baseHasKind(v, baseKindObject) {
			return invalidType(k, "an object")
		}
		var o struct {
			IncludeUsage *json.RawMessage `json:"include_usage"`
		}
		if err := json.Unmarshal(v, &o); err != nil {
			return invalidType(k, "an object")
		}
		if o.IncludeUsage != nil && !baseIsNull(*o.IncludeUsage) {
			if err := json.Unmarshal(*o.IncludeUsage, &req.includeUsage); err != nil {
				return invalidType(k+".include_usage", "a boolean")
			}
		}
	case "reasoning":
		if !baseHasKind(v, baseKindObject) {
			return invalidType(k, "an object")
		}
	case "reasoning_effort":
		if !baseHasKind(v, baseKindString) {
			return invalidType(k, "a string")
		}
	case "service_tier":
		if err := json.Unmarshal(v, &req.serviceTier); err != nil {
			return invalidType(k, "a string")
		}
		if !baseServiceTiers[req.serviceTier] {
			return unsupportedValue(k, "unknown service tier")
		}
	case "n":
		if err := json.Unmarshal(v, &req.n); err != nil || !baseHasKind(v, baseKindInteger) {
			return invalidType(k, "an integer")
		}
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
func (req *baseChatRequest) readMaxTokens(k string, v json.RawMessage) *paramError {
	var n int
	if err := json.Unmarshal(v, &n); err != nil || !baseHasKind(v, baseKindInteger) {
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

func baseTextOnlyModalities(v json.RawMessage) bool {
	var m []string
	return json.Unmarshal(v, &m) == nil && len(m) == 1 && m[0] == "text"
}

// parsePolicy is the alias-dependent half: the PASS caps of the alias policy
// and the messages (§4.5.1), developer rewritten to system, the DROP keys of
// every message removed, every content part checked — the image parts
// before any upstream contact (B18).
func (req *baseChatRequest) parsePolicy(l policyLimits) *paramError {
	if perr := baseCheckPassCaps(req.pass, l); perr != nil {
		return perr
	}
	if req.messages == nil {
		return missingParameter("messages")
	}
	msgs, dropped, perr := baseParseMessages(req.messages, l)
	if perr != nil {
		return perr
	}
	req.messages = msgs.raw
	req.messageCount = msgs.count
	req.ignored = append(req.ignored, dropped...)
	slices.Sort(req.ignored)
	req.ignored = slices.Compact(req.ignored)
	return nil
}

// baseCheckPassCaps applies the size caps of §3.2 to the PASS keys.
func baseCheckPassCaps(pass map[string]json.RawMessage, l policyLimits) *paramError {
	if v, ok := pass["tools"]; ok {
		if len(v) > l.toolsMaxBytes {
			return unsupportedValue("tools", "the tool definitions exceed the size limit of this model")
		}
		if perr := baseCheckTools(v, l.toolsMaxCount); perr != nil {
			return perr
		}
	}
	if v, ok := pass["response_format"]; ok {
		if len(v) > l.responseFormatMaxBytes {
			return unsupportedValue("response_format", "the response format exceeds the size limit of this model")
		}
		if perr := baseCheckResponseFormat(v); perr != nil {
			return perr
		}
	}
	if v, ok := pass["tool_choice"]; ok {
		var vals [2][]byte
		if _, perr := baseCheckHull("tool_choice", -1, v, baseToolHullKeys, vals[:]); perr != nil {
			return perr
		}
	}
	if v, ok := pass["logit_bias"]; ok {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return invalidType("logit_bias", "an object")
		}
		// A baseRepeated token id would count once and reach the wire as
		// often as sent: the entry cap counts the map (H-2).
		if perr := baseUniqueMembers("logit_bias", v, m); perr != nil {
			return perr
		}
		if len(m) > l.logitBiasMaxEntries {
			return unsupportedValue("logit_bias", fmt.Sprintf("at most %d entries are allowed", l.logitBiasMaxEntries))
		}
	}
	if v, ok := pass["stop"]; ok && baseHasKind(v, baseKindArray) {
		var stops []string
		if err := json.Unmarshal(v, &stops); err != nil {
			return invalidType("stop", "a string or an array of strings")
		}
		if len(stops) > l.stopMaxEntries {
			return unsupportedValue("stop", fmt.Sprintf("at most %d stop sequences are allowed", l.stopMaxEntries))
		}
	}
	if v, ok := pass["top_logprobs"]; ok {
		n, err := strconv.Atoi(string(bytes.TrimSpace(v)))
		if err != nil || n < 0 || n > baseTopLogprobsMax {
			return unsupportedValue("top_logprobs", fmt.Sprintf("must be between 0 and %d", baseTopLogprobsMax))
		}
	}
	return nil
}

// The known keys of the checked hulls whose other keys pass verbatim (H-2):
// a tool and tool_choice ({type, function}), response_format and its
// json_schema hull. Their free payloads (function, schema) keep their keys.
var (
	baseToolHullKeys           = []string{"type", "function"}
	baseResponseFormatKeys     = []string{"type", "json_schema"}
	baseResponseFormatJSONKeys = []string{"name", "description", "schema", "strict"}
)

// baseCheckTools counts the tools against the cap and checks every tool hull.
func baseCheckTools(v json.RawMessage, maxCount int) *paramError {
	var arr []json.RawMessage
	if err := json.Unmarshal(v, &arr); err != nil {
		return nil // the PASS type check let an array through; nothing to count
	}
	if len(arr) > maxCount {
		return unsupportedValue("tools", fmt.Sprintf("at most %d tools are allowed for this model", maxCount))
	}
	var vals [2][]byte
	for i, t := range arr {
		if _, perr := baseCheckHull("tools", i, t, baseToolHullKeys, vals[:]); perr != nil {
			return perr
		}
	}
	return nil
}

// baseCheckResponseFormat reads response_format.type from the exact key "type"
// (never a case-insensitive struct match) and checks the json_schema hull.
func baseCheckResponseFormat(v json.RawMessage) *paramError {
	var vals [2][]byte // type, json_schema
	isObject, perr := baseCheckHull("response_format", -1, v, baseResponseFormatKeys, vals[:])
	if perr != nil {
		return perr
	}
	var typ string
	if !isObject || vals[0] == nil || json.Unmarshal(vals[0], &typ) != nil || !baseResponseFormatTypes[typ] {
		return unsupportedValue("response_format.type", "must be text, json_object or json_schema")
	}
	if vals[1] != nil {
		var hull [4][]byte
		if _, perr := baseCheckHull("response_format.json_schema", -1, vals[1], baseResponseFormatJSONKeys, hull[:]); perr != nil {
			return perr
		}
	}
	return nil
}

// baseParsedMessages is the message array as it goes upstream.
type baseParsedMessages struct {
	raw   json.RawMessage
	count int
}

// baseParseMessages checks every message (§4.5.1) and returns the array as it
// goes upstream: verbatim when nothing changed (no developer role, no DROP
// key), else re-encoded from the per-message raw values — a value is never
// decoded into Go types, so a client number or string reaches the wire as
// sent.
func baseParseMessages(raw json.RawMessage, l policyLimits) (baseParsedMessages, []string, *paramError) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return baseParsedMessages{}, nil, invalidType("messages", "an array of message objects")
	}
	if len(arr) == 0 {
		return baseParsedMessages{}, nil, unsupportedValue("messages", "at least one message is required")
	}
	var dropped []string
	changed := false
	for i, m := range arr {
		out, drop, rewrote, perr := baseParseMessage(i, m, l)
		if perr != nil {
			return baseParsedMessages{}, nil, perr
		}
		dropped = append(dropped, drop...)
		if rewrote {
			arr[i] = out
			changed = true
		}
	}
	if !changed {
		return baseParsedMessages{raw: raw, count: len(arr)}, dropped, nil
	}
	enc, err := json.Marshal(arr)
	if err != nil {
		return baseParsedMessages{}, nil, &paramError{CodeInvalidJSON, "messages", "The messages could not be encoded."}
	}
	return baseParsedMessages{raw: enc, count: len(arr)}, dropped, nil
}

// baseParseMessage checks message i and returns its upstream form when it had
// to change (developer ⇒ system, DROP keys removed).
func baseParseMessage(i int, raw json.RawMessage, l policyLimits) (json.RawMessage, []string, bool, *paramError) {
	at := fmt.Sprintf("messages[%d]", i)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, nil, false, invalidType(at, "a message object")
	}
	if perr := baseUniqueMembers(at, raw, m); perr != nil {
		return nil, nil, false, perr // an unchanged message goes upstream byte for byte
	}
	var dropped []string
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case baseMessagePassKeys[k]:
		case baseMessageDropKeys[k]:
			dropped = append(dropped, "messages[]."+k)
		default:
			return nil, nil, false, unsupportedParameter(at + "." + k)
		}
	}
	role, perr := baseMessageRole(at, m["role"])
	if perr != nil {
		return nil, nil, false, perr
	}
	if perr := baseCheckMessageFields(at, role, m); perr != nil {
		return nil, nil, false, perr
	}
	if perr := baseCheckContent(at, role, m["content"], l); perr != nil {
		return nil, nil, false, perr
	}
	if role != "developer" && len(dropped) == 0 {
		return nil, nil, false, nil
	}
	for _, k := range dropped {
		delete(m, strings.TrimPrefix(k, "messages[]."))
	}
	if role == "developer" {
		m["role"] = json.RawMessage(`"system"`)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, false, invalidType(at, "a message object")
	}
	return out, dropped, true, nil
}

func baseMessageRole(at string, v json.RawMessage) (string, *paramError) {
	if v == nil {
		return "", missingParameter(at + ".role")
	}
	var role string
	if err := json.Unmarshal(v, &role); err != nil {
		return "", invalidType(at+".role", "a string")
	}
	if !baseMessageRoles[role] {
		return "", unsupportedValue(at+".role", "must be system, developer, user, assistant or tool")
	}
	return role, nil
}

// baseCheckMessageFields type-checks the PASS keys besides role and content.
func baseCheckMessageFields(at, role string, m map[string]json.RawMessage) *paramError {
	for k, want := range map[string]baseJSONKind{"name": baseKindString, "tool_call_id": baseKindString, "tool_calls": baseKindArray, "refusal": baseKindString} {
		if v, ok := m[k]; ok && !baseIsNull(v) && !baseHasKind(v, want) {
			return invalidType(at+"."+k, baseKindName(want))
		}
	}
	if _, ok := m["tool_calls"]; ok && role != "assistant" {
		return unsupportedParameter(at + ".tool_calls")
	}
	return nil
}

// baseCheckContent checks content over every role (§4.5.1): a string, an array
// of parts, or — for an assistant message carrying tool calls — null.
func baseCheckContent(at, role string, v json.RawMessage, l policyLimits) *paramError {
	switch {
	case v == nil || baseIsNull(v):
		if role == "assistant" {
			return nil
		}
		return missingParameter(at + ".content")
	case baseHasKind(v, baseKindString):
		return nil
	case baseHasKind(v, baseKindArray):
	default:
		return invalidType(at+".content", "a string or an array of content parts")
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(v, &parts); err != nil {
		return invalidType(at+".content", "an array of content parts")
	}
	for j, p := range parts {
		if perr := baseCheckPart(fmt.Sprintf("%s.content[%d]", at, j), p, l); perr != nil {
			return perr
		}
	}
	return nil
}

// baseCheckPart checks one content part: text, or image_url under the vision
// rule. Every other part type (input_audio, file, video_url, …) is refused.
func baseCheckPart(at string, raw json.RawMessage, l policyLimits) *paramError {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(raw, &p); err != nil || p == nil {
		return invalidType(at, "a content part object")
	}
	if perr := baseUniqueMembers(at, raw, p); perr != nil {
		return perr
	}
	var typ string
	if err := json.Unmarshal(p["type"], &typ); err != nil {
		return missingParameter(at + ".type")
	}
	allowed := map[string]bool{"type": true}
	switch typ {
	case "text":
		allowed["text"] = true
		if !baseHasKind(p["text"], baseKindString) {
			return invalidType(at+".text", "a string")
		}
	case "image_url":
		if !l.vision {
			return unsupportedValue(at+".image_url.url", "this model does not accept images")
		}
		allowed["image_url"] = true
		if perr := baseCheckImageURL(at+".image_url", p["image_url"], l.visionMaxBytes); perr != nil {
			return perr
		}
	default:
		return unsupportedValue(at+".type", "unsupported content part type")
	}
	for k := range p {
		if !allowed[k] {
			return unsupportedParameter(at + "." + k)
		}
	}
	return nil
}

// baseCheckImageURL is the vision rule (§4.5.1, B18): url exactly one of the
// four data-URI prefixes, the payload valid base64 and its decoded size ≤
// the policy's vision_max_bytes; detail PASS.
func baseCheckImageURL(at string, raw json.RawMessage, maxBytes int) *paramError {
	var img map[string]json.RawMessage
	if err := json.Unmarshal(raw, &img); err != nil || img == nil {
		return invalidType(at, "an object")
	}
	if perr := baseUniqueMembers(at, raw, img); perr != nil {
		return perr // two urls: the checked one and the one an engine reads may differ (B18)
	}
	for k := range img {
		if k != "url" && k != "detail" {
			return unsupportedParameter(at + "." + k)
		}
	}
	if d, ok := img["detail"]; ok && !baseHasKind(d, baseKindString) {
		return invalidType(at+".detail", "a string")
	}
	var url string
	if err := json.Unmarshal(img["url"], &url); err != nil {
		return invalidType(at+".url", "a string")
	}
	i := slices.IndexFunc(baseVisionPrefixes, func(p string) bool { return strings.HasPrefix(url, p) })
	if i < 0 {
		return unsupportedValue(at+".url", "only base64 data URIs of png, jpeg, webp or gif images are accepted")
	}
	payload := url[len(baseVisionPrefixes[i]):]
	n, ok := baseBase64Size(payload)
	switch {
	case !ok:
		return unsupportedValue(at+".url", "the image data is not valid base64")
	case n > maxBytes:
		return unsupportedValue(at+".url", "the image exceeds the size limit of this model")
	}
	return nil
}

// baseBase64Size validates s as standard padded base64 without allocating its
// decoded form and returns the decoded length.
func baseBase64Size(s string) (int, bool) {
	if len(s) == 0 || len(s)%4 != 0 {
		return 0, false
	}
	pad := 0
	for i := len(s) - 1; i >= len(s)-2 && s[i] == '='; i-- {
		pad++
	}
	for i := 0; i < len(s)-pad; i++ {
		if !baseIsBase64Char(s[i]) {
			return 0, false
		}
	}
	// The last quantum carries the padding bits: decode it to reject
	// non-canonical trailing bits the way a strict decoder would.
	if _, err := base64.StdEncoding.Strict().DecodeString(s[len(s)-4:]); err != nil {
		return 0, false
	}
	return len(s)/4*3 - pad, true
}

// baseIsBase64Char reports a character of the standard base64 alphabet.
func baseIsBase64Char(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
}

// --- JSON kind helpers ---.

func baseIsNull(v json.RawMessage) bool { return string(bytes.TrimSpace(v)) == "null" }

// baseKindOf is the coarse type of a raw JSON value by its first byte; an integer
// is a number literal without fraction or exponent.
func baseKindOf(v json.RawMessage) baseJSONKind {
	t := bytes.TrimSpace(v)
	if len(t) == 0 {
		return 0
	}
	switch c := t[0]; {
	case c == '"':
		return baseKindString
	case c == '{':
		return baseKindObject
	case c == '[':
		return baseKindArray
	case c == 't' || c == 'f':
		return baseKindBool
	case c == '-' || c >= '0' && c <= '9':
		if bytes.ContainsAny(t, ".eE") {
			return baseKindNumber
		}
		return baseKindNumber | baseKindInteger
	}
	return 0
}

func baseHasKind(v json.RawMessage, want baseJSONKind) bool { return baseKindOf(v)&want != 0 }

func baseKindName(k baseJSONKind) string {
	var names []string
	for _, kn := range []struct {
		k    baseJSONKind
		name string
	}{{baseKindInteger, "an integer"}, {baseKindNumber, "a number"}, {baseKindString, "a string"}, {baseKindBool, "a boolean"}, {baseKindArray, "an array"}, {baseKindObject, "an object"}} {
		if k&kn.k != 0 && (kn.k != baseKindNumber || k&baseKindInteger == 0) {
			names = append(names, kn.name)
		}
	}
	return strings.Join(names, " or ")
}

// baseMemberIter walks the members of one JSON object without decoding or
// allocating. raw must be valid JSON (json.Unmarshal accepted it); a shape
// the walk does not expect sets bad.
type baseMemberIter struct {
	raw     []byte
	i       int
	started bool
	bad     bool
}

// baseNewMemberIter starts a walk over raw; false when raw is no JSON object.
func baseNewMemberIter(raw []byte) (baseMemberIter, bool) {
	i := baseSkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return baseMemberIter{}, false
	}
	return baseMemberIter{raw: raw, i: i + 1}, true
}

// next returns the next member: its key as sent (between the quotes, escapes
// untouched) and its raw value; ok false at the end (or on a bad shape).
func (it *baseMemberIter) next() (key, val []byte, ok bool) {
	raw := it.raw
	i := baseSkipSpace(raw, it.i)
	if i >= len(raw) {
		it.bad = true
		return nil, nil, false
	}
	switch {
	case raw[i] == '}':
		return nil, nil, false
	case it.started && raw[i] == ',':
		i = baseSkipSpace(raw, i+1)
	case it.started:
		it.bad = true
		return nil, nil, false
	}
	it.started = true
	if i >= len(raw) || raw[i] != '"' {
		it.bad = true
		return nil, nil, false
	}
	end := baseSkipString(raw, i)
	if end > len(raw) || end-1 <= i {
		it.bad = true
		return nil, nil, false
	}
	key = raw[i+1 : end-1]
	j := baseSkipSpace(raw, end)
	if j >= len(raw) || raw[j] != ':' {
		it.bad = true
		return nil, nil, false
	}
	j = baseSkipSpace(raw, j+1)
	vend := baseSkipValue(raw, j)
	it.i = vend
	return key, raw[j:vend], true
}

// baseObjectMembers counts the members of the JSON object raw; -1 when raw is
// no object (never equal to a map's size). Two members whose names decode to
// the same string (an escape such as "url", invalid UTF-8 replaced by
// U+FFFD) count twice: a map of the same object is then smaller — exactly
// the duplicate.
func baseObjectMembers(raw []byte) int {
	it, ok := baseNewMemberIter(raw)
	if !ok {
		return -1
	}
	n := 0
	for {
		if _, _, more := it.next(); !more {
			break
		}
		n++
	}
	if it.bad {
		return -1
	}
	return n
}

func baseSkipSpace(raw []byte, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
		i++
	}
	return i
}

// baseSkipString returns the index after the string that starts at raw[i] ('"').
// The closing quote is the first quote not preceded by an odd run of
// backslashes; bytes.IndexByte jumps over the content (a data-URI image is
// megabytes of one string).
func baseSkipString(raw []byte, i int) int {
	j := i + 1
	for {
		k := bytes.IndexByte(raw[j:], '"')
		if k < 0 {
			return len(raw) + 1
		}
		q := j + k
		bs := 0
		for p := q - 1; p > i && raw[p] == '\\'; p-- {
			bs++
		}
		if bs%2 == 0 {
			return q + 1
		}
		j = q + 1
	}
}

// baseSkipValue returns the index after the value that starts at raw[i]: a
// string, a container (strings inside skipped), or a literal.
func baseSkipValue(raw []byte, i int) int {
	if i >= len(raw) {
		return i
	}
	switch raw[i] {
	case '"':
		return min(baseSkipString(raw, i), len(raw))
	case '{', '[':
		depth := 0
		for i < len(raw) {
			switch raw[i] {
			case '"':
				i = min(baseSkipString(raw, i), len(raw))
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for ; i < len(raw); i++ {
		switch raw[i] {
		case ',', '}', ']', ' ', '\t', '\r', '\n':
			return i
		}
	}
	return i
}

// baseRepeatedKey names the first member name of the object raw that occurs
// twice after decoding — the error path only, so it may allocate.
func baseRepeatedKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		k, _ := tok.(string)
		if seen[k] {
			return k
		}
		seen[k] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
}

// baseKeyPath is the param of key on the level at ("" = the top level).
func baseKeyPath(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

func baseRepeated(at, key string) *paramError {
	param := at
	if key != "" {
		param = baseKeyPath(at, key)
	}
	return &paramError{CodeInvalidJSON, param, fmt.Sprintf("The request repeats the key %s.", param)}
}

// baseUniqueMembers refuses an object whose map form m (decoded from raw) lost a
// member to a baseRepeated key.
func baseUniqueMembers(at string, raw []byte, m map[string]json.RawMessage) *paramError {
	if baseObjectMembers(raw) == len(m) {
		return nil
	}
	return baseRepeated(at, baseRepeatedKey(raw))
}

// baseHullPairwiseMax bounds the allocation-free duplicate check of a hull
// level (pairwise over its keys): a real hull has two to four keys; a
// larger one takes the decoding path, so the check stays linear.
const baseHullPairwiseMax = 16

// baseCheckHull checks one hull level whose unknown keys pass verbatim
// (response_format, its json_schema object, tool_choice, a tool): a baseRepeated
// key and a key that differs from one of known only in letter case are
// refused. vals[i] receives the raw value of known[i] (nil when absent).
// isObject is false when raw is no JSON object (the caller's type check
// decides). The level's path is at, or at[idx] for idx ≥ 0 — built only for
// a refusal.
func baseCheckHull(at string, idx int, raw []byte, known []string, vals [][]byte) (isObject bool, perr *paramError) {
	it, ok := baseNewMemberIter(raw)
	if !ok {
		return false, nil
	}
	var seen [baseHullPairwiseMax][]byte
	n := 0
	for {
		k, v, more := it.next()
		if !more {
			break
		}
		if n == baseHullPairwiseMax || baseNeedsDecode(k) {
			return true, baseCheckHullDecoded(baseLevelPath(at, idx), raw, known, vals)
		}
		for _, p := range seen[:n] {
			if bytes.Equal(p, k) {
				return true, baseRepeated(baseLevelPath(at, idx), string(k))
			}
		}
		seen[n] = k
		n++
		for i, kn := range known {
			switch {
			case string(k) == kn:
				vals[i] = v
			case baseAsciiEqualFold(k, kn):
				return true, unsupportedParameter(baseKeyPath(baseLevelPath(at, idx), string(k)))
			}
		}
	}
	if it.bad {
		return false, nil
	}
	return true, nil
}

// baseLevelPath is at, or at[idx] for an element of an array level.
func baseLevelPath(at string, idx int) string {
	if idx < 0 {
		return at
	}
	return fmt.Sprintf("%s[%d]", at, idx)
}

// baseCheckHullDecoded is baseCheckHull over the decoded map: keys with escapes or
// non-ASCII bytes (Unicode folding: "ſ" folds to "s"), or many keys.
func baseCheckHullDecoded(at string, raw []byte, known []string, vals [][]byte) *paramError {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if perr := baseUniqueMembers(at, raw, m); perr != nil {
		return perr
	}
	for k := range m {
		for i, kn := range known {
			switch {
			case k == kn:
				vals[i] = m[k]
			case strings.EqualFold(k, kn):
				return unsupportedParameter(baseKeyPath(at, k))
			}
		}
	}
	return nil
}

// baseNeedsDecode reports a key the byte comparison cannot judge: an escape or
// a non-ASCII byte.
func baseNeedsDecode(k []byte) bool {
	for _, c := range k {
		if c == '\\' || c >= 0x80 {
			return true
		}
	}
	return false
}

// baseAsciiEqualFold is strings.EqualFold for an ASCII key against a known key.
func baseAsciiEqualFold(k []byte, kn string) bool {
	if len(k) != len(kn) {
		return false
	}
	for i := range k {
		a, b := k[i], kn[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}
