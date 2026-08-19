package dream

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Format tokens for parseLinks, persisted in context_llm_log.metadata.parse_format
// to track LLM drift patterns over time. Empty string means "no parseable shape"
// (parse error or empty/sentinel input — distinguishable via the returned error).
const (
	formatArray           = "array"
	formatObject          = "object"
	formatFencedArray     = "fenced-array"
	formatFencedObject    = "fenced-object"
	formatWrapped         = "wrapped"
	formatFencedWrapped   = "fenced-wrapped"
	formatStringMap       = "string-map"
	formatFencedStringMap = "fenced-string-map"
)

// parseLinks parses the LLM JSON response into Link structs.
// Tolerates the drift patterns observed in production:
//   - Named-wrapper form (cloud relays, PR #11 review): {"<key>": [ <links> ], ...}
//   - Single flat object form (Welle 49): {"target_id": "...", "type": ..., ...}
//   - Object-map form (audit S25, 2026-05-03): {"<uuid>": {"type": "...", "confidence": <float|string>}, ...}
//   - String-map form (deepseek-v4-flash, PR #12): {"<uuid>": "<type>", ...} — no confidence field
//   - String-encoded confidence labels: "high"|"medium"|"low" → 0.9/0.6/0.3
//
// Strips a leading ```json fence if present (43/1481 historical cases). The fence
// presence is detected BEFORE stripping so "fenced-X" / "X" can be distinguished
// in the returned format token.
//
// Returns (links, format, err). format is one of formatArray | formatObject |
// formatWrapped | formatStringMap or their fenced-* variants;
// empty for empty/sentinel inputs and for parse errors. Map→slice conversion
// sorts by TargetID for deterministic downstream behavior.
func parseLinks(raw string) ([]Link, string, error) {
	trimmed := strings.TrimSpace(raw)
	wasFenced := strings.HasPrefix(trimmed, "```")
	body := stripCodeFence(trimmed)
	if body == "" || body == "[]" || body == "{}" {
		return nil, "", nil
	}

	var rawArr []struct {
		TargetID   string          `json:"target_id"`
		Type       string          `json:"type"`
		Confidence json.RawMessage `json:"confidence"`
	}
	arrErr := json.Unmarshal([]byte(body), &rawArr)
	if arrErr == nil {
		out := make([]Link, 0, len(rawArr))
		for _, r := range rawArr {
			conf, floored, ok := confidenceOrFloor(r.Confidence, r.Type)
			if !ok {
				continue
			}
			out = append(out, Link{TargetID: r.TargetID, Relationship: r.Type, Confidence: conf, Floored: floored})
		}
		// Zero-link contract: a non-empty structure that yields no usable link
		// is degenerate output, not a "nothing to link" verdict — surface it as
		// a parse error (transient retry) instead of a success that books the
		// multi-day inert cooldown. The empty-array sentinel returned early.
		if len(rawArr) > 0 && len(out) == 0 {
			return nil, "", fmt.Errorf("parse links: %d array entries, none usable", len(rawArr))
		}
		if wasFenced {
			return out, formatFencedArray, nil
		}
		return out, formatArray, nil
	}

	if strings.HasPrefix(body, "{") {
		// Drift form 0 (cloud relays, PR #11 review) — see parseWrappedLinks.
		if links, ok := parseWrappedLinks(body); ok {
			if wasFenced {
				return links, formatFencedWrapped, nil
			}
			return links, formatWrapped, nil
		}

		// Drift form 1 (Welle-49, qwen3.6:27b via OpenRouter): single flat
		// object with top-level target_id/type/confidence fields. LLM emits
		// this when there's exactly one link instead of wrapping it in an
		// array. Tried before the wrapper-map path below, which would
		// mis-interpret `target_id`/`type`/`confidence` as map-keys pointing
		// at nested structs. The wrapper branch above defers to it by key
		// signature for the same reason.
		var single struct {
			TargetID   string          `json:"target_id"`
			Type       string          `json:"type"`
			Confidence json.RawMessage `json:"confidence"`
		}
		if err := json.Unmarshal([]byte(body), &single); err == nil && single.TargetID != "" && single.Type != "" {
			conf, floored, ok := confidenceOrFloor(single.Confidence, single.Type)
			if !ok {
				// Zero-link contract (see array branch): the model named a link
				// but its confidence is unusable — error (retry), not a silent
				// zero-link success that books the inert cooldown.
				return nil, "", fmt.Errorf("parse links: flat single link with unusable confidence: %w", arrErr)
			}
			return []Link{{TargetID: single.TargetID, Relationship: single.Type, Confidence: conf, Floored: floored}}, formatObject, nil
		}

		// Drift form 2 (Welle-25, qwen3.6:27b ollama): wrapper-map with IDs as
		// keys — see parseObjectMapLinks.
		if links, degenerate, ok := parseObjectMapLinks(body); ok {
			// Zero-link contract (see array branch): covers {"<uuid>": null}
			// and maps whose every entry lost its confidence — error (retry),
			// not inert cooldown. The empty-object sentinel returned early.
			if degenerate {
				return nil, "", fmt.Errorf("parse links: object map entries present, none usable: %w", arrErr)
			}
			if wasFenced {
				return links, formatFencedObject, nil
			}
			return links, formatObject, nil
		}

		// Drift form 3 — see parseStringMapLinks. Distinct format token: the
		// whole point of parse_format telemetry is tracking WHICH drift form a
		// model emits; reusing formatObject (already shared by forms 1 and 2)
		// would make this form invisible in the drift log.
		if links, ok := parseStringMapLinks(body); ok {
			if len(links) == 0 {
				return nil, "", fmt.Errorf("parse links: string map has no uuid→relationship entries (prose or degenerate object): %w", arrErr)
			}
			if wasFenced {
				return links, formatFencedStringMap, nil
			}
			return links, formatStringMap, nil
		}
	}

	return nil, "", fmt.Errorf("parse links: %w", arrErr)
}

