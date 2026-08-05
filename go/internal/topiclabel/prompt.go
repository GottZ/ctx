package topiclabel

import (
	"strings"

	"github.com/GottZ/ctx/internal/promptguard"
)

// promptLanguage normalizes a dream.language value to its primary subtag
// ("de-DE" → "de"), the same reduction the daily-report surface applies.
//
// The KEY is shared with the report by decision E3-01 — one language knob per
// corpus, and a per-tenant language (parked backlog) inherits automatically.
// The prompt SURFACE is not shared: this package must not import
// internal/dream (the label pipeline is deliberately independent of the dream
// router), and the two prompts say different things anyway.
func promptLanguage(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexByte(lang, '-'); i >= 0 {
		lang = lang[:i]
	}
	return lang
}

// languageName maps a primary subtag to the English name the instruction uses.
// Unknown subtags pass through — naming the tag is still a better instruction
// than silently switching the model to English.
func languageName(primary string) string {
	switch primary {
	case "", "de":
		return "German"
	case "en":
		return "English"
	case "tr":
		return "Turkish"
	case "fr":
		return "French"
	case "es":
		return "Spanish"
	case "pt":
		return "Portuguese"
	case "ru":
		return "Russian"
	case "zh":
		return "Chinese"
	case "ja":
		return "Japanese"
	default:
		return primary
	}
}

// systemPromptFor builds the instruction plus the guard rule for one nonce.
//
// The empty language means German — the same "" = German convention
// dream.language already carries, so an untouched deployment keeps writing in
// the language its corpus is in.
func systemPromptFor(lang, nonce string) string {
	return "You name clusters of a knowledge base. Given the titles of the most central blocks of one cluster " +
		"plus its most frequent tags and categories, answer with a SHORT topical name of two to five words in " +
		languageName(promptLanguage(lang)) + ".\n\n" +
		"Rules: name the common SUBJECT, not one of the documents. No sentence, no punctuation at the end, no " +
		"quotes, at most 120 characters. Never copy an identifier, a path, a host name or a key from the input.\n\n" +
		"Answer with JSON and nothing else: {\"label\": \"...\"}\n\n" +
		promptguard.Rule(nonce)
}

// promptCore is everything the model sees about one topic.
type promptCore struct {
	Titles     []string
	Tags       []string
	Categories []string
}

// buildUser assembles the user message: the task line, the separator, and then
// EXACTLY ONE guarded block carrying every byte of corpus text — titles, tags
// and categories alike (the classify doctrine, llm/classify.go). A separator
// reproduced inside a title is then data, not structure.
//
// Titles only, never content (design/01 §5.4): the sensitivity fold is the same
// either way, but the transferred volume differs by orders of magnitude, a
// cluster name is an abstraction OVER titles, and a title is single-line and
// therefore fully normalisable by ClampLine while content legitimately carries
// newlines.
func buildUser(nonce string, core promptCore) string {
	var body strings.Builder
	body.WriteString("Titles:\n")
	for _, t := range core.Titles {
		body.WriteString("- ")
		body.WriteString(promptguard.ClampLine(t))
		body.WriteString("\n")
	}
	if len(core.Tags) > 0 {
		body.WriteString("\nTags: ")
		body.WriteString(promptguard.ClampLine(strings.Join(core.Tags, ", ")))
		body.WriteString("\n")
	}
	if len(core.Categories) > 0 {
		body.WriteString("\nCategories: ")
		body.WriteString(promptguard.ClampLine(strings.Join(core.Categories, ", ")))
		body.WriteString("\n")
	}
	return "Name the cluster described by the following block titles.\n\n" +
		promptguard.Wrap(nonce, "cluster-core", body.String())
}
