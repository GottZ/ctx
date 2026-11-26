//go:build integration

// Issue #555, the code-owner review of the pull request: four ways a
// tenant-delete took something of another owner or left something of its own
// behind.
//
//   - the key step carried no owner test: a second delete whose first had
//     ended deleted the key the next owner of a scope name had its home in,
//     and took the name out of the lists of that owner's other keys;
//   - a scope assigned to the tenant after the delete read its scopes was
//     never pruned, and the tenant row's delete cascaded its mapping away;
//   - an id of the tenant in capitals left the tenant's queued embed-exclusion
//     reconciles behind (the queue names the tenant as PostgreSQL writes it);
//   - the closing round after the last of the bound failed although nothing
//     was left.
package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// keyStepHold stops a prune right before the first statement of its key step:
// it has marked the tenant offboarding and read its scope names.
func keyStepHold() *pruneHold {
	return newPruneHold(func(sql string) bool {
		return strings.Contains(sql, "hashtext(id::text) AS h FROM context_api_keys")
	})
}

func TestSecondTenantDeleteLeavesTheNextOwnersKeys(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "keytwice", 1)
	f.block(t, scopes[0], "keytwice seed")

	h := keyStepHold()
	second := make(chan error, 1)
	go func() { second <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, second)

	if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
		close(h.release)
		<-second
		t.Fatalf("first delete: %v", err)
	}
	// The next tenant with the slug takes the scope name, has a key at home
	// there and a second key, at home elsewhere, that lists the name.
	next, err := store.CreateTenant(ctx, f.pool, "keytwice", "keytwice")
	if err != nil {
		t.Fatalf("tenant with the deleted one's slug: %v", err)
	}
	for _, s := range []string{scopes[0], "keytwice-other"} {
		if _, err := store.AssignTenantScope(ctx, f.pool, next.ID, s, nil); err != nil {
			t.Fatalf("assign %s to the next tenant: %v", s, err)
		}
	}
	home, _, err := store.CreateApiKey(ctx, f.pool, "next home key", scopes[0], []string{scopes[0]}, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	lists, _, err := store.CreateApiKey(ctx, f.pool, "next listing key", "keytwice-other", []string{"keytwice-other", scopes[0]}, next.ID)
	if err != nil {
		t.Fatal(err)
	}

	close(h.release)
	var serr error
	select {
	case serr = <-second:
	case <-time.After(30 * time.Second):
		t.Fatal("the second delete did not end")
	}
	var homeLeft int
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_api_keys WHERE id = $1::uuid`, home.ID).Scan(&homeLeft); err != nil {
		t.Fatal(err)
	}
	var allowed []string
	if err := f.pool.QueryRow(ctx, `SELECT allowed_scopes::text[] FROM context_api_keys WHERE id = $1::uuid`, lists.ID).Scan(&allowed); err != nil {
		t.Fatalf("the next owner's listing key: %v", err)
	}
	t.Logf("second delete: %v; the next owner's home key rows: %d; its other key lists %v", serr, homeLeft, allowed)
	if homeLeft != 1 {
		t.Errorf("the second delete of a deleted tenant deleted the key the next owner has its home in %s with", scopes[0])
	}
	if !slices.Contains(allowed, scopes[0]) {
		t.Errorf("the second delete of a deleted tenant took %s out of the next owner's key: it lists %v", scopes[0], allowed)
	}
	if !errors.Is(serr, store.ErrTenantNotFound) {
		t.Errorf("the second delete, after the first had deleted the tenant: %v, want ErrTenantNotFound", serr)
	}
}

// A scope assigned once the delete has begun is refused: the delete has read
// its scopes and would never prune this one.
func TestAssignRefusesATenantWhoseDeleteHasBegun(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "lateassign", 1)
	f.block(t, scopes[0], "lateassign seed")

	h := keyStepHold()
	done := make(chan error, 1)
	go func() { done <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, done)

	const late = "lateassign-late"
	_, aerr := store.AssignTenantScope(ctx, f.pool, tenantID, late, nil)
	if aerr == nil {
		// What the assign made possible before: a row in the scope.
		f.block(t, late, "a block in the scope assigned during the delete")
	}
	close(h.release)
	var perr error
	select {
	case perr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the delete did not end")
	}
	blocks, _, _, tenant := pruneLeftovers(t, f.pool, tenantID, []string{late})
	t.Logf("assign during the delete: %v; delete: %v; blocks left in %s: %d; tenant row: %d", aerr, perr, late, blocks, tenant)
	if !errors.Is(aerr, store.ErrTenantOffboarding) {
		t.Errorf("assign to a tenant whose delete has begun: %v, want ErrTenantOffboarding", aerr)
	}
	if perr != nil {
		t.Errorf("delete: %v", perr)
	}
	if blocks != 0 {
		t.Errorf("%d block(s) left in %s, a scope no tenant owns after the delete", blocks, late)
	}
}

// A scope that joins the tenant without asking (an older instance during a
// rolling deploy writes the mapping without the status test) is not cascaded
// away with the tenant row: the delete fails, the tenant stays offboarding,
// and the next delete prunes the scope.
func TestTenantDeleteStopsAtAScopeItNeverRead(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "latescope", 1)
	f.block(t, scopes[0], "latescope seed")

	h := newPruneHold(func(sql string) bool {
		return strings.Contains(sql, "DELETE FROM context_projects WHERE tenant_id")
	})
	done := make(chan error, 1)
	go func() { done <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, done)

	const late = "latescope-late"
	// The mapping as an instance before this change writes it.
	if _, err := f.pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, late, tenantID); err != nil {
		t.Fatal(err)
	}
	f.block(t, late, "a block in the scope the delete never read")

	close(h.release)
	var perr error
	select {
	case perr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the delete did not end")
	}
	blocks, _, _, tenant := pruneLeftovers(t, f.pool, tenantID, []string{late})
	t.Logf("delete beside a scope it never read: %v; blocks in %s: %d; tenant row: %d", perr, late, blocks, tenant)
	if !errors.Is(perr, store.ErrTenantPruneIncomplete) {
		t.Errorf("delete: %v, want ErrTenantPruneIncomplete", perr)
	}
	if tenant != 1 {
		t.Fatalf("the tenant row is gone and %d block(s) stand in %s, which no tenant owns", blocks, late)
	}
	if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
		t.Fatalf("the next delete: %v", err)
	}
	blocks, _, _, tenant = pruneLeftovers(t, f.pool, tenantID, []string{late, scopes[0]})
	if blocks != 0 || tenant != 0 {
		t.Errorf("after the next delete: %d blocks, tenant row %d", blocks, tenant)
	}
}

// The tenant's id in capitals names the same tenant; its queued reconciles go
// with it. The queue row is the one the prune's own type delete queues (the
// registry trigger of migration 176).
func TestTenantDeleteByAnIDInCapitalsTakesItsQueuedReconciles(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "capsid", 1)
	f.block(t, scopes[0], "capsid seed")
	if _, err := f.pool.Exec(ctx, `INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
		VALUES ('capsid-x', $1, 'capsid', false, false, '{"retrieval":{"policy":"excluded"}}')`, scopes[0]); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_embed_excluded_queue WHERE grp = 't:' || $1`, tenantID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued == 0 {
		t.Fatal("the fixture queued no reconcile for the tenant")
	}
	upper := strings.ToUpper(tenantID)
	if upper == tenantID {
		t.Fatalf("the tenant id %s has no letter to write in capitals", tenantID)
	}
	if err := store.PruneTenant(ctx, f.pool, upper, store.AuditStamp{}); err != nil {
		t.Fatalf("delete by %s: %v", upper, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_embed_excluded_queue WHERE grp = 't:' || $1`, tenantID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("%d queued reconcile(s) of the deleted tenant left", queued)
	}
}

// A writer without a gate writes one row after every closing round up to the
// tenth and one after each count of what is left from then on, the last after
// the count of round nineteen: round twenty deletes that row and leaves
// nothing. Before, the delete failed there ("0 are left now"); the round after
// it now deletes the tenant.
func TestTenantPruneRowInTheTwentiethRound(t *testing.T) {
	f := newP547Fixture(t)
	tenantID, scopes, _ := pruneWritersTenant(t, f, "twentieth", 1)
	f.block(t, scopes[0], "twentieth seed")
	w := &closingRoundWriter{t: t, pool: f.pool, scope: scopes[0], after: func(round int) bool { return round < 10 }, lateCounts: 10}
	err := closingRoundPrune(t, f, tenantID, w)
	blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
	t.Logf("delete beside a row per round for %d rounds: %v; left %d blocks, %d links, %d other, tenant row %d", w.round, err, blocks, links, other, tenant)
	if w.round < 20 {
		t.Fatalf("only %d closing rounds ran — the fixture did not reach the twentieth", w.round)
	}
	if err != nil {
		t.Fatalf("a delete whose twentieth round took the last row: %v", err)
	}
	if blocks+links+other+tenant != 0 {
		t.Fatalf("left after success: %d blocks, %d links, %d other, tenant row %d", blocks, links, other, tenant)
	}
}

// And the bound holds: a writer that goes on after the count of round twenty
// ends the delete in the round after it.
func TestTenantPruneBoundHoldsAfterTheTwentiethRound(t *testing.T) {
	f := newP547Fixture(t)
	tenantID, scopes, _ := pruneWritersTenant(t, f, "bound", 1)
	f.block(t, scopes[0], "bound seed")
	w := &closingRoundWriter{t: t, pool: f.pool, scope: scopes[0], after: func(round int) bool { return round < 10 }, lateCounts: 1000}
	err := closingRoundPrune(t, f, tenantID, w)
	_, _, _, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
	t.Logf("delete beside a row per round without end: %v after %d rounds; tenant row %d", err, w.round, tenant)
	if !errors.Is(err, store.ErrTenantPruneIncomplete) || tenant != 1 {
		t.Fatalf("delete: %v, tenant row %d; want ErrTenantPruneIncomplete and the tenant kept", err, tenant)
	}
	if w.round != 21 {
		t.Errorf("the delete ran %d closing rounds, want 21", w.round)
	}
}

// An operator sets the tenant's status back to active while its delete runs
// (tenant-update): a server run holds the tenant's gate again and writes.
// The delete's closing round waits for that write — it takes the gate as the
// first step did — and then does not finish over a tenant whose door is open
// again: the tenant row stays, and the next delete takes the run's row.
// Before, the delete answered success beside the open write, which then
// committed into a scope no tenant owned.
func TestTenantDeleteDoesNotFinishOverAStatusSetBack(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "setback", 1)
	f.block(t, scopes[0], "setback seed")

	h := keyStepHold()
	done := make(chan error, 1)
	go func() { done <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, done)
	if _, err := store.UpdateTenant(ctx, f.pool, tenantID, "active", ""); err != nil {
		t.Fatal(err)
	}
	// A run's write: the gate first, then a row in the tenant's scope.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.HoldRunTenant(ctx, tx, tenantID); err != nil {
		t.Fatalf("the run's gate after the status was set back: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'setback run write', 'c', $1)`, scopes[0]); err != nil {
		t.Fatal(err)
	}
	close(h.release)
	select {
	case err := <-done:
		_ = tx.Commit(ctx)
		blocks, _, _, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
		t.Fatalf("the delete ended (%v) beside an open run write of the tenant; after the write committed: %d block(s) in %s, tenant row %d", err, blocks, scopes[0], tenant)
	case <-time.After(2 * time.Second):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the run's commit: %v", err)
	}
	var perr error
	select {
	case perr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the delete did not end after the run's write committed")
	}
	_, _, _, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
	t.Logf("delete over a status set back: %v; tenant row %d", perr, tenant)
	if !errors.Is(perr, store.ErrTenantPruneIncomplete) || !errors.Is(perr, store.ErrTenantStatusSetBack) || tenant != 1 {
		t.Fatalf("delete: %v, tenant row %d; want ErrTenantPruneIncomplete with ErrTenantStatusSetBack and the tenant kept", perr, tenant)
	}
	if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
		t.Fatalf("the next delete: %v", err)
	}
	blocks, _, _, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
	if blocks != 0 || tenant != 0 {
		t.Errorf("after the next delete: %d blocks, tenant row %d", blocks, tenant)
	}
}

