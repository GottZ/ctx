package goldset_test

// Wave C4-3c: the core draw of a SINGLE-REGIME slice.
//
// G-GLOB is a slice on which the X-W0 regime partition does not apply: all 80
// cases are corpus aggregations (origin `tag-aggregate`), and the partition
// into `local`/`global` is a property of G-REAL (regime.go:3-7). Its label file
// therefore carries 80 identical `global` rows, and the core allocation has to
// be able to say "nothing from local" — not as a number the tool guesses, but
// as a stated 0.
//
// The three properties this file holds apart:
//
//	(1) a 0 in ONE regime draws nothing there and is not an error,
//	(2) a 0 in BOTH is an error — a key without a core is a calibration
//	    without its census anchor, and the HT weights rest on that anchor,
//	(3) the 14/6 G-REAL path is unchanged down to the byte.
//
// The fixtures are synthetic, like the C3-4a ones: the real gold directory is
// private and root-only, so a test bound to it could run nowhere but on this
// machine.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/goldset"
)

// buildRegimeFixture lays out `queries` queries that ALL carry the same regime
// label — the G-GLOB shape. The per-query cell layout is the C3-4a one
// (fxCandidates candidates in four stratum groups plus fxControls controls), so
// the strata populate exactly as they do in the two-regime fixture and nothing
// but the labelling differs.
func buildRegimeFixture(queries int, regime string) drawFixture {
	fx := drawFixture{
		judged:  map[string][]goldset.Judgement{},
		key:     goldset.PoolKey{Version: 1, Seed: 20260812, Controls: fxControls, ControlIDs: map[string][]string{}},
		regimes: map[string]string{},
	}
	for q := 0; q < queries; q++ {
		sha := goldset.SHA256Hex(fmt.Sprintf("c43c-query-%02d", q))
		k := goldset.CaseKey(goldset.SliceReal, q, sha)
		fx.regimes[sha] = regime
		entry := goldset.PoolEntry{Slice: goldset.SliceReal, Index: q, QuerySHA256: sha}
		for c := 0; c < fxCandidates; c++ {
			id := fmt.Sprintf("blk-%02d-%02d", q, c)
			fx.cells = append(fx.cells, goldset.JudgeCell{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha,
				Query: fmt.Sprintf("Aggregat %02d?", q), BlockID: id,
				Title: "Titel " + id, Excerpt: "Auszug " + id,
			})
			group := c / 10
			switch group {
			case 0: // two arms → judge=1 with >=2 arms → S1
				entry.Semantic = append(entry.Semantic, id)
				entry.FTSDe = append(entry.FTSDe, id)
			case 1: // one arm → judge=1 → S2
				entry.Semantic = append(entry.Semantic, id)
			case 2: // one arm, head ranks 1..10 → judge=0 → S3
				entry.Trigram = append(entry.Trigram, id)
			default: // one arm, ranks 11..20 → judge=0 → S4
				for len(entry.FTSEn) < 10 {
					entry.FTSEn = append(entry.FTSEn, fmt.Sprintf("pad-%02d-%02d", q, len(entry.FTSEn)))
				}
				entry.FTSEn = append(entry.FTSEn, id)
			}
			fx.judged[k] = append(fx.judged[k], goldset.Judgement{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha, BlockID: id, Relevant: group <= 1,
			})
		}
		for c := 0; c < fxControls; c++ {
			id := fmt.Sprintf("ctl-%02d-%02d", q, c)
			fx.cells = append(fx.cells, goldset.JudgeCell{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha,
				Query: fmt.Sprintf("Aggregat %02d?", q), BlockID: id,
				Title: "Titel " + id, Excerpt: "Auszug " + id,
			})
			fx.judged[k] = append(fx.judged[k], goldset.Judgement{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha, BlockID: id, Relevant: false,
			})
			fx.key.ControlIDs[k] = append(fx.key.ControlIDs[k], id)
		}
		fx.pool = append(fx.pool, entry)
	}
	return fx
}

// singleRegimeSpec is the G-GLOB allocation: no local core, 12 global core
// queries, the C3-4a strata.
func singleRegimeSpec(seed int64) goldset.DrawSpec {
	s := goldset.DefaultDrawSpec(seed)
	s.CoreLocal, s.CoreGlobal = 0, 12
	return s
}

