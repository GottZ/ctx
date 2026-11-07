//go:build integration

// W01-6: AuthenticateModelKey against a real ctx_model_auth (migration 161).
// Every K20 column reaches ConsumerAuth, and the address and transport
// arguments reach gates 5 and 6. W01-8: DeadModelKeys over
// ctx_model_auth_live (migration 165) — one statement, the dead ids only.
//
//	go test -tags=integration -p 1 -count=1 -run 'Consumer|ModelKey' ./internal/auth/
package auth_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestAuthenticateModelKeyFields_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, pool, "w016-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	stamp := store.APIStamp(admin.ID, "w016")
	scope := "work"
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	c, k, key, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{
		TenantID: store.DefaultTenantID, Label: "w016-peer", Kind: "ctx-peer", TenantScope: &scope,
		AllModels: true, PromptSensitivity: "internal", FairWeight: 7, AdmissionLane: "urgent",
		AllowedCIDRs: []string{"10.0.0.0/8"},
	}, &exp, stamp)
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}
	const peer = "0190f000-0000-7000-8000-00000000c0de"
	if _, err := pool.Exec(ctx, `UPDATE context_model_consumers SET peer_instance_id = $2, peer_bound_at = now() WHERE id = $1`, c.ID, peer); err != nil {
		t.Fatalf("bind peer: %v", err)
	}
	inside := netip.MustParseAddr("10.1.2.3")

	ca, err := auth.AuthenticateModelKey(ctx, pool, key.Hash(), inside, true)
	if err != nil || ca == nil || !ca.IsValid {
		t.Fatalf("valid key: ca=%+v err=%v", ca, err)
	}
	if ca.ModelKeyID != k.ID || ca.ConsumerID != c.ID || ca.TenantID != store.DefaultTenantID ||
		ca.TenantScope != "work" || ca.Kind != auth.KindCtxPeer || !ca.AllModels ||
		ca.PromptSensitivity != "internal" || ca.FairWeight != 7 || ca.AdmissionLane != "urgent" ||
		ca.PeerInstanceID != peer || ca.KeyExpiresAt == nil || !ca.KeyExpiresAt.Equal(exp) {
		t.Fatalf("K20 fields: got %+v (key %s consumer %s exp %v)", ca, k.ID, c.ID, exp)
	}

	// Gate 5 sees the address as passed: IPv4-mapped is the same host,
	// outside the allowlist and an unknown address (NULL) are refused.
	for name, tc := range map[string]struct {
		ip     netip.Addr
		secure bool
		valid  bool
	}{
		"ipv4-mapped":   {netip.MustParseAddr("::ffff:10.1.2.3"), true, true},
		"outside-cidr":  {netip.MustParseAddr("192.168.1.1"), true, false},
		"unknown-addr":  {netip.Addr{}, true, false},
		"plaintext":     {inside, false, false},
		"zoned-v6-miss": {netip.MustParseAddr("fe80::1%eth0"), true, false},
	} {
		got, err := auth.AuthenticateModelKey(ctx, pool, key.Hash(), tc.ip, tc.secure)
		if err != nil || got == nil || got.IsValid != tc.valid {
			t.Errorf("%s: ca=%+v err=%v, want valid=%v", name, got, err, tc.valid)
		}
		if got != nil && !got.IsValid && (got.ModelKeyID != "" || got.ConsumerID != "" || got.TenantID != "" || got.Kind != "") {
			t.Errorf("%s: an invalid result carries sentinel fields: %+v", name, got)
		}
	}

	// Unknown and empty hashes are plain invalid results.
	for _, h := range []string{"", "ab", "0000000000000000000000000000000000000000000000000000000000000000"} {
		got, err := auth.AuthenticateModelKey(ctx, pool, h, inside, true)
		if err != nil || got == nil || got.IsValid {
			t.Errorf("hash %q: ca=%+v err=%v, want a plain invalid result", h, got, err)
		}
	}

	// A consumer without allowlist and without allow_plaintext: unknown
	// address passes gate 5, plaintext fails gate 6.
	_, _, open, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w016-open"}, nil, stamp)
	if err != nil {
		t.Fatalf("create open consumer: %v", err)
	}
	if got, err := auth.AuthenticateModelKey(ctx, pool, open.Hash(), netip.Addr{}, true); err != nil || !got.IsValid || got.Kind != auth.KindApp || got.TenantScope != "" || got.PeerInstanceID != "" || got.KeyExpiresAt != nil {
		t.Errorf("open consumer, NULL address: ca=%+v err=%v", got, err)
	}
	if got, err := auth.AuthenticateModelKey(ctx, pool, open.Hash(), netip.Addr{}, false); err != nil || got.IsValid {
		t.Errorf("open consumer, plaintext: ca=%+v err=%v, want invalid", got, err)
	}

	if err := store.RevokeKey(ctx, pool, k.ID, store.ModelAccess{ServerAdmin: true}, stamp); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got, err := auth.AuthenticateModelKey(ctx, pool, key.Hash(), inside, true); err != nil || got.IsValid {
		t.Errorf("revoked key: ca=%+v err=%v, want invalid", got, err)
	}
}

// TestDeadModelKeys_Integration: of a live key, a revoked key, a key of a
// deactivated consumer and an id no key carries, DeadModelKeys names the
// three dead ones — gates 5 and 6 do not apply (the live key's consumer has
// an allowlist the call knows nothing of); an empty list asks nothing; a
// closed pool is an error. Red against 164: SQLSTATE 42883.
func TestDeadModelKeys_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, pool, "w018-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	stamp := store.APIStamp(admin.ID, "w018")
	mk := func(label string) (string, string) {
		c, k, _, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{
			TenantID: store.DefaultTenantID, Label: label, AllowedCIDRs: []string{"10.0.0.0/8"},
		}, nil, stamp)
		if err != nil {
			t.Fatalf("create consumer %s: %v", label, err)
		}
		return c.ID, k.ID
	}
	_, live := mk("w018-live")
	_, revoked := mk("w018-revoked")
	inactiveConsumer, inactive := mk("w018-inactive")
	if err := store.RevokeKey(ctx, pool, revoked, store.ModelAccess{ServerAdmin: true}, stamp); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := store.DeleteConsumer(ctx, pool, inactiveConsumer, store.ModelAccess{ServerAdmin: true}, stamp); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	const unknown = "00000000-0000-0000-0000-00000000feed"
	dead, err := auth.DeadModelKeys(ctx, pool, []string{live, revoked, inactive, unknown})
	if err != nil {
		t.Fatalf("DeadModelKeys: %v", err)
	}
	got := map[string]bool{}
	for _, id := range dead {
		got[id] = true
	}
	if len(dead) != 3 || !got[revoked] || !got[inactive] || !got[unknown] || got[live] {
		t.Fatalf("dead = %v, want exactly the revoked, the deactivated and the unknown id (live %s)", dead, live)
	}
	if dead, err := auth.DeadModelKeys(ctx, pool, nil); err != nil || dead != nil {
		t.Errorf("empty list: %v, %v — want nothing asked", dead, err)
	}
	pool.Close()
	if _, err := auth.DeadModelKeys(ctx, pool, []string{live}); err == nil {
		t.Error("closed pool: no error")
	}
}