// The same for a tenant that owned no scope when its delete began: the list
// the delete read is empty (nil in Go, which must not reach the count as SQL
// NULL — "scope <> ALL(NULL)" is unknown for every row and counted none).
func TestTenantDeleteStopsAtAScopeOfATenantThatOwnedNone(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, f.pool, "nonescope", "nonescope")
	if err != nil {
		t.Fatal(err)
	}
	h := newPruneHold(func(sql string) bool {
		return strings.Contains(sql, "DELETE FROM context_projects WHERE tenant_id")
	})
	done := make(chan error, 1)
	go func() { done <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tn.ID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, done)
	const late = "nonescope-late"
	if _, err := f.pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, late, tn.ID); err != nil {
		t.Fatal(err)
	}
	f.block(t, late, "a block in the only scope, which the delete never read")
	close(h.release)
	var perr error
	select {
	case perr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the delete did not end")
	}
	blocks, _, _, tenant := pruneLeftovers(t, f.pool, tn.ID, []string{late})
	t.Logf("delete of a tenant without a scope beside a scope that joined: %v; blocks in %s: %d; tenant row: %d", perr, late, blocks, tenant)
	if !errors.Is(perr, store.ErrTenantPruneIncomplete) || tenant != 1 {
		t.Fatalf("delete: %v, tenant row %d (%d block(s) in %s); want ErrTenantPruneIncomplete and the tenant kept", perr, tenant, blocks, late)
	}
	if err := store.PruneTenant(ctx, f.pool, tn.ID, store.AuditStamp{}); err != nil {
		t.Fatalf("the next delete: %v", err)
	}
	if blocks, _, _, tenant := pruneLeftovers(t, f.pool, tn.ID, []string{late}); blocks != 0 || tenant != 0 {
		t.Errorf("after the next delete: %d blocks, tenant row %d", blocks, tenant)
	}
}
