package dream

import (
	"encoding/json"
	"testing"
	"time"
)

// --- ExtractKeywords Tests ---.

func TestExtractKeywords_BasicExtraction(t *testing.T) {
	title := "PostgreSQL Embedding Configuration"
	content := "The embedding model uses qwen3-embedding with 1024 dimensions. PostgreSQL stores vectors via pgvector extension. The HNSW index provides approximate nearest neighbor search."

	keywords := ExtractKeywords(title, content, 5)
	if len(keywords) == 0 {
		t.Fatal("expected at least 1 keyword")
	}
	if len(keywords) > 5 {
		t.Fatalf("expected at most 5 keywords, got %d", len(keywords))
	}

	// "postgresql" should be high-ranked (in title + content).
	found := false
	for _, kw := range keywords {
		if kw == "postgresql" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'postgresql' in keywords (title+content term), got %v", keywords)
	}
}

func TestExtractKeywords_StopwordsRemoved(t *testing.T) {
	keywords := ExtractKeywords("Test", "the and or but not with for from this that", 5)
	if len(keywords) != 0 {
		t.Errorf("expected no keywords from pure stopwords, got %v", keywords)
	}
}

func TestExtractKeywords_ShortWordsRemoved(t *testing.T) {
	keywords := ExtractKeywords("AB", "ab cd ef gh ij kl", 5)
	if len(keywords) != 0 {
		t.Errorf("expected no keywords from <3 char terms, got %v", keywords)
	}
}

func TestExtractKeywords_TitlePriority(t *testing.T) {
	title := "Gravity"
	content := "gravity is used for temporal ranking. semantic search uses embeddings. gravity provides distance-based scoring."

	keywords := ExtractKeywords(title, content, 3)
	if len(keywords) == 0 {
		t.Fatal("expected keywords")
	}
	if keywords[0] != "gravity" {
		t.Errorf("expected 'gravity' as top keyword (title bonus), got %q", keywords[0])
	}
}

func TestExtractKeywords_EmptyContent(t *testing.T) {
	keywords := ExtractKeywords("", "", 5)
	if len(keywords) != 0 {
		t.Errorf("expected no keywords from empty content, got %v", keywords)
	}
}

func TestExtractKeywords_LimitRespected(t *testing.T) {
	content := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda"
	keywords := ExtractKeywords("Test", content, 3)
	if len(keywords) > 3 {
		t.Errorf("expected at most 3 keywords, got %d: %v", len(keywords), keywords)
	}
}

func TestExtractKeywords_GermanContent(t *testing.T) {
	title := "Konfiguration"
	content := "Die Konfiguration der Datenbank erfolgt über Umgebungsvariablen. PostgreSQL verwendet pgvector für Vektorsuche."

	keywords := ExtractKeywords(title, content, 5)
	// Should filter German stopwords (die, der, über, für).
	for _, kw := range keywords {
		if isStopword(kw) {
			t.Errorf("stopword %q should not be in keywords", kw)
		}
	}
}

func TestExtractKeywords_Deterministic(t *testing.T) {
	title := "Dream Mode Architecture"
	content := "Dream Mode picks random blocks and evaluates cross-references using keyword extraction and RRF search."

	kw1 := ExtractKeywords(title, content, 5)
	kw2 := ExtractKeywords(title, content, 5)

	if len(kw1) != len(kw2) {
		t.Fatalf("non-deterministic: %v vs %v", kw1, kw2)
	}
	for i := range kw1 {
		if kw1[i] != kw2[i] {
			t.Fatalf("non-deterministic at index %d: %q vs %q", i, kw1[i], kw2[i])
		}
	}
}

// --- tokenize Tests ---.

func TestTokenize_Basic(t *testing.T) {
	tokens := tokenize("Hello, World! Test-Case 123.")
	if len(tokens) == 0 {
		t.Fatal("expected tokens from non-empty string")
	}
	for _, tok := range tokens {
		if tok == "" {
			t.Error("empty token found")
		}
	}
}

func TestTokenize_Empty(t *testing.T) {
	tokens := tokenize("")
	if len(tokens) != 0 {
		t.Errorf("expected no tokens from empty string, got %v", tokens)
	}
}

// --- parseLinks Tests ---.

func TestParseLinks_ValidJSON(t *testing.T) {
	raw := `[{"target_id":"abc-123","type":"factual","confidence":0.85}]`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}
	if links[0].TargetID != "abc-123" {
		t.Errorf("expected target_id 'abc-123', got %q", links[0].TargetID)
	}
	if links[0].Relationship != "factual" {
		t.Errorf("expected type 'factual', got %q", links[0].Relationship)
	}
	if links[0].Confidence != 0.85 {
		t.Errorf("expected confidence 0.85, got %f", links[0].Confidence)
	}
}

func TestParseLinks_EmptyArray(t *testing.T) {
	links, _, err := parseLinks("[]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expected 0 links, got %d", len(links))
	}
}

func TestParseLinks_EmptyString(t *testing.T) {
	links, _, err := parseLinks("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expected 0 links, got %d", len(links))
	}
}

func TestParseLinks_InvalidJSON(t *testing.T) {
	_, _, err := parseLinks("not json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseLinks_MultipleLinks(t *testing.T) {
	raw := `[
		{"target_id":"a","type":"topical","confidence":0.6},
		{"target_id":"b","type":"causal","confidence":0.9},
		{"target_id":"c","type":"supersedes","confidence":0.95}
	]`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("expected 3 links, got %d", len(links))
	}
}

// --- parseLinks Array-Form String-Confidence Drift Tests (S37 prod drift, 2026-05-20) ---.

func TestParseLinks_ArrayForm_StringConfidence(t *testing.T) {
	raw := `[
		{"target_id":"019d-aaaa","type":"topical","confidence":"high"},
		{"target_id":"019d-bbbb","type":"topical","confidence":"medium"},
		{"target_id":"019d-cccc","type":"factual","confidence":"low"}
	]`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("want 3 links, got %d", len(links))
	}
	want := map[string]float64{"019d-aaaa": 0.9, "019d-bbbb": 0.6, "019d-cccc": 0.3}
	for _, l := range links {
		if l.Confidence != want[l.TargetID] {
			t.Errorf("%s: want %.2f, got %.2f", l.TargetID, want[l.TargetID], l.Confidence)
		}
	}
}

func TestParseLinks_ArrayForm_MixedConfidence(t *testing.T) {
	raw := `[{"target_id":"a","type":"topical","confidence":0.85},{"target_id":"b","type":"causal","confidence":"high"}]`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 2 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	seen := map[string]float64{}
	for _, l := range links {
		seen[l.TargetID] = l.Confidence
	}
	if seen["a"] != 0.85 || seen["b"] != 0.9 {
		t.Errorf("got %v", seen)
	}
}

