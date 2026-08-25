package armsweep

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// DriftStamp is one census of the corpus the measurement runs against — taken
// immediately before and immediately after a dump (design 04 §4.7 (2)).
//
// A dump is a measurement of a moving object: the store is live, dream writes,
// the embed backfill fills vectors, and a 650-query run takes minutes. The
// stamp does not stop any of that. It makes the movement VISIBLE, and the three
// rules in EvaluateDrift decide which movement invalidates the run rather than
// merely annotating it.
type DriftStamp struct {
	// At is the SERVER's clock at census time. The comparison window for gold
	// mutations is [before.At, after.At], so it must come from the same clock
	// the updated_at values do — a driver-side timestamp would be a different
	// clock and could bracket the window wrongly by exactly the skew.
	At                string        `json:"at"`
	RetrievableBlocks int           `json:"retrievable_blocks"`
	Types             []TypeDrift   `json:"types"`
	GoldIDs           []GoldIDState `json:"gold_ids"`
}

// TypeDrift is the per-type census: the four numbers §4.7 (2) names.
type TypeDrift struct {
	TypeName string `json:"type_name"`
	// Retrievable is the type's retrieval class. The NULL-embedding rule is
	// evaluated over retrievable types ONLY: the live corpus carries thousands
	// of null embeddings in retrieval=excluded types (checkpoint) as standing
	// policy, and a rule that counted those would abort every run.
	Retrievable   bool   `json:"retrievable"`
	Count         int    `json:"count"`
	MaxCreatedAt  string `json:"max_created_at"`
	MaxUpdatedAt  string `json:"max_updated_at"`
	NullEmbedding int    `json:"null_embedding"`
}

// GoldIDState is the lifecycle of one labelled block at census time. An id the
// server did not return is a DELETED row, which is why the driver compares
// membership and not only timestamps.
type GoldIDState struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// RetrievableDriftTolerance is the §4.7 threshold on the retrievable block
// count: more than ±0.5 % movement between the two censuses and the dump is
// discarded. Below it the corpus is the same object; above it the two halves of
// the run measured different populations.
const RetrievableDriftTolerance = 0.005

// DriftVerdict is the outcome of the §4.7 rules over a census pair.
type DriftVerdict struct {
	Abort   bool     `json:"abort"`
	Reasons []string `json:"reasons"`
	// Notes are movements that are visible but not disqualifying — the record
	// that the corpus moved and by how much.
	Notes []string `json:"notes"`
}

// EvaluateDrift applies the three hard-abort rules of §4.7 plus the §5.3 b
// contamination probe to a census pair.
//
// corpusMaxCreatedAt is STAMP.json's corpus_max_created_at: the instant the
// gold set was drawn. A labelled block created AFTER it cannot have been a
// label at draw time, so its presence means the gold set and the corpus have
// diverged — the exact shape of contamination the gold stamp exists to detect.
//
// Reasons are formulated so a report can carry them verbatim: they name ids,
// types and numbers, never query texts or block content.
func EvaluateDrift(before, after DriftStamp, corpusMaxCreatedAt string) DriftVerdict {
	var v DriftVerdict
	v.Reasons = append(v.Reasons, goldIDReasons(before, after, corpusMaxCreatedAt)...)
	v.Reasons = append(v.Reasons, nullEmbeddingReasons(before, after)...)

	lo := float64(before.RetrievableBlocks) * (1 - RetrievableDriftTolerance)
	hi := float64(before.RetrievableBlocks) * (1 + RetrievableDriftTolerance)
	got := float64(after.RetrievableBlocks)
	switch {
	case before.RetrievableBlocks == 0:
		v.Reasons = append(v.Reasons, "retrievable block count was 0 before the run — nothing to measure against")
	case got < lo || got > hi:
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"retrievable blocks moved %d → %d (%.3f %%), tolerance ±%.1f %%",
			before.RetrievableBlocks, after.RetrievableBlocks,
			100*(got-float64(before.RetrievableBlocks))/float64(before.RetrievableBlocks),
			100*RetrievableDriftTolerance))
	case after.RetrievableBlocks != before.RetrievableBlocks:
		v.Notes = append(v.Notes, fmt.Sprintf("retrievable blocks moved %d → %d, inside tolerance",
			before.RetrievableBlocks, after.RetrievableBlocks))
	}

	if delta := typeCountDeltas(before, after); len(delta) > 0 {
		v.Notes = append(v.Notes, delta...)
	}
	v.Abort = len(v.Reasons) > 0
	return v
}

