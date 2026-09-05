package goldset

// The judging run of wave M-W9 (design/05 §4.5 (3), §5 B6, entscheid E2-4).
//
// Three things live here, and they are one wave because they are one contract:
// a judge produces verdicts, a CALIBRATION judge re-judges a sample of them,
// and Cohen's kappa between the two decides whether a downstream gate may rest
// on the machine verdicts at all.
//
// The rule is written down before the run, not after it: kappa below the stated
// threshold — or a gate that FLIPS between the machine-judged and the
// calibration-judged computation — leaves the gate "nicht entschieden". Not
// passed, not failed. A tool that took the machine verdict at low kappa would
// turn a measurement into a self-confirmation, and this axis already has that
// precedent (the 30-sample self-judge of session 24, refuted at n = 122).
//
// Who calibrates: the Haupt-Lead (Fable), goal-directed, per entscheid E2-4
// ("ich urteile nicht. fable urteilt unter berücksichtigung des ziels.") — the
// earlier "user sample" of board #1 is superseded. The kappa mechanics do not
// care who the second judge is; the naming does, because a report that names
// the wrong judge misstates its own provenance.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/jsonl"
)

// ErrJudgeIncomplete marks a rendering attempt over a run that still has open
// cells. Rendering a partial template would produce a file `ingest` accepts and
// that silently labels a subset of the slice.
var ErrJudgeIncomplete = errors.New("judgement run incomplete")

// ------------------------------------------------------------------- seam.

// JudgeCandidate is one candidate put before a judge: the same three fields the
// blind template shows a human, and nothing else.
type JudgeCandidate struct {
	BlockID string
	Title   string
	Excerpt string
}

// Judge is the verdict source of a run. Two methods, because a judge that
// cannot state its own provenance cannot be gated on it: RunJudge asserts
// Provenance() against the on-prem rule BEFORE the first cell (§5 B6).
type Judge interface {
	Judge(ctx context.Context, query string, c JudgeCandidate) (bool, error)
	Provenance() Generator
}

// The frozen judging prompt. Its digest reaches the stamp, so a later edit is
// visible as changed provenance instead of as silent verdict drift.
const (
	judgeSystemPrompt = "You judge retrieval relevance. " +
		"Given a QUESTION and one candidate knowledge note, decide whether the note is relevant to the question. " +
		"Relevant means the note contributes to answering the question — it need not be the only or the best answer. " +
		"Judge against the question, never against an expected ranking. " +
		"Answer with EXACTLY ONE character: 1 for relevant, 0 for not relevant. No explanation, no punctuation."

	judgeUserTemplate = "QUESTION: %s\n\nTITLE: %s\n\nNOTE:\n%s\n\nAnswer (1 or 0):"
)

// JudgePromptSHA256 digests the frozen judging prompt pair.
func JudgePromptSHA256() string { return SHA256Hex(judgeSystemPrompt + "\n\n" + judgeUserTemplate) }

// ChatJudge is the on-prem judge. It reuses the goldset chat client rather than
// internal/llm for the reason stated at NewChatClient: this tool must not be
// able to pick up a runtime backend switch and send block excerpts elsewhere.
type ChatJudge struct {
	client *ChatClient
	gen    Generator
}

// NewChatJudge builds a judge over a verified on-prem backend.
func NewChatJudge(b Backend, model string, timeout time.Duration) (*ChatJudge, error) {
	c, err := NewChatClient(b, model, timeout)
	if err != nil {
		return nil, err
	}
	return &ChatJudge{client: c, gen: Generator{
		Backend: b.Name, Model: c.Model, Endpoint: c.URL,
		Locality: b.Locality, Trust: b.Trust, PromptSHA256: JudgePromptSHA256(),
	}}, nil
}

// Provenance reports what the judge is, for the gate and for the stamp.
func (j *ChatJudge) Provenance() Generator { return j.gen }

