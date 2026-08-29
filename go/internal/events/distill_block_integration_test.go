//go:build integration

// Gate A02-9 (design/02 §7.2), database half: the written row and its seven
// properties, the audit exemption, the metadata white list at the column, the
// squatted title, the write order, the upsert identity and the embed cascade.
//
// The half that needs no database — the block format in the 1500-rune window,
// the metadata builder, the per-insight detector and the untrusted framing — is
// in distill_block_test.go.
//
// NO REAL LLM CALL: the stub sits behind the backend seam exactly as in A02-8,
// so everything above it is production code — prompt, chain, gate, accumulator,
// upsert. What is faked is the model, never the pipeline.
//
//	go test -tags=integration ./internal/events/ -run TestDistillBlockWrite -count=1 -v
package events

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/distillsource/ctxcheckpoint"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	a9Man1 = "019f5b5f-e51c-7a94-a374-91c1044911a1"
	a9Man2 = "019f5b5f-e51c-7a94-a374-91c1044911a2"
	a9SHA  = "6f1c2d3e4a5b60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"
	a9AKIA = "AKIAIOSFODNN7EXAMPLE"
)

// a9ManifestN is the provenance of the n-th batch's compaction.
func a9ManifestN(n int) distillsource.Manifest {
	ids := []string{a9Man1, a9Man2}
	return distillsource.Manifest{
		ID:              ids[(n-1)%len(ids)],
		SHA256:          a9SHA,
		ParentID:        a9Man1,
		ActiveSessionID: fmt.Sprintf("20260712_205012_00000%d", n),
	}
}

// a9Stub answers with ONE insight per address the prompt showed, and every
// claim carries the address it was built from — so a repeated insight is
// visible as a repeated STRING rather than having to be inferred from a count.
// a8AnswerFromPrompt cannot do that: its claim is a constant.
func a9Stub(req a8Request) (string, int) {
	addrs := a8Addrs(req.User)
	ins := make([]map[string]any, 0, len(addrs))
	for _, a := range addrs {
		q := a8QuoteFrom(req.User, a)
		if q == "" {
			continue
		}
		ins = append(ins, map[string]any{
			"claim": a8Claim + " Beleg " + a.block + "-" + strconv.Itoa(a.chunk),
			"quote": q, "block": a.block, "chunk": a.chunk, "kind": "finding",
		})
	}
	return a8Answer(ins...), http.StatusOK
}

// a9Source serves `batches` batches of `perBatch` chunks, each batch carrying
// its own manifest. failAt > 0 makes the read of that batch fail.
//
// IT ANSWERS `after`, unlike the A02-8 fixture next to it, and that is what
// makes a SECOND run over the same journal meaningful: batch n is a function of
// the caller's watermark, so a run that resumes gets the material it has not
// covered and a run that repeats a range gets byte-identical texts the dedup
// ledger can recognise. A counter-driven source would hand run 2 the next batch
// no matter what the watermark said, and every multi-run probe below would
// measure the fixture instead of the arm.
func a9Source(batches, perBatch, failAt int) *fakeDistillSource {
	return &fakeDistillSource{
		sessions: []distillsource.Ref{{Session: dfRoot, Watermark: int64(batches) * 10}},
		head:     map[string]int64{dfRoot: int64(batches) * 10},
		hasNew:   map[string]bool{dfRoot: true},
		readFn: func(after int64) (distillsource.Batch, error) {
			idx := int(after/10) + 1
			if idx > batches {
				return distillsource.Batch{Watermark: after, Complete: true}, nil
			}
			if idx == failAt {
				return distillsource.Batch{}, distillsource.ErrQueryFailed
			}
			man := a9ManifestN(idx)
			parts := []string{a8Block1, a8Block2}
			items := make([]distillsource.Item, 0, perBatch)
			for i := 0; i < perBatch; i++ {
				items = append(items, distillsource.Item{
					Text:        fmt.Sprintf("[b%d c%d] %s", idx, i, a8Body),
					Origin:      distillsource.Origin{BlockID: parts[i%len(parts)], ChunkIndex: idx*100 + i, Role: "user"},
					Manifest:    man,
					Sensitivity: backends.SensCredentials,
					Untrusted:   true,
				})
			}
			return distillsource.Batch{Items: items, Watermark: int64(idx) * 10, Complete: true}, nil
		},
	}
}

// a9Block is one written insight row.
type a9Block struct {
	id, title, typeName, typeSource string
	sens, sensSource, scope         string
	content                         string
	metaKeys                        []string
	manifestID                      string
	detectorKind, detectorReason    string
	insightCount                    int
	hasGuardKey                     bool
}

func a9Blocks(t *testing.T, pool *pgxpool.Pool) []a9Block {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id::text, title, type_name, type_source, sensitivity, sensitivity_source, scope, content,
		       ARRAY(SELECT jsonb_object_keys(metadata) ORDER BY 1),
		       COALESCE(metadata->>'manifest_id',''),
		       COALESCE(metadata->'sensitivity_detector'->>'kind',''),
		       COALESCE(metadata->'sensitivity_detector'->>'reason',''),
		       COALESCE((metadata->>'insight_count')::int, -1),
		       metadata ? 'guard_checked_at'
		  FROM context_blocks
		 WHERE category = 'session-insights' AND NOT is_archived
		 ORDER BY title`)
	if err != nil {
		t.Fatalf("select insight blocks: %v", err)
	}
	defer rows.Close()
	var out []a9Block
	for rows.Next() {
		var b a9Block
		if err := rows.Scan(&b.id, &b.title, &b.typeName, &b.typeSource, &b.sens, &b.sensSource,
			&b.scope, &b.content, &b.metaKeys, &b.manifestID, &b.detectorKind, &b.detectorReason,
			&b.insightCount, &b.hasGuardKey); err != nil {
			t.Fatalf("scan insight block: %v", err)
		}
		out = append(out, b)
	}
	return out
}

func a9Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	a8Truncate(t, pool)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM context_blocks WHERE category = 'session-insights'`); err != nil {
		t.Fatalf("clear insight blocks: %v", err)
	}
}

