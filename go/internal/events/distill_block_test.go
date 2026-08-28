// Gate A02-9 (design/02 §7.2), the half that needs no database: the block
// format in the 1500-rune prompt window, the metadata white list, the
// per-insight credential scan and the untrusted framing.
//
// The database half — the written row, the squatted title, the write order and
// the upsert identity — is in distill_block_integration_test.go.
//
//	go test ./internal/events/ -run TestDistillBlock -count=1 -v
package events

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/redact"
	"github.com/GottZ/ctx/internal/sensitivity"
	"github.com/GottZ/ctx/internal/util"
)

const (
	a9Root   = "20260712_205012_837f2c"
	a9Part1  = "019f5b5f-e51c-7a94-a374-91c104491d01"
	a9Part2  = "019f5b5f-e51c-7a94-a374-91c104491d02"
	a9RunID  = "019f5b5f-e51c-7a94-a374-91c104491dff"
	a9Man    = "019f5b5f-e51c-7a94-a374-91c104491daa"
	a9WMFrom = int64(1787892725313110)
	a9WMTo   = int64(1787892999999999)
)

// a9Manifest is a live-shaped manifest provenance.
func a9Manifest() distillsource.Manifest {
	return distillsource.Manifest{
		ID:              a9Man,
		SHA256:          "6f1c2d3e4a5b60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9",
		ParentID:        "019f5b5f-e51c-7a94-a374-91c104491dab",
		ActiveSessionID: "20260712_205012_aaaaaa",
	}
}

func a9Opts() distillWriteOpts {
	return distillWriteOpts{
		category:    "session-insights",
		scope:       "private",
		typeName:    "insight",
		sensitivity: backends.SensCredentials,
		maxRunes:    6000,
		sourceLabel: "ctx-checkpoint",
	}
}

// a9State builds an accumulator with n plain insights.
func a9State(n int) *distillBlockState {
	st := newDistillBlockState(a9Root, a9RunID, a9WMFrom)
	// A FIXED stamp: the block text carries the run's creation date, and a
	// wall clock in a fixture makes an assertion about the format depend on
	// when it ran.
	st.createdAt = time.Unix(1787893000, 0).UTC()
	st.wmTo = a9WMTo
	st.newest = a9Manifest()
	st.manifests = []string{a9Man}
	st.parts[a9Part1] = struct{}{}
	st.parts[a9Part2] = struct{}{}
	st.seen, st.selected, st.droppedCred, st.droppedDup = 12, 8, 1, 3
	st.model = "qwen38-27b"
	for i := 1; i <= n; i++ {
		st.insights = append(st.insights, distillKept{
			claim:   fmt.Sprintf("Kernaussage %d ueber den Retrieval-Pfad und seine vier Arme.", i),
			quote:   strings.Repeat("Zitat-Text aus dem Roh-Transkript, wortgetreu uebernommen. ", 4),
			blockID: a9Part1, chunk: i,
		})
	}
	return st
}

// GATE 6 — the prompt window. The red state is recorded in the wave report
// (predecessor format: 2 of 6 claims outside the window, no UNTRUSTED, no
// anchor); this is the green half plus the property that carries it.
func TestDistillBlockPromptWindow(t *testing.T) {
	st := a9State(6)
	content, over := distillRenderBlock(st, a9Opts())
	if over != 0 {
		t.Fatalf("%d insights fell out of max_block_runes at n=6 — the probe would measure the cap", over)
	}
	cut := util.TruncateRunesWithSuffix(content, redact.Truncated, llm.MaxBlockChars)

	if !strings.Contains(cut, "UNTRUSTED") {
		t.Error("the trust framing is outside the 1500-rune window")
	}
	if i := strings.Index(cut, "UNTRUSTED"); i > 200 {
		t.Errorf("UNTRUSTED starts at rune offset ~%d, want inside the first ~200 (§4.4.4 property 1)", i)
	}
	for i := 1; i <= 6; i++ {
		claim := fmt.Sprintf("Kernaussage %d ueber den Retrieval-Pfad und seine vier Arme.", i)
		if !strings.Contains(cut, claim) {
			t.Errorf("claim %d is outside the 1500-rune window — assertions must survive, evidence may fall", i)
		}
		anchor := "[" + distillShort8(a9Part1) + "#" + fmt.Sprint(i) + "]"
		if !strings.Contains(cut, anchor) {
			t.Errorf("claim %d carries no inline anchor %s inside the window", i, anchor)
		}
	}
	// The direction of the loss is the point: the cut takes evidence.
	if strings.Contains(cut, "## Belege") {
		t.Error("the evidence section survived the cut — the section order does not do its job")
	}
	if !strings.Contains(content, "## Belege") {
		t.Error("the full block carries no evidence section at all")
	}
}