// Judge asks for one binary verdict. max_tokens is 4 rather than 1: a serving
// that emits a leading space or newline would otherwise return an empty string
// and turn every cell into an error.
func (j *ChatJudge) Judge(ctx context.Context, query string, c JudgeCandidate) (bool, error) {
	ans, err := j.client.Ask(ctx, judgeSystemPrompt,
		fmt.Sprintf(judgeUserTemplate, query, c.Title, c.Excerpt), 4, 0)
	if err != nil {
		return false, err
	}
	return ParseJudgeAnswer(ans)
}

// ParseJudgeAnswer maps a model answer onto the SAME closed vocabulary a human
// judgement cell uses. An answer outside it is an error: a model that starts
// explaining has not judged, and guessing at its intent is how a non-answer
// becomes a label.
func ParseJudgeAnswer(s string) (bool, error) {
	t := strings.TrimSpace(s)
	if i := strings.IndexAny(t, " \t\r\n"); i > 0 {
		t = t[:i]
	}
	return verdict(strings.Trim(t, ".,:;\"'`"))
}

// ------------------------------------------------------------------ cells.

// JudgeCell is one open judgement unit read back out of the pooling template.
type JudgeCell struct {
	Slice       string
	Index       int
	QuerySHA256 string
	Query       string
	BlockID     string
	Title       string
	Excerpt     string
	// Control marks a cell of the mandatory calibration sample. It is set from
	// the SEPARATE key file, never from the template — the template stays blind.
	Control bool
	// Stratum, Weight and CoreQuery are the C3-4a draw facts (design/05a
	// §C3-2-D05-8 a). Like Control they come from a key file and never from the
	// template: a stratum name IS the machine verdict (S1/S2 mean "relevant"),
	// so a cell that carried its own stratum would carry its own answer.
	Stratum   string
	Weight    float64
	CoreQuery bool
}

// Key is the cross-artefact case key.
func (c JudgeCell) Key() string { return CaseKey(c.Slice, c.Index, c.QuerySHA256) }

func (c JudgeCell) cellKey() string { return c.Key() + "/" + c.BlockID }

