package goldset

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/GottZ/ctx/internal/jsonl"
	"github.com/GottZ/ctx/internal/safepath"
)

// PoolEntry is one arm's candidate list for a pooling judgement (design 04
// §4.5, G-REAL; design/05a §C3-2-D05-8 k added G-GLOB in wave C4-3a — the two
// slices whose gold is judged rather than constructed, pooled by the same
// construction so their judgement figures stay comparable). Top-PoolDepth per
// arm BY RANK — the union of four solo-arm heads is the standard pooling
// construction, and taking it per arm rather than from the fused order is what
// keeps the pool from inheriting the very weighting under test.
//
// The type lives here rather than in the sweep driver that writes it because
// the dependency runs armsweep -> goldset: this package cannot import the
// driver back, and two structs for one wire format would be two formats the day
// one of them gains a field. armsweep.PoolEntry is an alias of this type.
type PoolEntry struct {
	Slice       string   `json:"slice"`
	Index       int      `json:"index"`
	QuerySHA256 string   `json:"query_sha256"`
	Semantic    []string `json:"semantic"`
	FTSDe       []string `json:"fts_de"`
	FTSEn       []string `json:"fts_en"`
	Trigram     []string `json:"trigram"`
}

// PoolDepth is the per-arm pooling depth.
const PoolDepth = 20

// judgedSlices is the ONE table of slices whose gold is JUDGED from a pool
// instead of set by construction, together with the gold file each of them
// lives in. G-REAL has been judged since design 04 §4.5; G-GLOB joined in wave
// C4-3a (design/05a §C3-2-D05-8 k) — its cases were generated with a pool
// reference and an EMPTY gold list (E-9), so a pool is the only way it ever
// gets labels.
//
// The sweep driver decides which slices it POOLS (armsweep.pooledSlice); this
// table decides which slices a template can be BUILT for, and the two must name
// the same set or the tooling would pool a slice nobody can judge, or offer a
// template for a slice nobody pooled. They are pinned against each other by
// TestPrimePoolsExactlyTheJudgedSlices (internal/armsweep), which drives the
// production priming path rather than comparing two lists.
//
// The order is the canonical slice order and is what PooledSlices reports, so
// an error message naming the alternatives always names them in the same order.
var judgedSlices = []struct{ Slice, File string }{
	{SliceReal, FileReal},
	{SliceGlob, FileGlob},
}

// PooledSlices names, in canonical order, the slices a judgement template
// exists for.
func PooledSlices() []string {
	out := make([]string, 0, len(judgedSlices))
	for _, s := range judgedSlices {
		out = append(out, s.Slice)
	}
	return out
}

// PoolSliceFile resolves the gold file of a judged slice. ok is false for every
// other slice, and a caller is expected to REFUSE on that rather than fall back
// to a default: a template built for the wrong slice looks exactly like a
// correct one, and the mistake would only surface as labels written into the
// wrong file.
func PoolSliceFile(slice string) (file string, ok bool) {
	for _, s := range judgedSlices {
		if s.Slice == slice {
			return s.File, true
		}
	}
	return "", false
}

// Key is the cross-artefact case key.
func (e PoolEntry) Key() string { return CaseKey(e.Slice, e.Index, e.QuerySHA256) }

// CaseKey is the cross-artefact case key: slice, index and the query digest.
// The digest belongs in it — an index alone would still match after a slice was
// redrawn, and a stale judgement file would then attach labels to different
// queries without producing a single error.
func CaseKey(slice string, index int, sha string) string {
	return fmt.Sprintf("%s/%d/%s", slice, index, sha)
}

// Key is the cross-artefact case key.
func (c Case) Key() string { return CaseKey(c.Slice, c.Index, c.QuerySHA256) }

// ReadPool loads a pooling file written by the sweep driver.
func ReadPool(path string) ([]PoolEntry, error) {
	return jsonl.All[PoolEntry](path)
}

// PooledCase is one judgement unit: a query plus the blinded, permuted list of
// block ids put before the judge. Nothing in it says which arm nominated an id,
// at which rank, or that an id is a control draw — that mapping lives in
// PoolKey and is read only at ingest time.
type PooledCase struct {
	Slice       string
	Index       int
	QuerySHA256 string
	Query       string
	BlockIDs    []string
}

