package handler

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
)

// floorTitleMatch is the query's Step 6d-title (issue #507): a question that
// is the title of exactly one candidate names that block, and the block comes
// to a place every synthesis prompt carries (place, from titleFloorPlace).
//
// Why a floor behind every stage: among blocks whose titles differ in a date or
// a version number only (one block per release, handover, daily report,
// archive part), the title arm finds the named block first and carries the
// smallest weight of the four, the semantic arm sees near-identical texts, and
// a date in the question fills both full-text arms with the date expansion. On
// the measured corpus 85 of 440 blocks were not among the ten sources for
// their own title — 74 of the 124 with twenty or more title neighbours — and
// 81 of the 85 were already outside the ten after the fusion; the stages
// behind it lost the other four. The fusion is SQL; this pass runs in Go on
// the candidate window and changes no statement.
//
// The rule, deliberately narrow:
//   - the question equals the candidate's title in their folded form
//     (foldTitle: normal form, quotes, case, white space, closing
//     punctuation) — no similarity threshold, so a question about a family's
//     topic, or one that contains a short title, moves nothing;
//   - it names exactly ONE candidate: a second candidate with the same title,
//     or any other candidate whose title contains the question (a title made
//     of one frequent word, "Deploy" beside "Deploy runbook"), leaves the
//     order as it is;
//   - a block behind the place moves to it; the results before the place keep
//     their order, those from the place on move down by one, and the result at
//     the end of the limit leaves the answer. The place is the last one a
//     low_confidence prompt still carries (titleFloorPlace): at the last place
//     of the limit the block stood among the sources but, under
//     query.low_confidence_max_sources, outside the prompt the answer is
//     written from;
//   - the block takes the score fields of the row whose place it takes
//     (RRFScore and RRFScoreOriginal), the way the graph and cluster stages
//     place a block by a score relative to the list: the list stays sorted,
//     the synthesis path's score threshold (query.score_threshold) judges the
//     place rather than the fused score that put the block behind its
//     siblings, and both confidence labels — the synthesis reads the scores,
//     the retrieval-only path rrf_score_original — read the values they read
//     for that place before. The floor therefore never lifts an answer's label;
//     it changes only when the result that leaves at the end of the limit
//     carried the list's highest score.
//
// A block the candidate window (the fusion's internal limit, 200 or the
// aggregate over-fetch) does not hold cannot be placed; the pass only reorders
// what retrieval found. Rows of a type in skipTypes (the shadow measurement
// types, dropped after the truncation) are never placed, so the floor cannot
// hand a slot to a row that leaves the answer afterwards.
func floorTitleMatch(results []rrf.SearchResult, question string, place int, skipTypes []string) []rrf.SearchResult {
	if place <= 0 || len(results) <= place {
		return results
	}
	q := foldTitle(question)
	if q == "" {
		return results
	}
	match := -1
	for i := range results {
		t := foldTitle(results[i].Title)
		if t == q {
			if match >= 0 {
				return results
			}
			match = i
			continue
		}
		if strings.Contains(t, q) {
			return results
		}
	}
	at := place - 1
	if match <= at {
		return results
	}
	for _, st := range skipTypes {
		if results[match].TypeName == st {
			return results
		}
	}
	placed := results[match]
	placed.RRFScore = results[at].RRFScore
	placed.RRFScoreOriginal = results[at].RRFScoreOriginal
	out := make([]rrf.SearchResult, 0, len(results))
	out = append(out, results[:at]...)
	out = append(out, placed)
	out = append(out, results[at:match]...)
	out = append(out, results[match+1:]...)
	return out
}

// titleFloorPlace is the place floorTitleMatch moves a named block to: the
// last place a synthesis prompt carries under every label — the first
// llm.LowConfidenceCut sources of a low_confidence answer — and never past the
// caller's limit.
func titleFloorPlace(limit int, st llm.SynthesisSettings) int {
	if cut := llm.LowConfidenceCut(st); cut < limit {
		return cut
	}
	return limit
}

// foldTitle is the comparison form of a title and of a question, the same on
// both sides (issue #507, round 1): Unicode NFKC, typographic quotes and
// apostrophes as their ASCII forms, lower case, runs of white space as one
// space, and no closing punctuation (titleCloseCutset) or space at the end.
//
// NFKC and strings.ToLower are the form the repository already compares
// titles in (derived.Normalize, topiclabel's echo guard): a question typed in
// one normal form, or with full-width characters, names the title stored in
// another. Punctuation inside stays: "v5.75.1", "(2026-11-22)" and
// "2026.08.31" remain what tells one block of a family from the next — only
// the closing "?", ".", "!", ":" or "…" a person adds to a question is dropped.
// "ß" is not folded to "ss": that would join two words ("Maße", "Masse").
func foldTitle(s string) string {
	s = typographicQuotes.Replace(norm.NFKC.String(s))
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.TrimRight(s, titleCloseCutset)
}

// titleCloseCutset is the closing punctuation foldTitle drops from the end of a
// title or question, with the spaces before it. NFKC has already turned "…"
// into "..." and the full-width forms into these.
const titleCloseCutset = "?.!: "

// typographicQuotes maps typographic quotation marks and apostrophes to the
// ASCII forms, so a title written with „…“ or “…” and a question typed with
// "…" (or the reverse) fold alike.
var typographicQuotes = strings.NewReplacer(
	"\u201c", `"`, "\u201d", `"`, "\u201e", `"`, "\u201f", `"`, "\u00ab", `"`, "\u00bb", `"`,
	"\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'", "\u2039", "'", "\u203a", "'",
)
