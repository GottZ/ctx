package goldset

// The frozen question prompts of the M-W5 slices. Frozen means: their sha256
// goes into the slice profile, so a later edit shows up as changed provenance
// instead of as silent slice drift.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	sessSystemPrompt = "You write retrieval evaluation questions about a work log. " +
		"Given a PERIOD label and the body of the daily report(s) written for it, produce EXACTLY ONE question " +
		"asking what was worked on in that period. Rules: the question MUST contain the PERIOD label verbatim; " +
		"it must name ONE concrete topic, component or decision from the body; " +
		"it must not mention 'the report', 'the note' or 'this text'; " +
		"write it in the same language as the body (German or English); " +
		"one line, ending with a question mark, no preamble, no quotes, no numbering."

	sessUserTemplate = "PERIOD: %s\n\nDAILY REPORT:\n%s\n\nQuestion:"

	mhSystemPrompt = "You write multi-hop retrieval evaluation questions. " +
		"Given TWO knowledge notes, produce EXACTLY ONE question that can only be answered by using BOTH of them — " +
		"a question answerable from one note alone is useless. Rules: name the connection, not the notes; " +
		"do not quote either title verbatim; do not mention 'the notes', 'the documents' or 'this text'; " +
		"write it in the same language as the notes (German or English); " +
		"one line, ending with a question mark, no preamble, no quotes, no numbering."

	mhUserTemplate = "NOTE A — %s\n%s\n\nNOTE B — %s\n%s\n\nRELATION: %s\n\nQuestion:"

	globSystemPrompt = "You write aggregating retrieval evaluation questions. " +
		"Given a TOPIC and a sample of note titles filed under it, produce EXACTLY ONE broad question about the topic " +
		"that needs SEVERAL notes to answer — never a single fact. Rules: follow the requested ASPECT; " +
		"mention the topic; do not quote a title verbatim; do not mention 'the notes' or 'this list'; " +
		"write it in the language the titles are mostly in (German or English); " +
		"one line, ending with a question mark, no preamble, no quotes, no numbering."

	globUserTemplate = "TOPIC: %s\nASPECT: %s\nTITLES:\n%s\n\nQuestion:"
)

// Aspects steer the aggregating prompt. They exist so one topic can carry more
// than one case without two cases becoming the same question — and they are a
// fixed, ordered list so the draw stays reproducible.
const (
	AspectOverview    = "the recurring themes and the overall picture"
	AspectDifferences = "the differences, disagreements or trade-offs inside the topic"
	AspectEvolution   = "how the topic developed over time"
)

// GlobAspects is the aspect order used for the floor slice; GlobAspects[:1] is
// what the judged slice uses.
var GlobAspects = []string{AspectOverview, AspectDifferences, AspectEvolution}

// maxNotePairChars bounds each side of a multi-hop prompt. Two full blocks at
// maxBodyChars would double the generation window on a production serving host
// for no measurable gain in question quality.
const maxNotePairChars = 1800

// SessSystem, MHSystem and GlobSystem expose the frozen system prompts.
func SessSystem() string { return sessSystemPrompt }
func MHSystem() string   { return mhSystemPrompt }
func GlobSystem() string { return globSystemPrompt }

// SessPrompt renders the session-window user prompt.
func SessPrompt(w SessionWindow) string {
	return fmt.Sprintf(sessUserTemplate, w.Label, clip(w.Digest, maxBodyChars))
}

// MHPrompt renders the multi-hop user prompt.
func MHPrompt(l DreamLink) string {
	rel := l.Relationship
	if rel == "" {
		rel = "related"
	}
	return fmt.Sprintf(mhUserTemplate,
		l.Source.Title, clip(l.Source.Content, maxNotePairChars),
		l.Target.Title, clip(l.Target.Content, maxNotePairChars), rel)
}

// GlobPrompt renders the aggregating user prompt for one pool and aspect.
func GlobPrompt(p Pool, aspect string) string {
	return fmt.Sprintf(globUserTemplate, p.Label, aspect, "- "+strings.Join(p.Titles, "\n- "))
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// PromptSHA256For digests the frozen prompt pair of a slice — the value that
// goes into the slice profile. The two aggregating slices share a prompt on
// purpose: the floor check is only a floor for the judged slice if both ask the
// same KIND of question.
func PromptSHA256For(slice string) string {
	switch slice {
	case SliceSess:
		return SHA256Hex(sessSystemPrompt + "\n\n" + sessUserTemplate)
	case SliceMH:
		return SHA256Hex(mhSystemPrompt + "\n\n" + mhUserTemplate)
	case SliceGlob, SliceGlobKonstr:
		return SHA256Hex(globSystemPrompt + "\n\n" + globUserTemplate)
	case SliceQ:
		return PromptSHA256()
	default:
		return ""
	}
}

// Verdict is why a generated question was or was not taken.
type Verdict int

// The three outcomes. Redaction is separate from Shape because they are
// different findings: a shape reject grades the generator, a redaction reject
// is a credential that was about to be written into a gold file.
const (
	VerdictAccept Verdict = iota
	VerdictShape
	VerdictRedaction
)

// minQuestionLen is the shape floor shared with AcceptQuestion.
const minQuestionLen = 20

// dateRe matches the ISO date a session question has to carry.
var dateRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

// InspectGeneratedQuery is the quality filter of the new generators.
//
// Two rules carry weight. First, the redaction sweep: a question that fires
// ScanQuery is DISCARDED whole, never carried on redacted — a part-redacted
// query is no longer the query the model wrote, and a gold file is the last
// place a credential should be preserved in any form. Second, a G-SESS question
// must repeat its period label: the window IS the question, and one without a
// date is unanswerable rather than merely vague.
func InspectGeneratedQuery(raw, slice string) (string, Verdict) {
	q := strings.TrimSpace(raw)
	q = strings.Trim(q, "\"'`")
	if i := strings.IndexAny(q, "\n"); i >= 0 {
		q = strings.TrimSpace(q[:i])
	}
	if _, hit := ScanQuery(q); hit {
		return "", VerdictRedaction
	}
	if len(q) < minQuestionLen || !strings.HasSuffix(q, "?") || strings.Count(q, "?") > 1 {
		return "", VerdictShape
	}
	if slice == SliceSess && !dateRe.MatchString(q) {
		return "", VerdictShape
	}
	return q, VerdictAccept
}

// AcceptGeneratedQuery is InspectGeneratedQuery as a boolean.
func AcceptGeneratedQuery(raw, slice string) (string, bool) {
	q, v := InspectGeneratedQuery(raw, slice)
	return q, v == VerdictAccept
}
