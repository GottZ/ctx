//go:build integration

// Wissens-Ebenen V-W8 (design/05 §7 row V-W8, §5 B3): the G40 credentials
// detector runs inside store.UpsertBlock, so EVERY upsert path carries it —
// not just the two handler entry points (context_store.go:107,
// stage_gates.go:81).
//
// These probes drive UpsertBlock with the EXACT argument shapes of the four
// in-process writers that never saw the handler detector:
//
//	digest/digest.go:146, :267        store.SensitivityWrite{}
//	dream/synthesize_report.go:334    store.SensitivityWrite{}
//	handler/ingest.go:223             store.SensitivityWrite{}   (block mode)
//	rootmap/run.go:190                store.SensitivityWrite{Value: SensInternal}
//
// Subtests:
//
//	a_InProcessWriterRaised   — SensitivityWrite{} + key pattern ⇒ credentials/pattern/trace.
//	                            RED: 'default' source, no trace (DDL default masks the LEVEL).
//	b_RootmapShapeRaised      — {Value: internal} + key pattern ⇒ credentials/pattern.
//	                            RED: the block stays 'internal'/'default' — the LEVEL move.
//	c_DoubleApplicationIdempotent — pre-applied verdict (handler) vs store-only: identical row.
//	d_CleanContentUnchanged   — all four shapes, pattern-free ⇒ pre-wave row, insert AND conflict.
//	e_ManualWithPattern       — Manual=true + pattern: insert 'pattern', conflict leaves 'manual'.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestWriteDetectorInUpsert -count=1 -v
package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/sensitivity"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// vw8Cred carries an AWS access key id shape (AKIA + 16 base32 chars) —
// sensitivity.Scan rule "aws-key", the FIRST-matching structured rule, so the
// expected Kind is stable regardless of the surrounding prose. Synthetic: the
// 16 payload chars are a constant run, never a real credential.
var vw8Cred = "rotation note: AKIA" + strings.Repeat("Z", 16) + " showed up in a paste"

// vw8Clean has no structured signal and sits far below every entropy gate.
const vw8Clean = "an ordinary sentence with nothing sensitive in it at all"

