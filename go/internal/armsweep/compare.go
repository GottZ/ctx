package armsweep

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/GottZ/ctx/internal/evalscore"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// The conditional comparison (design/05 §4.3, wave M-W3d).
//
// Why this is a subcommand of its own and not a second flag on `score`:
// `score`'s -dump-b is declared as the V0' REPLICATE — "the noise floor, not a
// variant" (configs.go:58), "the replicate is the noise floor, never a win
// candidate" (report.go:321). A conditional comparison ("dump A without the
// shadow type, dump B with it") uses the same paired mechanics but INVERTS the
// interpretation: at V0/V0' a difference is noise, at base/cond it is the
// signal. Carrying both roles on one flag would take the report's own
// definition of noise away from it — the textbook way to corrupt a noise gate.
//
// So `compare` consumes THREE inputs: the two conditions AND the replicate pair
// of the same campaign, and it refuses to say anything at all without the
// latter.

// CompareVersion is the compare report's schema generation.
const CompareVersion = 1

// DisplacementCut is the rank depth the displacement table is read at. It is
// RecallCut deliberately: the same five places ΔRecall@5 is scored on, so a
// displacement row and a recall row in one report talk about one window.
const DisplacementCut = RecallCut

// MDEThresholdNDCG is the resolution line of §4.4b: a slice whose minimal
// detectable effect on ΔnDCG@10 exceeds two percentage points cannot resolve an
// effect of the size the literature reports for hierarchical layers (RAPTOR
// +2,0/+2,7/+1,31 pp). Such a slice is declared unresolvable BEFORE the effect
// is read, and a gate that goes green on it is a chance finding.
const MDEThresholdNDCG = 0.02

// semanticModeANN is the selector mode under which ctx_rrf_arms sets the two
// ANN GUCs (142:216-220). Everything else runs the exact path, which sets
// neither — which is why the mode is part of the GUC congruence below.
const semanticModeANN = "ann"

// gucUnset is what the report prints for a GUC the measured statement never
// set. Not the empty string: "unset" is a value the congruence check compares
// like any other, and an empty cell in a report reads as missing data.
const gucUnset = "unset"

// maxNamedUnpaired caps the case keys a report lists individually. The count is
// always exact; the list exists to make a small mismatch diagnosable, not to
// reproduce a dump.
const maxNamedUnpaired = 25

// ErrStampIncongruent is the exit-4 class of `compare`: the artefacts are
// well-formed, the RUN was clean, and the dump set is rejected because its
// stamps do not describe one campaign (design/05 §4.3, F-32).
//
// Its own class, next to ErrGateRefused: a scheduler retries a gate refusal
// after fixing the instrument, but re-running an incongruent dump set produces
// the same incongruence — that needs a new measurement, not a retry.
var ErrStampIncongruent = errors.New("Dump-Satz verworfen: inkongruente Stempel")

// DumpRef is one measured artefact as `compare` consumes it: where the records
// are, and what the run said about itself.
type DumpRef struct {
	Role  string    `json:"role"`
	Path  string    `json:"path"`
	Stamp DumpStamp `json:"-"`
}

// CompareInput is the whole conditional comparison.
type CompareInput struct {
	Base DumpRef
	Cond DumpRef
	// NoisePair is the V0/V0' replicate pair OF THE SAME CAMPAIGN. Exactly two
	// entries; anything else is gate (a) and refuses.
	NoisePair []DumpRef

	// RegimeSplit carries the X-W0 labels that cut G-REAL into its two regime
	// rows (wave X-W0b). Inactive by default; an inactive split leaves the
	// report exactly as it was before the wave.
	RegimeSplit RegimeSplit

	Seed        int64
	GitRevision string
	GoldStamp   goldset.Stamp
}

// CompareEnv is the provenance block. It carries the congruent values ONCE —
// they are only reported after the congruence check passed, so a single value
// is the honest rendering.
type CompareEnv struct {
	Tool                string             `json:"tool"`
	GitRevision         string             `json:"git_revision"`
	Seed                int64              `json:"seed"`
	SampleSeed          int64              `json:"gold_sample_seed"`
	SplitSeed           int64              `json:"gold_split_seed"`
	GoldSHA256          string             `json:"gold_sha256"`
	GoldStampSHA256     string             `json:"gold_stamp_sha256"`
	SliceFiles          []SliceDigest      `json:"slice_files"`
	Generator           *goldset.Generator `json:"generator,omitempty"`
	CorpusMaxCreatedAt  string             `json:"corpus_max_created_at"`
	MigrationsMax       int                `json:"migrations_max"`
	PostFusionStages    map[string]any     `json:"post_fusion_stages"`
	CampaignPinRunID    string             `json:"campaign_pin_run_id"`
	InstanceKind        string             `json:"instance_kind"`
	ShadowTypes         []string           `json:"shadow_types,omitempty"`
	AllowLiveInstance   bool               `json:"allow_live_instance,omitempty"`
	AllowOutsideGoldset bool               `json:"allow_outside_goldset,omitempty"`
	// RegimeLabels is the X-W0 label file the G-REAL strata were cut from
	// (wave X-W0b), absent from the bytes when no split was asked for.
	RegimeLabels *RegimeStamp `json:"regime_labels,omitempty"`
	// GUCs are the three ANN determinism knobs of §4.4b/F-23, as the campaign
	// ran them: hnsw.ef_search off the instance stamp, iterative_scan and
	// max_scan_tuples off the per-case selector state.
	GUCs  []GUCValue       `json:"gucs"`
	Dumps []DumpProvenance `json:"dumps"`
}

