//go:build integration

package store_test

// Store gates of wave W06-15 (design/06 §3.5, §7 W06-15 (d), (e), (g)): a
// row round-trips through UpsertGatewayLimit and LoadGatewayLimits as a full
// replacement, the access rule keeps a tenant-admin to its own consumers and
// tenant and away from the instance and the envelope (one not-found error
// for unknown, malformed and foreign subjects), every write is stamped for
// the audit row, and the tenant prune removes the rows inside its stamped
// transaction so their audit rows keep via='offboarding' and the tenant.
//
//	go test -tags=integration -p 1 -run 'TestGatewayLimits' ./internal/store/ -count=1 -v

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/store"
)

func glI64(v int64) *int64 { return &v }

// TestGatewayLimitsStoreRoundTrip: upsert, load, replace (a nil dimension
// clears it), delete — and the stamp's key as updated_by and audit actor.
func TestGatewayLimitsStoreRoundTrip(t *testing.T) {
	f := newM168Fixture(t)
	ctx := context.Background()
	actor, _, err := store.CreateApiKey(ctx, f.pool, "gl-actor", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	stamp := store.APIStamp(actor.ID, "gl-req-1")
	key := store.GatewayLimitKey{Kind: gatewaylimits.SubjectConsumer, ConsumerID: f.consumerA, Tier: gatewaylimits.TierServer, Op: keyquota.OpChat}
	row, err := store.UpsertGatewayLimit(ctx, f.pool, store.ModelAccess{ServerAdmin: true}, key, gatewaylimits.Values{RPM: glI64(5), TPM: glI64(900)}, stamp)
	if err != nil {
		t.Fatalf("UpsertGatewayLimit: %v", err)
	}
	if row.ID == "" || row.RPM == nil || *row.RPM != 5 || row.TPM == nil || *row.TPM != 900 {
		t.Errorf("stored row %+v, want rpm 5 tpm 900 with an id", row)
	}
	if _, err := store.UpsertGatewayLimit(ctx, f.pool, store.ModelAccess{ServerAdmin: true}, key, gatewaylimits.Values{RPM: glI64(6)}, stamp); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	rows, err := store.LoadGatewayLimits(ctx, f.pool)
	if err != nil {
		t.Fatalf("LoadGatewayLimits: %v", err)
	}
	if len(rows) != 1 || rows[0].RPM == nil || *rows[0].RPM != 6 || rows[0].TPM != nil || rows[0].ConsumerID != f.consumerA ||
		rows[0].Kind != gatewaylimits.SubjectConsumer || rows[0].Tier != gatewaylimits.TierServer || rows[0].Op != keyquota.OpChat {
		t.Errorf("loaded %+v, want one consumer/server/chat row with rpm 6 and tpm cleared (full replacement)", rows)
	}
	var by, via string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(updated_by::text, ''), (SELECT metadata->>'via' FROM context_settings_audit
		WHERE entity_type = 'gateway_limits' ORDER BY id DESC LIMIT 1) FROM context_gateway_limits`).Scan(&by, &via); err != nil {
		t.Fatalf("read updated_by: %v", err)
	}
	if by != actor.ID || via != "api" {
		t.Errorf("updated_by %q via %q, want the stamp's key %q and via api", by, via, actor.ID)
	}
	deleted, err := store.DeleteGatewayLimit(ctx, f.pool, store.ModelAccess{ServerAdmin: true}, key, stamp)
	if err != nil || !deleted {
		t.Errorf("DeleteGatewayLimit: %v %v, want deleted", deleted, err)
	}
	deleted, err = store.DeleteGatewayLimit(ctx, f.pool, store.ModelAccess{ServerAdmin: true}, key, stamp)
	if err != nil || deleted {
		t.Errorf("second DeleteGatewayLimit: %v %v, want false without error", deleted, err)
	}
}

// TestGatewayLimitsStoreAccess is gate (d) at the store: a tenant-admin's
// access reaches its own consumer and tenant; another tenant's consumer or
// tenant, an unknown or malformed id, the instance and the envelope are one
// ErrGatewayLimitSubject and write nothing.
func TestGatewayLimitsStoreAccess(t *testing.T) {
	f := newM168Fixture(t)
	ctx := context.Background()
	stamp := store.APIStamp("", "gl-req-2")
	own := store.ModelAccess{TenantID: f.tenantA}
	v := gatewaylimits.Values{RPM: glI64(1)}
	ok := []store.GatewayLimitKey{
		{Kind: gatewaylimits.SubjectConsumer, ConsumerID: f.consumerA, Tier: gatewaylimits.TierTenant, Op: keyquota.OpChat},
		{Kind: gatewaylimits.SubjectTenant, TenantID: f.tenantA, Tier: gatewaylimits.TierTenant, Op: keyquota.OpChat},
	}
	for _, k := range ok {
		if _, err := store.UpsertGatewayLimit(ctx, f.pool, own, k, v, stamp); err != nil {
			t.Errorf("own subject %+v: %v", k, err)
		}
	}
	refused := []store.GatewayLimitKey{
		{Kind: gatewaylimits.SubjectConsumer, ConsumerID: f.consumerB, Tier: gatewaylimits.TierTenant, Op: keyquota.OpChat},
		{Kind: gatewaylimits.SubjectConsumer, ConsumerID: "00000000-0000-7000-8000-000000000000", Tier: gatewaylimits.TierTenant, Op: keyquota.OpChat},
		{Kind: gatewaylimits.SubjectConsumer, ConsumerID: "not-a-uuid", Tier: gatewaylimits.TierTenant, Op: keyquota.OpChat},
		{Kind: gatewaylimits.SubjectTenant, TenantID: f.tenantB, Tier: gatewaylimits.TierTenant, Op: keyquota.OpChat},
		{Kind: gatewaylimits.SubjectInstance, Tier: gatewaylimits.TierServer, Op: keyquota.OpChat},
		{Kind: gatewaylimits.SubjectTenantEnvelope, TenantID: f.tenantA, Tier: gatewaylimits.TierServer, Op: keyquota.OpChat},
	}
	for _, k := range refused {
		if _, err := store.UpsertGatewayLimit(ctx, f.pool, own, k, v, stamp); !errors.Is(err, store.ErrGatewayLimitSubject) {
			t.Errorf("tenant-admin on %+v: %v, want ErrGatewayLimitSubject", k, err)
		}
		if _, err := store.DeleteGatewayLimit(ctx, f.pool, own, k, stamp); !errors.Is(err, store.ErrGatewayLimitSubject) {
			t.Errorf("tenant-admin delete on %+v: %v, want ErrGatewayLimitSubject", k, err)
		}
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM context_gateway_limits`).Scan(&n); err != nil || n != len(ok) {
		t.Errorf("rows after the access probes: %d (%v), want %d (the refused writes wrote nothing)", n, err, len(ok))
	}
}