func TestParseLinks_ArrayForm_UnknownStringConfDropped(t *testing.T) {
	raw := `[{"target_id":"a","type":"topical","confidence":"vibes"},{"target_id":"b","type":"topical","confidence":0.8}]`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 1 || links[0].TargetID != "b" {
		t.Fatalf("want vibes-entry dropped, got %+v", links)
	}
}

// --- parseLinks Object-Form Drift Tests (audit S25, 2026-05-03) ---.

func TestParseLinks_ObjectForm_FloatConfidence(t *testing.T) {
	raw := `{"019d-aaaa":{"type":"topical","confidence":0.9}}`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 1 || links[0].TargetID != "019d-aaaa" ||
		links[0].Relationship != "topical" || links[0].Confidence != 0.9 {
		t.Fatalf("got %+v", links)
	}
}

func TestParseLinks_ObjectForm_StringConfidence(t *testing.T) {
	raw := `{"a":{"type":"supersedes","confidence":"high"},"b":{"type":"topical","confidence":"medium"},"c":{"type":"factual","confidence":"low"}}`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("want 3 links, got %d", len(links))
	}
	want := map[string]float64{"a": 0.9, "b": 0.6, "c": 0.3}
	for _, l := range links {
		if l.Confidence != want[l.TargetID] {
			t.Errorf("%s: want %.2f, got %.2f", l.TargetID, want[l.TargetID], l.Confidence)
		}
	}
}

func TestParseLinks_ObjectForm_Mixed(t *testing.T) {
	raw := `{"a":{"type":"topical","confidence":0.85},"b":{"type":"causal","confidence":"high"}}`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 2 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	seen := map[string]float64{}
	for _, l := range links {
		seen[l.TargetID] = l.Confidence
	}
	if seen["a"] != 0.85 || seen["b"] != 0.9 {
		t.Errorf("got %v", seen)
	}
}

func TestParseLinks_ObjectForm_GarbageKey(t *testing.T) {
	raw := `{"not-a-uuid":{"type":"topical","confidence":0.8}}`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("non-UUID keys must pass parser (downstream filters): %v", err)
	}
	if len(links) != 1 || links[0].TargetID != "not-a-uuid" {
		t.Fatalf("want passthrough, got %+v", links)
	}
}

func TestParseLinks_ObjectForm_UnknownStringConf(t *testing.T) {
	// Contract change (PR #12 review, finding: zero-link contract): the entry
	// itself is still dropped, but a non-empty map whose EVERY entry drops is
	// degenerate output and must surface as a parse error (transient retry) —
	// previously this returned a zero-link success, which books the multi-day
	// inert cooldown and contradicted TestParseLinks_StringMap_AllEmptyErrors'
	// contract for the same downstream consequence.
	raw := `{"id":{"type":"topical","confidence":"vibes"}}`
	if _, _, err := parseLinks(raw); err == nil {
		t.Fatal("map with only unusable entries must be a parse error")
	}
	// A sibling entry with usable confidence keeps the map parseable; only
	// the unusable entry is dropped.
	raw = `{"a":{"type":"topical","confidence":"vibes"},"b":{"type":"factual","confidence":0.9}}`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 || links[0].TargetID != "b" {
		t.Fatalf("err=%v links=%+v", err, links)
	}
}

func TestParseLinks_ObjectForm_NullValueErrors(t *testing.T) {
	// {"<uuid>": null} unmarshals into the struct-map form as a zero-value
	// no-op entry; without the zero-link guard it returned a silent success
	// (inert cooldown) instead of the transient retry it deserves.
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":null}`
	if _, _, err := parseLinks(raw); err == nil {
		t.Fatal("null-valued map entry must be a parse error")
	}
}

func TestParseLinks_ObjectForm_MissingConfidenceFloored(t *testing.T) {
	// Absent confidence (key missing entirely) on a known type takes the
	// per-type floor — the model committed to a relationship, it just gave no
	// strength signal (string-map doctrine applied to the object-map form).
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":{"type":"topical"}}`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	if links[0].Confidence != minRawConfidence["topical"] {
		t.Fatalf("want per-type floor %.2f, got %.2f", minRawConfidence["topical"], links[0].Confidence)
	}
}

func TestParseLinks_FlatSingle_MissingConfidenceFloored(t *testing.T) {
	raw := `{"target_id":"019fb992-ea5a-7ef8-aa5c-ed7db94699ca","type":"causal"}`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	if links[0].Confidence != minRawConfidence["causal"] {
		t.Fatalf("want per-type floor %.2f, got %.2f", minRawConfidence["causal"], links[0].Confidence)
	}
}

func TestParseLinks_FlatSingle_UnusableConfidenceErrors(t *testing.T) {
	// Present-but-unparseable confidence on the single-link form leaves zero
	// links — parse error (retry), not a silent zero-link success. Was a
	// (nil, "", nil) return before the zero-link contract.
	raw := `{"target_id":"019fb992-ea5a-7ef8-aa5c-ed7db94699ca","type":"topical","confidence":"vibes"}`
	if _, _, err := parseLinks(raw); err == nil {
		t.Fatal("unusable confidence on flat single link must be a parse error")
	}
}

func TestParseLinks_Array_AllUnusableErrors(t *testing.T) {
	// Same contract on the canonical array form: entries exist but none is
	// usable (unknown type + no confidence, or unparseable confidence).
	raw := `[{"target_id":"019fb992-ea5a-7ef8-aa5c-ed7db94699ca","type":"topical","confidence":"vibes"}]`
	if _, _, err := parseLinks(raw); err == nil {
		t.Fatal("array with only unusable entries must be a parse error")
	}
}

func TestParseLinks_Array_MissingConfidenceFloored(t *testing.T) {
	raw := `[{"target_id":"019fb992-ea5a-7ef8-aa5c-ed7db94699ca","type":"supersedes"}]`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	if links[0].Confidence != minRawConfidence["supersedes"] {
		t.Fatalf("want per-type floor %.2f, got %.2f", minRawConfidence["supersedes"], links[0].Confidence)
	}
}

func TestParseLinks_EmptyObject(t *testing.T) {
	links, _, err := parseLinks("{}")
	if err != nil || len(links) != 0 {
		t.Fatalf("got %v err=%v", links, err)
	}
}

func TestParseLinks_ObjectForm_DeterministicOrder(t *testing.T) {
	raw := `{"c-3":{"type":"topical","confidence":0.7},"a-1":{"type":"topical","confidence":0.7},"b-2":{"type":"topical","confidence":0.7}}`
	for i := 0; i < 20; i++ {
		links, _, err := parseLinks(raw)
		if err != nil || len(links) != 3 {
			t.Fatalf("iter %d: err=%v len=%d", i, err, len(links))
		}
		if links[0].TargetID != "a-1" || links[1].TargetID != "b-2" || links[2].TargetID != "c-3" {
			t.Fatalf("iter %d: order not lex-sorted: %+v", i, links)
		}
	}
}

