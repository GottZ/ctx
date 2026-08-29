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

// Below: wave W-L4 — the shard chain in the block text (amendment C4-2 A.6,
// design/02-destillat-arm.md:3310-3321).

// wl4ChainFrame is the chain line's cost WITHOUT the predecessor title and
// without the ordinal's digits, in runes.
//
// Pinned as a number because the wave's price is exactly this frame plus a
// title the identity already fixes: the line is charged to the 1500-rune prompt
// window and to distill.max_block_runes alike, so a later hand that adds half a
// sentence to it must see a test move.
const wl4ChainFrame = 45

// wl4RuneIndex is the offset of sub in s, counted in RUNES — the unit both
// llm.MaxBlockChars and the S7 measurement are written in. strings.Index alone
// answers bytes and would flatter every umlaut in the head.
func wl4RuneIndex(t *testing.T, s, sub string) int {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(s[:i])
}

// wl4Line returns the block's chain line as it stands in the text, or "".
//
// IT LOOKS FOR A LINE, NOT FOR A SUBSTRING, and that is the whole difference
// between a probe and a self-fulfilling one: the shard-2 title IS the shard-1
// title plus " — Teil 2", so strings.Contains(content, predecessorTitle) is
// true on a tree that has no chain line at all — through the block's own H1
// line. Measured before the wave (report §3, red probe).
func wl4Line(content string) string {
	for _, ln := range strings.Split(content, "\n") {
		if strings.HasPrefix(ln, "Teil ") {
			return ln
		}
	}
	return ""
}

// TestDistillShardChainLine pins the line itself: its form, its silence at
// shard 1 and the origin of the name it carries.
func TestDistillShardChainLine(t *testing.T) {
	t.Run("shard 1 and everything below it carries no chain line", func(t *testing.T) {
		for _, n := range []int{-1, 0, 1} {
			if got := distillChainLine(a9Root, a9WMFrom, n); got != "" {
				t.Errorf("ordinal %d renders %q, want the empty string — the stock is shard 1 by "+
					"construction and has no predecessor", n, got)
			}
		}
	})

	t.Run("a shard names its predecessor by the arm's own derived title", func(t *testing.T) {
		for _, n := range []int{2, 3, 10, distillShardMaxOrdinal} {
			line := distillChainLine(a9Root, a9WMFrom, n)
			pred := distillBlockTitle(a9Root, a9WMFrom, n-1)
			want := "\nTeil " + strconv.Itoa(n) + " dieses Bereichs — Fortsetzung von „" + pred + "“.\n"
			if line != want {
				t.Fatalf("ordinal %d renders\n got %q\nwant %q", n, line, want)
			}
			// The predecessor of shard 2 is the STOCK title — the coexistence path
			// of A.3 (c) read from the other end: the name the chain line hands a
			// reader is exactly the row that exists.
			if n == 2 && !strings.Contains(line, distillBlockTitle(a9Root, a9WMFrom, 1)+"“") {
				t.Error("shard 2 does not name the stock title as its predecessor")
			}
			if got, want := utf8.RuneCountInString(line),
				wl4ChainFrame+len(strconv.Itoa(n))+utf8.RuneCountInString(pred); got != want {
				t.Errorf("ordinal %d: the chain line is %d runes, want %d (frame %d + digits + title) — "+
					"the line's price in the prompt window is pinned, not free", n, got, want, wl4ChainFrame)
			}
		}
	})

	// It must never be readable as a per-insight line, on EITHER of the two paths
	// that parse this arm's bodies: the running shard's carry and the cross-shard
	// dedup set (distillReadShardGroup builds both with distillSplitCarry).
	t.Run("the chain line is not a bullet and cannot be read as a claim", func(t *testing.T) {
		line := distillChainLine(a9Root, a9WMFrom, 2)
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			t.Error("the chain line opens like a rendered insight line")
		}
		if got := distillBulletLines(line); len(got) != 0 {
			t.Errorf("distillBulletLines reads %d line(s) out of the chain line: %q", len(got), got)
		}
	})
}

