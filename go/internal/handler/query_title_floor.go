package handler

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/GottZ/ctx/internal/rrf"
)

// floorTitleMatch is the query's Step 6d-title (issue #507): a question that
// is the title of a candidate names that block, and the block comes first.
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
//     punctuation, one pair of quotation marks around the whole text) — no
//     similarity threshold, so a question about a family's topic, or one that
//     contains a short title, moves nothing;
//   - any other candidate whose title contains the question (a title made of
//     one frequent word, "Deploy" beside "Deploy runbook") leaves the order as
//     it is; of several candidates whose titles fold to the question (twins:
//     "Release notes" and "Release notes?"), the one whose title equals the
//     question as typed (verbatimTitle: case and white space aside, issue
//     #528) is the named block, and among the rest the one the pipeline ranks
//     highest; if it is already first nothing moves;
//   - a question that wraps a title in a leading phrase ("zeig mir …", "show
//     me …", titlePhrases) names the block whose title equals the rest, when
//     exactly one candidate's title does and no other candidate's title
//     contains the rest (phraseNamed, issue #528);
//   - the named block moves to the first place; the results before its old
//     place move down by one, and the result at the end of the limit leaves
//     the answer. The first place is the one every synthesis prompt keeps: a
//     low_confidence answer is written from the first
//     query.low_confidence_max_sources sources only (at the last place of the
//     limit the block stood among the sources but outside the prompt), and the
//     prompt budget evicts the latest part of the prompt first, where
//     LostInMiddleReorder puts the second source — so the second place is the
//     first to go when a small context window cuts;
//   - the block takes the score fields of the row whose place it takes, the
//     former first (RRFScore and RRFScoreOriginal), the way the graph and
//     cluster stages place a block by a score relative to the list: the list
//     stays sorted, the synthesis path's score threshold (query.score_threshold)
//     judges the place rather than the fused score that put the block behind
//     its siblings, and the list's highest score is still there for the
//     confidence label (llm.ClassifySources reads the scores on both paths,
//     issue #528). The floor therefore never lifts the label and never lowers
//     it: the first place's score stays the list's highest. The block's own
//     score stays readable in TitleFloorScore (title_floor_score on the wire,
//     issue #528).
//
// A block the candidate window (the fusion's internal limit, 200 or the
// aggregate over-fetch) does not hold cannot be placed; the pass only reorders
// what retrieval found. Rows of a type in skipTypes (the shadow measurement
// types, dropped after the truncation) are never placed, so the floor cannot
// hand a slot to a row that leaves the answer afterwards.
func floorTitleMatch(results []rrf.SearchResult, question string, skipTypes []string) []rrf.SearchResult {
	if len(results) <= 1 {
		return results
	}
	q := foldTitle(question)
	if q == "" {
		return results
	}
	match := titleNamed(results, q, verbatimTitle(question), typedForm(question))
	if match < 0 {
		match = phraseNamed(results, q, question)
	}
	if match <= 0 {
		return results
	}
	for _, st := range skipTypes {
		if results[match].TypeName == st {
			return results
		}
	}
	placed := results[match]
	own := placed.RRFScore
	placed.TitleFloorScore = &own
	placed.RRFScore = results[0].RRFScore
	placed.RRFScoreOriginal = results[0].RRFScoreOriginal
	out := make([]rrf.SearchResult, 0, len(results))
	out = append(out, placed)
	out = append(out, results[:match]...)
	out = append(out, results[match+1:]...)
	return out
}

// titleNamed is the index of the candidate whose title equals the folded
// question q, or -1 when none does or another candidate's title contains q.
// Of twins — several titles that fold to q — the first whose title equals the
// question as typed (asTyped, its verbatimTitle form) wins, then the first
// whose title equals it without an enclosing pair of marks (unquoted, its
// typedForm): "Release notes" names the block of that title, not its
// higher-ranked twin "Release notes?" (issue #528), so does "„Release notes“",
// and a question equal to a title that carries the marks itself names that
// title. Without such a title the pipeline's rank decides.
func titleNamed(results []rrf.SearchResult, q, asTyped, unquoted string) int {
	match, exact, bare := -1, -1, -1
	for i := range results {
		t := foldTitle(results[i].Title)
		if t == q {
			if match < 0 {
				match = i
			}
			v := verbatimTitle(results[i].Title)
			if exact < 0 && v == asTyped {
				exact = i
			}
			if bare < 0 && v == unquoted {
				bare = i
			}
			continue
		}
		if strings.Contains(t, q) {
			return -1
		}
	}
	switch {
	case exact >= 0:
		return exact
	case bare >= 0:
		return bare
	}
	return match
}