// TestGatewayLimitsPruneTenant is gate (g): the tenant prune removes the
// rows of the tenant's consumers and of the tenant itself inside its stamped
// transaction — every delete audit row carries via='offboarding' and the
// tenant — and leaves another tenant's rows; there is no other delete path
// than the manage action.
func TestGatewayLimitsPruneTenant(t *testing.T) {
	f := newM168Fixture(t)
	ctx := context.Background()
	actor, _, err := store.CreateApiKey(ctx, f.pool, "gl-prune-actor", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, ins := range []struct{ kind, id string }{
		{"consumer", f.consumerA}, {"tenant", f.tenantA}, {"tenant_envelope", f.tenantA}, {"consumer", f.consumerB},
	} {
		if err := f.glInsert(t, ins.kind, ins.id, "server", "chat", map[string]string{"daily_tokens": "7"}); err != nil {
			t.Fatalf("insert %v: %v", ins, err)
		}
	}
	if err := store.PruneTenant(ctx, f.pool, f.tenantA, store.APIStamp(actor.ID, "gl-prune")); err != nil {
		t.Fatalf("PruneTenant: %v", err)
	}
	var left, other int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE consumer_id = $1::uuid OR tenant_id = $2::uuid),
		count(*) FILTER (WHERE consumer_id = $3::uuid) FROM context_gateway_limits`, f.consumerA, f.tenantA, f.consumerB).Scan(&left, &other); err != nil {
		t.Fatal(err)
	}
	if left != 0 || other != 1 {
		t.Errorf("after the prune: %d rows of tenant A left (want 0), %d of tenant B (want 1)", left, other)
	}
	rows, err := f.pool.Query(ctx, `SELECT entity_key, COALESCE(metadata->>'via', ''), COALESCE(metadata->>'tenant_id', '')
		FROM context_settings_audit WHERE entity_type = 'gateway_limits' AND action = 'delete' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var key, via, tenant string
		if err := rows.Scan(&key, &via, &tenant); err != nil {
			t.Fatal(err)
		}
		n++
		if via != "offboarding" || tenant != f.tenantA {
			t.Errorf("prune audit row %s: via %q tenant_id %q, want offboarding and %q (deleted inside the stamped transaction, before the consumer)", key, via, tenant, f.tenantA)
		}
	}
	if n != 3 {
		t.Errorf("%d delete audit rows, want 3 (consumer, tenant, envelope rows of tenant A)", n)
	}
}
