package oai

// Gate 2 of FOLGE-P: the allocation-free parse against the base parse of
// 4839a958 (parse_oracle_test.go, kept verbatim) as a pure test oracle. Over
// a corpus — every body the route tests post (testdata/chat_parse_corpus.jsonl,
// recorded by chat_corpus_test.go), the forms of the heap probe in small,
// repeats, nesting depth 9 996 / 10 001, invalid UTF-8, escapes, whitespace —
// and under go test -fuzz both parses must agree on acceptance, error code,
// param and text, the ignored keys (X-Ctx-Ignored-Params), the CTX values,
// every PASS value byte for byte and the upstream message array. The only
// differences allowed are the ones FOLGE-P names (allowedDeviation), each
// counted.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// diffLimits are the policy limits the differential runs every body under:
// the defaults, vision with a small image cap, and tight caps.
var diffLimits = []policyLimits{
	{toolsMaxCount: 64, toolsMaxBytes: 256 << 10, responseFormatMaxBytes: 64 << 10, logitBiasMaxEntries: 64, stopMaxEntries: 4, visionMaxBytes: 4 << 20},
	{toolsMaxCount: 64, toolsMaxBytes: 256 << 10, responseFormatMaxBytes: 64 << 10, logitBiasMaxEntries: 64, stopMaxEntries: 4, vision: true, visionMaxBytes: 1024},
	{toolsMaxCount: 2, toolsMaxBytes: 300, responseFormatMaxBytes: 64, logitBiasMaxEntries: 20, stopMaxEntries: 1, vision: true, visionMaxBytes: 3},
}

// parseOutcome is what one parse leaves, in a comparable form.
type parseOutcome struct {
	stage                      string // "top", "policy" or "ok"
	code, param, message       string
	model, serviceTier         string
	modelOverlong              bool // the new parse left a raw name past modelRawMax undecoded
	stream                     bool
	n, maxTokens, messageCount int
	ignored                    []string
	pass                       map[string]string
	messages                   string
	unsized                    bool // a rewritten array whose length is not the size reserved for it
}

func errOutcome(stage string, perr *paramError) parseOutcome {
	return parseOutcome{stage: stage, code: perr.code, param: perr.param, message: perr.message}
}

// newParse runs the FOLGE-P parse.
func newParse(body []byte, l policyLimits) parseOutcome {
	req, perr := parseTopLevel(body)
	if perr != nil {
		return errOutcome("top", perr)
	}
	o := parseOutcome{model: req.model, modelOverlong: req.modelOverlong, serviceTier: req.serviceTier, stream: req.stream, n: req.n,
		maxTokens: req.maxTokens, pass: map[string]string{}}
	for k, v := range req.pass {
		o.pass[k] = string(v)
	}
	raw := req.messages
	var reserved int64
	if perr := req.parsePolicy(l, func(n int64) bool { reserved += n; return true }); perr != nil {
		o.stage, o.code, o.param, o.message = "policy", perr.code, perr.param, perr.message
		return o
	}
	o.stage, o.messageCount, o.ignored, o.messages = "ok", req.messageCount, req.ignored, string(req.messages)
	o.unsized = reserved != 0 && (int64(len(req.messages)) != reserved || cap(req.messages) != len(req.messages)) ||
		reserved == 0 && !bytes.Equal(raw, req.messages)
	return o
}

// baseParseRuns counts the base parses (TestOracleMapOrderIsEnumerated: A4
// enumerates the base's answers instead of sampling them).
var baseParseRuns atomic.Int64

// baseParse runs the base parse (the oracle).
func baseParse(body []byte, l policyLimits) parseOutcome {
	baseParseRuns.Add(1)
	req, perr := baseParseTopLevel(body)
	if perr != nil {
		return errOutcome("top", perr)
	}
	o := parseOutcome{model: req.model, serviceTier: req.serviceTier, stream: req.stream, n: req.n, maxTokens: req.maxTokens,
		pass: map[string]string{}}
	for k, v := range req.pass {
		o.pass[k] = string(v)
	}
	if perr := req.parsePolicy(l); perr != nil {
		o.stage, o.code, o.param, o.message = "policy", perr.code, perr.param, perr.message
		return o
	}
	o.stage, o.messageCount, o.ignored, o.messages = "ok", req.messageCount, req.ignored, string(req.messages)
	return o
}

// wireForm is a message array as llm.BuildUpstreamBody of the base sent it:
// compacted and HTML-escaped (json.Marshal of the raw value).
func wireForm(raw string) string {
	b, err := json.Marshal(json.RawMessage(raw))
	if err != nil {
		return "!" + err.Error()
	}
	return string(b)
}

