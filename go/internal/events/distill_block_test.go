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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
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
	// Two keys longer than before wave W-L1: amendment C4-2 A.3 (d) adds
	// shard_ordinal and shard_of_watermark to the pinned set. Both are
	// code-computed, so the §4.4.3 typing rule has nothing to check on them —
	// what this gate checks is that the set grew by exactly two and by nothing
	// else.
	base := []string{
		"active_session_id", "coverage", "evidence_date", "gen", "insight_count",
		"invalidated_by", "manifest_id", "manifest_sha256", "model",
		"parent_manifest_id", "root_session_id", "run_id", "shard_of_watermark",
		"shard_ordinal", "source_block_ids", "source_kind", "source_label",
		"warnings", "watermark_from", "watermark_to",
	}

	t.Run("the two shard keys carry the code-computed values", func(t *testing.T) {
		st := a9State(2)
		st.ordinal = 3
		md := distillBlockMetadata(st, a9Opts(), 2)
		if md[distillMetaShardOrdinal] != 3 {
			t.Errorf("shard_ordinal = %v, want 3", md[distillMetaShardOrdinal])
		}
		if md[distillMetaShardOfWM] != a9WMFrom {
			t.Errorf("shard_of_watermark = %v, want %d", md[distillMetaShardOfWM], a9WMFrom)
		}
		// The default is shard 1, so a block written without a seed is a shard-1
		// block — the state every stock block is in.
		if plain := distillBlockMetadata(a9State(2), a9Opts(), 2); plain[distillMetaShardOrdinal] != 1 {
			t.Errorf("shard_ordinal = %v on a fresh state, want 1", plain[distillMetaShardOrdinal])
		}
	})

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
	title := distillBlockTitle(a9Root, a9WMFrom, 1)
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
	if distillBlockTitle(a9Root, a9WMFrom, 1) != title {
		t.Error("the title is not deterministic")
	}
	if distillBlockTitle(a9Root, a9WMFrom+1, 1) == title {
		t.Error("two watermarks share one title — a second run would overwrite the first range")
	}
}

// Below: wave W-L1 — the identity's third axis (amendment C4-2 A.2).

// TestDistillShardTitle is the green half of the wave's first red point: on the
// tree before it, a call with three arguments did not compile.
//
// The RED of the sonderregel is recorded in the wave report and machine-checked
// in TestDistillShardTitleNonRegression: a version that suffixes n = 1 as well
// renames every existing block, which the measured stock (16 blocks, none of
// them carrying a suffix) turns into 16 orphans plus 16 new blocks.
func TestDistillShardTitle(t *testing.T) {
	base := distillBlockTitle(a9Root, a9WMFrom, 1)

	t.Run("shard 1 carries no suffix at all", func(t *testing.T) {
		if strings.Contains(base, distillShardSuffix) {
			t.Fatalf("title %q carries a shard suffix at n=1 — the whole stock would be renamed", base)
		}
		// The collision-freedom argument of A.2 (d), checked rather than asserted:
		// the base ends on the stamp, so no pre-wave title can read as a shard.
		if !strings.HasSuffix(base, distillMicroRFC3339(a9WMFrom)) {
			t.Errorf("title %q does not end on the RFC3339-µs stamp", base)
		}
	})

	t.Run("shard 2 and above carry the suffix in its canonical form", func(t *testing.T) {
		for n, want := range map[int]string{2: " — Teil 2", 3: " — Teil 3", 10: " — Teil 10", 1430: " — Teil 1430"} {
			got := distillBlockTitle(a9Root, a9WMFrom, n)
			if got != base+want {
				t.Errorf("n=%d: title = %q, want %q", n, got, base+want)
			}
		}
	})

	t.Run("no two ordinals share one title", func(t *testing.T) {
		seen := map[string]int{}
		for n := 1; n <= 64; n++ {
			title := distillBlockTitle(a9Root, a9WMFrom, n)
			if prev, dup := seen[title]; dup {
				t.Fatalf("n=%d and n=%d share the title %q", prev, n, title)
			}
			seen[title] = n
		}
	})

	// The ordinal is code-computed and opens at 1, so a value below it cannot
	// occur. Pinned anyway: a total function whose out-of-range behaviour is
	// undefined is the kind of identity trap A.2 (c) names by its own price.
	t.Run("an ordinal below 1 renders as shard 1", func(t *testing.T) {
		for _, n := range []int{0, -1, -1430} {
			if got := distillBlockTitle(a9Root, a9WMFrom, n); got != base {
				t.Errorf("n=%d: title = %q, want the shard-1 title %q", n, got, base)
			}
		}
	})

	// The other two axes keep working across ordinals: a shard of one range is
	// never a shard of another.
	t.Run("the ordinal does not collide across ranges or roots", func(t *testing.T) {
		if distillBlockTitle(a9Root, a9WMFrom, 2) == distillBlockTitle(a9Root, a9WMFrom+1, 2) {
			t.Error("two watermarks share one shard-2 title")
		}
		if distillBlockTitle(a9Root, a9WMFrom, 2) == distillBlockTitle(a9Root+"x", a9WMFrom, 2) {
			t.Error("two roots share one shard-2 title")
		}
	})
}

