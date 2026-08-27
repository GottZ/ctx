package armsweep

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/GottZ/ctx/internal/goldset"
)

// ReportVersion is the report schema generation.
const ReportVersion = 1

// EnvStamp is the §4.8 provenance block: everything a later reader needs to
// decide whether two reports are comparable at all.
//
// Nothing volatile lives here. The generation timestamp sits in the report
// HEADER LINE, outside the body, precisely so two runs of `score` over one dump
// produce byte-identical bodies (gate (c)).
type EnvStamp struct {
	Tool        string `json:"tool"`
	GitRevision string `json:"git_revision"`
	Seed        int64  `json:"seed"`
	SampleSeed  int64  `json:"gold_sample_seed"`
	SplitSeed   int64  `json:"gold_split_seed"`
	// GoldSHA256 digests the SORTED per-file digests — one value that changes
	// if any slice file changed, independent of directory listing order.
	GoldSHA256      string        `json:"gold_sha256"`
	GoldStampSHA256 string        `json:"gold_stamp_sha256"`
	SliceFiles      []SliceDigest `json:"slice_files"`
	// Generator is the on-prem model that produced the G-Q questions, copied
	// out of STAMP.json — the one point where block content reached a model.
	Generator           *goldset.Generator `json:"generator,omitempty"`
	CorpusMaxCreatedAt  string             `json:"corpus_max_created_at"`
	MigrationsMax       int                `json:"migrations_max"`
	PostFusionStages    map[string]any     `json:"post_fusion_stages"`
	AllowOutsideGoldset bool               `json:"allow_outside_goldset"`
	// InstanceKind/ShadowTypes/AllowLiveInstance carry gate (l) into the report
	// (§5 B4b): which instance the numbers came from, which shadow types were
	// measured, and whether the measure-copy requirement was overridden. The
	// override is only ever legitimate when it is visible.
	InstanceKind      string   `json:"instance_kind,omitempty"`
	ShadowTypes       []string `json:"shadow_types,omitempty"`
	AllowLiveInstance bool     `json:"allow_live_instance,omitempty"`
	// RegimeLabels is the X-W0 label file the G-REAL strata were cut from
	// (wave X-W0b). Absent — and, being omitempty, absent from the bytes as
	// well — when no split was asked for.
	RegimeLabels *RegimeStamp     `json:"regime_labels,omitempty"`
	Dumps        []DumpProvenance `json:"dumps"`
}

// DumpProvenance is one measured dump as the report cites it.
type DumpProvenance struct {
	Role      string       `json:"role"`
	RunID     string       `json:"run_id"`
	File      string       `json:"file"`
	CreatedAt string       `json:"created_at"`
	BaseURL   string       `json:"base_url"`
	PinFile   string       `json:"pin_file"`
	PinRunID  string       `json:"pin_run_id"`
	PinSHA256 string       `json:"pin_sha256"`
	Records   int          `json:"records"`
	Drift     DriftVerdict `json:"drift_verdict"`
	Latency   Latency      `json:"latency"`
}

// SliceProfile is the case census of one report slice.
type SliceProfile struct {
	Slice string `json:"slice"`
	N     int    `json:"n"`
	// Labelled is the number of cases carrying relevance judgements.
	Labelled      int     `json:"labelled"`
	Unlabelled    bool    `json:"unlabelled"`
	TemporalShare float64 `json:"temporal_share"`
	// RolloutCriterion is false for floor slices. It is carried per ROW rather
	// than left implicit in the slice name so a reader of the JSON cannot mix
	// a floor figure into a rollout argument.
	RolloutCriterion bool   `json:"rollout_criterion"`
	Note             string `json:"note,omitempty"`
}

// ReportBody is the deterministic half of a report.
type ReportBody struct {
	Version int      `json:"version"`
	Env     EnvStamp `json:"env"`
	// Interpretable is false when G-NOISE did not pass or could not be
	// evaluated. The variant table is still printed — suppressing it would hide
	// the evidence — but nothing in it may be read as a result.
	Interpretable bool           `json:"interpretable"`
	Slices        []SliceProfile `json:"slices"`
	Noise         []NoiseGate    `json:"noise_gate"`
	Configs       []ConfigResult `json:"configs"`
	Comparisons   []Comparison   `json:"comparisons"`
	Wins          []WinGate      `json:"win_gate"`
	// Damping is the M-W8 curve: one ConfigResult per support point of ONE
	// type, reported as its own family. Absent — and, being omitempty, absent
	// from the bytes as well — when no damping type was swept, so a report
	// scored the way every report before this wave was scored is byte-identical
	// to the one that wave produced.
	Damping     []ConfigResult `json:"damping,omitempty"`
	DampingType string         `json:"damping_type,omitempty"`
	Excluded    []ExcludedCase `json:"excluded"`
	Notes       []string       `json:"notes"`
}