// --- parseLinks String-Map Drift Tests (deepseek-v4-flash, 2026-08-01) ---.
//
// Drift form 3: the model collapses the array-of-objects into a terse
// {"<uuid>": "<type>"} map with NO confidence field. The parser must recover
// the relationship the model committed to (the type name) and assign the
// per-type minRawConfidence floor — not invent a "high" 0.9 the model never
// produced. Because ANY flat string→string object matches this shape, entries
// are discriminated (uuid key + known relationship value); prose/status
// envelopes must remain parse errors (transient retry, not inert cooldown).

func TestParseLinks_StringMap_Basic(t *testing.T) {
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"topical","019fb98f-cad3-7bbe-b3be-ccf74d5ba05f":"factual"}`
	links, format, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("want 2 links, got %d", len(links))
	}
	if format != formatStringMap {
		t.Errorf("want format %q, got %q", formatStringMap, format)
	}
	seen := map[string]string{}
	for _, l := range links {
		seen[l.TargetID] = l.Relationship
		if l.Confidence != 0.7 {
			t.Errorf("%s: confidence must be gate floor 0.7 (model gave none), got %.2f", l.TargetID, l.Confidence)
		}
	}
	if seen["019fb992-ea5a-7ef8-aa5c-ed7db94699ca"] != "topical" ||
		seen["019fb98f-cad3-7bbe-b3be-ccf74d5ba05f"] != "factual" {
		t.Errorf("relationship types lost: %v", seen)
	}
}

func TestParseLinks_StringMap_DeterministicOrder(t *testing.T) {
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699cc":"topical","019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"causal","019fb992-ea5a-7ef8-aa5c-ed7db94699cb":"factual"}`
	for i := 0; i < 20; i++ {
		links, _, err := parseLinks(raw)
		if err != nil || len(links) != 3 {
			t.Fatalf("iter %d: err=%v len=%d", i, err, len(links))
		}
		if links[0].TargetID != "019fb992-ea5a-7ef8-aa5c-ed7db94699ca" ||
			links[1].TargetID != "019fb992-ea5a-7ef8-aa5c-ed7db94699cb" ||
			links[2].TargetID != "019fb992-ea5a-7ef8-aa5c-ed7db94699cc" {
			t.Fatalf("iter %d: order not lex-sorted: %+v", i, links)
		}
		// Key→value pairing must survive the sort, not just the ordering.
		if links[0].Relationship != "causal" || links[1].Relationship != "factual" || links[2].Relationship != "topical" {
			t.Fatalf("iter %d: key→value pairing broken: %+v", i, links)
		}
	}
}

func TestParseLinks_StringMap_EmptyValueDropped(t *testing.T) {
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"topical","019fb992-ea5a-7ef8-aa5c-ed7db94699cb":"   ","019fb992-ea5a-7ef8-aa5c-ed7db94699cc":""}`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 1 || links[0].TargetID != "019fb992-ea5a-7ef8-aa5c-ed7db94699ca" {
		t.Fatalf("blank relationship values must be dropped, got %+v", links)
	}
}

func TestParseLinks_StringMap_AllEmptyErrors(t *testing.T) {
	// A map whose values are all blank yields zero links; that must surface as
	// a parse error (transient retry) rather than a silent "no links" success
	// that would book a multi-day dream cooldown on the block.
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"","019fb992-ea5a-7ef8-aa5c-ed7db94699cb":"  "}`
	_, _, err := parseLinks(raw)
	if err == nil {
		t.Error("expected error when all string-map values are blank")
	}
}

func TestParseLinks_StringMap_Fenced(t *testing.T) {
	raw := "```json\n{\"019fb992-ea5a-7ef8-aa5c-ed7db94699ca\":\"supersedes\"}\n```"
	links, format, err := parseLinks(raw)
	if err != nil || len(links) != 1 || links[0].TargetID != "019fb992-ea5a-7ef8-aa5c-ed7db94699ca" {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	if format != formatFencedStringMap {
		t.Errorf("want format %q, got %q", formatFencedStringMap, format)
	}
}

func TestParseLinks_StringMap_ProseEnvelopeStaysError(t *testing.T) {
	// The critical boundary of this drift form: ANY flat string→string object
	// unmarshals into map[string]string, including prose/status envelopes.
	// Those carried a parse error (transient ~5-min retry) before this form
	// existed; they must KEEP erroring — a pseudo-link parse would be filtered
	// by filterValidCandidates into a zero-link success and book the multi-day
	// inert cooldown instead (observed classes: reasoning envelopes, status
	// envelopes, stringified arrays).
	for _, raw := range []string{
		`{"reasoning":"the blocks are unrelated","conclusion":"no links"}`,
		`{"analysis":"I found no relationships between the source and the candidates."}`,
		`{"status":"ok","links":"none"}`,
		`{"relationships":"none"}`,
		`{"result":"no relationships found"}`,
		`{"relationships":"[{\"target_id\":\"019fb992-ea5a-7ef8-aa5c-ed7db94699ca\"}]"}`,
	} {
		if _, _, err := parseLinks(raw); err == nil {
			t.Errorf("prose envelope must stay a parse error, got success for %s", raw)
		}
	}
}

func TestParseLinks_StringMap_UnknownRelationshipErrors(t *testing.T) {
	// A uuid key with an unknown relationship value is indistinguishable from
	// prose; with no qualifying entry left the map must decline to the parse
	// error (retry may yield the canonical array with a known type).
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"related"}`
	if _, _, err := parseLinks(raw); err == nil {
		t.Error("unknown relationship value must not produce a link")
	}
}

func TestParseLinks_StringMap_MixedProseEntriesIgnored(t *testing.T) {
	// Commentary keys next to a valid uuid→type entry are dropped, the valid
	// entry survives — mirrors the wrapped form skipping sibling prose fields.
	raw := `{"note":"weak but real","019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"topical"}`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	if links[0].TargetID != "019fb992-ea5a-7ef8-aa5c-ed7db94699ca" || links[0].Relationship != "topical" {
		t.Fatalf("valid entry lost among prose keys: %+v", links[0])
	}
}

func TestParseLinks_StringMap_CaseAndWhitespaceNormalised(t *testing.T) {
	// deepseek drift includes cased type names and cased UUIDs; both are
	// normalised so the entry clears filterValidCandidates unchanged.
	raw := `{"019FB992-EA5A-7EF8-AA5C-ED7DB94699CA":" Topical "}`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	if links[0].TargetID != "019fb992-ea5a-7ef8-aa5c-ed7db94699ca" || links[0].Relationship != "topical" {
		t.Fatalf("case/whitespace not normalised: %+v", links[0])
	}
}

