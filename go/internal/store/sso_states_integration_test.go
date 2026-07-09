//go:build integration

// Integration test for OAuth wave L4 (design 04 §7 W5, migration 101
// "context_sso_states").
//
// L4 ports the serviceportal sso_pending_states pattern: a server-side,
// single-use, HA-safe SSO login state. The row id (cookie value) and the
// OAuth `state` URL param are TWO distinct UUIDs (double-UUID principle,
// 101 header) — this test seeds distinct values for both and asserts the
// roundtrip returns them byte-identically.
//
// Negative probes (all must MISS, i.e. (nil, nil) with no error — the
// consume path is deliberately oracle-free about WHICH check failed):
//   - second consume of the same id (single-use / replay)
//   - purpose cross-use (login state consumed as link)
//   - expired state (negative TTL)
//   - syntactically invalid id (non-UUID cookie value → no 22P02 leak)
//
// The evict sweep only removes rows past expiry PLUS the grace hour: a
// just-expired row survives the sweep (debugging grace) but is still a
// consume miss — correctness never depends on the janitor.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestSSOStates -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSSOStates(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	want := store.SSOStateData{
		ProviderSlug: "github",
		State:        "1d61dc50-5c64-4a26-a37c-8a58f0f7d1a2", // OAuth state param — distinct from the row id
		PKCEVerifier: "pkce-verifier-0123456789abcdef",
		Nonce:        "9a0f5b1c-7e2d-4f3a-b8c6-0d1e2f3a4b5c",
		ReturnTo:     "/blocks?q=test",
	}

	t.Run("roundtrip_and_single_use", func(t *testing.T) {
		id, err := store.StoreSSOState(ctx, pool, store.SSOPurposeLogin, want, 10*time.Minute)
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		if id == want.State {
			t.Fatalf("row id equals OAuth state param — double-UUID principle violated")
		}

		got, err := store.ConsumeSSOState(ctx, pool, id, store.SSOPurposeLogin)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if got == nil {
			t.Fatalf("consume: miss on fresh state, want hit")
		}
		if *got != want {
			t.Fatalf("consume: state_data mismatch:\n got %+v\nwant %+v", *got, want)
		}

		// Replay: the second consume of the SAME id must be a silent miss.
		got2, err := store.ConsumeSSOState(ctx, pool, id, store.SSOPurposeLogin)
		if err != nil {
			t.Fatalf("second consume: unexpected error: %v", err)
		}
		if got2 != nil {
			t.Fatalf("second consume returned data — single-use broken (replay)")
		}
	})

	t.Run("purpose_cross_use_misses", func(t *testing.T) {
		id, err := store.StoreSSOState(ctx, pool, store.SSOPurposeLogin, want, 10*time.Minute)
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		// A login state must never authorize an account link.
		got, err := store.ConsumeSSOState(ctx, pool, id, store.SSOPurposeLink)
		if err != nil {
			t.Fatalf("cross-use consume: unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("login state consumed under purpose=link — cross-use guard broken")
		}
		// The failed cross-use must NOT have burned the row: the correct
		// purpose still consumes it.
		got, err = store.ConsumeSSOState(ctx, pool, id, store.SSOPurposeLogin)
		if err != nil {
			t.Fatalf("consume after cross-use miss: %v", err)
		}
		if got == nil {
			t.Fatalf("cross-use miss deleted the row, want it intact")
		}
	})

	t.Run("expired_misses", func(t *testing.T) {
		id, err := store.StoreSSOState(ctx, pool, store.SSOPurposeLogin, want, -1*time.Minute)
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		got, err := store.ConsumeSSOState(ctx, pool, id, store.SSOPurposeLogin)
		if err != nil {
			t.Fatalf("expired consume: unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("expired state consumed, want miss")
		}
	})

	t.Run("invalid_id_misses", func(t *testing.T) {
		got, err := store.ConsumeSSOState(ctx, pool, "not-a-uuid", store.SSOPurposeLogin)
		if err != nil {
			t.Fatalf("invalid id must be a silent miss, got error: %v", err)
		}
		if got != nil {
			t.Fatalf("invalid id returned data")
		}
	})

	t.Run("invalid_purpose_rejected_on_store", func(t *testing.T) {
		if _, err := store.StoreSSOState(ctx, pool, "admin", want, time.Minute); err == nil {
			t.Fatalf("purpose outside the closed value list accepted, want error")
		}
	})

	t.Run("evict_respects_grace", func(t *testing.T) {
		// Three rows: live, just-expired (inside the 1h grace), long-expired.
		liveID, err := store.StoreSSOState(ctx, pool, store.SSOPurposeLogin, want, 10*time.Minute)
		if err != nil {
			t.Fatalf("store live: %v", err)
		}
		graceID, err := store.StoreSSOState(ctx, pool, store.SSOPurposeLogin, want, -1*time.Minute)
		if err != nil {
			t.Fatalf("store grace: %v", err)
		}
		oldID, err := store.StoreSSOState(ctx, pool, store.SSOPurposeLogin, want, -2*time.Hour)
		if err != nil {
			t.Fatalf("store old: %v", err)
		}

		deleted, err := store.EvictExpiredSSOStates(ctx, pool)
		if err != nil {
			t.Fatalf("evict: %v", err)
		}
		if deleted != 1 {
			t.Fatalf("evict deleted %d rows, want exactly 1 (only past-grace)", deleted)
		}

		var count int
		for _, id := range []string{liveID, graceID, oldID} {
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM context_sso_states WHERE id = $1`, id).Scan(&count); err != nil {
				t.Fatalf("count row: %v", err)
			}
			switch id {
			case oldID:
				if count != 0 {
					t.Fatalf("long-expired row survived the sweep")
				}
			default:
				if count != 1 {
					t.Fatalf("evict removed a row inside grace/live (id %s)", id)
				}
			}
		}

		// The grace row survived the sweep but is STILL a consume miss —
		// correctness lives in ConsumeSSOState, not in the janitor.
		got, err := store.ConsumeSSOState(ctx, pool, graceID, store.SSOPurposeLogin)
		if err != nil {
			t.Fatalf("grace consume: %v", err)
		}
		if got != nil {
			t.Fatalf("expired-but-unswept state consumed, want miss")
		}

		// And the live row is untouched and consumable.
		got, err = store.ConsumeSSOState(ctx, pool, liveID, store.SSOPurposeLogin)
		if err != nil {
			t.Fatalf("live consume: %v", err)
		}
		if got == nil {
			t.Fatalf("live state gone after evict, want hit")
		}
	})
}
