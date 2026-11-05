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
//
// #70, the follow-ups from the #69 review. TestIngestClassifySnapshotOnce-
// PerRequest counts the tenant-set build WARN lines of one request of three
// block-mode chunks, want 1: RED on b52b340a (the hook resolved per chunk)
// with 3, and with 4 when the hook resolves on its own again next to the set
// HandleIngest hands processChunk. RED with the resolution moved ahead of the
// batch gates or the rate limit: the request the rate limit refuses adds a
// line. RED with the hook run for index == 0 only (every probe above sends
// exactly one chunk and stays green): block_mode_multi_chunk_each_classified
// and mixed_block_and_chunk_mode_request read (knowledge, auto) for chunks 1
// and 2. The mixed request leads with its chunk-mode chunk, so a snapshot
// resolved only when chunk 0 is block mode leaves both block-mode chunks
// unclassified as well.
package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
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

	// #70: one request of several block-mode chunks classifies EVERY chunk,
	// each by its own title and metadata. Three different verdicts, the first
	// one the row default: a loop that classifies index 0 only, stops short of
	// the last chunk or hands one chunk's verdict to the whole batch leaves a
	// row on the wrong type.
	t.Run("block_mode_multi_chunk_each_classified", func(t *testing.T) {
		postIngestProbes(t, h, keyCtx, pool, set, []ingestProbe{
			{title: "i70 plain batch note", want: ingestRowDefault},
			{title: "i70 metadata-only batch note", meta: map[string]any{"is_meta": true}, want: "system-meta"},
			{title: "Session 70 Handover batch ingest", want: "audit-trail"},
		})
	})

	// #70: a request that leads with a chunk-mode chunk leaves that chunk
	// unclassified and still classifies both block-mode chunks after it — the
	// hook keys on each chunk's mode, not on its position or on chunk 0.
	t.Run("mixed_block_and_chunk_mode_request", func(t *testing.T) {
		src := seedSource(t, pool, "/i70/mixed.md", "private")
		postIngestProbes(t, h, keyCtx, pool, set, []ingestProbe{
			{title: "Session 70 Handover mixed chunk ingest", sourceID: src, want: ingestRowDefault},
			{title: "Session 70 Handover mixed block ingest", want: "audit-trail"},
			{title: "i70 metadata-only mixed note", meta: map[string]any{"is_meta": true}, want: "system-meta"},
		})
	})
}

// ingestRowDefault is the type_name a context_blocks row carries until the
// classifier assigns one (the column default, 113 baseline).
const ingestRowDefault = "knowledge"

// ingestProbe is one chunk of a multi-chunk classify probe (#70): block mode
// when sourceID is empty, chunk mode (category vault, chunk_index 0) on that
// source otherwise. want is the type_name its row carries after the request;
// type_source stays 'auto' in both modes.
type ingestProbe struct {
	title    string
	meta     map[string]any
	sourceID string
	want     string
}