func TestParseLinks_StringMap_PerTypeFloor(t *testing.T) {
	// The assigned confidence is the per-type minRawConfidence floor, not a
	// hardcoded 0.7 — 'recurrent' has floor 0.8 and would be silently dropped
	// by filterValidCandidates at 0.7.
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"recurrent"}`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	if links[0].Confidence != minRawConfidence["recurrent"] {
		t.Fatalf("want per-type floor %.2f, got %.2f", minRawConfidence["recurrent"], links[0].Confidence)
	}
}

func TestParseLinks_StringMap_FlatSingleShapeStaysError(t *testing.T) {
	// {"target_id": "<uuid>", "type": ""} misses the flat-single form (empty
	// type) and is all-strings — without key discrimination it would parse as
	// a garbage link with TargetID "target_id".
	raw := `{"target_id":"019fb992-ea5a-7ef8-aa5c-ed7db94699ca","type":""}`
	if _, _, err := parseLinks(raw); err == nil {
		t.Error("degenerate flat-single shape must stay a parse error")
	}
}

func TestParseLinks_CodeFenceArray(t *testing.T) {
	raw := "```json\n[{\"target_id\":\"id-1\",\"type\":\"factual\",\"confidence\":0.9}]\n```"
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 || links[0].TargetID != "id-1" {
		t.Fatalf("err=%v links=%+v", err, links)
	}
}

func TestParseLinks_CodeFenceObject(t *testing.T) {
	raw := "```\n{\"id\":{\"type\":\"causal\",\"confidence\":\"high\"}}\n```"
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 1 || links[0].Confidence != 0.9 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
}

func TestCoerceConfidence(t *testing.T) {
	cases := []struct {
		raw    string
		want   float64
		wantOk bool
	}{
		{"0.85", 0.85, true},
		{"1.0", 1.0, true},
		{`"high"`, 0.9, true},
		{`"HIGH"`, 0.9, true},
		{`"medium"`, 0.6, true},
		{`"low"`, 0.3, true},
		{`"vibes"`, 0, false},
		{`""`, 0, false},
		{"null", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := coerceConfidence(json.RawMessage(c.raw))
		if got != c.want || ok != c.wantOk {
			t.Errorf("coerceConfidence(%q): got=(%v,%v), want=(%v,%v)", c.raw, got, ok, c.want, c.wantOk)
		}
	}
}

// --- parseLinks Named-Wrapper Drift Tests (drift form 0, PR #11 review) ---.
//
// Cloud relays under response_format json_object wrap the requested array in a
// named key. Every case runs repeatedly: the branch scans a Go map, so a single
// pass would not distinguish "picks the right array" from "picked it by chance"
// (the unpinned version returned a different array per run, 25/30 vs 5/30).
func TestParseLinks_WrapperForm(t *testing.T) {
	const linkA = `{"target_id":"a","type":"topical","confidence":0.9}`
	const linkB = `{"target_id":"b","type":"causal","confidence":0.8}`

	cases := []struct {
		name       string
		raw        string
		wantIDs    []string
		wantFormat string
		wantErr    bool
	}{
		{
			name:       "single-key",
			raw:        `{"analysis":[` + linkA + `]}`,
			wantIDs:    []string{"a"},
			wantFormat: formatWrapped,
		},
		{
			name:       "multi-key-with-prose",
			raw:        `{"analysis":"the blocks share a topic","relationships":[` + linkA + `,` + linkB + `]}`,
			wantIDs:    []string{"a", "b"},
			wantFormat: formatWrapped,
		},
		{
			// Empty sibling arrays must not end the scan: a (nil, nil) return
			// here would book a multi-day dream cooldown instead of a retry.
			name:       "empty-sibling-array",
			raw:        `{"warnings":[],"relationships":[` + linkA + `,` + linkB + `]}`,
			wantIDs:    []string{"a", "b"},
			wantFormat: formatWrapped,
		},
		{
			// Same, with the empty array sorting FIRST — proves the skip is
			// real and not an artifact of the key order.
			name:       "empty-array-sorts-first",
			raw:        `{"aaa_warnings":[],"zzz_relationships":[` + linkA + `]}`,
			wantIDs:    []string{"a"},
			wantFormat: formatWrapped,
		},
		{
			// Ambiguous input: pick deterministically, alphabetically first.
			name:       "two-populated-arrays",
			raw:        `{"beta":[` + linkB + `],"alpha":[` + linkA + `]}`,
			wantIDs:    []string{"a"},
			wantFormat: formatWrapped,
		},
		{
			// Regression pin: drift form 1 with a side array. The wrapper
			// branch must defer on the top-level target_id signature.
			name:       "form1-with-evidence-side-array",
			raw:        `{"target_id":"u1","type":"topical","confidence":0.9,"evidence":[{"target_id":"nope","type":"factual","confidence":0.95}]}`,
			wantIDs:    []string{"u1"},
			wantFormat: formatObject,
		},
		{
			// Single-key empty-array wrapper (deepseek-v4-flash via
			// opencode.ai, 2026-08-14): {"classifications": []} — the model
			// explicitly says "nothing to link", same verdict as the bare
			// "[]" sentinel. Must NOT surface as an unmarshal error.
			name:       "single-key-empty-array",
			raw:        `{"classifications":[]}`,
			wantIDs:    []string{},
			wantFormat: formatWrapped,
		},
		{
			// Same verdict via the canonical key name.
			name:       "single-key-empty-relationships",
			raw:        `{"relationships":[]}`,
			wantIDs:    []string{},
			wantFormat: formatWrapped,
		},
		{
			// Fenced variant of the same drift.
			name:       "fenced-single-key-empty-array",
			raw:        "```json\n{\"classifications\":[]}\n```",
			wantIDs:    []string{},
			wantFormat: formatFencedWrapped,
		},
		{
			// Whitespace inside the brackets is the same verdict: the
			// emptiness test is structural, not a byte compare against "[]".
			name:       "single-key-empty-array-spaced",
			raw:        `{"classifications": [ ]}`,
			wantIDs:    []string{},
			wantFormat: formatWrapped,
		},
		{
			// Pretty-printed relay output — the shape a json_object relay
			// emits with indent enabled.
			name:       "single-key-empty-array-pretty",
			raw:        "{\n  \"classifications\": [\n  ]\n}",
			wantIDs:    []string{},
			wantFormat: formatWrapped,
		},
		{
			// Fenced pretty variant of the same drift.
			name:       "fenced-single-key-empty-array-pretty",
			raw:        "```json\n{\n  \"classifications\": [\n  ]\n}\n```",
			wantIDs:    []string{},
			wantFormat: formatFencedWrapped,
		},
		{
			// A null value is NOT an empty array: the model emitted no
			// verdict at all, which stays a transient parse error. Pins the
			// "[" prefix guard in isEmptyJSONArray — JSON null unmarshals
			// into a zero-length slice and would otherwise pass.
			name:    "single-key-null-value-errors",
			raw:     `{"classifications":null}`,
			wantErr: true,
		},
		{
			// Canonical json_object relay shape with nothing to report. The
			// prose key cannot be the sibling constraint 3 defends, so the
			// single empty array is as unambiguous as the bare "[]".
			name:       "prose-plus-empty-relationships",
			raw:        `{"reasoning":"none","relationships":[]}`,
			wantIDs:    []string{},
			wantFormat: formatWrapped,
		},
		{
			// Same shape as a relay emits it with indent enabled.
			name:       "prose-plus-empty-pretty",
			raw:        "{\n  \"reasoning\": \"none\",\n  \"relationships\": [\n  ]\n}",
			wantIDs:    []string{},
			wantFormat: formatWrapped,
		},
		{
			// Prose does not lift the two-array bar: with a second array
			// present the response stays ambiguous and must retry, even
			// though both arrays happen to be empty here.
			name:    "prose-plus-two-empty-arrays-errors",
			raw:     `{"reasoning":"x","warnings":[],"relationships":[]}`,
			wantErr: true,
		},
		{
			// Duplicate keys must not fake a lone empty array: Go keeps the
			// LAST one, so the decoded map holds a single empty array while
			// the raw text carried a usable link. Booking that as a
			// "nothing to link" verdict would lose the link to a multi-day
			// cooldown — the key count is taken from the text instead.
			name:    "duplicate-key-hiding-a-link-errors",
			raw:     `{"relationships":[` + linkA + `],"relationships":[]}`,
			wantErr: true,
		},
		{
			// Multi-key with an empty array still errors: an empty sibling
			// must not book a "nothing to link" verdict while a populated
			// key may exist (constraint 3 — regression pin stays).
			name:    "only-empty-arrays-errors",
			raw:     `{"warnings":[],"notes":[]}`,
			wantErr: true,
		},
		{
			// Array present but every entry drops on confidence coercion:
			// no links means no wrapper match, so the error path stays reachable.
			name:    "link-less-array-errors",
			raw:     `{"relationships":[{"target_id":"x","type":"topical","confidence":"vibes"}]}`,
			wantErr: true,
		},
		{
			name:       "fenced",
			raw:        "```json\n{\"relationships\":[" + linkA + "]}\n```",
			wantIDs:    []string{"a"},
			wantFormat: formatFencedWrapped,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				links, format, err := parseLinks(c.raw)
				if c.wantErr {
					if err == nil {
						t.Fatalf("iter %d: want error, got links=%+v format=%q", i, links, format)
					}
					if format != "" {
						t.Fatalf("iter %d: parse error must report empty format, got %q", i, format)
					}
					continue
				}
				if err != nil {
					t.Fatalf("iter %d: unexpected error: %v", i, err)
				}
				if format != c.wantFormat {
					t.Fatalf("iter %d: format = %q, want %q", i, format, c.wantFormat)
				}
				got := make([]string, 0, len(links))
				for _, l := range links {
					got = append(got, l.TargetID)
				}
				if len(got) != len(c.wantIDs) {
					t.Fatalf("iter %d: got IDs %v, want %v", i, got, c.wantIDs)
				}
				for j := range got {
					if got[j] != c.wantIDs[j] {
						t.Fatalf("iter %d: got IDs %v, want %v", i, got, c.wantIDs)
					}
				}
			}
		})
	}
}