// GUCValue is one pinned knob and where its value was read.
type GUCValue struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// MDEReport is the §4.4b resolution row of one slice: what the smallest
// difference is that this slice can show at all.
type MDEReport struct {
	Slice      string  `json:"slice"`
	N          int     `json:"n"`
	NoiseCILo  float64 `json:"noise_ci_lo"`
	NoiseCIHi  float64 `json:"noise_ci_hi"`
	MDE        float64 `json:"mde_ndcg_10"`
	Threshold  float64 `json:"threshold"`
	Resolvable bool    `json:"resolvable"`
	Note       string  `json:"note,omitempty"`
}

// CompareEffect is the condition against the baseline on one slice.
type CompareEffect struct {
	Slice        string  `json:"slice"`
	N            int     `json:"n"`
	Level        float64 `json:"level"`
	Unlabelled   bool    `json:"unlabelled"`
	DeltaNDCG10  float64 `json:"delta_ndcg_10"`
	NDCGCILo     float64 `json:"ndcg_ci_lo"`
	NDCGCIHi     float64 `json:"ndcg_ci_hi"`
	DeltaRecall5 float64 `json:"delta_recall_5"`
	RecallCILo   float64 `json:"recall_ci_lo"`
	RecallCIHi   float64 `json:"recall_ci_hi"`
	DeltaMRR10   float64 `json:"delta_mrr_10"`
	MRRCILo      float64 `json:"mrr_ci_lo"`
	MRRCIHi      float64 `json:"mrr_ci_hi"`

	McNemar     evalscore.McNemarResult `json:"mcnemar_hit_5"`
	Discordance float64                 `json:"discordance_hit_5"`
	// NoiseDiscordance is the same figure for the replicate pair on this slice.
	NoiseDiscordance float64 `json:"noise_discordance_hit_5"`
	// Separable is the F-32 rule: the condition must move MORE cases than the
	// instrument moves on its own, or the effect is not separable from noise.
	Separable bool `json:"separable"`
	// AboveMDE compares the measured effect against the slice's own resolution.
	AboveMDE bool `json:"above_mde"`
	// Readable is the conjunction the report is read on: a CI excluding 0, an
	// effect above the MDE, and separability from the noise floor.
	Readable bool     `json:"readable"`
	Reasons  []string `json:"reasons,omitempty"`
}

// TypeCount is one block type and how often it appeared in a displacement role.
type TypeCount struct {
	TypeName string `json:"type_name"`
	N        int    `json:"n"`
}

// DisplacementRow is the table §4.3 calls central for this axis: what the
// condition's blocks push out of the top five, and whether anything labelled
// relevant was among it.
type DisplacementRow struct {
	Slice                 string `json:"slice"`
	RolloutCriterion      bool   `json:"rollout_criterion"`
	Cases                 int    `json:"cases"`
	CasesWithDisplacement int    `json:"cases_with_displacement"`
	Cut                   int    `json:"cut"`
	ShadowInTopK          int    `json:"shadow_in_top_k"`
	ShadowAtRank1         int    `json:"shadow_at_rank_1"`
	Displaced             int    `json:"displaced"`
	MinPerCase            int    `json:"min_per_case"`
	MaxPerCase            int    `json:"max_per_case"`
	// DisplacedLabelled counts displaced blocks that carried a relevance
	// judgement. Without labels on the slice the column stays 0 and
	// LabelsAvailable says why — §4.3 requires the report to say it rather than
	// print a zero that reads like a measurement.
	DisplacedLabelled int         `json:"displaced_labelled"`
	LabelsAvailable   bool        `json:"labels_available"`
	DisplacedByType   []TypeCount `json:"displaced_by_type"`
	EntrantsByType    []TypeCount `json:"entrants_by_type"`
	Note              string      `json:"note,omitempty"`
}

// CompareBody is the deterministic half of a compare report. Nothing volatile
// lives here — the generation timestamp sits in the header line, so two runs
// over one dump set produce the same bytes (gate (d)).
type CompareBody struct {
	Version int        `json:"version"`
	Env     CompareEnv `json:"env"`
	// Refused is true when G-NOISE did not pass. The tables below are still
	// written: suppressing them would hide the evidence an operator needs to
	// find the determinism source.
	Refused        bool              `json:"refused"`
	RefusalReasons []string          `json:"refusal_reasons,omitempty"`
	Slices         []SliceProfile    `json:"slices"`
	Noise          []NoiseGate       `json:"noise_gate"`
	MDE            []MDEReport       `json:"mde"`
	Effects        []CompareEffect   `json:"effects"`
	Displacement   []DisplacementRow `json:"displacement"`
	Paired         int               `json:"paired_cases"`
	UnpairedTotal  int               `json:"unpaired_cases"`
	Unpaired       []string          `json:"unpaired,omitempty"`
	Notes          []string          `json:"notes"`
}

