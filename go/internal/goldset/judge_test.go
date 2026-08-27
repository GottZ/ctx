package goldset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures of pool_test.go are reused: a judging run is a second pass over
// exactly the artefacts the pooling pass produced, and a second set of fixtures
// would let the two drift apart.

// stubJudge is the Judge seam under test. No model, no socket: wave C2-6 builds
// the tool, the judging run itself is C2-6b.
type stubJudge struct {
	calls    int
	failAt   int // fail the (failAt+1)-th call; 0 disables
	prov     Generator
	relevant func(c JudgeCandidate) bool
}

func (s *stubJudge) Judge(_ context.Context, _ string, c JudgeCandidate) (bool, error) {
	if s.failAt > 0 && s.calls >= s.failAt {
		return false, errors.New("stub: abgebrochen")
	}
	s.calls++
	return s.relevant(c), nil
}

func (s *stubJudge) Provenance() Generator { return s.prov }

// onPremProvenance is what a judge over the sanctioned chain declares.
func onPremProvenance() Generator {
	return Generator{
		Backend: "spark-chat", Model: "qwen38-27b",
		Endpoint: "http://10.13.37.22:30000/v1/chat/completions",
		Locality: "lan", Trust: "full-trust", PromptSHA256: JudgePromptSHA256(),
	}
}

// pooledIsRelevant is the deterministic verdict rule of the stub: the pooled
// candidates (prefix p) count as relevant, the uniform control draws (prefix k)
// do not.
func pooledIsRelevant(c JudgeCandidate) bool { return strings.HasPrefix(c.BlockID, "p") }

// judgeFixture renders the pooling template to disk and reads it back as cells
// with the control sample marked — the real artefact path, not a hand-built
// slice (W10).
func judgeFixture(t *testing.T, controls int) (dir string, cells []JudgeCell, key PoolKey) {
	t.Helper()
	pooled, key := buildFixture(t, fixtureSeed, controls)
	dir = t.TempDir()
	tpl := filepath.Join(dir, "judge-fixture.jsonl")
	body, err := RenderTemplateJSONL(pooled, fixtureBlocks(), 200)
	if err != nil {
		t.Fatalf("render template: %v", err)
	}
	if werr := os.WriteFile(tpl, body, 0o600); werr != nil {
		t.Fatalf("write template: %v", werr)
	}
	cells, err = ReadTemplateCells(tpl)
	if err != nil {
		t.Fatalf("ReadTemplateCells: %v", err)
	}
	marked, err := MarkControls(cells, key)
	if err != nil {
		t.Fatalf("MarkControls: %v", err)
	}
	if want := len(pooled) * controls; marked != want {
		t.Fatalf("marked %d control cells, want %d", marked, want)
	}
	return dir, cells, key
}

// --- Gate: the run feeds `ctx-goldset ingest -judged` (W10 — the real path).

func TestJudgedTemplateFeedsTheIngestPath(t *testing.T) {
	t.Parallel()
	dir, cells, _ := judgeFixture(t, 3)
	journal := filepath.Join(dir, "judged-fixture-journal.jsonl")
	st, err := RunJudge(context.Background(), &stubJudge{prov: onPremProvenance(), relevant: pooledIsRelevant}, cells, journal)
	if err != nil {
		t.Fatalf("RunJudge: %v", err)
	}
	if st.Judged != len(cells) || st.Resumed != 0 {
		t.Fatalf("judged=%d resumed=%d, want %d/0", st.Judged, st.Resumed, len(cells))
	}
	done, err := ReadJudgeJournal(journal)
	if err != nil {
		t.Fatalf("ReadJudgeJournal: %v", err)
	}
	filled, err := RenderJudgedJSONL(cells, done)
	if err != nil {
		t.Fatalf("RenderJudgedJSONL: %v", err)
	}
	out := filepath.Join(dir, "judged-fixture.jsonl")
	if werr := os.WriteFile(out, filled, 0o600); werr != nil {
		t.Fatalf("write: %v", werr)
	}

	judged, err := ParseJudgements(out)
	if err != nil {
		t.Fatalf("ParseJudgements over the judged template: %v", err)
	}
	labelled, stats, err := ApplyLabels(fixtureCases(), judged)
	if err != nil {
		t.Fatalf("ApplyLabels: %v", err)
	}
	if stats.Cases != 3 || stats.Labelled != 3 {
		t.Fatalf("cases=%d labelled=%d, want 3/3", stats.Cases, stats.Labelled)
	}
	for _, c := range labelled {
		if len(c.GoldIDs) == 0 {
			t.Fatalf("case %s carries no gold id after the judging run", c.Key())
		}
		for _, id := range c.GoldIDs {
			if !strings.HasPrefix(id, "p") {
				t.Errorf("case %s labelled control draw %s as gold", c.Key(), id)
			}
		}
	}
	if _, _, _, err := ControlHitRate(judged, mustKey(t, dir)); err != nil {
		t.Fatalf("ControlHitRate over the machine judgements: %v", err)
	}
}

