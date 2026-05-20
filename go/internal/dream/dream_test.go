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
	raw := `{"id":{"type":"topical","confidence":"vibes"}}`
	links, _, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("unparseable confidence must drop entry, got %+v", links)
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
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	same := older
	cases := []struct {
		name         string
		srcCat       string
		tgtCat       string
		srcUpdated   time.Time
		tgtUpdated   time.Time
		titleSim     float64
		wantAccepted bool
		wantReason   string
	}{
		{"all conditions met", "decisions", "decisions", older, newer, 0.5, true, ""},
		{"different category", "decisions", "projects", older, newer, 0.5, false, "different category"},
		{"source not older", "decisions", "decisions", newer, older, 0.5, false, "source not older"},
		{"source same updated_at", "decisions", "decisions", same, same, 0.5, false, "source not older"},
		{"low title similarity", "decisions", "decisions", older, newer, 0.20, false, "low title similarity"},
		{"exact threshold", "decisions", "decisions", older, newer, 0.25, true, ""},
		{"just below threshold", "decisions", "decisions", older, newer, 0.249, false, "low title similarity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := acceptSupersedes(c.srcCat, c.tgtCat, c.srcUpdated, c.tgtUpdated, c.titleSim)
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
	prompt := buildEvalPrompt(source, candidates)

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
	prompt := buildEvalPrompt(source, nil)

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
