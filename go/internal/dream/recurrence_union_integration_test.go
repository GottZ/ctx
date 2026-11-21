//go:build integration

// Eval/recurrence union pin (RunDreamCycle step 5): WriteLinks replaces per
// CALL — every unpinned link of the source whose target is not in THIS call's
// batch is deleted (replaceStaleLinks). A cycle that wrote its eval links and
// its recurrent links in two separate calls therefore let the second call
// sweep away everything the first one had just written: any source with a
// confirmed recurrent pair lost all of its topical/factual/causal/supersedes
// links from the same cycle (live: 394 of 399 sources with a recurrent link
// had no other outgoing link). This file pins what a full cycle LEAVES BEHIND
// when both link classes fire, through the production path end to end — the
// pick, the cycle's own temporal Phase 1 for the source, the RRF candidate
// search, the eval and the recurrence confirm all run for real; only the chat
// wire is the seam.
package dream_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	ruSrcID   = "019d0000-0000-7000-9000-000000001b01" // S: the dreamed block
	ruTopicID = "019d0000-0000-7000-9000-000000001b02" // C: eval candidate, topical
	ruRecurID = "019d0000-0000-7000-9000-000000001b03" // D: recurrence partner

	// Own scope so pick, RRF arm and recurrence Phase 1 (same-scope) see only
	// these three blocks (context_blocks.scope is VARCHAR(20)).
	ruScope = "recunion-it"

	// The one keyword the cycle searches with, pre-seeded on S and in
	// context_embed_cache so neither a keyword LLM call nor an embed wire
	// call happens.
	ruKeyword = "recunion"

	// First line of recurrenceSystemPrompt — the seam's discriminator for the
	// Phase-2 confirm (crEvalMarker discriminates the eval, see
	// capretry_integration_test.go for why the SYSTEM prompt is the key).
	ruRecurMarker = "Classify whether two knowledge blocks form a recurring pattern"

	// S and D share this title shape (pg_trgm sim > 0.5, the Phase-1 gate);
	// C's title is far from it, and C carries no context_temporal row at all,
	// so Phase 1 pairs S with D only.
	ruSrcTitle   = "Recurring Weekly Sync Session"
	ruRecurTitle = "Recurring Weekly Sync Session Gamma"
	ruTopicTitle = "Deployment checklist notes"
)

