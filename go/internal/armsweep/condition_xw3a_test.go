package armsweep_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// Wave X-W3a, seam 2 — the declared condition (X-W2b N-A).
//
// `compare` refuses every post-fusion difference as an incongruent campaign
// (compare.go, stampMismatches) because design/05 §4.3 demands it. §7 X-W2b
// then defines the CONDITION of a whole wave as exactly such a difference: a
// settings write on cluster.inject_max. X-W2b measured the collision — exit 4,
// and the wave's contract step was unexecutable.
//
// The resolution is NOT a generic allow flag. It is a declaration of exactly
// ONE named congruence field as the condition of this comparison, with every
// other field staying hard, and with the consequence the declaration carries:
// a post-fusion stage is invisible in the ARM ranks by construction
// (fuse.go:22-27 — X-W2b measured byte-identical arm signatures across
// inject 0/3/20), so a declared post-fusion condition is measured on the
// DELIVERED ranking or it is measured on nothing at all.

// xw3aOpts are the knobs one synthetic dump of this wave differs in. The ARM
// rows are identical in every variant — that is the point.
type xw3aOpts struct {
	// postStage puts one id that exists in NO arm at delivered rank 1, which is
	// what the cluster injection does (rrf/cluster.go:639, after ctx_rrf).
	postStage bool
	// dropGold removes the gold id from the delivered window of that case: the
	// Hit@5 flip a McNemar discordance is counted on.
	dropGold func(i int) bool
	// noDelivered writes no delivered ranking at all — the artefact shape of a
	// dry run and of every dump written before the delivered block existed.
	noDelivered bool
}

// xw3aRecords builds one dump of labelledN labelled G-KI cases.
func xw3aRecords(o xw3aOpts) []armsweep.Record {
	out := make([]armsweep.Record, 0, labelledN)
	for i := 0; i < labelledN; i++ {
		ids := caseIDs(goldset.SliceKI, i)
		rec := armsweep.Record{
			Slice: goldset.SliceKI, Index: i,
			QuerySHA256:    goldset.SHA256Hex(fmt.Sprintf("%s/%d", goldset.SliceKI, i)),
			EffectiveQuery: "synthetic",
			GoldIDs:        []string{ids[0]},
			Selector: armsweep.Selector{
				Mode: "ann", Reason: "grey", Estimate: 1000, ScanTuples: intp(60000),
			},
			Attempts: 1, LatencyMS: int64(100 + i),
		}
		for pos, id := range ids {
			rec.Rows = append(rec.Rows, armRow(id, plainType, pos+1))
		}
		rec.FusionOrder = armsweep.FusedIDs(armsweep.Fuse(rec.Rows, armsweep.ConfigV0()))
		if !o.noDelivered {
			rec.Delivered = xw3aDelivered(o, i, rec.FusionOrder, ids[0])
		}
		out = append(out, rec)
	}
	return out
}

// xw3aDelivered is the live delivered window: the top five of the fusion,
// optionally with the gold id removed and a post-stage id pushed onto rank 1.
func xw3aDelivered(o xw3aOpts, i int, fusion []string, goldID string) []armsweep.Delivered {
	order := make([]string, 0, len(fusion))
	for _, id := range fusion {
		if o.dropGold != nil && o.dropGold(i) && id == goldID {
			continue
		}
		order = append(order, id)
	}
	out := make([]armsweep.Delivered, 0, armsweep.DisplacementCut)
	if o.postStage {
		out = append(out, armsweep.Delivered{ID: fmt.Sprintf("post-%03d", i), ViaPostStage: true})
	}
	for _, id := range order {
		if len(out) == armsweep.DisplacementCut {
			break
		}
		out = append(out, armsweep.Delivered{ID: id})
	}
	return out
}

// xw3aCampaign is a four-dump campaign whose condition dump differs from the
// base ONLY in the post-fusion stage state — the X-W2b shape.
type xw3aCampaign struct {
	base, cond, na, nb     armsweep.DumpRef
	baseRecs, condRecs     []armsweep.Record
	condStamp, noiseAStamp armsweep.DumpStamp
}

