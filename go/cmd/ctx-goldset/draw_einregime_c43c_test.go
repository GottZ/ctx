package main

// Wave C4-3c: the flag side of the single-regime core draw.
//
// `-core-queries local,global` is the only allocation flag on which a 0 has a
// defined meaning. It names two POPULATIONS, and a slice can genuinely have
// none in one of them — G-GLOB's 80 cases are corpus aggregations, all labelled
// `global`. `-strata` names five SAMPLE SIZES inside populations that are drawn
// from by construction; a 0 there would be a stratum with weight N/0, so it
// stays strictly positive and this file pins that it does.
//
// The fixture is synthetic, like the C4-3b ones: no test reads the private gold
// directory.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/goldset"
)

// c43cSeed is the draw seed of this file. Fixed, because the seed decides which
// queries become the core and a varying one would make the assertions vary.
const c43cSeed = int64(20260829)

// c43cFixture builds `queries` single-regime queries with 20 pooled candidates
// each (5 per stratum) and 5 control draws, small enough that the whole draw is
// a handful of milliseconds and large enough that all four strata populate.
func c43cFixture(queries int, regime string) goldset.DrawInput {
	in := goldset.DrawInput{
		SourceRun: "c43c-fixture",
		Judged:    map[string][]goldset.Judgement{},
		Key: goldset.PoolKey{
			Version: 1, Seed: c43cSeed, Controls: 5, ControlIDs: map[string][]string{},
		},
		Regimes: map[string]string{},
	}
	for q := 0; q < queries; q++ {
		sha := goldset.SHA256Hex(fmt.Sprintf("c43c-cmd-query-%02d", q))
		k := goldset.CaseKey(goldset.SliceReal, q, sha)
		in.Regimes[sha] = regime
		entry := goldset.PoolEntry{Slice: goldset.SliceReal, Index: q, QuerySHA256: sha}
		for c := 0; c < 20; c++ {
			id := fmt.Sprintf("blk-%02d-%02d", q, c)
			in.Cells = append(in.Cells, goldset.JudgeCell{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha,
				Query: fmt.Sprintf("Aggregat %02d?", q), BlockID: id,
				Title: "Titel " + id, Excerpt: "Auszug " + id,
			})
			switch c / 5 {
			case 0: // two arms, judge=1 → S1
				entry.Semantic = append(entry.Semantic, id)
				entry.FTSDe = append(entry.FTSDe, id)
			case 1: // one arm, judge=1 → S2
				entry.Semantic = append(entry.Semantic, id)
			case 2: // one arm, head rank, judge=0 → S3
				entry.Trigram = append(entry.Trigram, id)
			default: // one arm, tail rank, judge=0 → S4
				for len(entry.FTSEn) < 10 {
					entry.FTSEn = append(entry.FTSEn, fmt.Sprintf("pad-%02d-%02d", q, len(entry.FTSEn)))
				}
				entry.FTSEn = append(entry.FTSEn, id)
			}
			in.Judged[k] = append(in.Judged[k], goldset.Judgement{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha, BlockID: id, Relevant: c/5 <= 1,
			})
		}
		for c := 0; c < 5; c++ {
			id := fmt.Sprintf("ctl-%02d-%02d", q, c)
			in.Cells = append(in.Cells, goldset.JudgeCell{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha,
				Query: fmt.Sprintf("Aggregat %02d?", q), BlockID: id,
				Title: "Titel " + id, Excerpt: "Auszug " + id,
			})
			in.Judged[k] = append(in.Judged[k], goldset.Judgement{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha, BlockID: id, Relevant: false,
			})
			in.Key.ControlIDs[k] = append(in.Key.ControlIDs[k], id)
		}
		in.Pool = append(in.Pool, entry)
	}
	return in
}

