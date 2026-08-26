package goldset

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- Gate (b): the redaction sweep of the NEW generators. A generated query
// carrying a credential is DISCARDED, never carried on redacted (§4.5).

func TestAcceptGeneratedQueryDiscardsCredentials(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"Welcher Wert stand hinter Bearer sk_live_AbCdEf0123456789xyz am 2026-08-20?",
		"authorization: Bearer abcdefghijklmnop0123456789 - warum lief das am 2026-08-20 durch?",
	} {
		got, ok := AcceptGeneratedQuery(q, SliceSess)
		if ok {
			t.Errorf("credential query accepted: %q", q)
		}
		// The policy is DISCARD, not redact: nothing of the text survives.
		if got != "" {
			t.Errorf("discarded query returned text %q - a part-redacted query is not a query", got)
		}
	}
}

func TestAcceptGeneratedQueryKeepsWellFormedQuestions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		slice, query string
	}{
		{SliceSess, "Was wurde am 2026-08-20 an der Retrieval-Kette gearbeitet?"},
		{SliceMH, "Wie haengen die RRF-Gewichte mit der Trigramm-Kandidatenmenge zusammen?"},
		{SliceGlob, "Welche wiederkehrenden Themen gibt es rund um die Deploy-Doktrin?"},
		{SliceGlobKonstr, "Welche Bausteine gehoeren zum Thema Guard und Sensitivity?"},
	}
	for _, tc := range cases {
		got, ok := AcceptGeneratedQuery(tc.query, tc.slice)
		if !ok {
			t.Errorf("slice %s: well-formed question rejected: %q", tc.slice, tc.query)
		}
		if got != tc.query {
			t.Errorf("slice %s: query rewritten to %q", tc.slice, got)
		}
	}
}

// A G-SESS question that dropped the date is unanswerable: the window IS the
// question. It is rejected rather than repaired.
func TestAcceptGeneratedQueryRequiresSessionWindowLabel(t *testing.T) {
	t.Parallel()
	if _, ok := AcceptGeneratedQuery("Was wurde an der Retrieval-Kette gearbeitet?", SliceSess); ok {
		t.Fatal("G-SESS question without a date accepted")
	}
}

// --- Gate (c): non-circularity. No G-MH case may use a dream link below the
// empirically justified confidence floor (learning_dream_quality: 100 % correct
// at conf >= 0.7, 56 % overall).

func TestDreamLinkFloorIsSevenTenths(t *testing.T) {
	t.Parallel()
	if MinDreamConfidence != 0.7 {
		t.Fatalf("MinDreamConfidence = %v, want 0.7 - the floor is the gate", MinDreamConfidence)
	}
}

func TestFilterDreamLinksDropsBelowFloor(t *testing.T) {
	t.Parallel()
	mk := func(a, b string, conf float64) DreamLink {
		return DreamLink{
			Source:     Block{ID: a, Title: "T" + a, Content: "content of " + a},
			Target:     Block{ID: b, Title: "T" + b, Content: "content of " + b},
			Confidence: conf, Relationship: "relates_to",
		}
	}
	in := []DreamLink{
		mk("aaa", "bbb", 0.69),
		mk("ccc", "ddd", 0.70),
		mk("eee", "fff", 0.95),
	}
	got := FilterDreamLinks(in)
	if len(got) != 2 {
		t.Fatalf("kept %d links, want 2 (0.70 and 0.95)", len(got))
	}
	for _, l := range got {
		if l.Confidence < MinDreamConfidence {
			t.Errorf("link %s->%s with confidence %v passed the floor", l.Source.ID, l.Target.ID, l.Confidence)
		}
		if l.Source.ID == "aaa" || l.Target.ID == "bbb" {
			t.Errorf("the 0.69 link was drawn: %s->%s", l.Source.ID, l.Target.ID)
		}
	}
}

// Both directions of the same pair are one bridge, not two cases.
func TestFilterDreamLinksDeduplicatesPairs(t *testing.T) {
	t.Parallel()
	in := []DreamLink{
		{Source: Block{ID: "a"}, Target: Block{ID: "b"}, Confidence: 0.9},
		{Source: Block{ID: "b"}, Target: Block{ID: "a"}, Confidence: 0.8},
		{Source: Block{ID: "a"}, Target: Block{ID: "a"}, Confidence: 0.9},
	}
	got := FilterDreamLinks(in)
	if len(got) != 1 {
		t.Fatalf("kept %d links, want 1 (deduplicated pair, self-link dropped)", len(got))
	}
}

// --- Gate (e): provenance. A stamp whose generator is not on-prem aborts the
// build - the stamp is the artefact a later reader trusts (§5 B6).

