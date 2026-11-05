package oai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
)

// The request parse of POST /v1/chat/completions (design/02 §4.5, §4.5.1,
// L1/L2): the body is read as a map of raw values, every top-level key, every
// message key and every content-part key falls into exactly one class —
// PASS (verbatim upstream after a type and size check), CTX (read and set by
// ctx), DROP (removed, named in X-Ctx-Ignored-Params) or REJECT (400) — and
// an allowed value reaches the wire byte for byte (llm.BuildUpstreamBody).
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
	messageRoles    = map[string]bool{"system": true, "user": true, "assistant": true, "tool": true, "developer": true}
)

// serviceTiers are the service_tier values of the OpenAI spec; the planner
// reads them (flex lowers the lane to bulk, design/03 §4.4).
var serviceTiers = map[string]bool{"auto": true, "default": true, "flex": true, "scale": true, "priority": true}

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

// parseTopLevel is the alias-independent half of the parse: JSON, the class
// of every top-level key (unknown ⇒ 400 before the model is resolved, so the
// answer says nothing about any alias), the model name and the CTX values.
// The message and policy checks follow once the alias is known (parsePolicy).
func parseTopLevel(body []byte) (*chatRequest, *paramError) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, &paramError{CodeInvalidJSON, "", "The request body is not a valid JSON object."}
	}
	if perr := uniqueMembers("", body, raw); perr != nil {
		return nil, perr // H-2: the checked value is the sent value
	}
	req := &chatRequest{raw: raw, pass: make(map[string]json.RawMessage, len(raw))}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the first refused key is deterministic
	for _, k := range keys {
		v := raw[k]
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
	if req.model == "" {
		return nil, missingParameter("model")
	}
	return req, nil
}

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