// sameOutcome compares two outcomes; the message arrays by their wire form
// (FOLGE-P P-4: a rewritten message keeps the client's value bytes, the
// base had escaped them once more), and byte for byte when the base sent the
// array as it came.
func sameOutcome(body []byte, n, o parseOutcome) string {
	if n.stage != o.stage || n.code != o.code || n.param != o.param || n.message != o.message {
		return fmt.Sprintf("answer: new %s %q %q %q, base %s %q %q %q", n.stage, n.code, n.param, n.message, o.stage, o.code, o.param, o.message)
	}
	if n.stage == "top" {
		return ""
	}
	if !sameModel(n, o) || n.serviceTier != o.serviceTier || n.stream != o.stream || n.n != o.n || n.maxTokens != o.maxTokens {
		return fmt.Sprintf("CTX values: new %+v, base %+v", n, o)
	}
	if !maps.Equal(n.pass, o.pass) {
		return fmt.Sprintf("PASS values: new %v, base %v", n.pass, o.pass)
	}
	if n.stage != "ok" {
		return ""
	}
	if n.unsized {
		return "the rewritten message array is not the size parsePolicy reserved"
	}
	if n.messageCount != o.messageCount || !slices.Equal(n.ignored, o.ignored) {
		return fmt.Sprintf("count/ignored: new %d %v, base %d %v", n.messageCount, n.ignored, o.messageCount, o.ignored)
	}
	if wireForm(n.messages) != wireForm(o.messages) {
		return fmt.Sprintf("upstream messages: new %s, base %s", wireForm(n.messages), wireForm(o.messages))
	}
	if bytes.Contains(body, []byte(o.messages)) && n.messages != o.messages {
		return fmt.Sprintf("an unchanged message array is no longer sent as it came: new %s, base %s", n.messages, o.messages)
	}
	return ""
}

// The FOLGE-P deviations from the base answer (bau report FOLGE-P
// §Antwort-Änderungen), each held to its exact condition — read off the
// level the base named, its members counted on that level (Fremd-Review 1
// B1: a coarse count let an accepting parse through on any repeat):
//
//   - repeatBeyondWindow (A1): on a level that refuses unknown keys (the top
//     level, a message, a part, an image_url) — or on a hull for a case
//     variant of a hull key — the base named a repeated key the level does
//     not name whose first occurrence is past the level's first pairwiseKeys
//     such keys. FOLGE-P must still refuse (a 400).
//   - hullRepeatBeyondWindow (A2): the same on a hull (tool_choice, a tool,
//     response_format, its json_schema) for a vendor key: FOLGE-P accepts
//     — the hull goes upstream as sent — or refuses; accepts only while no
//     named key of the hull is repeated as well (Delta-Review 2 F-4: the
//     base named the vendor key because it sorts first, and an acceptance
//     would have let the named repeat through).
//   - logitBiasCapFirst, logitBiasLaterRepeat (A3): logit_bias past its cap,
//     its first repeat past the window: FOLGE-P answers the cap's
//     unsupported_value, or the invalid_json of a later repeat the window
//     holds (the base named the first repeat in body order; found by the
//     fuzzer of the lead once this rule was exact) — a key the body does
//     repeat (Delta-Review 2 F-5).
//   - oracleMapOrder (A4): the base ranged over a Go map (message fields, a
//     part's or image_url's unknown keys, a decoded hull's case variants) and
//     named one of several refused keys at random; FOLGE-P names one of them
//     deterministically. Allowed when FOLGE-P's answer is one the base could
//     give: the answers are enumerated from the base's map loops (each key
//     of the level it refuses alike, mapOrderAnswers), not sampled — 64 base
//     runs missed the rarer of two keys 2.2·10⁻⁴ of the time (Delta-Review 2
//     F-8).
//   - echoClipped (A8, hardening H-9): the base named a key longer than
//     echoNameMax bytes; FOLGE-P names it cut to its first echoNameMax bytes
//     at a rune boundary, marked "…" — the answer is the same otherwise.
//
// A repeated key the level names (model, role, image_url.url, a hull's
// type) is never a deviation: such a repeat is always found.
func allowedDeviation(body []byte, l policyLimits, n, o parseOutcome) string {
	for _, c := range clippedOutcomes(o) {
		if sameOutcome(body, n, c) == "" {
			return "echoClipped"
		}
	}
	if o.code == CodeInvalidJSON && strings.HasPrefix(o.message, "The request repeats the key") {
		if kind := repeatDeviation(body, l, n, o); kind != "" {
			return kind
		}
	}
	for _, b := range mapOrderAnswers(body, o) {
		for _, c := range append(clippedOutcomes(b), b) {
			if sameOutcome(body, n, c) == "" {
				return "oracleMapOrder"
			}
		}
	}
	return ""
}