// --- parseLinks Format-Tag Tests (S25 drift-counter) ---.

func TestParseLinks_FormatTag(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"array", `[{"target_id":"a","type":"factual","confidence":0.9}]`, "array"},
		{"fenced-array", "```json\n[{\"target_id\":\"a\",\"type\":\"factual\",\"confidence\":0.9}]\n```", "fenced-array"},
		{"object", `{"a":{"type":"topical","confidence":0.9}}`, "object"},
		{"fenced-object", "```\n{\"a\":{\"type\":\"causal\",\"confidence\":\"high\"}}\n```", "fenced-object"},
		{"wrapped", `{"relationships":[{"target_id":"a","type":"factual","confidence":0.9}]}`, "wrapped"},
		{"fenced-wrapped", "```json\n{\"relationships\":[{\"target_id\":\"a\",\"type\":\"factual\",\"confidence\":0.9}]}\n```", "fenced-wrapped"},
		{"empty-array", "[]", ""},
		{"empty-object", "{}", ""},
		{"empty-string", "", ""},
		{"parse-error", "not json", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, format, _ := parseLinks(c.raw)
			if format != c.want {
				t.Errorf("got %q, want %q", format, c.want)
			}
		})
	}
}

// --- applyHardCap Tests (S25 Welle 4 — Hard-Cap with Type-Diversity Tie-Break) ---.

func TestApplyHardCap_NoOpUnderCap(t *testing.T) {
	in := []Link{
		{TargetID: "a", Relationship: "topical", Confidence: 0.7},
		{TargetID: "b", Relationship: "factual", Confidence: 0.95},
		{TargetID: "c", Relationship: "topical", Confidence: 0.8},
	}
	out, dropped := applyHardCap(in, 5)
	if dropped != 0 {
		t.Errorf("under cap: dropped=%d, want 0", dropped)
	}
	if len(out) != 3 {
		t.Errorf("under cap: len=%d, want 3", len(out))
	}
	if out[0].TargetID != "a" || out[1].TargetID != "b" || out[2].TargetID != "c" {
		t.Errorf("under cap: order changed: %+v", out)
	}
}

func TestApplyHardCap_TrimsByConfidence(t *testing.T) {
	in := []Link{
		{TargetID: "low1", Relationship: "topical", Confidence: 0.71},
		{TargetID: "high1", Relationship: "topical", Confidence: 0.95},
		{TargetID: "mid1", Relationship: "topical", Confidence: 0.80},
		{TargetID: "low2", Relationship: "topical", Confidence: 0.72},
		{TargetID: "mid2", Relationship: "topical", Confidence: 0.88},
		{TargetID: "high2", Relationship: "topical", Confidence: 0.99},
		{TargetID: "low3", Relationship: "topical", Confidence: 0.75},
		{TargetID: "mid3", Relationship: "topical", Confidence: 0.91},
	}
	out, dropped := applyHardCap(in, 5)
	if dropped != 3 {
		t.Errorf("dropped=%d, want 3", dropped)
	}
	if len(out) != 5 {
		t.Fatalf("len=%d, want 5", len(out))
	}
	wantConfs := []float64{0.99, 0.95, 0.91, 0.88, 0.80}
	for i, l := range out {
		if l.Confidence != wantConfs[i] {
			t.Errorf("pos %d: conf=%v, want %v", i, l.Confidence, wantConfs[i])
		}
	}
}

func TestApplyHardCap_TypeDiversityWithinTier(t *testing.T) {
	// 6 Links all at conf=0.9; one of each non-topical type, three topical.
	// Diversity tie-break must pick one of each type before second topical.
	in := []Link{
		{TargetID: "t1", Relationship: "topical", Confidence: 0.9},
		{TargetID: "t2", Relationship: "topical", Confidence: 0.9},
		{TargetID: "t3", Relationship: "topical", Confidence: 0.9},
		{TargetID: "f1", Relationship: "factual", Confidence: 0.9},
		{TargetID: "c1", Relationship: "causal", Confidence: 0.9},
		{TargetID: "s1", Relationship: "supersedes", Confidence: 0.9},
	}
	out, dropped := applyHardCap(in, 5)
	if dropped != 1 {
		t.Errorf("dropped=%d, want 1", dropped)
	}
	if len(out) != 5 {
		t.Fatalf("len=%d, want 5", len(out))
	}
	typeCounts := map[string]int{}
	for _, l := range out {
		typeCounts[l.Relationship]++
	}
	for _, typ := range []string{"topical", "factual", "causal", "supersedes"} {
		if typeCounts[typ] < 1 {
			t.Errorf("type %q missing in cap output: %v", typ, typeCounts)
		}
	}
	if typeCounts["topical"] != 2 {
		t.Errorf("topical count=%d, want 2 (filling 5th slot)", typeCounts["topical"])
	}
}