// a9Ledger reads the column this wave fills.
func a9Written(t *testing.T, pool *pgxpool.Pool, key string) (written int, outcome, errClass string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT blocks_written, outcome, COALESCE(error,'')
		  FROM distill_run WHERE source_key = $1 ORDER BY started_at DESC LIMIT 1`, key).
		Scan(&written, &outcome, &errClass); err != nil {
		t.Fatalf("read blocks_written: %v", err)
	}
	return
}

// a9Run drives one full tick against the stub.
func a9Run(t *testing.T, pool *pgxpool.Pool, src distillsource.Source) {
	t.Helper()
	stub := a8NewStub(t, a9Stub)
	s := a8Scheduler(pool, a8Config(), src, a8Pool(stub.srv.URL))
	s.distillOnce(context.Background(), dfNoDemand)
}

func TestDistillBlockWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key := distillSourceKey(dfLabel, dfScope, dfRoot)
	base := []string{
		"active_session_id", "coverage", "evidence_date", "gen", "insight_count",
		"invalidated_by", "manifest_id", "manifest_sha256", "model",
		"parent_manifest_id", "root_session_id", "run_id", "source_block_ids",
		"source_kind", "source_label", "warnings", "watermark_from", "watermark_to",
	}

	// GATE 1 — the arm writes a block, and it carries the seven properties
	// §7.2 names. The red is measured in the wave report: insights_kept = 1 and
	// 0 insight blocks on the unchanged tree.
	t.Run("green: the run writes one block with the seven properties", func(t *testing.T) {
		a9Truncate(t, pool)
		a9Run(t, pool, a9Source(1, 2, 0))

		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d insight blocks, want exactly 1", len(blocks))
		}
		b := blocks[0]
		if b.typeName != "insight" || b.typeSource != "manual" {
			t.Errorf("type = %q/%q, want insight/manual", b.typeName, b.typeSource)
		}
		if b.sens != string(backends.SensCredentials) || b.sensSource != "derived" {
			t.Errorf("sensitivity = %q/%q, want credentials/derived", b.sens, b.sensSource)
		}
		if b.scope == distillForbiddenScope || b.scope != dfScope {
			t.Errorf("scope = %q, want %q and never %q", b.scope, dfScope, distillForbiddenScope)
		}
		if b.manifestID == "" {
			t.Error("metadata.manifest_id is empty — the block names no compaction")
		}
		if b.title != distillBlockTitle(dfRoot, 0) {
			t.Errorf("title = %q, want %q", b.title, distillBlockTitle(dfRoot, 0))
		}
		if b.insightCount < 1 {
			t.Errorf("insight_count = %d, want at least one", b.insightCount)
		}
		if !strings.Contains(b.content, "UNTRUSTED") {
			t.Error("the block content carries no UNTRUSTED framing")
		}

		written, outcome, errClass := a9Written(t, pool, key)
		if written < 1 || outcome != distillOutcomeOk || errClass != "" {
			t.Errorf("journal blocks_written/outcome/error = %d/%q/%q, want >=1/ok/\"\"",
				written, outcome, errClass)
		}
	})

	// GATE 3's database half: a derived block is NOT audit-selectable. The red
	// (source='default', selectable) is in the wave report.
	t.Run("the written block is exempt from the sensitivity audit", func(t *testing.T) {
		// EVERY SUB-TEST STANDS ON ITS OWN STATE (round-2 minor #8). Three of
		// them used to read the row the FIRST one wrote, so a -run-filtered
		// re-run — the normal way to re-check a single gate — panicked on an
		// empty slice instead of measuring anything.
		a9Truncate(t, pool)
		a9Run(t, pool, a9Source(1, 2, 0))

		picked, err := store.PickAuditBlocks(ctx, pool, dfScope, 0, 100, false)
		if err != nil {
			t.Fatalf("pick audit blocks: %v", err)
		}
		for _, b := range a9Blocks(t, pool) {
			for _, p := range picked {
				if p.ID == b.id {
					t.Errorf("block %s is queued for an LLM audit of its own transcript prose", b.id)
				}
			}
		}
	})

	// GATE 5's database half. The red is in the report: a manifest's own map
	// passed through carried `platform` and three more foreign keys.
	t.Run("the written metadata is exactly the pinned key set", func(t *testing.T) {
		a9Truncate(t, pool)
		a9Run(t, pool, a9Source(1, 2, 0))
		b := a9Blocks(t, pool)[0]
		if strings.Join(b.metaKeys, ",") != strings.Join(base, ",") {
			t.Errorf("keys = %v\nwant %v", b.metaKeys, base)
		}
		if b.hasGuardKey {
			t.Error("guard_checked_at reached the block — a guard off-switch in a JSON field")
		}
	})

	// GATE 12 (R2-1): a block written from two parts names BOTH in
	// source_block_ids, which is what makes the anchor's "one containing part"
	// semantics checkable.
	t.Run("R2-1: source_block_ids carries every part of the run", func(t *testing.T) {
		a9Truncate(t, pool)
		a9Run(t, pool, a9Source(1, 2, 0))
		var ids []string
		if err := pool.QueryRow(ctx, `
			SELECT ARRAY(SELECT jsonb_array_elements_text(metadata->'source_block_ids') ORDER BY 1)
			  FROM context_blocks WHERE category='session-insights' AND NOT is_archived LIMIT 1`).
			Scan(&ids); err != nil {
			t.Fatalf("read source_block_ids: %v", err)
		}
		want := []string{a8Block1, a8Block2}
		if len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
			t.Errorf("source_block_ids = %v, want %v", ids, want)
		}
	})

	// GATE 4 — the detector, injected PAST G1-G7 by driving the write path
	// directly. Red (report): the same insight rendered into the content gave
	// sensitivity_source='pattern' and left the secret in the corpus.
	t.Run("an AKIA insight past the gate leaves derived provenance and no secret", func(t *testing.T) {
		a9Truncate(t, pool)
		stub := a8NewStub(t, a9Stub)
		s := a8Scheduler(pool, a8Config(), a9Source(1, 1, 0), a8Pool(stub.srv.URL))

		st := newDistillBlockState(dfRoot, a9Man1, 0)
		st.newest = a9ManifestN(1)
		st.addBatch(distillExtractResult{insights: []distillKept{
			{claim: "Der Deploy nutzt " + a9AKIA + " als Zugang.",
				quote:   strings.Repeat("harmloser Zitattext aus dem Transkript. ", 2),
				blockID: a8Block1, chunk: 1},
			{claim: "Die Migration 147 hat einen deterministischen Tiebreak eingebaut.",
				quote:   strings.Repeat("harmloser Zitattext aus dem Transkript. ", 2),
				blockID: a8Block2, chunk: 1},
		}}, distillLedger{}, 10, nil)

		opts := distillWriteOpts{
			category: "session-insights", scope: dfScope, typeName: "insight",
			sensitivity: backends.SensCredentials, maxRunes: 6000, sourceLabel: dfLabel,
		}
		if err := s.distillWriteBlock(ctx, opts, st); err != nil {
			t.Fatalf("write: %v", err)
		}
		b := a9Blocks(t, pool)[0]
		if b.sens != string(backends.SensCredentials) || b.sensSource != "derived" {
			t.Errorf("sensitivity = %q/%q, want credentials/derived (Festlegung 5)", b.sens, b.sensSource)
		}
		if b.detectorKind != "aws-key" {
			t.Errorf("sensitivity_detector.kind = %q, want aws-key", b.detectorKind)
		}
		if strings.Contains(b.detectorReason, a9AKIA) {
			t.Errorf("the reason carries the matched secret: %q", b.detectorReason)
		}
		if strings.Contains(b.content, a9AKIA) {
			t.Error("the secret stands in the corpus content")
		}
		if b.insightCount != 1 {
			t.Errorf("insight_count = %d, want 1 — the offending insight is dropped, not written", b.insightCount)
		}
		want := append([]string{}, base...)
		want = append(want, "sensitivity_detector")
		slices.Sort(want)
		if strings.Join(b.metaKeys, ",") != strings.Join(want, ",") {
			t.Errorf("keys on a hit = %v\nwant %v", b.metaKeys, want)
		}
	})

	// GATE 8 — title squatting, all three shapes the identity can be attacked in.
	// §7.2 admits either answer per case ("credentials/derived ODER
	// failed/block_write_failed"); which one applies follows from whether the
	// arm can tell that the body is its own.
	t.Run("title squatting", func(t *testing.T) {
		title := distillBlockTitle(dfRoot, 0)

		// (a) A FOREIGN TYPE on the arm's identity: refused before a single call.
		// The refusal moved in front of the run in round 2 (the seed reads the
		// row anyway), which is strictly better than round 1's mid-run refusal —
		// nothing is bought at all.
		t.Run("a foreign type ends the run without a write and without a call", func(t *testing.T) {
			a9Truncate(t, pool)
			if _, err := store.UpsertBlock(ctx, pool, "session-insights", title, "fremder Typ",
				nil, map[string]any{}, dfScope, true,
				store.SensitivityWrite{Value: backends.SensPublic, Manual: true}, "knowledge"); err != nil {
				t.Fatalf("seed squatter: %v", err)
			}
			a9Run(t, pool, a9Source(1, 2, 0))

			var outcome, errClass string
			if err := pool.QueryRow(ctx, `
				SELECT outcome, COALESCE(error,'') FROM distill_run
				 WHERE source_key=$1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&outcome, &errClass); err != nil {
				t.Fatalf("journal: %v", err)
			}
			if outcome != distillOutcomeFailed || errClass != distillErrBlockWriteFailed {
				t.Errorf("journal = %q/%q, want failed/block_write_failed", outcome, errClass)
			}
			b := a9Blocks(t, pool)[0]
			if b.content != "fremder Typ" || b.typeName != "knowledge" || b.sens != string(backends.SensPublic) {
				t.Errorf("the foreign row was touched: %q/%q/%q", b.content, b.typeName, b.sens)
			}
			if rows := a8Rows(t, pool); len(rows) != 0 {
				t.Errorf("%d llm calls were made for a run that could never write", len(rows))
			}
		})

		// (b) The arm's OWN TYPE but a body it did not write. Round 1 overwrote
		// it; since the carry exists that would silently destroy content whose
		// shape the arm cannot read, so it is refused instead.
		t.Run("an unreadable body on the arm's own type is refused, not replaced", func(t *testing.T) {
			a9Truncate(t, pool)
			if _, err := store.UpsertBlock(ctx, pool, "session-insights", title, "fremder Inhalt",
				nil, map[string]any{}, dfScope, true,
				store.SensitivityWrite{Value: backends.SensPublic, Manual: true}, "insight"); err != nil {
				t.Fatalf("seed squatter: %v", err)
			}
			a9Run(t, pool, a9Source(1, 2, 0))

			var outcome, errClass string
			if err := pool.QueryRow(ctx, `
				SELECT outcome, COALESCE(error,'') FROM distill_run
				 WHERE source_key=$1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&outcome, &errClass); err != nil {
				t.Fatalf("journal: %v", err)
			}
			if outcome != distillOutcomeFailed || errClass != distillErrBlockWriteFailed {
				t.Errorf("journal = %q/%q, want failed/block_write_failed", outcome, errClass)
			}
			if b := a9Blocks(t, pool)[0]; b.content != "fremder Inhalt" {
				t.Errorf("the foreign body was replaced: %q", b.content)
			}
		})

		// (c) FESTLEGUNG 4(a), in the shape that is actually reachable: the arm's
		// own block, whose sensitivity someone lowered afterwards. The next run
		// raises it back — upgrade-only over sensRankSQL, which is what the
		// Derived badge buys.
		t.Run("a lowered sensitivity on the arm's own block is raised again", func(t *testing.T) {
			a9Truncate(t, pool)
			a9Run(t, pool, a9Source(2, 2, 0))
			if _, err := pool.Exec(ctx, `
				UPDATE context_blocks SET sensitivity='public', sensitivity_source='manual'
				 WHERE category='session-insights' AND title=$1 AND scope=$2`, title, dfScope); err != nil {
				t.Fatalf("lower the row: %v", err)
			}
			if b := a9Blocks(t, pool)[0]; b.sens != string(backends.SensPublic) {
				t.Fatalf("the row is %q — the probe would be vacuous", b.sens)
			}
			// A second run over the SAME identity: the brake left the watermark,
			// so the next write is an upsert onto the lowered row.
			a8ClearWindow(t, pool)
			stub := a8NewStub(t, a9Stub)
			s := a8Scheduler(pool, a8Config(), a9Source(2, 2, 0), a8Pool(stub.srv.URL))
			st, err := s.distillSeedBlock(ctx, distillWriteOpts{
				category: "session-insights", scope: dfScope, typeName: "insight",
				sensitivity: backends.SensCredentials, maxRunes: 6000, sourceLabel: dfLabel,
			}, dfRoot, 0)
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			opts := distillWriteOpts{
				category: "session-insights", scope: dfScope, typeName: "insight",
				sensitivity: backends.SensCredentials, maxRunes: 6000, sourceLabel: dfLabel,
			}
			if err := s.distillWriteBlock(ctx, opts, st); err != nil {
				t.Fatalf("write: %v", err)
			}
			b := a9Blocks(t, pool)[0]
			if b.sens != string(backends.SensCredentials) || b.sensSource != "derived" {
				t.Errorf("row = %q/%q, want credentials/derived — upgrade-only over sensRankSQL",
					b.sens, b.sensSource)
			}
			if strings.Count(b.content, "- **") == 0 {
				t.Error("the raise dropped the block's insights")
			}
		})
	})

	// GATE 11 — the upsert identity, all three statements.
	t.Run("upsert identity", func(t *testing.T) {
		a9Truncate(t, pool)
		a9Run(t, pool, a9Source(2, 2, 0))

		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d blocks after a two-batch run, want 1 growing block", len(blocks))
		}
		first := blocks[0]
		if n := strings.Count(first.content, "Beleg "); n < 4 {
			t.Errorf("the block carries %d claims, want both batches' insights", n)
		}
		if first.insightCount < 2 {
			t.Errorf("insight_count = %d after two batches", first.insightCount)
		}

		// A second run over a NEW range is a second block: the title is anchored
		// on watermark_from, which the first run moved.
		a9Run(t, pool, a9Source(3, 2, 0))
		if blocks = a9Blocks(t, pool); len(blocks) != 2 {
			t.Fatalf("%d blocks after a second run over a new range, want 2", len(blocks))
		}

		// A repeated upsert of an UNCHANGED content keeps the embedding — the
		// property §4.5.4 rests its write order on.
		t.Run("a repeated identical write keeps the embedding", func(t *testing.T) {
			id := blocks[0].id
			if _, err := pool.Exec(ctx,
				`UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector,
				        embed_model = 'probe' WHERE id = $1`, id); err != nil {
				t.Fatalf("seed embedding: %v", err)
			}
			stub := a8NewStub(t, a9Stub)
			s := a8Scheduler(pool, a8Config(), a9Source(1, 1, 0), a8Pool(stub.srv.URL))

			st := newDistillBlockState(dfRoot, a9Man1, 0)
			st.newest = a9ManifestN(1)
			st.addBatch(distillExtractResult{insights: []distillKept{
				{claim: "Die Migration 147 hat einen deterministischen Tiebreak eingebaut.",
					quote: "harmloser Zitattext", blockID: a8Block1, chunk: 1},
			}}, distillLedger{}, 10, nil)
			opts := distillWriteOpts{
				category: "session-insights", scope: dfScope, typeName: "insight",
				sensitivity: backends.SensCredentials, maxRunes: 6000, sourceLabel: dfLabel,
			}
			// Write once so the row's content matches the state, then seed the
			// vector, then write the SAME state again.
			if err := s.distillWriteBlock(ctx, opts, st); err != nil {
				t.Fatalf("first write: %v", err)
			}
			if _, err := pool.Exec(ctx,
				`UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector,
				        embed_model = 'probe'
				  WHERE category='session-insights' AND title=$1 AND scope=$2`,
				distillBlockTitle(dfRoot, 0), dfScope); err != nil {
				t.Fatalf("seed embedding: %v", err)
			}
			if err := s.distillWriteBlock(ctx, opts, st); err != nil {
				t.Fatalf("second write: %v", err)
			}
			var model string
			if err := pool.QueryRow(ctx,
				`SELECT COALESCE(embed_model,'') FROM context_blocks
				  WHERE category='session-insights' AND title=$1 AND scope=$2`,
				distillBlockTitle(dfRoot, 0), dfScope).Scan(&model); err != nil {
				t.Fatalf("read embed_model: %v", err)
			}
			if model != "probe" {
				t.Errorf("embed_model = %q after an identical re-write, want probe — the repetition must be free", model)
			}

			// A real content change DOES invalidate it, which is the other half
			// of the same property.
			st.addBatch(distillExtractResult{insights: []distillKept{
				{claim: "Der Retrieval-Pfad faltet vier Arme per Reciprocal Rank Fusion zusammen.",
					quote: "harmloser Zitattext", blockID: a8Block2, chunk: 2},
			}}, distillLedger{}, 20, nil)
			if err := s.distillWriteBlock(ctx, opts, st); err != nil {
				t.Fatalf("third write: %v", err)
			}
			if err := pool.QueryRow(ctx,
				`SELECT COALESCE(embed_model,'') FROM context_blocks
				  WHERE category='session-insights' AND title=$1 AND scope=$2`,
				distillBlockTitle(dfRoot, 0), dfScope).Scan(&model); err != nil {
				t.Fatalf("read embed_model: %v", err)
			}
			if model != "" {
				t.Errorf("embed_model = %q after a content change, want cleared", model)
			}
		})
	})

	// GATE 10 — the write order, with a discriminator this time (round-2 major
	// #2). Round 1 aborted the run through a READ error, i.e. before the write
	// step, so the reviewer's mutation — ledger and watermark BEFORE the write —
	// left the probe green. The barrier aborts between the durable write and the
	// ledger instead, which is the state a SIGTERM produces and the only one in
	// which the order is observable.
	t.Run("write order: the block is durable before the watermark moves", func(t *testing.T) {
		a9Truncate(t, pool)

		fired := 0
		distillWriteBarrier = func(context.Context) error {
			fired++
			return errors.New("probe: killed between the block write and the ledger")
		}
		a9Run(t, pool, a9Source(3, 2, 0))
		distillWriteBarrier = func(context.Context) error { return nil }

		if fired == 0 {
			t.Fatal("the barrier never fired — the probe measures nothing")
		}
		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d blocks after the abort, want 1 — the write must stand BEFORE the ledger", len(blocks))
		}
		first := blocks[0].content
		if n := strings.Count(first, "Beleg "); n < 2 {
			t.Fatalf("the block carries %d claims — batch 1's insights are not durable", n)
		}
		var wmTo int64
		var outcome string
		if err := pool.QueryRow(ctx, `
			SELECT watermark_to, outcome FROM distill_run
			 WHERE source_key = $1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&wmTo, &outcome); err != nil {
			t.Fatalf("read journal: %v", err)
		}
		if wmTo != 0 {
			t.Errorf("watermark_to = %d, want 0 — the abort came before the watermark could move", wmTo)
		}

		// The restart over the SAME range: the durable insights survive it and
		// nothing is written twice.
		before := strings.Count(first, "Beleg 1-100")
		if before == 0 {
			t.Fatal("batch 1's claim is not in the block at all — the duplicate probe would be vacuous")
		}
		a8ClearWindow(t, pool)
		a9Run(t, pool, a9Source(3, 2, 0))
		total := 0
		for _, b := range a9Blocks(t, pool) {
			total += strings.Count(b.content, "Beleg 1-100")
		}
		if total != before {
			t.Errorf("the claim of batch 1 appears %d times after the restart, want %d", total, before)
		}
	})

	// BLOCKER #1 — the yield of a braked run survives the follow-up run over the
	// SAME identity. Measured red before the fix: run 1 held five insights
	// (partial/budget, watermark_to=0), run 2 replaced all five.
	t.Run("a braked run's insights survive the follow-up run", func(t *testing.T) {
		a9Truncate(t, pool)

		// The call clamp brakes INSIDE the first batch: ten chunks at
		// rows_per_call 5 are two call groups against a budget of one.
		braked := a8Config()
		braked.Distill.SpendMaxCalls = 1
		stub := a8NewStub(t, a9Stub)
		s := a8Scheduler(pool, braked, a9Source(1, 10, 0), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("run 1: %d blocks, want 1", len(blocks))
		}
		var carried []string
		for _, ln := range strings.Split(blocks[0].content, "\n") {
			if strings.HasPrefix(ln, "- **") {
				carried = append(carried, ln)
			}
		}
		if len(carried) == 0 {
			t.Fatal("the braked run held no insight — the probe would be vacuous")
		}
		var wmTo int64
		var outcome, skip string
		if err := pool.QueryRow(ctx, `
			SELECT watermark_to, outcome, COALESCE(skip_reason,'') FROM distill_run
			 WHERE source_key=$1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&wmTo, &outcome, &skip); err != nil {
			t.Fatalf("journal: %v", err)
		}
		if outcome != distillOutcomePartial || skip != distillSkipBudget || wmTo != 0 {
			t.Fatalf("run 1 = %s/%s watermark_to=%d, want partial/budget with a standing watermark",
				outcome, skip, wmTo)
		}

		// Run 2 sees the SAME watermark_from and therefore the same title. Its
		// chunks 0-4 are duplicates; 5-9 produce new insights.
		a8ClearWindow(t, pool)
		stub2 := a8NewStub(t, a9Stub)
		s2 := a8Scheduler(pool, a8Config(), a9Source(1, 10, 0), a8Pool(stub2.srv.URL))
		s2.distillOnce(ctx, dfNoDemand)

		blocks = a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("run 2: %d blocks, want 1 — the identity is unchanged", len(blocks))
		}
		second := blocks[0].content
		for _, ln := range carried {
			if !strings.Contains(second, ln) {
				t.Errorf("the block LOST an insight of the braked run: %q", ln)
			}
			if strings.Count(second, ln) != 1 {
				t.Errorf("the carried insight was written twice: %q", ln)
			}
		}
		if strings.Count(second, "- **") <= len(carried) {
			t.Errorf("run 2 added nothing: %d claim lines against %d carried",
				strings.Count(second, "- **"), len(carried))
		}
		if b := blocks[0]; b.insightCount != strings.Count(second, "- **") {
			t.Errorf("insight_count = %d, block carries %d claims", b.insightCount, strings.Count(second, "- **"))
		}
	})

	// MAJOR #3 — the concatenation restklasse, measured instead of assumed.
	//
	// The class is real IN PRINCIPLE: the arm scans each insight, the store
	// scans the finished CONTENT, and a rule that fires on a span crossing a
	// rendered separator would be seen only by the second — where Detector wins
	// over Derived, so the row would end `pattern` with the material in the
	// corpus. The question is whether this FORMAT can produce such a span.
	//
	// The battery below is the attempt, one construction per span rule, and it
	// asserts the invariant that matters rather than a guess: whenever no
	// insight scans, the rendered content does not scan either. A construction
	// that breaks it is a real hole and this test is where it becomes visible.
	t.Run("a span the per-insight scan misses does not exist in this format", func(t *testing.T) {
		hex32 := strings.Repeat("ab12cd34", 4)       // 32 hex, below reHexBlob's floor
		b64 := strings.Repeat("Zm9vYmFy", 3)         // 24 base64 chars, below the 32 floor
		entropy := "Xq7ZLm2Pv9Tk4Rw8Nb3Yc6Hd1Gf5Js0" // a value that passes the entropy gate

		for _, tc := range []struct {
			name      string
			a, b      distillKept
			sameClaim bool
		}{
			{"hex halves across two claims",
				distillKept{claim: "Der Anhang endet auf " + hex32, quote: "kurzes Zitat aus dem Transkript", blockID: a8Block1, chunk: 1},
				distillKept{claim: hex32 + " eroeffnet die naechste Aussage.", quote: "kurzes Zitat aus dem Transkript", blockID: a8Block2, chunk: 2}, false},
			{"base64 halves across two claims",
				distillKept{claim: "Der Anhang endet auf " + b64, quote: "kurzes Zitat aus dem Transkript", blockID: a8Block1, chunk: 1},
				distillKept{claim: b64 + " eroeffnet die naechste Aussage.", quote: "kurzes Zitat aus dem Transkript", blockID: a8Block2, chunk: 2}, false},
			{"assignment key and value across two claims",
				distillKept{claim: "Die Konfiguration nennt password:", quote: "kurzes Zitat aus dem Transkript", blockID: a8Block1, chunk: 1},
				distillKept{claim: entropy + " steht danach.", quote: "kurzes Zitat aus dem Transkript", blockID: a8Block2, chunk: 2}, false},
			{"label in the claim, hex halves in claim and quote",
				distillKept{claim: "Der Anhang traegt sha256: " + hex32, quote: hex32 + " und mehr Text aus dem Transkript", blockID: a8Block1, chunk: 1},
				distillKept{}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a9Truncate(t, pool)
				st := newDistillBlockState(dfRoot, a9Man1, 0)
				st.newest = a9ManifestN(1)
				ins := []distillKept{tc.a}
				if !tc.sameClaim {
					ins = append(ins, tc.b)
				}
				st.addBatch(distillExtractResult{insights: ins}, distillLedger{}, 10, nil)
				if st.redacted != 0 {
					t.Skipf("the per-insight scan caught the construction (redacted=%d) — "+
						"the class is closed for this shape, which is the outcome the invariant wants",
						st.redacted)
				}

				stub := a8NewStub(t, a9Stub)
				s := a8Scheduler(pool, a8Config(), a9Source(1, 1, 0), a8Pool(stub.srv.URL))
				opts := distillWriteOpts{
					category: "session-insights", scope: dfScope, typeName: "insight",
					sensitivity: backends.SensCredentials, maxRunes: 6000, sourceLabel: dfLabel,
				}
				if err := s.distillWriteBlock(ctx, opts, st); err != nil {
					t.Fatalf("write: %v", err)
				}
				b := a9Blocks(t, pool)[0]
				if b.sensSource != "derived" {
					t.Errorf("sensitivity_source = %q with no per-insight hit — the CONTENT scan found "+
						"a span the arm did not, and the block lost its derived provenance "+
						"(detector kind %q). This is the restklasse becoming real: it needs a "+
						"discard on content level, not only on insight level.", b.sensSource, b.detectorKind)
				}
			})
		}
	})

	// MAJOR #4 — the cap must not be paid for twice.
	//
	// Two halves, and they are reachable in different states. A cap strike on a
	// batch that COMPLETED ends the run but still moves the watermark, so the
	// next run writes a different block and starts empty — that is the harmless
	// case. The expensive one is a run that is BOTH braked (watermark stands)
	// and full: without the pre-run gate its successor would open a run, pay for
	// calls and drop every insight they produce.
	t.Run("a full block is not paid for twice", func(t *testing.T) {
		// THE CAP IS CALIBRATED, not guessed: a first run under the default cap
		// measures what this fixture's block actually costs, and the real cap is
		// then "frame + note + exactly one insight". A literal would stop
		// measuring the gate the day the block format changes by twenty runes,
		// and a cap derived from an EMPTY probe state would be wrong by the
		// length of the provenance line, which depends on the run's own values.
		a9Truncate(t, pool)
		calib := a8Config()
		calib.Distill.SpendMaxCalls = 1
		stubC := a8NewStub(t, a9Stub)
		a8Scheduler(pool, calib, a9Source(1, 10, 0), a8Pool(stubC.srv.URL)).distillOnce(ctx, dfNoDemand)
		cb := a9Blocks(t, pool)
		if len(cb) != 1 {
			t.Fatalf("calibration run left %d blocks", len(cb))
		}
		nIns := strings.Count(cb[0].content, "- **")
		if nIns == 0 {
			t.Fatal("the calibration run held no insight")
		}
		c, e := distillInsightLine(distillKept{
			claim: a8Claim + " Beleg 1-100", quote: a8Quote, blockID: a8Block1, chunk: 100,
		})
		pair := len([]rune(c)) + len([]rune(e))
		frame := len([]rune(cb[0].content)) - nIns*pair

		tight := a8Config()
		tight.Distill.MaxBlockRunes = frame + len([]rune(distillOverflowNote(nIns))) + pair
		// A clamp that brakes inside the first batch, so the watermark stays put
		// and the follow-up run meets the SAME identity.
		tight.Distill.SpendMaxCalls = 1
		t.Logf("calibrated: insights=%d pair=%d frame=%d cap=%d",
			nIns, pair, frame, tight.Distill.MaxBlockRunes)

		a9Truncate(t, pool)
		stub := a8NewStub(t, a9Stub)
		s := a8Scheduler(pool, tight, a9Source(1, 10, 0), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		var wmTo int64
		var outcome, skip string
		if err := pool.QueryRow(ctx, `
			SELECT watermark_to, outcome, COALESCE(skip_reason,'') FROM distill_run
			 WHERE source_key=$1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&wmTo, &outcome, &skip); err != nil {
			t.Fatalf("journal: %v", err)
		}
		if outcome != distillOutcomePartial || skip != distillSkipBudget || wmTo != 0 {
			t.Fatalf("run 1 = %s/%s watermark_to=%d, want partial/budget with a standing watermark",
				outcome, skip, wmTo)
		}
		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("run 1 left %d blocks, want 1", len(blocks))
		}
		if n := strings.Count(blocks[0].content, "- **"); n == 0 {
			t.Fatalf("run 1 left an empty block (%d runes, cap %d) — the cap has no room for a single "+
				"insight, so the probe would measure the frame instead of the gate",
				len([]rune(blocks[0].content)), tight.Distill.MaxBlockRunes)
		}
		var overBudget int
		if err := pool.QueryRow(ctx,
			`SELECT (metadata->'coverage'->>'insights_over_budget')::int FROM context_blocks WHERE id=$1`,
			blocks[0].id).Scan(&overBudget); err != nil {
			t.Fatalf("read coverage: %v", err)
		}
		if overBudget == 0 {
			t.Fatalf("the cap never struck (over_budget=0) — the probe measures the clamp, not the cap")
		}

		// The FOLLOW-UP over the same identity buys nothing: every insight it
		// could produce would be dropped by the same cap.
		a8ClearWindow(t, pool)
		before := len(a8Rows(t, pool))
		stub2 := a8NewStub(t, a9Stub)
		s2 := a8Scheduler(pool, tight, a9Source(1, 10, 0), a8Pool(stub2.srv.URL))
		s2.distillOnce(ctx, dfNoDemand)
		if got := len(a8Rows(t, pool)); got != before {
			t.Errorf("%d llm-log rows after the follow-up run, want %d — a full block must buy nothing",
				got, before)
		}
		var skipped int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM distill_run
			 WHERE source_key=$1 AND outcome='skipped' AND skip_reason='budget'`, key).Scan(&skipped); err != nil {
			t.Fatalf("journal: %v", err)
		}
		if skipped == 0 {
			t.Error("the follow-up run left no skipped/budget row — the state is invisible to an operator")
		}
		// And the state is THROTTLED: a third tick adds no second row.
		a8ClearWindow(t, pool)
		stub3 := a8NewStub(t, a9Stub)
		s3 := a8Scheduler(pool, tight, a9Source(1, 10, 0), a8Pool(stub3.srv.URL))
		s3.distillOnce(ctx, dfNoDemand)
		var skipped2 int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM distill_run
			 WHERE source_key=$1 AND outcome='skipped' AND skip_reason='budget'`, key).Scan(&skipped2); err != nil {
			t.Fatalf("journal: %v", err)
		}
		if skipped2 != skipped {
			t.Errorf("a standing full block writes one row per tick (%d → %d) — the state-change rule "+
				"is what keeps a permanent state from burying the journal", skipped, skipped2)
		}
	})

	// GATE 12 (R2-2): a source without corpus ids writes no insight and says so.
	t.Run("R2-2: an unanchored insight is counted, never written", func(t *testing.T) {
		a9Truncate(t, pool)
		src := a9Source(1, 2, 0)
		inner := src.readFn
		src.readFn = func(after int64) (distillsource.Batch, error) {
			b, err := inner(after)
			for i := range b.Items {
				b.Items[i].Origin.BlockID = ""
			}
			return b, err
		}
		a9Run(t, pool, src)

		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d blocks, want 1", len(blocks))
		}
		if blocks[0].insightCount != 0 {
			t.Errorf("insight_count = %d, want 0 — an insight without a corpus id cannot be cited",
				blocks[0].insightCount)
		}
		var unanchored int
		if err := pool.QueryRow(ctx, `
			SELECT (metadata->'coverage'->>'insights_unanchored')::int
			  FROM context_blocks WHERE id = $1`, blocks[0].id).Scan(&unanchored); err != nil {
			t.Fatalf("read coverage: %v", err)
		}
		if unanchored < 1 {
			t.Errorf("insights_unanchored = %d, want at least one — the loss must be a number", unanchored)
		}
	})

	// THE CADENCE DECISION, MEASURED. Writing per batch is only free because
	// the insight type is retrieval 'excluded': both embed backfill picks AND-in
	// store.RetrievalExcludedTypePredicate, so the block never takes an embed
	// slot and there is no invalidate/re-embed cascade to pay for. If E-4 flips
	// this type to visible, the number stops being zero.
	t.Run("cadence: the embed cascade of a per-batch write is zero", func(t *testing.T) {
		a9Truncate(t, pool)
		a9Run(t, pool, a9Source(3, 2, 0))
		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d blocks, want 1", len(blocks))
		}
		var pending int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks
			  WHERE id = $1 AND embedding IS NULL`+store.RetrievalExcludedTypePredicate,
			blocks[0].id).Scan(&pending); err != nil {
			t.Fatalf("backfill predicate: %v", err)
		}
		if pending != 0 {
			t.Errorf("the block is embed-backfill eligible (%d) — three batch writes would be three re-embeds", pending)
		}
		var embedded bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1`, blocks[0].id).Scan(&embedded); err != nil {
			t.Fatalf("read embedding: %v", err)
		}
		if embedded {
			t.Error("the block carries an embedding although its type is retrieval-excluded")
		}
	})

	// The reader's half of the wave: the manifest provenance really arrives
	// from the DATABASE, not only from a fake source.
	t.Run("the ctx checkpoint reader carries the manifest provenance", func(t *testing.T) {
		a9Truncate(t, pool)
		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE type_name = 'checkpoint'`); err != nil {
			t.Fatalf("clear checkpoints: %v", err)
		}
		root := "20260712_205012_reader"
		wm, manifestID, partID := dfSeedCheckpoint(t, pool, root, time.Now().UTC())
		if _, err := pool.Exec(ctx, `
			UPDATE context_blocks
			   SET metadata = metadata || jsonb_build_object(
			         'sha256', $2::text, 'parent_manifest_id', $3::text, 'active_session_id', $4::text)
			 WHERE id = $1::uuid`, manifestID, a9SHA, a9Man1, "20260712_205012_active"); err != nil {
			t.Fatalf("stamp manifest provenance: %v", err)
		}
		src, err := ctxcheckpoint.New(pool, ctxcheckpoint.Options{
			Label: dfLabel, Scope: dfScope, Category: dfCategory,
			MaxSessions: 4, MaxManifests: 10,
		})
		if err != nil {
			t.Fatalf("build reader: %v", err)
		}
		batch, err := src.Read(ctx, root, 0, 400, 4000)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(batch.Items) == 0 {
			t.Fatalf("the reader delivered nothing for %s (wm %d, part %s)", root, wm, partID)
		}
		m := batch.Items[0].Manifest
		if m.ID != manifestID || m.SHA256 != a9SHA || m.ParentID != a9Man1 ||
			m.ActiveSessionID != "20260712_205012_active" {
			t.Errorf("manifest provenance = %+v, want the four values of the manifest row", m)
		}
	})
}

// Below: wave C3-1, part B — N-3, the cap that discards two thirds of the paid
// yield (pilot report §10 N-3).

// c31Calibrate runs one clamped tick and reports what THIS fixture's block
// costs: the rune length of one insight's two rendered lines, and the frame
// everything else needs.
//
// MEASURED AND NOT GUESSED, for the reason the neighbouring major-#4 probe
// gives: a literal cap stops measuring the gate the day the block format
// changes by twenty runes, and a frame derived from an EMPTY probe state is
// wrong by the length of the provenance line, which depends on the run's own
// counters.
func c31Calibrate(t *testing.T, pool *pgxpool.Pool) (pair, frame, note int) {
	t.Helper()
	a9Truncate(t, pool)
	calib := a8Config()
	calib.Distill.RowsPerCall = 1
	calib.Distill.SpendMaxCalls = 1
	stub := a8NewStub(t, a9Stub)
	a8Scheduler(pool, calib, a9Source(1, c31Chunks, 0), a8Pool(stub.srv.URL)).
		distillOnce(context.Background(), dfNoDemand)

	blocks := a9Blocks(t, pool)
	if len(blocks) != 1 {
		t.Fatalf("calibration run left %d blocks, want 1", len(blocks))
	}
	nIns := strings.Count(blocks[0].content, "- **")
	if nIns != 1 {
		t.Fatalf("the calibration run held %d insights, want exactly 1 (one call, one chunk)", nIns)
	}
	c, e := distillInsightLine(distillKept{
		claim: a8Claim + " Beleg 1-100", quote: a8Quote, blockID: a8Block1, chunk: 100,
	})
	pair = len([]rune(c)) + len([]rune(e))
	frame = len([]rune(blocks[0].content)) - nIns*pair
	note = len([]rune(distillOverflowNote(nIns)))
	return pair, frame, note
}

// c31Chunks is how many chunks the fixture batch carries. With rows_per_call =
// 1 that is exactly how many calls the UNSTEERED arm makes — the number the
// probe below reads as its red state.
const c31Chunks = 6

// c31Run is one tick plus everything the probes below read back from it.
type c31Run struct {
	calls, kept, held, over, stops int
	outcome, skip                  string
	wmTo                           int64
	blockID                        string
}

// c31Tick drives one tick and collects journal and block state.
func c31Tick(t *testing.T, pool *pgxpool.Pool, cfg *config.Config, src distillsource.Source) c31Run {
	t.Helper()
	ctx := context.Background()
	key := distillSourceKey(dfLabel, dfScope, dfRoot)
	stub := a8NewStub(t, a9Stub)
	a8Scheduler(pool, cfg, src, a8Pool(stub.srv.URL)).distillOnce(ctx, dfNoDemand)

	var r c31Run
	r.calls, r.kept, _, r.outcome = a8Ledger(t, pool, key)
	blocks := a9Blocks(t, pool)
	if len(blocks) != 1 {
		t.Fatalf("%d blocks, want 1", len(blocks))
	}
	r.blockID = blocks[0].id
	r.held = strings.Count(blocks[0].content, "- **")
	if err := pool.QueryRow(ctx, `
		SELECT (metadata->'coverage'->>'insights_over_budget')::int,
		       COALESCE((metadata->'coverage'->>'calls_stopped_block_full')::int, -1)
		  FROM context_blocks WHERE id = $1`, blocks[0].id).Scan(&r.over, &r.stops); err != nil {
		t.Fatalf("read coverage: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT watermark_to, COALESCE(skip_reason,'') FROM distill_run
		 WHERE source_key=$1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&r.wmTo, &r.skip); err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Logf("calls=%d kept=%d held=%d over_budget=%d stops=%d outcome=%s skip=%s wm=%d",
		r.calls, r.kept, r.held, r.over, r.stops, r.outcome, r.skip, r.wmTo)
	return r
}

// TestDistillRuneBudget is part B's gate.
//
// THE RED STATE, measured in the X-W4 pilot and reproduced here: the cap is
// enforced at RENDER time, i.e. after every call of the batch has been paid
// for. Four of sixteen pilot blocks stood at 5 271–5 934 runes against a cap of
// 6 000, coverage.insights_over_budget was 106 against 69 insights actually in
// the blocks, and the arm paid 24,70 GPU-s per PUBLISHED insight instead of the
// 5,41 it paid per gate-kept one.
//
// The steering is a rune meter in the call loop, the same shape the GPU meter
// and the call clamp already have (distill_extract.go): it answers "does the
// block still have room for one more insight" BEFORE the next call is made, and
// ends the tick with the journal's own `budget` word when it does not.
//
//	go test -tags=integration ./internal/events/ -run TestDistillRuneBudget -count=1 -v
func TestDistillRuneBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	pair, frame, note := c31Calibrate(t, pool)
	t.Logf("calibrated: pair=%d frame=%d note=%d chunks=%d", pair, frame, note, c31Chunks)

	// ONE CALL PER CHUNK — the shape that isolates the BRAKE: every call is a
	// decision point, so the loop must stop at the exact insight the cap can no
	// longer hold. Room for two insights plus half of a third.
	t.Run("rows_per_call=1: the brake stops the call loop", func(t *testing.T) {
		cfg := a8Config()
		cfg.Distill.RowsPerCall = 1
		cfg.Distill.MaxBlockRunes = frame + note + 2*pair + pair/2

		a9Truncate(t, pool)
		r := c31Tick(t, pool, cfg, a9Source(1, c31Chunks, 0))

		// (1) THE CALLS. The unsteered arm pays for every chunk of the batch; the
		// steered one stops as soon as the block has no room left.
		if r.calls >= c31Chunks {
			t.Errorf("the arm made %d calls for %d chunks — every call past the cap is paid for "+
				"yield that the render then discards (pilot N-3)", r.calls, c31Chunks)
		}
		if r.calls < 2 {
			t.Errorf("the arm made %d calls — the cap holds two insights, so a brake before the "+
				"second call throws away yield the block could carry", r.calls)
		}
		// (2) THE DISCARDED YIELD. This is the number N-3 is about.
		if r.over != 0 {
			t.Errorf("insights_over_budget = %d, want 0 — the arm still pays for insights the "+
				"render discards", r.over)
		}
		// (3) THE BLOCK IS ACTUALLY FILLED. A brake that stops before the first
		// call would satisfy (1) and (2) while producing nothing at all.
		if r.held == 0 {
			t.Fatal("the block holds no insight — the probe measures a brake, not a steering")
		}
		// (4) NOT A SILENT SKIP.
		if r.outcome != distillOutcomePartial {
			t.Errorf("outcome = %q, want partial — a run the cap ended did not cover its range", r.outcome)
		}
		if r.skip != distillSkipBudget {
			t.Errorf("skip_reason = %q, want %q — the brake is invisible to an operator",
				r.skip, distillSkipBudget)
		}
		if r.stops < 1 {
			t.Errorf("coverage.calls_stopped_block_full = %d, want at least 1 — the block does not "+
				"say that its own cap stopped the run", r.stops)
		}
		// (5) THE MATERIAL IS POSTPONED, NOT COVERED.
		if r.wmTo != 0 {
			t.Errorf("watermark_to = %d, want 0 — the unshown remainder of the batch must stay readable", r.wmTo)
		}
	})

	// THE PRODUCTION SHAPE (round 2, review major #2). distill.rows_per_call
	// defaults to 5 (internal/config/config.go:1842) and a8Config() — the base of
	// every other probe in this package — sets 5 too. A gate that only ever runs
	// at 1 measures a configuration nobody deploys.
	//
	// It is also the PILOT's shape rather than an empty block with a tiny cap:
	// X-W4 measured N-3 on blocks that already CARRIED material and stood at
	// 5 271–5 934 runes against 6 000. A carried block is exactly the state in
	// which the meter has a real size estimate — the mean of the insights the
	// block already holds — so the call planner can size the group before the
	// first call of the run instead of after it.
	t.Run("production rows_per_call over a block that carries material", func(t *testing.T) {
		// Run 1 seeds the identity with one insight and leaves the watermark
		// standing (spend clamp inside the first batch), so run 2 meets the same
		// title and reads it back as carry.
		seed := a8Config()
		seed.Distill.RowsPerCall = 1
		seed.Distill.SpendMaxCalls = 1
		a9Truncate(t, pool)
		first := c31Tick(t, pool, seed, a9Source(1, c31Chunks, 0))
		if first.held != 1 || first.wmTo != 0 {
			t.Fatalf("seed run held %d insights at watermark %d, want 1/0 — run 2 would not meet "+
				"the same identity", first.held, first.wmTo)
		}

		// Run 2 at the PRODUCTION call size, with room for two more insights.
		cfg := a8Config()
		if cfg.Distill.RowsPerCall != config.Defaults().Distill.RowsPerCall {
			t.Fatalf("a8Config rows_per_call = %d, production default = %d — the probe would not "+
				"measure the deployed shape", cfg.Distill.RowsPerCall, config.Defaults().Distill.RowsPerCall)
		}
		cfg.Distill.MaxBlockRunes = frame + note + 3*pair + pair/2
		r := c31Tick(t, pool, cfg, a9Source(1, c31Chunks, 0))

		if r.over != 0 {
			t.Errorf("insights_over_budget = %d at rows_per_call=%d, want 0 — the call planner did "+
				"not size the group to the room the block has left", r.over, cfg.Distill.RowsPerCall)
		}
		if r.kept > 2 {
			t.Errorf("the call bought %d insights although only two fit — the group was not "+
				"sized to the remaining room", r.kept)
		}
		if r.held < 3 {
			t.Errorf("the block holds %d insights (1 carried + %d new), want 3 — the planner "+
				"under-filled the block it was steering", r.held, r.kept)
		}
		if r.calls != 1 {
			t.Errorf("calls = %d, want 1 — one sized group fills the block, and the brake ends "+
				"the loop before a second one", r.calls)
		}
		if r.stops < 1 {
			t.Errorf("coverage.calls_stopped_block_full = %d, want at least 1", r.stops)
		}
		if r.wmTo != 0 {
			t.Errorf("watermark_to = %d, want 0", r.wmTo)
		}
	})

	// THE DOCUMENTED LIMIT (round 2, review major #2). The FIRST call of a run
	// over an EMPTY block is structurally blind: there is no insight of this
	// block to take a size from, so distillNextInsightRunes falls back to the
	// theoretical minimum (its own doc says so) and the planner cannot size the
	// group below rows_per_call. Whatever that one call buys above the cap is the
	// irreducible remainder of this design — an atomic call cannot be cut in half.
	//
	// It is pinned as a NUMBER rather than described, so a regression that makes
	// it worse is visible: at most one call's worth, never more.
	t.Run("the blind first call over an empty block is the documented limit", func(t *testing.T) {
		cfg := a8Config()
		cfg.Distill.MaxBlockRunes = frame + note + 2*pair + pair/2

		a9Truncate(t, pool)
		r := c31Tick(t, pool, cfg, a9Source(1, c31Chunks, 0))

		if r.calls != 1 {
			t.Errorf("calls = %d, want 1 — after the blind first call the meter has a measurement "+
				"and must stop", r.calls)
		}
		if r.over > cfg.Distill.RowsPerCall-r.held {
			t.Errorf("insights_over_budget = %d, want at most %d — the loss must stay inside the "+
				"ONE call that had no size estimate", r.over, cfg.Distill.RowsPerCall-r.held)
		}
		if r.over == 0 {
			t.Logf("no overshoot at all in this fixture — the limit is an upper bound, not a floor")
		}
		// The unsteered arm made TWO calls here and discarded four insights
		// (reviewer measurement at the same cap). One call is the steering.
		if r.held < 2 {
			t.Errorf("the block holds %d insights, want the two the cap can carry", r.held)
		}
	})
}