// vw8Row reads the write-path-relevant columns of one block.
func vw8Row(t *testing.T, pool *pgxpool.Pool, title string) (sens, source string, md map[string]any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT sensitivity, sensitivity_source, metadata
		   FROM context_blocks WHERE title = $1 AND NOT is_archived`,
		title).Scan(&sens, &source, &md); err != nil {
		t.Fatalf("read block %q: %v", title, err)
	}
	return
}

// vw8Trace is the verdict shape both the write path and the SQL sweep
// (store/sensitivity.go:269-270 jsonb_build_object) must produce — kind and
// reason, never the matched secret.
func vw8Trace(t *testing.T, md map[string]any, title string) map[string]any {
	t.Helper()
	trace, ok := md["sensitivity_detector"].(map[string]any)
	if !ok {
		t.Fatalf("%s: metadata.sensitivity_detector missing, got metadata %v", title, md)
	}
	return trace
}

func TestWriteDetectorInUpsert_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("a_InProcessWriterRaised", func(t *testing.T) {
		// The digest/dream/ingest argument shape: SensitivityWrite{} = "no
		// explicit intent". RED before V-W8: UpsertBlock wrote no sensitivity
		// column at all, so the row took the DDL defaults
		// ('credentials'/'default', 113_baseline.sql:5474-5477) — the LEVEL was
		// right by accident, the PROVENANCE and the trace were absent.
		const title = "vw8-inprocess-key"
		if _, err := store.UpsertBlock(ctx, pool, "index", title, vw8Cred,
			[]string{"index"}, map[string]any{"source": "context-digest"},
			"private", true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		sens, source, md := vw8Row(t, pool, title)
		if sens != "credentials" {
			t.Errorf("sensitivity = %q, want credentials", sens)
		}
		if source != "pattern" {
			t.Errorf("sensitivity_source = %q, want pattern (the detector decided this write)", source)
		}
		trace := vw8Trace(t, md, title)
		if trace["kind"] != "aws-key" || trace["reason"] != "AWS access key id pattern" {
			t.Errorf("trace = %v, want kind=aws-key reason=\"AWS access key id pattern\"", trace)
		}
		if md["source"] != "context-digest" {
			t.Errorf("caller metadata lost: %v", md)
		}
	})

	t.Run("b_RootmapShapeRaised", func(t *testing.T) {
		// rootmap/run.go:190 writes a hard 'internal' — the §5 B3 case: the
		// map's "labels, counts and IDs, no block content" reasoning stops
		// holding the moment quoted raw text lands in the block. RED before
		// V-W8: internal/default, i.e. a genuine LEVEL move, not just a
		// provenance one.
		const title = "vw8-rootmap-key"
		if _, err := store.UpsertBlock(ctx, pool, "index", title, vw8Cred,
			[]string{"index", "root-map"}, map[string]any{"is_meta": true},
			"private", true, store.SensitivityWrite{Value: backends.SensInternal}, ""); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		sens, source, md := vw8Row(t, pool, title)
		if sens != "credentials" || source != "pattern" {
			t.Errorf("root-map row = %s/%s, want credentials/pattern", sens, source)
		}
		if trace := vw8Trace(t, md, title); trace["kind"] != "aws-key" {
			t.Errorf("trace kind = %v, want aws-key", trace["kind"])
		}
	})

	t.Run("c_DoubleApplicationIdempotent", func(t *testing.T) {
		// The handler keeps its own applyWriteDetector call (the staged path
		// pins the verdict into the hash-bound canonical payload,
		// store/confirm_payload.go:47-51), so on the REST/MCP paths the
		// detector runs TWICE. Belegt, nicht angenommen: a pre-applied verdict
		// and a store-only write must land byte-identical rows.
		const pre, only = "vw8-double-pre", "vw8-double-only"

		// The pre-applied verdict is built here from sensitivity.Scan the way
		// handler/context_store.go:239-245 builds it — an INDEPENDENT oracle,
		// not a call into the code under test.
		m, hit := sensitivity.Scan(vw8Cred)
		if !hit {
			t.Fatal("sensitivity.Scan reported no hit on the key fixture")
		}
		preSens := store.SensitivityWrite{Value: backends.SensCredentials, Detector: true}
		preMD := map[string]any{
			"origin":               "handler",
			"sensitivity_detector": map[string]any{"kind": m.Kind, "reason": m.Reason},
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", pre, vw8Cred, nil, preMD,
			"private", false, preSens, ""); err != nil {
			t.Fatalf("upsert pre-applied: %v", err)
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", only, vw8Cred, nil, map[string]any{"origin": "handler"},
			"private", false, store.SensitivityWrite{Value: backends.SensPublic}, ""); err != nil {
			t.Fatalf("upsert store-only: %v", err)
		}

		preS, preSrc, preRow := vw8Row(t, pool, pre)
		onlyS, onlySrc, onlyRow := vw8Row(t, pool, only)
		if preS != onlyS || preSrc != onlySrc {
			t.Errorf("double application diverges: pre-applied %s/%s vs store-only %s/%s",
				preS, preSrc, onlyS, onlySrc)
		}
		if !reflect.DeepEqual(preRow, onlyRow) {
			t.Errorf("metadata diverges:\n pre-applied %v\n store-only  %v", preRow, onlyRow)
		}
		// The trace is a single JSON key by construction — a second
		// application overwrites, it cannot append.
		if n := len(preRow); n != 2 {
			t.Errorf("metadata carries %d keys (%v), want exactly origin + sensitivity_detector", n, preRow)
		}
		if preSrc != "pattern" {
			t.Errorf("sensitivity_source = %q, want pattern", preSrc)
		}
	})

	t.Run("d_CleanContentUnchanged", func(t *testing.T) {
		// Nicht-Regression: pattern-free content through all four in-process
		// argument shapes keeps the pre-wave row — the values below were read
		// off the UNCHANGED binary, not recomputed from the new code.
		cases := []struct {
			name       string
			sens       store.SensitivityWrite
			wantSens   string
			wantSource string
		}{
			{"digest", store.SensitivityWrite{}, "credentials", "default"},
			{"dream", store.SensitivityWrite{}, "credentials", "default"},
			{"ingest", store.SensitivityWrite{}, "credentials", "default"},
			{"rootmap", store.SensitivityWrite{Value: backends.SensInternal}, "internal", "default"},
		}
		for _, tc := range cases {
			title := "vw8-clean-" + tc.name
			in := map[string]any{"source": tc.name, "is_meta": true}
			if _, err := store.UpsertBlock(ctx, pool, "index", title, vw8Clean, []string{"index"}, in,
				"private", true, tc.sens, ""); err != nil {
				t.Fatalf("[%s] upsert: %v", tc.name, err)
			}
			sens, source, md := vw8Row(t, pool, title)
			if sens != tc.wantSens || source != tc.wantSource {
				t.Errorf("[%s] insert row = %s/%s, want %s/%s", tc.name, sens, source, tc.wantSens, tc.wantSource)
			}
			if _, present := md["sensitivity_detector"]; present {
				t.Errorf("[%s] clean content grew a detector trace: %v", tc.name, md)
			}
			if !reflect.DeepEqual(md, map[string]any{"source": tc.name, "is_meta": true}) {
				t.Errorf("[%s] metadata not byte-identical: %v", tc.name, md)
			}

			// Conflict branch: a second, content-CHANGING upsert of the same
			// key must move nothing either.
			if _, err := store.UpsertBlock(ctx, pool, "index", title, vw8Clean+" (revised)",
				[]string{"index"}, in, "private", true, tc.sens, ""); err != nil {
				t.Fatalf("[%s] conflict upsert: %v", tc.name, err)
			}
			sens, source, md = vw8Row(t, pool, title)
			if sens != tc.wantSens || source != tc.wantSource {
				t.Errorf("[%s] conflict row = %s/%s, want %s/%s", tc.name, sens, source, tc.wantSens, tc.wantSource)
			}
			if _, present := md["sensitivity_detector"]; present {
				t.Errorf("[%s] conflict grew a detector trace: %v", tc.name, md)
			}
		}
	})

	t.Run("e_ManualWithPattern", func(t *testing.T) {
		// Manual + pattern, pinned at the code (blocks.go:265-293): the
		// detector clears Manual and sets Detector, so the INSERT stamps
		// source='pattern'; on conflict the strict '>' comparison never
		// re-stamps an already-credentials row, so a manual credentials block
		// keeps source='manual'. Identical to what the handler produced before
		// V-W8 — the store-side call must not move it.
		const fresh, existing = "vw8-manual-fresh", "vw8-manual-existing"

		if _, err := store.UpsertBlock(ctx, pool, "learnings", fresh, vw8Cred, nil, nil,
			"private", false, store.SensitivityWrite{Value: backends.SensCredentials, Manual: true}, ""); err != nil {
			t.Fatalf("manual insert: %v", err)
		}
		if sens, source, _ := vw8Row(t, pool, fresh); sens != "credentials" || source != "pattern" {
			t.Errorf("manual insert row = %s/%s, want credentials/pattern", sens, source)
		}

		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity, sensitivity_source)
			 VALUES ('learnings', $1, 'seeded manual body', 'private', 'credentials', 'manual')`,
			existing); err != nil {
			t.Fatalf("seed manual block: %v", err)
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", existing, vw8Cred, nil, nil,
			"private", false, store.SensitivityWrite{Value: backends.SensCredentials, Manual: true}, ""); err != nil {
			t.Fatalf("manual conflict: %v", err)
		}
		if sens, source, _ := vw8Row(t, pool, existing); sens != "credentials" || source != "manual" {
			t.Errorf("manual conflict row = %s/%s, want credentials/manual (strict > never re-stamps)", sens, source)
		}
	})
}