// The cap falls on insight boundaries and drops from the END, so the block's
// prefix is stable across the batches of a run.
func TestDistillBlockRuneCap(t *testing.T) {
	st := a9State(40)
	opts := a9Opts()
	content, over := distillRenderBlock(st, opts)
	if over == 0 {
		t.Fatal("40 insights fit into 6000 runes — the probe measures nothing")
	}
	if n := utf8.RuneCountInString(content); n > opts.maxRunes {
		t.Errorf("content is %d runes against a cap of %d", n, opts.maxRunes)
	}
	kept := 40 - over
	if !strings.Contains(content, fmt.Sprintf("Kernaussage %d ", kept)) {
		t.Errorf("the last kept insight (%d) is not in the block", kept)
	}
	if strings.Contains(content, fmt.Sprintf("Kernaussage %d ", kept+1)) {
		t.Errorf("insight %d is in the block although it was counted as dropped", kept+1)
	}
	if !strings.Contains(content, "überschreiten distill.max_block_runes") {
		t.Error("the block does not say that insights were left out — a silent loss")
	}
	// No half evidence line: every anchor in the evidence section has a claim.
	for i := 1; i <= kept; i++ {
		if !strings.Contains(content, "Abschnitt "+fmt.Sprint(i)+".") {
			t.Errorf("evidence line for insight %d is missing", i)
		}
	}
}