// baseMessageFieldKinds are the PASS fields of a message the base
// type-checks ranging over a map (baseCheckMessageFields).
var baseMessageFieldKinds = map[string]baseJSONKind{"name": baseKindString, "tool_call_id": baseKindString, "tool_calls": baseKindArray, "refusal": baseKindString}

// mapOrderAnswers are the answers the base parse can give where it ranges
// over a Go map and names the first refused key it meets (A4), for its
// answer o: on the level o names, every key the base's loop refuses alike —
// a part's keys its type does not allow (baseCheckPart), an image_url's keys
// besides url and detail (baseCheckImageURL), a decoded hull's case variants
// of its known keys (baseCheckHullDecoded): unsupported_parameter; a
// message's PASS fields of the wrong JSON kind (baseCheckMessageFields):
// invalid_type. Each answer is o with its code, param and message replaced
// by the oracle's own constructor's. Only when o itself is an answer of that
// loop (fromMapLoop, Delta-Review 3 R3-2): where the base answered before
// its loop or in another check, it answers that one thing every time.
func mapOrderAnswers(body []byte, o parseOutcome) []parseOutcome {
	if o.stage != "policy" {
		return nil
	}
	var out []parseOutcome
	add := func(perr *paramError) {
		c := o
		c.code, c.param, c.message = perr.code, perr.param, perr.message
		out = append(out, c)
	}
	for _, lv := range diffLevels(body, o.param) {
		at, ok := strings.CutSuffix(o.param, "."+lv.key)
		if !ok || !lv.fromMapLoop(o) {
			continue
		}
		switch lv.kind {
		case "part":
			typ := lv.partType()
			for _, k := range lv.members {
				if partRefuses(typ, k) {
					add(unsupportedParameter(at + "." + k))
				}
			}
		case "image_url":
			for _, k := range lv.members {
				if k != "url" && k != "detail" {
					add(unsupportedParameter(at + "." + k))
				}
			}
		case "hull":
			for _, k := range lv.members {
				if lv.caseVariant(k) {
					add(unsupportedParameter(baseKeyPath(at, k)))
				}
			}
		case "message":
			for k, want := range baseMessageFieldKinds {
				if v := jsonMember(lv.obj, k); v != nil && !baseIsNull(v) && !baseHasKind(v, want) {
					add(invalidType(at+"."+k, baseKindName(want)))
				}
			}
		}
	}
	return out
}

// fromMapLoop reports o as an answer the base's map loop on the level gives
// (Delta-Review 3 R3-2): unsupported_parameter of a key a part's type
// refuses (baseCheckPart reaches its loop only for text and image_url), of
// an image_url key besides url and detail, of a hull's case variant while
// the base decodes the hull (baseHullDecodes; its byte walk names the first
// variant in body order); invalid_type of a message's PASS field. The key o
// names must be a member of the level: a param reads as several levels
// ("…content[0].image_url.zz" also as the part's key "image_url.zz"), and
// only the level that holds the key ran the loop that named it.
func (lv diffLevel) fromMapLoop(o parseOutcome) bool {
	if !slices.Contains(lv.members, lv.key) {
		return false
	}
	switch lv.kind {
	case "part":
		typ := lv.partType()
		return o.code == CodeUnsupportedParameter && (typ == "text" || typ == "image_url") && partRefuses(typ, lv.key)
	case "image_url":
		return o.code == CodeUnsupportedParameter && lv.key != "url" && lv.key != "detail"
	case "hull":
		return o.code == CodeUnsupportedParameter && lv.caseVariant(lv.key) && baseHullDecodes(lv.obj, lv.named)
	case "message":
		_, pass := baseMessageFieldKinds[lv.key]
		return o.code == CodeInvalidType && pass
	}
	return false
}

// partType is the type a part level declares ("" when it has none).
func (lv diffLevel) partType() string {
	var typ string
	_ = json.Unmarshal(jsonMember(lv.obj, "type"), &typ)
	return typ
}

// partRefuses reports a key the base's part loop refuses for a part of typ.
func partRefuses(typ, k string) bool {
	return k != "type" && (k != "text" || typ != "text") && (k != "image_url" || typ != "image_url")
}

// caseVariant reports k as a case variant of a named key of the level.
func (lv diffLevel) caseVariant(k string) bool {
	return slices.ContainsFunc(lv.named, func(kn string) bool { return k != kn && strings.EqualFold(k, kn) })
}

