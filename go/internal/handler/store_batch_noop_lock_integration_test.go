//go:build integration

// Review of #389 (CodeRabbit, outside the diff): a batch decided which of its
// items change nothing BEFORE its transaction and without a lock, and left
// those items out of its locks and writes. A write another client committed
// between that decision and the batch's commit then stood beside the batch's
// other items: A=a0, B=b0; batch P asks for A=a0, B=b1 and leaves A out; batch
// Q writes A=a1, B=b2 in between; P writes B=b1 — A=a1, B=b1, which neither
// order of the two whole batches produces.
//
// Each test pauses one party on a lock a gate transaction holds, lets the
// other run into it, then releases, and checks the end state against the two
// serial orders. The parties run on their own pools named by
// application_name, so pg_stat_activity tells when one of them waits.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestStoreBatchNoop' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
)

// actor is a two-connection pool whose backends carry name.
func (f *sbFixture) actor(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("actor pool %s: %v", name, err)
	}
	t.Cleanup(p.Close)
	return p
}

// restAsync posts one /api/store/batch body through a handler on pool and
// returns its status and decoded answer on the channel.
type sbRestResult struct {
	status int
	body   map[string]any
}

func (f *sbFixture) restAsync(t *testing.T, pool *pgxpool.Pool, k sbKey, body any) chan sbRestResult {
	t.Helper()
	done := make(chan sbRestResult, 1)
	on := &sbFixture{pool: pool, cfg: f.cfg, reg: f.reg}
	go func() {
		status, resp := on.restNoFatal(k, body)
		done <- sbRestResult{status, resp}
	}()
	return done
}

// waitLockOrDone returns true once the backend named name waits for a lock,
// false when done delivers first (the result is put back).
func (f *sbFixture) waitLockOrDone(t *testing.T, name string, done chan sbRestResult) bool {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; {
		select {
		case r := <-done:
			done <- r
			return false
		default:
		}
		var waiting int
		if err := f.pool.QueryRow(context.Background(),
			`SELECT count(*)::int FROM pg_stat_activity
			  WHERE application_name = $1 AND datname = current_database() AND wait_event_type = 'Lock'`,
			name).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return true
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s neither waited for a lock nor finished", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sbAwait[T any](t *testing.T, name string, done <-chan T) T {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish", name)
		var zero T
		return zero
	}
}

// sbStoreItem is a store.BatchUpsert of the fixture's category and scope.
func sbStoreItem(title, content string) store.BatchUpsert {
	return store.BatchUpsert{Category: "learnings", Title: title, Content: content, Scope: "private",
		Sens: store.SensitivityWrite{Value: backends.SensInternal}}
}

