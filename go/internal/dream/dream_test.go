package dream

import (
	"encoding/json"
	"testing"
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
	links, err := parseLinks(raw)
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
	links, err := parseLinks("[]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expected 0 links, got %d", len(links))
	}
}

func TestParseLinks_EmptyString(t *testing.T) {
	links, err := parseLinks("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expected 0 links, got %d", len(links))
	}
}

func TestParseLinks_InvalidJSON(t *testing.T) {
	_, err := parseLinks("not json")
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
	links, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("expected 3 links, got %d", len(links))
	}
}

// --- parseLinks Object-Form Drift Tests (audit S25, 2026-05-03) ---.

func TestParseLinks_ObjectForm_FloatConfidence(t *testing.T) {
	raw := `{"019d-aaaa":{"type":"topical","confidence":0.9}}`
	links, err := parseLinks(raw)
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
	links, err := parseLinks(raw)
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
	links, err := parseLinks(raw)
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
	links, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("non-UUID keys must pass parser (downstream filters): %v", err)
	}
	if len(links) != 1 || links[0].TargetID != "not-a-uuid" {
		t.Fatalf("want passthrough, got %+v", links)
	}
}

func TestParseLinks_ObjectForm_UnknownStringConf(t *testing.T) {
	raw := `{"id":{"type":"topical","confidence":"vibes"}}`
	links, err := parseLinks(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("unparseable confidence must drop entry, got %+v", links)
	}
}

func TestParseLinks_EmptyObject(t *testing.T) {
	links, err := parseLinks("{}")
	if err != nil || len(links) != 0 {
		t.Fatalf("got %v err=%v", links, err)
	}
}

func TestParseLinks_ObjectForm_DeterministicOrder(t *testing.T) {
	raw := `{"c-3":{"type":"topical","confidence":0.7},"a-1":{"type":"topical","confidence":0.7},"b-2":{"type":"topical","confidence":0.7}}`
	for i := 0; i < 20; i++ {
		links, err := parseLinks(raw)
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
	links, err := parseLinks(raw)
	if err != nil || len(links) != 1 || links[0].TargetID != "id-1" {
		t.Fatalf("err=%v links=%+v", err, links)
	}
}

func TestParseLinks_CodeFenceObject(t *testing.T) {
	raw := "```\n{\"id\":{\"type\":\"causal\",\"confidence\":\"high\"}}\n```"
	links, err := parseLinks(raw)
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