// TestDistillShardChainInRenderedBlock is the wave's red gate turned green at
// the real render: "ein Shard-2-Block nennt seinen Vorgänger nirgends"
// (design/02:3311).
func TestDistillShardChainInRenderedBlock(t *testing.T) {
	t.Run("shard 1 renders no chain line at all", func(t *testing.T) {
		content, over := distillRenderBlock(a9State(6), a9Opts())
		if over != 0 {
			t.Fatalf("%d insights dropped — the fixture is not the standard one", over)
		}
		if got := wl4Line(content); got != "" {
			t.Errorf("the shard-1 block carries a chain line: %q — the stock body must not move", got)
		}
	})

	for _, n := range []int{2, 3, 7} {
		t.Run(fmt.Sprintf("shard %d names part and predecessor in its head", n), func(t *testing.T) {
			st := a9State(6)
			st.ordinal = n
			content, over := distillRenderBlock(st, a9Opts())
			if over != 0 {
				t.Fatalf("%d insights dropped — the fixture would not be comparable", over)
			}
			line := wl4Line(content)
			if line == "" {
				t.Fatal("no chain line in the rendered block")
			}
			if !strings.HasPrefix(line, fmt.Sprintf("Teil %d dieses Bereichs", n)) {
				t.Errorf("the chain line does not say which part this is: %q", line)
			}
			if pred := distillBlockTitle(a9Root, a9WMFrom, n-1); !strings.Contains(line, pred) {
				t.Errorf("the chain line does not name the predecessor %q: %q", pred, line)
			}
			// Position: head, not body. Between the trust paragraph and the claims.
			trust := strings.Index(content, "**UNTRUSTED, abgeleitet.**")
			chain := strings.Index(content, distillChainLine(a9Root, a9WMFrom, n))
			claims := strings.Index(content, distillSecClaims)
			if trust < 0 || trust >= chain || chain >= claims {
				t.Errorf("the chain line stands outside the head (trust=%d chain=%d claims=%d)",
					trust, chain, claims)
			}
		})
	}
}

// TestDistillShardChainPromptWindow is the wave's green gate and its head-length
// probe, measured AT THE CUT and not on the source text (design/02:3313-3318).
func TestDistillShardChainPromptWindow(t *testing.T) {
	st := a9State(6)
	st.ordinal = 2
	content, over := distillRenderBlock(st, a9Opts())
	if over != 0 {
		t.Fatalf("%d insights dropped — the fixture is not the standard one", over)
	}
	cut := util.TruncateRunesWithSuffix(content, redact.Truncated, llm.MaxBlockChars)
	chain := distillChainLine(a9Root, a9WMFrom, 2)

	// GREEN GATE: the line the synthesis prompt sees. Whole, predecessor title
	// included — a chain line cut in half names no block.
	if !strings.Contains(cut, chain) {
		t.Fatalf("the chain line is not complete inside the first %d runes:\n%s",
			llm.MaxBlockChars, cut)
	}

	// NEGATIVE PROBE, half 1: the trust sentence stays the FIRST paragraph and
	// stays whole. Both halves measured, because a paragraph that is first but
	// truncated tells a reader nothing.
	trust := wl4RuneIndex(t, cut, "**UNTRUSTED, abgeleitet.**")
	if trust < 0 || trust > 200 {
		t.Errorf("UNTRUSTED starts at rune %d, want inside the first 200 (S7, 16/16 blocks in the pilots)",
			trust)
	}
	if !strings.Contains(cut, "lebenden Fenster stand.\n") {
		t.Error("the trust paragraph is not complete inside the window")
	}
	if c := wl4RuneIndex(t, cut, chain); c < trust {
		t.Errorf("the chain line stands at rune %d, before the trust paragraph at %d", c, trust)
	}

	// THE COUNTER-VERSION, spelled out so the red is checked and not remembered:
	// the same block with the chain line in front of the trust paragraph. Both
	// orders keep the line inside the window — only this one pushes UNTRUSTED
	// past the 200-rune mark the pilots measured on every block.
	head := "# " + distillBlockTitle(a9Root, a9WMFrom, 2) + "\n"
	counter := head + chain + strings.TrimPrefix(strings.Replace(content, chain, "", 1), head)
	ccut := util.TruncateRunesWithSuffix(counter, redact.Truncated, llm.MaxBlockChars)
	if i := wl4RuneIndex(t, ccut, "**UNTRUSTED, abgeleitet.**"); i <= 200 {
		t.Errorf("the counter-version puts UNTRUSTED at rune %d — the probe measures nothing", i)
	}
	if i := wl4RuneIndex(t, ccut, chain); i > trust {
		t.Error("the counter-version did not move the chain line in front of the trust paragraph")
	}

	// NEGATIVE PROBE, half 2, at this fixture: no claim leaves the window, and
	// every claim that stands in it keeps its inline anchor. The general form of
	// the price is TestDistillShardChainWindowCost.
	without := util.TruncateRunesWithSuffix(strings.Replace(content, chain, "", 1),
		redact.Truncated, llm.MaxBlockChars)
	for i := 1; i <= 6; i++ {
		claim := fmt.Sprintf("Kernaussage %d ueber den Retrieval-Pfad und seine vier Arme.", i)
		if !strings.Contains(cut, claim) {
			t.Errorf("claim %d left the window with the chain line (it stood in it without: %v)",
				i, strings.Contains(without, claim))
		}
		anchor := "[" + distillShort8(a9Part1) + "#" + fmt.Sprint(i) + "]"
		if !strings.Contains(cut, anchor) {
			t.Errorf("claim %d carries no inline anchor %s inside the window", i, anchor)
		}
	}
}