// wl1TitleFixtures are the (root, watermark) shapes the digest below pins: the
// unit fixture, the epoch watermark every seed run of the measure copy carries,
// a preflight root, and a root at the length the metadata check still admits.
var wl1TitleFixtures = []struct {
	root string
	wm   int64
}{
	{"20260712_205012_837f2c", 0},
	{"20260712_205012_837f2c", 1787892725313110},
	{"20260819_115828_ecbc1118", 1787892999999999},
	{"preflight-20260809T174528Z", 0},
	{"20260729_081118_9f9fb1", 1234567890123456},
	{strings.Repeat("a", 64), 999999999999999},
}

// TestDistillShardTitleNonRegression is the gate amendment C4-2 A.2 (c) asks
// for by name: `distillBlockTitle(root, wm, 1)` is BYTE-IDENTICAL to the
// two-parameter function this wave replaced.
//
// A digest over a fixture set and not a prose assertion, for the reason
// TestDistillInsightLineNonRegression gives for the render seam: the property
// under probe is "not one byte moved", and every weaker formulation would pass
// a title that quietly gained a space.
//
// THE PIN WAS TAKEN ON THE UNCHANGED TREE, before the ordinal existed, with the
// two-parameter function and the same fixture list (command and output in the
// wave report). It is therefore evidence and not a self-fulfilling recording.
//
// The negative side is in the same test, because it is the whole reason for the
// sonderregel: the counter-version — suffix at n = 1 as well — produces a
// DIFFERENT digest, and every one of its titles misses the stock row it was
// supposed to grow.
func TestDistillShardTitleNonRegression(t *testing.T) {
	var b strings.Builder
	for _, f := range wl1TitleFixtures {
		b.WriteString(distillBlockTitle(f.root, f.wm, 1))
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	const want = "a6dd8eab1c5339cebf13c954d2abba9901ca48f78ee0dcdb9918af7a8ceb6430"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("the shard-1 title moved against the pre-wave tree:\n got %s\nwant %s\n%s",
			got, want, b.String())
	}

	// The counter-version, spelled out so the red is checked rather than
	// remembered: with a suffix at n = 1 not one of the fixtures keeps its name.
	var c strings.Builder
	for _, f := range wl1TitleFixtures {
		c.WriteString(distillBlockTitle(f.root, f.wm, 1) + distillShardSuffix + "1")
		c.WriteString("\n")
	}
	if csum := sha256.Sum256([]byte(c.String())); hex.EncodeToString(csum[:]) == want {
		t.Fatal("the counter-version digests identically — the probe measures nothing")
	}
	for _, f := range wl1TitleFixtures {
		if distillBlockTitle(f.root, f.wm, 1)+distillShardSuffix+"1" == distillBlockTitle(f.root, f.wm, 1) {
			t.Errorf("counter-version title equals the stock title for %q", f.root)
		}
	}
}

