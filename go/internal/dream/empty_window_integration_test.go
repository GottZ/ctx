//go:build integration

// Empty dream window (two tenants, one container): a NON-nil, EMPTY read
// window is what store.IntersectWindow returns for a bound tenant whose
// read_scopes share no scope with what it owns — a freshly bootstrapped tenant
// (scope "<slug>:main") under the default scheduler.read_scopes
// (private,shared,work). That window must mean "this tenant reads nothing":
// no pick, no claim, no LLM call, and every tenant's block stays untouched.
// nil keeps the legacy unbounded single pass.
//
//	go test -tags=integration ./internal/dream/ -run EmptyWindow -count=1 -v
package dream_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ewBlockState is the dream bookkeeping of one block — the columns a cycle
// over that block writes (claim, keywords, checked/cooldown stamps).
type ewBlockState struct {
	checkedAt  *time.Time
	cooldown   *time.Time
	keywords   []string
	evalCount  int
	temporalAt *time.Time
}

func readEWState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) ewBlockState {
	t.Helper()
	var st ewBlockState
	if err := pool.QueryRow(ctx,
		`SELECT dream_checked_at, dream_cooldown_until, dream_keywords, dream_eval_count, dream_temporal_validated_at
		 FROM context_blocks WHERE id = $1::uuid`, id).
		Scan(&st.checkedAt, &st.cooldown, &st.keywords, &st.evalCount, &st.temporalAt); err != nil {
		t.Fatalf("read dream state of %s: %v", id, err)
	}
	return st
}

func TestEmptyWindow_TwoTenants_NoForeignPick_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Two tenants, each owning exactly its bootstrap scope.
	tenA, err := store.CreateTenant(ctx, pool, "ewa", "Empty window A")
	if err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	tenB, err := store.CreateTenant(ctx, pool, "ewb", "Empty window B")
	if err != nil {
		t.Fatalf("create tenant B: %v", err)
	}
	const scopeA, scopeB = "ewa:main", "ewb:main"
	if _, err := store.AssignTenantScope(ctx, pool, tenA.ID, scopeA, nil); err != nil {
		t.Fatalf("assign %s: %v", scopeA, err)
	}
	if _, err := store.AssignTenantScope(ctx, pool, tenB.ID, scopeB, nil); err != nil {
		t.Fatalf("assign %s: %v", scopeB, err)
	}

	// B owns one dream-eligible block: embedding set, keywords pre-seeded
	// (no keyword LLM call needed), cooldown NULL, never checked. It is the
	// ONLY block in the database, so an unfiltered pick has exactly one answer.
	const idB = "019f3100-0000-7000-9000-00000000b0e1"
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insertBlock(t, pool, idB, scopeB, "learnings", "Empty window B block", tEarly, tEarly)
	armCapRetryBlock(t, pool, idB) // embedding, dream_keywords={capretry}, cooldown NULL, sensitivity internal

	// A's window under the default read_scopes: the clamp leaves nothing.
	ownedA, err := store.TenantScopes(ctx, pool, tenA.ID)
	if err != nil {
		t.Fatalf("TenantScopes(A): %v", err)
	}
	window := store.IntersectWindow([]string{"private", "shared", "work"}, ownedA)
	if window == nil || len(window) != 0 {
		t.Fatalf("fixture: A's window = %#v, want a non-nil empty slice (owned %v)", window, ownedA)
	}

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	linkable := reg.SnapshotForTenant(ctx, scopeA).DreamLinkableTypes()

	backend := crBackend()
	seedKeywordEmbed(t, pool, backend)
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{backend})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	// Router of the ITERATED tenant A — the one whose chain would have carried
	// B's content before the gate.
	r := &dream.Router{
		Pool:       p,
		Blocktypes: reg,
		Tenant:     scopeA,
		Admit:      llm.Admission{Admitter: d, Class: dispatch.ClassBackground},
	}

	var llmCalls atomic.Int32
	swapChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		llmCalls.Add(1)
		return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: `{}`}}, nil
	})

	before := readEWState(t, ctx, pool, idB)
	backoff := dream.BackoffConfig{Mode: "exp", Factor: 1.6, Grace: 0, MinHours: 12, CapHours: 1080, InertOffset: 7}

	// 1. The full cycle under A's empty window: nothing picked, nothing linked,
	//    no LLM call.
	written, err := dream.RunDreamCycle(ctx, pool, r, dream.DreamOptions(), backoff, window, dream.NoThrottle)
	if err != nil {
		t.Fatalf("RunDreamCycle(A, empty window): %v", err)
	}
	if written != 0 {
		t.Fatalf("RunDreamCycle(A, empty window) wrote %d links, want 0", written)
	}
	if n := llmCalls.Load(); n != 0 {
		t.Fatalf("RunDreamCycle(A, empty window) made %d LLM call(s) — on B's block, through A's router", n)
	}

	// 2. PickBlock directly: no pick.
	got, err := dream.PickBlock(ctx, pool, linkable, window, dream.PickClaimTTL(nil))
	if err != nil {
		t.Fatalf("PickBlock(empty window): %v", err)
	}
	if got != nil {
		t.Fatalf("PickBlock(empty window) picked %s (scope %s) — a foreign tenant's block", got.ID, got.Scope)
	}

	// 3. The stats/restamp family reports nothing for the empty window.
	total, checked, linked, pending, err := dream.Stats(ctx, pool, window, linkable)
	if err != nil || total+checked+linked+pending != 0 {
		t.Fatalf("Stats(empty window) = %d/%d/%d/%d, err %v; want zeros", total, checked, linked, pending, err)
	}
	q, err := dream.QueueDepth(ctx, pool, window, linkable)
	if err != nil || q.PickableNow != 0 || q.NeverDreamed != 0 {
		t.Fatalf("QueueDepth(empty window) = %+v, err %v; want zeros", q, err)
	}
	if restamped, skipped, err := dream.RestampBackoff(ctx, pool, window, linkable, backoff); err != nil || restamped != 0 || skipped != 0 {
		t.Fatalf("RestampBackoff(empty window) = %d/%d, err %v; want 0/0", restamped, skipped, err)
	}

	// 4. B's block is byte-for-byte untouched by everything above.
	after := readEWState(t, ctx, pool, idB)
	if !sameTime(before.checkedAt, after.checkedAt) || !sameTime(before.cooldown, after.cooldown) ||
		!sameTime(before.temporalAt, after.temporalAt) || before.evalCount != after.evalCount ||
		!slices.Equal(before.keywords, after.keywords) {
		t.Fatalf("B's block changed under A's empty window:\n before %+v\n after  %+v", before, after)
	}
	if after.cooldown != nil || after.checkedAt != nil {
		t.Fatalf("B's block carries dream stamps after A's empty-window cycle: checked %v, cooldown %v", after.checkedAt, after.cooldown)
	}

	// 5. Legacy arm: nil is the unbounded single pass and still picks — the
	//    only block there is, B's (byte-identical pre-gate behaviour).
	legacy, err := dream.PickBlock(ctx, pool, linkable, nil, dream.PickClaimTTL(nil))
	if err != nil {
		t.Fatalf("PickBlock(nil): %v", err)
	}
	if legacy == nil || legacy.ID != idB {
		t.Fatalf("PickBlock(nil) = %+v, want B's block %s (legacy unbounded pick)", legacy, idB)
	}
	if st := readEWState(t, ctx, pool, idB); st.cooldown == nil {
		t.Fatal("PickBlock(nil) claimed B's block but wrote no claim cooldown")
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
