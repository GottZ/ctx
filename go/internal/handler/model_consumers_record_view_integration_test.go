//go:build integration

// The read form of model-consumer-update never reports a recording state it
// did not read (issue #571, T1 of the review of PR #573): an update that does
// not name record_bodies leaves the switch as it is, and the consumer it
// answers with names the switch as read — also when the re-read after the
// write fails and the answer falls back to the written row, whose update did
// not read the column.
//
// The re-read is made to fail through the handler's own pool: a query tracer
// notes the commit of the transaction that updated context_model_consumers,
// and from then on the pool's PrepareConn refuses every acquire — the
// production handler, store and database, with the one fault the fallback
// exists for.
//
//	go test -tags=integration -p 1 -run 'TestModelConsumerUpdateViewNamesTheReadSwitch' ./internal/handler/ -count=1 -v
package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// errReReadRefused is the fault the pool injects after the update's commit.
var errReReadRefused = errors.New("test: the pool refuses connections after the update committed")

// commitTrap is a query tracer: once a connection that ran an UPDATE of
// context_model_consumers commits, tripped is set.
type commitTrap struct {
	armed   atomic.Bool
	tripped atomic.Bool
	mu      sync.Mutex
	updated map[*pgx.Conn]bool
}

type commitTrapSQL struct{}

func (c *commitTrap) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if c.armed.Load() && strings.Contains(data.SQL, "UPDATE context_model_consumers") {
		c.mu.Lock()
		c.updated[conn] = true
		c.mu.Unlock()
	}
	return context.WithValue(ctx, commitTrapSQL{}, data.SQL)
}

func (c *commitTrap) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(commitTrapSQL{}).(string)
	if data.Err != nil || !strings.EqualFold(strings.TrimSpace(sql), "commit") {
		return
	}
	c.mu.Lock()
	hit := c.updated[conn]
	delete(c.updated, conn)
	c.mu.Unlock()
	if hit {
		c.tripped.Store(true)
	}
}

func TestModelConsumerUpdateViewNamesTheReadSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "rec-view")
	recordOf := func(resp map[string]any) any {
		c, _ := resp["consumer"].(map[string]any)
		return c["record_bodies"]
	}
	status, body, resp := mcCall(t, h, admin, "model-consumer-create",
		map[string]any{"label": "rec-view-on", "tenant_id": a.id, "record_bodies": true, "confirm_record_bodies": true})
	if status != http.StatusOK || recordOf(resp) != true {
		t.Fatalf("create the recording consumer: %d %s", status, body)
	}
	id := resp["consumer"].(map[string]any)["id"].(string)

	// The handler under test runs over a pool of its own that refuses every
	// connection once the update's transaction has committed.
	trap := &commitTrap{updated: map[*pgx.Conn]bool{}}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Tracer = trap
	cfg.PrepareConn = func(context.Context, *pgx.Conn) (bool, error) {
		if trap.tripped.Load() {
			return true, errReReadRefused
		}
		return true, nil
	}
	faulty, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(faulty.Close)
	hf := mcHandler(t, faulty, false, false)

	for _, tc := range []struct {
		name string
		data map[string]any
	}{
		{"label_only", map[string]any{"id": id, "label": "rec-view-renamed"}},
		{"active_only", map[string]any{"id": id, "active": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trap.tripped.Store(false)
			trap.armed.Store(true)
			status, body, resp := mcCall(t, hf, admin, "model-consumer-update", tc.data)
			trap.armed.Store(false)
			if !trap.tripped.Load() {
				t.Fatalf("the update's commit was not seen (%d %s): the re-read did not fail, the test does not reach the fallback", status, body)
			}
			trap.tripped.Store(false)
			if status != http.StatusOK {
				t.Fatalf("update: %d %s, want 200 (the update committed)", status, body)
			}
			if got := recordOf(resp); got != true {
				t.Errorf("the update's answer reads record_bodies %v for a consumer that records (re-read failed) — want true, the state as read", got)
			}
			// The consumer still records: the update did not touch the switch.
			if _, _, r := mcCall(t, h, admin, "model-consumer-get", map[string]any{"id": id}); recordOf(r) != true {
				t.Fatalf("model-consumer-get reads record_bodies %v after an update that did not name it", recordOf(r))
			}
		})
	}
}
