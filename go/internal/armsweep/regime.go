package armsweep

// Wave X-W0b: the G-REAL regime strata as report rows of their own.
//
// The design asks for this at design/05 §7 (row X-W0, "the slice registry
// carries the two halves as rows of their own afterwards") for the reason given
// in §4.4b: local and global have measured OPPOSITE winners, so a single G-REAL
// figure is a mean over two regimes that cancel each other out.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/goldset"
)

// RegimeSplit is the X-W0 label file as the offline steps consume it: the
// digest→regime map plus the provenance of the file it came from.
//
// The zero value is INACTIVE, and that is the whole continuity guarantee of
// this wave: a run that does not supply labels produces exactly the bytes this
// instrument produced before it — the same shape M-W8 gave the damping curve.
type RegimeSplit struct {
	// File is the name the report cites, relative to the gold directory.
	File string
	// SHA256 binds the report to the exact label bytes the split was read from.
	SHA256 string
	// Regimes maps a case's query_sha256 to goldset.RegimeLocal/RegimeGlobal.
	Regimes map[string]string
}

// Active reports whether a split was supplied at all.
func (rs RegimeSplit) Active() bool { return len(rs.Regimes) > 0 }

// Stamp is the provenance block the report carries, or nil when inactive — the
// field is omitempty, so an inactive split leaves no byte behind.
func (rs RegimeSplit) Stamp() *RegimeStamp {
	if !rs.Active() {
		return nil
	}
	return &RegimeStamp{File: rs.File, SHA256: rs.SHA256, Labels: len(rs.Regimes)}
}

// RegimeStamp is the §4.8 provenance of the stratification: which label file
// fed the split rows, and its digest. A report that prints "G-REAL-global n=19"
// without it would state a partition no reader can reproduce.
type RegimeStamp struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Labels int    `json:"labels"`
}

// ErrRegimeLabelMissing refuses a stratified report over a case set the label
// file does not cover.
//
// It is an error rather than a note for the same reason ErrDumpPredatesTypeName
// is (report.go:132-141): the failure it prevents is SILENT. An unlabelled
// G-REAL case would fall into neither half, both halves would still add up to
// something, and nothing in the report would say that the partition is short
// the cases it dropped. There is no defensible default — a "rest half" is a
// figure about a set nobody defined.
//
// The total G-REAL row is never affected: the split is an addition, so a
// refusal costs the split rows, not the report they would have joined.
var ErrRegimeLabelMissing = errors.New("Regime-Split verweigert: die X-W0-Labels decken nicht jeden G-REAL-Fall des Dumps ab — eine Rest-Hälfte wäre eine Zahl über eine Menge, die niemand definiert hat")

// StratumSlices are the two regime rows of G-REAL.
//
// They are REPORTED rows and never gate inputs, and the reason is sharper than
// the one FloorSlices carries: a stratum is a SUBSET of a row that already
// votes. Letting both vote would count the same cases twice — once in G-REAL
// and once in its half — in G-NOISE's interpretability conjunction and in
// G-WIN's regression veto. Those two keep walking ReportSlices; the strata
// carry metrics, effects, displacement and their own MDE (§4.4b), which is
// exactly what the split exists for.
func StratumSlices() []string { return []string{SliceRealLocal, SliceRealGlobal} }

// StratumNote is what a stratum row says about itself in every report. The
// separation is structural (StratumSlices), and the row states it too: a reader
// of the JSON must not be able to mix a half into an argument the total row
// already made.
const StratumNote = "X-W0 regime stratum of " + SliceRealName +
	": a re-partition of that row, reported per regime (§4.4b) — never a rollout criterion of its own, its cases already vote in the total row"

// IsStratum reports whether a report row is one of the G-REAL regime strata.
func IsStratum(slice string) bool {
	return slice == SliceRealLocal || slice == SliceRealGlobal
}

// SliceKeysOf is SliceKeyOf plus the stratum a record belongs to: a G-REAL case
// with a regime stamped on it appears in the total row AND in exactly one half.
//
// Both, not one: the total row is the continuity of every report written before
// this wave, and the halves are the resolution §4.4b asks for. Replacing the
// total by its halves would silently change what an existing G-REAL figure
// means.
func SliceKeysOf(rec Record) []string {
	key := SliceKeyOf(rec)
	if rec.Slice != goldset.SliceReal || rec.Regime == "" {
		return []string{key}
	}
	return []string{key, key + "-" + rec.Regime}
}

// stampRegime copies a record with its X-W0 regime filled in, or refuses.
//
// Only G-REAL cases are looked up: the other slices are constructive and carry
// no regime label, and demanding one for them would refuse every run.
func stampRegime(rec Record, rs RegimeSplit) (Record, error) {
	if !rs.Active() || rec.Slice != goldset.SliceReal {
		return rec, nil
	}
	regime, ok := rs.Regimes[rec.QuerySHA256]
	if !ok {
		return rec, fmt.Errorf("%s (%s) ohne Regime-Label in %q: %w",
			rec.Key(), ShortSHA(rec.QuerySHA256), rs.File, ErrRegimeLabelMissing)
	}
	rec.Regime = regime
	return rec, nil
}

// applyRegime stamps a whole record set, refusing on the first uncovered case.
// The input slice is never mutated: `score` hands the same records to several
// passes, and a partially stamped set would stratify differently per pass.
func applyRegime(recs []Record, rs RegimeSplit) ([]Record, error) {
	if !rs.Active() || len(recs) == 0 {
		return recs, nil
	}
	out := make([]Record, len(recs))
	for i, rec := range recs {
		stamped, err := stampRegime(rec, rs)
		if err != nil {
			return nil, err
		}
		out[i] = stamped
	}
	return out, nil
}
