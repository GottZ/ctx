package llmlog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSettle pins the ordering contract behind issue #50: Settle returns only
// once no insert Record started on the pool is still in flight, gives up with
// ctx, and does not wait on another pool's inserts.
func TestSettle(t *testing.T) {
	// pgxpool.New connects lazily; port 1 refuses at once, so every insert
	// fails fast and the test needs no database.
	newPool := func() *pgxpool.Pool {
		p, err := pgxpool.New(context.Background(), "postgres://u@127.0.0.1:1/x?connect_timeout=1")
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		t.Cleanup(p.Close)
		return p
	}
	a, b := newPool(), newPool()

	// A started insert is counted before Record returns, and Settle waits it out.
	Record(a, Entry{Pipeline: "settle-test", Model: "m"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Settle(ctx, a); err != nil {
		t.Fatalf("Settle after a failing insert: %v", err)
	}
	if n := inflightFor(a).Load(); n != 0 {
		t.Fatalf("in flight after Settle = %d, want 0", n)
	}

	// A pending insert holds Settle until ctx ends, and only on its own pool.
	inflightFor(a).Add(1)
	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	if err := Settle(short, a); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Settle with a pending insert = %v, want DeadlineExceeded", err)
	}
	if err := Settle(context.Background(), b); err != nil {
		t.Fatalf("Settle on another pool waited on this one: %v", err)
	}
	inflightFor(a).Add(-1)
	if err := Settle(context.Background(), a); err != nil {
		t.Fatalf("Settle after the insert finished: %v", err)
	}
}