func TestApplyHardCap_DiversityRespectsTierBoundary(t *testing.T) {
	// High-conf topicals must not be displaced by lower-conf factual just for
	// diversity sake. Tier 0.95: 4× topical → take all 4 (no other type at this conf).
	// Tier 0.80: 2× factual → 5th slot goes to one factual (any).
	in := []Link{
		{TargetID: "t1", Relationship: "topical", Confidence: 0.95},
		{TargetID: "t2", Relationship: "topical", Confidence: 0.95},
		{TargetID: "t3", Relationship: "topical", Confidence: 0.95},
		{TargetID: "t4", Relationship: "topical", Confidence: 0.95},
		{TargetID: "f1", Relationship: "factual", Confidence: 0.80},
		{TargetID: "f2", Relationship: "factual", Confidence: 0.80},
	}
	out, dropped := applyHardCap(in, 5)
	if dropped != 1 {
		t.Errorf("dropped=%d, want 1", dropped)
	}
	if len(out) != 5 {
		t.Fatalf("len=%d, want 5", len(out))
	}
	// First 4 are 0.95 topicals; 5th is one of the 0.80 factuals.
	for i := 0; i < 4; i++ {
		if out[i].Confidence != 0.95 || out[i].Relationship != "topical" {
			t.Errorf("pos %d: %+v, want 0.95 topical", i, out[i])
		}
	}
	if out[4].Confidence != 0.80 || out[4].Relationship != "factual" {
		t.Errorf("pos 4: %+v, want 0.80 factual", out[4])
	}
}

func TestApplyHardCap_Deterministic(t *testing.T) {
	in := []Link{
		{TargetID: "a", Relationship: "topical", Confidence: 0.9},
		{TargetID: "b", Relationship: "topical", Confidence: 0.9},
		{TargetID: "c", Relationship: "topical", Confidence: 0.9},
		{TargetID: "d", Relationship: "factual", Confidence: 0.9},
		{TargetID: "e", Relationship: "causal", Confidence: 0.9},
		{TargetID: "f", Relationship: "supersedes", Confidence: 0.9},
	}
	first, _ := applyHardCap(append([]Link(nil), in...), 5)
	for i := 0; i < 50; i++ {
		out, _ := applyHardCap(append([]Link(nil), in...), 5)
		if len(out) != len(first) {
			t.Fatalf("iter %d: len differs: %d vs %d", i, len(out), len(first))
		}
		for j := range out {
			if out[j].TargetID != first[j].TargetID {
				t.Errorf("iter %d pos %d: %s vs %s", i, j, out[j].TargetID, first[j].TargetID)
			}
		}
	}
}

// --- Structural Filter Tests (S25 Welle 8 — V5/V6/V8/V9/V10) ---.

func TestAcceptScopeAndArchived(t *testing.T) {
	cases := []struct {
		name           string
		targetScope    string
		sourceScope    string
		targetArchived bool
		wantAccepted   bool
		wantReason     string
	}{
		{"same scope alive", "private", "private", false, true, ""},
		{"same scope archived", "private", "private", true, false, "target archived"},
		{"cross scope alive", "shared", "private", false, false, "cross-scope"},
		{"cross scope archived", "shared", "private", true, false, "target archived"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := acceptScopeAndArchived(c.targetScope, c.sourceScope, c.targetArchived)
			if ok != c.wantAccepted || reason != c.wantReason {
				t.Errorf("got (%v, %q), want (%v, %q)", ok, reason, c.wantAccepted, c.wantReason)
			}
		})
	}
}

func TestCoerceCategoryFactual(t *testing.T) {
	cases := []struct {
		name   string
		rel    string
		srcCat string
		tgtCat string
		want   string
	}{
		{"factual same cat → topical", "factual", "decisions", "decisions", "topical"},
		{"factual different cat → unchanged", "factual", "decisions", "projects", "factual"},
		{"causal same cat → unchanged", "causal", "decisions", "decisions", "causal"},
		{"topical same cat → unchanged", "topical", "decisions", "decisions", "topical"},
		{"supersedes same cat → unchanged", "supersedes", "decisions", "decisions", "supersedes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := coerceCategoryFactual(c.rel, c.srcCat, c.tgtCat)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestAcceptSupersedes(t *testing.T) {
	// Welle 46 (2026-05-22): direction inverted — source MUST be newer than
	// target. Variable names below follow created_at semantics (older = earlier
	// created_at, newer = later created_at).
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	same := older
	cases := []struct {
		name         string
		srcCat       string
		tgtCat       string
		srcCreated   time.Time
		tgtCreated   time.Time
		titleSim     float64
		wantAccepted bool
		wantReason   string
	}{
		{"all conditions met (src newer)", "decisions", "decisions", newer, older, 0.5, true, ""},
		{"different category", "decisions", "projects", newer, older, 0.5, false, "different category"},
		{"source not newer", "decisions", "decisions", older, newer, 0.5, false, "source not newer"},
		{"source same created_at", "decisions", "decisions", same, same, 0.5, false, "source not newer"},
		{"low title similarity", "decisions", "decisions", newer, older, 0.20, false, "low title similarity"},
		{"exact threshold", "decisions", "decisions", newer, older, 0.25, true, ""},
		{"just below threshold", "decisions", "decisions", newer, older, 0.249, false, "low title similarity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := acceptSupersedes(c.srcCat, c.tgtCat, c.srcCreated, c.tgtCreated, c.titleSim)
			if ok != c.wantAccepted || reason != c.wantReason {
				t.Errorf("got (%v, %q), want (%v, %q)", ok, reason, c.wantAccepted, c.wantReason)
			}
		})
	}
}

func TestAcceptCausal(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	same := older
	cases := []struct {
		name         string
		srcCreated   time.Time
		tgtCreated   time.Time
		wantAccepted bool
		wantReason   string
	}{
		{"src older than tgt", older, newer, true, ""},
		{"src equal to tgt", same, same, false, "source not older"},
		{"src newer than tgt", newer, older, false, "source not older"},
		// Even 1 nanosecond before is acceptable — semantic check is delegated to V6 prompt.
		{"src 1ns older", older, older.Add(1), true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := acceptCausal(c.srcCreated, c.tgtCreated)
			if ok != c.wantAccepted || reason != c.wantReason {
				t.Errorf("got (%v, %q), want (%v, %q)", ok, reason, c.wantAccepted, c.wantReason)
			}
		})
	}
}

