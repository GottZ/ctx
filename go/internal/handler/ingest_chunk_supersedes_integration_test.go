//go:build integration

// Issue #191 — a chunk row belongs to the ingest: a dream supersedes link onto
// it must not move it out of the 'chunk' lifecycle, because InsertChunk only
// finds its (source_id, chunk_index) slot through the arbiter
// `ON CONFLICT (source_id, chunk_index) WHERE NOT is_archived AND
// lifecycle_state = 'chunk'`. Before the fix the supersedes side-effect
// retired the chunk as a snapshot, the reverse turned it into 'knowledge', and
// a re-ingest of that slot with changed content missed the arbiter, tried a
// fresh insert and failed on uq_context_category_title_scope (23505).
//
// Every fixture goes through the production write paths: the chunk through
// POST /api/ingest, the superseder through store.UpsertBlock, the link through
// dream.WriteLinks, its removal through store.DreamLinkResolve.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestIngestChunkSlotSurvivesDreamSupersedes -count=1 -v
package handler

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i191Supersede ingests one chunk into a fresh source, writes a newer block and
// lets dream.WriteLinks accept "newer supersedes chunk" at confidence 0.9.
// It returns the source id, the chunk id and the superseder id.
func i191Supersede(t *testing.T, h *IngestHandler, keyCtx context.Context, label, title, content string) (sourceID, chunkID, supersederID string) {
	t.Helper()
	ctx := context.Background()
	sourceID = seedSource(t, h.pool, "/i191/"+label+".md", "private")
	seed := postIdenticalChunk(t, h, keyCtx, sourceID, title, content, []string{"i191"}, nil)
	if seed["status"] != "inserted" {
		t.Fatalf("seed chunk: %v", seed)
	}
	chunkID, _ = seed["id"].(string)

	// The superseder is created after the chunk: supersedes points from the
	// newer block to the older one (dream.enforceSupersedesDirection).
	time.Sleep(20 * time.Millisecond)
	src, err := store.UpsertBlock(ctx, h.pool, "test", title+" v2", content+" — revised later",
		nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("upsert superseder: %v", err)
	}
	set := blocktype.NewRegistry().Snapshot()
	n, err := dream.WriteLinks(ctx, h.pool, set, src.ID, "private", 1.0,
		[]dream.Link{{TargetID: chunkID, Relationship: "supersedes", Confidence: 0.9}})
	if err != nil || n != 1 {
		t.Fatalf("WriteLinks supersedes onto the chunk: written=%d err=%v", n, err)
	}
	return sourceID, chunkID, src.ID
}

// i191Lifecycle reads lifecycle_state and superseded_by ("" for NULL).
func i191Lifecycle(t *testing.T, h *IngestHandler, id string) (lifecycle, supersededBy string) {
	t.Helper()
	if err := h.pool.QueryRow(context.Background(),
		`SELECT lifecycle_state, COALESCE(superseded_by::text, '') FROM context_blocks WHERE id = $1::uuid`, id,
	).Scan(&lifecycle, &supersededBy); err != nil {
		t.Fatalf("read lifecycle of %s: %v", id, err)
	}
	return lifecycle, supersededBy
}

func TestIngestChunkSlotSurvivesDreamSupersedes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewIngestHandler(pool, nil, nil)
	keyCtx, _ := ingestKey(t, pool, "i191-ingest")
	ctx := context.Background()

	// The supersedes link is written and stays readable by the query path
	// (dream.SupersedesMap feeds filterSuperseded and superseded_by in the
	// query responses) — the chunk itself keeps its lifecycle and gets no
	// snapshot pointer.
	t.Run("supersedes_link_keeps_the_chunk_in_its_lifecycle", func(t *testing.T) {
		_, chunkID, supersederID := i191Supersede(t, h, keyCtx, "keep",
			"i191 deployment runbook for the cluster", "i191 chunk content of the deployment runbook")

		if lc, ptr := i191Lifecycle(t, h, chunkID); lc != "chunk" || ptr != "" {
			t.Errorf("chunk after supersedes: lifecycle_state=%q superseded_by=%q, want chunk and NULL", lc, ptr)
		}
		m, err := dream.SupersedesMap(ctx, pool, []string{chunkID})
		if err != nil {
			t.Fatalf("SupersedesMap: %v", err)
		}
		if !slices.Equal(m[chunkID], []string{supersederID}) {
			t.Errorf("SupersedesMap[chunk] = %v, want [%s] — the supersedes statement must stay readable", m[chunkID], supersederID)
		}
	})

	// The issue's failing case: changed content into the slot of a chunk that
	// a supersedes link points at lands in that same row.
	t.Run("changed_content_reingest_lands_in_the_slot", func(t *testing.T) {
		const (
			title   = "i191 backup procedure for the archive"
			content = "i191 chunk content of the backup procedure"
		)
		sourceID, chunkID, _ := i191Supersede(t, h, keyCtx, "reingest", title, content)

		edited := postIdenticalChunk(t, h, keyCtx, sourceID, title, content+" edited", []string{"i191"}, nil)
		if edited["status"] != "inserted" || edited["id"] != chunkID {
			t.Fatalf("changed-content re-ingest = %v, want status inserted on the same row %s", edited, chunkID)
		}
		got := readIdenticalChunkState(t, pool, chunkID)
		if got.content != content+" edited" || got.lifecycleState != "chunk" {
			t.Errorf("slot row after re-ingest: content=%q lifecycle_state=%q, want the edited content and chunk",
				got.content, got.lifecycleState)
		}
		again := postIdenticalChunk(t, h, keyCtx, sourceID, title, content+" edited", []string{"i191"}, nil)
		if again["status"] != "skipped" || again["id"] != chunkID {
			t.Errorf("identical re-ingest = %v, want skipped on %s", again, chunkID)
		}
	})

	// The reverse path: when the supersedes link goes, the chunk is still a
	// chunk — not 'knowledge', which would make it a dream pick and a guard
	// candidate and keep it out of the arbiter for good.
	t.Run("removing_the_link_keeps_the_chunk_in_its_lifecycle", func(t *testing.T) {
		const title = "i191 incident playbook for the database"
		sourceID, chunkID, supersederID := i191Supersede(t, h, keyCtx, "remove", title, "i191 chunk content of the incident playbook")

		res, err := store.DreamLinkResolve(ctx, pool, supersederID, chunkID, "supersedes", "delete", "", []string{"private"})
		if err != nil || res == nil {
			t.Fatalf("dream-link-resolve delete: res=%v err=%v", res, err)
		}
		if res.SupersedesReverted {
			t.Errorf("dream-link-resolve delete reported supersedes_reverted for a chunk target")
		}
		if lc, ptr := i191Lifecycle(t, h, chunkID); lc != "chunk" || ptr != "" {
			t.Errorf("chunk after link removal: lifecycle_state=%q superseded_by=%q, want chunk and NULL", lc, ptr)
		}
		edited := postIdenticalChunk(t, h, keyCtx, sourceID, title, "i191 playbook rewritten", []string{"i191"}, nil)
		if edited["status"] != "inserted" || edited["id"] != chunkID {
			t.Errorf("changed-content re-ingest after link removal = %v, want inserted on %s", edited, chunkID)
		}
	})
}