// TestDrawSingleRegimeCoreC43C is the C4-3c gate: on a slice whose queries all
// carry one regime, a core allocation of 0/12 draws 12 queries from `global`,
// none from `local`, and is deterministic.
func TestDrawSingleRegimeCoreC43C(t *testing.T) {
	fx := buildRegimeFixture(80, goldset.RegimeGlobal)
	spec := singleRegimeSpec(20260829)
	key, err := goldset.Draw(fx.input(spec))
	if err != nil {
		t.Fatalf("Ein-Regime-Ziehung mit Kern 0/12: %v", err)
	}
	if len(key.CoreQueries) != 12 {
		t.Fatalf("Kern: %d Queries, erwartet 12", len(key.CoreQueries))
	}
	local, global := 0, 0
	for _, q := range key.CoreQueries {
		switch q.Regime {
		case goldset.RegimeLocal:
			local++
		case goldset.RegimeGlobal:
			global++
		}
	}
	if local != 0 || global != 12 {
		t.Errorf("Kern-Regime: local=%d global=%d, erwartet 0/12", local, global)
	}
	// The core stays a census: every non-control cell of a core query is drawn.
	core := map[string]int{}
	for _, c := range key.Cells {
		if c.Stratum == goldset.StratumCore {
			core[c.QuerySHA256]++
			if c.Weight != 1 {
				t.Errorf("Kern-Zelle mit Gewicht %.4f, erwartet 1", c.Weight)
			}
		}
	}
	if len(core) != 12 {
		t.Errorf("Kern-Zellen verteilen sich auf %d Queries, erwartet 12", len(core))
	}
	for sha, n := range core {
		if n != fxCandidates {
			t.Errorf("Kern-Query %s: %d Zellen, erwartet %d (vollständig)", sha[:8], n, fxCandidates)
		}
	}
	// The strata are untouched by the core allocation and keep hitting their
	// numbers with the HT weight N_h/n_h.
	want := map[string]int{
		goldset.StratumS1: 120, goldset.StratumS2: 140,
		goldset.StratumS3: 140, goldset.StratumS4: 80, goldset.StratumS0: 60,
	}
	got := map[string]int{}
	for _, c := range key.Cells {
		got[c.Stratum]++
	}
	for s, n := range want {
		if got[s] != n {
			t.Errorf("Schicht %s: %d Zellen, erwartet %d", s, got[s], n)
		}
	}
	// Determinism: two draws of one seed are byte-identical, and the key is a
	// function of its inputs rather than of the clock.
	k2, err := goldset.Draw(fx.input(spec))
	if err != nil {
		t.Fatalf("zweiter Lauf: %v", err)
	}
	j1, err := goldset.MarshalDrawKey(key)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := goldset.MarshalDrawKey(k2)
	if err != nil {
		t.Fatal(err)
	}
	if string(j1) != string(j2) {
		t.Error("Ein-Regime-Schlüssel nicht byte-identisch zwischen zwei Läufen")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(j1, &raw); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"created_at", "drawn_at", "timestamp"} {
		if _, bad := raw[f]; bad {
			t.Errorf("der Ziehungs-Schlüssel führt %q — er wäre dann eine Funktion der Uhr", f)
		}
	}
	// The mirrored slice must work the same way: a local-only slice with 12/0.
	mirror := buildRegimeFixture(80, goldset.RegimeLocal)
	ms := goldset.DefaultDrawSpec(20260829)
	ms.CoreLocal, ms.CoreGlobal = 12, 0
	mk, err := goldset.Draw(mirror.input(ms))
	if err != nil {
		t.Fatalf("Ein-Regime-Ziehung mit Kern 12/0: %v", err)
	}
	if len(mk.CoreQueries) != 12 {
		t.Errorf("gespiegelter Kern: %d Queries, erwartet 12", len(mk.CoreQueries))
	}
	for _, q := range mk.CoreQueries {
		if q.Regime != goldset.RegimeLocal {
			t.Errorf("gespiegelter Kern zog Regime %q, erwartet %q", q.Regime, goldset.RegimeLocal)
		}
	}
}