func newXW3aCampaign(t *testing.T, condOpts xw3aOpts) xw3aCampaign {
	t.Helper()
	dir := t.TempDir()
	c := xw3aCampaign{}
	c.baseRecs = xw3aRecords(xw3aOpts{})
	c.condRecs = xw3aRecords(condOpts)
	noiseRecs := xw3aRecords(xw3aOpts{})

	baseStamp := stampFor("BASE", "BASE.jsonl", c.baseRecs)
	c.condStamp = stampFor("COND", "COND.jsonl", c.condRecs)
	c.condStamp.PostFusionStages = xw3aStages(3)
	c.noiseAStamp = stampFor("NOISEA", "NOISEA.jsonl", noiseRecs)

	c.base = writeDump(t, dir, "BASE", c.baseRecs, baseStamp)
	c.cond = writeDump(t, dir, "COND", c.condRecs, c.condStamp)
	c.na = writeDump(t, dir, "NOISEA", noiseRecs, c.noiseAStamp)
	c.nb = writeDump(t, dir, "NOISEB", noiseRecs, stampFor("NOISEB", "NOISEB.jsonl", noiseRecs))
	return c
}

// xw3aStages is the post-fusion stage state at one cluster.inject_max — the
// exact shape X-W2b wrote into its stamps.
func xw3aStages(injectMax float64) map[string]any {
	return map[string]any{
		"cluster.enabled": true, "cluster.inject_max": injectMax,
		"graph.enabled": true, "rerank.enabled": false,
	}
}

func (c xw3aCampaign) input() armsweep.CompareInput {
	return armsweep.CompareInput{
		Base: c.base, Cond: c.cond,
		NoisePair:   []armsweep.DumpRef{c.na, c.nb},
		Seed:        20260812,
		GitRevision: "deadbeef",
		GoldStamp: goldset.Stamp{SampleSeed: 20260812, SplitSeed: 20260825,
			CorpusMaxCreatedAt: "2026-08-25T13:49:49.736510Z"},
	}
}

// ---------------------------------------------------------------- gate 2.1.

// TestXW3aUndeclaredPostFusionDifferenceStillRefuses is the half of the seam
// that must NOT move: without a declaration the post-fusion difference is an
// incongruent campaign, exactly as §4.3 says.
func TestXW3aUndeclaredPostFusionDifferenceStillRefuses(t *testing.T) {
	c := newXW3aCampaign(t, xw3aOpts{postStage: true, dropGold: func(i int) bool { return i%2 == 0 }})
	_, err := armsweep.Compare(c.input())
	if !errors.Is(err, armsweep.ErrStampIncongruent) {
		t.Fatalf("Compare = %v, erwartet ErrStampIncongruent", err)
	}
	if !strings.Contains(err.Error(), armsweep.ConditionFieldPostFusionStages) {
		t.Errorf("die Abweisung nennt das Feld nicht: %v", err)
	}
	// The refusal has to name the way out, or the §7↔§4.3 collision stays a
	// riddle for whoever hits it next.
	if !strings.Contains(err.Error(), "-condition-field") {
		t.Errorf("die Abweisung nennt die Deklaration nicht: %v", err)
	}
}

// ---------------------------------------------------------------- gate 2.2.

// TestXW3aDeclaredConditionRunsAndIsReported: with the declaration the
// comparison runs, and the report carries the declaration prominently — the
// declaration is the whole interpretation of the numbers below it.
func TestXW3aDeclaredConditionRunsAndIsReported(t *testing.T) {
	c := newXW3aCampaign(t, xw3aOpts{postStage: true, dropGold: func(i int) bool { return i%2 == 0 }})
	in := c.input()
	in.ConditionField = armsweep.ConditionFieldPostFusionStages

	body, err := armsweep.Compare(in)
	if err != nil {
		t.Fatalf("Compare mit Deklaration = %v, erwartet nil", err)
	}
	decl := body.Condition
	if decl == nil {
		t.Fatal("body.Condition ist nil — die Deklaration steht nicht im Report")
	}
	if decl.Field != armsweep.ConditionFieldPostFusionStages {
		t.Errorf("Condition.Field = %q, erwartet %q", decl.Field, armsweep.ConditionFieldPostFusionStages)
	}
	if decl.Basis != armsweep.RankingBasisDelivered {
		t.Errorf("Condition.Basis = %q, erwartet %q — eine Post-Fusion-Stufe ist in den Armen unsichtbar",
			decl.Basis, armsweep.RankingBasisDelivered)
	}
	if !decl.Applies {
		t.Error("Condition.Applies = false, obwohl Basis und Bedingung verschiedene Werte tragen")
	}
	if !strings.Contains(decl.BaseValue, `"cluster.inject_max":0`) ||
		!strings.Contains(decl.CondValue, `"cluster.inject_max":3`) {
		t.Errorf("die Deklaration druckt die Werte nicht: base=%q cond=%q", decl.BaseValue, decl.CondValue)
	}
	if decl.NoiseValue != decl.BaseValue {
		t.Errorf("Condition.NoiseValue = %q, erwartet den Basis-Wert %q", decl.NoiseValue, decl.BaseValue)
	}

	md := armsweep.RenderCompareMarkdown("2026-08-27T00:00:00Z", body)
	for _, want := range []string{
		"Deklarierte Bedingung",
		armsweep.ConditionFieldPostFusionStages,
		armsweep.RankingBasisDelivered,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("der Markdown-Report trägt %q nicht", want)
		}
	}

	t.Run("byte-identisch", func(t *testing.T) {
		a, err := armsweep.MarshalCompareBody(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		second, err := armsweep.Compare(in)
		if err != nil {
			t.Fatalf("zweiter Lauf: %v", err)
		}
		b, err := armsweep.MarshalCompareBody(second)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(a) != string(b) {
			t.Error("zwei Läufe über denselben Dump-Satz erzeugen verschiedene Bytes")
		}
	})
}