// ReportHeader is the volatile line. Everything that would break byte-identity
// lives here and nowhere else.
type ReportHeader struct {
	Tool        string `json:"tool"`
	GeneratedAt string `json:"generated_at"`
	BodySHA256  string `json:"body_sha256"`
}

// ScoreInput is everything the offline score step consumes.
type ScoreInput struct {
	RecordsA []Record
	StampA   DumpStamp
	// RecordsB/StampB are the V0' replicate. Absent means G-NOISE cannot be
	// evaluated, and the report says so instead of quietly passing it.
	RecordsB []Record
	StampB   *DumpStamp

	// DampingType names the block type whose damping curve is swept (M-W8).
	// Empty means no curve: the report is exactly the one this instrument
	// produced before the wave, and a pre-142 dump stays scorable.
	DampingType string

	// RegimeSplit carries the X-W0 labels that cut G-REAL into its two regime
	// rows (wave X-W0b). The zero value is inactive and the report is then
	// byte-identical to the one this instrument produced before the wave.
	RegimeSplit RegimeSplit

	Seed        int64
	GitRevision string
	GoldStamp   goldset.Stamp
}

// ErrDumpPredatesTypeName refuses a damping sweep over a dump measured before
// migration 142.
//
// The failure it prevents is silent, not loud: such a dump carries the empty
// string in every row's TypeName, so every damping lookup misses, every support
// point falls back to the dump's own factor, and the curve comes out FLAT —
// ten identical rows that read as "damping does not matter on this corpus"
// while in truth nothing was damped at all. There is no defensible default
// here, which is why this is an error and not a note in the report.
var ErrDumpPredatesTypeName = errors.New("dump predates migration 142: its rows carry no type_name, so a damping curve over it would be flat by construction, not by measurement")

// Score is the whole offline evaluation: re-fuse, measure, gate, report.
//
// It touches no clock, no network and no randomness beyond the seed it is
// handed, which is what makes gate (c) achievable at all. The only error it
// returns is the damping refusal above: everything else it can measure, it
// measures and reports, including its own failed gates.
func Score(in ScoreInput) (ReportBody, error) {
	if err := checkDampingDumps(in); err != nil {
		return ReportBody{}, err
	}
	excluded := unionExcluded(in.StampA, in.StampB)
	recsA := dropExcluded(in.RecordsA, excluded)
	recsB := dropExcluded(in.RecordsB, excluded)

	// The stratification runs AFTER the exclusions: a case that left the report
	// needs no label, and demanding one for it would refuse a run over a fact
	// the report does not state.
	var err error
	if recsA, err = applyRegime(recsA, in.RegimeSplit); err != nil {
		return ReportBody{}, err
	}
	if recsB, err = applyRegime(recsB, in.RegimeSplit); err != nil {
		return ReportBody{}, err
	}

	body := ReportBody{
		Version: ReportVersion,
		Env:     buildEnv(in),
		Slices:  BuildSliceProfiles(recsA),
	}

	labelled := labelledCounts(recsA)
	unlab := map[string]bool{}
	for slice, c := range labelled {
		unlab[slice] = c[1] == 0
	}

	// Pass 1: the static configurations, so the solo arms exist before V6 is
	// derived from them.
	static := scoreConfigs(staticConfigs(), recsA, recsB, unlab, in.Seed)
	solo := soloProfile(static)
	all := scoreConfigs(AllConfigs(solo), recsA, recsB, unlab, in.Seed)
	body.Configs = all

	// The per-case views every gate below is computed on.
	setsA := map[string]map[string]*caseSet{}
	for _, cfg := range AllConfigs(solo) {
		setsA[cfg.Name] = scoreSlices(recsA, cfg)
	}
	var v0B map[string]*caseSet
	if len(recsB) > 0 {
		v0B = scoreSlices(recsB, ConfigV0())
	}

	noiseCmp := map[string]Comparison{}
	for _, slice := range ReportSlices() {
		if unlab[slice] || v0B == nil {
			continue
		}
		if c, ok := compare(NameV0Prime, slice, setsA[NameV0][slice], v0B[slice], PrimaryLevel, in.Seed); ok {
			noiseCmp[slice] = c
			body.Noise = append(body.Noise, evaluateNoise(c))
		}
	}
	body.Interpretable = len(body.Noise) > 0
	for _, g := range body.Noise {
		if !g.Pass {
			body.Interpretable = false
		}
	}
	if v0B == nil {
		body.Notes = append(body.Notes,
			"G-NOISE not evaluated: only one dump was supplied. Without the V0' replicate the instrument has no measured noise floor, so no variant below is a result.")
	} else if !body.Interpretable {
		body.Notes = append(body.Notes,
			"G-NOISE failed: the replicate pair disagrees beyond the §4.9 tolerance. The variant table below is evidence about the instrument, not about the variants.")
	}

	body.Comparisons, body.Wins = compareAll(all, setsA, v0B, noiseCmp, unlab, in.Seed)

	if in.DampingType != "" {
		body.DampingType = in.DampingType
		body.Damping = scoreConfigs(DampingConfigs(in.DampingType), recsA, nil, unlab, in.Seed)
		body.Notes = append(body.Notes, fmt.Sprintf(
			"damping curve over %q: %d support points on dump A, live weights throughout. Reported, never gated — the optimum of this curve is a finding for the registry, not a rollout criterion (design 05 §6.1). It is deliberately absent from the variant table above, whose Bonferroni level is fixed at %d comparisons.",
			in.DampingType, len(DampingStops), SecondaryComparisons))
	}

	body.Excluded = excluded
	body.Notes = append(body.Notes, unlabelledNotes(labelled)...)
	return body, nil
}

