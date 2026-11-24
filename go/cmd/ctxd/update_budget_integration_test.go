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
		// Hold the rows of the eight updates, so they all reach their
		// budget step together once they are let go: with the key's budget
		// lock the first holds it while it waits for its row and the others
		// wait for the lock; without it all eight would count the same rest.
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
		// As many updates as the router's pool has connections can wait in
		// the database at once; the rest wait for a connection.
		inDB := min(parallel, int(f.pool.Config().MaxConns))
		raced := ids[updateBudgetLimit-1:]
		if _, err := holder.Exec(context.Background(), `SELECT 1 FROM context_blocks WHERE id = ANY($1) FOR UPDATE`, raced); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		statuses := map[int]int{}
		for i := range parallel {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				r := f.restUpdate(k, id, map[string]any{"content": "raced"})
				mu.Lock()
				statuses[r.status]++
				mu.Unlock()
			}(ids[updateBudgetLimit-1+i])
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