// drawErr runs a draw that is expected to be REFUSED and turns a panic into a
// failure of its own. A slice expression on a negative allocation panics rather
// than returning, and "the tool crashed" is not the same answer as "the tool
// refused" — only the second one is a fail-closed rejection.
func drawErr(t *testing.T, in goldset.DrawInput) error {
	t.Helper()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Draw geriet in Panik statt einen Fehler zu liefern: %v", r)
				err = fmt.Errorf("Panik: %v", r)
			}
		}()
		_, err = goldset.Draw(in)
	}()
	return err
}

// TestDrawCoreZeroFailClosedC43C holds the four rejections the 0-semantics must
// NOT weaken: both regimes at 0, a negative allocation, a population smaller
// than a positive allocation, and a query without a regime label.
func TestDrawCoreZeroFailClosedC43C(t *testing.T) {
	fx := buildRegimeFixture(80, goldset.RegimeGlobal)
	for _, tc := range []struct {
		name              string
		local, global     int
		wantErrorContains string
	}{
		{"beide Regime 0", 0, 0, "Kern"},
		{"local negativ", -1, 12, "negativ"},
		{"global negativ", 12, -1, "negativ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := goldset.DefaultDrawSpec(20260829)
			spec.CoreLocal, spec.CoreGlobal = tc.local, tc.global
			err := drawErr(t, fx.input(spec))
			if err == nil {
				t.Fatalf("Kern %d/%d wurde angenommen — die Ziehung trüge keinen Zensus-Anker",
					tc.local, tc.global)
			}
			if !strings.Contains(err.Error(), tc.wantErrorContains) {
				t.Errorf("Fehlermeldung %q nennt %q nicht", err.Error(), tc.wantErrorContains)
			}
		})
	}
	// A positive allocation over a population that cannot serve it stays an
	// error — the 0-semantics must not turn "too few" into "draw what there is".
	t.Run("Population zu klein", func(t *testing.T) {
		small := buildRegimeFixture(5, goldset.RegimeGlobal)
		spec := singleRegimeSpec(20260829)
		spec.S1, spec.S2, spec.S3, spec.S4, spec.S0 = 5, 5, 5, 5, 5
		err := drawErr(t, small.input(spec))
		if err == nil {
			t.Fatal("5 Queries bei Kern-Anforderung 12 wurden angenommen")
		}
		if !strings.Contains(err.Error(), "der Kern verlangt 12") {
			t.Errorf("Fehlermeldung %q nennt die verlangte Zahl nicht", err.Error())
		}
	})
	// An unlabelled query stays an error too: the core is drawn per regime and
	// must not invent one, single-regime slice or not.
	t.Run("Query ohne Regime-Label", func(t *testing.T) {
		unlabelled := buildRegimeFixture(80, goldset.RegimeGlobal)
		for sha := range unlabelled.regimes {
			delete(unlabelled.regimes, sha)
			break
		}
		err := drawErr(t, unlabelled.input(singleRegimeSpec(20260829)))
		if err == nil {
			t.Fatal("ungelabelte Query wurde angenommen")
		}
		if !strings.Contains(err.Error(), "Regime-Label") {
			t.Errorf("Fehlermeldung %q nennt das fehlende Label nicht", err.Error())
		}
	})
}

// TestDrawDefaultKeyDigestC43C is the non-regression pin of the change: the
// 14/6 G-REAL draw over the C3-4a fixture must produce the SAME BYTES as
// before. The digest was taken from the unchanged tree at ceb77d2d — a pin on
// the marshalled key rather than on counted cells, because the counts would
// still match if the order or a weight had moved.
func TestDrawDefaultKeyDigestC43C(t *testing.T) {
	const wantDigest = "56955f26cbc372e468bdf1536182aab67f001ed71c382784cc499e5f0013a86d"
	fx := buildDrawFixture()
	spec := goldset.DefaultDrawSpec(20260829)
	if spec.CoreLocal != 14 || spec.CoreGlobal != 6 {
		t.Fatalf("die G-REAL-Vorgabe ist %d/%d, festgeschrieben sind 14/6", spec.CoreLocal, spec.CoreGlobal)
	}
	key, err := goldset.Draw(fx.input(spec))
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	b, err := goldset.MarshalDrawKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if got := goldset.SHA256Hex(string(b)); got != wantDigest {
		t.Errorf("14/6-Bestandspfad: Schlüssel-Digest %s, gepinnt %s — der Bestandspfad hat sich bewegt",
			got, wantDigest)
	}
}