// TestDistillShardChainWindowCost is the general form of the head-length probe,
// and it states the price instead of hoping a fixture hides it.
//
// WHAT THE GATE ASKS AND WHAT IS PROVABLE. design/02:3316 asks that the chain
// line displace NO claim from the 1500-rune window. In CONJUNCTION with the
// green gate — the line stands COMPLETE inside the window — that is unreachable
// whenever the dead space d = (1500 − head) mod claim-line is smaller than the
// line's length L: the window is a hard rune cut, claim lines are atomic, and
// the arm does not choose d. It is not unreachable in general (the review
// measured two geometries with d ≥ L where the head placement displaces
// nothing, and one placement — the line behind the last whole claim line —
// that displaces nothing anywhere but leaves only a cut half-line in the
// window, naming no block).
//
// WHAT THIS TEST PINS, and it is the strongest form that is true: the chain
// line costs EXACTLY its own length in the window and NOTHING ELSE. The number
// of whole claim lines inside the cut equals what the rune arithmetic predicts
// from the head plus L — no reflow, no second-order loss.
//
// WHAT IT DOES NOT PIN, corrected after the review (finding #3): the LENGTH of
// the line. This test computes its expectation from the same L it measures, so
// a longer line stays green here even though it displaces more — the length is
// pinned by the identity wl4ChainFrame + digits + predecessor title in
// TestDistillShardChainLine, and the assertion below re-states that identity so
// the numbers of this fan cannot drift away from it unnoticed.
func TestDistillShardChainWindowCost(t *testing.T) {
	// The dead space of the fixture in TestDistillShardChainPromptWindow is
	// larger than the line, which is why no claim moves there. The fan below
	// covers both sides of that threshold.
	for _, claimLen := range []int{60, 120, 200, 340} {
		t.Run(fmt.Sprintf("claim lines of %d runes", claimLen), func(t *testing.T) {
			st := a9State(0)
			st.ordinal = 2
			for i := 1; i <= 40; i++ {
				st.insights = append(st.insights, distillKept{
					claim:   fmt.Sprintf("A%02d ", i) + strings.Repeat("x", claimLen-4),
					quote:   strings.Repeat("q", 160),
					blockID: a9Part1, chunk: i,
				})
			}
			content, _ := distillRenderBlock(st, a9Opts())
			chain := distillChainLine(a9Root, a9WMFrom, 2)
			l := utf8.RuneCountInString(chain)
			without := strings.Replace(content, chain, "", 1)

			// The measured length against the pinned identity (review #3): this
			// fan's numbers are only meaningful for the line the wave decided on,
			// and a longer line must not pass here as "no effect".
			if want := wl4ChainFrame + 1 + utf8.RuneCountInString(
				distillBlockTitle(a9Root, a9WMFrom, 1)); l != want {
				t.Fatalf("the chain line measures %d runes against the pinned %d — the fan below "+
					"would report the cost of a line nobody decided on", l, want)
			}

			// The rune arithmetic: how many whole claim lines fit behind the head.
			line, _ := distillInsightLine(st.insights[0])
			per := utf8.RuneCountInString(line)
			fits := func(s string) int {
				head := utf8.RuneCountInString(s[:strings.Index(s, distillSecClaims)+len(distillSecClaims)])
				n := (llm.MaxBlockChars - head) / per
				return min(max(n, 0), len(st.writtenClaims))
			}
			count := func(s string) int {
				c := util.TruncateRunesWithSuffix(s, redact.Truncated, llm.MaxBlockChars)
				k := 0
				for i := 1; i <= 40; i++ {
					if strings.Contains(c, fmt.Sprintf("A%02d ", i)+strings.Repeat("x", claimLen-4)+"**") {
						k++
					}
				}
				return k
			}
			got, base := count(content), count(without)
			if want := fits(content); got != want {
				t.Errorf("%d claims inside the window, the arithmetic says %d — the chain line has an "+
					"effect beyond its own %d runes", got, want, l)
			}
			if want := fits(without); base != want {
				t.Errorf("without the chain line: %d claims inside the window, arithmetic says %d",
					base, want)
			}
			// The loss is bounded by the line's own length: never more claims than
			// fit into L runes.
			if lost := base - got; lost < 0 || lost > (l+per-1)/per {
				t.Errorf("the chain line moved %d claim(s) out of the window; %d runes can displace at "+
					"most %d line(s) of %d runes", lost, l, (l+per-1)/per, per)
			}
			t.Logf("claim line %d runes: %d claims in the window with the chain line, %d without "+
				"(chain %d runes)", per, got, base, l)
		})
	}
}