// checkDampingDumps is the M-W1-review gate: a damping sweep may only run over
// dumps that actually recorded a type name.
//
// Both dumps are checked, not only the one the curve is scored on. The B dump
// feeds V0' and therefore G-NOISE, and a pair whose halves were measured across
// the 142 boundary is not a replicate pair at all — refusing it here costs a
// re-dump and hides nothing.
func checkDampingDumps(in ScoreInput) error {
	if in.DampingType == "" {
		return nil
	}
	stamps := []struct {
		role  string
		stamp *DumpStamp
	}{{"A", &in.StampA}}
	if in.StampB != nil {
		stamps = append(stamps, struct {
			role  string
			stamp *DumpStamp
		}{"B", in.StampB})
	}
	for _, s := range stamps {
		if s.stamp.MigrationsMax < TypeNameMigration {
			return fmt.Errorf("damping sweep over %q refused: dump %s (%s) was measured at migrations_max %d, below %d: %w",
				in.DampingType, s.role, s.stamp.DumpFile, s.stamp.MigrationsMax, TypeNameMigration, ErrDumpPredatesTypeName)
		}
	}
	return nil
}

// scoreConfigs measures every configuration over the right dump. V0' is the
// ONLY configuration read off the second dump — that is what makes it the
// replicate rather than a fifteenth variant.
func scoreConfigs(cfgs []Config, recsA, recsB []Record, unlabelled map[string]bool, seed int64) []ConfigResult {
	out := make([]ConfigResult, 0, len(cfgs))
	for _, cfg := range cfgs {
		recs, dump := recsA, "A"
		if cfg.Name == NameV0Prime {
			recs, dump = recsB, "B"
		}
		res := ConfigResult{Config: cfg, Dump: dump}
		sets := scoreSlices(recs, cfg)
		for _, slice := range ReportSlices() {
			if sets[slice] == nil && len(recs) > 0 {
				continue
			}
			res.Slices = append(res.Slices, metricsOf(slice, sets[slice], unlabelled[slice], seed))
		}
		// The strata are appended only where records for them exist. Unlike the
		// rollout slices they get NO empty row on a missing dump: an absent
		// split is absent, and a zero row would read as a measured half.
		for _, slice := range StratumSlices() {
			if sets[slice] == nil {
				continue
			}
			res.Slices = append(res.Slices, metricsOf(slice, sets[slice], unlabelled[slice], seed))
		}
		out = append(out, res)
	}
	return out
}

// compareAll produces the variant-vs-V0 table and the G-WIN verdicts.
func compareAll(cfgs []ConfigResult, setsA map[string]map[string]*caseSet, v0B map[string]*caseSet,
	noiseCmp map[string]Comparison, unlabelled map[string]bool, seed int64) ([]Comparison, []WinGate) {
	var cmps []Comparison
	var wins []WinGate

	for _, r := range cfgs {
		name := r.Config.Name
		if name == NameV0 {
			continue
		}
		level := SecondaryLevel
		if name == NameV1 {
			level = PrimaryLevel
		}
		primary := name == NameV1

		var hold, holdRef *Comparison
		var others []Comparison
		// The strata are walked in the same loop and produce the same rows — but
		// they never enter `others`. A stratum is a SUBSET of G-REAL, which
		// already votes there; letting both vote would give the same cases two
		// regression vetoes over one win (§4.4b asks for the row, not the vote).
		for _, slice := range append(ReportSlices(), StratumSlices()...) {
			if unlabelled[slice] {
				continue
			}
			c, ok := compare(name, slice, setsA[NameV0][slice],
				variantSet(name, slice, setsA, v0B), level, seed)
			if !ok {
				continue
			}
			cmps = append(cmps, c)
			switch {
			case slice == SliceQHold:
				cc := c
				hold = &cc
			case IsStratum(slice):
			default:
				others = append(others, c)
			}
		}
		if name == NameV0Prime {
			continue // the replicate is the noise floor, never a win candidate
		}
		if ref, ok := noiseCmp[SliceQHold]; ok {
			holdRef = &ref
		}
		wins = append(wins, evaluateWin(name, primary, hold, holdRef, others))
	}
	return cmps, wins
}