// TestDistillShardOrdinalFromTitle pins the inverse: which shard a title IS.
//
// It is the function that replaces the amendment's `ORDER BY
// (metadata->>'shard_ordinal')::int`, and the W-L0 measurement is why: the 16
// stock blocks carry no such key, so the SQL expression sorts them against the
// shards (measured: ASC puts NULL last, DESC first — both open the stock block
// instead of the highest shard). A title-derived ordinal has no NULL in the
// decision at all.
func TestDistillShardOrdinalFromTitle(t *testing.T) {
	base := distillBlockTitle(a9Root, a9WMFrom, 1)

	t.Run("the stock title is shard 1", func(t *testing.T) {
		n, ok := distillShardOrdinal(a9Root, a9WMFrom, base)
		if !ok || n != 1 {
			t.Fatalf("(%d, %v), want (1, true) — the whole coexistence path rests on this", n, ok)
		}
	})

	t.Run("every written shard reads back as itself", func(t *testing.T) {
		for i := 1; i <= 64; i++ {
			title := distillBlockTitle(a9Root, a9WMFrom, i)
			n, ok := distillShardOrdinal(a9Root, a9WMFrom, title)
			if !ok || n != i {
				t.Errorf("%q -> (%d, %v), want (%d, true)", title, n, ok, i)
			}
		}
	})

	// A title this arm can never have written is NOT a shard of this chain. Each
	// of these would otherwise let a second title claim an ordinal that already
	// belongs to one.
	t.Run("non-canonical and foreign forms are refused", func(t *testing.T) {
		for _, title := range []string{
			base + distillShardSuffix + "1",   // the counter-version's shard 1
			base + distillShardSuffix + "01",  // leading zero
			base + distillShardSuffix + "+2",  // signed
			base + distillShardSuffix + "-2",  // negative
			base + distillShardSuffix + "2 ",  // trailing space
			base + distillShardSuffix + " 2",  // leading space
			base + distillShardSuffix + "2.0", // not an integer
			base + distillShardSuffix,         // no number at all
			base + " — Teil2",                 // suffix without the space
			base + " - Teil 2",                // hyphen instead of the em dash
			base + "x",
			distillBlockTitle(a9Root, a9WMFrom+1, 2), // another range
			distillBlockTitle(a9Root+"x", a9WMFrom, 2),
			"Destillat aus Compaction",
			"",
		} {
			if n, ok := distillShardOrdinal(a9Root, a9WMFrom, title); ok {
				t.Errorf("%q was read as shard %d", title, n)
			}
		}
	})

	// Round trip against the identity function, over both axes.
	t.Run("the inverse holds for every fixture", func(t *testing.T) {
		for _, f := range wl1TitleFixtures {
			for _, i := range []int{1, 2, 9, 1430} {
				n, ok := distillShardOrdinal(f.root, f.wm, distillBlockTitle(f.root, f.wm, i))
				if !ok || n != i {
					t.Errorf("%q/%d: (%d, %v)", f.root, i, n, ok)
				}
			}
		}
	})
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
		Title: distillBlockTitle(a9Root, a9WMFrom, 1), Category: "session-insights",
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

// Below: wave C3-1, part A — N-1, the line break inside a quote (pilot report
// §10 N-1, board decision E4-2 "render-escape").

// c31MultilineQuote is the shape the pilot measured: transcript prose whose
// quote carries a hard line break. distillDecode admits it deliberately
// (distill_extract.go:611-619, "a quote out of transcript prose legitimately
// carries them"), so the render is the only place that can still be wrong.
const c31MultilineQuote = "Die Migration 147 hat einen Tiebreak eingebaut.\n" +
	"Der Retrieval-Pfad faltet vier Arme per Reciprocal Rank Fusion zusammen."

// c31BulletQuote is the KILLER PATH of N-1: the continuation line opens with
// "- ", which is exactly distillBulletLines' prefix, so the evidence section
// reads back one line more than the claims one and distillSplitCarry refuses
// the whole body. Measured in the pilot: one of sixteen blocks ended
// permanently failed/block_write_failed.
const c31BulletQuote = "Der Entscheid steht in zwei Zeilen:\n" +
	"- der Deckel bleibt, die Steuerung kommt davor."

// c31MultilineClaim and c31BulletClaim are the SAME two shapes on the claim
// side (round 2, review major #1). distillDecode admits tab, LF and CR in the
// claim exactly as it does in the quote, the claim is rendered into the same
// bullet list, and distillBulletLines cannot tell which of the two lines
// produced a stray "- " — so the block-killing path exists twice and a fixture
// that only ever breaks the quote leaves half of it unmeasured. Round 1 shipped
// the claim normalisation without a red state for it; the reviewer's mutation
// "claim not normalised" stayed silent across the whole suite.
const c31MultilineClaim = "Die Cap-Steuerung setzt vor der Call-Auswahl an.\n" +
	"Der Deckel selbst bleibt unveraendert."

const c31BulletClaim = "Der Entscheid nennt zwei Haelften:\n" +
	"- die Steuerung kommt vor den Call."

// c31TabCRQuote carries the two control runes the LF fixtures never exercise:
// a tab between two columns and a CRLF pair as the line break (round 2, review
// minor #5a). Without it the regex may shrink to `[\n]+` unnoticed.
const c31TabCRQuote = "Spalte A\tSpalte B ist die zweite Haelfte der Zeile.\r\n" +
	"Und hier steht die Fortsetzung des Zitats."

// TestDistillInsightLineIsOneLine is part A's gate 1.
//
// RED against the unchanged tree: distillInsightLine splices claim and quote
// into single-line bullets, so the first \n inside either of them ends its line
// and the block uuid tail — the part that makes the citation checkable at all —
// moves onto a line of its own.
//
// TABLE-DRIVEN OVER BOTH FIELDS (round 2, review major #1 and minor #5): every
// shape is probed once with the break in the QUOTE and once with it in the
// CLAIM, so removing the normalisation from either half turns a case red.
func TestDistillInsightLineIsOneLine(t *testing.T) {
	const plainClaim = "Der Retrieval-Pfad faltet vier Arme per Reciprocal Rank Fusion zusammen."
	const plainQuote = "Ein Zitat aus dem Roh-Transkript, wortgetreu uebernommen und lang genug."

	for _, tc := range []struct {
		name         string
		claim, quote string
		// words must survive in the rendered line the break was planted in.
		words []string
	}{
		{"LF in the quote", plainClaim, c31MultilineQuote, []string{"Migration 147", "Reciprocal Rank Fusion"}},
		{"LF in the claim", c31MultilineClaim, plainQuote, []string{"Cap-Steuerung", "Deckel selbst bleibt"}},
		{"bullet continuation in the quote", plainClaim, c31BulletQuote, []string{"zwei Zeilen", "die Steuerung kommt davor"}},
		{"bullet continuation in the claim", c31BulletClaim, plainQuote, []string{"zwei Haelften", "vor den Call"}},
		{"tab and CRLF in the quote", plainClaim, c31TabCRQuote, []string{"Spalte A", "Spalte B", "Fortsetzung des Zitats"}},
		{"tab and CRLF in the claim", strings.ReplaceAll(c31TabCRQuote, "Zitats", "Claims"), plainQuote,
			[]string{"Spalte A", "Spalte B", "Fortsetzung des Claims"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim, evidence := distillInsightLine(distillKept{
				claim: tc.claim, quote: tc.quote, blockID: a9Part1, chunk: 7,
			})
			for _, ln := range []struct {
				name, line string
			}{{"claim", claim}, {"evidence", evidence}} {
				if n := strings.Count(ln.line, "\n"); n != 1 {
					t.Errorf("%s line carries %d newlines, want exactly the closing one:\n%q",
						ln.name, n, ln.line)
				}
				if strings.ContainsAny(strings.TrimSuffix(ln.line, "\n"), "\n\r\t") {
					t.Errorf("%s line still carries a control whitespace rune:\n%q", ln.name, ln.line)
				}
			}
			// The uuid tail is what makes the evidence line checkable — it must
			// stand in the SAME line as the anchor it belongs to.
			head := strings.SplitN(evidence, "\n", 2)[0]
			if !strings.Contains(head, a9Part1) {
				t.Errorf("the block uuid fell out of the evidence line:\n%q", head)
			}
			if !strings.Contains(head, "Abschnitt 7.") {
				t.Errorf("the section number fell out of the evidence line:\n%q", head)
			}
			// The words around the break survive in the line that carried it —
			// normalisation replaces the break, it does not truncate.
			carrier := head
			if tc.quote == plainQuote {
				carrier = strings.SplitN(claim, "\n", 2)[0]
			}
			for _, w := range tc.words {
				if !strings.Contains(carrier, w) {
					t.Errorf("the line lost %q — the text was cut, not normalised:\n%q", w, carrier)
				}
			}
		})
	}
}

// TestDistillOneLineSeparatesWithASpace is part A's gate 1b (round 2, review
// minor #5b).
//
// E4-2 says the break becomes a SPACE. Replacing it with the empty string would
// glue the two words around it together ("eingebaut.Der"), which destroys
// exactly what the evidence line is for: checking the quote against the raw
// transcript word for word. The probe pins the separator itself, and it pins
// that a MULTI-RUNE break (CRLF, LF+tab) still becomes exactly one space.
func TestDistillOneLineSeparatesWithASpace(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"LF", "eingebaut.\nDer Pfad", "eingebaut. Der Pfad"},
		{"CRLF is one separator", "eingebaut.\r\nDer Pfad", "eingebaut. Der Pfad"},
		{"CR alone", "eingebaut.\rDer Pfad", "eingebaut. Der Pfad"},
		{"tab", "Spalte A\tSpalte B", "Spalte A Spalte B"},
		{"LF plus tab is one separator", "eingebaut.\n\tDer Pfad", "eingebaut. Der Pfad"},
		{"a run of newlines is one separator", "Absatz eins.\n\n\nAbsatz zwei.", "Absatz eins. Absatz zwei."},
		{"ordinary double spaces are untouched", "zwei  Leerzeichen", "zwei  Leerzeichen"},
		{"nothing to do", "eine ganz gewoehnliche Zeile", "eine ganz gewoehnliche Zeile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := distillOneLine(tc.in); got != tc.want {
				t.Errorf("distillOneLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// And the same property THROUGH the render, so a caller that stops using the
	// helper is caught too.
	_, evidence := distillInsightLine(distillKept{
		claim: "egal", quote: c31MultilineQuote, blockID: a9Part1, chunk: 1,
	})
	if !strings.Contains(evidence, "eingebaut. Der Retrieval-Pfad") {
		t.Errorf("the rendered evidence line does not separate the two halves with a single space:\n%q",
			evidence)
	}
}

// TestDistillCarrySurvivesABulletQuote is part A's gate 2 — the killer path.
//
// RED against the unchanged tree: the rendered block's evidence section holds
// one line more than its claims section, distillSplitCarry answers ok=false,
// and distillSeedBlock turns that into errDistillBlockWrite — the identity ends
// permanently failed/block_write_failed and no later run can write it again.
//
// BOTH HALVES (round 2, review major #1): the continuation line is planted once
// in the quote and once in the claim. The claim case lands in the CLAIMS
// section, so it desynchronises the two sections in the opposite direction —
// two claim lines against one evidence line — and hits the same refusal.
func TestDistillCarrySurvivesABulletQuote(t *testing.T) {
	for _, tc := range []struct {
		name, claim, quote string
	}{
		{
			"continuation in the quote",
			"Der Deckel bleibt bestehen und die Steuerung setzt davor an.",
			c31BulletQuote,
		},
		{
			"continuation in the claim",
			c31BulletClaim,
			"Ein Zitat aus dem Roh-Transkript, wortgetreu uebernommen und lang genug.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := a9State(0)
			st.insights = []distillKept{{
				claim: tc.claim, quote: tc.quote, blockID: a9Part1, chunk: 4,
			}}
			content, over := distillRenderBlock(st, a9Opts())
			if over != 0 {
				t.Fatalf("%d insights dropped although the block holds one — the probe would measure the cap", over)
			}

			carry, ok := distillSplitCarry(content)
			if !ok {
				t.Fatalf("the arm cannot read back its own block — a follow-up run over this identity "+
					"answers block_write_failed forever:\n%s", content)
			}
			if carry.count() != 1 || len(carry.evidence) != 1 {
				t.Fatalf("carry = %d claims / %d evidence lines, want 1/1 — the continuation line was read "+
					"as a second insight", carry.count(), len(carry.evidence))
			}
			// And the round trip is exact: the carried lines are the rendered ones.
			c, e := distillInsightLine(st.insights[0])
			if carry.claims[0] != c || carry.evidence[0] != e {
				t.Errorf("round trip differs:\n got claim %q\nwant claim %q\n got ev %q\nwant ev %q",
					carry.claims[0], c, carry.evidence[0], e)
			}
		})
	}
}

// TestDistillInsightLineNonRegression is part A's negative probe 3: an insight
// WITHOUT a control whitespace rune renders byte-identically to the tree before
// the fix.
//
// The pin is a digest of the whole rendered block of the standard fixture,
// taken from the UNCHANGED tree (recorded in the wave report next to the
// command that produced it). A digest and not a prose assertion, because the
// property under probe is "not one byte moved" and every weaker formulation
// would pass a render that quietly reflowed a line.
func TestDistillInsightLineNonRegression(t *testing.T) {
	content, over := distillRenderBlock(a9State(6), a9Opts())
	if over != 0 {
		t.Fatalf("%d dropped at n=6 — the fixture changed and the pin below is meaningless", over)
	}
	sum := sha256.Sum256([]byte(content))
	const want = "79edd335790b9a5a35fc9801d31783800b09fa248fece02065be6a21c4d7194e"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("the rendered block moved:\n got %s\nwant %s\n%s", got, want, content)
	}
}

