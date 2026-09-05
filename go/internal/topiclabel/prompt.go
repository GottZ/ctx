package topiclabel

import (
	"strings"

	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/util"
)

// languageName maps a primary subtag to the English name the instruction uses.
// Unknown subtags pass through — naming the tag is still a better instruction
// than silently switching the model to English.
//
// The subtag reduction itself is util.PrimaryLanguageSubtag: the KEY is shared
// with the daily report by decision E3-01 — one language knob per corpus, and
// a per-tenant language (parked backlog) inherits automatically.
//
// DELIBERATELY NOT MERGED with dream.langName (design D-04, Naht 9). The two
// tables differ in exactly one branch and the difference is the contract:
// ""/"de" maps to "German" here and is UNREACHABLE there (the report takes a
// frozen German prompt for those tags and never consults its table). Merging
// them would make this surface fall through to a passthrough for the default
// corpus language. The prompt SURFACE is not shared for a second reason
// either: this package must not import internal/dream — the label pipeline is
// deliberately independent of the dream router.
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
		languageName(util.PrimaryLanguageSubtag(lang)) + ".\n\n" +
		"Rules: name the common SUBJECT, not one of the documents. No sentence, no punctuation at the end, no " +
		"quotes, at most 120 characters. Never copy an identifier, a path, a host name or a key from the input.\n\n" +
		"Answer with JSON and nothing else: {\"label\": \"...\"}\n\n" +
		promptguard.Rule(nonce) +
		clusterHarden
}

// clusterHarden pins the output shape to a single-key object against chatty
// models that add reasoning fields. Byte-identical to the goldbench
// cluster-label-v2 A/B variant and appended in the same position (after the
// guard rule) that the A/B measured. Evidence: parse 0.304→0.391, token-F1
// 0.188→0.221 on a format-breaking model (nemotron35-lightning, same-run A/B
// 2026-08-15); neutral on models that already parse at 1.0.
const clusterHarden = "\n\nReturn exactly one JSON object with the single key \"label\" and no other keys. " +
	"Do not add a \"reasoning\", \"explanation\", \"notes\" or any further field."

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