// postIngestProbes sends probes as ONE /api/ingest request and asserts every
// row by its result index. Each fixture is checked against set first: a
// block-mode title (with its metadata) must classify to want, or to nothing
// when want is the row default; a chunk-mode title must hit a rule, or the
// probe could not tell a skipped hook from one that ran.
func postIngestProbes(t *testing.T, h *IngestHandler, keyCtx context.Context, pool *pgxpool.Pool, set *blocktype.Set, probes []ingestProbe) {
	t.Helper()
	chunks := make([]map[string]any, len(probes))
	for i, p := range probes {
		name, matched := set.Classify(p.title, p.meta)
		if p.sourceID != "" && !matched {
			t.Fatalf("fixture: chunk %d title %q hits no classify rule, want a match", i, p.title)
		}
		if p.sourceID == "" && (name != p.want || matched != (p.want != ingestRowDefault)) {
			t.Fatalf("fixture: chunk %d classifies to (%q, %v), want %q", i, name, matched, p.want)
		}
		chunk := map[string]any{"category": "learnings", "title": p.title, "content": "i70 batch chunk: " + p.title}
		if p.meta != nil {
			chunk["metadata"] = p.meta
		}
		if p.sourceID != "" {
			chunk["category"], chunk["source_id"], chunk["chunk_index"] = "vault", p.sourceID, 0
		}
		chunks[i] = chunk
	}
	status, resp := postIngest(t, h, keyCtx, map[string]any{"source": "i70", "chunks": chunks})
	if status != http.StatusOK || resp["success"] != true {
		t.Fatalf("status = %d, success = %v, want 200 and true (body %v)", status, resp["success"], resp)
	}
	results, _ := resp["results"].([]any)
	if len(results) != len(probes) {
		t.Fatalf("results = %v, want %d", resp["results"], len(probes))
	}
	for i, p := range probes {
		r, _ := results[i].(map[string]any)
		id, _ := r["id"].(string)
		wantStatus := "updated"
		if p.sourceID != "" {
			wantStatus = "inserted"
		}
		if idx, _ := r["index"].(float64); int(idx) != i || r["status"] != wantStatus || id == "" {
			t.Fatalf("result %d = %v, want index %d, status %s, with an id", i, r, i, wantStatus)
		}
		var name, source string
		if err := pool.QueryRow(context.Background(),
			`SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).Scan(&name, &source); err != nil {
			t.Fatalf("read type of chunk %d: %v", i, err)
		}
		if name != p.want || source != "auto" {
			t.Errorf("chunk %d (%q): (type_name, type_source) = (%q, %q), want (%s, auto)", i, p.title, name, source, p.want)
		}
	}
}

// TestIngestClassifySnapshotOncePerRequest pins #70 item 1: HandleIngest
// resolves the caller's block-type set ONCE per request and hands it to every
// chunk. The probe drives the registry's miss path, where each resolution is
// visible: a tenant row DecodePolicy rejects (a reserved graph class, as in
// TestGuardLoadPath_TenantFallback_Integration) fails the tenant-set build,
// which is not cached and logs one WARN per resolution. One request of three
// block-mode chunks logs that WARN once; resolving per chunk logs it three
// times — and lets a registry reload landing mid-batch classify one request
// against two rule generations. A second request, refused by the rate limit,
// must not resolve the set at all.
func TestIngestClassifySnapshotOncePerRequest(t *testing.T) {
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
	// The production hook (cmd/ctxd/server.go): the registry resolves the
	// request's tenant from the key's home scope. Without it every request
	// reads the base set and never reaches the tenant miss path.
	blocktype.SetRequestScopeHook(RequestTenantScope)
	t.Cleanup(func() { blocktype.SetRequestScopeHook(nil) })

	keyCtx, ar := ingestKey(t, pool, "i70-snapshot")
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 VALUES ('i70-undecodable-type', $1, false, false, '{"v":1,"structural_link_classes":["topical"]}'::jsonb)`,
		ar.HomeScope); err != nil {
		t.Fatalf("plant a tenant row the registry cannot decode: %v", err)
	}

	// Write rate limit 1: the first request books the key's only slot, the
	// second is refused with 429.
	h := NewIngestHandler(pool, staticConfigStore{cfg: &config.Config{Query: config.QueryConfig{RateLimitWrite: 1}}}, reg)
	logs := captureFLLogs(t)
	const buildFailed = "WARN blocktype: tenant set build failed"
	chunks := make([]map[string]any, 3)
	for i := range chunks {
		title := fmt.Sprintf("i70 snapshot probe note %d", i)
		chunks[i] = map[string]any{"category": "learnings", "title": title, "content": "one snapshot per request: " + title}
	}
	status, resp := postIngest(t, h, keyCtx, map[string]any{"source": "i70", "chunks": chunks})
	if status != http.StatusOK || resp["success"] != true || resp["updated"] != float64(len(chunks)) {
		t.Fatalf("status = %d, body %v, want 200 with %d block-mode writes", status, resp, len(chunks))
	}
	if got := logs.count(buildFailed); got != 1 {
		t.Errorf("one request of %d block-mode chunks logged %d %q lines, want 1 (one snapshot per request):\n%s",
			len(chunks), got, buildFailed, logs.all())
	}

	status, resp = postIngest(t, h, keyCtx, map[string]any{"source": "i70", "chunks": chunks[:1]})
	if status != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429 (body %v)", status, resp)
	}
	if got := logs.count(buildFailed); got != 1 {
		t.Errorf("after a request the rate limit refused: %d %q lines, want still 1 (a refused request resolves no snapshot):\n%s",
			got, buildFailed, logs.all())
	}
}