// Below: wave C3-1 round 2, part B — the carry was counted twice (review
// major #3).

// c31CarryState builds an accumulator whose CARRY is a previously rendered
// block of n insights — the state the arm is in on every run after the first.
func c31CarryState(t *testing.T, n int) *distillBlockState {
	t.Helper()
	old := a9State(n)
	content, over := distillRenderBlock(old, a9Opts())
	if over != 0 {
		t.Fatalf("%d of %d insights fell out of the cap — the carry would not be the whole block", over, n)
	}
	carry, ok := distillSplitCarry(content)
	if !ok {
		t.Fatal("the arm cannot read back its own block")
	}
	if carry.count() != n {
		t.Fatalf("carry holds %d claims, want %d", carry.count(), n)
	}
	st := a9State(0)
	st.carry = carry
	return st
}

// TestDistillUsedRunesCountsTheCarryOnce is round 2's gate for review major #3.
//
// THE DEFECT, as a number. distillFrameRunes measures distillRenderN(st, opts,
// nil, nil, 0), and distillRenderN writes the CARRY LINES ITSELF into that
// buffer (the two loops over st.carry.claims / st.carry.evidence). distillUsedRunes
// then added the same lines a second time. Measured by the reviewer on a real
// block: 2 596 runes rendered, 3 856 reported — the overshoot is exactly the
// carry sum (1 260).
//
// WHY IT MATTERS NOW rather than before. The double count is INHERITED: the
// same two loops sat inside distillRenderBlock before this wave (cc1fe320,
// distill_block.go:458-464), and the render cut with that sum, so the block
// CONTENT was consistent with itself. Wave C3-1 made the same sum a STEERING
// value for the call loop, and there it is not a cut but a purchase decision:
// with five carried insights the meter reported 5 697 against a limit of 6 000
// and braked, while the block really stood at 3 432 and had room for roughly
// six more. The effective ceiling became `max_block_runes − carry length`,
// which is the cap change the briefing rules out, only in the other direction.
//
// The probe is the render itself: for zero new lines, "what the block costs"
// must equal "what the block renders".
func TestDistillUsedRunesCountsTheCarryOnce(t *testing.T) {
	opts := a9Opts()
	for _, n := range []int{0, 1, 3, 6} {
		t.Run(fmt.Sprintf("carry of %d", n), func(t *testing.T) {
			st := c31CarryState(t, n)
			real := utf8.RuneCountInString(distillRenderN(st, opts, nil, nil, 0))
			used := distillUsedRunes(st, opts, 0)
			if used != real {
				t.Errorf("distillUsedRunes = %d against a rendered %d — the carry is counted "+
					"%.1f times (overshoot %d runes)", used, real, float64(used)/float64(real), used-real)
			}
		})
	}

	// AND THE CONSEQUENCE THE DEFECT HAD, measured on the meter: a block whose
	// carry leaves plenty of room must not report itself exhausted.
	st := c31CarryState(t, 5)
	real := utf8.RuneCountInString(distillRenderN(st, opts, nil, nil, 0))
	m := distillNewRuneMeter(st, opts)
	if m.exhausted() {
		t.Errorf("the meter is exhausted at used=%d/limit=%d although the block renders %d runes "+
			"and holds room for ~%d more insights", m.used, m.limit, real, (opts.maxRunes-real)/m.next())
	}
	// full() and the meter must answer the same question the same way. They are
	// allowed to differ by the overflow note the meter reserves and full() does
	// not — never by the length of the carry.
	if slack := m.used - real; slack > utf8.RuneCountInString(distillOverflowNote(1)) {
		t.Errorf("the meter stands %d runes above the rendered block; only the overflow-note "+
			"reserve (%d) may separate them from full()'s arithmetic",
			slack, utf8.RuneCountInString(distillOverflowNote(1)))
	}
	if st.full(opts) {
		t.Error("full() reports a block with five carried insights as full under a cap of 6000")
	}
}