// baseHullDecodes reports the base checking the hull obj over its decoded
// map (baseCheckHull): its byte walk meets a key that needs decoding, or a
// key past baseHullPairwiseMax, before it answers a repeat or a case
// variant of a known key.
func baseHullDecodes(obj []byte, known []string) bool {
	it, ok := baseNewMemberIter(obj)
	if !ok {
		return false
	}
	var seen [][]byte
	for n := 0; ; n++ {
		k, _, more := it.next()
		switch {
		case !more:
			return false
		case n == baseHullPairwiseMax || baseNeedsDecode(k):
			return true
		case slices.ContainsFunc(seen, func(p []byte) bool { return bytes.Equal(p, k) }):
			return false
		case slices.ContainsFunc(known, func(kn string) bool { return string(k) != kn && baseAsciiEqualFold(k, kn) }):
			return false
		}
		seen = append(seen, k)
	}
}

// diffLevel is one object level a base param can name: its kind, the key
// the param names on it and the members of the level (decoded keys in body
// order), nil when the body has no such level.
type diffLevel struct {
	kind    string // "top", "message", "part", "image_url", "hull", "logit_bias"
	key     string
	named   []string
	members []string
	obj     []byte // the level's raw object
}

// The param prefixes of the levels a key check runs on (FOLGE-P: the
// level's path, then the key).
var diffLevelParams = []struct {
	re    *regexp.Regexp
	kind  string
	named []string
}{
	{regexp.MustCompile(`(?s)^messages\[(\d+)\]\.content\[(\d+)\]\.image_url\.(.+)$`), "image_url", []string{"url", "detail"}},
	{regexp.MustCompile(`(?s)^messages\[(\d+)\]\.content\[(\d+)\]\.(.+)$`), "part", []string{"type", "text", "image_url"}},
	{regexp.MustCompile(`(?s)^messages\[(\d+)\]\.(.+)$`), "message", messageNames},
	{regexp.MustCompile(`(?s)^tools\[(\d+)\]\.(.+)$`), "hull", toolHullKeys},
	{regexp.MustCompile(`(?s)^tool_choice\.(.+)$`), "hull", toolHullKeys},
	{regexp.MustCompile(`(?s)^response_format\.json_schema\.(.+)$`), "hull", responseFormatJSONKeys},
	{regexp.MustCompile(`(?s)^response_format\.(.+)$`), "hull", responseFormatKeys},
	{regexp.MustCompile(`(?s)^logit_bias\.(.+)$`), "logit_bias", nil},
}

// diffLevels are the levels param may name in body — every reading of it (a
// key may itself hold a dot or a bracket), the top level always among them.
// Navigated with encoding/json, independent of the parse under test; an
// outer repeat would have been the base's answer, so the first member of a
// name is the one meant.
func diffLevels(body []byte, param string) []diffLevel {
	top := diffLevel{kind: "top", key: param, named: topLevelNames, members: jsonKeys(body), obj: body}
	out := []diffLevel{top}
	for _, p := range diffLevelParams {
		m := p.re.FindStringSubmatch(param)
		if m == nil {
			// An empty key: repeated names the level itself ("messages[0]"),
			// unsupportedParameter the level and a dot ("messages[0].").
			m = p.re.FindStringSubmatch(strings.TrimSuffix(param, ".") + ".\x00")
			if m == nil {
				continue
			}
			m[len(m)-1] = ""
		}
		var obj []byte
		switch p.kind {
		case "image_url":
			obj = jsonMember(jsonIndex(jsonMember(jsonIndex(jsonMember(body, "messages"), m[1]), "content"), m[2]), "image_url")
		case "part":
			obj = jsonIndex(jsonMember(jsonIndex(jsonMember(body, "messages"), m[1]), "content"), m[2])
		case "message":
			obj = jsonIndex(jsonMember(body, "messages"), m[1])
		default:
			pp := strings.TrimSuffix(param, ".") + "." // an empty key's param names the hull itself
			switch {
			case strings.HasPrefix(pp, "tools["):
				obj = jsonIndex(jsonMember(body, "tools"), m[1])
			case strings.HasPrefix(pp, "tool_choice."):
				obj = jsonMember(body, "tool_choice")
			case strings.HasPrefix(pp, "response_format.json_schema."):
				obj = jsonMember(jsonMember(body, "response_format"), "json_schema")
			case strings.HasPrefix(pp, "response_format."):
				obj = jsonMember(body, "response_format")
			case strings.HasPrefix(pp, "logit_bias."):
				obj = jsonMember(body, "logit_bias")
			}
		}
		if members := jsonKeys(obj); members != nil {
			out = append(out, diffLevel{kind: p.kind, key: m[len(m)-1], named: p.named, members: members, obj: obj})
		}
	}
	return out
}

// beyondWindow reports the level's key as one the window cannot see: no
// named key of the level, repeated, its first occurrence past the first
// pairwiseKeys keys the level does not name.
func (lv diffLevel) beyondWindow() bool {
	if slices.Contains(lv.named, lv.key) {
		return false
	}
	unknown, first, count := 0, -1, 0
	for _, k := range lv.members {
		if slices.Contains(lv.named, k) {
			continue
		}
		if k == lv.key {
			count++
			if first < 0 {
				first = unknown
			}
		}
		unknown++
	}
	return count >= 2 && first >= pairwiseKeys
}