// Compare runs the whole conditional comparison: congruence, paired streaming,
// noise gate, effects, displacement and resolution.
//
// It returns the body ALONGSIDE a refusal error whenever the refusal is a
// verdict about the measurement (gate (b)) — the caller writes the artefact and
// then propagates the exit code, the same shape `dump` uses for an aborted run
// (cmd/ctx-armsweep/commands.go:158-180). A congruence failure returns no body:
// there is nothing measured to report.
func Compare(in CompareInput) (CompareBody, error) {
	if len(in.NoisePair) != 2 {
		return CompareBody{}, fmt.Errorf(
			"%w: -noise-pair fehlt (genau zwei Dumps, das V0/V0'-Paar derselben Kampagne) — ohne gemessenen Rauschboden ist eine Differenz zwischen den Bedingungen keine Aussage (§4.3)",
			ErrGateRefused)
	}
	refs := compareRefs(in)
	if err := checkStampCongruence(refs); err != nil {
		return CompareBody{}, err
	}
	shadow := shadowTypeSet(in.Cond.Stamp.ShadowTypes)
	acc, err := streamPairs(refs, shadow, in.RegimeSplit)
	if err != nil {
		return CompareBody{}, err
	}
	body := buildCompareBody(in, refs, acc)
	if body.Refused {
		return body, fmt.Errorf("%w: %s", ErrGateRefused, strings.Join(body.RefusalReasons, "; "))
	}
	return body, nil
}

// compareRefs is the canonical dump order of the whole subcommand: base, cond,
// then the replicate pair. Roles are assigned here rather than trusted from the
// caller, so a report never labels a dump differently than the gates read it.
func compareRefs(in CompareInput) []DumpRef {
	out := []DumpRef{in.Base, in.Cond, in.NoisePair[0], in.NoisePair[1]}
	for i, role := range []string{RoleBase, RoleCond, RoleNoiseA, RoleNoiseB} {
		out[i].Role = role
	}
	return out
}

// The four roles of one comparison.
const (
	RoleBase   = "base"
	RoleCond   = "cond"
	RoleNoiseA = "noise-a"
	RoleNoiseB = "noise-b"
)

func shadowTypeSet(types []string) map[string]bool {
	out := map[string]bool{}
	for _, t := range types {
		if t != "" {
			out[t] = true
		}
	}
	return out
}

// ------------------------------------------------------------- congruence.

// checkStampCongruence is gates (c) and the stamp half of (h): every dump of a
// campaign must describe the same measurement setup.
//
// The instance kind is read off the RAW stamps on purpose. Since the M-W2
// nachbesserung the REPORT env merges the kinds of a pair into one string
// (report.go:331-341, "differing kinds are BOTH named rather than resolved"),
// which is the honest rendering for `score` but useless as a gate input: a
// merged "live / measure-copy" is one value where the campaign rule of F-32
// needs two.
func checkStampCongruence(refs []DumpRef) error {
	var reasons []string
	for _, r := range refs[1:] {
		reasons = append(reasons, stampMismatches(refs[0], r)...)
	}
	if len(reasons) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrStampIncongruent, strings.Join(reasons, "; "))
}

func stampMismatches(a, b DumpRef) []string {
	var out []string
	add := func(field, want, got string) {
		if want != got {
			out = append(out, fmt.Sprintf("%s: %s=%q gegen %s=%q", b.Role, field, got, a.Role, want))
		}
	}
	// The campaign anchor is the PRIMING run, not the dump run id: `prime`
	// collects the pins for BOTH conditions and refuses -shadow-types for
	// exactly that reason (cmd/ctx-armsweep/commands.go:19-26). Two dumps pinned
	// from different priming runs measured two different question texts.
	add("run_id-Stamm (pin_run_id)", a.Stamp.PinRunID, b.Stamp.PinRunID)
	add("pin_sha256", a.Stamp.PinSHA256, b.Stamp.PinSHA256)
	add("gold_stamp_sha256", a.Stamp.GoldStamp, b.Stamp.GoldStamp)
	add("gold_sha256", CombinedDigest(a.Stamp.SliceFiles), CombinedDigest(b.Stamp.SliceFiles))
	add("migrations_max", strconv.Itoa(a.Stamp.MigrationsMax), strconv.Itoa(b.Stamp.MigrationsMax))
	add("post_fusion_stages", canonicalJSON(a.Stamp.PostFusionStages), canonicalJSON(b.Stamp.PostFusionStages))
	add("instance_kind", a.Stamp.InstanceKind, b.Stamp.InstanceKind)
	add("hnsw.ef_search", a.Stamp.EfSearch, b.Stamp.EfSearch)
	return out
}

// canonicalJSON renders a stamp map in a stable form. json.Marshal sorts map
// keys, so equal states compare equal regardless of read order.
func canonicalJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// caseGUC is the per-case half of gate (h): the ANN knobs the measured
// statement actually applied for THIS query.
//
// Read off the recorded selector state rather than a configuration snapshot,
// because that is where the SQL decides them: ctx_rrf_arms sets
// hnsw.iterative_scan='relaxed_order' and hnsw.max_scan_tuples ONLY on the ann
// path (142:216-220), and the exact path sets neither.
type caseGUC struct {
	iterativeScan string
	maxScanTuples string
}

func gucOf(rec Record) caseGUC {
	if rec.Selector.Mode != semanticModeANN {
		return caseGUC{iterativeScan: gucUnset, maxScanTuples: gucUnset}
	}
	scan := gucUnset
	if rec.Selector.ScanTuples != nil {
		scan = strconv.Itoa(*rec.Selector.ScanTuples)
	}
	return caseGUC{iterativeScan: "relaxed_order", maxScanTuples: scan}
}