// ruArm makes an inserted fixture row cycle-ready: an embedding (pick conjunct
// for S, vector-arm hit for a candidate — crVec puts it at cosine 1.0 to the
// keyword) and sensitivity 'internal' instead of the fail-closed credentials
// default. pickable=false parks the row behind a far cooldown so PickBlock
// cannot choose it over S; a parked row is still an RRF candidate, the
// candidate search does not read dream cooldowns.
func ruArm(t *testing.T, pool *pgxpool.Pool, id string, keywords []string, pickable bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cooldown := "NULL"
	if !pickable {
		cooldown = "now() + interval '30 days'"
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET
			embedding = $2,
			dream_keywords = $3::text[],
			dream_cooldown_until = `+cooldown+`,
			sensitivity = 'internal'
		 WHERE id = $1::uuid`,
		id, crVec(), keywords); err != nil {
		t.Fatalf("arm block %s: %v", id, err)
	}
}

// TestRunDreamCycle_RecurrentLinksKeepEvalLinks_DB is the regression pin for
// the per-call replace semantics: ONE cycle in which the eval confirms a
// topical link S→C and the recurrence confirm a recurrent link S→D must leave
// BOTH rows behind.
//
// "recurrent overlaps eval" additionally pins the same-pair order inside the
// one WriteLinks call: the eval ALSO answers topical for S→D, the recurrence
// confirm answers recurrent for the same pair, and recurrent — appended after
// the eval links — must be what the row ends up as (the more specific class
// wins, the pre-fix contract of the separate second call), without a
// duplicate-target error inside the transaction.
func TestRunDreamCycle_RecurrentLinksKeepEvalLinks_DB(t *testing.T) {
	cases := []struct {
		name string
		// recurInEval guarantees D a place in the eval candidate set
		// (embedding, vector arm at cosine 1.0) and has the eval answer
		// topical for it.
		recurInEval bool
	}{
		{name: "disjoint targets", recurInEval: false},
		{name: "recurrent overlaps eval", recurInEval: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runRecurrentUnionCycle(t, tc.recurInEval)
		})
	}
}

func runRecurrentUnionCycle(t *testing.T, recurInEval bool) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tRef := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	insertBlock(t, pool, ruSrcID, ruScope, "projects", ruSrcTitle, tRef, tRef)
	insertBlock(t, pool, ruTopicID, ruScope, "decisions", ruTopicTitle, tRef, tRef)
	insertBlock(t, pool, ruRecurID, ruScope, "projects", ruRecurTitle, tRef, tRef)
	ruArm(t, pool, ruSrcID, []string{ruKeyword}, true)
	ruArm(t, pool, ruTopicID, nil, false)
	if recurInEval {
		ruArm(t, pool, ruRecurID, nil, false)
	} else if _, err := pool.Exec(ctx,
		// No embedding: D is not pickable and no vector-arm hit. Another RRF
		// arm may still list it as an eval candidate (on this fixture the
		// cycle logs candidate_count=2) — irrelevant here, the eval answer
		// names C only, so D's link can come from the recurrence confirm alone.
		`UPDATE context_blocks SET sensitivity = 'internal' WHERE id = $1::uuid`, ruRecurID); err != nil {
		t.Fatalf("set D sensitivity: %v", err)
	}

	// D's temporal dimensions through the store write path (what the store
	// handler runs on save); S gets the same created_at dimensions from the
	// cycle's own temporal Phase 1 (step 1b, dream_temporal_validated_at is
	// NULL on a fresh row). C gets none, so it cannot pair in Phase 1.
	var recurContent string
	if err := pool.QueryRow(ctx, `SELECT content FROM context_blocks WHERE id = $1::uuid`, ruRecurID).Scan(&recurContent); err != nil {
		t.Fatalf("read D content: %v", err)
	}
	if applied, err := store.PopulateTemporal(ctx, pool, ruRecurID, recurContent, nil, tRef); err != nil || !applied {
		t.Fatalf("populate D temporal: applied=%v err=%v", applied, err)
	}

	backend := crBackend()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
		 VALUES ($1, $2, $3, $4)`,
		embedcache.HashKey(embed.PrefixQuery, ruKeyword),
		backend.ModelFor(backends.RoleEmbed).Model, crVec(), ruKeyword); err != nil {
		t.Fatalf("seed keyword embed cache: %v", err)
	}

	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{backend})
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	r := &dream.Router{
		Pool:       p,
		Blocktypes: reg,
		Admit:      llm.Admission{Admitter: d, Class: dispatch.ClassBackground},
	}

	evalAnswer := `[{"target_id":"` + ruTopicID + `","type":"topical","confidence":0.9}]`
	if recurInEval {
		evalAnswer = `[{"target_id":"` + ruTopicID + `","type":"topical","confidence":0.9},` +
			`{"target_id":"` + ruRecurID + `","type":"topical","confidence":0.9}]`
	}
	var evalCalls, recurCalls atomic.Int32
	swapChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, systemPrompt, userPrompt string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		switch {
		case strings.Contains(systemPrompt, crEvalMarker):
			evalCalls.Add(1)
			return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: evalAnswer}}, nil
		case strings.Contains(systemPrompt, ruRecurMarker):
			recurCalls.Add(1)
			if strings.Contains(userPrompt, ruRecurID) {
				return recurResp(), nil // recurrent, confidence 0.95
			}
			return &llm.ChatResponse{Message: llm.Message{Role: "assistant",
				Content: `{"verdict":"none","pattern":"none","confidence":0.9}`}}, nil
		default:
			return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: crBenign}}, nil
		}
	})

	backoff := dream.BackoffConfig{Mode: "exp", Factor: 1.6, Grace: 0, MinHours: 12, CapHours: 1080, InertOffset: 7}
	written, err := dream.RunDreamCycle(ctx, pool, r, dream.DreamOptions(), backoff,
		[]string{ruScope}, dream.NoThrottle)
	if err != nil {
		t.Fatalf("RunDreamCycle: %v", err)
	}

	// Both classifiers must actually have fired, or the row assertions below
	// would test a fixture gap instead of the write.
	if n := evalCalls.Load(); n != 1 {
		t.Fatalf("dream-eval calls = %d, want 1", n)
	}
	if n := recurCalls.Load(); n != 1 {
		t.Fatalf("dream-recurrence calls = %d, want 1 — Phase 1 must pair S with D only (shared created_at dimensions, title sim > 0.5)", n)
	}
	if !recurInEval && written != 2 {
		t.Errorf("cycle reported %d written link(s), want 2 (one topical, one recurrent)", written)
	}

	got := map[string]string{}
	rows, err := pool.Query(ctx,
		`SELECT target_block_id::text, relationship FROM context_dream_links WHERE source_block_id = $1::uuid`,
		ruSrcID)
	if err != nil {
		t.Fatalf("read links: %v", err)
	}
	for rows.Next() {
		var tgt, rel string
		if err := rows.Scan(&tgt, &rel); err != nil {
			t.Fatalf("scan link: %v", err)
		}
		got[tgt] = rel
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate links: %v", err)
	}

	if rel, ok := got[ruTopicID]; !ok || rel != "topical" {
		t.Errorf("eval link S→C = %q (present=%v), want topical — the recurrence write must not sweep the cycle's eval links (links: %v)", rel, ok, got)
	}
	if rel, ok := got[ruRecurID]; !ok || rel != "recurrent" {
		t.Errorf("recurrence link S→D = %q (present=%v), want recurrent (links: %v)", rel, ok, got)
	}
	if len(got) != 2 {
		t.Errorf("source has %d link row(s), want exactly 2: %v", len(got), got)
	}

	var lastInert bool
	if err := pool.QueryRow(ctx,
		`SELECT dream_last_inert FROM context_blocks WHERE id = $1::uuid`, ruSrcID).Scan(&lastInert); err != nil {
		t.Fatalf("read source state: %v", err)
	}
	if lastInert {
		t.Errorf("dream_last_inert = true after a cycle that wrote links")
	}
}