// ---------------------------------------------------------------- gate 2.3.

// TestXW3aDeclarationHardensEveryOtherField is the hardness probe: the
// declaration buys ONE field, never a second. A campaign that additionally
// disagrees on the schema generation is still exit 4.
func TestXW3aDeclarationHardensEveryOtherField(t *testing.T) {
	for name, mutate := range map[string]func(*armsweep.CompareInput){
		"migrations_max": func(in *armsweep.CompareInput) {
			in.Cond.Stamp.MigrationsMax = armsweep.TypeNameMigration + 1
		},
		"instance_kind": func(in *armsweep.CompareInput) {
			in.Cond.Stamp.InstanceKind = armsweep.InstanceKindLive
		},
		"hnsw.ef_search": func(in *armsweep.CompareInput) {
			in.Cond.Stamp.EfSearch = "200"
		},
		"pin_sha256": func(in *armsweep.CompareInput) {
			in.Cond.Stamp.PinSHA256 = goldset.SHA256Hex("andere pins")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newXW3aCampaign(t, xw3aOpts{postStage: true})
			in := c.input()
			in.ConditionField = armsweep.ConditionFieldPostFusionStages
			mutate(&in)
			_, err := armsweep.Compare(in)
			if !errors.Is(err, armsweep.ErrStampIncongruent) {
				t.Fatalf("Compare = %v, erwartet ErrStampIncongruent trotz Deklaration", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("die Abweisung nennt %q nicht: %v", name, err)
			}
		})
	}
}

// ---------------------------------------------------------------- gate 2.4.

// TestXW3aDeclaredConditionThatDidNotOccurIsNamed: declaring a field that does
// not differ is not an error — but it is never a silent success either. The
// comparison then measures a replicate and says so.
func TestXW3aDeclaredConditionThatDidNotOccurIsNamed(t *testing.T) {
	c := newXW3aCampaign(t, xw3aOpts{})
	in := c.input()
	in.Cond.Stamp.PostFusionStages = in.Base.Stamp.PostFusionStages
	in.ConditionField = armsweep.ConditionFieldPostFusionStages

	body, err := armsweep.Compare(in)
	if err != nil {
		t.Fatalf("Compare = %v, erwartet nil", err)
	}
	if body.Condition == nil {
		t.Fatal("body.Condition ist nil")
	}
	if body.Condition.Applies {
		t.Error("Condition.Applies = true, obwohl beide Seiten denselben Wert tragen")
	}
	if !xw3aAnyNote(body.Condition.Notes, "NICHT eingetreten") {
		t.Errorf("kein Hinweis auf die nicht eingetretene Bedingung: %v", body.Condition.Notes)
	}
	md := armsweep.RenderCompareMarkdown("2026-08-27T00:00:00Z", body)
	if !strings.Contains(md, "NICHT eingetreten") {
		t.Error("der Markdown-Report verschweigt die nicht eingetretene Bedingung")
	}
}

func xw3aAnyNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- gate 2.5.