// Key is the cross-artefact case key.
func (p PooledCase) Key() string { return CaseKey(p.Slice, p.Index, p.QuerySHA256) }

// PoolKey records, per case, which ids were drawn as the uniform control sample
// (§4.5). It is a SEPARATE file so the judgement template can carry no trace of
// it: a judge who can tell a control draw from a pooled candidate measures
// their own expectation of the pooling bias instead of measuring the bias.
type PoolKey struct {
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"`
	PoolRunID string `json:"pool_run_id"`
	Seed      int64  `json:"seed"`
	Controls  int    `json:"controls_per_query"`
	// ControlIDs maps a case key to the ids drawn as control for that case.
	ControlIDs map[string][]string `json:"control_ids"`
}

// BuildPool assembles the blinded judgement units for the given cases: the
// union of the four solo-arm heads, plus `controls` blocks drawn uniformly from
// controlPool as noise, deduplicated and seeded-permuted.
//
// controlPool is the retrievable corpus in the deterministic order the database
// already produced for this seed; the draw here is a seeded selection over it,
// so the same seed yields the same control blocks for the same query without a
// second database round trip.
func BuildPool(cases []Case, entries []PoolEntry, controlPool []Block, controls int, seed int64) ([]PooledCase, PoolKey, error) {
	byKey := make(map[string]PoolEntry, len(entries))
	for _, e := range entries {
		if _, dup := byKey[e.Key()]; dup {
			return nil, PoolKey{}, fmt.Errorf("duplicate pool entry for case %s", e.Key())
		}
		byKey[e.Key()] = e
	}
	key := PoolKey{Version: 1, Seed: seed, Controls: controls, ControlIDs: map[string][]string{}}
	out := make([]PooledCase, 0, len(cases))
	for _, c := range cases {
		k := CaseKey(c.Slice, c.Index, c.QuerySHA256)
		e, ok := byKey[k]
		if !ok {
			return nil, PoolKey{}, fmt.Errorf("no pool entry for case %s", k)
		}
		union := unionArms(e)
		drawn, err := drawControls(union, controlPool, controls, seed, c.QuerySHA256)
		if err != nil {
			return nil, PoolKey{}, fmt.Errorf("case %s: %w", k, err)
		}
		out = append(out, PooledCase{
			Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
			Query: c.Query, BlockIDs: Permute(append(union, drawn...), seed, c.QuerySHA256),
		})
		key.ControlIDs[k] = drawn
	}
	return out, key, nil
}