// TestDistillShardChainCarryRoundTrip is the wave's carry probe (design/02:3319):
// the new head text changes distillSplitCarry NOT — neither the running shard's
// carry nor the cross-shard dedup set may ever see the chain line.
func TestDistillShardChainCarryRoundTrip(t *testing.T) {
	st := a9State(3)
	st.ordinal = 2
	content, over := distillRenderBlock(st, a9Opts())
	if over != 0 {
		t.Fatalf("%d insights dropped — the round trip would be partial", over)
	}
	chain := distillChainLine(a9Root, a9WMFrom, 2)

	carry, ok := distillSplitCarry(content)
	if !ok {
		t.Fatal("the arm cannot read back a body carrying its own chain line")
	}
	// The same body without the chain line — the shape every stock block and
	// every shard 1 has. Both must split into the SAME carry.
	plain, ok2 := distillSplitCarry(strings.Replace(content, chain, "", 1))
	if !ok2 {
		t.Fatal("the arm cannot read back the body without the chain line")
	}
	if !slices.Equal(carry.claims, plain.claims) || !slices.Equal(carry.evidence, plain.evidence) {
		t.Errorf("the chain line changed the carry:\n with %q\nwithout %q", carry.claims, plain.claims)
	}
	if carry.count() != 3 {
		t.Errorf("carry holds %d claims, want 3", carry.count())
	}
	for _, l := range append(append([]string{}, carry.claims...), carry.evidence...) {
		if strings.Contains(l, "Fortsetzung von") || strings.Contains(l, "dieses Bereichs") {
			t.Errorf("the chain line was read as a per-insight line: %q", l)
		}
	}

	// The dedup set of the sealed shards is built from exactly this carry
	// (distillReadShardGroup), so the same body must not contribute the chain
	// line there either — probed on the set the state actually uses.
	next := a9State(0)
	for _, l := range carry.claims {
		next.groupClaims[l] = struct{}{}
	}
	if _, bad := next.groupClaims[strings.TrimPrefix(chain, "\n")]; bad {
		t.Error("the chain line entered the cross-shard dedup set")
	}
	if len(next.groupClaims) != 3 {
		t.Errorf("the dedup set holds %d lines, want 3", len(next.groupClaims))
	}

	// A body a previous wave wrote — no chain line anywhere — is still read.
	old := a9State(2)
	oldContent, _ := distillRenderBlock(old, a9Opts())
	if c, ok := distillSplitCarry(oldContent); !ok || c.count() != 2 {
		t.Errorf("a stock body without a chain line no longer reads back (ok=%v, claims=%d)",
			ok, c.count())
	}
}

