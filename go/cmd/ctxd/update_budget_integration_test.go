//go:build integration

// Issue #519: a single update was never booked on the write budget
// (query.rate_limit_write) — not over REST (manage update), not over MCP, not
// staged —, while an update item of a block batch was. An update that changes
// a row is a write and is booked like a store, on every surface: in the
// transaction that writes it (no booking without the write, no write without
// the booking), under the key's budget lock (two updates at the edge of the
// budget get exactly what is left), and refused at the budget like a store
// (429, code rate_limit) with nothing written. An update that changes nothing
// costs nothing. A staged update books its intent at stage time, as every
// staged write does; the confirm books nothing.
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

const updateBudgetLimit = 5

type updateBudgetFixture struct {
	*nulSweepFixture
	t *testing.T
}

func (f updateBudgetFixture) key(label string, confirm bool) string {
	ctx := context.Background()
	row, plain, err := store.CreateApiKey(ctx, f.pool, label, "private", nil, store.DefaultTenantID)
	if err != nil {
		f.t.Fatalf("key %s: %v", label, err)
	}
	if confirm {
		if _, err := f.pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
			f.t.Fatalf("confirm_writes: %v", err)
		}
	}
	f.keys[label] = plain
	f.keys[label+"#id"] = row.ID
	return label
}