// TestXW3aUndeclarableFieldIsRefused: the declaration is a closed list, not a
// generic escape. A field whose semantics nobody worked out cannot be declared.
func TestXW3aUndeclarableFieldIsRefused(t *testing.T) {
	for _, field := range []string{"migrations_max", "gold_sha256", "cluster.inject_max", "alles"} {
		t.Run(field, func(t *testing.T) {
			c := newXW3aCampaign(t, xw3aOpts{postStage: true})
			in := c.input()
			in.ConditionField = field
			_, err := armsweep.Compare(in)
			if !errors.Is(err, armsweep.ErrGateRefused) {
				t.Fatalf("Compare = %v, erwartet ErrGateRefused", err)
			}
			if !strings.Contains(err.Error(), armsweep.ConditionFieldPostFusionStages) {
				t.Errorf("die Abweisung nennt das deklarierbare Feld nicht: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------- gate 2.6.

// TestXW3aNoisePairMayNotStraddleTheDeclaredCondition: the replicate pair is a
// replicate in EVERY field, the declared one included. A noise pair whose
// halves sit on different sides of the condition measures the condition, not
// the instrument — and the whole comparison is read against it.
func TestXW3aNoisePairMayNotStraddleTheDeclaredCondition(t *testing.T) {
	c := newXW3aCampaign(t, xw3aOpts{postStage: true})
	in := c.input()
	in.ConditionField = armsweep.ConditionFieldPostFusionStages
	in.NoisePair[1].Stamp.PostFusionStages = xw3aStages(3)

	_, err := armsweep.Compare(in)
	if !errors.Is(err, armsweep.ErrStampIncongruent) {
		t.Fatalf("Compare = %v, erwartet ErrStampIncongruent — das Rausch-Paar überspannt die Bedingung", err)
	}
	if !strings.Contains(err.Error(), armsweep.RoleNoiseB) {
		t.Errorf("die Abweisung nennt die Rolle nicht: %v", err)
	}
}

// ---------------------------------------------------------------- gate 2.7.

// TestXW3aDeclaredConditionIsMeasuredOnTheDeliveredRanking is the reason the
// declaration cannot stop at the congruence check.
//
// The two dumps carry IDENTICAL arm rows — a post-fusion stage runs after
// ctx_rrf and cannot move them (fuse.go:22-27; X-W2b measured byte-identical
// arm signatures across inject 0/3/20). Scored on the offline fusion the
// comparison is therefore a tautology: exactly zero, with a full bootstrap CI
// around it. Only the delivered ranking carries the effect.
func TestXW3aDeclaredConditionIsMeasuredOnTheDeliveredRanking(t *testing.T) {
	c := newXW3aCampaign(t, xw3aOpts{postStage: true, dropGold: func(i int) bool { return i%2 == 0 }})

	// The tautology, measured on the fixture itself: the offline fusion of the
	// two dumps is the same ranking, case by case.
	cfg := armsweep.ConfigV0()
	for i := range c.baseRecs {
		b, cd := armsweep.ScoreCase(c.baseRecs[i], cfg), armsweep.ScoreCase(c.condRecs[i], cfg)
		if b != cd {
			t.Fatalf("Fall %d: die Fixture unterscheidet sich schon in der Fusion (%v gegen %v) — dann prüft dieser Test nichts", i, b, cd)
		}
	}

	in := c.input()
	in.ConditionField = armsweep.ConditionFieldPostFusionStages
	body, err := armsweep.Compare(in)
	if err != nil {
		t.Fatalf("Compare = %v, erwartet nil", err)
	}
	e, ok := effectOn(body, goldset.SliceKI)
	if !ok {
		t.Fatal("keine Effekt-Zeile auf G-KI")
	}
	if e.DeltaNDCG10 >= 0 {
		t.Errorf("ΔnDCG@10 = %+.5f, erwartet < 0 — auf der gelieferten Rangliste verliert die Bedingung Gold", e.DeltaNDCG10)
	}
	if e.Discordance <= e.NoiseDiscordance {
		t.Errorf("Diskordanz %.4f gegen Rauschboden %.4f — der Effekt ist nicht trennbar", e.Discordance, e.NoiseDiscordance)
	}
	if !e.Separable {
		t.Error("Separable = false, obwohl die Bedingung 20 von 40 Fällen kippt")
	}
	// And the caveat that comes with the basis: the delivered window is short.
	if body.Condition.DeliveredMaxLen != armsweep.DisplacementCut {
		t.Errorf("Condition.DeliveredMaxLen = %d, erwartet %d — die Kappung der Lieferliste muss im Report stehen",
			body.Condition.DeliveredMaxLen, armsweep.DisplacementCut)
	}
	if !xw3aAnyNote(body.Condition.Notes, "gelieferte") {
		t.Errorf("kein Hinweis auf die Basis der Kennzahlen: %v", body.Condition.Notes)
	}
	// The entrant of this condition is a post-stage id that stands in no arm.
	// It has to be NAMED as that; an empty type cell reads as missing data.
	row, ok := displacementOn(body, goldset.SliceKI)
	if !ok {
		t.Fatal("keine Verdrängungs-Zeile auf G-KI")
	}
	if len(row.EntrantsByType) == 0 {
		t.Fatal("die Verdrängungs-Tabelle zeigt keinen Eintretenden")
	}
	for _, c := range row.EntrantsByType {
		if strings.TrimSpace(c.TypeName) == "" {
			t.Errorf("ein Eintretender trägt einen leeren Typnamen (%d Fälle) — das liest sich als fehlende Angabe", c.N)
		}
	}
}

// TestXW3aUndeclaredComparisonStaysOnTheFusion is the non-regression half: no
// declaration, no basis change. The default comparison is the one M-W3d built.
func TestXW3aUndeclaredComparisonStaysOnTheFusion(t *testing.T) {
	c := newXW3aCampaign(t, xw3aOpts{postStage: true, dropGold: func(i int) bool { return i%2 == 0 }})
	in := c.input()
	// Make the campaign congruent again so the comparison actually runs.
	in.Cond.Stamp.PostFusionStages = in.Base.Stamp.PostFusionStages

	body, err := armsweep.Compare(in)
	if err != nil {
		t.Fatalf("Compare = %v, erwartet nil", err)
	}
	if body.Condition != nil {
		t.Errorf("body.Condition = %+v, erwartet nil ohne Deklaration", body.Condition)
	}
	e, ok := effectOn(body, goldset.SliceKI)
	if !ok {
		t.Fatal("keine Effekt-Zeile auf G-KI")
	}
	if e.DeltaNDCG10 != 0 || e.DeltaRecall5 != 0 || e.Discordance != 0 {
		t.Errorf("ohne Deklaration misst der Vergleich die Offline-Fusion: ΔnDCG=%+.5f ΔRecall=%+.5f Diskordanz=%.4f — erwartet 0/0/0",
			e.DeltaNDCG10, e.DeltaRecall5, e.Discordance)
	}
}

// ---------------------------------------------------------------- gate 2.8.

// TestXW3aDeliveredBasisRefusesADumpWithoutADeliveredRanking: a dump that
// carries no delivered block cannot answer a question declared on it. Scoring
// it would produce zeros that read like a measurement — fail-closed instead.
func TestXW3aDeliveredBasisRefusesADumpWithoutADeliveredRanking(t *testing.T) {
	c := newXW3aCampaign(t, xw3aOpts{noDelivered: true})
	in := c.input()
	in.ConditionField = armsweep.ConditionFieldPostFusionStages

	_, err := armsweep.Compare(in)
	if !errors.Is(err, armsweep.ErrStampIncongruent) {
		t.Fatalf("Compare = %v, erwartet ErrStampIncongruent", err)
	}
	if !strings.Contains(err.Error(), armsweep.RankingBasisDelivered) {
		t.Errorf("die Abweisung nennt die Basis nicht: %v", err)
	}
}

// TestXW3aOldDumpsStayReadable is the alt-dump probe: a dump written before
// this wave carries no instance_kind and no delivered block. It must still LOAD
// and stream — the campaign gate refuses it, the reader does not choke on it.
func TestXW3aOldDumpsStayReadable(t *testing.T) {
	c := newXW3aCampaign(t, xw3aOpts{})
	in := c.input()
	in.Cond.Stamp.PostFusionStages = in.Base.Stamp.PostFusionStages

	// Every stamp as an old one: no instance kind anywhere.
	in.Base.Stamp.InstanceKind = ""
	in.Cond.Stamp.InstanceKind = ""
	in.NoisePair[0].Stamp.InstanceKind = ""
	in.NoisePair[1].Stamp.InstanceKind = ""
	body, err := armsweep.Compare(in)
	if err != nil {
		t.Fatalf("Compare über Alt-Dumps = %v, erwartet nil (lesbar bleiben sie)", err)
	}
	if body.Paired != labelledN {
		t.Errorf("gepaart: %d, erwartet %d", body.Paired, labelledN)
	}

	// One old dump against three new ones is a MIXED campaign and refused —
	// fail-closed, and the refusal says which side was never stamped.
	in.Cond.Stamp.InstanceKind = armsweep.InstanceKindMeasureCopy
	_, err = armsweep.Compare(in)
	if !errors.Is(err, armsweep.ErrStampIncongruent) {
		t.Fatalf("Compare = %v, erwartet ErrStampIncongruent für den Alt/Neu-Mix", err)
	}
	if !strings.Contains(err.Error(), "nicht gestempelt") {
		t.Errorf("die Abweisung erklärt den leeren Stempel nicht: %v", err)
	}
}