func gucMismatch(key string, a, b Record, roleA, roleB string) error {
	ga, gb := gucOf(a), gucOf(b)
	if ga.iterativeScan != gb.iterativeScan {
		return fmt.Errorf("%w: Fall %s: hnsw.iterative_scan=%q (%s) gegen %q (%s)",
			ErrStampIncongruent, key, gb.iterativeScan, roleB, ga.iterativeScan, roleA)
	}
	if ga.maxScanTuples != gb.maxScanTuples {
		return fmt.Errorf("%w: Fall %s: hnsw.max_scan_tuples=%q (%s) gegen %q (%s)",
			ErrStampIncongruent, key, gb.maxScanTuples, roleB, ga.maxScanTuples, roleA)
	}
	return nil
}

// ---------------------------------------------------------------- pairing.

// sliceAcc is one report slice's paired accumulator. It grows with the CASE
// count, never with the candidate count — which is what keeps four 290 000-line
// dumps inside the RSS cap of gate (f).
type sliceAcc struct {
	n            int
	labelled     int
	temporal     int
	dNDCG        []float64
	dRecall      []float64
	dMRR         []float64
	baseHit      []bool
	condHit      []bool
	noiseDelta   []float64
	noiseAHit    []bool
	noiseBHit    []bool
	displacement dispAcc
}

// dispAcc accumulates the displacement table of one slice.
type dispAcc struct {
	cases           int
	casesWith       int
	displaced       int
	minPerCase      int
	maxPerCase      int
	labelledOut     int
	labelsAvailable bool
	shadowTopK      int
	shadowRank1     int
	byTypeOut       map[string]int
	byTypeIn        map[string]int
}

type compareAcc struct {
	slices        map[string]*sliceAcc
	paired        int
	unpairedTotal int
	unpaired      []string
	// guc is the per-case GUC state of the FIRST paired case. Every later case
	// is checked against its own partners, so by the end of the walk this one
	// value describes the whole campaign — and the report prints it once.
	guc      caseGUC
	gucKnown bool
}

func newCompareAcc() *compareAcc {
	return &compareAcc{slices: map[string]*sliceAcc{}}
}

func (a *compareAcc) slice(key string) *sliceAcc {
	s, ok := a.slices[key]
	if !ok {
		s = &sliceAcc{displacement: dispAcc{minPerCase: -1, byTypeOut: map[string]int{}, byTypeIn: map[string]int{}}}
		a.slices[key] = s
	}
	return s
}

func (a *compareAcc) noteUnpaired(key string) {
	a.unpairedTotal++
	if len(a.unpaired) < maxNamedUnpaired {
		a.unpaired = append(a.unpaired, key)
	}
}

// streamPairs walks the four dumps in lockstep and folds every case that exists
// in ALL of them. A case missing from one dump is counted, never paired by
// position: an exclusion that hit one run and not the other would otherwise
// pair case i of one dump with a different case of the next.
func streamPairs(refs []DumpRef, shadow map[string]bool, regime RegimeSplit) (*compareAcc, error) {
	streams := make([]*RecordStream, len(refs))
	defer func() {
		for _, s := range streams {
			if s != nil {
				_ = s.Close()
			}
		}
	}()
	cur := make([]Record, len(refs))
	live := make([]bool, len(refs))
	for i, r := range refs {
		s, err := OpenRecordStream(r.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Role, err)
		}
		streams[i] = s
		if cur[i], live[i], err = s.Next(); err != nil {
			return nil, err
		}
	}

	acc := newCompareAcc()
	for {
		key, open := smallestKey(cur, live)
		if !open {
			return acc, nil
		}
		if allAt(cur, live, key) {
			if err := foldPair(acc, refs, cur, shadow, regime); err != nil {
				return nil, err
			}
			acc.paired++
		} else {
			acc.noteUnpaired(key)
		}
		for i := range refs {
			if !live[i] || cur[i].Key() != key {
				continue
			}
			var err error
			if cur[i], live[i], err = streams[i].Next(); err != nil {
				return nil, err
			}
		}
	}
}

func smallestKey(cur []Record, live []bool) (string, bool) {
	key, found := "", false
	for i := range cur {
		if !live[i] {
			continue
		}
		if k := cur[i].Key(); !found || k < key {
			key, found = k, true
		}
	}
	return key, found
}

func allAt(cur []Record, live []bool, key string) bool {
	for i := range cur {
		if !live[i] || cur[i].Key() != key {
			return false
		}
	}
	return true
}

