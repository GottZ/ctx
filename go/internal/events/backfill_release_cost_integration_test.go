//go:build integration

// Issue #236, point one: what a release costs the backfill. A tenant that
// releases a mass of its own parked memos must not flood the backfill of
// another tenant. The background arm picks inside one tenant's owned scopes
// per turn (backfillClasses binds the pick to them), so a released block is
// read only by its own tenant's picks; the arm turns from tenant to tenant
// (backfillArmPace), so the other tenant's turns keep coming. Measured here
// on 200 parked blocks of tenant T next to one pending block of tenant U:
// the shared-buffer accesses of each tenant's pick before and after T's
// release (fresh statistics each time), at both deployment values of
// random_page_cost, and the picks T spends on its released blocks (one each:
// the pre-wire estimate parks them again without a wire call). Asserted: U's
// pick does not grow with T's release (a tenth of a buffer per released
// block is the bound; a plan the statistics flip moves it by a few index
// pages), and T works off its release in exactly one pick per block. The
// turn order is the arm's own pace logic, run on the same outcomes.
//
//	go test -tags=integration ./internal/events/ -run TestBackfillReleaseCost -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// releasePickBuffers is the shared-buffer accesses (hits and reads) of one
// execution of the arm's pick for the tenant that owns owned.
func releasePickBuffers(t *testing.T, pool *pgxpool.Pool, router *dream.Router, owned []string) int {
	t.Helper()
	cl := backfillClasses(router, releaseCfg(1, time.Minute, time.Hour), owned, router.EmbedCeiling())
	var raw string
	if err := pool.QueryRow(context.Background(), `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+backfillPickSQL,
		append(cl.Args(), []string{})...).Scan(&raw); err != nil {
		t.Fatalf("explain pick: %v", err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
		t.Fatalf("parse plan: %v", err)
	}
	hit, _ := plans[0].Plan["Shared Hit Blocks"].(float64)
	read, _ := plans[0].Plan["Shared Read Blocks"].(float64)
	return int(hit + read)
}

func TestBackfillReleaseCostInTheNextPick(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
	srv, calls := releaseEmbedServer(t, 0, nil)
	router := releaseRouter(t, srv.URL)
	tenant := func(slug, scope string) string {
		tn, err := store.CreateTenant(ctx, pool, slug, slug)
		if err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
		return tn.ID
	}
	const n = 200
	scopeT, scopeU := "relc-t:data", "relc-u:data"
	tT := tenant("relc-t", scopeT)
	tenant("relc-u", scopeU)
	ownedT, ownedU := []string{scopeT}, []string{scopeU}
	long := strings.Repeat("a long text ", 40)
	for i := range n {
		if _, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("relc t %03d", i), long, nil, nil, scopeT, true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.UpsertBlock(ctx, pool, "learnings", "relc u", "short text", nil, nil, scopeU, true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatal(err)
	}
	small := releaseCfg(50, time.Minute, time.Hour)
	workOff := func(t *testing.T) int {
		t.Helper()
		picks := 0
		for {
			out, err := s.backfillStep(ctx, router, small, ownedT)
			if err != nil {
				t.Fatalf("pick %d of T: %v", picks, err)
			}
			if out == backfillIdle {
				return picks
			}
			if out != backfillParked {
				t.Fatalf("pick %d of T: outcome %v, want parked", picks, out)
			}
			picks++
		}
	}
	if got := workOff(t); got != n {
		t.Fatalf("parking T's blocks took %d picks, want %d", got, n)
	}

	type cost struct{ t, u int }
	measure := func() map[string]cost {
		if _, err := pool.Exec(ctx, `ANALYZE context_blocks, context_embed_failures`); err != nil {
			t.Fatal(err)
		}
		m := map[string]cost{}
		for _, rpc := range testdb.RandomPageCosts {
			p := testdb.WithRandomPageCost(t, pool, rpc)
			m[rpc] = cost{releasePickBuffers(t, p, router, ownedT), releasePickBuffers(t, p, router, ownedU)}
		}
		return m
	}
	before := measure()
	counted := int64(n)
	res, err := store.ReleaseBackfillMemos(ctx, pool, store.BackfillRelease{Class: store.EmbedFailureOversize, TenantID: tT, Confirm: &counted})
	if err != nil || res.Released != n {
		t.Fatalf("T's release: %+v %v, want %d released", res, err, n)
	}
	after := measure()
	wire := calls.Load()
	picks := workOff(t)
	for _, rpc := range testdb.RandomPageCosts {
		t.Logf("random_page_cost %s: pick of T %d → %d buffer accesses, pick of U %d → %d (before → after T's release of %d)",
			rpc, before[rpc].t, after[rpc].t, before[rpc].u, after[rpc].u, n)
		// A release that reached U's pick would put T's released blocks in
		// its range, one or more buffers each; a plan the fresh statistics
		// choose may differ by a few index pages either way.
		if after[rpc].u > before[rpc].u+n/10 {
			t.Errorf("random_page_cost %s: U's pick reads %d buffers after T's release of %d, %d before — T's release reaches U's pick", rpc, after[rpc].u, n, before[rpc].u)
		}
	}
	t.Logf("T worked off its release in %d picks, %d wire calls", picks, calls.Load()-wire)
	if picks != n || calls.Load() != wire {
		t.Errorf("T's release cost %d picks and %d wire calls, want %d and 0 (one pick per released block, the estimate parks it again)", picks, calls.Load()-wire, n)
	}

	// The arm's turns on these outcomes: T parks on every pick of its own
	// (a parked block leaves its tenant due), U is due with a pending block;
	// the pace hands every second turn to U while T works off its release.
	p := newBackfillArmPace()
	now := time.Now()
	p.setTenants([]backgroundTenant{{scope: "t", owned: ownedT}, {scope: "u", owned: ownedU}}, now)
	p.begin(now, 15*time.Second)
	var order []string
	for range 6 {
		i, ok := p.nextDue(now, 15*time.Second)
		if !ok {
			t.Fatal("no tenant due")
		}
		bt := p.tenants[i]
		p.cursor = i + 1
		order = append(order, bt.scope)
		if bt.scope == "t" {
			p.record("t", backfillParked, now)
		} else {
			p.record("u", backfillEmbedded, now)
		}
	}
	if got := strings.Join(order, ""); got != "tututu" {
		t.Errorf("turns while T works off its release: %s, want tututu", got)
	}
}
