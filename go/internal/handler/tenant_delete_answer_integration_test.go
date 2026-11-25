//go:build integration

// Issue #555, point 3: a tenant-delete that stops part-way says so, and a
// client that gives up does not stop it.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestTenantDeleteFailureNamesTheStateAndTheWay: a delete that fails after
// its first step (here: its key step refused by a trigger) answers what holds
// — the tenant is offboarding, its keys refused, part of it deleted — and the
// way on, run the delete again; before, the answer was "delete tenant failed".
func TestTenantDeleteFailureNamesTheStateAndTheWay(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, pool)
	h := mcHandler(t, pool, false, false)
	tn := auMakeTenant(ctx, t, pool, h, admin, "i555-fail")
	if _, err := store.UpsertBlock(ctx, pool, "learnings", "i555 fail block", "content", nil, nil, tn.scope, true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE FUNCTION i555_refuse_key_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'i555 probe fault'; END $$`,
		`CREATE TRIGGER i555_refuse_key_delete BEFORE DELETE ON context_api_keys FOR EACH ROW EXECUTE FUNCTION i555_refuse_key_delete()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("fault fixture: %v", err)
		}
	}
	status, resp := kaManage(t, h, admin, "0555-f0", map[string]any{"action": "tenant-delete", "id": tn.id})
	var state string
	if err := pool.QueryRow(ctx, `SELECT status FROM context_tenants WHERE id = $1::uuid`, tn.id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	msg, _ := resp["error"].(string)
	t.Logf("answer %d %q; tenant status %s", status, msg, state)
	if status != http.StatusInternalServerError || state != "offboarding" {
		t.Fatalf("answer %d, tenant %s: want 500 and offboarding", status, state)
	}
	if !strings.Contains(msg, "offboarding") || !strings.Contains(msg, "run tenant-delete again") {
		t.Errorf("the answer of a delete that stopped part-way: %q — want the tenant's state (offboarding, its keys refused) and the way on (run it again)", msg)
	}
}

// statementHold stops the first statement match accepts until release.
type statementHold struct {
	match   func(sql string) bool
	once    sync.Once
	held    chan struct{}
	release chan struct{}
}

func (s *statementHold) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if s.match(d.SQL) {
		s.once.Do(func() {
			close(s.held)
			<-s.release
		})
	}
	return ctx
}

func (s *statementHold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestTenantDeleteRunsOnWhenItsClientLeaves: the client of a tenant-delete
// gives up while the delete works (its request context ends, as when the CLI's
// timeout or a proxy closes the connection). Before, the delete stopped at its
// next statement and left the tenant offboarding and half deleted; now it runs
// to its end.
func TestTenantDeleteRunsOnWhenItsClientLeaves(t *testing.T) {
	plain, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, plain)
	setup := mcHandler(t, plain, false, false)
	tn := auMakeTenant(ctx, t, plain, setup, admin, "i555-left")
	for i := range 5 {
		if _, err := store.UpsertBlock(ctx, plain, "learnings", fmt.Sprintf("i555 left %d", i), "content", nil, nil, tn.scope, true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatal(err)
		}
	}

	hold := &statementHold{match: func(sql string) bool { return strings.Contains(sql, "DELETE FROM context_dream_links WHERE ctid IN") },
		held: make(chan struct{}), release: make(chan struct{})}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = hold
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	h := mcHandler(t, traced, false, false)

	body, _ := json.Marshal(map[string]any{"action": "tenant-delete", "id": tn.id})
	reqCtx, cancel := context.WithCancel(context.WithValue(context.WithValue(ctx, authResultKey, admin), requestIDKey, "0555-f1"))
	req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(string(body))).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.HandleManage(rec, req); close(done) }()
	select {
	case <-hold.held:
	case <-done:
		t.Fatalf("the delete answered before its first batch: %d %s", rec.Code, rec.Body.String())
	case <-time.After(20 * time.Second):
		t.Fatal("the delete did not reach its first batch")
	}
	cancel() // the client gives up
	close(hold.release)
	<-done

	var tenants, blocks int
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := plain.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_tenants WHERE id = $1::uuid)::int, (SELECT count(*) FROM context_blocks WHERE scope = $2)::int`, tn.id, tn.scope).Scan(&tenants, &blocks); err != nil {
			t.Fatal(err)
		}
		if tenants == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("after the client left: answer %d %.200q; tenant rows %d, blocks in its scope %d", rec.Code, rec.Body.String(), tenants, blocks)
	if tenants != 0 || blocks != 0 {
		t.Errorf("a delete whose client left stopped part-way: tenant rows %d, blocks %d — want it run to its end", tenants, blocks)
	}
}