// mustKey rebuilds the control key of the fixture for the hit-rate check.
func mustKey(t *testing.T, _ string) PoolKey {
	t.Helper()
	_, key := buildFixture(t, fixtureSeed, 3)
	return key
}

// --- Gate 4: resume. An aborted run continues, and no cell is judged twice.

func TestJudgeRunResumesWithoutDoubleJudgement(t *testing.T) {
	t.Parallel()
	dir, cells, _ := judgeFixture(t, 3)
	journal := filepath.Join(dir, "resume-journal.jsonl")

	const k = 7
	first := &stubJudge{prov: onPremProvenance(), relevant: pooledIsRelevant, failAt: k}
	st1, err := RunJudge(context.Background(), first, cells, journal)
	if err == nil {
		t.Fatal("aborted run returned no error")
	}
	if st1.Judged != k {
		t.Fatalf("first run persisted %d judgements, want %d", st1.Judged, k)
	}

	second := &stubJudge{prov: onPremProvenance(), relevant: pooledIsRelevant}
	st2, err := RunJudge(context.Background(), second, cells, journal)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if st2.Resumed != k {
		t.Fatalf("resumed run re-read %d decisions, want %d", st2.Resumed, k)
	}
	if want := len(cells) - k; second.calls != want {
		t.Fatalf("resumed run made %d judge calls, want exactly the %d remaining cells", second.calls, want)
	}
	done, err := ReadJudgeJournal(journal)
	if err != nil {
		t.Fatalf("ReadJudgeJournal: %v", err)
	}
	if len(done) != len(cells) {
		t.Fatalf("journal holds %d cells, want %d", len(done), len(cells))
	}
	// A double judgement would show up as a second line for a key, so count the
	// lines rather than trusting the map.
	body, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	lines := 0
	for _, l := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if lines != len(cells) {
		t.Fatalf("journal holds %d lines for %d cells — a cell was judged twice", lines, len(cells))
	}
}

// --- Gate 3: provenance. A non-on-prem endpoint aborts BEFORE the first cell.

func TestJudgeRunAbortsOnNonOnPremEndpoint(t *testing.T) {
	t.Parallel()
	dir, cells, _ := judgeFixture(t, 3)
	journal := filepath.Join(dir, "external-journal.jsonl")
	external := onPremProvenance()
	external.Endpoint = "https://api.example.com/v1/chat/completions"

	j := &stubJudge{prov: external, relevant: pooledIsRelevant}
	if _, err := RunJudge(context.Background(), j, cells, journal); !errors.Is(err, ErrNotOnPrem) {
		t.Fatalf("RunJudge over an external endpoint returned %v, want ErrNotOnPrem", err)
	}
	if j.calls != 0 {
		t.Errorf("judge was called %d times before the abort — the gate must run before the first cell", j.calls)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Errorf("journal exists after an aborted run (%v) — the abort came too late", err)
	}
}

// TestJudgeRunAbortsOnLyingLocality pins the second axis of §5 B6: a row that
// declares lan while pointing at a public host is still external.
func TestJudgeRunAbortsOnLyingLocality(t *testing.T) {
	t.Parallel()
	dir, cells, _ := judgeFixture(t, 3)
	prov := onPremProvenance()
	prov.Endpoint = "https://openrouter.ai/api/v1/chat/completions"
	prov.Locality = "lan"
	j := &stubJudge{prov: prov, relevant: pooledIsRelevant}
	if _, err := RunJudge(context.Background(), j, cells, filepath.Join(dir, "liar-journal.jsonl")); !errors.Is(err, ErrNotOnPrem) {
		t.Fatalf("a public host declared as lan was accepted: %v", err)
	}
}

// TestNewChatJudgeRefusesExternalBackend is the constructor half of the same
// rule — the client cannot even be built for an external row.
func TestNewChatJudgeRefusesExternalBackend(t *testing.T) {
	t.Parallel()
	be := Backend{
		Name: "openrouter", BaseURL: "https://openrouter.ai/api", Locality: "external",
		Trust: "no-credentials", ModelMap: []byte(`{"default":{"model":"x"}}`),
	}
	if _, err := NewChatJudge(be, "", 0); !errors.Is(err, ErrNotOnPrem) {
		t.Fatalf("NewChatJudge accepted an external backend: %v", err)
	}
}

// --- Gate: the calibration sample is marked and carries both columns.