// goldIDReasons covers the two gold-label rules and the contamination probe.
func goldIDReasons(before, after DriftStamp, corpusMaxCreatedAt string) []string {
	var out []string
	beforeAt, beforeErr := parseStampTime(before.At)
	afterAt, afterErr := parseStampTime(after.At)
	if beforeErr != nil || afterErr != nil {
		return []string{fmt.Sprintf("drift census timestamps unparsable (before %q, after %q)", before.At, after.At)}
	}
	drawn, drawnErr := parseStampTime(corpusMaxCreatedAt)

	beforeIDs := map[string]GoldIDState{}
	for _, g := range before.GoldIDs {
		beforeIDs[g.ID] = g
	}
	afterIDs := map[string]GoldIDState{}
	for _, g := range after.GoldIDs {
		afterIDs[g.ID] = g
	}

	for _, id := range sortedGoldIDs(beforeIDs, afterIDs) {
		b, hadBefore := beforeIDs[id]
		a, hasAfter := afterIDs[id]
		switch {
		case !hadBefore:
			out = append(out, fmt.Sprintf("gold block %s was absent from the corpus before the run", id))
			continue
		case !hasAfter:
			out = append(out, fmt.Sprintf("gold block %s disappeared during the run", id))
			continue
		}
		if u, err := parseStampTime(a.UpdatedAt); err == nil {
			if !u.Before(beforeAt) && !u.After(afterAt) {
				out = append(out, fmt.Sprintf("gold block %s was updated during the run (updated_at %s)", id, a.UpdatedAt))
			}
		}
		if a.UpdatedAt != b.UpdatedAt {
			out = append(out, fmt.Sprintf("gold block %s changed updated_at %s → %s", id, b.UpdatedAt, a.UpdatedAt))
		}
		if drawnErr == nil {
			if c, err := parseStampTime(a.CreatedAt); err == nil && c.After(drawn) {
				out = append(out, fmt.Sprintf(
					"contamination: gold block %s was created %s, after the gold stamp's corpus_max_created_at %s",
					id, a.CreatedAt, corpusMaxCreatedAt))
			}
		}
	}
	return dedupe(out)
}

// nullEmbeddingReasons implements the 0 → >0 rule over retrievable types.
func nullEmbeddingReasons(before, after DriftStamp) []string {
	prior := map[string]TypeDrift{}
	for _, t := range before.Types {
		prior[t.TypeName] = t
	}
	var out []string
	for _, t := range after.Types {
		if !t.Retrievable || t.NullEmbedding == 0 {
			continue
		}
		if p, ok := prior[t.TypeName]; ok && p.NullEmbedding == 0 {
			out = append(out, fmt.Sprintf(
				"type %q gained null embeddings during the run (0 → %d) — the semantic arm lost candidates mid-dump",
				t.TypeName, t.NullEmbedding))
		}
	}
	sort.Strings(out)
	return out
}

// typeCountDeltas records per-type movement as notes, never as an abort: a
// growing non-gold type is ordinary life in a live store, and the aggregate
// tolerance above is the rule that decides.
func typeCountDeltas(before, after DriftStamp) []string {
	prior := map[string]int{}
	for _, t := range before.Types {
		prior[t.TypeName] = t.Count
	}
	var out []string
	for _, t := range after.Types {
		if p, ok := prior[t.TypeName]; ok && p != t.Count {
			out = append(out, fmt.Sprintf("type %q count %d → %d", t.TypeName, p, t.Count))
		} else if !ok {
			out = append(out, fmt.Sprintf("type %q appeared during the run (%d blocks)", t.TypeName, t.Count))
		}
	}
	sort.Strings(out)
	return out
}

func sortedGoldIDs(a, b map[string]GoldIDState) []string {
	seen := map[string]bool{}
	var out []string
	for id := range a {
		if !seen[id] {
			seen[id], out = true, append(out, id)
		}
	}
	for id := range b {
		if !seen[id] {
			seen[id], out = true, append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// parseStampTime accepts the two shapes the API emits for a timestamp.
func parseStampTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02T15:04:05.999999Z07:00", s)
}

// TemporalShare is the fraction of a slice's cases that received a temporal FTS
// expansion — a stamp field, because a slice that is 40 % temporal and one that
// is 2 % temporal are not comparable instruments even at identical n.
func TemporalShare(n, temporal int) float64 {
	if n == 0 {
		return 0
	}
	return math.Round(float64(temporal)/float64(n)*10000) / 10000
}