// ReadTemplateCells parses an UNJUDGED pooling template into cells. It is a
// separate reader from ParseJudgements on purpose: that one refuses an empty
// judgement cell, which is exactly what every row of a fresh template has.
func ReadTemplateCells(path string) ([]JudgeCell, error) {
	queries := map[string]string{}
	var out []JudgeCell
	if err := jsonl.Each(path, func(n int, l templateLine) error {
		k := CaseKey(l.Slice, l.Index, l.QuerySHA256)
		switch l.Kind {
		case "query":
			queries[k] = l.Query
		case "candidate":
			q, ok := queries[k]
			if !ok {
				return fmt.Errorf("%s:%d: candidate row before its query row (case %s)", path, n, k)
			}
			out = append(out, JudgeCell{
				Slice: l.Slice, Index: l.Index, QuerySHA256: l.QuerySHA256, Query: q,
				BlockID: l.BlockID, Title: l.Title, Excerpt: l.Excerpt,
			})
		default:
			return fmt.Errorf("%s:%d: unknown row kind %q", path, n, l.Kind)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkControls flags the mandatory calibration cells from the control key and
// returns how many were marked.
//
// A key id without a template row is an ABORT, not a skipped cell: the control
// sample is the only measurement of the pooling bias, and a sample that quietly
// lost members measures something smaller than it claims (same rule as
// ControlHitRate).
func MarkControls(cells []JudgeCell, key PoolKey) (int, error) {
	if len(key.ControlIDs) == 0 {
		return 0, errors.New("control key holds no entries — the calibration sample cannot be marked")
	}
	pos := make(map[string]int, len(cells))
	for i, c := range cells {
		pos[c.cellKey()] = i
	}
	marked := 0
	for _, k := range sortedKeys(key.ControlIDs) {
		for _, id := range key.ControlIDs[k] {
			i, ok := pos[k+"/"+id]
			if !ok {
				return 0, fmt.Errorf("case %s: control block %s has no template row", k, id)
			}
			cells[i].Control = true
			marked++
		}
	}
	return marked, nil
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- journal.

// JudgeDecision is one journal line: the unit of resume. The journal is
// append-only and written per decision rather than at the end, because the
// resume case IS the crash case — a buffered run that dies loses exactly the
// work the journal exists to protect.
type JudgeDecision struct {
	Slice       string `json:"slice"`
	Index       int    `json:"index"`
	QuerySHA256 string `json:"query_sha256"`
	BlockID     string `json:"block_id"`
	Relevant    bool   `json:"relevant"`
	Control     bool   `json:"control"`
	DecidedAt   string `json:"decided_at"`
}

func (d JudgeDecision) cellKey() string {
	return CaseKey(d.Slice, d.Index, d.QuerySHA256) + "/" + d.BlockID
}

// ReadJudgeJournal loads the decisions of earlier runs. A cell that appears
// twice with DIFFERENT verdicts is an error rather than a last-one-wins: two
// verdicts for one cell mean two runs disagreed, and silently keeping one of
// them would hide it.
func ReadJudgeJournal(path string) (map[string]JudgeDecision, error) {
	out := map[string]JudgeDecision{}
	err := jsonl.Each(path, func(n int, d JudgeDecision) error {
		if prev, dup := out[d.cellKey()]; dup && prev.Relevant != d.Relevant {
			return fmt.Errorf("%s:%d: cell %s judged twice with different verdicts", path, n, d.cellKey())
		}
		out[d.cellKey()] = d
		return nil
	})
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// JudgeStats is what a run reports about itself.
type JudgeStats struct {
	Cells    int
	Resumed  int
	Judged   int
	Relevant int
	Controls int
}

// RunJudge judges every open cell and appends each verdict to the journal.
//
// The provenance gate runs FIRST, before the journal is even created: an
// external endpoint must abort before block excerpts leave the machine, and a
// check placed after the first call would be a report, not a gate (§5 B6).
func RunJudge(ctx context.Context, j Judge, cells []JudgeCell, journalPath string) (JudgeStats, error) {
	st := JudgeStats{Cells: len(cells)}
	g := j.Provenance()
	if err := RequireOnPrem(Backend{
		Name: g.Backend, BaseURL: g.Endpoint, Locality: g.Locality, Trust: g.Trust,
	}); err != nil {
		return st, err
	}
	done, err := ReadJudgeJournal(journalPath)
	if err != nil {
		return st, err
	}
	f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, fileMode)
	if err != nil {
		return st, err
	}
	defer func() { _ = f.Close() }()
	if err := os.Chmod(journalPath, fileMode); err != nil {
		return st, err
	}
	for _, c := range cells {
		if c.Control {
			st.Controls++
		}
		if _, seen := done[c.cellKey()]; seen {
			st.Resumed++
			continue
		}
		rel, jerr := j.Judge(ctx, c.Query, JudgeCandidate{BlockID: c.BlockID, Title: c.Title, Excerpt: c.Excerpt})
		if jerr != nil {
			return st, fmt.Errorf("cell %s: %w", c.cellKey(), jerr)
		}
		d := JudgeDecision{
			Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256, BlockID: c.BlockID,
			Relevant: rel, Control: c.Control, DecidedAt: time.Now().UTC().Format(time.RFC3339),
		}
		if werr := appendDecision(f, d); werr != nil {
			return st, werr
		}
		done[d.cellKey()] = d
		st.Judged++
		if rel {
			st.Relevant++
		}
	}
	return st, nil
}

func appendDecision(f *os.File, d JudgeDecision) error {
	line, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// ------------------------------------------------------------- artefacts.

// RenderJudgedJSONL renders the completed run in exactly the form
// `ctx-goldset ingest -judged` reads. A cell without a decision aborts: a
// partial file would label a subset of the slice without saying so.
func RenderJudgedJSONL(cells []JudgeCell, done map[string]JudgeDecision) ([]byte, error) {
	counts := map[string]int{}
	for _, c := range cells {
		counts[c.Key()]++
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	lastCase := ""
	for _, c := range cells {
		d, ok := done[c.cellKey()]
		if !ok {
			return nil, fmt.Errorf("%w: cell %s carries no judgement", ErrJudgeIncomplete, c.cellKey())
		}
		if c.Key() != lastCase {
			lastCase = c.Key()
			if err := enc.Encode(templateLine{
				Kind: "query", Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
				Query: c.Query, Candidates: counts[c.Key()],
			}); err != nil {
				return nil, err
			}
		}
		v := "0"
		if d.Relevant {
			v = "1"
		}
		if err := enc.Encode(templateLine{
			Kind: "candidate", Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
			BlockID: c.BlockID, Title: c.Title, Excerpt: c.Excerpt, Judgement: &v,
		}); err != nil {
			return nil, err
		}
	}
	return []byte(buf.String()), nil
}

// controlRow is one row of the calibration sheet: the machine verdict and the
// EMPTY column the calibration judge fills. Both verdicts stand side by side
// because kappa is a paired measure — a sheet that carried only one of them
// would have to be joined back against the journal, and a join is a place where
// a pairing can go wrong unnoticed.
type controlRow struct {
	Kind             string     `json:"kind"`
	Slice            string     `json:"slice,omitempty"`
	Index            int        `json:"index,omitempty"`
	QuerySHA256      string     `json:"query_sha256,omitempty"`
	Query            string     `json:"query,omitempty"`
	BlockID          string     `json:"block_id,omitempty"`
	Title            string     `json:"title,omitempty"`
	Excerpt          string     `json:"excerpt,omitempty"`
	LLMJudgement     string     `json:"llm_judgement,omitempty"`
	ControlJudgement *string    `json:"control_judgement,omitempty"`
	Controller       string     `json:"controller,omitempty"`
	Controls         int        `json:"controls,omitempty"`
	Judge            *Generator `json:"judge,omitempty"`
}

// ControllerRole names who fills the control column. It is a constant rather
// than a caller argument because entscheid E2-4 fixed it: the calibration
// judgements are the Haupt-Lead's, goal-directed — not the user's.
const ControllerRole = "Haupt-Lead (Fable), zielgeleitet — Entscheid E2-4"

// RenderControlSheetJSONL emits the calibration sheet: a header with the judge's
// provenance, then one row per control cell.
func RenderControlSheetJSONL(cells []JudgeCell, done map[string]JudgeDecision, gen Generator) ([]byte, error) {
	rows := make([]controlRow, 0, 64)
	for _, c := range cells {
		if !c.Control {
			continue
		}
		d, ok := done[c.cellKey()]
		if !ok {
			return nil, fmt.Errorf("%w: control cell %s carries no machine judgement", ErrJudgeIncomplete, c.cellKey())
		}
		v := "0"
		if d.Relevant {
			v = "1"
		}
		empty := ""
		rows = append(rows, controlRow{
			Kind: "control", Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
			Query: c.Query, BlockID: c.BlockID, Title: c.Title, Excerpt: c.Excerpt,
			LLMJudgement: v, ControlJudgement: &empty,
		})
	}
	if len(rows) == 0 {
		return nil, errors.New("no control cell marked — the calibration sample would be empty")
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	g := gen
	if err := enc.Encode(controlRow{
		Kind: "header", Controller: ControllerRole, Controls: len(rows), Judge: &g,
	}); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return nil, err
		}
	}
	return []byte(buf.String()), nil
}

// WriteOwnerOnly persists tool output at mode 0600. It is the exported face of
// the package's write helper: the judging artefacts carry query texts and block
// excerpts of a private corpus, and the command layer must not be able to reach
// for a plain os.WriteFile that would leave a pre-existing file world-readable.
func WriteOwnerOnly(path string, b []byte) error { return writeOwnerOnly(path, b) }

// JudgePair is one calibrated cell: the two verdicts over the same candidate.
type JudgePair struct {
	Slice   string
	LLM     bool
	Control bool
}

// ParseControlSheet reads a filled calibration sheet. Both columns go through
// the same closed vocabulary as a human template cell, so an unfilled control
// column is ErrUnjudged — never a silent 0 that would inflate agreement.
func ParseControlSheet(path string) ([]JudgePair, error) {
	var out []JudgePair
	if err := jsonl.Each(path, func(n int, r controlRow) error {
		switch r.Kind {
		case "header":
			return nil
		case "control":
		default:
			return fmt.Errorf("%s:%d: unknown row kind %q", path, n, r.Kind)
		}
		llm, lerr := verdict(r.LLMJudgement)
		if lerr != nil {
			return fmt.Errorf("%s:%d: block %s: llm_judgement: %w", path, n, r.BlockID, lerr)
		}
		ctrl := ""
		if r.ControlJudgement != nil {
			ctrl = *r.ControlJudgement
		}
		cv, cerr := verdict(ctrl)
		if cerr != nil {
			return fmt.Errorf("%s:%d: block %s: control_judgement: %w", path, n, r.BlockID, cerr)
		}
		out = append(out, JudgePair{Slice: r.Slice, LLM: llm, Control: cv})
		return nil
	}); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("control sheet holds no calibrated row")
	}
	return out, nil
}

// ------------------------------------------------------------------ kappa.

// KappaResult is Cohen's kappa over one paired set plus the agreement table it
// was computed from — the table is reported because a kappa without its
// marginals cannot be re-derived by a reader.
type KappaResult struct {
	Slice string `json:"slice,omitempty"`
	N     int    `json:"n"`
	// The 2x2 table, rows = machine judge, columns = calibration judge.
	Both        int `json:"both_relevant"`
	LLMOnly     int `json:"llm_only"`
	ControlOnly int `json:"control_only"`
	Neither     int `json:"neither_relevant"`

	Agreement float64 `json:"observed_agreement"`
	Expected  float64 `json:"expected_agreement"`
	Kappa     float64 `json:"kappa"`
	// MarginalP is the two-sided exact McNemar p over the discordant pairs: the
	// question "do the two judges call the same SHARE relevant", which kappa
	// itself does not answer.
	MarginalP float64 `json:"marginal_p"`
	// NotComputable is true when the expected agreement is 1 — two constant,
	// identical raters. Kappa is 0/0 there, and reporting 1.0 would read as
	// perfect calibration where nothing was calibrated at all.
	NotComputable bool `json:"not_computable"`
}

// Kappa computes Cohen's kappa over the paired verdicts.
func Kappa(p []JudgePair) KappaResult {
	r := KappaResult{N: len(p)}
	for _, x := range p {
		switch {
		case x.LLM && x.Control:
			r.Both++
		case x.LLM:
			r.LLMOnly++
		case x.Control:
			r.ControlOnly++
		default:
			r.Neither++
		}
	}
	if r.N == 0 {
		r.NotComputable = true
		return r
	}
	n := float64(r.N)
	r.Agreement = float64(r.Both+r.Neither) / n
	llmRel, ctrlRel := float64(r.Both+r.LLMOnly)/n, float64(r.Both+r.ControlOnly)/n
	r.Expected = llmRel*ctrlRel + (1-llmRel)*(1-ctrlRel)
	if 1-r.Expected <= 1e-12 {
		r.NotComputable = true
		r.MarginalP = mcNemarExact(r.LLMOnly, r.ControlOnly)
		return r
	}
	r.Kappa = (r.Agreement - r.Expected) / (1 - r.Expected)
	r.MarginalP = mcNemarExact(r.LLMOnly, r.ControlOnly)
	return r
}

// KappaBySlice reports kappa per slice and over the whole calibration sample.
func KappaBySlice(p []JudgePair) (map[string]KappaResult, KappaResult) {
	bySlice := map[string][]JudgePair{}
	for _, x := range p {
		bySlice[x.Slice] = append(bySlice[x.Slice], x)
	}
	out := make(map[string]KappaResult, len(bySlice))
	for s, pairs := range bySlice {
		r := Kappa(pairs)
		r.Slice = s
		out[s] = r
	}
	return out, Kappa(p)
}

// mcNemarExact is the two-sided exact McNemar test over the discordant pairs:
// under "the two judges call the same share relevant", the b discordances of
// one direction are Binomial(b+c, 1/2). Exact rather than the chi-square
// approximation because a calibration sample is small by construction and the
// approximation is unreliable exactly there.
func mcNemarExact(b, c int) float64 {
	nd := b + c
	if nd == 0 {
		return 1
	}
	k := b
	if c < k {
		k = c
	}
	sum := 0.0
	for i := 0; i <= k; i++ {
		sum += math.Exp(logChoose(nd, i) - float64(nd)*math.Ln2)
	}
	if p := 2 * sum; p < 1 {
		return p
	}
	return 1
}

func logChoose(n, k int) float64 {
	ln, _ := math.Lgamma(float64(n + 1))
	lk, _ := math.Lgamma(float64(k + 1))
	lnk, _ := math.Lgamma(float64(n - k + 1))
	return ln - lk - lnk
}

// ------------------------------------------------------------------ gates.

// The two verdicts a gate can carry after calibration. The vocabulary is closed
// to these two on purpose: this tool decides whether a gate MAY be decided on
// the machine verdicts, never whether it passed or failed.
const (
	GateCarries   = "trägt"
	GateUndecided = "nicht entschieden"
)

// marginalAlpha is the significance level of the flip check. It is the
// campaign's standing convention (the paired 95 % intervals of G-NOISE and of
// `ctx-armsweep compare`), not a new number invented here — the kappa threshold
// itself has deliberately NO default and must be named by the caller.
const marginalAlpha = 0.05

// JudgeGate is a downstream decision that rests on judged labels.
type JudgeGate struct {
	Name    string   `json:"name"`
	Slices  []string `json:"slices"`
	Decides string   `json:"decides"`
}

// judgeGates are the three decisions entscheid E2-4 unlocks. They are listed
// here rather than derived, because "which gate rests on which slice" is a
// project decision and a derivation would hide the day it changed.
var judgeGates = []JudgeGate{
	{
		Name: "G-REAL-MDE", Slices: []string{SliceReal},
		Decides: "die auflösbare Effektgröße auf G-REAL (§4.4b) — ohne Gold ist sie nicht berechenbar, nicht bloß groß",
	},
	{
		Name: "Splits", Slices: []string{SliceReal},
		Decides: "die local/global-Aufteilung von G-REAL als eigene Slice-Zeilen (X-W0)",
	},
	{
		Name: "B-Substanz", Slices: []string{SliceGlob},
		Decides: "ob die Katalog-Ebene auf G-GLOB Substanz hat (H0-B1/B3/B4/B5)",
	},
}

// JudgeGates returns the registry.
func JudgeGates() []JudgeGate { return append([]JudgeGate(nil), judgeGates...) }

// GateVerdict is one line of the Kipp-Report.
type GateVerdict struct {
	Name    string                 `json:"name"`
	Slices  []string               `json:"slices"`
	Decides string                 `json:"decides"`
	Verdict string                 `json:"verdict"`
	Reasons []string               `json:"reasons,omitempty"`
	Kappa   map[string]KappaResult `json:"kappa"`
	// GoldReach and Notes are the C3-4a additions (design/05a §C3-2-D05-6).
	// They stay empty on the E2-4 path, which is what keeps the older report
	// byte-identical: the kappa threshold decided a GATE there, and it decides
	// the REACH of the judge labels here — two different statements that must
	// not share one field.
	GoldReach string   `json:"gold_reach,omitempty"`
	Notes     []string `json:"notes,omitempty"`
}

// JudgeGateReport is the Kipp-Report: for every gate, whether the machine
// verdicts of the slices it rests on are calibrated well enough to carry it.
//
// Three conditions leave a gate "nicht entschieden", and each is fail-closed:
// no calibrated pair at all, kappa below the stated threshold, or a marginal
// shift between the two judges — the flip case of design/05 §4.5 (3). Anything
// else carries. The gate is never reported as passed or failed here; that
// decision belongs to the measurement the gate guards.
func JudgeGateReport(bySlice map[string]KappaResult, kappaMin float64) []GateVerdict {
	out := make([]GateVerdict, 0, len(judgeGates))
	for _, g := range judgeGates {
		v := GateVerdict{
			Name: g.Name, Slices: g.Slices, Decides: g.Decides,
			Verdict: GateCarries, Kappa: map[string]KappaResult{},
		}
		for _, s := range g.Slices {
			r, ok := bySlice[s]
			v.Kappa[s] = r
			switch {
			case !ok || r.N == 0:
				v.Reasons = append(v.Reasons,
					fmt.Sprintf("%s: keine kalibrierte Zelle — κ nicht berechenbar", s))
			case r.NotComputable:
				v.Reasons = append(v.Reasons,
					fmt.Sprintf("%s: κ nicht berechenbar (erwartete Übereinstimmung = 1 bei n=%d)", s, r.N))
			case r.Kappa < kappaMin:
				v.Reasons = append(v.Reasons,
					fmt.Sprintf("%s: κ=%.4f unter der vorab genannten Schranke %.4f (n=%d)", s, r.Kappa, kappaMin, r.N))
			case r.MarginalP < marginalAlpha:
				v.Reasons = append(v.Reasons,
					fmt.Sprintf("%s: Kipp — die Randverteilungen der beiden Urteiler unterscheiden sich "+
						"(McNemar exakt p=%.5f < %.2f; %d nur maschinell, %d nur kalibriert)",
						s, r.MarginalP, marginalAlpha, r.LLMOnly, r.ControlOnly))
			}
		}
		if len(v.Reasons) > 0 {
			v.Verdict = GateUndecided
		}
		out = append(out, v)
	}
	return out
}

// RenderKappaReport is the human form of the Kipp-Report.
func RenderKappaReport(kappaMin float64, bySlice map[string]KappaResult, overall KappaResult, gates []GateVerdict) string {
	var b strings.Builder
	b.WriteString("# Kalibrierung der Urteile — Cohens κ und Kipp-Report\n\n")
	fmt.Fprintf(&b, "Vorab genannte κ-Schranke: **%.4f**. Kipp-Prüfung: exakter McNemar-Test, α = %.2f.\n", kappaMin, marginalAlpha)
	fmt.Fprintf(&b, "Zweiter Urteiler: %s.\n\n", ControllerRole)
	b.WriteString("## κ je Slice\n\n")
	b.WriteString("| Slice | n | Übereinstimmung | erwartet | κ | nur maschinell | nur kalibriert | McNemar p |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	names := make([]string, 0, len(bySlice))
	for s := range bySlice {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		b.WriteString(kappaRow(s, bySlice[s]))
	}
	b.WriteString(kappaRow("gesamt", overall))
	b.WriteString("\n## Kipp-Report je Gate\n\n")
	for _, g := range gates {
		fmt.Fprintf(&b, "### %s — %s\n\n", g.Name, g.Verdict)
		fmt.Fprintf(&b, "Entscheidet: %s\nRuht auf: %s\n", g.Decides, strings.Join(g.Slices, ", "))
		for _, r := range g.Reasons {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}
	b.WriteString("Ein Gate mit dem Vermerk „" + GateUndecided + "“ ist damit weder als erreicht noch als\n" +
		"verfehlt ausgewiesen: die maschinellen Urteile tragen die Entscheidung nicht, und die\n" +
		"Entscheidung bleibt offen, bis eine tragfähige Kalibrierung vorliegt (D-05 §4.5 (3)).\n")
	return b.String()
}

func kappaRow(name string, r KappaResult) string {
	k := fmt.Sprintf("%.4f", r.Kappa)
	if r.NotComputable {
		k = "n/a"
	}
	return fmt.Sprintf("| %s | %d | %.4f | %.4f | %s | %d | %d | %.5f |\n",
		name, r.N, r.Agreement, r.Expected, k, r.LLMOnly, r.ControlOnly, r.MarginalP)
}