// TestStoreBatchNoopDecidedUnderTheLock: the no-op decision of a batch item is
// made on its row while the batch's transaction holds it, and the row stays
// held to the commit.
func TestStoreBatchNoopDecidedUnderTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()

	// The finding's interleaving with two batches. Q writes A=a1, B=b2 and is
	// held on its third block D by the gate, holding A and B. P asks for
	// A=a0 (what A holds before Q commits) and B=b1. Released, Q commits,
	// then P: the end state is one of the two serial orders.
	t.Run("two batches", func(t *testing.T) {
		a, b := f.seed(t, "sb-nl-a", "a0"), f.seed(t, "sb-nl-b", "b0")
		d := f.seed(t, "sb-nl-d", "d0")
		gate, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = gate.Rollback(ctx) }()
		if _, err := gate.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, d); err != nil {
			t.Fatal(err)
		}
		qPool := f.actor(t, "sb-nl-q")
		qDone := make(chan error, 1)
		go func() {
			_, err := store.UpsertBlocks(ctx, qPool, []store.BatchUpsert{
				sbStoreItem("sb-nl-a", "a1"), sbStoreItem("sb-nl-b", "b2"), sbStoreItem("sb-nl-d", "d1"),
			}, "sb-nl-q", nil, nil)
			qDone <- err
		}()
		qWait := make(chan sbRestResult, 1)
		go func() {
			if err := <-qDone; err != nil {
				qWait <- sbRestResult{status: 500, body: map[string]any{"error": err.Error()}}
				return
			}
			qWait <- sbRestResult{status: 200}
		}()
		if !f.waitLockOrDone(t, "sb-nl-q", qWait) {
			t.Fatalf("Q finished without waiting for the gate: %+v", <-qWait)
		}

		k := f.key(t, false)
		pDone := f.restAsync(t, f.actor(t, "sb-nl-p"), k, sbItems(sbItem("sb-nl-a", "a0"), sbItem("sb-nl-b", "b1")))
		pWaited := f.waitLockOrDone(t, "sb-nl-p", pDone)
		if err := gate.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if q := sbAwait(t, "Q", qWait); q.status != 200 {
			t.Fatalf("Q: %v", q.body)
		}
		p := sbAwait(t, "P", pDone)
		if p.status != http.StatusOK {
			t.Fatalf("P: status %d body %v", p.status, p.body)
		}
		gotA, gotB := f.content(t, a), f.content(t, b)
		t.Logf("P waited for a lock: %v; end state A=%s B=%s", pWaited, gotA, gotB)
		if (gotA != "a0" || gotB != "b1") && (gotA != "a1" || gotB != "b2") {
			t.Fatalf("end state A=%s B=%s; want A=a0 B=b1 (Q, then P) or A=a1 B=b2 (P, then Q) — P answered %v",
				gotA, gotB, p.body["items"])
		}
	})

	// A single-row writer (a manage update, a single store) holds A and
	// changes it while the batch arrives; it commits while the batch waits
	// for A. The batch must compare A as that writer committed it — not as
	// it read it before — so A=a0 is written back, beside B=b1.
	t.Run("a single write commits while the batch waits for the row", func(t *testing.T) {
		a, b := f.seed(t, "sb-ns-a", "a0"), f.seed(t, "sb-ns-b", "b0")
		gate, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = gate.Rollback(ctx) }()
		if _, err := gate.Exec(ctx, `UPDATE context_blocks SET content = 'a1' WHERE id = $1`, a); err != nil {
			t.Fatal(err)
		}
		k := f.key(t, false)
		pDone := f.restAsync(t, f.actor(t, "sb-ns-p"), k, sbItems(sbItem("sb-ns-a", "a0"), sbItem("sb-ns-b", "b1")))
		pWaited := f.waitLockOrDone(t, "sb-ns-p", pDone)
		if err := gate.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		p := sbAwait(t, "P", pDone)
		if p.status != http.StatusOK {
			t.Fatalf("P: status %d body %v", p.status, p.body)
		}
		gotA, gotB := f.content(t, a), f.content(t, b)
		t.Logf("P waited for a lock: %v; end state A=%s B=%s", pWaited, gotA, gotB)
		if gotA != "a0" || gotB != "b1" {
			t.Fatalf("end state A=%s B=%s; want A=a0 B=b1 — P committed after the single write, so P's A stands — P answered %v",
				gotA, gotB, p.body["items"])
		}
	})

	// The row of an item the batch leaves as it is stays locked until the
	// batch commits: while the batch waits for its next block, nobody else
	// can take the unchanged one.
	t.Run("the row of a no-op item is held to the commit", func(t *testing.T) {
		a := f.seed(t, "sb-nh-a", "a0")
		d := f.seed(t, "sb-nh-d", "d0")
		gate, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = gate.Rollback(ctx) }()
		if _, err := gate.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, d); err != nil {
			t.Fatal(err)
		}
		k := f.key(t, false)
		pDone := f.restAsync(t, f.actor(t, "sb-nh-p"), k, sbItems(sbItem("sb-nh-a", "a0"), sbItem("sb-nh-d", "d1")))
		if !f.waitLockOrDone(t, "sb-nh-p", pDone) {
			t.Fatalf("P finished without waiting for the gate: %+v", <-pDone)
		}
		_, probeErr := f.pool.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE NOWAIT`, a)
		if err := gate.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		p := sbAwait(t, "P", pDone)
		var pgErr *pgconn.PgError
		if !errors.As(probeErr, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("lock probe on the unchanged block while the batch waits: %v; want 55P03 (held by the batch)", probeErr)
		}
		items, _ := p.body["items"].([]any)
		if p.status != http.StatusOK || len(items) != 2 {
			t.Fatalf("P: status %d body %v", p.status, p.body)
		}
		if first, _ := items[0].(map[string]any); first["action"] != "noop" || first["existing_id"] != a {
			t.Fatalf("items[0] = %v; want the noop answer for %s", first, a)
		}
	})
}

// TestStoreBatchNoopBudgetAndFields: what an item that changes nothing costs
// (nothing, decided on its locked row) and what makes an item a write (every
// part the single store compares).
func TestStoreBatchNoopBudgetAndFields(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	same := f.seed(t, "sb-nb-same", "same body")
	same2 := f.seed(t, "sb-nb-same2", "same body 2")

	t.Run("the budget counts the items that write", func(t *testing.T) {
		k := f.key(t, false)
		for i := 0; i < sbRateLimit-1; i++ {
			if a := f.mcpCall(t, k, "store", sbItem(fmt.Sprintf("sb-nb-single-%d", i), "x")); a.IsError {
				t.Fatalf("single store %d: %+v", i, a)
			}
		}
		status, resp := f.rest(t, k, sbItems(sbItem("sb-nb-same", "same body"), sbItem("sb-nb-new", "new")))
		if status != http.StatusOK {
			t.Fatalf("%d used + one writing item and one unchanged: status %d body %v; want written", sbRateLimit-1, status, resp)
		}
		if got := f.counts(t, k); got.budget != sbRateLimit {
			t.Fatalf("budget %d; want %d — the unchanged item booked nothing", got.budget, sbRateLimit)
		}
		before := f.counts(t, k)
		status, resp = f.rest(t, k, sbItems(sbItem("sb-nb-same", "same body"), sbItem("sb-nb-same2", "same body 2")))
		if status != http.StatusOK {
			t.Fatalf("window spent, every item unchanged: status %d body %v; want the noop answers", status, resp)
		}
		items, _ := resp["items"].([]any)
		for i, want := range []string{same, same2} {
			if it, _ := items[i].(map[string]any); it["action"] != "noop" || it["existing_id"] != want {
				t.Errorf("items[%d] = %v; want noop on %s", i, it, want)
			}
		}
		if after := f.counts(t, k); after != before {
			t.Fatalf("an all-unchanged batch changed the store: before %+v after %+v", before, after)
		}
	})

	// Identical content, one other part each: every such item writes, and the
	// answer names the part — the single store's "changed".
	t.Run("each compared part makes an item a write", func(t *testing.T) {
		for _, c := range []struct {
			field string
			extra []any
		}{
			{"tags", []any{"tags", []string{"sb-nb"}}},
			{"metadata", []any{"metadata", map[string]any{"source": "sb-nb"}}},
			{"sensitivity", []any{"sensitivity", "personal"}},
			{"type", []any{"type", "reference"}},
		} {
			t.Run(c.field, func(t *testing.T) {
				title := "sb-nb-field-" + c.field
				id := f.seed(t, title, "field body")
				k := f.key(t, false)
				status, resp := f.rest(t, k, sbItems(sbItem("sb-nb-same", "same body"), sbItem(title, "field body", c.extra...)))
				items, _ := resp["items"].([]any)
				if status != http.StatusOK || len(items) != 2 {
					t.Fatalf("status %d body %v", status, resp)
				}
				it, _ := items[1].(map[string]any)
				block, _ := it["block"].(map[string]any)
				changed := fmt.Sprint(it["changed"])
				if it["action"] != "updated" || block["id"] != id || changed != "["+c.field+"]" {
					t.Fatalf("items[1] = %v; want written, action updated, changed [%s]", it, c.field)
				}
				if got := f.counts(t, k); got.budget != 1 {
					t.Fatalf("budget %d; want 1", got.budget)
				}
			})
		}
	})
}

// TestStoreBatchNoopStaged: a staged batch's card holds every item, the ones
// unchanged at stage time included, and the confirm decides each on its
// locked row. With the unchanged item left off the card, a write between
// stage and confirm stood beside the confirmed rest.
func TestStoreBatchNoopStaged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()

	t.Run("a block changed between stage and confirm", func(t *testing.T) {
		a, b := f.seed(t, "sb-ng-a", "a0"), f.seed(t, "sb-ng-b", "b0")
		k := f.key(t, true)
		staged := f.mcpCall(t, k, "store_batch", sbItems(sbItem("sb-ng-a", "a0"), sbItem("sb-ng-b", "b1")))
		m := sbHashRe.FindStringSubmatch(staged.Text)
		if !staged.IsError || m == nil {
			t.Fatalf("stage: %+v", staged)
		}
		if !strings.Contains(staged.Text, "2 items staged as one batch.") ||
			!strings.Contains(staged.Text, "items[0]: identical content exists now (id: "+a+")") {
			t.Errorf("stage: %+v; want both items on the card, items[0] named as identical now", staged)
		}
		if got := f.counts(t, k); got.budget != 2 {
			t.Errorf("stage charged %d; want 2 — one intent per item of the card", got.budget)
		}
		// Another writer changes A between the stage and the confirm.
		if _, err := store.UpsertBlock(ctx, f.pool, "learnings", "sb-ng-a", "a1", nil, nil, "private", false,
			store.SensitivityWrite{Value: backends.SensInternal}, ""); err != nil {
			t.Fatal(err)
		}
		c := f.mcpCall(t, k, "confirm", map[string]any{"payload_hash": m[1]})
		if c.IsError || !strings.HasPrefix(c.Text, "Confirmed and stored 2 blocks in one transaction:\n") {
			t.Errorf("confirm: %+v", c)
		}
		if gotA, gotB := f.content(t, a), f.content(t, b); gotA != "a0" || gotB != "b1" {
			t.Fatalf("end state A=%s B=%s; want A=a0 B=b1 — the confirmed batch commits after the other write", gotA, gotB)
		}
	})

	t.Run("an item still unchanged at the confirm is left as it is", func(t *testing.T) {
		a := f.seed(t, "sb-nu-a", "a0")
		k := f.key(t, true)
		staged := f.mcpCall(t, k, "store_batch", sbItems(sbItem("sb-nu-a", "a0"), sbItem("sb-nu-b", "b1")))
		m := sbHashRe.FindStringSubmatch(staged.Text)
		if m == nil {
			t.Fatalf("stage: %+v", staged)
		}
		c := f.mcpCall(t, k, "confirm", map[string]any{"payload_hash": m[1]})
		if c.IsError || !strings.HasPrefix(c.Text, "Confirmed and stored 1 blocks in one transaction, 1 unchanged:\n"+
			"items[0]: No change needed — identical content already exists (id: "+a+")\nitems[1]: sb-nu-b (id: ") {
			t.Fatalf("confirm: %+v", c)
		}
	})

	t.Run("a batch unchanged at stage time stages nothing", func(t *testing.T) {
		a := f.seed(t, "sb-nz-a", "a0")
		k := f.key(t, true)
		before := f.counts(t, k)
		staged := f.mcpCall(t, k, "store_batch", sbItems(sbItem("sb-nz-a", "a0")))
		if staged.IsError || staged.Text != "Batch written in one transaction: 0 stored, 1 unchanged.\n"+
			"items[0]: No change needed — identical content already exists (id: "+a+")\n" {
			t.Fatalf("stage: %+v", staged)
		}
		if after := f.counts(t, k); after != before {
			t.Fatalf("an all-unchanged stage changed the store: before %+v after %+v", before, after)
		}
	})
}

// sbBeforeRead is a query tracer that runs between once, right before the
// stage's identical-content read of the item titled title: a write another
// client commits between two item reads of one stage.
type sbBeforeRead struct {
	title   string
	between func()
	ran     atomic.Bool
}

func (h *sbBeforeRead) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "content_hash = encode(digest($1, 'sha256'), 'hex')") && strings.Contains(d.SQL, "LIMIT 1") &&
		len(d.Args) > 3 && d.Args[3] == h.title && h.ran.CompareAndSwap(false, true) {
		h.between()
	}
	return ctx
}

func (h *sbBeforeRead) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestStoreBatchStageUnchangedOnOneSnapshot (review of #389): a staged batch of
// which every item is unchanged stages nothing and is answered at once, and
// that answer must describe one moment of the store. Read item by item, each
// read its own snapshot, a batch Q committed between two of the reads made
// item 0 match before Q and item 1 after it: A=a1, B=b0; Q writes A=a0, B=b1;
// the stage asks for A=a1, B=b1 — a pair the store never held — and was
// answered "No change needed" with no card.
func TestStoreBatchStageUnchangedOnOneSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	a := f.seed(t, "sb-os-a", "a1")
	f.seed(t, "sb-os-b", "b0")
	var qErr error
	hook := &sbBeforeRead{title: "sb-os-b", between: func() {
		_, qErr = store.UpsertBlocks(ctx, f.pool, []store.BatchUpsert{
			sbStoreItem("sb-os-a", "a0"), sbStoreItem("sb-os-b", "b1"),
		}, "sb-os-q", nil, nil)
	}}
	cfg, err := pgxpool.ParseConfig(f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = hook
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	on := &sbFixture{pool: traced, cfg: f.cfg, reg: f.reg,
		mcp: NewMCPHandler(MCPConfig{Pool: traced, Cfg: f.cfg, Blocktypes: f.reg})}

	k := f.key(t, true)
	staged := on.mcpCall(t, k, "store_batch", sbItems(sbItem("sb-os-a", "a1"), sbItem("sb-os-b", "b1")))
	if !hook.ran.Load() || qErr != nil {
		t.Fatalf("Q between the two reads: ran %v, error %v", hook.ran.Load(), qErr)
	}
	if !staged.IsError || !strings.HasPrefix(staged.Text, "STAGED — NOT saved yet.") ||
		!strings.Contains(staged.Text, "items[0]: identical content exists now (id: "+a+")") {
		t.Fatalf("stage across Q's commit: %+v; want a card, items[0] named identical — "+
			"A matched before Q and B after it, never both at once", staged)
	}
}

// restNoFatal is rest without t (it runs in a goroutine).
func (f *sbFixture) restNoFatal(k sbKey, body any) (int, map[string]any) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/store/batch", bytes.NewReader(raw)).WithContext(k.ctx)
	rec := httptest.NewRecorder()
	NewStoreHandler(f.pool, f.cfg, f.reg).HandleStoreBatch(rec, req)
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		return rec.Code, map[string]any{"decode": err.Error(), "raw": rec.Body.String()}
	}
	return rec.Code, m
}
