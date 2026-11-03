//go:build integration

// #61: POST /api/ingest in block mode upserted through store.UpsertBlock and
// returned without the auto-classify hook that POST /api/store and the MCP
// store and update paths run after their write. An ingested block kept the
// row default (knowledge, auto) even when its title or metadata matched a
// registry classify rule.
//
// RED without the ClassifyBlockAfterUpsert call in processChunk:
// block_mode_title_rule_classifies reads (knowledge, auto), want
// (audit-trail, auto). type_source alone cannot show the bug — the hook only
// writes type_name and keeps 'auto' — so the probe asserts both columns.
// RED with nil passed as the metadata at the call:
// block_mode_metadata_rule_classifies reads (knowledge, auto), want
// (system-meta, auto). RED with the hook also run after InsertChunk:
// chunk_mode_does_not_classify reads (audit-trail, auto), want
// (knowledge, auto). RED when a classify error fails the chunk:
// block_mode_classify_failure_keeps_write reads success false (the chunk
// status "failed"), want true and "updated".
package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ingestClassifyAuditTitle hits audit-trail's 'session' title pattern and no
// rule of a higher priority (the same fixture shape as
// TestManageUpdateReclassify_Integration).
const ingestClassifyAuditTitle = "Session 61 Handover Protokoll ingest"

