// Wave H12 prompt budget (design/04 §4.7): a guard that neutralises control
// tokens still lets a caller push a prompt past the model's context window,
// where the SILENT loser is whichever end the backend truncates — in practice
// the front, i.e. the security rule. A rule that fell out of the window is
// worse than no rule, because everything downstream still behaves as if it
// were there. Hence: the budget is resolved BEFORE the prompt is built, the
// rule is the one part that is never shortened, and a budget that cannot hold
// the rule is an ERROR rather than an unguarded prompt.
package promptguard

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/redact"
	"github.com/GottZ/ctx/internal/util"
)

// Priority orders the parts of a prompt by what a shortening pass may touch
// LAST. Code-set at the call site, never derived from foreign text — a payload
// that could raise its own priority would be able to evict the rule.
//
// Ascending order is load-bearing: Assemble shortens from the LOWEST value
// upwards, so the numeric order IS the eviction order.
type Priority int

// The three priority classes. Content is what gets cut, the question is what
// the model must still be able to act on, the rule is what makes the block
// markers verifiable at all.
const (
	// PriorityContent is foreign text: sources, candidates, documents.
	PriorityContent Priority = iota
	// PriorityQuestion is what the model is asked to do with the content.
	PriorityQuestion
	// PriorityRule is the nonce-bound security sentence (see Rule). Never
	// shortened, never dropped — see ErrRuleOverBudget.
	PriorityRule
)

// Part is one addressable piece of a prompt.
type Part struct {
	// Kind is the code-set class of the part ("source", "question", "rule"),
	// used for telemetry and for the caller's own dispatch. Not rendered.
	Kind string
	// Ref identifies the part for the caller: the ordinal of a source, a block
	// id, "" for the singleton parts. Reported back in Report.DroppedRefs so a
	// call site can map a drop onto its own item.
	Ref string
	// Payload is the part's text. Already guarded by the caller — Assemble is
	// a budget pass, not a neutralisation pass.
	Payload string
	// Priority is the eviction class, see Priority.
	Priority Priority
}

// Report is the accounting of one Assemble pass. A caller that only wants the
// string may ignore it; a caller that logs telemetry reads Dropped/Truncated
// (see the promptguard_dropped llmlog metadata key).
type Report struct {
	// Budget is the rune budget the pass was given.
	Budget int
	// Runes is the rune length of the assembled string (0 on error).
	Runes int
	// Dropped counts parts removed WHOLE.
	Dropped int
	// Truncated counts parts kept but shortened.
	Truncated int
	// DroppedRefs carries the Ref of every dropped part, in input order.
	DroppedRefs []string
	// Parts are the parts AS ASSEMBLED: dropped ones removed, truncated ones
	// carrying their shortened payload, input order preserved. This is what a
	// caller with its own renderer (a structured XML prompt builder, say) feeds
	// back into that renderer instead of using the joined string.
	Parts []Part
	// Err is set when the pass could not produce a guarded prompt. The only
	// such case today is ErrRuleOverBudget.
	Err error
}

// Cut reports whether the pass shortened anything at all.
func (r Report) Cut() bool { return r.Dropped > 0 || r.Truncated > 0 }

// ErrRuleOverBudget is returned when the budget cannot hold the parts that may
// not be shortened. There is deliberately NO fallback to an unguarded prompt:
// a prompt whose security rule did not fit is not a degraded prompt, it is a
// prompt whose markers assert something untrue.
var ErrRuleOverBudget = errors.New("promptguard: budget below the unshortenable parts")

// partSeparator joins the assembled parts. Blank-line separated, matching the
// shape every existing prompt builder already uses between its sections.
const partSeparator = "\n\n"

// truncMarker keeps a shortening VISIBLE to the model — it must know it is
// reading an excerpt. The same constant as the synthesis and classify paths,
// and the same one internal/derived reads back as a negative list (M-W4a).
const truncMarker = redact.Truncated

