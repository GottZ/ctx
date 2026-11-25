//go:build integration

// Issue #555, point 2: a second tenant-delete of the same tenant beside the
// first. Both read the tenant's scope names at their start; once the first
// has deleted the tenant row, a tenant created with the same slug can be
// assigned one of those names and write there — and the second delete's
// statements, keyed by the names it read, deleted what that tenant wrote
// and answered success. Every scope-keyed statement of the prune now names
// only the scopes the pruned tenant still owns (context_tenant_scopes), so
// the second delete takes nothing of the next owner and answers that the
// tenant is gone.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

func TestSecondTenantDeleteLeavesTheNextOwnersRows(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "twice", 2)
	for _, s := range scopes {
		f.block(t, s, "twice seed "+s)
	}
	if _, err := store.UpsertBlob(ctx, f.pool, "files", "twice blob", "f.txt", "text/plain", scopes[0], []byte("x"), nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	// The second delete stands right before its first scope lock: it has
	// read the tenant's scope names and holds nothing.
	h := newPruneHold(func(sql string) bool {
		return strings.Contains(sql, "pg_advisory_xact_lock(hashtext('ctx:supersedes:'")
	})
	second := make(chan error, 1)
	go func() { second <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, second)

	if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
		close(h.release)
		<-second
		t.Fatalf("first delete: %v", err)
	}
	// The next tenant with the slug takes a scope name of the deleted one
	// and writes there: a block, a blob, a secret.
	next, err := store.CreateTenant(ctx, f.pool, "twice", "twice")
	if err != nil {
		t.Fatalf("tenant with the deleted one's slug: %v", err)
	}
	if _, err := store.AssignTenantScope(ctx, f.pool, next.ID, scopes[0], nil); err != nil {
		t.Fatalf("assign %s to the next tenant: %v", scopes[0], err)
	}
	f.block(t, scopes[0], "the next owner's block")
	if _, err := store.UpsertBlob(ctx, f.pool, "files", "next blob", "n.txt", "text/plain", scopes[0], []byte("y"), nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	close(h.release)
	var serr error
	select {
	case serr = <-second:
	case <-time.After(30 * time.Second):
		t.Fatal("the second delete did not end")
	}
	blocks, _, other, _ := pruneLeftovers(t, f.pool, next.ID, scopes[:1])
	t.Logf("second delete: %v; the next owner's rows in %s after it: %d blocks, %d other rows", serr, scopes[0], blocks, other)
	if blocks != 1 || other != 1 {
		t.Errorf("the second delete of a deleted tenant took the next owner's rows: %d blocks and %d other rows left of 1 and 1", blocks, other)
	}
	if !errors.Is(serr, store.ErrTenantNotFound) {
		t.Errorf("the second delete, after the first had deleted the tenant: %v, want ErrTenantNotFound", serr)
	}
}

// closingRoundWriter is the prune pool's tracer of the closing-round tests: a
// writer that holds no gate (an older instance beside this one) writes one
// block into scope right after the prune's last pass statement (the projects)
// and after the commit of every closing round for which after says so, and
// one more right after the count the closing rounds run once they are
// pruneClosingRounds deep, when late is set.
type closingRoundWriter struct {
	t       *testing.T
	pool    *pgxpool.Pool
	scope   string
	after   func(round int) bool
	late    bool
	mu      sync.Mutex
	closing bool
	round   int
	wrote   int
	counted bool
}

type closingRoundMark struct{}

func (w *closingRoundWriter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.Contains(d.SQL, "DELETE FROM context_projects WHERE tenant_id"):
		return context.WithValue(ctx, closingRoundMark{}, "projects")
	case d.SQL == "commit":
		return context.WithValue(ctx, closingRoundMark{}, "commit")
	case strings.Contains(d.SQL, "SELECT count(*) FROM context_dream_links"):
		return context.WithValue(ctx, closingRoundMark{}, "count")
	}
	return ctx
}

func (w *closingRoundWriter) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	what, _ := ctx.Value(closingRoundMark{}).(string)
	w.mu.Lock()
	write := false
	switch what {
	case "projects":
		w.closing, write = true, true
	case "commit":
		if w.closing {
			w.round++
			write = w.after(w.round)
		}
	case "count":
		if w.late && !w.counted {
			w.counted, write = true, true
		}
	}
	if write {
		w.wrote++
	}
	n := w.wrote
	w.mu.Unlock()
	if write {
		if _, err := store.UpsertBlock(context.Background(), w.pool, "learnings", fmt.Sprintf("closing round writer %d", n), "no gate", nil, nil, w.scope, true, store.SensitivityWrite{}, ""); err != nil {
			w.t.Errorf("closing round writer: %v", err)
		}
	}
}

// closingRoundPrune runs the tenant's prune on a pool traced by w.
func closingRoundPrune(t *testing.T, f *p547Fixture, tenantID string, w *closingRoundWriter) error {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = w
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.PruneTenant(context.Background(), pool, tenantID, store.AuditStamp{})
}

// TestTenantPruneBacklogDrainedInTheLastRound (point 3): what a writer
// without a gate writes after the passes is the closing rounds'. Here every
// round of the ten finds a row (one written after the round before
// committed), and the tenth leaves nothing: before, its row made the delete
// fail with "rows kept arriving" although none were left; now the delete
// counts what is left first and finishes.
func TestTenantPruneBacklogDrainedInTheLastRound(t *testing.T) {
	f := newP547Fixture(t)
	tenantID, scopes, _ := pruneWritersTenant(t, f, "backlog", 1)
	f.block(t, scopes[0], "backlog seed")
	w := &closingRoundWriter{t: t, pool: f.pool, scope: scopes[0], after: func(round int) bool { return round < 10 }}
	err := closingRoundPrune(t, f, tenantID, w)
	blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
	t.Logf("delete beside %d rows written after the passes, one per closing round: %v; left %d blocks, %d links, %d other, tenant row %d", w.wrote, err, blocks, links, other, tenant)
	if w.round < 10 {
		t.Fatalf("only %d closing rounds ran — the fixture did not reach the tenth", w.round)
	}
	if err != nil {
		t.Fatalf("a backlog the closing rounds drained: %v", err)
	}
	if blocks+links+other+tenant != 0 {
		t.Fatalf("left after success: %d blocks, %d links, %d other, tenant row %d", blocks, links, other, tenant)
	}
}

// TestTenantPruneRowInTheEleventhRound (review round, point 6b): the same, and
// one more row written after the tenth round's count found nothing left — so
// the eleventh round deletes a row and nothing is left after it. Before, the
// delete failed then ("rows kept arriving … 0 are left now"); now a round
// that leaves nothing is followed by one more, which deletes the tenant.
func TestTenantPruneRowInTheEleventhRound(t *testing.T) {
	f := newP547Fixture(t)
	tenantID, scopes, _ := pruneWritersTenant(t, f, "eleventh", 1)
	f.block(t, scopes[0], "eleventh seed")
	w := &closingRoundWriter{t: t, pool: f.pool, scope: scopes[0], after: func(round int) bool { return round < 10 }, late: true}
	err := closingRoundPrune(t, f, tenantID, w)
	blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
	if !w.counted {
		t.Fatal("the closing rounds never counted — the fixture did not reach the tenth round")
	}
	if err != nil {
		t.Fatalf("a delete whose eleventh round took the last row: %v (left %d blocks, %d links, %d other, tenant %d)", err, blocks, links, other, tenant)
	}
	if blocks+links+other+tenant != 0 {
		t.Fatalf("left after success: %d blocks, %d links, %d other, tenant row %d", blocks, links, other, tenant)
	}
}