// TestDistillRenderCutAfterTheCarryFix is the VISIBLE CONSEQUENCE of the fix on
// the render path (round 2, review major #3).
//
// The cut is now driven by the same, correct sum, so a block that CARRIES
// material admits more new insights under an unchanged cap than it did before.
// That is a behaviour change of the render and it is deliberate: the previous
// number was wrong, the block never came close to the cap it was cut against,
// and the arm discarded gate-verified insights over an arithmetic error. The
// carry-free path is untouched — TestDistillInsightLineNonRegression pins that
// byte for byte.
func TestDistillRenderCutAfterTheCarryFix(t *testing.T) {
	opts := a9Opts()
	st := c31CarryState(t, 3)
	carryRunes := 0
	for _, l := range st.carry.claims {
		carryRunes += utf8.RuneCountInString(l) + 1
	}
	for _, l := range st.carry.evidence {
		carryRunes += utf8.RuneCountInString(l) + 1
	}

	// Twenty candidates against the standard cap: how many does the cut admit?
	fresh := a9State(20)
	st.insights = fresh.insights
	content, over := distillRenderBlock(st, opts)
	kept := len(st.insights) - over
	if n := utf8.RuneCountInString(content); n > opts.maxRunes {
		t.Fatalf("the block is %d runes over a cap of %d — the cut is not a cut", n, opts.maxRunes)
	}
	if kept <= 0 {
		t.Fatalf("the cut admitted nothing at all (over=%d)", over)
	}
	// The block really uses the room it was given: what is left under the cap is
	// less than one more insight. Before the fix the cut stopped `carryRunes`
	// runes early — with three carried insights of this fixture that is over a
	// thousand runes, i.e. two to three whole insights.
	rest := opts.maxRunes - utf8.RuneCountInString(content)
	if rest >= carryRunes {
		t.Errorf("the cut left %d runes unused under a cap of %d while the carry measures %d — "+
			"the block is still cut against a sum that counts the carry twice", rest, opts.maxRunes, carryRunes)
	}
	t.Logf("carry=%d runes, kept %d of 20 new insights, %d runes left under the cap",
		carryRunes, kept, rest)
}