// variantSet resolves the case set a configuration is read off. V0' is the only
// one taken from the second dump — that is what makes it the replicate.
func variantSet(name, slice string, setsA map[string]map[string]*caseSet, v0B map[string]*caseSet) *caseSet {
	if name != NameV0Prime {
		return setsA[name][slice]
	}
	if v0B == nil {
		return nil
	}
	return v0B[slice]
}

// mergeInstanceKinds names every instance kind the pair was measured against,
// in dump order, without repeating one. Empty stamps contribute nothing — a
// dump that named no shadow types makes no claim about its instance, and
// inventing "live" for it would be a statement the driver never took.
func mergeInstanceKinds(a DumpStamp, b *DumpStamp) string {
	kinds := appendDistinct(nil, a.InstanceKind)
	if b != nil {
		kinds = appendDistinct(kinds, b.InstanceKind)
	}
	return strings.Join(kinds, " / ")
}

// mergeShadowTypes is the union of both dumps' shadow lists, dump order kept,
// duplicates dropped.
func mergeShadowTypes(a DumpStamp, b *DumpStamp) []string {
	types := appendDistinct(nil, a.ShadowTypes...)
	if b != nil {
		types = appendDistinct(types, b.ShadowTypes...)
	}
	return types
}

func appendDistinct(out []string, vals ...string) []string {
	for _, v := range vals {
		if v == "" || contains(out, v) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// buildEnv assembles the provenance block from the dump stamps and the gold
// stamp — never from the driver's own environment.
func buildEnv(in ScoreInput) EnvStamp {
	env := EnvStamp{
		Tool:                "ctx-armsweep",
		GitRevision:         in.GitRevision,
		Seed:                in.Seed,
		SampleSeed:          in.GoldStamp.SampleSeed,
		SplitSeed:           in.GoldStamp.SplitSeed,
		GoldStampSHA256:     in.StampA.GoldStamp,
		SliceFiles:          in.StampA.SliceFiles,
		Generator:           in.GoldStamp.Generator,
		CorpusMaxCreatedAt:  in.GoldStamp.CorpusMaxCreatedAt,
		MigrationsMax:       in.StampA.MigrationsMax,
		PostFusionStages:    in.StampA.PostFusionStages,
		AllowOutsideGoldset: in.StampA.AllowOutsideGoldset || (in.StampB != nil && in.StampB.AllowOutsideGoldset),
		// All THREE shadow-provenance fields are merged across the dump pair,
		// not read off A. A report over two dumps inherits the weakest
		// provenance of the two, and stating otherwise would let one clean dump
		// launder the other — which is exactly what happened while only the
		// boolean was OR-ed: a pair whose SHADOW dump was B carried
		// allow_live_instance=true and named neither the instance nor the types
		// (M-W2 review finding #2).
		//
		// Differing kinds are BOTH named rather than resolved. F-32 forbids the
		// mixed pair, but `score` does not enforce that rule (M-W3d's compare
		// does), so the honest rendering of an incongruent pair is to show the
		// incongruence.
		InstanceKind:      mergeInstanceKinds(in.StampA, in.StampB),
		ShadowTypes:       mergeShadowTypes(in.StampA, in.StampB),
		AllowLiveInstance: in.StampA.AllowLiveInstance || (in.StampB != nil && in.StampB.AllowLiveInstance),
		RegimeLabels:      in.RegimeSplit.Stamp(),
	}
	env.GoldSHA256 = CombinedDigest(in.StampA.SliceFiles)
	env.Dumps = append(env.Dumps, dumpProvenance("A", in.StampA))
	if in.StampB != nil {
		env.Dumps = append(env.Dumps, dumpProvenance("B", *in.StampB))
	}
	return env
}

func dumpProvenance(role string, s DumpStamp) DumpProvenance {
	return DumpProvenance{
		Role: role, RunID: s.RunID, File: s.DumpFile, CreatedAt: s.CreatedAt, BaseURL: s.BaseURL,
		PinFile: s.PinFile, PinRunID: s.PinRunID, PinSHA256: s.PinSHA256,
		Records: s.Records, Drift: s.Drift, Latency: s.Latency,
	}
}

// CombinedDigest is the sha256 over the SORTED "file:sha256" lines — one value
// that moves if any slice file moved, in an order no directory listing decides.
func CombinedDigest(files []SliceDigest) string {
	lines := make([]string, 0, len(files))
	for _, f := range files {
		lines = append(lines, f.File+":"+f.SHA256)
	}
	sort.Strings(lines)
	return goldset.SHA256Hex(strings.Join(lines, "\n"))
}

// BuildSliceProfiles is the report's slice census. It walks CensusSlices, so a
// floor slice gets its own row — and carries RolloutCriterion=false on it —
// while every gate in this package keeps walking ReportSlices and never sees it.
func BuildSliceProfiles(recs []Record) []SliceProfile {
	counts := labelledCounts(recs)
	temporal := map[string]int{}
	for _, rec := range recs {
		if rec.EffectiveTemporal != "" {
			for _, k := range SliceKeysOf(rec) {
				temporal[k]++
			}
		}
	}
	floor := map[string]bool{}
	for _, s := range FloorSlices() {
		floor[s] = true
	}
	out := make([]SliceProfile, 0, len(CensusSlices()))
	for _, slice := range CensusSlices() {
		c, ok := counts[slice]
		if !ok {
			continue
		}
		p := SliceProfile{Slice: slice, N: c[0], Labelled: c[1],
			Unlabelled: c[1] == 0, TemporalShare: TemporalShare(c[0], temporal[slice]),
			RolloutCriterion: !floor[slice] && !IsStratum(slice)}
		switch {
		case floor[slice]:
			p.Note = "floor check: gold is constructive and circular against the layer it would judge — never a rollout criterion"
		case IsStratum(slice):
			p.Note = StratumNote
		case c[1] == 0:
			p.Note = "unlabelled, skipped — relevance judgements land in wave B-W6"
		case c[1] < c[0]:
			p.Note = fmt.Sprintf("partly labelled: %d of %d cases carry judgements; the rest score 0 by construction", c[1], c[0])
		}
		out = append(out, p)
	}
	return out
}

func unlabelledNotes(counts map[string][2]int) []string {
	var out []string
	for _, slice := range ReportSlices() {
		if c, ok := counts[slice]; ok && c[1] == 0 {
			out = append(out, fmt.Sprintf("slice %s: unlabelled, skipped (%d cases)", slice, c[0]))
		}
	}
	return out
}

// unionExcluded merges the exclusion lists of the dump pair (§4.9): a case
// excluded from EITHER dump is excluded from BOTH, or the two halves of the
// replicate comparison would run over different populations.
func unionExcluded(a DumpStamp, b *DumpStamp) []ExcludedCase {
	byKey := map[string]ExcludedCase{}
	for _, e := range a.Excluded {
		byKey[e.Key()] = e
	}
	if b != nil {
		for _, e := range b.Excluded {
			if _, ok := byKey[e.Key()]; !ok {
				byKey[e.Key()] = e
			}
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ExcludedCase, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out
}

func dropExcluded(recs []Record, excluded []ExcludedCase) []Record {
	if len(excluded) == 0 {
		return recs
	}
	drop := make(map[string]bool, len(excluded))
	for _, e := range excluded {
		drop[e.Key()] = true
	}
	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		if !drop[r.Key()] {
			out = append(out, r)
		}
	}
	return out
}

// MarshalBody renders the report body deterministically.
func MarshalBody(body ReportBody) ([]byte, error) {
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteReport writes header line + body. The header carries the only volatile
// field, so a diff of two reports over the same dump is empty from line 2 on.
func WriteReport(path, generatedAt string, body ReportBody) error {
	b, err := MarshalBody(body)
	if err != nil {
		return err
	}
	hdr, err := json.Marshal(ReportHeader{
		Tool: "ctx-armsweep", GeneratedAt: generatedAt, BodySHA256: goldset.SHA256Hex(string(b)),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(append(hdr, '\n'), b...), fileMode)
}

// ReadReportBody reads back the body of a report file, skipping the header
// line — the shape the determinism gate compares.
func ReadReportBody(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	i := strings.IndexByte(string(b), '\n')
	if i < 0 {
		return nil, fmt.Errorf("%s: no header line", path)
	}
	return b[i+1:], nil
}
