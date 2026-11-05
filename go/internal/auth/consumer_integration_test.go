//go:build integration

// W01-6: AuthenticateModelKey and ReauthenticateModelKey against a real
// ctx_model_auth (migration 161). Every K20 column reaches ConsumerAuth, the
// address and transport arguments reach gates 5 and 6, and the re-check uses
// the credential of the original request.
//
//	go test -tags=integration -p 1 -count=1 -run 'Consumer|ModelKey' ./internal/auth/
package auth_test

import (
	"context"
	"errors"
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

func TestReauthenticateModelKey_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, pool, "w016-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	stamp := store.APIStamp(admin.ID, "w016")
	_, k, key, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{
		TenantID: store.DefaultTenantID, Label: "w016-cidr", AllowedCIDRs: []string{"10.0.0.0/8"},
	}, nil, stamp)
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}
	ca, err := auth.AuthenticateModelKey(ctx, pool, key.Hash(), netip.MustParseAddr("10.9.9.9"), true)
	if err != nil || !ca.IsValid {
		t.Fatalf("first auth: ca=%+v err=%v", ca, err)
	}
	// The re-check runs with the ORIGINAL address and transport: with NULL
	// it would fail the allowlist.
	again, err := auth.ReauthenticateModelKey(ctx, pool, ca)
	if err != nil || !again.IsValid || again.ModelKeyID != k.ID {
		t.Fatalf("re-auth of a live key: ca=%+v err=%v", again, err)
	}
	if err := store.RevokeKey(ctx, pool, k.ID, store.ModelAccess{ServerAdmin: true}, stamp); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if again, err := auth.ReauthenticateModelKey(ctx, pool, ca); err != nil || again.IsValid {
		t.Errorf("re-auth after revoke: ca=%+v err=%v, want invalid", again, err)
	}
	for name, in := range map[string]*auth.ConsumerAuth{"nil": nil, "no-credential": {IsValid: true, ModelKeyID: k.ID}} {
		if _, err := auth.ReauthenticateModelKey(ctx, pool, in); !errors.Is(err, auth.ErrNoModelCredential) {
			t.Errorf("%s: err=%v, want ErrNoModelCredential", name, err)
		}
	}
	pool.Close()
	if _, err := auth.ReauthenticateModelKey(ctx, pool, ca); err == nil {
		t.Error("re-auth on a closed pool: no error")
	}
}