// TestDrawSpecCoreZeroC43C is the flag semantics: 0 is accepted for exactly one
// regime of `-core-queries`, refused for both, refused when negative, and
// `-strata` keeps refusing every 0 with the message it already had.
func TestDrawSpecCoreZeroC43C(t *testing.T) {
	t.Run("0 in einem Regime wird angenommen", func(t *testing.T) {
		for _, tc := range []struct {
			in            string
			local, global int
		}{
			{"0,12", 0, 12},
			{"12,0", 12, 0},
			{" 0 , 12 ", 0, 12},
		} {
			spec, err := drawSpecOf(judgeOpts{drawSeed: c43cSeed, coreQueries: tc.in})
			if err != nil {
				t.Errorf("-core-queries %q wurde abgewiesen: %v", tc.in, err)
				continue
			}
			if spec.CoreLocal != tc.local || spec.CoreGlobal != tc.global {
				t.Errorf("-core-queries %q ergab %d/%d, erwartet %d/%d",
					tc.in, spec.CoreLocal, spec.CoreGlobal, tc.local, tc.global)
			}
		}
	})
	t.Run("Fehler-Erhalt", func(t *testing.T) {
		for _, tc := range []struct{ in, contains string }{
			{"0,0", "beide"},
			{"-1,12", "≥ 0"},
			{"12,-1", "≥ 0"},
			{"a,12", "≥ 0"},
			{"12", "erwartet 2 Zahlen"},
			{"1,2,3", "erwartet 2 Zahlen"},
		} {
			if _, err := drawSpecOf(judgeOpts{drawSeed: c43cSeed, coreQueries: tc.in}); err == nil {
				t.Errorf("-core-queries %q wurde angenommen", tc.in)
			} else if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("-core-queries %q: Meldung %q nennt %q nicht", tc.in, err.Error(), tc.contains)
			}
		}
	})
	// Non-regression: the strata flag is untouched, including its wording. A 0
	// there would mean a stratum weight of N/0.
	t.Run("-strata bleibt strikt positiv", func(t *testing.T) {
		_, err := drawSpecOf(judgeOpts{drawSeed: c43cSeed, strata: "0,140,140,80,60"})
		if err == nil {
			t.Fatal("-strata nahm eine 0 an — eine Schicht ohne Zellen trägt keine Hochrechnung")
		}
		const want = `-strata: "0" ist keine positive Zahl`
		if err.Error() != want {
			t.Errorf("-strata-Meldung %q, unverändert erwartet %q", err.Error(), want)
		}
		if _, err := drawSpecOf(judgeOpts{drawSeed: c43cSeed, strata: "120,140,140,80,60"}); err != nil {
			t.Errorf("gültige -strata wurden abgewiesen: %v", err)
		}
	})
	// The default stays 14/6 when the flag is absent.
	t.Run("Vorgabe unverändert 14/6", func(t *testing.T) {
		spec, err := drawSpecOf(judgeOpts{drawSeed: c43cSeed})
		if err != nil {
			t.Fatal(err)
		}
		if spec.CoreLocal != 14 || spec.CoreGlobal != 6 {
			t.Errorf("Vorgabe %d/%d, festgeschrieben 14/6", spec.CoreLocal, spec.CoreGlobal)
		}
	})
}

// TestDrawSpecSingleRegimeEndToEndC43C runs the whole flag path: the string a
// lead types on the command line becomes a spec, and that spec draws a core out
// of a slice that has only one regime.
func TestDrawSpecSingleRegimeEndToEndC43C(t *testing.T) {
	spec, err := drawSpecOf(judgeOpts{
		drawSeed: c43cSeed, coreQueries: "0,12", strata: "20,20,20,20,20",
	})
	if err != nil {
		t.Fatalf(`-core-queries "0,12": %v`, err)
	}
	in := c43cFixture(20, goldset.RegimeGlobal)
	in.Spec = spec
	key, err := goldset.Draw(in)
	if err != nil {
		t.Fatalf("Ziehung über den Ein-Regime-Slice: %v", err)
	}
	if len(key.CoreQueries) != 12 {
		t.Fatalf("Kern: %d Queries, erwartet 12", len(key.CoreQueries))
	}
	for _, q := range key.CoreQueries {
		if q.Regime != goldset.RegimeGlobal {
			t.Errorf("Kern-Query %s im Regime %q, erwartet %q", q.QuerySHA256[:8], q.Regime, goldset.RegimeGlobal)
		}
	}
	if key.Spec.CoreLocal != 0 || key.Spec.CoreGlobal != 12 {
		t.Errorf("der Schlüssel schreibt %d/%d fest, gezogen wurde mit 0/12",
			key.Spec.CoreLocal, key.Spec.CoreGlobal)
	}
}
