package rrf

import (
	"strings"
	"testing"
)

func TestBuildORQuery_SingleWord(t *testing.T) {
	got := BuildORQuery("pgvector", 20)
	if got != "pgvector" {
		t.Errorf("expected 'pgvector', got %q", got)
	}
}

func TestBuildORQuery_MultiWord(t *testing.T) {
	got := BuildORQuery("bilingual retrieval gap", 20)
	if got != "bilingual OR retrieval OR gap" {
		t.Errorf("expected 'bilingual OR retrieval OR gap', got %q", got)
	}
}

func TestBuildORQuery_Empty(t *testing.T) {
	got := BuildORQuery("", 20)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBuildORQuery_HyphenExpansion(t *testing.T) {
	got := BuildORQuery("SHA-256", 20)
	// Should contain the compound and both parts.
	if !strings.Contains(got, "sha-256") {
		t.Errorf("expected 'sha-256' in result, got %q", got)
	}
	if !strings.Contains(got, "sha") {
		t.Errorf("expected 'sha' in result, got %q", got)
	}
	if !strings.Contains(got, "256") {
		t.Errorf("expected '256' in result, got %q", got)
	}
}

func TestBuildORQuery_Dedup(t *testing.T) {
	got := BuildORQuery("model model model", 20)
	if got != "model" {
		t.Errorf("expected 'model', got %q", got)
	}
}

func TestBuildORQuery_TermCap(t *testing.T) {
	// 30 unique words, cap at 10.
	words := make([]string, 30)
	for i := range words {
		words[i] = strings.Repeat("x", 3) + string(rune('a'+i))
	}
	got := BuildORQuery(strings.Join(words, " "), 10)
	parts := strings.Split(got, " OR ")
	if len(parts) > 10 {
		t.Errorf("expected max 10 terms, got %d", len(parts))
	}
}

func TestBuildORQuery_ShortWordsRemoved(t *testing.T) {
	got := BuildORQuery("a b cd efg", 20)
	// "a" and "b" are < 2 chars, removed. "cd" is 2 chars, kept.
	if strings.Contains(got, " a ") || got == "a" {
		t.Errorf("single char 'a' should be removed, got %q", got)
	}
	if !strings.Contains(got, "cd") {
		t.Errorf("expected 'cd' in result, got %q", got)
	}
	if !strings.Contains(got, "efg") {
		t.Errorf("expected 'efg' in result, got %q", got)
	}
}

func TestBuildORQuery_StopwordsRemoved(t *testing.T) {
	got := BuildORQuery("What port does Ollama run on?", 20)
	// "what", "does", "run", "on" are stopwords; "port" and "ollama" remain.
	if strings.Contains(got, "what") {
		t.Errorf("stopword 'what' should be removed, got %q", got)
	}
	if strings.Contains(got, "run") {
		t.Errorf("stopword 'run' should be removed, got %q", got)
	}
	if !strings.Contains(got, "port") {
		t.Errorf("expected 'port' in result, got %q", got)
	}
	if !strings.Contains(got, "ollama") {
		t.Errorf("expected 'ollama' in result, got %q", got)
	}
}

func TestBuildORQuery_GermanStopwords(t *testing.T) {
	got := BuildORQuery("Wie funktioniert der Schreibschutz?", 20)
	if strings.Contains(got, "wie") {
		t.Errorf("German stopword 'wie' should be removed, got %q", got)
	}
	if strings.Contains(got, "der") {
		t.Errorf("German stopword 'der' should be removed, got %q", got)
	}
	if !strings.Contains(got, "funktioniert") {
		t.Errorf("expected 'funktioniert', got %q", got)
	}
	if !strings.Contains(got, "schreibschutz") {
		t.Errorf("expected 'schreibschutz', got %q", got)
	}
}

func TestBuildORQuery_Punctuation(t *testing.T) {
	got := BuildORQuery("hello, world! test.", 20)
	if strings.Contains(got, ",") || strings.Contains(got, "!") || strings.Contains(got, ".") {
		t.Errorf("expected punctuation stripped, got %q", got)
	}
	if got != "hello OR world OR test" {
		t.Errorf("expected 'hello OR world OR test', got %q", got)
	}
}

func TestBuildORQuery_DefaultMaxTerms(t *testing.T) {
	// maxTerms 0 should use default (20).
	words := make([]string, 30)
	for i := range words {
		words[i] = strings.Repeat("w", 3) + string(rune('a'+i))
	}
	got := BuildORQuery(strings.Join(words, " "), 0)
	parts := strings.Split(got, " OR ")
	if len(parts) > 20 {
		t.Errorf("expected max 20 terms with default, got %d", len(parts))
	}
}