func TestWriteStampRejectsExternalGenerator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, FileStamp)
	s := Stamp{Version: 1, Slices: map[string]SliceStamp{}}
	s.Generator = &Generator{
		Backend: "openrouter", Model: "gpt-x", Endpoint: "https://openrouter.ai/api/v1/chat/completions",
		Locality: "external", Trust: "no-credentials",
	}
	if err := WriteStamp(p, s); err == nil {
		t.Fatal("stamp with an external generator was written")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the rejected stamp reached disk")
	}
}

func TestWriteStampRejectsExternalSliceProfile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, FileStamp)
	s := Stamp{Version: 1, Slices: map[string]SliceStamp{
		SliceMH: {N: 3, File: FileMH, Profile: &SliceProfile{
			Construction: "x", Generator: &Generator{
				Backend: "vendor", Model: "m", Endpoint: "https://api.vendor.example/v1/chat/completions",
				Locality: "lan", // lies: the host is public
			},
		}},
	}}
	if err := WriteStamp(p, s); err == nil {
		t.Fatal("slice profile with a public endpoint was written")
	}
}

func TestWriteStampAcceptsOnPremGenerator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, FileStamp)
	s := Stamp{Version: 1, Slices: map[string]SliceStamp{}}
	s.Generator = &Generator{
		Backend: "spark-chat", Model: "qwen38-27b", Endpoint: "http://10.13.37.22:30000/v1/chat/completions",
		Locality: "lan", Trust: "full-trust",
	}
	if err := WriteStamp(p, s); err != nil {
		t.Fatalf("on-prem stamp rejected: %v", err)
	}
}

// --- Session windows: the window definition is part of the slice contract and
// is reproducible without a database.

func TestBuildSessionWindows(t *testing.T) {
	t.Parallel()
	day := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return d.UTC()
	}
	reports := []SessionReport{
		{Day: day("2026-08-18"), ID: "r1", Title: "Tagesbericht 2026-08-18"},
		{Day: day("2026-08-19"), ID: "r2", Title: "Tagesbericht 2026-08-19"},
		{Day: day("2026-08-20"), ID: "r3", Title: "Tagesbericht 2026-08-20"},
		{Day: day("2026-08-20"), ID: "r4", Title: "Tagesbericht 2026-08-20 (Nachtrag)"},
	}
	got := BuildSessionWindows(reports, []int{3})
	if len(got) != 4 {
		t.Fatalf("built %d windows, want 4 (3 day + 1 span)", len(got))
	}
	// Day windows come first, chronologically, one per calendar day.
	if got[0].Kind != WindowDay || got[0].Label != "2026-08-18" {
		t.Errorf("window 0 = %s/%s, want day/2026-08-18", got[0].Kind, got[0].Label)
	}
	if got[2].Label != "2026-08-20" || len(got[2].ReportIDs) != 2 {
		t.Errorf("window 2 = %s with %d reports, want 2026-08-20 with 2", got[2].Label, len(got[2].ReportIDs))
	}
	// The half-open window is [day, day+1).
	if !got[0].To.Equal(got[0].From.AddDate(0, 0, 1)) {
		t.Errorf("day window is not half-open: %v..%v", got[0].From, got[0].To)
	}
	span := got[3]
	if span.Kind != WindowSpan {
		t.Fatalf("window 3 kind = %s, want span", span.Kind)
	}
	if span.Label != "2026-08-18..2026-08-20" {
		t.Errorf("span label = %q", span.Label)
	}
	if len(span.ReportIDs) != 4 {
		t.Errorf("span carries %d report ids, want 4", len(span.ReportIDs))
	}
	if !span.From.Equal(day("2026-08-18")) || !span.To.Equal(day("2026-08-21")) {
		t.Errorf("span window = %v..%v, want 2026-08-18..2026-08-21", span.From, span.To)
	}
}

// --- The slice profile registry: every new slice declares its construction and
// its bias, and G-GLOB-KONSTR declares itself a floor check.

func TestSliceProfilesDeclareBiasAndRolloutRole(t *testing.T) {
	t.Parallel()
	for _, name := range []string{SliceSess, SliceMH, SliceGlob, SliceGlobKonstr} {
		p, ok := ProfileFor(name)
		if !ok {
			t.Fatalf("slice %s has no declared profile", name)
		}
		if p.Construction == "" || p.DeclaredBias == "" || p.GoldSource == "" {
			t.Errorf("slice %s: incomplete profile %+v", name, p)
		}
	}
	if p, _ := ProfileFor(SliceGlobKonstr); p.RolloutCriterion {
		t.Error("G-GLOB-KONSTR declares itself a rollout criterion - it is a floor check")
	}
	if p, _ := ProfileFor(SliceGlob); !p.RolloutCriterion {
		t.Error("G-GLOB is not declared a rollout criterion")
	}
}
