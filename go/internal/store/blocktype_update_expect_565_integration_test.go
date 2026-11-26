//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestUpdateBlockType565_ExpectConfig pins the lost-update guard the merge of
// issue #565 needs: the handlers read the row's config, lay the request over
// it and write the result, so a second write of the row between that read and
// the write would otherwise lose its fields. The update writes only while the
// row still holds the config it was merged with.
//
// RED probes (mutations): the `AND config = …` conjunct dropped — the stale
// update overwrites the concurrent one (both interleavings); the existence
// recheck dropped — the conflict reads as a 404.
func TestUpdateBlockType565_ExpectConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scopes := []string{store.GlobalScope}
	const c0 = `{"v":1,"retrieval":{"policy":"full-pass"}}`
	bt, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "t565-expect", Scope: store.GlobalScope, Config: json.RawMessage(c0)}, nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored := func() string {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT config FROM context_block_types WHERE id = $1`, bt.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	sameJSON := func(a, b string) bool {
		var x, y any
		_ = json.Unmarshal([]byte(a), &x)
		_ = json.Unmarshal([]byte(b), &y)
		return reflect.DeepEqual(x, y)
	}
	set := func(cfg string) {
		t.Helper()
		if _, err := store.UpdateBlockType(ctx, pool, bt.ID, scopes, store.BlockTypeUpdate{Config: json.RawMessage(cfg)}, nil, ""); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("matching config writes", func(t *testing.T) {
		set(c0)
		const c1 = `{"v":1,"retrieval":{"policy":"excluded"}}`
		got, err := store.UpdateBlockType(ctx, pool, bt.ID, scopes, store.BlockTypeUpdate{
			Config: json.RawMessage(c1), ExpectConfig: json.RawMessage(stored()),
		}, nil, "")
		if err != nil || got == nil || !sameJSON(stored(), c1) {
			t.Fatalf("update = %v, %v; stored %s, want %s", got, err, stored(), c1)
		}
	})

	t.Run("a write after the read refuses the stale update", func(t *testing.T) {
		set(c0)
		read := stored() // the handler's read
		set(`{"v":1,"guard":{"check":false}}`)
		_, err := store.UpdateBlockType(ctx, pool, bt.ID, scopes, store.BlockTypeUpdate{
			Config: json.RawMessage(`{"v":1,"retrieval":{"policy":"excluded"}}`), ExpectConfig: json.RawMessage(read),
		}, nil, "")
		if !errors.Is(err, store.ErrBlockTypeConfigChanged) {
			t.Fatalf("stale update: err = %v, want store.ErrBlockTypeConfigChanged", err)
		}
		if !sameJSON(stored(), `{"v":1,"guard":{"check":false}}`) {
			t.Fatalf("stale update wrote: %s", stored())
		}
	})

	t.Run("a write holding the row lock commits first; the waiting update sees its config", func(t *testing.T) {
		set(c0)
		read := stored()
		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx,
			`UPDATE context_block_types SET config = '{"v":1,"dream":{"linkable":false}}'::jsonb WHERE id = $1`, bt.ID); err != nil {
			t.Fatal(err)
		}
		cfg := pool.Config().Copy()
		cfg.ConnConfig.RuntimeParams["application_name"] = "t565-update"
		waiterPool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer waiterPool.Close()
		done := make(chan error, 1)
		go func() {
			_, err := store.UpdateBlockType(ctx, waiterPool, bt.ID, scopes, store.BlockTypeUpdate{
				Config: json.RawMessage(`{"v":1,"retrieval":{"policy":"excluded"}}`), ExpectConfig: json.RawMessage(read),
			}, nil, "")
			done <- err
		}()
		for deadline := time.Now().Add(20 * time.Second); ; {
			var waiting bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				  WHERE application_name = 't565-update' AND datname = current_database() AND wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the update never waited for the row lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, store.ErrBlockTypeConfigChanged) {
				t.Fatalf("update after the lock wait: err = %v, want store.ErrBlockTypeConfigChanged", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the update did not return after the holder committed")
		}
		if !sameJSON(stored(), `{"v":1,"dream":{"linkable":false}}`) {
			t.Fatalf("the waiting update overwrote the committed one: %s", stored())
		}
	})

	t.Run("a row that is gone stays a 404", func(t *testing.T) {
		got, err := store.UpdateBlockType(ctx, pool, "019f0000-0000-7000-8000-000000000565", scopes, store.BlockTypeUpdate{
			Config: json.RawMessage(c0), ExpectConfig: json.RawMessage(c0),
		}, nil, "")
		if err != nil || got != nil {
			t.Fatalf("unknown id: %v, %v — want nil, nil", got, err)
		}
	})

	t.Run("without ExpectConfig the update writes as before", func(t *testing.T) {
		set(`{"v":1,"guard":{"check":false}}`)
		if !sameJSON(stored(), `{"v":1,"guard":{"check":false}}`) {
			t.Fatalf("stored %s", stored())
		}
	})
}
