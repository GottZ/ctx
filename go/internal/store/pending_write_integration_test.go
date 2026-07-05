//go:build integration

// Integration coverage for the F6-C6 staging store (D-W1, migration 089).
// Pins the confirm-protocol invariants the later MCP/Chat waves build on:
//
//  1. Fail-closed consume: expired stage ⇒ ErrPendingWriteNotFound (Rot-Probe
//     D-W1 gate "Consume auf abgelaufen → 0").
//  2. Single-use: the second consume of the same stage rejects (replay).
//  3. Principal binding: a foreign api_key_id never consumes the stage.
//  4. DECOUPLED knobs (D2-C1 Rot-Probe): ttl=0 stages NEVER expire and
//     consume SUCCEEDS — the coupled draft (expires_at=now()+0) would reject
//     every confirm; asserting expires_at IS NULL + a green consume proves
//     the decoupling.
//  5. Stage idempotency: re-staging the same (key, hash) re-arms the open
//     row instead of duplicating (app-side re-arm, hypertable unique
//     restriction, 089 header).
//  6. Migration 089 actually created a hypertable (E4).
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestPendingWrite -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestPendingWriteStageConsume(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	key, _, err := store.CreateApiKey(ctx, pool, "pw-test", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	foreign, _, err := store.CreateApiKey(ctx, pool, "pw-foreign", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create foreign key: %v", err)
	}

	payload := json.RawMessage(`{"op":"store","category":"test","title":"t","content":"c"}`)

	t.Run("migration 089 made a hypertable", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM timescaledb_information.hypertables
			  WHERE hypertable_name = 'context_pending_writes'`).Scan(&n); err != nil {
			t.Fatalf("hypertable probe: %v", err)
		}
		if n != 1 {
			t.Fatalf("context_pending_writes is not a hypertable (count=%d) — E4 violated", n)
		}
	})

	t.Run("stage then consume executes exactly once", func(t *testing.T) {
		pw, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", payload, "hash-once", 10*time.Minute)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		if pw.ExpiresAt == nil || time.Until(*pw.ExpiresAt) < 9*time.Minute {
			t.Fatalf("expires_at not ~now+ttl: %v", pw.ExpiresAt)
		}

		got, err := store.ConsumePendingWrite(ctx, pool, key.ID, "hash-once")
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		// JSONB round-trips are NOT byte-stable (key order/whitespace get
		// normalized) — compare semantically. D-W2 consequence: the payload
		// hash must be computed over the canonical CLIENT serialization,
		// never over the JSONB read-back bytes.
		var want, have map[string]any
		if err := json.Unmarshal(payload, &want); err != nil {
			t.Fatalf("unmarshal want: %v", err)
		}
		if err := json.Unmarshal(got.Payload, &have); err != nil {
			t.Fatalf("unmarshal have: %v", err)
		}
		if got.Op != "store" || !reflect.DeepEqual(want, have) {
			t.Fatalf("consume returned wrong row: op=%q payload=%s", got.Op, got.Payload)
		}
		if got.ConsumedAt == nil {
			t.Fatal("consumed row missing consumed_at")
		}

		// Replay (single-use): the second consume must reject.
		if _, err := store.ConsumePendingWrite(ctx, pool, key.ID, "hash-once"); !errors.Is(err, store.ErrPendingWriteNotFound) {
			t.Fatalf("replay consume: want ErrPendingWriteNotFound, got %v", err)
		}
	})

	t.Run("expired stage rejects fail-closed", func(t *testing.T) {
		if _, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", payload, "hash-expired", 10*time.Minute); err != nil {
			t.Fatalf("stage: %v", err)
		}
		// Force expiry in the past — deterministic, no sleep.
		if _, err := pool.Exec(ctx,
			`UPDATE context_pending_writes SET expires_at = now() - interval '1 second'
			  WHERE payload_hash = 'hash-expired'`); err != nil {
			t.Fatalf("force expiry: %v", err)
		}
		if _, err := store.ConsumePendingWrite(ctx, pool, key.ID, "hash-expired"); !errors.Is(err, store.ErrPendingWriteNotFound) {
			t.Fatalf("expired consume: want ErrPendingWriteNotFound, got %v", err)
		}
	})

	t.Run("foreign key never consumes the stage", func(t *testing.T) {
		if _, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", payload, "hash-principal", 10*time.Minute); err != nil {
			t.Fatalf("stage: %v", err)
		}
		if _, err := store.ConsumePendingWrite(ctx, pool, foreign.ID, "hash-principal"); !errors.Is(err, store.ErrPendingWriteNotFound) {
			t.Fatalf("foreign consume: want ErrPendingWriteNotFound, got %v", err)
		}
		// The rightful principal still can.
		if _, err := store.ConsumePendingWrite(ctx, pool, key.ID, "hash-principal"); err != nil {
			t.Fatalf("rightful consume after foreign miss: %v", err)
		}
	})

	t.Run("ttl=0 never expires and consume succeeds (decoupled knobs)", func(t *testing.T) {
		pw, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "chat", payload, "hash-ttl0", 0)
		if err != nil {
			t.Fatalf("stage ttl=0: %v", err)
		}
		// Rot-Probe D2-C1: the COUPLED draft stored expires_at=now()+0 here,
		// making this very consume reject. NULL expiry + green consume is the
		// proof of decoupling.
		if pw.ExpiresAt != nil {
			t.Fatalf("ttl=0 must store NULL expiry (coupled semantics detected): %v", *pw.ExpiresAt)
		}
		if _, err := store.ConsumePendingWrite(ctx, pool, key.ID, "hash-ttl0"); err != nil {
			t.Fatalf("ttl=0 consume must succeed, got %v", err)
		}
	})

	t.Run("re-stage re-arms the open row instead of duplicating", func(t *testing.T) {
		first, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", payload, "hash-rearm", 1*time.Minute)
		if err != nil {
			t.Fatalf("stage 1: %v", err)
		}
		second, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", payload, "hash-rearm", 30*time.Minute)
		if err != nil {
			t.Fatalf("stage 2: %v", err)
		}
		if first.ID != second.ID {
			t.Fatalf("re-stage duplicated: %s != %s", first.ID, second.ID)
		}
		if second.ExpiresAt == nil || time.Until(*second.ExpiresAt) < 29*time.Minute {
			t.Fatalf("re-stage did not re-arm expiry: %v", second.ExpiresAt)
		}
		var open int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_pending_writes
			  WHERE payload_hash = 'hash-rearm' AND consumed_at IS NULL`).Scan(&open); err != nil {
			t.Fatalf("open count: %v", err)
		}
		if open != 1 {
			t.Fatalf("want exactly 1 open row after re-stage, got %d", open)
		}
	})

	t.Run("lookup reads without consuming", func(t *testing.T) {
		if _, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "update", "chat", payload, "hash-lookup", 10*time.Minute); err != nil {
			t.Fatalf("stage: %v", err)
		}
		got, err := store.LookupPendingWrite(ctx, pool, key.ID, "hash-lookup")
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if got.ConsumedAt != nil {
			t.Fatal("lookup must not consume")
		}
		if _, err := store.ConsumePendingWrite(ctx, pool, key.ID, "hash-lookup"); err != nil {
			t.Fatalf("consume after lookup: %v", err)
		}
	})
}