// GATE 5 — the metadata is exactly the pinned key set, in both cases.
func TestDistillBlockMetadataIsWhite(t *testing.T) {
	base := []string{
		"active_session_id", "coverage", "evidence_date", "gen", "insight_count",
		"invalidated_by", "manifest_id", "manifest_sha256", "model",
		"parent_manifest_id", "root_session_id", "run_id", "source_block_ids",
		"source_kind", "source_label", "warnings", "watermark_from", "watermark_to",
	}

	t.Run("without a detector hit the key set is exactly the base", func(t *testing.T) {
		md := distillBlockMetadata(a9State(2), a9Opts(), 2)
		if got := slices.Sorted(mapKeys(md)); !slices.Equal(got, base) {
			t.Errorf("keys = %v\nwant %v", got, base)
		}
		if _, ok := md["guard_checked_at"]; ok {
			t.Error("guard_checked_at is in the metadata — a guard off-switch in a JSON field")
		}
	})

	t.Run("with a hit it is the base plus exactly one key", func(t *testing.T) {
		st := a9State(2)
		m := sensitivity.Match{Kind: "aws-key", Reason: "AWS access key id pattern"}
		st.detector = &m
		md := distillBlockMetadata(st, a9Opts(), 2)
		want := append(slices.Clone(base), "sensitivity_detector")
		slices.Sort(want)
		if got := slices.Sorted(mapKeys(md)); !slices.Equal(got, want) {
			t.Errorf("keys = %v\nwant %v", got, want)
		}
		det, _ := md["sensitivity_detector"].(map[string]any)
		if det["kind"] != "aws-key" || det["reason"] != "AWS access key id pattern" {
			t.Errorf("verdict = %v, want the tree's {kind,reason} shape", det)
		}
	})

	// The VALUES are the second half, and the one the design calls the real
	// channel: everything that originates in plugin metadata is re-typed.
	t.Run("foreign values are re-typed, never forwarded", func(t *testing.T) {
		st := a9State(1)
		st.newest = distillsource.Manifest{
			ID:              "nicht-uuid",
			SHA256:          "abc <img src=x> def",
			ParentID:        "'; DROP TABLE context_blocks; --",
			ActiveSessionID: strings.Repeat("a", 200),
		}
		st.parts["kein-uuid"] = struct{}{}
		md := distillBlockMetadata(st, a9Opts(), 1)
		for _, k := range []string{"manifest_id", "manifest_sha256", "parent_manifest_id", "active_session_id"} {
			if md[k] != "" {
				t.Errorf("%s = %q, want \"\" — an unusable value is reported as absent, never partially cleaned", k, md[k])
			}
		}
		ids, _ := md["source_block_ids"].([]string)
		if !slices.Equal(ids, []string{a9Part1, a9Part2}) {
			t.Errorf("source_block_ids = %v, want only the two parsable uuids", ids)
		}
	})

	t.Run("the good case keeps the values", func(t *testing.T) {
		md := distillBlockMetadata(a9State(3), a9Opts(), 3)
		man := a9Manifest()
		if md["manifest_id"] != man.ID || md["manifest_sha256"] != man.SHA256 ||
			md["parent_manifest_id"] != man.ParentID || md["active_session_id"] != man.ActiveSessionID {
			t.Errorf("a live-shaped manifest lost values: %v", md)
		}
		if md["root_session_id"] != a9Root || md["source_kind"] != distillSourceKind {
			t.Errorf("root/source_kind = %v/%v", md["root_session_id"], md["source_kind"])
		}
		if md["watermark_from"] != a9WMFrom || md["watermark_to"] != a9WMTo {
			t.Errorf("watermarks = %v/%v", md["watermark_from"], md["watermark_to"])
		}
		if md["gen"] != 1 || md["insight_count"] != 3 {
			t.Errorf("gen/insight_count = %v/%v, want 1/3", md["gen"], md["insight_count"])
		}
		cov, _ := md["coverage"].(map[string]any)
		if cov["parts"] != 2 || cov["chunks_seen"] != 12 || cov["chunks_selected"] != 8 ||
			cov["chunks_dropped_cred"] != 1 || cov["chunks_dropped_dup"] != 3 {
			t.Errorf("coverage = %v", cov)
		}
		if md["evidence_date"] != distillMicroRFC3339(a9WMTo) {
			t.Errorf("evidence_date = %v", md["evidence_date"])
		}
	})
}