// namedRepeated reports a named key of the level that occurs twice or more.
func (lv diffLevel) namedRepeated() bool {
	for _, kn := range lv.named {
		if occurrences(lv.members, kn) >= 2 {
			return true
		}
	}
	return false
}

// occurrences is how often key occurs in members.
func occurrences(members []string, key string) int {
	n := 0
	for _, k := range members {
		if k == key {
			n++
		}
	}
	return n
}

// repeatedName reports the key a logit_bias repeat answer n names as one
// the body repeats on the level (its param is "logit_bias.<key>", an empty
// key's "logit_bias"; a key over echoNameMax bytes is named cut, A8).
func (lv diffLevel) repeatedName(n parseOutcome) bool {
	key, ok := strings.CutPrefix(n.param, "logit_bias.")
	if !ok && n.param != "logit_bias" {
		return false
	}
	o := parseOutcome{param: n.param}
	for _, k := range lv.members {
		o.param = "logit_bias." + k
		if k == key || n.param == "logit_bias" && k == "" || slices.ContainsFunc(clippedOutcomes(o), func(c parseOutcome) bool { return c.param == n.param }) {
			return occurrences(lv.members, k) >= 2
		}
	}
	return false
}

// variant reports the level's key as a case variant of a named key.
func (lv diffLevel) variant() bool {
	return slices.ContainsFunc(lv.named, func(k string) bool { return strings.EqualFold(k, lv.key) })
}

// repeatDeviation is A1, A2 or A3 for a base answer that named a repeat.
func repeatDeviation(body []byte, l policyLimits, n, o parseOutcome) string {
	refused := n.stage != "ok" && statusOf(n.code) == http.StatusBadRequest
	for _, lv := range diffLevels(body, o.param) {
		if !lv.beyondWindow() {
			continue
		}
		switch {
		case lv.kind == "logit_bias":
			switch past := len(lv.members) > l.logitBiasMaxEntries && n.stage == "policy"; {
			case past && n.code == CodeUnsupportedValue && n.param == "logit_bias":
				return "logitBiasCapFirst"
			case past && n.code == CodeInvalidJSON && strings.HasPrefix(n.message, "The request repeats the key") && lv.repeatedName(n):
				return "logitBiasLaterRepeat"
			}
		case lv.kind == "hull" && !lv.variant():
			if refused || n.stage == "ok" && passAsSent(body, n) && !lv.namedRepeated() {
				return "hullRepeatBeyondWindow"
			}
		case refused:
			return "repeatBeyondWindow"
		}
	}
	return ""
}

// passAsSent reports every PASS value of n as the body's member value.
func passAsSent(body []byte, n parseOutcome) bool {
	for k, v := range n.pass {
		if m := jsonMember(body, k); !bytes.Equal(bytes.TrimSpace(m), bytes.TrimSpace([]byte(v))) {
			return false
		}
	}
	return true
}

// statusOf is the HTTP status of an error code (the route's vocabulary).
func statusOf(code string) int {
	if i := slices.IndexFunc(codes, func(s CodeSpec) bool { return s.Code == code }); i >= 0 {
		return codes[i].Status
	}
	return 0
}

// clippedOutcomes are o with the key its param names cut as FOLGE-P cuts an
// echoed name (A8) — one per reading of the param whose key is longer than
// echoNameMax; none when no key is.
func clippedOutcomes(o parseOutcome) []parseOutcome {
	if o.param == "" {
		return nil
	}
	var out []parseOutcome
	readings := []string{""}
	for _, p := range diffLevelParams {
		if m := p.re.FindStringSubmatchIndex(o.param); m != nil {
			readings = append(readings, o.param[:m[len(m)-2]])
		}
	}
	for _, prefix := range readings {
		key := o.param[len(prefix):]
		if len(key) <= echoNameMax {
			continue
		}
		cut := echoNameMax
		for cut > 0 && !utf8.RuneStart(key[cut]) {
			cut--
		}
		c := o
		c.param = prefix + key[:cut] + "…"
		c.message = strings.ReplaceAll(o.message, o.param, c.param)
		out = append(out, c)
	}
	return out
}

// jsonMember is the raw value of the first member name of the JSON object
// raw, nil when there is none (encoding/json: the test's own reading).
func jsonMember(raw []byte, name string) []byte {
	var m []byte
	jsonWalk(raw, '{', func(k string, v json.RawMessage) bool {
		if k == name {
			m = v
			return false
		}
		return true
	})
	return m
}