// parseStringMapLinks handles drift form 3 (deepseek-v4-flash via opencode.ai,
// 2026-08-01): compact string-map with IDs as keys and the relationship type
// as a bare string value, no confidence field — {"<uuid>": "topical", ...}.
// The model emits this when it collapses the array-of-objects into a terse
// id→type map. Tried after the object-map form, whose struct-valued unmarshal
// fails on the bare-string values and falls through to here.
//
// This shape is maximally ambiguous: ANY flat string→string object matches
// map[string]string, including prose/status envelopes like
// {"reasoning": "the blocks are unrelated"} that MUST stay parse errors
// (transient retry) — accepting them as pseudo-links would read downstream as
// "nothing to link" and book the multi-day inert cooldown the
// parseWrappedLinks constraints exist to prevent. So unlike the other drift
// forms, entries are discriminated HERE: the key must look like a block UUID
// and the value must name a known relationship type (both case-normalised so
// the entry clears filterValidCandidates unchanged). Returns ok=false when
// body is not a flat string map at all; ok=true with an empty slice when it
// is one but no entry qualifies — the caller turns that into the hard parse
// error (this is not a link map).
//
// Confidence is ABSENT: the model named a relationship type but gave no
// strength signal. The parser assigns the per-type minRawConfidence floor —
// the lowest value that still clears the dream write gate for that type
// (0.7; 'recurrent' 0.8) — and marks the link Floored. The OPERATOR decides
// what a type-only answer is worth: applyLinkFloor (evaluate.go) lifts
// floored links to dream.link_floor_confidence (default 0.9 — above the RRF
// graph-expansion retrieval gate graph.min_confidence 0.75, so the edges are
// retrieval-live; set 0.7 to keep them write-only until a later cycle
// re-classifies them with a real signal).
func parseStringMapLinks(body string) ([]Link, bool) {
	var strMap map[string]string
	if err := json.Unmarshal([]byte(body), &strMap); err != nil {
		return nil, false
	}
	ids := make([]string, 0, len(strMap))
	for id := range strMap {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Link, 0, len(strMap))
	for _, rawID := range ids {
		id := strings.ToLower(strings.TrimSpace(rawID))
		rel := strings.ToLower(strings.TrimSpace(strMap[rawID]))
		if !uuidPattern.MatchString(id) || !validRelationships[rel] {
			continue
		}
		out = append(out, Link{TargetID: id, Relationship: rel, Confidence: minRawConfidence[rel], Floored: true})
	}
	return out, true
}