func mapKeys(m map[string]any) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// GATE 4 — the detector, in the two cases that differ. Injected PAST G1-G7 by
// calling the accumulator directly, which is what "am Gate vorbei" means.
func TestDistillBlockDetector(t *testing.T) {
	t.Run("an AKIA insight is dropped and the block stays derived", func(t *testing.T) {
		st := newDistillBlockState(a9Root, a9RunID, a9WMFrom)
		st.addBatch(distillExtractResult{insights: []distillKept{
			{claim: "Der Deploy nutzt AKIAIOSFODNN7EXAMPLE als Zugang.",
				quote:   strings.Repeat("harmloser Zitattext aus dem Transkript. ", 2),
				blockID: a9Part1, chunk: 1},
			{claim: "Die Migration 147 hat einen deterministischen Tiebreak eingebaut.",
				quote:   strings.Repeat("harmloser Zitattext aus dem Transkript. ", 2),
				blockID: a9Part1, chunk: 2},
		}}, distillLedger{}, a9WMTo, nil)

		if len(st.insights) != 1 || st.redacted != 1 {
			t.Fatalf("kept=%d redacted=%d, want 1/1", len(st.insights), st.redacted)
		}
		if st.detector == nil || st.detector.Kind != "aws-key" {
			t.Fatalf("detector = %v, want kind aws-key", st.detector)
		}
		if strings.Contains(st.detector.Reason, "AKIAIOSFODNN7EXAMPLE") {
			t.Error("the reason carries the matched secret")
		}
		content, _ := distillRenderBlock(st, a9Opts())
		if strings.Contains(content, "AKIAIOSFODNN7EXAMPLE") {
			t.Error("the secret reached the block content")
		}
		sens := distillBlockSensitivity(st, backends.SensCredentials)
		if sens.Value != backends.SensCredentials || !sens.Derived || sens.Detector || sens.Manual {
			t.Errorf("write intent = %+v, want credentials/derived with Detector false (Festlegung 5)", sens)
		}
	})

	// THE RAISE IS VISIBLE ONLY BELOW credentials, so the probe lowers the base
	// to prove the mechanism rather than the default.
	t.Run("a hit raises a lower configured level", func(t *testing.T) {
		st := newDistillBlockState(a9Root, a9RunID, a9WMFrom)
		st.addBatch(distillExtractResult{insights: []distillKept{
			{claim: "Der Deploy nutzt AKIAIOSFODNN7EXAMPLE als Zugang.",
				quote: "irrelevant", blockID: a9Part1, chunk: 1},
		}}, distillLedger{}, a9WMTo, nil)
		sens := distillBlockSensitivity(st, backends.SensInternal)
		if sens.Value != backends.SensCredentials || sens.Detector {
			t.Errorf("write intent = %+v, want credentials with Detector false", sens)
		}
	})

	t.Run("no hit leaves the configured level and writes no verdict", func(t *testing.T) {
		st := a9State(3)
		sens := distillBlockSensitivity(st, backends.SensInternal)
		if sens.Value != backends.SensInternal || !sens.Derived {
			t.Errorf("write intent = %+v, want internal/derived", sens)
		}
		if st.detector != nil {
			t.Errorf("a verdict was recorded without a hit: %v", st.detector)
		}
	})

	// FESTLEGUNG 1 — the value is never the zero value, whatever the config says.
	t.Run("an empty configured level fails closed to credentials", func(t *testing.T) {
		st := a9State(1)
		if sens := distillBlockSensitivity(st, ""); sens.Value != backends.SensCredentials {
			t.Errorf("value = %q, want credentials — an empty value writes no sensitivity column at all", sens.Value)
		}
	})

	// G5's OWN LESSON, at the block level: the per-insight scan is what §4.4.2
	// asks for, and the concatenation it must never run over is the one the
	// CONTENT is. Measured here in both directions rather than argued.
	t.Run("the per-insight scan sees what a concatenation would whitelist", func(t *testing.T) {
		hex := strings.Repeat("ab12cd34", 8) // 64 hex characters
		joined := `sha256: "` + hex
		if _, hit := sensitivity.Scan(joined); hit {
			t.Fatal("the joined form fires — the probe would not show the whitelisting")
		}
		if _, hit := sensitivity.Scan(hex); !hit {
			t.Fatal("the quote alone does not fire — the probe measures nothing")
		}
		st := newDistillBlockState(a9Root, a9RunID, a9WMFrom)
		st.addBatch(distillExtractResult{insights: []distillKept{
			{claim: `Der Anhang traegt sha256: "`, quote: hex,
				blockID: a9Part1, chunk: 1},
		}}, distillLedger{}, a9WMTo, nil)
		if st.redacted != 1 || len(st.insights) != 0 {
			t.Errorf("redacted=%d kept=%d, want 1/0 — the separate scan of the quote is what catches it",
				st.redacted, len(st.insights))
		}
	})

	// §4.4.4 property 3 — the leak ACROSS an insight boundary. The design asks
	// for a separator wider than sensitivity.hashLabelWindow (32 bytes); what
	// actually carries here is that the anchor between two claims BREAKS
	// reHashLabel's $-anchored shape, so the content scan is not fooled either.
	// Measured, because the property is what matters and not the byte count.
	t.Run("an anchor between two claims does not whitelist the next hex run", func(t *testing.T) {
		hex := strings.Repeat("ab12cd34", 8)
		st := newDistillBlockState(a9Root, a9RunID, a9WMFrom)
		st.newest = a9Manifest()
		// Injected past the accumulator's own scan, so only the CONTENT scan is
		// under probe here.
		st.insights = []distillKept{
			{claim: `Der Anhang traegt sha256: "`, quote: "q", blockID: a9Part1, chunk: 1},
			{claim: hex + " steht in der naechsten Aussage.", quote: "q", blockID: a9Part2, chunk: 1},
		}
		content, _ := distillRenderBlock(st, a9Opts())
		if _, hit := sensitivity.Scan(content); !hit {
			t.Error("the content scan was whitelisted across the insight boundary — §4.4.4 property 3 is not carried")
		}
	})
}