// TestDistillShardRolloverState is wave W-L2's database-free half: what the
// rollover moves, what it leaves behind, and that the dedup set it builds
// actually suppresses a repeated line (amendment C4-2 A.3 b, A.4 c, A.6).
//
// The behavioural half — the run that rolls over, the watermark, the crash and
// the cost — is in distill_rollover_integration_test.go.
func TestDistillShardRolloverState(t *testing.T) {
	// The rendered lines of the fixture's insights, which is what the group set
	// and the carry are both made of.
	line := func(in distillKept) string {
		c, _ := distillInsightLine(in)
		return c
	}

	t.Run("the rollover seals the shard and opens the next", func(t *testing.T) {
		st := a9State(4)
		opts := a9Opts()
		// A cap that admits two of the four — the render then hands the other two
		// to the rollover instead of dropping them.
		opts.maxRunes = utf8.RuneCountInString(distillRenderN(a9State(0), opts, nil, nil, 0)) +
			2*utf8.RuneCountInString(line(st.insights[0])) +
			2*utf8.RuneCountInString(func() string { _, e := distillInsightLine(st.insights[0]); return e }())
		st.carry = distillCarry{claims: []string{"- **carried claim** [aaaaaaaa#1]\n"},
			evidence: []string{"- [aaaaaaaa#1] im Transkript geäußert: „x“ — Block `b`, Abschnitt 1.\n"}}
		st.shardCalls = 3

		_, over := distillRenderBlock(st, opts)
		if over == 0 {
			t.Fatalf("the fixture cap admits everything — nothing would move (maxRunes=%d)", opts.maxRunes)
		}
		want := append([]distillKept(nil), st.overflowInsights...)
		written := append([]string(nil), st.writtenClaims...)
		carried := st.carry.claims[0]

		st.rollover()

		if st.ordinal != 2 {
			t.Errorf("ordinal = %d, want 2", st.ordinal)
		}
		if st.carry.count() != 0 {
			t.Errorf("the new shard opens with %d carried claims, want 0", st.carry.count())
		}
		if st.shardCalls != 0 {
			t.Errorf("shardCalls = %d, want 0 — the progress condition must restart", st.shardCalls)
		}
		if len(st.insights) != len(want) {
			t.Fatalf("the new shard opens with %d insights, want the %d the cap could not take",
				len(st.insights), len(want))
		}
		for i := range want {
			if st.insights[i].claim != want[i].claim {
				t.Errorf("insight %d is %q, want %q", i, st.insights[i].claim, want[i].claim)
			}
		}
		if st.overflow != 0 {
			t.Errorf("overflow = %d, want 0 — the new shard has dropped nothing yet", st.overflow)
		}
		// The sealed shard's lines — its own carry AND what this run wrote into it
		// — are the group set of the new one.
		if _, ok := st.groupClaims[carried]; !ok {
			t.Error("the sealed shard's carried claim is not in the dedup set")
		}
		for _, l := range written {
			if _, ok := st.groupClaims[l]; !ok {
				t.Errorf("a line written into the sealed shard is not in the dedup set: %q", l)
			}
		}
	})

	t.Run("a line of another shard is not written a second time", func(t *testing.T) {
		st := a9State(2)
		opts := a9Opts()
		st.groupClaims[line(st.insights[0])] = struct{}{}

		content, over := distillRenderBlock(st, opts)
		if over != 0 {
			t.Fatalf("the cap dropped %d insights — the probe would be ambiguous", over)
		}
		if st.duplicates != 1 {
			t.Errorf("duplicates = %d, want 1 — the group set did not suppress the repeated line",
				st.duplicates)
		}
		if strings.Contains(content, "Kernaussage 1 ") {
			t.Error("the claim of another shard was written a second time")
		}
		if !strings.Contains(content, "Kernaussage 2 ") {
			t.Error("the group set suppressed a line no other shard carries")
		}
	})

	t.Run("the group set is never rendered into the block", func(t *testing.T) {
		st := a9State(0)
		st.groupClaims["- **eine Aussage aus einem anderen Shard** [aaaaaaaa#1]\n"] = struct{}{}
		content, _ := distillRenderBlock(st, a9Opts())
		if strings.Contains(content, "einem anderen Shard") {
			t.Error("the dedup set of the other shards was rendered into this one")
		}
	})

	t.Run("shardFull answers both brakes", func(t *testing.T) {
		st := a9State(0)
		if st.shardFull(distillExtractResult{}) {
			t.Error("an untouched shard is reported full")
		}
		if !st.shardFull(distillExtractResult{blockFull: true}) {
			t.Error("the rune meter's brake is not read as a full shard")
		}
		st.overflow = 1
		if !st.shardFull(distillExtractResult{}) {
			t.Error("a render that had to drop is not read as a full shard")
		}
	})
}

