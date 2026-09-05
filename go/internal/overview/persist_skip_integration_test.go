//go:build integration

// persist unter Advisory-Lock-Konkurrenz: ein fremdes Tx hält den
// Partitions-Lock, persist muss Stats{Skipped:true, SkipReason:"advisory-lock",
// CandidateCount:…} mit err == nil liefern und nichts schreiben (T04-4d: der
// Ausgang läuft über errPersistLockSkipped + errors.Is hinter der pgxdb-Klammer).
//
// Wie super_lock_integration_test.go liegt die Datei in package `overview` und
// nicht in `overview_test`: persist ist unexportiert, und der direkte Aufruf IST
// die Probe — der Skip-Ausgang ist von außen nicht ansteuerbar.
package overview

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestPersistSkipsUnderAdvisoryLock(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	cl, scopes, edges := superLockFixture(t, pool, 4)
	types := []string{"knowledge"}
	opts := Options{Resolution: 1.0, VisibleTypes: types, OverviewTypes: types}
	level := computeSuperLevel(ctx, cl, scopes, edges, superParams{Enabled: false})

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	var locked bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKeyForScopes(opts.ScopeFilter)).Scan(&locked); err != nil || !locked {
		t.Fatalf("holder lock: locked=%v err=%v", locked, err)
	}

	candidates := tallyScopes(scopes)
	stats, err := persist(ctx, pool, cl, opts, scopes, candidates, level)
	if err != nil {
		t.Fatalf("persist unter Lock-Konkurrenz: err=%v, erwartet nil", err)
	}
	if !stats.Skipped || stats.SkipReason != "advisory-lock" || stats.CandidateCount["private"] != candidates["private"] {
		t.Fatalf("persist unter Lock-Konkurrenz: %+v, erwartet Skipped/advisory-lock/CandidateCount=%v", stats, candidates)
	}
	if stats.NodeCount != 0 || stats.ClusterCount != 0 || stats.PersistMs != 0 || stats.LockHeldMs != 0 {
		t.Fatalf("Skip-Stats tragen Fremdwerte: %+v", stats)
	}
	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_member`).Scan(&members); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if members != 0 {
		t.Fatalf("Skip hat %d Member geschrieben", members)
	}

	_ = holder.Rollback(ctx)
	stats2, err := persist(ctx, pool, cl, opts, scopes, candidates, level)
	if err != nil || stats2.Skipped {
		t.Fatalf("persist ohne Konkurrenz: err=%v skipped=%v", err, stats2.Skipped)
	}
}