// GATE 12 — R2-1 and R2-2.
func TestDistillBlockProvenanceSemantics(t *testing.T) {
	// R2-2 — and it CALLS the production resolution instead of restating it.
	// Round 1 re-implemented the rule in the test body; the reviewer disarmed the
	// production path and this sub-test stayed green (round-2 minor #6), so it
	// measured nothing at all.
	t.Run("R2-2 an unanchored insight is counted, not written", func(t *testing.T) {
		shown := distillShown{
			text: map[distillChunkKey]string{
				{block: "1", chunk: 1}: "egal", {block: "2", chunk: 1}: "egal",
			},
			uuid: map[string]string{"1": "", "2": a9Part1},
		}
		out, unanchored := distillResolveKept([]distillInsight{
			{Claim: "ohne Anker", Quote: "q", Block: "1", Chunk: 1, Kind: "finding"},
			{Claim: "mit Anker", Quote: "q", Block: "2", Chunk: 1, Kind: "finding"},
		}, shown)
		if unanchored != 1 || len(out) != 1 {
			t.Fatalf("unanchored=%d resolved=%d, want 1/1", unanchored, len(out))
		}
		if out[0].claim != "mit Anker" || out[0].blockID != a9Part1 {
			t.Errorf("the survivor resolved to %+v", out[0])
		}
	})

	// R2-1: one sentence can live in two parts, so an anchor names A containing
	// part and the block says so. The full list is source_block_ids.
	t.Run("R2-1 the block states what an anchor means", func(t *testing.T) {
		st := a9State(2)
		content, _ := distillRenderBlock(st, a9Opts())
		if !strings.Contains(content, "benennt EINEN Part") ||
			!strings.Contains(content, "nicht zwingend den einzigen") {
			t.Error("the block does not state the anchor's semantics (R2-1)")
		}
		md := distillBlockMetadata(st, a9Opts(), 2)
		ids, _ := md["source_block_ids"].([]string)
		if !slices.Equal(ids, []string{a9Part1, a9Part2}) {
			t.Errorf("source_block_ids = %v, want both parts of the run", ids)
		}
	})
}

// The freshness stamp is taken ONCE PER RUN, and this measures it directly
// (round-2 minor #7). Round 1 inferred the property from `embed_model` surviving
// a repeated write — which stays green under a per-render wall clock whenever
// both writes fall into the same RFC3339 second, i.e. almost always. Rendering
// twice and comparing bytes has no such window, and the fixed stamp makes the
// mutation deterministic rather than flaky.
func TestDistillBlockFreshnessStamp(t *testing.T) {
	st := a9State(3)
	stamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.createdAt = stamp

	first, _ := distillRenderBlock(st, a9Opts())
	second, _ := distillRenderBlock(st, a9Opts())
	if first != second {
		t.Error("two renders of the same state differ — the content feeds a GENERATED hash column, " +
			"so a moving value makes every batch's upsert a content CHANGE")
	}
	if !strings.Contains(first, "Erzeugt am "+stamp.Format(time.RFC3339)) {
		t.Errorf("the block does not carry the RUN's stamp %q", stamp.Format(time.RFC3339))
	}
	// A new run is a new stamp: the property is "once per run", not "constant".
	other := newDistillBlockState(a9Root, a9RunID, a9WMFrom)
	if other.createdAt.IsZero() {
		t.Error("a fresh accumulator carries no stamp at all")
	}
}