// minPartRunes is the floor under which a shortened part is dropped whole
// instead. A part cut down to a few runes is not a shorter source, it is a
// misleading one: the model would see a title-less fragment and weigh it as
// evidence. Sized just above truncMarker so a kept part always carries some
// text besides its own truncation notice.
const minPartRunes = 64

// Assemble renders parts into one prompt no longer than budget runes.
//
// Overflow is resolved from BELOW: the lowest Priority first and, within one
// priority, the LAST part first — so on a candidate list the FIRST candidate
// is the one that survives (it is the highest-ranked by every call site's own
// ordering). A part that cannot keep at least minPartRunes is dropped whole
// rather than shortened to a stub.
//
// PriorityRule parts are never touched, and a PriorityQuestion part may be
// shortened but never removed. If that unshortenable core does not fit, the
// pass fails with ErrRuleOverBudget and returns the EMPTY string — neither an
// unguarded prompt nor a task-less one is an acceptable degradation
// (design/04 §4.7).
//
// Rune-safe throughout: every cut goes through util.TruncateRunesSuffix, so a
// multi-byte rune is never split into invalid UTF-8 (the defect Issue #4 named
// on the byte-slice truncations this package's call sites replaced).
func Assemble(parts []Part, budget int) (string, Report) {
	rep := Report{Budget: budget}

	payload := make([]string, len(parts))
	live := make([]bool, len(parts))
	for i, p := range parts {
		payload[i] = p.Payload
		live[i] = true
	}

	// Eviction order: lowest priority first, latest part first inside one
	// priority. Rule parts are absent by construction, which is what makes
	// them unshortenable.
	order := evictionOrder(parts)

	next := 0
	for assembledRunes(payload, live) > budget {
		if next >= len(order) {
			// Only unshortenable parts left and still over budget.
			rep.Err = ErrRuleOverBudget
			return "", rep
		}
		i := order[next]
		over := assembledRunes(payload, live) - budget
		n := utf8.RuneCountInString(payload[i])
		switch {
		case n-over >= minPartRunes:
			payload[i] = util.TruncateRunesSuffix(payload[i], truncMarker, n-over)
			rep.Truncated++
			next++
		case parts[i].Priority == PriorityQuestion:
			// The question may be SHORTENED but never removed. A prompt that
			// kept its security rule and lost the task is not a smaller
			// prompt — it is a rule with nothing to apply it to, and the model
			// would answer from whatever content survived instead of from the
			// question. That belongs in the same class as a lost rule.
			rep.Err = ErrRuleOverBudget
			return "", rep
		default:
			// Dropping frees the payload AND its separator, so re-measuring
			// (loop head) is what decides whether the next victim is needed.
			live[i] = false
			payload[i] = ""
			rep.Dropped++
			rep.DroppedRefs = append(rep.DroppedRefs, parts[i].Ref)
			next++
		}
	}

	kept := make([]Part, 0, len(parts))
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		if !live[i] {
			continue
		}
		p.Payload = payload[i]
		kept = append(kept, p)
		out = append(out, payload[i])
	}
	rep.Parts = kept

	s := strings.Join(out, partSeparator)
	rep.Runes = utf8.RuneCountInString(s)
	return s, rep
}

// evictionOrder returns the indices Assemble may shorten, lowest priority
// first and latest-first within a priority. PriorityRule is excluded.
func evictionOrder(parts []Part) []int {
	var order []int
	for _, prio := range []Priority{PriorityContent, PriorityQuestion} {
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i].Priority == prio {
				order = append(order, i)
			}
		}
	}
	return order
}

// assembledRunes is the rune length of the join over the live parts.
func assembledRunes(payload []string, live []bool) int {
	total, n := 0, 0
	for i, s := range payload {
		if !live[i] {
			continue
		}
		total += utf8.RuneCountInString(s)
		n++
	}
	if n > 1 {
		total += (n - 1) * utf8.RuneCountInString(partSeparator)
	}
	return total
}
