//go:build integration

// Review-2 gates of W01-5, second reviewer (reports/review/W01-5-fremdreview-2.md):
// F-1 NUL bytes in label, description and q are a 400, not a 500 with an ERROR
// line (SQLSTATE 22021); F-2 the existence oracle with GRANTING payloads — the
// tier check runs on the GetConsumer row, so only GetConsumer's tenant
// predicate keeps foreign and unknown byte-equal (mutant M11); F-3 the F1
// invariant on the update path for the server-admin (mutant M22).

package handler

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

// F-1: a NUL byte in label, description or q is refused before the store.
func TestModelConsumerR2NulBytesAre400(t *testing.T) {
	f, _ := cr2Setup(t)
	own := f.fresh(f.a.id, nil)
	for _, x := range []struct {
		name, action string
		data         map[string]any
	}{
		{"create_label", "model-consumer-create", map[string]any{"label": "a\u0000b", "tenant_id": f.a.id}},
		{"create_description", "model-consumer-create", map[string]any{"label": "r2-nul", "description": "a\u0000b", "tenant_id": f.a.id}},
		{"update_label", "model-consumer-update", map[string]any{"id": own.id, "label": "a\u0000b"}},
		{"update_description", "model-consumer-update", map[string]any{"id": own.id, "description": "a\u0000b"}},
		{"list_q", "model-consumer-list", map[string]any{"q": "\u0000"}},
	} {
		r := cr2Call(t, f.hOff, f.admin, x.action, x.data)
		if r.code != 400 {
			t.Errorf("%s: %d %q, want 400 (NUL bytes must not reach the store)", x.name, r.code, cr2Err(r))
		}
	}
}

// F-2: foreign vs. unknown id with payloads a tier check would refuse —
// granting fields, a peer's rotation, a foreign scope. The handler's tier
// check runs on the GetConsumer row, so only GetConsumer's tenant predicate
// keeps these answers equal.
func TestModelConsumerR2OracleGrantingPayloads(t *testing.T) {
	f, _ := cr2Setup(t)
	bApp := f.fresh(f.b.id, nil)
	bPeer := f.fresh(f.b.id, map[string]any{"kind": "ctx-peer"})
	unknown := uuid.NewString()
	for _, x := range []struct {
		name, action, field string
		h                   *ManageHandler
		foreign             string
		extra               map[string]any
	}{
		{"update_all_models_noSS", "model-consumer-update", "id", f.hOff, bApp.id, map[string]any{"all_models": true}},
		{"update_fair_weight_SS", "model-consumer-update", "id", f.hOn, bApp.id, map[string]any{"fair_weight": 99}},
		{"update_scope_own_noSS", "model-consumer-update", "id", f.hOff, bApp.id, map[string]any{"tenant_scope": f.a.scope}},
		{"update_scope_own_SS", "model-consumer-update", "id", f.hOn, bApp.id, map[string]any{"tenant_scope": f.a.scope}},
		{"update_peer_grant_SS", "model-consumer-update", "id", f.hOn, bPeer.id, map[string]any{"all_models": true}},
		{"rotate_peer_SS", "model-key-rotate", "consumer_id", f.hOn, bPeer.id, nil},
	} {
		d := func(id string) map[string]any {
			m := map[string]any{x.field: id}
			for k, v := range x.extra {
				m[k] = v
			}
			return m
		}
		fr := cr2Call(t, x.h, f.a.ar, x.action, d(x.foreign))
		un := cr2Call(t, x.h, f.a.ar, x.action, d(unknown))
		if fr.code != un.code || !bytes.Equal(fr.body, un.body) || cr2Hdr(fr.hdr) != cr2Hdr(un.hdr) {
			t.Errorf("%s: foreign %d %q #%s / unknown %d %q #%s — existence oracle", x.name,
				fr.code, cr2Err(fr), cr2Hash(fr.body), un.code, cr2Err(un), cr2Hash(un.body))
		}
	}
}

// F-3: the F1 invariant (a scope stays inside the consumer's tenant) holds on
// update for every caller, the server-admin included.
func TestModelConsumerR2ServerAdminUpdateScopeStaysInTenant(t *testing.T) {
	f, _ := cr2Setup(t)
	aApp := f.fresh(f.a.id, nil)
	r := cr2Call(t, f.hOff, f.admin, "model-consumer-update", map[string]any{"id": aApp.id, "tenant_scope": f.b.scope})
	if r.code != 403 {
		t.Fatalf("server-admin update with tenant B's scope on tenant A's consumer: %d %q, want 403", r.code, cr2Err(r))
	}
	own := cr2Call(t, f.hOff, f.admin, "model-consumer-update", map[string]any{"id": aApp.id, "tenant_scope": f.a.scope})
	if own.code != 200 {
		t.Fatalf("server-admin update with the consumer's own tenant scope: %d %q, want 200", own.code, cr2Err(own))
	}
}

// CodeRabbit on #101 (store/tenant.go:162): the tenant-limit-set echo carries
// max_model_consumers for an unlimited cap too — an explicit null, not a
// missing key — while tenant-get keeps the key out (the shared tenant row is
// omitempty because the read path serves schemas before 161).
func TestModelConsumerR2TenantLimitSetEchoesUnlimitedCap(t *testing.T) {
	f, _ := cr2Setup(t)
	set := func(capacity any) (cr2Resp, map[string]any) {
		r := cr2Raw(t, f.hOff, f.admin, map[string]any{"action": "tenant-limit-set", "id": f.a.id,
			"data": map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": capacity}})
		tn, _ := r.m["tenant"].(map[string]any)
		return r, tn
	}
	if r, tn := set(3); r.code != 200 || tn["max_model_consumers"] != float64(3) {
		t.Fatalf("cap 3: %d %v, want 200 and max_model_consumers 3 echoed", r.code, r.m)
	}
	r, tn := set(nil)
	v, present := tn["max_model_consumers"]
	if r.code != 200 || !present || v != nil {
		t.Fatalf("unlimited cap: %d present=%v value=%v (%s) — want 200 and max_model_consumers present as null", r.code, present, v, r.body)
	}
	g := cr2Raw(t, f.hOff, f.admin, map[string]any{"action": "tenant-get", "id": f.a.id})
	gt, _ := g.m["tenant"].(map[string]any)
	if _, has := gt["max_model_consumers"]; g.code != 200 || has {
		t.Fatalf("tenant-get: %d has max_model_consumers=%v — the shared tenant row leaves the key out", g.code, has)
	}
}