// jsonIndex is the raw element i (decimal) of the JSON array raw, nil when
// there is none.
func jsonIndex(raw []byte, i string) []byte {
	want, err := strconv.Atoi(i)
	if err != nil {
		return nil
	}
	var e []byte
	n := 0
	jsonWalk(raw, '[', func(_ string, v json.RawMessage) bool {
		if n == want {
			e = v
			return false
		}
		n++
		return true
	})
	return e
}

// jsonKeys are the decoded member names of the JSON object raw in body
// order, nil when raw is no object.
func jsonKeys(raw []byte) []string {
	keys := []string{}
	if !jsonWalk(raw, '{', func(k string, _ json.RawMessage) bool { keys = append(keys, k); return true }) {
		return nil
	}
	return keys
}

// jsonWalk calls f for every member (open '{') or element (open '[') of
// raw until f returns false; false when raw is no such container.
func jsonWalk(raw []byte, open json.Delim, f func(k string, v json.RawMessage) bool) bool {
	if raw == nil {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != open {
		return false
	}
	for dec.More() {
		var k string
		if open == '{' {
			t, err := dec.Token()
			if err != nil {
				return false
			}
			k, _ = t.(string)
		}
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return false
		}
		if !f(k, v) {
			return true
		}
	}
	return true
}

// sameModel compares the name Resolve looks up (hardening 5, B-1): the same
// name, or — where the new parse left a raw name past modelRawMax undecoded
// and Resolve looks up the empty name — a base name that is no alias name
// either: longer than aliasNameMaxBytes or not ASCII. Both answer the
// unknown model's 404 (TestOverlongModelAnswersLikeAnUnknownModel).
func sameModel(n, o parseOutcome) bool {
	if !n.modelOverlong {
		return n.model == o.model
	}
	ascii := true
	for i := 0; i < len(o.model); i++ {
		ascii = ascii && o.model[i] < utf8.RuneSelf
	}
	return n.model == "" && (len(o.model) > aliasNameMaxBytes || !ascii)
}

// diffCheck compares both parses of body under every diffLimits entry.
func diffCheck(t testing.TB, body []byte, deviations map[string]int) {
	t.Helper()
	for li, l := range diffLimits {
		n, o := newParse(body, l), baseParse(body, l)
		d := sameOutcome(body, n, o)
		if d == "" {
			deviations["equal"]++
			if n.modelOverlong {
				deviations["equal, model past the bound"]++ // counted inside equal, named for the report
			}
			continue
		}
		if kind := allowedDeviation(body, l, n, o); kind != "" {
			deviations[kind]++
			continue
		}
		t.Errorf("limits %d, body %.300q: %s", li, body, d)
	}
}

// diffCorpus is the corpus of the differential.
func diffCorpus(t testing.TB) [][]byte {
	t.Helper()
	f, err := os.Open("testdata/chat_parse_corpus.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var s string
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("corpus line %q: %v", sc.Bytes(), err)
		}
		out = append(out, []byte(s))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return append(out, generatedCorpus()...)
}