// TestDistillShardChainCapHandover is the cap interaction the briefing asks for:
// a block that sits exactly at distill.max_block_runes WITH the chain line rolls
// the material on instead of losing it.
//
// The line is charged to the frame automatically — distillFrameRunes measures
// distillRenderN, which is where the line is written — so the call loop's meter
// and the render cut at the same number. The probe measures both halves: the
// charge, and the handover it causes.
func TestDistillShardChainCapHandover(t *testing.T) {
	opts := a9Opts()
	one, two := a9State(0), a9State(0)
	two.ordinal = 2
	chain := utf8.RuneCountInString(distillChainLine(a9Root, a9WMFrom, 2))

	t.Run("the chain line is charged to max_block_runes", func(t *testing.T) {
		got := distillFrameRunes(two, opts, 0) - distillFrameRunes(one, opts, 0)
		// The shard-2 title also carries the suffix; the difference of the two
		// frames is the chain line plus that suffix, and nothing else.
		suffix := utf8.RuneCountInString(distillBlockTitle(a9Root, a9WMFrom, 2)) -
			utf8.RuneCountInString(distillBlockTitle(a9Root, a9WMFrom, 1))
		if want := chain + suffix; got != want {
			t.Errorf("the shard-2 frame is %d runes larger than the shard-1 frame, want %d "+
				"(chain %d + title suffix %d)", got, want, chain, suffix)
		}
	})

	t.Run("an insight the chain line no longer leaves room for rolls on", func(t *testing.T) {
		st := a9State(4)
		st.ordinal = 2
		st.shardCalls = 3
		claim, ev := distillInsightLine(st.insights[0])
		pair := utf8.RuneCountInString(claim) + utf8.RuneCountInString(ev)
		// A cap the shard-1 frame would fit THREE insights into — exactly, to the
		// rune. With the chain line the third no longer fits.
		opts.maxRunes = utf8.RuneCountInString(distillRenderN(one, opts, nil, nil, 0)) + 3*pair

		content, over := distillRenderBlock(st, opts)
		if over == 0 {
			t.Fatalf("nothing rolled at a cap of %d runes — the probe measures nothing", opts.maxRunes)
		}
		if n := utf8.RuneCountInString(content); n > opts.maxRunes {
			t.Errorf("the block renders %d runes over a cap of %d — the chain line is not in the "+
				"cap arithmetic", n, opts.maxRunes)
		}
		if len(st.writtenClaims)+len(st.overflowInsights) != 4 {
			t.Fatalf("%d written + %d handed over, want 4 in total — material was dropped",
				len(st.writtenClaims), len(st.overflowInsights))
		}
		want := append([]distillKept(nil), st.overflowInsights...)

		st.rollover()
		if st.ordinal != 3 {
			t.Errorf("ordinal = %d, want 3", st.ordinal)
		}
		if len(st.insights) != len(want) {
			t.Fatalf("the next shard opens with %d insights, want the %d the cap could not take",
				len(st.insights), len(want))
		}
		next, _ := distillRenderBlock(st, opts)
		for _, in := range want {
			c, _ := distillInsightLine(in)
			if !strings.Contains(next, strings.TrimSuffix(c, "\n")) {
				t.Errorf("an insight the chain line displaced is in no shard: %q", in.claim)
			}
		}
		if line := wl4Line(next); !strings.Contains(line, distillBlockTitle(a9Root, a9WMFrom, 2)) {
			t.Errorf("the next shard does not name shard 2 as its predecessor: %q", line)
		}
	})
}

