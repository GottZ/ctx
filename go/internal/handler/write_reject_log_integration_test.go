//go:build integration

// Issue #66, the rejections the unit file cannot reach without a database:
// 429 rate_limit (the gate counts context_access_log rows), 422
// unknown_parent (the parent pre-flight) and 403 provenance_protected (decided
// in store.UpsertBlock) — the last two AFTER the gate chain. Each must leave
// one WARN line naming the key; the rate-limit burst — the
// shape `ctx persist --hook` produces when a key is over budget — leaves ONE
// line, not one per refused write.
//
// RED on b52b340a: no line for any of them.
//
//	go test -tags=integration -p 1 -count=1 -run TestStoreWriteRejectionLogDB ./internal/handler/
package handler

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStoreWriteRejectionLogDB(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	cfgWith := func(limit int) staticConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Query: config.QueryConfig{RateLimitWrite: limit},
			Pool:  config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		}}
	}
	mkKey := func(label string) (string, *auth.AuthResult) {
		t.Helper()
		row, plain, err := store.CreateApiKey(ctx, pool, label, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", label, err)
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v", label, err)
		}
		if ar.ApiKeyID != row.ID {
			t.Fatalf("AuthResult.ApiKeyID %q != key row id %q", ar.ApiKeyID, row.ID)
		}
		return row.ID, ar
	}

	t.Run("rate_limit_burst_one_line", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		keyID, ar := mkKey("w66-rate")
		const limit = 1
		// Spend the budget synchronously: the REST success path books its row
		// in a goroutine, which would make the 429 below a race.
		if err := store.LogAccess(ctx, pool, keyID, "", "write"); err != nil {
			t.Fatalf("seed the write budget: %v", err)
		}
		h := NewStoreHandler(pool, cfgWith(limit), reg)
		reqIDs := []string{"req-66-rate-1", "req-66-rate-2", "req-66-rate-3"}
		for _, id := range reqIDs {
			status, code := storeRejectedAs(t, h, ar, id, map[string]any{
				"category": "learnings", "title": rejectLogSecretTitle + id, "content": rejectLogSecretContent,
			})
			if status != 429 || code != "rate_limit" {
				t.Fatalf("%s: answer = %d %q, want 429 rate_limit", id, status, code)
			}
		}
		lines := logs.withMsg(rejectLogMsg)
		if len(lines) != 1 {
			t.Fatalf("%q lines = %d after a burst of %d refusals of one key and code, want 1: %v",
				rejectLogMsg, len(lines), len(reqIDs), lines)
		}
		want := map[string]any{
			"api_key_id": keyID, "code": "rate_limit", "status": int64(429),
			"request_id": reqIDs[0], "category": "learnings", "scope": "",
		}
		for k, v := range want {
			if got := lines[0].attrs[k]; got != v {
				t.Errorf("attr %s = %v, want %v", k, got, v)
			}
		}
	})

	t.Run("unknown_parent_after_the_chain", func(t *testing.T) {
		// The parent pre-flight refuses past the gate chain too — a typo in
		// parent_id loses the block exactly like a gate refusal does.
		logs := installRejectLogCapture(t)
		keyID, ar := mkKey("w66-parent")
		status, code := storeRejectedAs(t, NewStoreHandler(pool, cfgWith(0), reg), ar, "req-66-parent", map[string]any{
			"category": "learnings", "title": rejectLogSecretTitle, "content": rejectLogSecretContent,
			"parent_id": "01990000-0000-7000-8000-0000000000ff", // well-formed, no such block
		})
		if status != 422 || code != "unknown_parent" {
			t.Fatalf("answer = %d %q, want 422 unknown_parent", status, code)
		}
		lines := logs.withMsg(rejectLogMsg)
		if len(lines) != 1 {
			t.Fatalf("%q lines = %d, want 1: %v", rejectLogMsg, len(lines), lines)
		}
		if got := lines[0].attrs["api_key_id"]; got != keyID {
			t.Errorf("api_key_id = %v, want %s", got, keyID)
		}
		if got := lines[0].attrs["code"]; got != "unknown_parent" {
			t.Errorf("code = %v, want unknown_parent", got)
		}
	})

	t.Run("provenance_protected_after_the_chain", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		keyID, ar := mkKey("w66-prov")
		// A derivative written by the server path (the badge), then the same
		// identity from a client: the refusal comes out of store.UpsertBlock,
		// past every gate of runStageWriteGates.
		const category, title, scope = "learnings", "W66 derivative", "private"
		provenanceMD := map[string]any{derived.MetadataKey: map[string]any{
			"v": derived.ContractVersion, "stratum": int(derived.StratumDerived), "arm": "w66-probe",
		}}
		if _, err := store.UpsertBlock(ctx, pool, category, title, "server-written derivative", nil, provenanceMD,
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("server-path write: %v", err)
		}
		status, code := storeRejectedAs(t, NewStoreHandler(pool, cfgWith(0), reg), ar, "req-66-prov", map[string]any{
			"category": category, "title": title, "content": rejectLogSecretContent,
		})
		if status != 403 || code != "provenance_protected" {
			t.Fatalf("answer = %d %q, want 403 provenance_protected", status, code)
		}
		lines := logs.withMsg(rejectLogMsg)
		if len(lines) != 1 {
			t.Fatalf("%q lines = %d, want 1: %v", rejectLogMsg, len(lines), lines)
		}
		if got := lines[0].attrs["api_key_id"]; got != keyID {
			t.Errorf("api_key_id = %v, want %s", got, keyID)
		}
		if got := lines[0].attrs["code"]; got != "provenance_protected" {
			t.Errorf("code = %v, want provenance_protected", got)
		}
		if got := lines[0].attrs["request_id"]; got != "req-66-prov" {
			t.Errorf("request_id = %v, want req-66-prov", got)
		}
	})
}