// parseWrappedLinks handles drift form 0 (cloud relays via response_format
// json_object, PR #11 review): the OpenAI-style object constraint contradicts
// the prompt's "output a JSON array", and models resolve the conflict by
// wrapping the array under a named key — {"analysis": [...]} (single key) or
// {"reasoning": "prose...", "relationships": [...]} (array plus commentary
// fields). Returns ok=false when body is not such a wrapper, leaving the
// caller on its remaining drift forms. Three hard constraints, each from an
// empirical finding of the PR #11 review:
//
//  1. A top-level `target_id` key means this is drift form 1 (flat single
//     link), whose side arrays (e.g. "evidence") would otherwise be stolen as
//     the link list — 30/30 probes lost the link. Decline outright.
//  2. Keys are scanned in sorted order. Go map iteration is randomised, so an
//     unsorted scan picked a different array per run when several were present
//     (25/30 vs 5/30) — the same reason drift form 2 pins its key order.
//  3. A candidate array only counts when it yields at least one link. An empty
//     or link-less array (a `"warnings": []` sibling was the observed case,
//     28/30 probes returning 0 links) must NOT terminate the scan: reporting
//     success with no links reads downstream as "nothing to link" and books a
//     multi-day dream cooldown on the block, instead of the transient error
//     retry the situation deserves. If no array yields links we decline, and
//     the caller ultimately reaches its hard parse error.
func parseWrappedLinks(body string) ([]Link, bool) {
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &wrapper); err != nil || len(wrapper) == 0 {
		return nil, false
	}
	if _, isFlatSingle := wrapper["target_id"]; isFlatSingle {
		return nil, false
	}
	keys := make([]string, 0, len(wrapper))
	for k := range wrapper {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val := strings.TrimSpace(string(wrapper[k]))
		if !strings.HasPrefix(val, "[") {
			continue
		}
		links, _, err := parseLinks(val)
		if err != nil || len(links) == 0 {
			continue
		}
		return links, true
	}
	// Lone empty-array wrapper, e.g. {"classifications": []} or
	// {"reasoning": "none", "relationships": []}: the model explicitly says
	// "nothing to link" — the same verdict as the bare "[]" sentinel, which
	// parseLinks accepts at its head. The scan above already declined (no
	// array yielded a link), and when the response carries exactly ONE array
	// there is no sibling array left that could hold the links constraint 3
	// protects. Two or more arrays keep declining, so {"warnings": [],
	// "notes": []} stays the transient error it has to be.
	if isLoneEmptyArrayWrapper(body) {
		return nil, true
	}
	return nil, false
}

// isLoneEmptyArrayWrapper reports whether the wrapper's only ARRAY-valued
// top-level key holds an empty array — the unambiguous "nothing to link"
// verdict. Prose keys are ignored on purpose: the canonical json_object relay
// shape with nothing to report is {"reasoning": "...", "relationships": []},
// and a commentary string can never be the array constraint 3 defends. What
// the constraint rules out is a SECOND array that might still hold links, so
// the count that decides is the number of array-valued keys, not of keys.
//
// Keys are counted on the TEXT (topLevelValues), not on the decoded map: Go
// keeps the last of duplicate JSON keys, so a repeated key would otherwise
// collapse two emitted arrays into one and re-open exactly the hole this
// guards.
func isLoneEmptyArrayWrapper(body string) bool {
	vals, ok := topLevelValues(body)
	if !ok {
		return false
	}
	arrays, empty := 0, false
	for _, val := range vals {
		if !strings.HasPrefix(strings.TrimSpace(string(val)), "[") {
			continue
		}
		arrays++
		if arrays > 1 {
			return false
		}
		empty = isEmptyJSONArray(val)
	}
	return arrays == 1 && empty
}