// TestDistillShardCapState is wave W-L3's database-free half: the off semantics
// of distill.max_blocks_per_root, the row bound it puts on the group read, and
// the handover counter it publishes (amendment C4-2 A.4 b, A.6 "W-L3").
//
// The behavioural half — the capped run, the capped seed, the bytes the group
// read moves and the counter on the written block — is in
// distill_cap_integration_test.go.
func TestDistillShardCapState(t *testing.T) {
	// The whole off semantics in one table, because it is the one property of
	// this key that a reader has to be able to check at a glance: 0 never
	// refuses, a cap refuses AT its own number, and a chain that is already
	// longer than a lowered cap is refused further growth rather than declared
	// invalid.
	t.Run("zero never caps, a cap binds at its own number", func(t *testing.T) {
		for _, tc := range []struct {
			maxShards, ordinal int
			want               bool
		}{
			{0, 1, false}, {0, 9, false}, {0, 1024, false}, // 0 = off, on every ordinal
			{2, 1, false}, {2, 2, true}, {2, 3, true}, // binds AT the cap, and above it
			{1, 1, true},   // a cap of 1 is "one block per range", not "no block"
			{64, 9, false}, // the A.4 (b) headroom shape: never binds at the expected size
		} {
			if got := distillShardCapReached(tc.maxShards, tc.ordinal); got != tc.want {
				t.Errorf("distillShardCapReached(%d, %d) = %v, want %v",
					tc.maxShards, tc.ordinal, got, tc.want)
			}
		}
	})

	// The group read's row bound follows the cap where there is one — plus the
	// one row of head room that makes an over-long chain VISIBLE instead of
	// silently truncated — and falls back to the named constant where there is
	// none.
	t.Run("the group limit follows the cap and falls back to the named bound", func(t *testing.T) {
		if got := distillShardGroupLimit(0); got != distillShardGroupMaxRows {
			t.Errorf("distillShardGroupLimit(0) = %d, want %d", got, distillShardGroupMaxRows)
		}
		for _, tc := range []struct{ cap, want int }{
			{1, 2}, {2, 3}, {64, 65},
			// Clamped to the same form limit the hard bound uses (re-review R3,
			// note #2): a chain cannot hold more than shard 1 plus the ordinals
			// 2…distillShardMaxOrdinal, so the two ceilings must not drift apart.
			{distillShardMaxOrdinal, distillShardMaxOrdinal + 1},
			{distillShardMaxOrdinal + 1, distillShardMaxOrdinal + 1},
			{1 << 20, distillShardMaxOrdinal + 1},
		} {
			if got := distillShardGroupLimit(tc.cap); got != tc.want {
				t.Errorf("distillShardGroupLimit(%d) = %d, want %d", tc.cap, got, tc.want)
			}
		}
		// The constant is a decision, not a detail: it has to stay far above the
		// smallest cap A.4 (b) calls sensible, or it would silently bind a
		// configuration an operator deliberately chose.
		if distillShardGroupMaxRows <= 64 {
			t.Errorf("distillShardGroupMaxRows = %d — at or below the smallest sensible cap (64), "+
				"so the fallback bound would truncate a configured chain", distillShardGroupMaxRows)
		}
	})

	// The counter is defined as an ordinal STEP, and this is where that
	// definition is pinned: it grows with the ordinal in one statement, so
	// `ordinal − rollovers` is the ordinal the run opened on and stays constant
	// over the whole run. That is the property the coverage key rests on, and it
	// is the one the W-L2 review's note #9 asks for instead of another summable
	// counter.
	t.Run("the handover counter is an ordinal step, not a sum", func(t *testing.T) {
		st := a9State(0)
		st.ordinal = 3 // a run that opened over two sealed shards
		start := st.ordinal - st.rollovers
		if st.rollovers != 0 {
			t.Fatalf("a fresh state states %d handovers, want 0", st.rollovers)
		}
		for i := 0; i < 3; i++ {
			st.rollover()
			if got := st.ordinal - st.rollovers; got != start {
				t.Errorf("after %d handover(s): ordinal − rollovers = %d, want the opening ordinal %d",
					i+1, got, start)
			}
		}
		if st.rollovers != 3 || st.ordinal != 6 {
			t.Errorf("ordinal/rollovers = %d/%d, want 6/3", st.ordinal, st.rollovers)
		}
	})

	// And the key reaches the block: the coverage object carries the count as a
	// number, next to the counters it must not be added to.
	t.Run("the coverage block carries the handover count", func(t *testing.T) {
		st := a9State(2)
		st.rollover()
		md := distillBlockMetadata(st, a9Opts(), 2)
		cov, ok := md["coverage"].(map[string]any)
		if !ok {
			t.Fatalf("coverage is %T, want a map", md["coverage"])
		}
		if got, ok := cov["shard_rollovers"]; !ok || got != 1 {
			t.Errorf("coverage.shard_rollovers = %v (present=%v), want 1", got, ok)
		}
		if got := md[distillMetaShardOrdinal]; got != 2 {
			t.Errorf("shard_ordinal = %v, want 2 — the axis the counter is read against", got)
		}
	})

	// The bounds of the chain, after the re-review: a form limit that no title may
	// exceed, an operating bound that the operator's cap can RAISE, and the
	// clamp between them.
	t.Run("the hard bound is the larger of the constant and the cap, clamped to the form limit",
		func(t *testing.T) {
			for _, tc := range []struct{ maxShards, want int }{
				{0, distillShardGroupMaxRows}, // no cap: the constant
				{2, distillShardGroupMaxRows}, // a small cap binds earlier anyway
				{300, 300},                    // a cap above the constant RAISES the bound
				{distillShardMaxOrdinal + 5, distillShardMaxOrdinal}, // clamped to the form
			} {
				if got := distillShardHardBound(tc.maxShards); got != tc.want {
					t.Errorf("distillShardHardBound(%d) = %d, want %d", tc.maxShards, got, tc.want)
				}
			}
		})

	// The form limit at the parser, which is what keeps a planted title from
	// setting the group's running ordinal to an arbitrary number (re-review note
	// #7: " — Teil 99999999993" produced exactly that).
	t.Run("an ordinal beyond the form limit is not a shard title", func(t *testing.T) {
		base := distillBlockTitle(a9Root, a9WMFrom, 1)
		for _, tc := range []struct {
			name, suffix string
			want         int
		}{
			{"canonical", "2", 2},
			{"at the form limit", strconv.Itoa(distillShardMaxOrdinal), distillShardMaxOrdinal},
			{"one past the form limit", strconv.Itoa(distillShardMaxOrdinal + 1), 0},
			{"absurd", "99999999993", 0},
			{"very long digits", strings.Repeat("9", 40), 0},
			{"trailing garbage", "2x", 0},
			{"leading zero", "007", 0},
		} {
			got, ok := distillShardOrdinalFrom(base, base+distillShardSuffix+tc.suffix)
			if tc.want == 0 {
				if ok {
					t.Errorf("%s: parsed as ordinal %d, want rejected", tc.name, got)
				}
				continue
			}
			if !ok || got != tc.want {
				t.Errorf("%s: got %d/%v, want %d/true", tc.name, got, ok, tc.want)
			}
		}
	})
}