// --- applyLinkFloor Tests (dream.link_floor_confidence, PR #12 follow-up) ---.

func TestApplyLinkFloor_LiftsFlooredToConfiguredValue(t *testing.T) {
	// Default operator floor 0.9: type-only links become retrieval-live
	// (above the RRF graph gate 0.75); model-emitted confidences untouched.
	links := []Link{
		{TargetID: "a", Relationship: "topical", Confidence: 0.7, Floored: true},
		{TargetID: "b", Relationship: "factual", Confidence: 0.85}, // model-emitted
	}
	out := applyLinkFloor(links, 0.9)
	if out[0].Confidence != 0.9 {
		t.Errorf("floored link must lift to configured 0.9, got %.2f", out[0].Confidence)
	}
	if out[1].Confidence != 0.85 {
		t.Errorf("model-emitted confidence must stay untouched, got %.2f", out[1].Confidence)
	}
}

func TestApplyLinkFloor_PerTypeGateStaysLowerBound(t *testing.T) {
	// A configured floor below a type's minRawConfidence write gate is lifted
	// to that gate — otherwise the link would be a silent write-path no-op.
	links := []Link{
		{TargetID: "a", Relationship: "recurrent", Confidence: 0.8, Floored: true},
		{TargetID: "b", Relationship: "topical", Confidence: 0.7, Floored: true},
	}
	out := applyLinkFloor(links, 0.7)
	if out[0].Confidence != 0.8 {
		t.Errorf("recurrent floored link must stay at its 0.8 gate under floor 0.7, got %.2f", out[0].Confidence)
	}
	if out[1].Confidence != 0.7 {
		t.Errorf("topical floored link must take configured 0.7, got %.2f", out[1].Confidence)
	}
}

func TestApplyLinkFloor_ZeroKeepsParserFloors(t *testing.T) {
	// floor <= 0 (unwired routers, tests) keeps the parser's per-type floors.
	links := []Link{{TargetID: "a", Relationship: "topical", Confidence: 0.7, Floored: true}}
	out := applyLinkFloor(links, 0)
	if out[0].Confidence != 0.7 {
		t.Errorf("zero floor must keep parser value, got %.2f", out[0].Confidence)
	}
}

func TestApplyLinkFloor_ClampsAboveOne(t *testing.T) {
	links := []Link{{TargetID: "a", Relationship: "causal", Confidence: 0.7, Floored: true}}
	out := applyLinkFloor(links, 1.5)
	if out[0].Confidence != 1.0 {
		t.Errorf("floor must clamp to 1.0, got %.2f", out[0].Confidence)
	}
}

// TestParseLinks_StringMap_FlooredMarking pins the parser-side contract the
// operator floor depends on: absent-confidence entries carry Floored=true,
// model-emitted confidences carry Floored=false.
func TestParseLinks_StringMap_FlooredMarking(t *testing.T) {
	links, _, err := parseLinks(`{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"topical"}`)
	if err != nil || len(links) != 1 || !links[0].Floored {
		t.Fatalf("string-map link must be marked Floored, err=%v links=%+v", err, links)
	}
	links, _, err = parseLinks(`[{"target_id":"019fb992-ea5a-7ef8-aa5c-ed7db94699ca","type":"topical","confidence":0.9}]`)
	if err != nil || len(links) != 1 || links[0].Floored {
		t.Fatalf("model-emitted confidence must not be Floored, err=%v links=%+v", err, links)
	}
	links, _, err = parseLinks(`[{"target_id":"019fb992-ea5a-7ef8-aa5c-ed7db94699ca","type":"topical"}]`)
	if err != nil || len(links) != 1 || !links[0].Floored {
		t.Fatalf("absent-confidence array entry must be Floored, err=%v links=%+v", err, links)
	}
}

// TestParseLinks_StringMap_SurvivesFilterPipeline runs the string-map form
// through the ACTUAL downstream gate (filterValidCandidates) instead of only
// asserting parser output — the review found every string-map test stopped at
// the parser while the real failure modes (uuid gate, per-type confidence
// floor) live one call later.
func TestParseLinks_StringMap_SurvivesFilterPipeline(t *testing.T) {
	raw := `{"019fb992-ea5a-7ef8-aa5c-ed7db94699ca":"topical","019fb98f-cad3-7bbe-b3be-ccf74d5ba05f":"recurrent"}`
	links, _, err := parseLinks(raw)
	if err != nil || len(links) != 2 {
		t.Fatalf("err=%v links=%+v", err, links)
	}
	candidateIDs := map[string]bool{
		"019fb992-ea5a-7ef8-aa5c-ed7db94699ca": true,
		"019fb98f-cad3-7bbe-b3be-ccf74d5ba05f": true,
	}
	valid := filterValidCandidates(links, candidateIDs)
	// BOTH must survive: the topical link at floor 0.7 and the recurrent link
	// at its per-type floor 0.8 (a hardcoded 0.7 would silently lose it here).
	if len(valid) != 2 {
		t.Fatalf("string-map links must clear filterValidCandidates, got %d of 2: %+v", len(valid), valid)
	}
}

// --- enforceSupersedesDirection Tests ---.

// TestEnforceSupersedesDirection_DowngradesInverted verifies that supersedes
// links with inverted direction (candidate UpdatedAt >= source CreatedAt)
// are downgraded to 'topical' rather than dropped. Welle 46 Convention-Switch
// (2026-05-22): the LLM-emitted relation between two related blocks is
// preserved as an edge, but the directional spec-claim is discarded.
func TestEnforceSupersedesDirection_DowngradesInverted(t *testing.T) {
	tOlder := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	tNewer := time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)

	srcCreated := tNewer
	candidates := []BlockInfo{
		{ID: "tgt-correct", UpdatedAt: tOlder}, // source newer — keep supersedes
		{ID: "tgt-invert", UpdatedAt: tNewer},  // source NOT newer — downgrade to topical
		{ID: "tgt-same", UpdatedAt: tNewer},    // source equal — downgrade to topical
	}

	links := []Link{
		{TargetID: "tgt-correct", Relationship: "supersedes", Confidence: 0.9},
		{TargetID: "tgt-invert", Relationship: "supersedes", Confidence: 0.9},
		{TargetID: "tgt-same", Relationship: "supersedes", Confidence: 0.9},
		{TargetID: "tgt-correct", Relationship: "topical", Confidence: 0.8},
		{TargetID: "tgt-invert", Relationship: "causal", Confidence: 0.8},
	}

	out, downgraded := enforceSupersedesDirection(links, srcCreated, candidates)

	// All 5 input links must survive — none are dropped.
	if len(out) != 5 {
		t.Fatalf("got %d kept, want 5 (downgrade, never drop)", len(out))
	}
	// The downgrade count feeds the supersedes_direction_downgraded telemetry
	// (previously dead: the caller measured a len-diff that never changes).
	if downgraded != 2 {
		t.Errorf("downgrade count: got %d, want 2", downgraded)
	}

	// Correctly oriented supersedes stays as supersedes.
	if rel := findRel(out, "tgt-correct", 0.9); rel != "supersedes" {
		t.Errorf("correctly oriented supersedes: got %q, want supersedes", rel)
	}
	// Inverted supersedes downgraded to topical.
	if rel := findRel(out, "tgt-invert", 0.9); rel != "topical" {
		t.Errorf("inverted supersedes: got %q, want topical (downgrade)", rel)
	}
	// Equal-timestamp supersedes downgraded to topical (must be STRICTLY newer).
	if rel := findRel(out, "tgt-same", 0.9); rel != "topical" {
		t.Errorf("equal-timestamp supersedes: got %q, want topical (downgrade)", rel)
	}
	// Pre-existing topical/causal links untouched.
	if rel := findRel(out, "tgt-correct", 0.8); rel != "topical" {
		t.Errorf("pre-existing topical changed: got %q", rel)
	}
	if rel := findRel(out, "tgt-invert", 0.8); rel != "causal" {
		t.Errorf("pre-existing causal touched by direction filter: got %q", rel)
	}
}