// THE CARRY (round-2 blocker #1): what an earlier run made durable survives the
// next run over the same identity, and the two sections round-trip.
func TestDistillBlockCarry(t *testing.T) {
	t.Run("a rendered block splits back into its two per-insight sections", func(t *testing.T) {
		st := a9State(3)
		content, _ := distillRenderBlock(st, a9Opts())
		carry, ok := distillSplitCarry(content)
		if !ok {
			t.Fatal("the arm cannot read back its own block")
		}
		if carry.count() != 3 || len(carry.evidence) != 3 {
			t.Fatalf("carry = %d claims / %d evidence lines, want 3/3", carry.count(), len(carry.evidence))
		}
		for i := 1; i <= 3; i++ {
			claim := fmt.Sprintf("Kernaussage %d ueber den Retrieval-Pfad und seine vier Arme.", i)
			if !strings.Contains(carry.claims[i-1], claim) {
				t.Errorf("carried claim %d is %q", i, carry.claims[i-1])
			}
		}
	})

	t.Run("a carried block plus a new run renders both, in order", func(t *testing.T) {
		old := a9State(2)
		oldContent, _ := distillRenderBlock(old, a9Opts())
		carry, ok := distillSplitCarry(oldContent)
		if !ok {
			t.Fatal("split failed")
		}

		next := a9State(0)
		next.carry = carry
		next.insights = []distillKept{{
			claim: "Neue Aussage aus dem Folgelauf.", quote: "irgendein Zitat aus dem Transkript",
			blockID: a9Part2, chunk: 9,
		}}
		content, over := distillRenderBlock(next, a9Opts())
		if over != 0 {
			t.Fatalf("%d dropped although the block is nearly empty", over)
		}
		for i := 1; i <= 2; i++ {
			claim := fmt.Sprintf("Kernaussage %d ueber den Retrieval-Pfad und seine vier Arme.", i)
			if !strings.Contains(content, claim) {
				t.Errorf("the carried claim %d was lost", i)
			}
		}
		if !strings.Contains(content, "Neue Aussage aus dem Folgelauf.") {
			t.Error("the new claim is missing")
		}
		// Order: everything carried stands before everything new.
		if strings.Index(content, "Neue Aussage") < strings.Index(content, "Kernaussage 2") {
			t.Error("the new claim was rendered before a carried one — the prefix must stay stable")
		}
		// insight_count counts the BLOCK, not the run.
		md := distillBlockMetadata(next, a9Opts(), next.carry.count()+len(next.insights)-over)
		if md["insight_count"] != 3 {
			t.Errorf("insight_count = %v, want 3 (2 carried + 1 new)", md["insight_count"])
		}
	})

	t.Run("an unrecognised body is refused, never replaced", func(t *testing.T) {
		for _, body := range []string{
			"", "irgendein fremder Text ohne Abschnitte",
			"# Titel\n\n## Erkenntnisse\n\n- **a** [x#1]\n", // no provenance section
		} {
			if _, ok := distillSplitCarry(body); ok {
				t.Errorf("a body without this arm's sections was accepted as carry: %q", body)
			}
		}
	})

	// FULLNESS IS MEASURED AT THE SHORTEST ADMISSIBLE INSIGHT, so the edge is
	// exact rather than approximate: a cap that holds exactly what is already
	// there is full, one that holds one minimal insight more is not.
	t.Run("a full block reports itself full at the exact edge", func(t *testing.T) {
		big := a9State(6)
		content, over := distillRenderBlock(big, a9Opts())
		if over != 0 {
			t.Fatalf("%d dropped — the carry would not be the whole block", over)
		}
		carry, ok := distillSplitCarry(content)
		if !ok {
			t.Fatal("split failed")
		}
		next := a9State(0)
		next.carry = carry

		tight := a9Opts()
		tight.maxRunes = utf8.RuneCountInString(distillRenderN(next, tight, nil, nil, 0))
		if !next.full(tight) {
			t.Error("a block whose cap holds exactly its carry does not report itself full — " +
				"the next run would pay for calls whose insights the cap drops")
		}
		loose := tight
		loose.maxRunes = tight.maxRunes + distillNextInsightRunes(next)
		if next.full(loose) {
			t.Error("a block with room for one more insight reports itself full")
		}
		// The threshold follows the BLOCK, not a constant: an insight of this
		// block's own size, never the theoretical minimum.
		if got := distillNextInsightRunes(next); got <= distillMinInsightRunes {
			t.Errorf("the room estimate is %d, i.e. the theoretical minimum (%d) — a real insight "+
				"of this corpus is several times that, and the run would pay for calls it drops",
				got, distillMinInsightRunes)
		}
		if distillNextInsightRunes(a9State(0)) != distillMinInsightRunes {
			t.Error("an empty block does not fall back to the minimum")
		}
		if a9State(0).full(a9Opts()) {
			t.Error("an empty block reports itself full")
		}

		// The carry is never dropped, even under a cap far below it: the corpus
		// does not shrink because a key was lowered.
		tiny := a9Opts()
		tiny.maxRunes = 100
		grown, over2 := distillRenderBlock(next, tiny)
		if over2 != 0 {
			t.Errorf("over=%d although the run added nothing", over2)
		}
		if got := strings.Count(grown, "- **"); got != carry.count() {
			t.Errorf("the carry lost lines under a tiny cap: %d of %d", got, carry.count())
		}
	})
}