// phraseNamed is the index of the candidate a question names behind a leading
// phrase (titlePhrases), or -1 (issue #528). The phrase is read off the
// question only, and the rest must equal the folded title of exactly ONE
// candidate that no other candidate's title contains: a topic question with
// the same start ("zeig mir alle Deploys", "show me the latest handovers")
// equals no title, and a rest that is a word other titles contain ("zeig mir
// Deploy" beside "Deploy runbook") moves nothing — the two ways a stripped
// phrase could otherwise turn a topic into a title. Of twins whose titles fold
// to the rest, the one whose title equals the rest as typed (typedRest) is that
// one candidate, when it is the only one; otherwise the twins name nothing.
func phraseNamed(results []rrf.SearchResult, q, question string) int {
	for _, p := range titlePhrases {
		rest, ok := strings.CutPrefix(q, p)
		if !ok {
			continue
		}
		rest = foldTitle(rest)
		if rest == "" {
			return -1
		}
		typed, typedOK := typedRest(question, p)
		match, folded, verbatim, typedN := -1, 0, -1, 0
		for i := range results {
			t := foldTitle(results[i].Title)
			if t == rest {
				folded++
				match = i
				if typedOK && verbatimTitle(results[i].Title) == typed {
					typedN++
					verbatim = i
				}
				continue
			}
			if strings.Contains(t, rest) {
				return -1
			}
		}
		switch {
		case folded == 1:
			return match
		case typedN == 1:
			return verbatim
		}
		return -1
	}
	return -1
}

// typedRest is the question as typed (typedForm) behind the leading phrase p,
// which matched the folded question: the phrase is cut off rune by rune, each
// rune read the way foldTitle reads it (compatibility form, typographic
// apostrophes as ASCII), so "What’s in „Notes v1?“" keeps "notes v1?". ok is false when the typed question does not start with the
// phrase in that reading (a compatibility form the fold alone resolves).
func typedRest(question, p string) (string, bool) {
	v := []rune(verbatimTitle(question))
	pr := []rune(p)
	if len(v) < len(pr) {
		return "", false
	}
	for i, r := range pr {
		if m := []rune(typographicQuotes.Replace(norm.NFKC.String(string(v[i])))); len(m) != 1 || m[0] != r {
			return "", false
		}
	}
	return typedForm(string(v[len(pr):])), true
}

// titlePhrases are the leading phrases of a title lookup, in the folded form
// (lower case, ASCII apostrophe) and with the space that ends them (issue
// #528, point 4: the forms the review holdout of #507 asked in, German and
// English).
var titlePhrases = []string{
	"zeig mir ", "zeige mir ", "was steht in ",
	"show me ", "what's in ", "what is in ",
}

// foldTitle is the comparison form of a title and of a question, the same on
// both sides (issue #507, round 1): Unicode NFKC, typographic quotes and
// apostrophes as their ASCII forms, lower case, runs of white space as one
// space, no closing punctuation (titleCloseCutset) or space at the end, and
// without one pair of quotation marks that encloses the whole text (issue
// #528: „Release notes“ asks for the title Release notes; a question that
// contains a quotation is no title and keeps its marks).
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
	s = strings.TrimRight(s, titleCloseCutset)
	if inner, ok := enclosedText(s); ok {
		s = strings.TrimRight(strings.TrimSpace(inner), titleCloseCutset)
	}
	return s
}

// enclosedText is s without the quotation mark at its start and its end, when
// both are the same mark (titleQuoteMarks, after typographicQuotes). The mark
// may occur in between — an apostrophe ("'Orbit's report'") or a quotation
// inside the title ("„Plan „B“ final“"); a question made of two quotations
// ("\"a\" and \"b\"") loses its outer marks too, but what is left
// ("a\" and \"b") equals no title, so it still names nothing.
func enclosedText(s string) (string, bool) {
	if len(s) < 2 || s[0] != s[len(s)-1] || !strings.ContainsRune(titleQuoteMarks, rune(s[0])) {
		return "", false
	}
	return s[1 : len(s)-1], true
}

// titleQuoteMarks are the marks a title can be asked inside: the ASCII double
// and single quote every typographic form maps to, and the backtick of a
// code span.
const titleQuoteMarks = "\"'`"

// typedForm is the verbatim comparison form of a question (issue #528, review
// round 1): verbatimTitle without what surrounds a title asked inside one pair
// of quotation marks — the pair (any two marks foldTitle reads as the same
// mark) and the closing punctuation behind it — so "„Notes v1?“" is told apart
// from its twin "Notes v1" like the bare "Notes v1?". The closing punctuation
// of a question without a pair stays: it is what tells "Notes v1?" from
// "Notes v1".
func typedForm(s string) string {
	v := verbatimTitle(s)
	r := []rune(v)
	for len(r) > 0 && closingRune(r[len(r)-1]) {
		r = r[:len(r)-1]
	}
	if len(r) >= 2 {
		if m := quoteMark(r[0]); m != 0 && m == quoteMark(r[len(r)-1]) {
			return strings.TrimSpace(string(r[1 : len(r)-1]))
		}
	}
	return v
}

// closingRune reports whether foldTitle drops r at the end of a title or
// question: its compatibility form is closing punctuation or space
// (titleCloseCutset; "…" and the full-width "？" included).
func closingRune(r rune) bool {
	m := norm.NFKC.String(string(r))
	return m != "" && strings.Trim(m, titleCloseCutset) == ""
}

// quoteMark is the ASCII mark of titleQuoteMarks that foldTitle reads r as,
// or 0 for any other rune.
func quoteMark(r rune) byte {
	m := typographicQuotes.Replace(norm.NFKC.String(string(r)))
	if len(m) == 1 && strings.IndexByte(titleQuoteMarks, m[0]) >= 0 {
		return m[0]
	}
	return 0
}

// verbatimTitle is the form in which a question equals a title as typed:
// canonical composition, lower case, runs of white space as one space — no
// compatibility folding, no quote or closing-punctuation rule, which are what
// makes two titles twins (issue #528).
func verbatimTitle(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(norm.NFC.String(s)), " "))
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