// topLevelValues returns the value of every key that textually appears at the
// top level of a JSON object, in emission order, DUPLICATES INCLUDED. It exists
// because len(map) lies about the wrapper's shape: encoding/json keeps the last
// of duplicate keys, so {"relationships":[<link>],"relationships":[]} decodes
// into a one-entry map holding the empty array — the lone-key verdict would
// book it as an inert "nothing to link" although the raw text carried a usable
// link. Walking the tokens counts what the model actually emitted. Returns
// ok=false for anything that is not a well-formed top-level object; the caller
// then declines and the hard parse error (transient retry) stays reachable.
func topLevelValues(body string) ([]json.RawMessage, bool) {
	dec := json.NewDecoder(strings.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return nil, false
	}
	out := make([]json.RawMessage, 0, 4)
	for dec.More() {
		if _, err := dec.Token(); err != nil { // the key
			return nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false
		}
		out = append(out, val)
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, false
	}
	return out, true
}

// isEmptyJSONArray reports whether v holds a JSON array with no elements.
// The test is structural rather than a byte compare against "[]": a
// json.RawMessage keeps the interior bytes verbatim, so the whitespace drift
// models actually emit — "[ ]", "[\n  ]", any pretty-printed variant — would
// slip past a literal compare and land back on the hard unmarshal error. The
// "[" prefix guard is load-bearing: JSON null unmarshals into a nil slice of
// length 0, so without it {"<uuid>": null} would flip from the transient
// parse error the zero-link contract demands into an inert zero-link success.
func isEmptyJSONArray(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	if !strings.HasPrefix(s, "[") {
		return false
	}
	var arr []json.RawMessage
	return json.Unmarshal([]byte(s), &arr) == nil && len(arr) == 0
}

// stripCodeFence removes a leading ```json (or ```) fence and trailing ```
// from LLM responses that wrap their JSON output. Idempotent on plain JSON.
func stripCodeFence(raw string) string {
	if !strings.HasPrefix(raw, "```") {
		return raw
	}
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}

// parseObjectMapLinks handles drift form 2 (Welle-25, qwen3.6:27b ollama):
// wrapper-map with IDs as keys and {type, confidence} struct values.
// Returns ok=false when body is not such a map; degenerate=true when it IS
// one with entries but none was usable (the caller surfaces that as a parse
// error under the zero-link contract). Map→slice conversion sorts by
// TargetID for deterministic downstream behavior.
func parseObjectMapLinks(body string) (links []Link, degenerate, ok bool) {
	var obj map[string]struct {
		Type       string          `json:"type"`
		Confidence json.RawMessage `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return nil, false, false
	}
	ids := make([]string, 0, len(obj))
	for id := range obj {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Link, 0, len(obj))
	for _, id := range ids {
		v := obj[id]
		conf, floored, confOK := confidenceOrFloor(v.Confidence, v.Type)
		if !confOK {
			continue
		}
		out = append(out, Link{TargetID: id, Relationship: v.Type, Confidence: conf, Floored: floored})
	}
	return out, len(obj) > 0 && len(out) == 0, true
}

// confidenceOrFloor resolves an entry's confidence. A present value goes
// through coerceConfidence unchanged (floored=false). An ABSENT one (missing
// key or JSON null) falls back to the per-type minRawConfidence floor when
// the type names a known relationship — the model committed to a
// relationship but gave no strength signal, the same doctrine the string-map
// form applies (PR #12); such entries report floored=true so applyLinkFloor
// can lift them to the configured dream.link_floor_confidence. Present-but-
// unparseable values stay dropped (the model DID emit a strength signal, it
// is just unreadable — retry may fix it), and absent values with an unknown
// type cannot be floored.
func confidenceOrFloor(raw json.RawMessage, rel string) (conf float64, floored, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		if validRelationships[rel] {
			return minRawConfidence[rel], true, true
		}
		return 0, false, false
	}
	conf, ok = coerceConfidence(raw)
	return conf, false, ok
}

// coerceConfidence accepts a json.RawMessage and returns a float64 confidence.
// Tolerates float (canonical) and string labels emitted under format drift.
// Mapping: "high"=0.9, "medium"=0.6, "low"=0.3 — chosen so that "medium"/"low"
// land below the 0.7 minRawConfidence gate and get dropped downstream rather
// than synthesising a bogus pass-through value. Unparseable input returns
// ok=false so the caller can drop the entry.
func coerceConfidence(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "high":
			return 0.9, true
		case "medium":
			return 0.6, true
		case "low":
			return 0.3, true
		}
	}
	return 0, false
}