// TestDistillShardHoldState is the database-free half of the W-L3 fix round: the
// two predicates the cap's material fidelity rests on (review blocker #1 and
// finding #2).
func TestDistillShardHoldState(t *testing.T) {
	// The progress condition asks whether the shard was ALREADY full when this
	// run found it — carry, not the lines this run wrote. The distinction is
	// load-bearing: counting this run's own lines reaches into C3-1's brake and
	// turned the measured "one blind first call" into two.
	t.Run("shardCarries counts the carry, not this run's own lines", func(t *testing.T) {
		st := a9State(0)
		if st.shardCarries() {
			t.Error("a fresh shard reports carried material")
		}
		st.writtenClaims = []string{"- **eine Zeile dieses Laufs** [aaaaaaaa#1]\n"}
		if st.shardCarries() {
			t.Error("lines this run wrote are read as carry — that is C3-1's brake, not the seed's gap")
		}
		st.carry = distillCarry{claims: []string{"- **eine Zeile eines frueheren Laufs** [aaaaaaaa#1]\n"}}
		if !st.shardCarries() {
			t.Error("a shard holding an earlier run's lines does not report them")
		}
	})

	// The ledger cut of the held-back batch: the prefix up to the first chunk
	// whose insight found no shard.
	t.Run("distillHeldFrom cuts at the first held-back insight", func(t *testing.T) {
		items := []distillsource.Item{
			{Origin: distillsource.Origin{BlockID: "b1", ChunkIndex: 10}},
			{Origin: distillsource.Origin{BlockID: "b1", ChunkIndex: 11}},
			{Origin: distillsource.Origin{BlockID: "b2", ChunkIndex: 12}},
			{Origin: distillsource.Origin{BlockID: "b2", ChunkIndex: 13}},
		}
		if got := distillHeldFrom(items, nil); got != len(items) {
			t.Errorf("no overflow: cut = %d, want %d — a batch that placed everything is booked whole",
				got, len(items))
		}
		over := []distillKept{{blockID: "b2", chunk: 12}, {blockID: "b2", chunk: 13}}
		if got := distillHeldFrom(items, over); got != 2 {
			t.Errorf("cut = %d, want 2 — the prefix the shard took must stay booked", got)
		}
		// An anchor naming no chunk of this batch (R2-1 allows that) is answered
		// conservatively: nothing is booked, which costs a re-purchase and loses
		// nothing.
		if got := distillHeldFrom(items, []distillKept{{blockID: "b9", chunk: 99}}); got != 0 {
			t.Errorf("unmatched anchor: cut = %d, want 0 (book nothing rather than book too much)", got)
		}
	})
}