// GATE 2's other half and §4.4.1: the title.
func TestDistillBlockTitle(t *testing.T) {
	title := distillBlockTitle(a9Root, a9WMFrom)
	if !strings.HasPrefix(title, distillTitlePrefix) {
		t.Errorf("title %q does not open with the namespace prefix", title)
	}
	if strings.Contains(strings.ToLower(title), "session") {
		t.Errorf("title %q carries the word the audit-trail classifier matches on", title)
	}
	if !strings.Contains(title, a9Root) {
		t.Errorf("title %q does not carry the full root id — a truncated identity collides at target scale", title)
	}
	// Two ranges of the same root are two identities; two runs over the same
	// range are one.
	if distillBlockTitle(a9Root, a9WMFrom) != title {
		t.Error("the title is not deterministic")
	}
	if distillBlockTitle(a9Root, a9WMFrom+1) == title {
		t.Error("two watermarks share one title — a second run would overwrite the first range")
	}
}

// GATE 7 — the untrusted framing (BA7), as a Go test. The red state is
// recorded in the wave report: a caller that does not consult the registry
// renders no trust attribute and splices no framing sentence.
//
// IT ASKS THE CONFIGURED TYPE, not the constant "insight" (round-2 minor #9).
// The validator admits every derived type for distill.block_type, and `catalog`
// — the other one — carries Untrusted:false (blocktype/builtin.go). A valid
// configuration would therefore take the BA7 framing away from transcript prose
// without anything going red. Driving the gate from config.Defaults() makes
// that a red test rather than a footnote.
func TestDistillBlockUntrustedFraming(t *testing.T) {
	set := blocktype.NewRegistry().Snapshot()
	blockType := config.Defaults().Distill.BlockType
	if blockType == "" {
		t.Fatal("distill.block_type has no registry default — the gate would pin nothing")
	}
	if !set.IsUntrusted(blockType) {
		t.Fatalf("Set.IsUntrusted(%q) is false — the arm writes transcript prose under a type "+
			"that carries no retrieval.untrusted, so BA7's framing never reaches the prompt", blockType)
	}
	if set.IsUntrusted("kein-solcher-typ") {
		t.Error("an unknown type name answers true — the framing would be spliced for a type nothing registered")
	}

	src := llm.Source{
		Title: distillBlockTitle(a9Root, a9WMFrom), Category: "session-insights",
		Content: "irgendein Destillat", Untrusted: set.IsUntrusted(blockType),
	}
	sys, user := llm.BuildPrompt("frage", []llm.Source{src}, nil, llm.SynthesisSettings{})
	if !strings.Contains(user, `trust="untrusted"`) {
		t.Error(`the rendered prompt carries no trust="untrusted"`)
	}
	if !strings.Contains(sys, llm.UntrustedSourceRule) {
		t.Error("the system prompt carries no untrusted framing sentence")
	}

	// The fail-open direction of the unknown name is a property of the registry
	// lookup (blocktype/set.go) and is NOT re-decided here — probed so a change
	// there becomes visible in this wave's suite too.
	plain := llm.Source{Title: "x", Category: "knowledge", Content: "y", Untrusted: set.IsUntrusted("knowledge")}
	_, plainUser := llm.BuildPrompt("frage", []llm.Source{plain}, nil, llm.SynthesisSettings{})
	if strings.Contains(plainUser, `trust="untrusted"`) {
		t.Error("a first-party type is framed as untrusted")
	}
}