// TestEnforceSupersedesDirection_KeepsCorrect verifies correctly oriented
// supersedes-links (source strictly newer than target) are kept verbatim.
func TestEnforceSupersedesDirection_KeepsCorrect(t *testing.T) {
	tOlder := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	tNewer := time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)

	srcCreated := tNewer
	candidates := []BlockInfo{
		{ID: "tgt", UpdatedAt: tOlder},
	}
	links := []Link{
		{TargetID: "tgt", Relationship: "supersedes", Confidence: 0.92},
	}

	out, downgraded := enforceSupersedesDirection(links, srcCreated, candidates)
	if len(out) != 1 {
		t.Fatalf("got %d, want 1", len(out))
	}
	if downgraded != 0 {
		t.Errorf("downgrade count: got %d, want 0", downgraded)
	}
	if out[0].Relationship != "supersedes" {
		t.Errorf("relationship changed: got %q, want supersedes", out[0].Relationship)
	}
	if out[0].Confidence != 0.92 {
		t.Errorf("confidence changed: got %f, want 0.92", out[0].Confidence)
	}
}

// TestEnforceSupersedesDirection_UnknownTargetPassThrough verifies that a
// supersedes-link whose target is not in the candidates set passes through
// the direction filter (filterValidCandidates will drop it downstream).
func TestEnforceSupersedesDirection_UnknownTargetPassThrough(t *testing.T) {
	srcCreated := time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)
	candidates := []BlockInfo{
		{ID: "tgt-known", UpdatedAt: srcCreated.Add(-time.Hour)},
	}
	links := []Link{
		{TargetID: "tgt-unknown", Relationship: "supersedes", Confidence: 0.9},
	}
	out, _ := enforceSupersedesDirection(links, srcCreated, candidates)
	if len(out) != 1 {
		t.Fatalf("unknown-target supersedes was dropped by direction filter (must defer to filterValidCandidates)")
	}
	if out[0].Relationship != "supersedes" {
		t.Errorf("unknown-target supersedes was relabeled: got %q", out[0].Relationship)
	}
}

// TestEnforceSupersedesDirection_EmptyInput verifies the filter handles nil
// and empty inputs without panicking.
func TestEnforceSupersedesDirection_EmptyInput(t *testing.T) {
	out, _ := enforceSupersedesDirection(nil, time.Now(), nil)
	if len(out) != 0 {
		t.Errorf("got %v, want empty/nil", out)
	}
	out, _ = enforceSupersedesDirection([]Link{}, time.Now(), []BlockInfo{})
	if len(out) != 0 {
		t.Errorf("got %d links, want 0", len(out))
	}
}

// findRel returns the relationship label of the first link in links matching
// (target, confidence). Returns "" if no match.
func findRel(links []Link, target string, conf float64) string {
	for _, l := range links {
		if l.TargetID == target && l.Confidence == conf {
			return l.Relationship
		}
	}
	return ""
}

// --- Link validation Tests ---.

func TestValidRelationships(t *testing.T) {
	valid := []string{"topical", "factual", "causal", "supersedes"}
	for _, r := range valid {
		if !validRelationships[r] {
			t.Errorf("expected %q to be valid", r)
		}
	}

	invalid := []string{"similar", "related", "unknown", ""}
	for _, r := range invalid {
		if validRelationships[r] {
			t.Errorf("expected %q to be invalid", r)
		}
	}
}

// --- truncate Tests ---.

func TestTruncate_ShortString(t *testing.T) {
	s := "short"
	if truncate(s, 100) != s {
		t.Error("short string should not be truncated")
	}
}

func TestTruncate_LongString(t *testing.T) {
	s := "this is a longer string that needs to be cut at a word boundary"
	result := truncate(s, 30)
	if len(result) > 30 {
		t.Errorf("truncated string too long: %d > 30", len(result))
	}
}

// escapeXml tests are in llm/llm_test.go (canonical implementation).

// --- buildEvalPrompt Tests ---.

func TestBuildEvalPrompt_ContainsSourceAndCandidates(t *testing.T) {
	source := BlockInfo{ID: "src-1", Title: "Source Block", Category: "test", Content: "source content"}
	candidates := []BlockInfo{
		{ID: "c-1", Title: "Candidate 1", Category: "test", Content: "candidate content"},
	}
	_, prompt := buildEvalPrompt(source, candidates)

	if !contains(prompt, "src-1") {
		t.Error("prompt should contain source ID")
	}
	if !contains(prompt, "c-1") {
		t.Error("prompt should contain candidate ID")
	}
	if !contains(prompt, "<source>") || !contains(prompt, "</source>") {
		t.Error("prompt should have source XML tags")
	}
	if !contains(prompt, "<candidates>") || !contains(prompt, "</candidates>") {
		t.Error("prompt should have candidates XML tags")
	}
}

func TestBuildEvalPrompt_EscapesContent(t *testing.T) {
	source := BlockInfo{ID: "s", Title: "<injected>", Category: "test", Content: "a & b"}
	_, prompt := buildEvalPrompt(source, nil)

	if contains(prompt, "<injected>") {
		t.Error("prompt should escape title XML chars")
	}
	if contains(prompt, "a & b") && !contains(prompt, "a &amp; b") {
		t.Error("prompt should escape content ampersand")
	}
}

// --- JSON round-trip test ---.

func TestLink_JSONRoundTrip(t *testing.T) {
	original := Link{TargetID: "abc", Relationship: "causal", Confidence: 0.87}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Link
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.TargetID != original.TargetID || decoded.Relationship != original.Relationship || decoded.Confidence != original.Confidence {
		t.Errorf("round-trip mismatch: %+v vs %+v", original, decoded)
	}
}

// --- Helpers ---.

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && containsStr(s, substr)))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