func TestIngestBlockModeClassifies(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	set := reg.Snapshot()

	// The production wiring (cmd/ctxd/server.go): the same registry the
	// store handler classifies with.
	h := NewIngestHandler(pool, nil, reg)
	keyCtx, _ := ingestKey(t, pool, "i61-ingest")

	ingestWithMeta := func(t *testing.T, title string, metadata map[string]any) string {
		t.Helper()
		chunk := map[string]any{
			"category": "learnings", "title": title,
			"content": "ingested without a type: " + title,
		}
		if metadata != nil {
			chunk["metadata"] = metadata
		}
		status, resp := postIngest(t, h, keyCtx, map[string]any{"source": "i61", "chunks": []map[string]any{chunk}})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", status, resp)
		}
		r := firstResult(t, resp)
		id, _ := r["id"].(string)
		if r["status"] != "updated" || id == "" {
			t.Fatalf("result = %v, want a block-mode write (status updated, with an id)", r)
		}
		return id
	}
	ingestOne := func(t *testing.T, title string) string {
		t.Helper()
		return ingestWithMeta(t, title, nil)
	}
	typeOf := func(t *testing.T, id string) (name, source string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).Scan(&name, &source); err != nil {
			t.Fatalf("read type of %s: %v", id, err)
		}
		return name, source
	}

	t.Run("block_mode_title_rule_classifies", func(t *testing.T) {
		if name, matched := set.Classify(ingestClassifyAuditTitle, map[string]any{}); !matched || name != "audit-trail" {
			t.Fatalf("fixture: title classifies to (%q, %v), want (audit-trail, true) — otherwise this "+
				"probe cannot tell a classified block from the row default", name, matched)
		}
		id := ingestOne(t, ingestClassifyAuditTitle)
		if name, source := typeOf(t, id); name != "audit-trail" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (audit-trail, auto)", name, source)
		}
	})

	// The classifier reads metadata as well as the title: system-meta has no
	// title pattern, only the metadata flag is_meta (113 baseline seed), so
	// only the metadata branch of set.Classify can produce it. The title hits
	// no rule, which keeps the title branch out of the verdict.
	t.Run("block_mode_metadata_rule_classifies", func(t *testing.T) {
		const title = "i61 metadata-only classified note"
		meta := map[string]any{"is_meta": true}
		if name, matched := set.Classify(title, map[string]any{}); matched {
			t.Fatalf("fixture: title alone classifies to %q, want no match", name)
		}
		if name, matched := set.Classify(title, meta); !matched || name != "system-meta" {
			t.Fatalf("fixture: title+metadata classifies to (%q, %v), want (system-meta, true)", name, matched)
		}
		id := ingestWithMeta(t, title, meta)
		if name, source := typeOf(t, id); name != "system-meta" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (system-meta, auto)", name, source)
		}
	})

	// type_source='manual' keeps its type: a block-mode ingest that upserts
	// onto a manually typed block of the same (category, title, scope) leaves
	// (type_name, type_source) alone even though the title hits audit-trail.
	t.Run("block_mode_manual_type_kept", func(t *testing.T) {
		const title = "Session 61 manual typed ingest target"
		if name, matched := set.Classify(title, map[string]any{}); !matched || name != "audit-trail" {
			t.Fatalf("fixture: title classifies to (%q, %v), want (audit-trail, true)", name, matched)
		}
		seeded, err := store.UpsertBlock(ctx, pool, "learnings", title, "typed by hand",
			nil, nil, "private", true, store.SensitivityWrite{}, "reference")
		if err != nil {
			t.Fatalf("seed manual block: %v", err)
		}
		if name, source := typeOf(t, seeded.ID); name != "reference" || source != "manual" {
			t.Fatalf("fixture: seeded (%q, %q), want (reference, manual)", name, source)
		}
		id := ingestOne(t, title)
		if id != seeded.ID {
			t.Fatalf("ingest wrote %s, want the upsert onto the seeded block %s", id, seeded.ID)
		}
		if name, source := typeOf(t, id); name != "reference" || source != "manual" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (reference, manual)", name, source)
		}
	})

	// Chunk mode does not classify (the decision documented at the hook in
	// processChunk and in docs/api.md): a chunk whose title hits audit-trail
	// keeps the row default. The context_sources row is created out of band,
	// as a chunk-mode client needs it (docs/api.md, the /api/ingest row).
	t.Run("chunk_mode_does_not_classify", func(t *testing.T) {
		const title = "Session 61 Handover chunk ingest"
		if name, matched := set.Classify(title, map[string]any{}); !matched || name != "audit-trail" {
			t.Fatalf("fixture: title classifies to (%q, %v), want (audit-trail, true)", name, matched)
		}
		src := seedSource(t, pool, "/i61/chunk.md", "private")
		status, resp := postIngest(t, h, keyCtx, map[string]any{"source": "i61", "chunks": []map[string]any{{
			"category": "vault", "title": title, "content": "an ingested chunk: " + title,
			"source_id": src, "chunk_index": 0,
		}}})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", status, resp)
		}
		r := firstResult(t, resp)
		id, _ := r["id"].(string)
		if r["status"] != "inserted" || id == "" {
			t.Fatalf("result = %v, want a chunk-mode write (status inserted, with an id)", r)
		}
		if name, source := typeOf(t, id); name != "knowledge" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (knowledge, auto)", name, source)
		}
	})

	// No rule match is a no-op: the block keeps the row default.
	t.Run("block_mode_no_rule_keeps_default", func(t *testing.T) {
		const title = "i61 plain ingested note"
		if name, matched := set.Classify(title, map[string]any{}); matched {
			t.Fatalf("fixture: title classifies to %q, want no match", name)
		}
		id := ingestOne(t, title)
		if name, source := typeOf(t, id); name != "knowledge" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (knowledge, auto)", name, source)
		}
	})

	// The I7 net holds on the new hook site: a title that hits a derived
	// type's classify pattern is not handed that type (store/classify.go).
	t.Run("block_mode_derived_title_not_granted", func(t *testing.T) {
		if name, matched := set.Classify(wlCatalogTitle, map[string]any{}); !matched || name != derived.TypeCatalog {
			t.Fatalf("fixture: title classifies to (%q, %v), want (catalog, true)", name, matched)
		}
		id := ingestOne(t, wlCatalogTitle)
		if name, source := typeOf(t, id); derived.IsDerivedType(name) {
			t.Errorf("ingest classify granted the derived type %q (source %q)", name, source)
		}
	})

	// A failed classify does not fail the write: with no registry wired the
	// hook errors (nil set), is logged at WARN, and the chunk still reports
	// "updated" with the block written and the type it already had — the
	// /api/store contract (context_store.go).
	t.Run("block_mode_classify_failure_keeps_write", func(t *testing.T) {
		const title = "Session 61 Handover classify failure ingest"
		unwired := NewIngestHandler(pool, nil, nil)
		status, resp := postIngest(t, unwired, keyCtx, map[string]any{"source": "i61", "chunks": []map[string]any{{
			"category": "learnings", "title": title, "content": "ingested while classify fails: " + title,
		}}})
		if status != http.StatusOK || resp["success"] != true {
			t.Fatalf("status = %d, success = %v, want 200 and true (body %v)", status, resp["success"], resp)
		}
		r := firstResult(t, resp)
		id, _ := r["id"].(string)
		if r["status"] != "updated" || id == "" {
			t.Fatalf("result = %v, want status updated with an id despite the classify failure", r)
		}
		if name, source := typeOf(t, id); name != "knowledge" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (knowledge, auto)", name, source)
		}
	})
}