func TestControlSheetCarriesBothVerdictColumns(t *testing.T) {
	t.Parallel()
	dir, cells, _ := judgeFixture(t, 3)
	journal := filepath.Join(dir, "sheet-journal.jsonl")
	if _, err := RunJudge(context.Background(), &stubJudge{prov: onPremProvenance(), relevant: pooledIsRelevant}, cells, journal); err != nil {
		t.Fatalf("RunJudge: %v", err)
	}
	done, err := ReadJudgeJournal(journal)
	if err != nil {
		t.Fatalf("ReadJudgeJournal: %v", err)
	}
	sheet, err := RenderControlSheetJSONL(cells, done, onPremProvenance())
	if err != nil {
		t.Fatalf("RenderControlSheetJSONL: %v", err)
	}
	body := string(sheet)
	rows := 0
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, `"kind":"control"`) {
			rows++
		}
	}
	if rows != 9 {
		t.Fatalf("control sheet holds %d rows, want 9 (3 cases x 3 controls)", rows)
	}
	if !strings.Contains(body, `"control_judgement":""`) {
		t.Error("control sheet carries no empty controller column to fill")
	}
	if !strings.Contains(body, `"llm_judgement"`) {
		t.Error("control sheet carries no LLM verdict beside the controller column")
	}
	if !strings.Contains(body, "Haupt-Lead") {
		t.Error("control sheet does not name the calibration judge (E2-4: the Haupt-Lead, not the user)")
	}

	// An unfilled controller column is an ERROR, never a silent 0.
	p := filepath.Join(dir, "controls.jsonl")
	if werr := os.WriteFile(p, sheet, 0o600); werr != nil {
		t.Fatalf("write sheet: %v", werr)
	}
	if _, err := ParseControlSheet(p); !errors.Is(err, ErrUnjudged) {
		t.Fatalf("an unfilled control sheet parsed without ErrUnjudged: %v", err)
	}
}

// TestMarkControlsRefusesAMissingControlRow keeps the mandatory-control rule
// mechanical: a key id without a template row is an abort, not a skipped cell.
func TestMarkControlsRefusesAMissingControlRow(t *testing.T) {
	t.Parallel()
	_, cells, key := judgeFixture(t, 3)
	for k := range key.ControlIDs {
		key.ControlIDs[k] = append(key.ControlIDs[k], "k99")
		break
	}
	if _, err := MarkControls(cells, key); err == nil {
		t.Fatal("MarkControls accepted a control id that has no template row")
	}
}

// --- kappa itself.

// fillSheet builds paired verdicts directly: a, b, c, d are the four cells of
// the agreement table (LLM x controller).
func pairs(slice string, both, llmOnly, controlOnly, neither int) []JudgePair {
	var out []JudgePair
	add := func(n int, l, c bool) {
		for i := 0; i < n; i++ {
			out = append(out, JudgePair{Slice: slice, LLM: l, Control: c})
		}
	}
	add(both, true, true)
	add(llmOnly, true, false)
	add(controlOnly, false, true)
	add(neither, false, false)
	return out
}

func TestKappaMatchesTheHandComputedValue(t *testing.T) {
	t.Parallel()
	// a=20 b=5 c=5 d=70: po=0.90, pe=0.25*0.25+0.75*0.75=0.625, kappa=0.7333…
	got := Kappa(pairs(SliceReal, 20, 5, 5, 70))
	if got.N != 100 {
		t.Fatalf("N=%d, want 100", got.N)
	}
	if diff := got.Kappa - 0.733333; diff > 1e-5 || diff < -1e-5 {
		t.Fatalf("kappa=%.6f, want 0.733333", got.Kappa)
	}
	if diff := got.Agreement - 0.90; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("agreement=%.6f, want 0.9", got.Agreement)
	}
}

func TestKappaIsNotComputableWhenBothRatersAreConstant(t *testing.T) {
	t.Parallel()
	got := Kappa(pairs(SliceReal, 0, 0, 0, 40))
	if !got.NotComputable {
		t.Fatalf("kappa over a constant table reported as computable (%.4f)", got.Kappa)
	}
}

// --- Gate 2: the verbindliche negative probe of D-05.