// foldPair reduces one paired case to its contributions. The records leave
// scope with the next iteration — nothing but the folded numbers survives.
func foldPair(acc *compareAcc, refs []DumpRef, cur []Record, shadow map[string]bool, regime RegimeSplit) error {
	base, cond, na, nb := cur[0], cur[1], cur[2], cur[3]
	key := base.Key()
	for i := 1; i < len(cur); i++ {
		if err := gucMismatch(key, base, cur[i], refs[0].Role, refs[i].Role); err != nil {
			return err
		}
		if !sameIDs(base.GoldIDs, cur[i].GoldIDs) {
			return fmt.Errorf("%w: Fall %s: Gold-Menge in %s weicht von %s ab",
				ErrStampIncongruent, key, refs[i].Role, refs[0].Role)
		}
	}

	if !acc.gucKnown {
		acc.guc, acc.gucKnown = gucOf(base), true
	}

	// Fail-closed BEFORE the fold: an uncovered G-REAL case would otherwise
	// contribute to the total row and to neither half, and the halves would
	// still add up to something the report cannot name (regime.go).
	base, err := stampRegime(base, regime)
	if err != nil {
		return err
	}

	cfg := ConfigV0()
	bs, cs := ScoreCase(base, cfg), ScoreCase(cond, cfg)
	as, bs2 := ScoreCase(na, cfg), ScoreCase(nb, cfg)
	// A stratified G-REAL case is folded into TWO accumulators: its total row
	// and its regime half. Scored once, filed twice — the halves cannot drift
	// from the total they partition.
	for _, k := range SliceKeysOf(base) {
		foldInto(acc.slice(k), base, cond, bs, cs, as, bs2, cfg, shadow)
	}
	return nil
}