// bookings counts the key's 'write' rows of the window, and those of them
// that name a block.
func (f updateBudgetFixture) bookings(k string) (all, withBlock int) {
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*), count(block_id) FROM context_access_log WHERE api_key_id = $1 AND action = 'write'`,
		f.keys[k+"#id"]).Scan(&all, &withBlock); err != nil {
		f.t.Fatalf("bookings: %v", err)
	}
	return all, withBlock
}

// seed writes a block in the keys' home scope through the store's write
// path (store.UpsertBlock, as the REST store runs it), outside every key's
// budget.
func (f updateBudgetFixture) seed(title string) string {
	b, err := store.UpsertBlock(context.Background(), f.pool, "learnings", title, "body of "+title, nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		f.t.Fatalf("seed %s: %v", title, err)
	}
	return b.ID
}

func (f updateBudgetFixture) title(id string) string {
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT title FROM context_blocks WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("title: %v", err)
	}
	return s
}

func (f updateBudgetFixture) restUpdate(k, id string, data map[string]any) nulSweepResult {
	return f.do(f.t, nulSweepCase{method: http.MethodPost, url: "/api/manage", cred: k,
		body: nulSweepJSON(map[string]any{"action": "update", "id": id, "data": data})}, false)
}

func (f updateBudgetFixture) mcpUpdate(k, id string, args map[string]any) nulSweepResult {
	a := map[string]any{"id": id}
	for x, v := range args {
		a[x] = v
	}
	return f.do(f.t, nulSweepCase{method: http.MethodPost, url: "/mcp", cred: k, mcp: true, body: nulSweepMCP("update", a)}, false)
}

func TestSingleUpdateBooksWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := updateBudgetFixture{nulSweepRouter(t, map[string]string{"CTX_RATE_LIMIT_WRITE": fmt.Sprint(updateBudgetLimit)}), t}
	check := func(what string, k string, wantAll, wantBlock int) {
		t.Helper()
		if all, wb := f.bookings(k); all != wantAll || wb != wantBlock {
			t.Errorf("%s: key %s has %d bookings (%d naming a block), want %d (%d)", what, k, all, wb, wantAll, wantBlock)
		}
	}

	t.Run("each surface books a changing update once and an unchanged one not at all", func(t *testing.T) {
		k := f.key("ub-surfaces", false)
		a, b := f.seed("ub surface a"), f.seed("ub surface b")
		if r := f.restUpdate(k, a, map[string]any{"title": "ub surface a2"}); r.status != http.StatusOK || !strings.Contains(r.answer, `"success":true`) {
			t.Fatalf("REST update: %d %s", r.status, r.answer)
		}
		check("REST update", k, 1, 1)
		if r := f.restUpdate(k, a, map[string]any{"title": "ub surface a2"}); r.status != http.StatusOK {
			t.Fatalf("REST unchanged update: %d %s", r.status, r.answer)
		}
		check("REST unchanged update", k, 1, 1)
		if r := f.mcpUpdate(k, b, map[string]any{"content": "new body"}); r.isError {
			t.Fatalf("MCP update: %s", r.answer)
		}
		check("MCP update", k, 2, 2)
		if r := f.mcpUpdate(k, b, map[string]any{"content": "new body"}); r.isError {
			t.Fatalf("MCP unchanged update: %s", r.answer)
		}
		check("MCP unchanged update", k, 2, 2)
		// An update the identity check refuses rolls back with its booking.
		if r := f.restUpdate(k, b, map[string]any{"title": "ub surface a2"}); r.status != http.StatusConflict {
			t.Fatalf("identity clash: %d %s", r.status, r.answer)
		}
		check("refused update", k, 2, 2)
	})

	t.Run("a staged update books its intent at stage time and the confirm books nothing", func(t *testing.T) {
		k := f.key("ub-staged", true)
		x := f.seed("ub staged x")
		r := f.mcpUpdate(k, x, map[string]any{"title": "ub staged x2"})
		i := strings.Index(r.answer, "payload_hash: ")
		if i < 0 {
			t.Fatalf("stage: %s", r.answer)
		}
		hash := strings.Fields(r.answer[i+len("payload_hash: "):])[0]
		check("stage", k, 1, 0)
		c := f.do(t, nulSweepCase{method: http.MethodPost, url: "/mcp", cred: k, mcp: true, body: nulSweepMCP("confirm", map[string]any{"payload_hash": hash})}, false)
		if c.isError || f.title(x) != "ub staged x2" {
			t.Fatalf("confirm: %s (title %q)", c.answer, f.title(x))
		}
		check("confirm", k, 1, 0)
	})

	t.Run("at the budget an update writes nothing and answers like a store", func(t *testing.T) {
		k := f.key("ub-edge", false)
		ids := make([]string, updateBudgetLimit+1)
		for i := range ids {
			ids[i] = f.seed(fmt.Sprintf("ub edge %d", i))
		}
		for i := range updateBudgetLimit {
			if r := f.restUpdate(k, ids[i], map[string]any{"content": "spent"}); r.status != http.StatusOK {
				t.Fatalf("spend %d: %d %s", i, r.status, r.answer)
			}
		}
		check("spent", k, updateBudgetLimit, updateBudgetLimit)
		last := ids[updateBudgetLimit]
		r := f.restUpdate(k, last, map[string]any{"title": "ub edge over"})
		if r.status != http.StatusTooManyRequests || r.code != "rate_limit" || !strings.Contains(r.answer, fmt.Sprintf("max %d writes per 60 seconds", updateBudgetLimit)) {
			t.Errorf("REST at the budget: %d %s", r.status, r.answer)
		}
		m := f.mcpUpdate(k, last, map[string]any{"title": "ub edge over"})
		if !m.isError || m.code != "rate_limit" {
			t.Errorf("MCP at the budget: isError=%v code=%q %s", m.isError, m.code, m.answer)
		}
		if got := f.title(last); got != "ub edge 5" {
			t.Errorf("a refused update wrote: title %q", got)
		}
		check("refused at the budget", k, updateBudgetLimit, updateBudgetLimit)
		// An update that changes nothing costs nothing, so the budget does
		// not refuse it.
		if r := f.restUpdate(k, ids[0], map[string]any{"content": "spent"}); r.status != http.StatusOK {
			t.Errorf("an unchanged update at the budget: %d %s", r.status, r.answer)
		}
		check("unchanged at the budget", k, updateBudgetLimit, updateBudgetLimit)
	})

	t.Run("a staged update at the budget gets no card", func(t *testing.T) {
		k := f.key("ub-staged-edge", true)
		x := f.seed("ub staged edge")
		for i := range updateBudgetLimit {
			if r := f.mcpUpdate(k, x, map[string]any{"title": fmt.Sprintf("ub staged edge %d", i)}); !strings.Contains(r.answer, "STAGED") {
				t.Fatalf("stage %d: %s", i, r.answer)
			}
		}
		r := f.mcpUpdate(k, x, map[string]any{"title": "ub staged edge over"})
		if !r.isError || strings.Contains(r.answer, "STAGED") || !strings.Contains(r.answer, "Rate limit exceeded") {
			t.Errorf("a stage over the budget: %s", r.answer)
		}
		var cards int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1`, f.keys[k+"#id"]).Scan(&cards); err != nil {
			t.Fatalf("cards: %v", err)
		}
		if cards != updateBudgetLimit {
			t.Errorf("%d cards, want %d", cards, updateBudgetLimit)
		}
		check("stages", k, updateBudgetLimit, 0)
	})

	t.Run("parallel updates at the edge get exactly what is left", func(t *testing.T) {
		k := f.key("ub-race", false)
		const parallel = 8
		ids := make([]string, updateBudgetLimit-1+parallel)
		for i := range ids {
			ids[i] = f.seed(fmt.Sprintf("ub race %d", i))
		}
		for i := range updateBudgetLimit - 1 {
			if r := f.restUpdate(k, ids[i], map[string]any{"content": "spent"}); r.status != http.StatusOK {
				t.Fatalf("spend %d: %d %s", i, r.status, r.answer)
			}
		}
		// A second ctxd instance on the same database: its own pool and
		// router. Half the writes go through it.
		pool2, err := pgxpool.NewWithConfig(context.Background(), f.pool.Config())
		if err != nil {
			t.Fatal(err)
		}
		defer pool2.Close()
		f2 := &nulSweepFixture{router: nulSweepInstance(t, context.Background(), pool2), pool: pool2, log: f.log, keys: f.keys}
		// Hold the rows of the eight writes, so they all reach their
		// budget step together once they are let go: each takes the key's
		// budget lock after its row, counts and books under it; without it
		// all eight would count the same rest.
		// The holder and the monitor run on a pool of their own: the
		// updates hold connections of the router's pool while they wait
		// (the default pool has max(4, cores) connections — four on a
		// two-core runner), and a monitor query on that pool would wait
		// for one of them, never release the holder, and let all eight
		// run into the request timeout.
		sideCfg := f.pool.Config()
		sideCfg.MaxConns = 2
		side, err := pgxpool.NewWithConfig(context.Background(), sideCfg)
		if err != nil {
			t.Fatal(err)
		}
		defer side.Close()
		holder, err := side.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(context.Background()) }()
		// As many writes as each router's pool has connections can wait in
		// the database at once; the rest wait for a connection.
		inDB := min(parallel/2, int(f.pool.Config().MaxConns)) + min(parallel/2, int(pool2.Config().MaxConns))
		raced := ids[updateBudgetLimit-1:]
		if _, err := holder.Exec(context.Background(), `SELECT 1 FROM context_blocks WHERE id = ANY($1) FOR UPDATE`, raced); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		statuses := map[int]int{}
		for i := range parallel {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				via := f.nulSweepFixture
				if i%2 == 1 {
					via = f2
				}
				// Six single updates and two batches of one update item.
				c := nulSweepCase{method: http.MethodPost, url: "/api/manage", cred: k,
					body: nulSweepJSON(map[string]any{"action": "update", "id": id, "data": map[string]any{"content": "raced"}})}
				if i >= parallel-2 {
					c.url, c.body = "/api/store/batch", nulSweepJSON(map[string]any{"items": []any{map[string]any{"id": id, "content": "raced"}}})
				}
				r := via.do(t, c, false)
				mu.Lock()
				statuses[r.status]++
				mu.Unlock()
			}(i, ids[updateBudgetLimit-1+i])
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			var waiting int
			if err := side.QueryRow(context.Background(),
				`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting >= inDB || time.Now().After(deadline) {
				break
			}
		}
		_ = holder.Rollback(context.Background())
		wg.Wait()
		if statuses[http.StatusOK] != 1 || statuses[http.StatusTooManyRequests] != parallel-1 {
			t.Errorf("statuses %v, want one 200 and %d × 429", statuses, parallel-1)
		}
		var written int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM context_blocks WHERE id = ANY($1) AND content = 'raced'`, ids).Scan(&written); err != nil {
			t.Fatalf("written: %v", err)
		}
		if written != 1 {
			t.Errorf("%d blocks written, want 1", written)
		}
		check("race", k, updateBudgetLimit, updateBudgetLimit)
	})
}