func TestLowKappaLeavesTheGateUndecided(t *testing.T) {
	t.Parallel()
	// a=30 b=25 c=25 d=20: po=0.50, pe=0.55*0.55+0.45*0.45=0.505, kappa≈-0.0101
	low := pairs(SliceReal, 30, 25, 25, 20)
	bySlice, _ := KappaBySlice(low)
	if k := bySlice[SliceReal].Kappa; k >= 0.6 {
		t.Fatalf("fixture kappa=%.4f is not below the threshold — the probe would not probe", k)
	}
	report := JudgeGateReport(bySlice, 0.6)
	for _, g := range report {
		if !contains(g.Slices, SliceReal) {
			continue
		}
		if g.Verdict != GateUndecided {
			t.Fatalf("gate %q reads %q at kappa=%.4f below the threshold 0.6 — it must be %q",
				g.Name, g.Verdict, bySlice[SliceReal].Kappa, GateUndecided)
		}
		if len(g.Reasons) == 0 {
			t.Errorf("gate %q is undecided without naming a reason", g.Name)
		}
	}

	// The discriminating half: the same pairs above the threshold carry.
	high := pairs(SliceReal, 45, 2, 3, 50)
	byHigh, _ := KappaBySlice(high)
	if k := byHigh[SliceReal].Kappa; k < 0.6 {
		t.Fatalf("control fixture kappa=%.4f is not above the threshold", k)
	}
	for _, g := range JudgeGateReport(byHigh, 0.6) {
		if contains(g.Slices, SliceReal) && g.Verdict != GateCarries {
			t.Fatalf("gate %q reads %q at kappa=%.4f above the threshold — the report never carries anything",
				g.Name, g.Verdict, byHigh[SliceReal].Kappa)
		}
	}
}

// TestGateFlipLeavesTheGateUndecidedDespiteHighKappa is the second half of the
// D-05 rule: "kippt ein Gate zwischen maschinell- und menschlich-geurteilter
// Rechnung, gilt es als nicht entschieden" — a systematic marginal shift is a
// flip even when the raters otherwise agree well.
func TestGateFlipLeavesTheGateUndecidedDespiteHighKappa(t *testing.T) {
	t.Parallel()
	shifted := pairs(SliceReal, 40, 0, 15, 45)
	bySlice, _ := KappaBySlice(shifted)
	k := bySlice[SliceReal]
	if k.Kappa < 0.6 {
		t.Fatalf("fixture kappa=%.4f is below the threshold — the probe would not isolate the flip", k.Kappa)
	}
	if k.MarginalP >= 0.05 {
		t.Fatalf("fixture marginal p=%.6f is not significant — the probe would not isolate the flip", k.MarginalP)
	}
	for _, g := range JudgeGateReport(bySlice, 0.6) {
		if contains(g.Slices, SliceReal) && g.Verdict != GateUndecided {
			t.Fatalf("gate %q reads %q despite a marginal shift (p=%.6f)", g.Name, g.Verdict, k.MarginalP)
		}
	}
}

func TestGateWithoutPairsIsUndecidedNotCarried(t *testing.T) {
	t.Parallel()
	// Only G-REAL was calibrated; the G-GLOB gate has nothing to rest on.
	bySlice, _ := KappaBySlice(pairs(SliceReal, 45, 2, 3, 50))
	found := false
	for _, g := range JudgeGateReport(bySlice, 0.6) {
		if !contains(g.Slices, SliceGlob) {
			continue
		}
		found = true
		if g.Verdict != GateUndecided {
			t.Fatalf("gate %q reads %q without a single calibrated pair", g.Name, g.Verdict)
		}
	}
	if !found {
		t.Fatal("no gate rests on G-GLOB — the B-Substanz decision lost its slice")
	}
}

func TestJudgeGatesCoverTheThreeUnlockedDecisions(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"G-REAL-MDE": false, "Splits": false, "B-Substanz": false}
	for _, g := range JudgeGates() {
		if _, ok := want[g.Name]; !ok {
			t.Errorf("unexpected gate %q in the registry", g.Name)
			continue
		}
		want[g.Name] = true
		if len(g.Slices) == 0 || g.Decides == "" {
			t.Errorf("gate %q names no slice or no decision", g.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("gate %q missing — E2-4 unlocks exactly these three", name)
		}
	}
}

// TestKappaReportNamesTheThresholdAndNeverPasses pins the reporting contract:
// the threshold is stated, and a gate verdict is "trägt" or "nicht entschieden"
// — never a pass and never a fail.
func TestKappaReportNamesTheThresholdAndNeverPasses(t *testing.T) {
	t.Parallel()
	bySlice, overall := KappaBySlice(pairs(SliceReal, 30, 25, 25, 20))
	gates := JudgeGateReport(bySlice, 0.6)
	body := RenderKappaReport(0.6, bySlice, overall, gates)
	if !strings.Contains(body, "0.6") && !strings.Contains(body, "0,6") {
		t.Error("report does not state the threshold it applied")
	}
	if !strings.Contains(body, GateUndecided) {
		t.Error("report does not carry the 'nicht entschieden' verdict of its own gates")
	}
	for _, g := range gates {
		if g.Verdict != GateCarries && g.Verdict != GateUndecided {
			t.Errorf("gate %q carries verdict %q — the vocabulary is closed to two values", g.Name, g.Verdict)
		}
	}
}

func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
