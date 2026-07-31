package dream

import (
	"strings"
	"testing"
)

// TestReportSurfaceByLanguage pins the ONE thing this setting must never get
// wrong: an unset dream.language leaves the daily report byte-identical to the
// pre-setting build. Title and tag together form the report's identity — the
// title is half the (category, title, scope) upsert key — so they are asserted
// as a unit, per language, including the regional-variant case that must NOT
// fall out of its language's branch (de-DE is German, not "some other tag").
func TestReportSurfaceByLanguage(t *testing.T) {
	const date = "2026-07-31"

	legacyTags := []string{"synthesis", "tagesbericht", "auto"}
	localizedTags := []string{"synthesis", "daily-report", "auto"}

	cases := []struct {
		name       string
		lang       string
		wantTitle  string
		wantTags   []string
		wantLegacy bool   // German legacy system prompt, byte-frozen
		wantInSys  string // marker the localized prompt must name
	}{
		{"unset is legacy", "", "Tagesbericht " + date, legacyTags, true, ""},
		{"de is legacy", "de", "Tagesbericht " + date, legacyTags, true, ""},
		{"de-DE is legacy (primary subtag decides)", "de-DE", "Tagesbericht " + date, legacyTags, true, ""},
		{"de-de normalized is legacy", "de-de", "Tagesbericht " + date, legacyTags, true, ""},
		{"padded case-variant is legacy", "  DE  ", "Tagesbericht " + date, legacyTags, true, ""},
		{"en localizes", "en", "Daily Report " + date, localizedTags, false, "English"},
		{"tr localizes", "tr", "Daily Report " + date, localizedTags, false, "Turkish"},
		{"ja localizes", "ja", "Daily Report " + date, localizedTags, false, "Japanese"},
		{"pt-BR localizes on its primary subtag", "pt-BR", "Daily Report " + date, localizedTags, false, "Portuguese"},
		// Unmapped but V14-shaped: the tag itself goes into the prompt. Safe
		// because config.Validate constrains it to [a-z0-9-] — no free text.
		{"unmapped tag passes through", "haw", "Daily Report " + date, localizedTags, false, "haw"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dailyReportTitleFor(c.lang, date); got != c.wantTitle {
				t.Errorf("title = %q, want %q", got, c.wantTitle)
			}
			gotTags := dailyReportTags(c.lang)
			if strings.Join(gotTags, ",") != strings.Join(c.wantTags, ",") {
				t.Errorf("tags = %v, want %v", gotTags, c.wantTags)
			}
			sys := dailySynthesisPromptFor(c.lang)
			if c.wantLegacy {
				if sys != dailySynthesisSystemPrompt {
					t.Errorf("legacy language must yield the frozen German prompt, got %q", sys)
				}
				return
			}
			if sys == dailySynthesisSystemPrompt {
				t.Fatalf("localized language %q must not yield the German prompt", c.lang)
			}
			if !strings.Contains(sys, c.wantInSys) {
				t.Errorf("prompt lacks %q:\n%s", c.wantInSys, sys)
			}
		})
	}
}

// TestLegacyPromptFrozen is the byte-level regression anchor for the default
// path: the German system prompt is what 66 days of tuning produced (see
// dailySynthesisOptions). A refactor may move it, not reword it.
func TestLegacyPromptFrozen(t *testing.T) {
	const want = `Erzeuge einen kompakten Tagesbericht (200-400 Worte) für ein Knowledge-Store-System. Schreibe als Fließtext in Deutsch. Zähle Schwerpunkte der letzten 24h auf, nenne neue Themen, betone Patterns oder Anomalien.`
	if dailySynthesisSystemPrompt != want {
		t.Fatalf("legacy prompt drifted:\ngot  %q\nwant %q", dailySynthesisSystemPrompt, want)
	}
	if got := dailySynthesisPromptFor(""); got != want {
		t.Fatalf("empty language must serve the legacy prompt, got %q", got)
	}
}

// TestReportLanguagePrimarySubtag pins the normalization the switch rides on.
func TestReportLanguagePrimarySubtag(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"  ":          "",
		"de":          "de",
		"DE":          "de",
		" de-DE ":     "de",
		"zh-Hant-TW":  "zh",
		"pt-BR":       "pt",
		"en":          "en",
		"haw-us-x-yz": "haw",
	}
	for in, want := range cases {
		if got := reportLanguage(in); got != want {
			t.Errorf("reportLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLangNameMapping pins the prompt-facing name table. The legacy pair
// (""/"de") is deliberately absent: those tags take the frozen German prompt
// and never reach langName — a case for them would be dead code that reads
// like a live branch.
func TestLangNameMapping(t *testing.T) {
	cases := map[string]string{
		"en": "English", "tr": "Turkish", "fr": "French", "es": "Spanish",
		"pt": "Portuguese", "ru": "Russian", "zh": "Chinese", "ja": "Japanese",
		"haw": "haw", // unmapped: passthrough
	}
	for in, want := range cases {
		if got := langName(in); got != want {
			t.Errorf("langName(%q) = %q, want %q", in, got, want)
		}
	}
}