// unionArms deduplicates the four arm heads into one sorted id list. The
// per-arm order is discarded here rather than at render time: an id that
// reaches the template must not be traceable to the arm that nominated it.
func unionArms(e PoolEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, arm := range [][]string{e.Semantic, e.FTSDe, e.FTSEn, e.Trigram} {
		for _, id := range arm {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// drawControls picks n ids uniformly from controlPool, skipping anything
// already pooled. The stream is derived from the seed AND the query digest, so
// every query gets its own reproducible control sample instead of the same n
// blocks recurring under all 150 queries — which a judge would learn.
func drawControls(pooled []string, controlPool []Block, n int, seed int64, querySHA string) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	inPool := make(map[string]bool, len(pooled))
	for _, id := range pooled {
		inPool[id] = true
	}
	avail := make([]string, 0, len(controlPool))
	for _, b := range controlPool {
		if !inPool[b.ID] {
			avail = append(avail, b.ID)
		}
	}
	if len(avail) < n {
		return nil, fmt.Errorf("control sample of %d requested, only %d blocks available beside the pool", n, len(avail))
	}
	//nolint:gosec // deterministic reproducibility is the requirement here, not unpredictability
	r := rand.New(rand.NewPCG(uint64(seed)^0x63747278, streamOf(querySHA))) // "ctrx"
	for i := 0; i < n; i++ {
		j := i + r.IntN(len(avail)-i)
		avail[i], avail[j] = avail[j], avail[i]
	}
	drawn := append([]string(nil), avail[:n]...)
	sort.Strings(drawn)
	return drawn, nil
}

// Permute returns a seeded shuffle of ids. The input is sorted first, so the
// result depends on the SET and the seed alone — a caller that assembles the
// same ids in a different order must get the same template, or "same seed, same
// bytes" would be a property of the caller instead of this function.
func Permute(ids []string, seed int64, querySHA string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	//nolint:gosec // deterministic reproducibility is the requirement here, not unpredictability
	r := rand.New(rand.NewPCG(uint64(seed), streamOf(querySHA)))
	for i := len(out) - 1; i > 0; i-- {
		j := r.IntN(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// streamOf derives a per-query PCG stream from the query digest.
func streamOf(querySHA string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(querySHA))
	return h.Sum64()
}

// writeOwnerOnly writes b and then FORCES mode 0600.
//
// os.WriteFile applies its mode only when it creates the file, so rewriting a
// path that already exists at 0644 leaves it world-readable — and every file in
// this package carries query texts, block ids or the answer key to a blind
// judgement. The explicit chmod is what makes the mode a property of the
// writer instead of a property of whoever created the file first.
func writeOwnerOnly(path string, b []byte) error {
	if err := os.WriteFile(path, b, safepath.FileMode); err != nil { //nolint:gosec // G703: every caller passes a path Guard.Resolve produced
		return err
	}
	return os.Chmod(path, safepath.FileMode)
}

// WritePoolKey persists the control key at mode 0600.
func WritePoolKey(path string, k PoolKey) error {
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerOnly(path, append(b, '\n'))
}

// ReadPoolKey loads the control key.
func ReadPoolKey(path string) (PoolKey, error) {
	var k PoolKey
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return k, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// --------------------------------------------------------------- template.

// UnjudgedMark is the placeholder a judge overwrites. A visible character
// rather than a blank cell, so a skipped row stays a skipped row and is not
// confused with a cell that lost its content to an editor.
const UnjudgedMark = "_"

// excerptOf renders a block for reading: whitespace collapsed, truncated. The
// text is foreign content of a private corpus — it is displayed, never
// interpreted.
func excerptOf(b Block, limit int) string {
	s := strings.Join(strings.Fields(b.Content), " ")
	if limit > 0 && len(s) > limit {
		cut := limit
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = strings.TrimSpace(s[:cut]) + "…"
	}
	return s
}

// utf8Start reports whether b begins a UTF-8 rune, so a truncation never splits
// one — the corpus is German and a split rune would be a mojibake row.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// templateLine is one JSONL row of the judgement template. Every field a judge
// could use to reconstruct rank, arm or control membership is absent by
// construction, not filtered out afterwards.
type templateLine struct {
	Kind        string  `json:"kind"`
	Slice       string  `json:"slice"`
	Index       int     `json:"index"`
	QuerySHA256 string  `json:"query_sha256"`
	Query       string  `json:"query,omitempty"`
	Candidates  int     `json:"candidates,omitempty"`
	BlockID     string  `json:"block_id,omitempty"`
	Title       string  `json:"title,omitempty"`
	Excerpt     string  `json:"excerpt,omitempty"`
	Judgement   *string `json:"judgement,omitempty"`
}

// RenderTemplateJSONL is the machine form of the judgement template: one header
// row per query, then one row per candidate with an empty judgement field.
func RenderTemplateJSONL(pooled []PooledCase, blocks map[string]Block, excerpt int) ([]byte, error) {
	var buf strings.Builder
	w := bufio.NewWriter(&buf)
	enc := json.NewEncoder(w)
	for _, p := range pooled {
		if err := enc.Encode(templateLine{
			Kind: "query", Slice: p.Slice, Index: p.Index, QuerySHA256: p.QuerySHA256,
			Query: p.Query, Candidates: len(p.BlockIDs),
		}); err != nil {
			return nil, err
		}
		for _, id := range p.BlockIDs {
			b := blocks[id]
			empty := ""
			if err := enc.Encode(templateLine{
				Kind: "candidate", Slice: p.Slice, Index: p.Index, QuerySHA256: p.QuerySHA256,
				BlockID: id, Title: b.Title, Excerpt: excerptOf(b, excerpt), Judgement: &empty,
			}); err != nil {
				return nil, err
			}
		}
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// RenderTemplateMarkdown is the human form. The judgement is the first column,
// so one case is one keystroke per row at a fixed offset.
func RenderTemplateMarkdown(pooled []PooledCase, blocks map[string]Block, excerpt int) []byte {
	var b strings.Builder
	b.WriteString("# Relevanz-Urteile " + templateSliceName(pooled) + "\n\n")
	b.WriteString("Ein Urteil je Zeile, erste Spalte: `1` = relevant, `0` = nicht relevant.\n")
	b.WriteString("`" + UnjudgedMark + "` heißt ungeurteilt und wird beim Einlesen als Fehler abgewiesen.\n")
	b.WriteString("Geurteilt wird gegen die Frage, nicht gegen eine erwartete Reihenfolge.\n\n")
	for _, p := range pooled {
		// The heading carries the FULL digest, not a prefix: it is the only
		// anchor the markdown form gives the parser, and a prefix would let two
		// cases collide into one set of labels.
		fmt.Fprintf(&b, "## %s #%d · %s\n\n", p.Slice, p.Index, p.QuerySHA256)
		fmt.Fprintf(&b, "**Frage:** %s\n\n", mdCell(p.Query))
		b.WriteString("| U | Block | Titel | Auszug |\n| --- | --- | --- | --- |\n")
		for _, id := range p.BlockIDs {
			blk := blocks[id]
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
				UnjudgedMark, id, mdCell(blk.Title), mdCell(excerptOf(blk, excerpt)))
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// templateSliceName names the slice a template covers. `ctx-goldset pool`
// builds one slice at a time, so this is one name.
//
// It is DERIVED from the rows rather than passed in: the heading is what tells
// a human judge which instrument they are filling in, and a template headed
// "G-REAL" over G-GLOB rows would be worse than an unheaded one. An empty
// template keeps the historical name, so the byte form of every G-REAL
// template written before wave C4-3b stays reproducible.
func templateSliceName(pooled []PooledCase) string {
	seen := map[string]bool{}
	names := make([]string, 0, 1)
	for _, p := range pooled {
		if p.Slice != "" && !seen[p.Slice] {
			seen[p.Slice] = true
			names = append(names, p.Slice)
		}
	}
	if len(names) == 0 {
		return SliceReal
	}
	return strings.Join(names, " + ")
}

// mdCell makes foreign text safe for one table cell: no newlines, and no pipe
// that would open a column the table never planned.
func mdCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

// WriteTemplate writes both forms of the template at mode 0600.
func WriteTemplate(jsonlPath, mdPath string, pooled []PooledCase, blocks map[string]Block, excerpt int) error {
	jl, err := RenderTemplateJSONL(pooled, blocks, excerpt)
	if err != nil {
		return err
	}
	if err := writeOwnerOnly(jsonlPath, jl); err != nil {
		return err
	}
	return writeOwnerOnly(mdPath, RenderTemplateMarkdown(pooled, blocks, excerpt))
}

// ---------------------------------------------------------------- ingest.

// ErrUnjudged marks a candidate row a human left untouched. It is an ERROR and
// never a silent "not relevant": a skipped row and a negative verdict differ by
// the whole of the judge's attention, and treating the first as the second
// would inflate the labelled n with cases nobody looked at.
var ErrUnjudged = errors.New("missing judgement")

// Judgement is one binary relevance verdict of one candidate for one query.
type Judgement struct {
	Slice       string
	Index       int
	QuerySHA256 string
	BlockID     string
	Relevant    bool
}

// ParseJudgements reads a filled-in template in either of the two forms
// RenderTemplate* emits and returns the verdicts keyed by case. The form is
// detected from the first non-blank byte, so a judge may work in whichever file
// suits them without telling the tool which one they picked.
func ParseJudgements(path string) (map[string][]Judgement, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
		return parseJudgementsJSONL(path, b)
	}
	return parseJudgementsMarkdown(path, b)
}

// parseJudgementsJSONL reads the machine form.
func parseJudgementsJSONL(path string, b []byte) (map[string][]Judgement, error) {
	out := map[string][]Judgement{}
	if err := jsonl.EachReader(bytes.NewReader(b), path, func(n int, l templateLine) error {
		switch l.Kind {
		case "query":
			return nil
		case "candidate":
		default:
			return fmt.Errorf("%s:%d: unknown row kind %q", path, n, l.Kind)
		}
		if l.Judgement == nil {
			return fmt.Errorf("%s:%d: block %s: %w", path, n, l.BlockID, ErrUnjudged)
		}
		rel, verr := verdict(*l.Judgement)
		if verr != nil {
			return fmt.Errorf("%s:%d: block %s: %w", path, n, l.BlockID, verr)
		}
		k := CaseKey(l.Slice, l.Index, l.QuerySHA256)
		out[k] = append(out[k], Judgement{
			Slice: l.Slice, Index: l.Index, QuerySHA256: l.QuerySHA256,
			BlockID: l.BlockID, Relevant: rel,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// parseJudgementsMarkdown reads the human form. The case context comes from the
// heading; a table row that appears before one is an error rather than a row
// attached to whatever case was seen last.
func parseJudgementsMarkdown(path string, b []byte) (map[string][]Judgement, error) {
	out := map[string][]Judgement{}
	var cur Judgement
	have := false
	for n, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			h, err := parseHeading(trimmed)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, n+1, err)
			}
			cur, have = h, true
			continue
		}
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := tableCells(trimmed)
		if len(cells) < 2 || isTableFurniture(cells) {
			continue
		}
		if !have {
			return nil, fmt.Errorf("%s:%d: candidate row before any case heading", path, n+1)
		}
		rel, err := verdict(cells[0])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: block %s: %w", path, n+1, cells[1], err)
		}
		j := cur
		j.BlockID, j.Relevant = cells[1], rel
		out[CaseKey(j.Slice, j.Index, j.QuerySHA256)] = append(out[CaseKey(j.Slice, j.Index, j.QuerySHA256)], j)
	}
	return out, nil
}

// parseHeading reads "## <slice> #<index> · <query-sha256>".
func parseHeading(line string) (Judgement, error) {
	var j Judgement
	fields := strings.Fields(strings.TrimPrefix(line, "## "))
	if len(fields) < 4 || !strings.HasPrefix(fields[1], "#") {
		return j, fmt.Errorf("malformed case heading %q", line)
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(fields[1], "#"))
	if err != nil {
		return j, fmt.Errorf("malformed case heading %q: %w", line, err)
	}
	j.Slice, j.Index, j.QuerySHA256 = fields[0], idx, fields[3]
	return j, nil
}

// tableCells splits a markdown table row into its trimmed cells.
func tableCells(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// isTableFurniture skips the header and separator rows of a rendered table.
func isTableFurniture(cells []string) bool {
	if cells[0] == "U" {
		return true
	}
	return strings.Trim(cells[0], "-: ") == "" && cells[0] != ""
}

// verdict maps a judgement cell to a boolean. The vocabulary is closed on
// purpose — an unrecognised token is a typo somewhere in twelve thousand rows,
// and guessing at it is how a typo becomes a label.
func verdict(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "y":
		return true, nil
	case "0", "n":
		return false, nil
	case "", UnjudgedMark:
		return false, fmt.Errorf("%w (allowed: 1, 0, y, n)", ErrUnjudged)
	}
	return false, fmt.Errorf("invalid judgement %q (allowed: 1, 0, y, n)", strings.TrimSpace(s))
}

// LabelStats is the ingest profile that reaches the slice stamp.
type LabelStats struct {
	Cases      int
	Labelled   int
	NoRelevant int
	Judged     int
	Relevant   int
	PoolP50    int
	PoolMax    int
}

// ApplyLabels writes the judged relevance sets into the cases.
//
// A case whose pool holds nothing relevant KEEPS its place in the slice with an
// empty GoldIDs — declared in advance (§4.5). Dropping it would silently remove
// exactly the queries the retrieval is worst at, and every metric computed
// afterwards would be computed over a population selected by the thing under
// measurement.
func ApplyLabels(cases []Case, judged map[string][]Judgement) ([]Case, LabelStats, error) {
	return ApplyLabelsNamed(cases, judged, "", false)
}

// ApplyLabelsNamed is the C3-4a form (design/05a §C3-2-D05-8 i): the same
// labelling, plus the name of the gold source and the option to build a gold
// variant that covers only PART of the slice.
//
// `restrict` is what makes the core variant legitimate. Dropping unjudged cases
// is normally the selection trap ApplyLabels exists to prevent — but the core
// is not a selection ON the measurement, it is a pre-declared, hash-drawn set of
// 20 queries, and the variant carries its own name so no reader can mistake it
// for the full slice. Without `restrict` an unjudged case is still an abort.
func ApplyLabelsNamed(cases []Case, judged map[string][]Judgement,
	source string, restrict bool,
) ([]Case, LabelStats, error) {
	out := make([]Case, 0, len(cases))
	for _, c := range cases {
		if restrict {
			if js, ok := judged[c.Key()]; !ok || len(js) == 0 {
				continue
			}
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, LabelStats{}, fmt.Errorf("keine der %d Fälle trägt Urteile — "+
			"die Gold-Variante %q wäre leer", len(cases), source)
	}
	st := LabelStats{Cases: len(out)}
	sizes := make([]int, 0, len(out))
	for i := range out {
		k := out[i].Key()
		js, ok := judged[k]
		if !ok || len(js) == 0 {
			return nil, st, fmt.Errorf("no judgements for case %s — the template covers the whole slice, "+
				"and a partial file would label a subset without saying so", k)
		}
		out[i].GoldSource = source
		rel := make([]string, 0, len(js))
		for _, j := range js {
			st.Judged++
			if j.Relevant {
				st.Relevant++
				rel = append(rel, j.BlockID)
			}
		}
		sort.Strings(rel)
		out[i].GoldIDs = rel
		sizes = append(sizes, len(js))
		if len(rel) == 0 {
			st.NoRelevant++
		} else {
			st.Labelled++
		}
	}
	st.PoolP50, st.PoolMax = median(sizes), maxOf(sizes)
	return out, st, nil
}

// median is the upper median of the pool sizes.
func median(v []int) int {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	return s[len(s)/2]
}

// maxOf is the largest pool size.
func maxOf(v []int) int {
	m := 0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

// ControlHitRate is the declared residual bias of the pooling construction as a
// NUMBER: the share of uniformly drawn control blocks a judge called relevant.
// Without the key file the rate is not computable, and the ingest must say so
// instead of stamping a rate of zero that would read as "no bias measured".
func ControlHitRate(judged map[string][]Judgement, key PoolKey) (rate float64, hits, total int, err error) {
	if len(key.ControlIDs) == 0 {
		return 0, 0, 0, errors.New("control key holds no entries — the control hit rate is not computable")
	}
	for k, ids := range key.ControlIDs {
		byID := make(map[string]bool, len(judged[k]))
		seen := make(map[string]bool, len(judged[k]))
		for _, j := range judged[k] {
			byID[j.BlockID], seen[j.BlockID] = j.Relevant, true
		}
		for _, id := range ids {
			if !seen[id] {
				return 0, 0, 0, fmt.Errorf("case %s: no judgement for control block %s", k, id)
			}
			total++
			if byID[id] {
				hits++
			}
		}
	}
	if total == 0 {
		return 0, 0, 0, errors.New("control key holds no block ids — the control hit rate is not computable")
	}
	return float64(hits) / float64(total), hits, total, nil
}

// MergeStampSlice merges fields into slices[<name>] of the stamp file WITHOUT
// going through the typed Stamp struct.
//
// That is the point: a key some later wave writes into the stamp would be
// dropped by an earlier tool's typed round trip, and the loss would be
// invisible — the file stays valid JSON and simply forgets a field. Merging on
// the raw document keeps every foreign key that was there.
func MergeStampSlice(path, name string, fields map[string]any) error {
	doc := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(b, &doc); uerr != nil {
			return fmt.Errorf("%s: %w", path, uerr)
		}
	case os.IsNotExist(err):
		doc["version"] = 1
	default:
		return err
	}
	sl, _ := doc["slices"].(map[string]any)
	if sl == nil {
		sl = map[string]any{}
	}
	entry, _ := sl[name].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	for k, v := range fields {
		entry[k] = v
	}
	sl[name] = entry
	doc["slices"] = sl
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerOnly(path, append(out, '\n'))
}

// BackupFile copies path to path+".bak-<stamp>" before it is rewritten. The
// pre-ingest slice is the only proof of what the labels were derived from.
func BackupFile(path, stamp string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dst := path + ".bak-" + stamp
	if err := writeOwnerOnly(dst, b); err != nil {
		return "", err
	}
	return dst, nil
}