// generatedCorpus are the probe forms in small, repeats, depth, invalid
// UTF-8, escapes and whitespace variants.
func generatedCorpus() [][]byte {
	msg := `{"model":"chat","messages":[{"role":"user","content":"x"}]`
	rep := func(s string, n int) string { return strings.Repeat(s, n) }
	join := func(f func(i int) string, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = f(i)
		}
		return strings.Join(parts, ",")
	}
	keys := func(n int) string { return join(func(i int) string { return fmt.Sprintf(`"k%03d":0`, i) }, n) }
	img := `data:image/png;base64,AAAA`
	nest := func(d int) string { return rep("[", d) + rep("]", d) }
	var c []string
	for _, n := range []int{1, 15, 16, 17, 40} {
		c = append(c,
			`{"model":"chat","messages":[`+join(func(int) string { return `{"role":"user","content":"x"}` }, n)+`]}`,
			`{"model":"chat","messages":[`+join(func(int) string { return `{"role":"developer","content":"x"}` }, n)+`]}`,
			`{"model":"chat","messages":[{"role":"user","content":[`+join(func(int) string { return `{"type":"text","text":"x"}` }, n)+`]}]}`,
			msg+`,"stop":[`+join(func(int) string { return `"x"` }, n)+`]}`,
			msg+`,"logit_bias":{`+join(func(i int) string { return fmt.Sprintf(`"%d":0`, i) }, n)+`}}`,
			msg+`,"logit_bias":{`+join(func(i int) string { return fmt.Sprintf(`"%d":0`, i%7) }, n)+`}}`,
			msg+`,`+keys(n)+`}`,
			msg+`,`+keys(n)+`,"k000":1}`,
			msg+`,"modalities":[`+join(func(int) string { return `"text"` }, n)+`]}`,
			msg+`,"tools":[`+join(func(int) string { return `{"type":"function","function":{"name":"f"}}` }, n)+`]}`,
			msg+`,"tool_choice":{"type":"function","function":{"name":"f"},`+keys(n)+`}}`,
			msg+`,"tool_choice":{"type":"function","function":{"name":"f"},`+keys(n)+`,"Type":1}}`,
			msg+`,"tool_choice":{"type":"function",`+keys(n)+`,"function":{"name":"f"},"function":1}}`,
			`{"model":"vis","messages":[{"role":"user","content":[`+join(func(int) string { return `{"type":"image_url","image_url":{"url":"data:image\/png;base64,AAAA"}}` }, n)+`]}]}`,
		)
	}
	c = append(c,
		msg+`,"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":`+nest(9996)+`}}}`,
		msg+`,"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":`+nest(10001)+`}}}`,
		msg+`,"tools":[{"type":"function","function":{"name":"f","parameters":`+nest(9990)+`}}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[`+nest(9990)+`]}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"<a&b>  `+"  "+`"}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"`+"\xff\xfe"+`"}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"x","`+"\xff"+`":1,"`+"\xfe"+`":2}]}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}],"`+"\xff"+`":1,"`+"\xfe"+`":2}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}],"\ud800":1,"\udbff":2}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}],"😀":1,"😀":2}`,
		`{"model":"chat","messages":[{"role":"developer","content":"x","reasoning":"r"}]}`,
		`{"model":"chat","model":"chat","messages":[]}`,
		"{\n \"model\" : \"chat\" ,\r\n\t\"messages\" : [ { \"role\" : \"developer\" , \"content\" : [ { \"type\" : \"text\" , \"text\" : \"x\" } ] , \"name\" : null } ] , \"temperature\" : 0.5 , \"stop\" : [ \"a\" , null ] }",
		`{"model":"chat","messages":[{"role":"assistant","content":null,"name":1,"refusal":2,"tool_call_id":3,"tool_calls":4}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+img+`","b":1,"a":2}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+img+`"},"text":"x","a":1}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA="}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=A"}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAB="}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,===="}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/gif;base64,`+rep("A", 4096)+`"}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":null,"detail":null}}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":null},{"type":1}]}]}`,
		`{"model":"chat","messages":[{"role":null,"content":"x"}]}`,
		msg+`,"n":1`+rep("0", 400)+`}`,
		msg+`,"max_tokens":-0,"max_completion_tokens":0}`,
		msg+`,"top_logprobs":-0}`,
		msg+`,"top_logprobs":100}`,
		msg+`,"modalities":["text"]}`,
		msg+`,"modalities":[null]}`,
		msg+`,"modalities":null}`,
		msg+`,"stop":[1]}`,
		msg+`,"stop":"x"}`,
		`[]`, `null`, `"x"`, ` {} `, `{"model":"chat","messages":[{"role":"user","content":"x"}]} x`,
		// Unknown keys whose body order is not their sort order, on every
		// refusing level: the smallest is named.
		msg+`,"zz":1,"\u0061a":1,"mm":1}`,
		`{"model":"chat","messages":[{"role":"user","content":"x","zz":1,"aa":1}]}`,
		`{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x","zz":1,"aa":1}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+img+`","zz":1,"aa":1}}]}]}`,
		`{"zz":1,"model":"chat","aa":1,"messages":[]}`,
		// The named deviations (allowedDeviation): a repeat beyond the
		// window on a refusing level, on a hull and in logit_bias past its cap.
		msg+`,`+keys(40)+`,"k030":1}`,
		`{"model":"chat","messages":[{"role":"user","content":"x",`+keys(40)+`,"k030":1}]}`,
		msg+`,"tool_choice":{"type":"function","function":{"name":"f"},`+keys(40)+`,"k030":1}}`,
		msg+`,"logit_bias":{`+join(func(i int) string { return fmt.Sprintf(`"%d":0`, i) }, 100)+`,"50":1}}`,
	)
	// Escaped named keys and emoji keys sent as surrogate pairs on every
	// level (Fremd-Review 1 B5: MR1, MR15), key names past echoNameMax (H-9).
	e := func(k string) string { return k[:1] + escU(rune(k[1])) + k[2:] }
	pair := escU(0xd83d) + escU(0xde00)
	longName := strings.Repeat("n", echoNameMax-1) + "é" + "tail"
	c = append(c,
		`{"`+e("model")+`":"chat","`+e("messages")+`":[{"`+e("role")+`":"user","`+e("content")+`":"x"}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"`+e("type")+`":"text","`+e("text")+`":"x"},{"type":"image_url","`+e("image_url")+`":{"`+e("url")+`":"data:image/png;base64,AAAA","`+e("detail")+`":"low"}}]}]}`,
		msg+`,"tool_choice":{"`+e("type")+`":"function","`+e("function")+`":{"name":"f"}},"response_format":{"`+e("type")+`":"json_schema","`+e("json_schema")+`":{"`+e("name")+`":"s"}}}`,
		msg+`,"😀":1,"`+pair+`":2}`,
		msg+`,"`+pair+`":1,"`+escU(0xffff)+`":2}`,
		`{"model":"chat","messages":[{"role":"user","content":"x","😀":1,"`+pair+`":2}]}`,
		`{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x","`+pair+`":1,"😀":2}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","😀":1,"`+pair+`":2}}]}]}`,
		msg+`,"tool_choice":{"type":"function","function":{"name":"f"},"😀":1,"`+pair+`":2}}`,
		msg+`,"logit_bias":{"`+pair+`":1,"😀":2}}`,
		msg+`,"`+longName+`":1}`,
		msg+`,"`+longName+`":1,"`+longName+`":2}`,
		`{"model":"chat","messages":[{"role":"user","content":"x","`+strings.Repeat(escU('<'), 100)+`":1}]}`,
		msg+`,"`+strings.Repeat("q", echoNameMax)+`":1}`,
	)
	// Hardening 5, B-1: model names at and past modelRawMax — escaped,
	// invalid UTF-8, plain, a surrogate pair — with a key that sorts before
	// and after model, and a non-string past the bound.
	esc := strings.Repeat(`\u0061`, aliasNameMaxBytes)
	tail := `","messages":[{"role":"user","content":"x"}]`
	for _, raw := range []string{esc, esc + "a", esc + `\u0061`, rep("\xff", modelRawMax), rep("\xff", modelRawMax+1),
		rep("a", modelRawMax+1), rep(pair, modelRawMax/12+1), rep("a", aliasNameMaxBytes+1)} {
		c = append(c, `{"model":"`+raw+tail+`}`, `{"model":"`+raw+tail+`,"aa":1}`, `{"model":"`+raw+tail+`,"zz":1}`,
			`{"model":"`+raw+tail+`,"stream":true}`)
	}
	c = append(c, `{"model":["`+esc+`a"],"messages":[]}`)
	// Delta-Review 6 L-3: the fuzzer's find (509 s, 2 657 204 executions) —
	// a key past echoNameMax that holds a newline (escaped, \u000A). The new
	// answer clips it in param and message as wanted (echoClipped); the
	// oracle has to read the level's key across the newline, so every
	// regex of diffLevelParams matches with (?s). The find at the message
	// level, the same key in a part and an image_url, and repeated (the
	// answer names a repeated key) at each other level a regex names.
	nlKey := `0\u000A` + rep("0", 72)
	c = append(c, `{"model":"0","messages":[{"`+nlKey+`":0}]}`,
		`{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x","`+nlKey+`":0}]}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","`+nlKey+`":0}}]}]}`,
		msg+`,"tools":[{"type":"function","function":{"name":"f"},"`+nlKey+`":0,"`+nlKey+`":0}]}`,
		msg+`,"tool_choice":{"type":"function","function":{"name":"f"},"`+nlKey+`":0,"`+nlKey+`":0}}`,
		msg+`,"response_format":{"type":"json_schema","json_schema":{"name":"s","`+nlKey+`":0,"`+nlKey+`":0}}}`,
		msg+`,"response_format":{"type":"json_schema","json_schema":{"name":"s"},"`+nlKey+`":0,"`+nlKey+`":0}}`,
		msg+`,"logit_bias":{"`+nlKey+`":1,"`+nlKey+`":1}}`)
	out := make([][]byte, len(c))
	for i, s := range c {
		out[i] = []byte(s)
	}
	return out
}

// TestChatParseDifferential is gate 2 over the corpus.
func TestChatParseDifferential(t *testing.T) {
	corpus := diffCorpus(t)
	dev := map[string]int{}
	for _, body := range corpus {
		diffCheck(t, body, dev)
	}
	t.Logf("FOLGEP differential: %d bodies × %d limit sets: %v", len(corpus), len(diffLimits), dev)
	if dev["equal"] == 0 {
		t.Fatal("no body compared equal: the corpus did not load")
	}
}

// FuzzChatParseDifferential is gate 2 under the fuzzer (seeded with the
// corpus): go test -run '^$' -fuzz FuzzChatParseDifferential -fuzztime 120s.
func FuzzChatParseDifferential(f *testing.F) {
	for _, body := range diffCorpus(f) {
		if len(body) <= 4<<10 {
			f.Add(body)
		}
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		diffCheck(t, body, map[string]int{})
	})
}