// TestDistillRollShardHoldsPaidMaterial pins WHICH EXITS of the shard handover
// hold a batch back — the answer distillBatch turns into "do not book these
// chunks and do not move the watermark" (W-L3 review, blocker #1).
//
// WAVE W-L4 ADDS THE THIRD AND FOURTH EXIT. The chain line takes runes out of
// every shard above the first, so a handover can now place fewer insights than
// it moved, and a shard opened by one can be full without ever having been
// called. Both states hold paid material that no shard took; booking their
// batches marks the chunks seen and moves the watermark past them, which is the
// loss the reviewer measured at another cut (10 of 12 claims, ledger 12).
//
// The exits that write (the successful handover) are probed in
// distill_cap_integration_test.go and distill_chain_integration_test.go, because
// their answer depends on a rendered, stored block.
func TestDistillRollShardHoldsPaidMaterial(t *testing.T) {
	// The state of a shard that is full through the RENDER (not through the rune
	// meter) and holds insights nothing took.
	held := func(ordinal int) *distillBlockState {
		st := a9State(0)
		st.ordinal = ordinal
		st.overflow = 1
		st.overflowInsights = []distillKept{{
			claim: "Eine bezahlte Aussage, die kein Shard genommen hat.",
			quote: "Zitat", blockID: a9Part1, chunk: 7,
		}}
		return st
	}
	var s *Scheduler

	for _, tc := range []struct {
		name      string
		ordinal   int
		maxShards int
		maxRunes  int // 0 keeps the fixture's own cap
		back      bool
		wantHeld  bool
	}{
		{name: "the shard cap holds the batch back", ordinal: 2, maxShards: 2, back: true, wantHeld: true},
		{name: "the shard cap without unplaced material books normally",
			ordinal: 2, maxShards: 2, back: false, wantHeld: false},
		{name: "the hard bound holds the batch back",
			ordinal: distillShardGroupMaxRows, maxShards: 0, back: true, wantHeld: true},
		{name: "an empty shard that is still full holds the batch back",
			ordinal: 2, maxShards: 0, back: true, wantHeld: true},
		{name: "an empty shard that is still full and placed everything books normally",
			ordinal: 2, maxShards: 0, back: false, wantHeld: false},
		// The band between the two floors (wave W-L4 fix round): a successor that
		// could place nothing is not opened, and the material it would have
		// carried is held back exactly like at the cap.
		{name: "a successor that could place nothing is not opened",
			ordinal: 2, maxShards: 0, maxRunes: 1700, back: true, wantHeld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := held(tc.ordinal)
			if !tc.back {
				st.overflowInsights = nil
			}
			opts := a9Opts()
			opts.maxShards = tc.maxShards
			if tc.maxRunes > 0 {
				opts.maxRunes = tc.maxRunes
			}
			var l distillLedger
			stop, rolled, gotHeld, err := s.distillRollShard(t.Context(),
				distillTick{block: st, write: opts}, "probe", distillExtractResult{}, &l)
			if err != nil {
				t.Fatalf("roll: %v", err)
			}
			if stop != distillSkipBudget {
				t.Errorf("stop = %q, want %q — every exit here ends the run",
					stop, distillSkipBudget)
			}
			if rolled {
				t.Error("the exit reports a handover although it refused one")
			}
			if gotHeld != tc.wantHeld {
				t.Errorf("held = %v, want %v — paid material that no shard took must never be "+
					"booked as covered", gotHeld, tc.wantHeld)
			}
			if l.blocksWritten != 0 {
				t.Errorf("blocksWritten = %d, want 0 — a refused handover writes nothing",
					l.blocksWritten)
			}
		})
	}
}