// readCTX reads one CTX key. n > 1 is refused here (E4: RC-F) — before any
// alias is resolved. stream and stream_options.include_usage are read (W02-08);
// upstream, a stream always asks for usage (llm.BuildUpstreamBody).
func (req *chatRequest) readCTX(k string, v json.RawMessage) *paramError {
	if isNull(v) {
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
		if !hasKind(v, kindObject) {
			return invalidType(k, "an object")
		}
		var o struct {
			IncludeUsage *json.RawMessage `json:"include_usage"`
		}
		if err := json.Unmarshal(v, &o); err != nil {
			return invalidType(k, "an object")
		}
		if o.IncludeUsage != nil && !isNull(*o.IncludeUsage) {
			if err := json.Unmarshal(*o.IncludeUsage, &req.includeUsage); err != nil {
				return invalidType(k+".include_usage", "a boolean")
			}
		}
	case "reasoning":
		if !hasKind(v, kindObject) {
			return invalidType(k, "an object")
		}
	case "reasoning_effort":
		if !hasKind(v, kindString) {
			return invalidType(k, "a string")
		}
	case "service_tier":
		if err := json.Unmarshal(v, &req.serviceTier); err != nil {
			return invalidType(k, "a string")
		}
		if !serviceTiers[req.serviceTier] {
			return unsupportedValue(k, "unknown service tier")
		}
	case "n":
		if err := json.Unmarshal(v, &req.n); err != nil || !hasKind(v, kindInteger) {
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
func (req *chatRequest) readMaxTokens(k string, v json.RawMessage) *paramError {
	var n int
	if err := json.Unmarshal(v, &n); err != nil || !hasKind(v, kindInteger) {
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

func textOnlyModalities(v json.RawMessage) bool {
	var m []string
	return json.Unmarshal(v, &m) == nil && len(m) == 1 && m[0] == "text"
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

// parsePolicy is the alias-dependent half: the PASS caps of the alias policy
// and the messages (§4.5.1), developer rewritten to system, the DROP keys of
// every message removed, every content part checked — the image parts
// before any upstream contact (B18).
func (req *chatRequest) parsePolicy(l policyLimits) *paramError {
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
	req.messages = msgs.raw
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
		var m map[string]json.RawMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return invalidType("logit_bias", "an object")
		}
		// A repeated token id would count once and reach the wire as
		// often as sent: the entry cap counts the map (H-2).
		if perr := uniqueMembers("logit_bias", v, m); perr != nil {
			return perr
		}
		if len(m) > l.logitBiasMaxEntries {
			return unsupportedValue("logit_bias", fmt.Sprintf("at most %d entries are allowed", l.logitBiasMaxEntries))
		}
	}
	if v, ok := pass["stop"]; ok && hasKind(v, kindArray) {
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

// checkTools counts the tools against the cap and checks every tool hull.
func checkTools(v json.RawMessage, maxCount int) *paramError {
	var arr []json.RawMessage
	if err := json.Unmarshal(v, &arr); err != nil {
		return nil // the PASS type check let an array through; nothing to count
	}
	if len(arr) > maxCount {
		return unsupportedValue("tools", fmt.Sprintf("at most %d tools are allowed for this model", maxCount))
	}
	var vals [2][]byte
	for i, t := range arr {
		if _, perr := checkHull("tools", i, t, toolHullKeys, vals[:]); perr != nil {
			return perr
		}
	}
	return nil
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

// parsedMessages is the message array as it goes upstream.
type parsedMessages struct {
	raw   json.RawMessage
	count int
}

// parseMessages checks every message (§4.5.1) and returns the array as it
// goes upstream: verbatim when nothing changed (no developer role, no DROP
// key), else re-encoded from the per-message raw values — a value is never
// decoded into Go types, so a client number or string reaches the wire as
// sent.
func parseMessages(raw json.RawMessage, l policyLimits) (parsedMessages, []string, *paramError) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return parsedMessages{}, nil, invalidType("messages", "an array of message objects")
	}
	if len(arr) == 0 {
		return parsedMessages{}, nil, unsupportedValue("messages", "at least one message is required")
	}
	var dropped []string
	changed := false
	for i, m := range arr {
		out, drop, rewrote, perr := parseMessage(i, m, l)
		if perr != nil {
			return parsedMessages{}, nil, perr
		}
		dropped = append(dropped, drop...)
		if rewrote {
			arr[i] = out
			changed = true
		}
	}
	if !changed {
		return parsedMessages{raw: raw, count: len(arr)}, dropped, nil
	}
	enc, err := json.Marshal(arr)
	if err != nil {
		return parsedMessages{}, nil, &paramError{CodeInvalidJSON, "messages", "The messages could not be encoded."}
	}
	return parsedMessages{raw: enc, count: len(arr)}, dropped, nil
}

// parseMessage checks message i and returns its upstream form when it had
// to change (developer ⇒ system, DROP keys removed).
func parseMessage(i int, raw json.RawMessage, l policyLimits) (json.RawMessage, []string, bool, *paramError) {
	at := fmt.Sprintf("messages[%d]", i)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, nil, false, invalidType(at, "a message object")
	}
	if perr := uniqueMembers(at, raw, m); perr != nil {
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
		case messagePassKeys[k]:
		case messageDropKeys[k]:
			dropped = append(dropped, "messages[]."+k)
		default:
			return nil, nil, false, unsupportedParameter(at + "." + k)
		}
	}
	role, perr := messageRole(at, m["role"])
	if perr != nil {
		return nil, nil, false, perr
	}
	if perr := checkMessageFields(at, role, m); perr != nil {
		return nil, nil, false, perr
	}
	if perr := checkContent(at, role, m["content"], l); perr != nil {
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

func messageRole(at string, v json.RawMessage) (string, *paramError) {
	if v == nil {
		return "", missingParameter(at + ".role")
	}
	var role string
	if err := json.Unmarshal(v, &role); err != nil {
		return "", invalidType(at+".role", "a string")
	}
	if !messageRoles[role] {
		return "", unsupportedValue(at+".role", "must be system, developer, user, assistant or tool")
	}
	return role, nil
}

// checkMessageFields type-checks the PASS keys besides role and content.
func checkMessageFields(at, role string, m map[string]json.RawMessage) *paramError {
	for k, want := range map[string]jsonKind{"name": kindString, "tool_call_id": kindString, "tool_calls": kindArray, "refusal": kindString} {
		if v, ok := m[k]; ok && !isNull(v) && !hasKind(v, want) {
			return invalidType(at+"."+k, kindName(want))
		}
	}
	if _, ok := m["tool_calls"]; ok && role != "assistant" {
		return unsupportedParameter(at + ".tool_calls")
	}
	return nil
}

// checkContent checks content over every role (§4.5.1): a string, an array
// of parts, or — for an assistant message carrying tool calls — null.
func checkContent(at, role string, v json.RawMessage, l policyLimits) *paramError {
	switch {
	case v == nil || isNull(v):
		if role == "assistant" {
			return nil
		}
		return missingParameter(at + ".content")
	case hasKind(v, kindString):
		return nil
	case hasKind(v, kindArray):
	default:
		return invalidType(at+".content", "a string or an array of content parts")
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(v, &parts); err != nil {
		return invalidType(at+".content", "an array of content parts")
	}
	for j, p := range parts {
		if perr := checkPart(fmt.Sprintf("%s.content[%d]", at, j), p, l); perr != nil {
			return perr
		}
	}
	return nil
}

// checkPart checks one content part: text, or image_url under the vision
// rule. Every other part type (input_audio, file, video_url, …) is refused.
func checkPart(at string, raw json.RawMessage, l policyLimits) *paramError {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(raw, &p); err != nil || p == nil {
		return invalidType(at, "a content part object")
	}
	if perr := uniqueMembers(at, raw, p); perr != nil {
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
		if !hasKind(p["text"], kindString) {
			return invalidType(at+".text", "a string")
		}
	case "image_url":
		if !l.vision {
			return unsupportedValue(at+".image_url.url", "this model does not accept images")
		}
		allowed["image_url"] = true
		if perr := checkImageURL(at+".image_url", p["image_url"], l.visionMaxBytes); perr != nil {
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

// checkImageURL is the vision rule (§4.5.1, B18): url exactly one of the
// four data-URI prefixes, the payload valid base64 and its decoded size ≤
// the policy's vision_max_bytes; detail PASS.
func checkImageURL(at string, raw json.RawMessage, maxBytes int) *paramError {
	var img map[string]json.RawMessage
	if err := json.Unmarshal(raw, &img); err != nil || img == nil {
		return invalidType(at, "an object")
	}
	if perr := uniqueMembers(at, raw, img); perr != nil {
		return perr // two urls: the checked one and the one an engine reads may differ (B18)
	}
	for k := range img {
		if k != "url" && k != "detail" {
			return unsupportedParameter(at + "." + k)
		}
	}
	if d, ok := img["detail"]; ok && !hasKind(d, kindString) {
		return invalidType(at+".detail", "a string")
	}
	var url string
	if err := json.Unmarshal(img["url"], &url); err != nil {
		return invalidType(at+".url", "a string")
	}
	i := slices.IndexFunc(visionPrefixes, func(p string) bool { return strings.HasPrefix(url, p) })
	if i < 0 {
		return unsupportedValue(at+".url", "only base64 data URIs of png, jpeg, webp or gif images are accepted")
	}
	payload := url[len(visionPrefixes[i]):]
	n, ok := base64Size(payload)
	switch {
	case !ok:
		return unsupportedValue(at+".url", "the image data is not valid base64")
	case n > maxBytes:
		return unsupportedValue(at+".url", "the image exceeds the size limit of this model")
	}
	return nil
}

// base64Size validates s as standard padded base64 without allocating its
// decoded form and returns the decoded length.
func base64Size(s string) (int, bool) {
	if len(s) == 0 || len(s)%4 != 0 {
		return 0, false
	}
	pad := 0
	for i := len(s) - 1; i >= len(s)-2 && s[i] == '='; i-- {
		pad++
	}
	for i := 0; i < len(s)-pad; i++ {
		if !isBase64Char(s[i]) {
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