// foldInto adds one paired case to one slice accumulator.
func foldInto(s *sliceAcc, base, cond Record, bs, cs, as, bs2 CaseScore, cfg Config, shadow map[string]bool) {
	s.n++
	if len(base.GoldIDs) > 0 {
		s.labelled++
	}
	if base.EffectiveTemporal != "" {
		s.temporal++
	}

	s.dNDCG = append(s.dNDCG, cs.NDCG10-bs.NDCG10)
	s.dRecall = append(s.dRecall, cs.Recall5-bs.Recall5)
	s.dMRR = append(s.dMRR, cs.MRR10-bs.MRR10)
	s.baseHit = append(s.baseHit, bs.Hit5)
	s.condHit = append(s.condHit, cs.Hit5)

	s.noiseDelta = append(s.noiseDelta, bs2.NDCG10-as.NDCG10)
	s.noiseAHit = append(s.noiseAHit, as.Hit5)
	s.noiseBHit = append(s.noiseBHit, bs2.Hit5)

	foldDisplacement(&s.displacement, base, cond, cfg, shadow)
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// foldDisplacement compares the two top-K windows of one case: who entered, who
// was pushed out, and whether the pushed-out block carried a judgement.
func foldDisplacement(d *dispAcc, base, cond Record, cfg Config, shadow map[string]bool) {
	baseTop := topIDs(base, cfg)
	condTop := topIDs(cond, cfg)
	types := typeIndex(base.Rows, cond.Rows)
	gold := make(map[string]bool, len(base.GoldIDs))
	for _, id := range base.GoldIDs {
		gold[id] = true
	}

	d.cases++
	if len(gold) > 0 {
		d.labelsAvailable = true
	}
	inCond := make(map[string]bool, len(condTop))
	for i, id := range condTop {
		inCond[id] = true
		if shadow[types[id]] {
			d.shadowTopK++
			if i == 0 {
				d.shadowRank1++
			}
		}
	}
	inBase := make(map[string]bool, len(baseTop))
	for _, id := range baseTop {
		inBase[id] = true
	}
	out := 0
	for _, id := range baseTop {
		if inCond[id] {
			continue
		}
		out++
		d.displaced++
		d.byTypeOut[types[id]]++
		if gold[id] {
			d.labelledOut++
		}
	}
	for _, id := range condTop {
		if !inBase[id] {
			d.byTypeIn[types[id]]++
		}
	}
	if out > 0 {
		d.casesWith++
	}
	if d.minPerCase < 0 || out < d.minPerCase {
		d.minPerCase = out
	}
	if out > d.maxPerCase {
		d.maxPerCase = out
	}
}

// topIDs is the offline fusion's top-K ranking of one case — the same ranking
// the metrics are scored on (score.go:95-98), never the delivered one.
func topIDs(rec Record, cfg Config) []string {
	ids := FusedIDs(Fuse(rec.Rows, cfg))
	if len(ids) > DisplacementCut {
		ids = ids[:DisplacementCut]
	}
	return ids
}

// typeIndex maps candidate id to registry type name. Both dumps contribute: a
// block that exists in only one of them still has to be nameable in the table.
// A dump written before migration 142 carries no name at all — the table says
// so rather than inventing one (fuse.go:61-67).
func typeIndex(rowSets ...[]rrf.ArmRow) map[string]string {
	out := map[string]string{}
	for _, rows := range rowSets {
		for _, r := range rows {
			name := r.TypeName
			if name == "" {
				name = typeNameUnknown
			}
			if _, ok := out[r.ID]; !ok {
				out[r.ID] = name
			}
		}
	}
	return out
}

// typeNameUnknown stands for a candidate out of a pre-142 dump.
const typeNameUnknown = "(kein type_name im Dump)"

// ------------------------------------------------------------------ body.

// buildCompareBody turns the folded accumulators into the report. Every walk is
// over a sorted slice list — ReportSlices for the gates, CensusSlices for the
// descriptive tables — so no map order reaches the bytes (gate (d)).
func buildCompareBody(in CompareInput, refs []DumpRef, acc *compareAcc) CompareBody {
	body := CompareBody{
		Version:       CompareVersion,
		Env:           buildCompareEnv(in, refs, acc),
		Paired:        acc.paired,
		UnpairedTotal: acc.unpairedTotal,
		Unpaired:      acc.unpaired,
		Slices:        compareSliceProfiles(acc),
	}

	noiseBySlice := map[string]NoiseGate{}
	for _, slice := range ReportSlices() {
		s := acc.slices[slice]
		if s == nil || s.n == 0 || s.labelled == 0 {
			continue
		}
		cmp, ok := noiseComparison(slice, s, in.Seed)
		if !ok {
			continue
		}
		g := evaluateNoise(cmp)
		noiseBySlice[slice] = g
		body.Noise = append(body.Noise, g)
		body.MDE = append(body.MDE, mdeOf(slice, s, cmp))
	}

	// The refusal is decided on the rollout slices ALONE, before the strata are
	// measured: a stratum is a subset of G-REAL, so a red half would refuse the
	// comparison a second time for cases that already voted. Their MDE row is
	// what §4.4b asks for and it is written either way — with n=19 on the global
	// half that row will be large, and that is a fact ABOUT the slice, not a
	// verdict about the instrument.
	body.Refused, body.RefusalReasons = noiseVerdict(body.Noise)
	for _, slice := range StratumSlices() {
		s := acc.slices[slice]
		if s == nil || s.n == 0 || s.labelled == 0 {
			continue
		}
		cmp, ok := noiseComparison(slice, s, in.Seed)
		if !ok {
			continue
		}
		noiseBySlice[slice] = evaluateNoise(cmp)
		body.MDE = append(body.MDE, mdeOf(slice, s, cmp))
	}

	for _, slice := range append(ReportSlices(), StratumSlices()...) {
		s := acc.slices[slice]
		if s == nil || s.n == 0 {
			continue
		}
		body.Effects = append(body.Effects, effectOf(slice, s, noiseBySlice[slice], in.Seed))
	}
	body.Displacement = displacementRows(acc)
	body.Notes = compareNotes(in, body)
	return body
}

// noiseComparison is the V0/V0' comparison of one slice, in the shape the
// existing G-NOISE evaluation consumes. Reused rather than re-implemented: the
// thresholds belong to score.go:20-23 (the X-W1 gate) and this subcommand
// CONSUMES them, it does not redefine them.
func noiseComparison(slice string, s *sliceAcc, seed int64) (Comparison, bool) {
	if len(s.noiseDelta) == 0 {
		return Comparison{}, false
	}
	mc, err := evalscore.McNemarPaired(s.noiseAHit, s.noiseBHit)
	if err != nil {
		return Comparison{}, false
	}
	lo, hi := evalscore.PairedDiffCI(s.noiseDelta, PrimaryLevel, seed)
	return Comparison{
		Config: NameV0Prime, Slice: slice, N: len(s.noiseDelta), Level: PrimaryLevel,
		DeltaNDCG: evalscore.MeanOrZero(s.noiseDelta), CILo: lo, CIHi: hi,
		McNemar: mc, Discordance: evalscore.RatioOrZero(mc.Discordant, len(s.noiseDelta)),
	}, true
}

// mdeOf is the resolution row of §4.4b: the HALF CI WIDTH of ΔnDCG@10 between
// two identical runs is the smallest difference this slice can show at all.
func mdeOf(slice string, s *sliceAcc, noise Comparison) MDEReport {
	mde := (noise.CIHi - noise.CILo) / 2
	if mde < 0 {
		mde = -mde
	}
	rep := MDEReport{
		Slice: slice, N: len(s.noiseDelta), NoiseCILo: noise.CILo, NoiseCIHi: noise.CIHi,
		MDE: mde, Threshold: MDEThresholdNDCG, Resolvable: mde <= MDEThresholdNDCG,
	}
	if !rep.Resolvable {
		rep.Note = fmt.Sprintf(
			"MDE %.4f über der %.2f-Linie: ein Effekt in Literatur-Größenordnung ist auf dieser Slice nicht auflösbar (§4.4b) — ein Gate, das hier grün wird, ist ein Zufallsbefund",
			mde, MDEThresholdNDCG)
	}
	return rep
}

// noiseVerdict is gate (b): no evaluable slice, or one red slice, and the whole
// comparison is refused.
func noiseVerdict(gates []NoiseGate) (bool, []string) {
	if len(gates) == 0 {
		return true, []string{
			"G-NOISE nicht auswertbar: keine gelabelte Slice ist in allen vier Dumps gepaart vorhanden — ohne gemessenen Rauschboden ist keine Differenz ein Ergebnis",
		}
	}
	var reasons []string
	for _, g := range gates {
		for _, r := range g.Reasons {
			reasons = append(reasons, fmt.Sprintf("G-NOISE rot auf %s: %s", g.Slice, r))
		}
	}
	return len(reasons) > 0, reasons
}

// effectOf is the condition against the baseline on one slice: three deltas,
// three paired bootstrap CIs, McNemar on Hit@5 — and the two rules that decide
// whether any of it may be read (F-32 separability and the slice's own MDE).
func effectOf(slice string, s *sliceAcc, noise NoiseGate, seed int64) CompareEffect {
	e := CompareEffect{
		Slice: slice, N: len(s.dNDCG), Level: PrimaryLevel,
		Unlabelled:       s.labelled == 0,
		NoiseDiscordance: noise.Discordance,
	}
	if e.Unlabelled {
		e.Reasons = append(e.Reasons, "Slice ohne Relevanz-Urteile: gemeldet, nicht gewertet")
		return e
	}
	e.DeltaNDCG10 = evalscore.MeanOrZero(s.dNDCG)
	e.NDCGCILo, e.NDCGCIHi = evalscore.PairedDiffCI(s.dNDCG, PrimaryLevel, seed)
	e.DeltaRecall5 = evalscore.MeanOrZero(s.dRecall)
	e.RecallCILo, e.RecallCIHi = evalscore.PairedDiffCI(s.dRecall, PrimaryLevel, seed)
	e.DeltaMRR10 = evalscore.MeanOrZero(s.dMRR)
	e.MRRCILo, e.MRRCIHi = evalscore.PairedDiffCI(s.dMRR, PrimaryLevel, seed)

	if mc, err := evalscore.McNemarPaired(s.baseHit, s.condHit); err == nil {
		e.McNemar = mc
		e.Discordance = evalscore.RatioOrZero(mc.Discordant, len(s.baseHit))
	}

	mde := (noise.CIHi - noise.CILo) / 2
	if mde < 0 {
		mde = -mde
	}
	effect := e.DeltaNDCG10
	if effect < 0 {
		effect = -effect
	}
	e.AboveMDE = effect > mde
	e.Separable = e.Discordance > e.NoiseDiscordance

	if !(e.NDCGCILo > 0 || e.NDCGCIHi < 0) {
		e.Reasons = append(e.Reasons, fmt.Sprintf(
			"CI von ΔnDCG@10 ist [%.5f, %.5f] und enthält 0", e.NDCGCILo, e.NDCGCIHi))
	}
	if !e.Separable {
		e.Reasons = append(e.Reasons, fmt.Sprintf(
			"F-32: Diskordanz der Bedingung %.4f übersteigt den Rauschboden %.4f nicht — der Effekt ist vom Rauschen nicht trennbar",
			e.Discordance, e.NoiseDiscordance))
	}
	if !e.AboveMDE {
		e.Reasons = append(e.Reasons, fmt.Sprintf(
			"|ΔnDCG@10| %.5f liegt unter der MDE dieser Slice (%.5f, §4.4b)", effect, mde))
	}
	e.Readable = len(e.Reasons) == 0
	return e
}

// displacementRows walks CensusSlices, so a floor slice gets its own row and
// carries RolloutCriterion=false — the same separation report.go:422-457 makes
// for the case census.
func displacementRows(acc *compareAcc) []DisplacementRow {
	floor := map[string]bool{}
	for _, s := range FloorSlices() {
		floor[s] = true
	}
	var out []DisplacementRow
	for _, slice := range CensusSlices() {
		s := acc.slices[slice]
		if s == nil || s.displacement.cases == 0 {
			continue
		}
		d := s.displacement
		row := DisplacementRow{
			Slice: slice, RolloutCriterion: !floor[slice] && !IsStratum(slice), Cases: d.cases,
			CasesWithDisplacement: d.casesWith, Cut: DisplacementCut,
			ShadowInTopK: d.shadowTopK, ShadowAtRank1: d.shadowRank1,
			Displaced: d.displaced, MaxPerCase: d.maxPerCase,
			DisplacedLabelled: d.labelledOut, LabelsAvailable: d.labelsAvailable,
			DisplacedByType: typeCounts(d.byTypeOut), EntrantsByType: typeCounts(d.byTypeIn),
		}
		if d.minPerCase > 0 {
			row.MinPerCase = d.minPerCase
		}
		switch {
		case !d.labelsAvailable:
			row.Note = "ohne Labels auf dieser Slice bleibt die Spalte „verdrängt & gelabelt\" leer (§4.3)"
		case floor[slice]:
			row.Note = "Boden-Check: konstruktives Gold, nie Rollout-Kriterium"
		case IsStratum(slice):
			row.Note = StratumNote
		}
		out = append(out, row)
	}
	return out
}

// typeCounts renders a type histogram in a stable order: count descending, name
// ascending — a report whose rows moved between two runs would fail gate (d).
func typeCounts(m map[string]int) []TypeCount {
	out := make([]TypeCount, 0, len(m))
	for name, n := range m {
		out = append(out, TypeCount{TypeName: name, N: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].TypeName < out[j].TypeName
	})
	return out
}

func compareSliceProfiles(acc *compareAcc) []SliceProfile {
	floor := map[string]bool{}
	for _, s := range FloorSlices() {
		floor[s] = true
	}
	var out []SliceProfile
	for _, slice := range CensusSlices() {
		s := acc.slices[slice]
		if s == nil || s.n == 0 {
			continue
		}
		p := SliceProfile{
			Slice: slice, N: s.n, Labelled: s.labelled, Unlabelled: s.labelled == 0,
			TemporalShare:    TemporalShare(s.n, s.temporal),
			RolloutCriterion: !floor[slice] && !IsStratum(slice),
		}
		switch {
		case floor[slice]:
			p.Note = "floor check: gold is constructive and circular against the layer it would judge — never a rollout criterion"
		case IsStratum(slice):
			p.Note = StratumNote
		case s.labelled == 0:
			p.Note = "unlabelled, skipped — relevance judgements land in wave B-W6"
		case s.labelled < s.n:
			p.Note = fmt.Sprintf("partly labelled: %d of %d cases carry judgements; the rest score 0 by construction", s.labelled, s.n)
		}
		out = append(out, p)
	}
	return out
}

func buildCompareEnv(in CompareInput, refs []DumpRef, acc *compareAcc) CompareEnv {
	env := CompareEnv{
		Tool: "ctx-armsweep compare", GitRevision: in.GitRevision, Seed: in.Seed,
		SampleSeed: in.GoldStamp.SampleSeed, SplitSeed: in.GoldStamp.SplitSeed,
		GoldSHA256: CombinedDigest(in.Base.Stamp.SliceFiles), GoldStampSHA256: in.Base.Stamp.GoldStamp,
		SliceFiles: in.Base.Stamp.SliceFiles, Generator: in.GoldStamp.Generator,
		CorpusMaxCreatedAt: in.GoldStamp.CorpusMaxCreatedAt,
		MigrationsMax:      in.Base.Stamp.MigrationsMax,
		PostFusionStages:   in.Base.Stamp.PostFusionStages,
		CampaignPinRunID:   in.Base.Stamp.PinRunID,
		InstanceKind:       in.Base.Stamp.InstanceKind,
		ShadowTypes:        in.Cond.Stamp.ShadowTypes,
		RegimeLabels:       in.RegimeSplit.Stamp(),
		GUCs:               campaignGUCs(in, acc),
	}
	// The provenance cites each dump by the RELATIVE name its stamp carries, not
	// by the path this process opened: a report is an artefact that travels, and
	// the absolute location of a root-only gold directory is not part of the
	// measurement.
	for _, r := range refs {
		env.AllowLiveInstance = env.AllowLiveInstance || r.Stamp.AllowLiveInstance
		env.AllowOutsideGoldset = env.AllowOutsideGoldset || r.Stamp.AllowOutsideGoldset
		env.Dumps = append(env.Dumps, dumpProvenance(r.Role, r.Stamp))
	}
	return env
}

// campaignGUCs reports the three knobs of §4.4b with the place each was read.
// The per-case values are congruent across the campaign by the time this runs —
// the pairing refused otherwise — so ONE value per knob is the honest rendering.
func campaignGUCs(in CompareInput, acc *compareAcc) []GUCValue {
	ef := in.Base.Stamp.EfSearch
	source := "Dump-Stempel (/api/status db.hnsw.ef_search_effective)"
	if ef == "" {
		ef, source = gucUnset, "nicht gestempelt: Dumps aus einem Lauf vor Welle M-W3d"
	}
	out := []GUCValue{{Name: "hnsw.ef_search", Value: ef, Source: source}}
	it, scan := gucUnset, gucUnset
	if acc.gucKnown {
		it, scan = acc.guc.iterativeScan, acc.guc.maxScanTuples
	}
	out = append(out,
		GUCValue{Name: "hnsw.iterative_scan", Value: it, Source: "Selector-Zustand je Fall (142:216-220)"},
		GUCValue{Name: "hnsw.max_scan_tuples", Value: scan, Source: "Selector-Zustand je Fall (142:216-220)"})
	return out
}

// compareNotes states the reading rules the numbers above are subject to.
func compareNotes(in CompareInput, body CompareBody) []string {
	notes := []string{
		fmt.Sprintf("Bedingungs-Vergleich: %q gegen %q, Rauschboden aus %q/%q derselben Kampagne (§4.3).",
			in.Cond.Stamp.RunID, in.Base.Stamp.RunID, in.NoisePair[0].Stamp.RunID, in.NoisePair[1].Stamp.RunID),
		fmt.Sprintf("Gewertet wird die OFFLINE-Fusion unter %s, nicht die ausgelieferte Reihenfolge — wie in `score` (score.go:95-98).", NameV0),
	}
	if len(in.Cond.Stamp.ShadowTypes) == 0 {
		notes = append(notes,
			"Der Bedingungs-Dump nennt keine Schatten-Typen: die Verdrängungs-Tabelle weist deshalb keine Schatten-Plätze aus, die Ein-/Austritte je Typ bleiben gültig.")
	}
	if body.UnpairedTotal > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d Fälle sind nicht in allen vier Dumps vorhanden und bleiben ungepaart (Ausschlüsse eines einzelnen Laufs). Gepaart: %d.",
			body.UnpairedTotal, body.Paired))
	}
	for _, m := range body.MDE {
		if !m.Resolvable {
			notes = append(notes, fmt.Sprintf("Slice %s: %s", m.Slice, m.Note))
		}
	}
	return notes
}

// -------------------------------------------------------------- artefacts.

// MarshalCompareBody renders a compare report body deterministically.
func MarshalCompareBody(body CompareBody) ([]byte, error) {
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteCompareReport writes header line + body, the same split `score` uses:
// the only volatile field is in the header, so a diff of two reports over the
// same dump set is empty from line 2 on (gate (d)).
func WriteCompareReport(path, generatedAt string, body CompareBody) error {
	b, err := MarshalCompareBody(body)
	if err != nil {
		return err
	}
	hdr, err := json.Marshal(ReportHeader{
		Tool: "ctx-armsweep compare", GeneratedAt: generatedAt, BodySHA256: goldset.SHA256Hex(string(b)),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(append(hdr, '\n'), b...), fileMode)
}

// WriteCompareMarkdown writes the human-readable half.
func WriteCompareMarkdown(path, generatedAt string, body CompareBody) error {
	return os.WriteFile(path, []byte(RenderCompareMarkdown(generatedAt, body)), fileMode)
}