// TestDistillShardSuccessorPlaces pins the predicate the hand-over refusal rests
// on (wave W-L4 fix round, review finding #1): would opening the next shard
// place anything at all?
//
// THE TWO FLOORS ARE THE SUBJECT. A shard above the first carries the title
// suffix and the chain line, so `distill.max_block_runes` has a higher floor for
// a successor than for shard 1 — and in the band between them the old code
// opened one empty shard per tick (measured: 9 shards after 8 ticks, 8 empty).
func TestDistillShardSuccessorPlaces(t *testing.T) {
	// The two floors, measured against the production arithmetic rather than
	// written down as literals: they move with every render change.
	floor := func(ordinal, need int) int {
		st := a9State(0)
		st.ordinal = ordinal
		return distillFrameRunes(st, distillWriteOpts{maxRunes: 1 << 20}, 1) + need
	}
	first := floor(1, distillMinInsightRunes)
	second := floor(2, distillMinInsightRunes)
	if second <= first {
		t.Fatalf("the successor floor (%d) is not above shard 1's (%d) — the chain line would cost "+
			"nothing and this probe would measure nothing", second, first)
	}
	t.Logf("floors: shard 1 needs %d runes, its successor %d (delta %d)", first, second, second-first)

	t.Run("no cap never refuses", func(t *testing.T) {
		st := a9State(0)
		if !st.successorPlaces(distillWriteOpts{maxRunes: 0}) {
			t.Error("max_block_runes 0 is the off switch and must never refuse a hand-over")
		}
	})

	t.Run("inside the band the successor is refused, above it opened", func(t *testing.T) {
		st := a9State(0)
		st.ordinal = 1
		if st.successorPlaces(distillWriteOpts{maxRunes: second - 1}) {
			t.Errorf("a cap of %d admits a successor that cannot hold the smallest possible insight",
				second-1)
		}
		if !st.successorPlaces(distillWriteOpts{maxRunes: second}) {
			t.Errorf("a cap of %d is exactly the successor's floor and must open it", second)
		}
	})

	// With material moving, the question is not the theoretical floor but the
	// first insight that would move — the render admits in order and stops at the
	// first line that does not fit, so a successor that cannot take that one
	// takes none of them.
	t.Run("moving material is measured by its first insight, not by the minimum", func(t *testing.T) {
		st := a9State(1)
		st.ordinal = 1
		st.overflowInsights = []distillKept{st.insights[0]}
		c, e := distillInsightLine(st.insights[0])
		pair := utf8.RuneCountInString(c) + utf8.RuneCountInString(e)
		if pair <= distillMinInsightRunes {
			t.Fatalf("the fixture insight (%d runes) is not larger than the minimum (%d) — the two "+
				"halves would be indistinguishable", pair, distillMinInsightRunes)
		}
		withMaterial := floor(2, pair)
		if st.successorPlaces(distillWriteOpts{maxRunes: withMaterial - 1}) {
			t.Errorf("a cap of %d opens a successor that cannot take the insight it would receive",
				withMaterial-1)
		}
		if !st.successorPlaces(distillWriteOpts{maxRunes: withMaterial}) {
			t.Errorf("a cap of %d fits the moved insight exactly and must open the successor",
				withMaterial)
		}
		// And the minimum alone would have said yes at that cap — which is the
		// whole reason the predicate looks at the material.
		empty := a9State(0)
		empty.ordinal = 1
		if !empty.successorPlaces(distillWriteOpts{maxRunes: withMaterial - 1}) {
			t.Error("the fixture cap is below the theoretical floor too — the probe cannot tell the " +
				"two halves apart")
		}
	})

	// R2 #1: the overflow note counts its insights, and "10" is one rune wider
	// than "9" — a reserve sized for one note under-counts from the tenth moved
	// insight on, and the predicate would open the very empty shard it refuses.
	t.Run("ten moved insights reserve a two-digit overflow note", func(t *testing.T) {
		st := a9State(10)
		st.ordinal = 1
		st.overflowInsights = st.insights
		c, e := distillInsightLine(st.insights[0])
		pair := utf8.RuneCountInString(c) + utf8.RuneCountInString(e)
		tight := floor(2, pair) // floor reserves a ONE-digit note by construction
		if st.successorPlaces(distillWriteOpts{maxRunes: tight}) {
			t.Errorf("a cap of %d admits ten moved insights although their overflow note is one "+
				"rune wider than a one-note reserve", tight)
		}
		if !st.successorPlaces(distillWriteOpts{maxRunes: tight + 1}) {
			t.Errorf("a cap of %d fits the two-digit note and must open the successor", tight+1)
		}
	})

	// It asks about a shard that does not exist yet, so it works on a copy — and
	// a predicate that quietly raised the ordinal of the running block would
	// rename it at the next write.
	t.Run("the predicate does not move the state it asks about", func(t *testing.T) {
		st := a9State(2)
		st.ordinal = 3
		st.carry = distillCarry{claims: []string{"- **eine getragene Aussage** [aaaaaaaa#1]\n"},
			evidence: []string{"- [aaaaaaaa#1] im Transkript geäußert: „x“ — Block `b`, Abschnitt 1.\n"}}
		before, _ := distillRenderBlock(st, a9Opts())
		st.successorPlaces(a9Opts())
		after, _ := distillRenderBlock(st, a9Opts())
		if before != after {
			t.Error("the rendered block changed after asking the predicate")
		}
		if st.ordinal != 3 {
			t.Errorf("ordinal = %d, want 3 — the predicate moved the running shard", st.ordinal)
		}
		if st.carry.count() != 1 {
			t.Errorf("carry holds %d claims, want 1 — the predicate cleared the running carry",
				st.carry.count())
		}
	})
}
