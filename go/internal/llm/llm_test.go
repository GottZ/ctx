package llm

import (
	"strings"
	"testing"
)

// testSettings mirrors the config-registry defaults (query.score_threshold
// 0.001 / query.confident_threshold 0.008 / prompt v5.2) — the values the
// former package vars carried when env was unset. Shared by llm_test.go and
// synthesize_test.go (same package). Tests that prove the parametrization
// itself build their own divergent settings inline.
var testSettings = SynthesisSettings{
	ScoreThreshold:     0.001,
	ConfidentThreshold: 0.008,
	PromptVersion:      PromptVersionV52,
}

// --- EscapeXml ---.

func TestEscapeXml(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ampersand", "AT&T", "AT&amp;T"},
		{"less than", "a < b", "a &lt; b"},
		{"greater than", "a > b", "a &gt; b"},
		{"double quote", `say "hello"`, "say &quot;hello&quot;"},
		{"single quote", "it's", "it&apos;s"},
		{"all entities combined", `<b>"Tom & Jerry's"</b>`, "&lt;b&gt;&quot;Tom &amp; Jerry&apos;s&quot;&lt;/b&gt;"},
		{"no special chars", "plain text 123", "plain text 123"},
		{"empty string", "", ""},
		{"ampersand not double escaped", "&amp;", "&amp;amp;"},
		{"xml tag injection", `<script>alert("xss")</script>`, "&lt;script&gt;alert(&quot;xss&quot;)&lt;/script&gt;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EscapeXml(tt.in)
			if got != tt.want {
				t.Errorf("EscapeXml(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- ClassifyConfidence ---.

func TestClassifyConfidence(t *testing.T) {
	tests := []struct {
		name     string
		maxScore float64
		want     string
	}{
		{"well above confident", 0.05, ConfidenceConfident},
		{"exactly confident threshold", testSettings.ConfidentThreshold, ConfidenceConfident},
		{"just below confident", 0.0079, ConfidenceLow},
		{"mid low confidence", 0.006, ConfidenceLow},
		{"exactly score threshold", testSettings.ScoreThreshold, ConfidenceLow},
		{"just below score threshold", 0.0009, ConfidenceNoRelevant},
		{"zero score", 0.0, ConfidenceNoRelevant},
		{"high score", 1.0, ConfidenceConfident},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyConfidence(tt.maxScore, testSettings)
			if got != tt.want {
				t.Errorf("ClassifyConfidence(%f) = %q, want %q", tt.maxScore, got, tt.want)
			}
		})
	}
}

// --- FilterByScore ---.

func TestFilterByScore(t *testing.T) {
	tests := []struct {
		name      string
		sources   []Source
		wantLen   int
		wantMax   float64
	}{
		{
			"all above threshold",
			[]Source{
				{ID: "1", Score: 0.01},
				{ID: "2", Score: 0.02},
				{ID: "3", Score: 0.03},
			},
			3, 0.03,
		},
		{
			"mixed scores",
			[]Source{
				{ID: "1", Score: 0.0005},
				{ID: "2", Score: 0.01},
				{ID: "3", Score: 0.0008},
			},
			1, 0.01,
		},
		{
			"all below threshold",
			[]Source{
				{ID: "1", Score: 0.0001},
				{ID: "2", Score: 0.0008},
			},
			0, 0.0,
		},
		{
			"empty input",
			[]Source{},
			0, 0.0,
		},
		{
			"nil input",
			nil,
			0, 0.0,
		},
		{
			"exactly at threshold",
			[]Source{
				{ID: "1", Score: testSettings.ScoreThreshold},
				{ID: "2", Score: testSettings.ScoreThreshold - 0.0001},
			},
			1, testSettings.ScoreThreshold,
		},
		{
			"single source above",
			[]Source{{ID: "1", Score: 0.05}},
			1, 0.05,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered, maxScore := FilterByScore(tt.sources, testSettings)
			if len(filtered) != tt.wantLen {
				t.Errorf("FilterByScore() returned %d sources, want %d", len(filtered), tt.wantLen)
			}
			if maxScore != tt.wantMax {
				t.Errorf("FilterByScore() maxScore = %f, want %f", maxScore, tt.wantMax)
			}
		})
	}
}

// --- LostInMiddleReorder ---.

func TestLostInMiddleReorder(t *testing.T) {
	mkSources := func(ids ...string) []Source {
		s := make([]Source, len(ids))
		for i, id := range ids {
			s[i] = Source{ID: id}
		}
		return s
	}
	getIDs := func(sources []Source) []string {
		ids := make([]string, len(sources))
		for i, s := range sources {
			ids[i] = s.ID
		}
		return ids
	}
	sliceEqual := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	tests := []struct {
		name    string
		input   []Source
		wantIDs []string
	}{
		{"zero sources", mkSources(), []string{}},
		{"one source", mkSources("A"), []string{"A"}},
		{"two sources unchanged", mkSources("A", "B"), []string{"A", "B"}},
		{
			"three sources reordered",
			mkSources("A", "B", "C"),
			[]string{"A", "C", "B"}, // best, third, second-best
		},
		{
			"five sources reordered",
			mkSources("A", "B", "C", "D", "E"),
			[]string{"A", "C", "D", "E", "B"}, // best, 3rd..5th, second-best
		},
		{
			"ten sources reordered",
			mkSources("1", "2", "3", "4", "5", "6", "7", "8", "9", "10"),
			[]string{"1", "3", "4", "5", "6", "7", "8", "9", "10", "2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LostInMiddleReorder(tt.input)
			gotIDs := getIDs(got)
			if !sliceEqual(gotIDs, tt.wantIDs) {
				t.Errorf("LostInMiddleReorder() IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}

	// Verify original slice is not mutated.
	t.Run("does not mutate input", func(t *testing.T) {
		input := mkSources("A", "B", "C", "D")
		inputCopy := make([]Source, len(input))
		copy(inputCopy, input)
		_ = LostInMiddleReorder(input)
		for i := range input {
			if input[i].ID != inputCopy[i].ID {
				t.Errorf("input[%d] mutated: got %q, want %q", i, input[i].ID, inputCopy[i].ID)
			}
		}
	})
}

// --- FormatAnswer ---.

func TestFormatAnswer(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"normal text", "The service runs on port 443 [1].", "The service runs on port 443 [1]."},
		{"exact no relevant", "NO_RELEVANT_SOURCES", NoRelevantReplacement},
		{"no relevant with newline suffix", "NO_RELEVANT_SOURCES\nsome extra", NoRelevantReplacement},
		{"trailing no relevant marker", "Answer here. NO_RELEVANT_SOURCES", "Answer here."},
		{
			"double trailing marker",
			"Answer here. NO_RELEVANT_SOURCES NO_RELEVANT_SOURCES",
			"Answer here.",
		},
		{"empty string", "", "No response from LLM"},
		{"whitespace only", "   \n\t  ", "No response from LLM"},
		{"normal with leading whitespace", "  clean answer  ", "clean answer"},
		{
			"only marker after stripping becomes replacement",
			"  NO_RELEVANT_SOURCES  ",
			NoRelevantReplacement,
		},
		{"text before marker is preserved", "Port 443. NO_RELEVANT_SOURCES", "Port 443."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatAnswer(tt.raw)
			if got != tt.want {
				t.Errorf("FormatAnswer(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// --- BuildPrompt ---.

func TestBuildPrompt(t *testing.T) {
	t.Run("system prompt contains v5.2 text", func(t *testing.T) {
		sys, _ := BuildPrompt("test query", nil, nil, testSettings)
		if !strings.Contains(sys, "fact extraction engine") {
			t.Error("system prompt missing 'fact extraction engine'")
		}
		if !strings.Contains(sys, "NO_RELEVANT_SOURCES") {
			t.Error("system prompt missing 'NO_RELEVANT_SOURCES'")
		}
		if !strings.Contains(sys, "<constraints>") {
			t.Error("system prompt missing '<constraints>'")
		}
	})

	t.Run("user prompt has XML sources", func(t *testing.T) {
		sources := []Source{
			{ID: "abc", Title: "My Title", Category: "infra", Content: "Port 443", Score: 0.02, AgeDays: 5},
		}
		_, user := BuildPrompt("what port?", sources, nil, testSettings)
		if !strings.Contains(user, "<question>what port?</question>") {
			t.Errorf("user prompt missing question tag, got: %s", user)
		}
		if !strings.Contains(user, `<source id="1"`) {
			t.Error("user prompt missing source tag with id=1")
		}
		if !strings.Contains(user, `title="My Title"`) {
			t.Error("user prompt missing title attribute")
		}
		if !strings.Contains(user, `category="infra"`) {
			t.Error("user prompt missing category attribute")
		}
		if !strings.Contains(user, "Port 443") {
			t.Error("user prompt missing content")
		}
		if !strings.Contains(user, "</sources>") {
			t.Error("user prompt missing closing sources tag")
		}
	})

	t.Run("temporal dates injected", func(t *testing.T) {
		dates := []TemporalDate{
			{Ref: "gestern", Date: "2026-03-27"},
		}
		sys, _ := BuildPrompt("was war gestern?", nil, dates, testSettings)
		if !strings.Contains(sys, "Current date:") {
			t.Error("system prompt missing 'Current date:'")
		}
		if !strings.Contains(sys, "gestern = 2026-03-27") {
			t.Error("system prompt missing temporal date mapping")
		}
		if !strings.Contains(sys, "<context>") {
			t.Error("system prompt missing context tag")
		}
	})

	t.Run("temporal dates with range", func(t *testing.T) {
		end := "2026-03-28"
		dates := []TemporalDate{
			{Ref: "diese Woche", Date: "2026-03-23", End: &end},
		}
		sys, _ := BuildPrompt("was war diese Woche?", nil, dates, testSettings)
		if !strings.Contains(sys, "2026-03-23 to 2026-03-28") {
			t.Error("system prompt missing date range with 'to'")
		}
	})

	t.Run("no temporal dates means no context block", func(t *testing.T) {
		sys, _ := BuildPrompt("simple query", nil, nil, testSettings)
		if strings.Contains(sys, "<context>") {
			t.Error("system prompt should not contain context block without temporal dates")
		}
	})

	t.Run("XML escaping in query and sources", func(t *testing.T) {
		sources := []Source{
			{ID: "1", Title: "Tom & Jerry's <Show>", Category: "media", Content: "A < B & C > D", Score: 0.01, AgeDays: 1},
		}
		_, user := BuildPrompt("who & what?", sources, nil, testSettings)
		if !strings.Contains(user, "who &amp; what?") {
			t.Error("query not XML-escaped")
		}
		if !strings.Contains(user, "Tom &amp; Jerry&apos;s &lt;Show&gt;") {
			t.Error("title not XML-escaped")
		}
		if !strings.Contains(user, "A &lt; B &amp; C &gt; D") {
			t.Error("content not XML-escaped")
		}
	})

	t.Run("content truncated at MaxBlockChars", func(t *testing.T) {
		longContent := strings.Repeat("x", MaxBlockChars+500)
		sources := []Source{
			{ID: "1", Title: "Long", Category: "test", Content: longContent, Score: 0.01, AgeDays: 0},
		}
		_, user := BuildPrompt("q", sources, nil, testSettings)
		if !strings.Contains(user, "[... truncated]") {
			t.Error("long content not truncated")
		}
	})

	t.Run("multiple sources get sequential ids", func(t *testing.T) {
		sources := []Source{
			{ID: "a", Title: "First", Category: "c", Content: "one", Score: 0.01},
			{ID: "b", Title: "Second", Category: "c", Content: "two", Score: 0.02},
			{ID: "c", Title: "Third", Category: "c", Content: "three", Score: 0.03},
		}
		_, user := BuildPrompt("q", sources, nil, testSettings)
		if !strings.Contains(user, `<source id="1"`) {
			t.Error("missing source id=1")
		}
		if !strings.Contains(user, `<source id="2"`) {
			t.Error("missing source id=2")
		}
		if !strings.Contains(user, `<source id="3"`) {
			t.Error("missing source id=3")
		}
	})
}

// --- DetectGerman ---.

func TestDetectGerman(t *testing.T) {
	tests := []struct {
		name string
		query string
		want  bool
	}{
		{"umlaut ae", "Wie groß ist die Datenbank?", true},
		{"umlaut oe", "Öffne die Tür", true},
		{"umlaut ue", "Grüße aus Berlin", true},
		{"sharp s", "Straße", true},
		{"upper umlaut", "Ärger", true},
		{"two stopwords", "die ist gut", true},
		{"tech term pfad", "Welcher pfad ist korrekt?", true},
		{"tech term datenbank", "datenbank schema", true},
		{"tech term konfiguration", "konfiguration pruefen", true},
		{"english only", "What port does the service use?", false},
		{"english technical", "PostgreSQL runs on port 5432 with pgvector", false},
		{"single german stopword insufficient", "die hard movie", false},
		{"empty string", "", false},
		{"numbers only", "12345 67890", false},
		{"mixed but enough stopwords", "ich bin der neue user", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectGerman(tt.query)
			if got != tt.want {
				t.Errorf("DetectGerman(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// --- validateTranslation ---.

func TestValidateTranslation(t *testing.T) {
	tests := []struct {
		name       string
		translated string
		original   string
		want       bool
	}{
		{"valid translation", "What is the database schema?", "Was ist das Datenbank-Schema?", true},
		{"empty translation", "", "Hallo Welt", false},
		{"too long translation", strings.Repeat("word ", 100), "kurz", false},
		{"exactly 3x length is rejected", "abcdef", "ab", false}, // len>=len*3
		{"just under 3x is valid", "abcde", "ab", true},         // len 5 < 6
		{"multiline rejected", "line one\nline two", "Zeile eins Zeile zwei", false},
		{
			"sql injection single quote stripped upstream",
			"SELECT FROM users", // after sql stripping, safe chars pass
			"SELECT FROM users original",
			true,
		},
		{
			"unsafe chars rejected",
			"hello; DROP TABLE--",
			"hallo; DROP TABLE--",
			false, // semicolon not in safePattern
		},
		{
			"curly braces rejected",
			"hello {world}",
			"hallo {welt}",
			false,
		},
		{
			"safe chars with hyphens and parens",
			"Context Store (multi-tenant, v2.0)",
			"Kontextspeicher (multi-tenant, v2.0)",
			true,
		},
		{
			"unicode rejected",
			"Schöne Übersetzung",
			"Schoene Uebersetzung",
			false, // ö and ü not in safePattern
		},
		{
			"question mark and exclamation allowed",
			"What is this!",
			"Was ist das!",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateTranslation(tt.translated, tt.original)
			if got != tt.want {
				t.Errorf("validateTranslation(%q, %q) = %v, want %v", tt.translated, tt.original, got, tt.want)
			}
		})
	}
}