// updateBudgetSide is a pool of its own beside the router's, for a holder and
// a monitor: the router's updates hold connections of its pool while they
// wait.
func updateBudgetSide(t *testing.T, f updateBudgetFixture) *pgxpool.Pool {
	t.Helper()
	cfg := f.pool.Config()
	cfg.MaxConns = 3
	side, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(side.Close)
	return side
}

// waitForLock waits until n backends of the database wait for a lock of the
// given wait_event ("" = any), and fails after 10 s.
func waitForLock(t *testing.T, side *pgxpool.Pool, event string, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var waiting int
		if err := side.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND ($1 = '' OR wait_event = $1)`,
			event).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends wait for a lock (%q), want %d", waiting, event, n)
		}
	}
}

// TestSingleUpdateWaitingForARowHoldsNoOtherWrite (issue #519, review of PR
// #553): an update took its key's budget lock FIRST and then waited for its
// row, so while another transaction held that row every other update, batch
// and stage of the key waited with it — on other blocks, no-ops included —
// until their requests timed out. The budget lock is now taken after the rows,
// right before the count: an update that waits for a row holds no budget
// lock, and an update that changes nothing never takes it.
func TestSingleUpdateWaitingForARowHoldsNoOtherWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := updateBudgetFixture{nulSweepRouter(t, map[string]string{"CTX_RATE_LIMIT_WRITE": "50"}), t}
	side := updateBudgetSide(t, f)
	k := f.key("ub-hol", false)
	a, b, c := f.seed("ub hol a"), f.seed("ub hol b"), f.seed("ub hol c")
	quick := func(c nulSweepCase) nulSweepResult {
		c.cred, c.timeout = k, 5*time.Second
		return f.do(t, c, false)
	}
	update := func(id string, data map[string]any) nulSweepCase {
		return nulSweepCase{method: http.MethodPost, url: "/api/manage", body: nulSweepJSON(map[string]any{"action": "update", "id": id, "data": data})}
	}

	t.Run("an update waiting for its row lets the key's other writes through", func(t *testing.T) {
		holder, err := side.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(context.Background()) }()
		if _, err := holder.Exec(context.Background(), `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, a); err != nil {
			t.Fatal(err)
		}
		waited := make(chan nulSweepResult, 1)
		go func() { waited <- f.restUpdate(k, a, map[string]any{"content": "waited for its row"}) }()
		waitForLock(t, side, "", 1)

		for _, tc := range []struct {
			name string
			c    nulSweepCase
		}{
			{"an update of another block", update(b, map[string]any{"content": "b changed"})},
			{"an unchanged update of another block", update(b, map[string]any{"content": "b changed"})},
			{"a batch", nulSweepCase{method: http.MethodPost, url: "/api/store/batch", body: nulSweepJSON(map[string]any{"items": []any{
				map[string]any{"category": "learnings", "title": "ub hol batch", "content": "new"},
				map[string]any{"id": c, "content": "c changed"},
			}})}},
			{"an MCP update of another block", nulSweepCase{method: http.MethodPost, url: "/mcp", mcp: true, body: nulSweepMCP("update", map[string]any{"id": c, "title": "ub hol c2"})}},
		} {
			r := quick(tc.c)
			if r.status != http.StatusOK || r.isError || strings.Contains(r.answer, `"success":false`) {
				t.Errorf("%s while the key's update of %s waits for its row: %d %s", tc.name, a, r.status, r.answer)
			}
		}
		select {
		case r := <-waited:
			t.Fatalf("the update of the held row ended before the holder let go: %d %s", r.status, r.answer)
		default:
		}
		_ = holder.Rollback(context.Background())
		if r := <-waited; r.status != http.StatusOK {
			t.Errorf("the update of the held row after the holder let go: %d %s", r.status, r.answer)
		}
		// b, the batch's store item, c (by the batch and by MCP) and a.
		if all, withBlock := f.bookings(k); all != 5 || withBlock != 5 {
			t.Errorf("%d bookings (%d naming a block), want 5", all, withBlock)
		}
	})

	t.Run("an unchanged update never waits for the budget lock", func(t *testing.T) {
		holder, err := side.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(context.Background()) }()
		if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_xact_lock(hashtext('ctx:write-budget'), hashtext($1::text))`, f.keys[k+"#id"]); err != nil {
			t.Fatal(err)
		}
		before, _ := f.bookings(k)
		for _, tc := range []struct {
			name string
			c    nulSweepCase
		}{
			// No MCP case here: a tool call is not ended by the request's
			// bound, so on a regression it would wait for the holder for good.
			{"an unchanged update", update(b, map[string]any{"content": "b changed"})},
			{"an unchanged batch", nulSweepCase{method: http.MethodPost, url: "/api/store/batch", body: nulSweepJSON(map[string]any{"items": []any{
				map[string]any{"category": "learnings", "title": "ub hol batch", "content": "new"},
			}})}},
		} {
			r := quick(tc.c)
			if r.status != http.StatusOK || r.isError || strings.Contains(r.answer, `"success":false`) {
				t.Errorf("%s while the budget lock is held: %d %s", tc.name, r.status, r.answer)
			}
		}
		// A changing update waits for the lock, and writes once it is free.
		changed := make(chan nulSweepResult, 1)
		go func() { changed <- f.restUpdate(k, b, map[string]any{"content": "b changed again"}) }()
		waitForLock(t, side, "advisory", 1)
		_ = holder.Rollback(context.Background())
		if r := <-changed; r.status != http.StatusOK {
			t.Errorf("a changing update after the lock was let go: %d %s", r.status, r.answer)
		}
		if all, _ := f.bookings(k); all != before+1 {
			t.Errorf("%d bookings, want %d: the unchanged writes booked, or the changing one did not", all, before+1)
		}
	})
}
