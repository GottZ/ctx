package handler

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestRedactActor is the B-06-23 table (design/06 §4.10): every reader ×
// actor combination through the pure redaction function, no database.
//
// Readers: a server-admin (sees every row as stored) and a tenant-admin of
// tenant T. Actors: a server-admin whose key lives in another tenant or in T,
// a tenant-admin of T, an actor of a foreign tenant, rows without an actor
// (via sql / migration / bootstrap), and the fail-closed edges — an actor key
// whose identity fields are missing (key gone at write time), a malformed or
// missing server-admin flag, an unknown future actor_* field, an empty reader
// tenant.
func TestRedactActor(t *testing.T) {
	const (
		tenantT  = "0193a6f0-0000-7000-8000-00000000000a"
		tenantB  = "0193a6f0-0000-7000-8000-00000000000b"
		tenantD  = "00000000-0000-0000-0000-0000000d3fa0"
		keyAdmin = "0193a6f0-1111-7000-8000-000000000001"
		keyOwn   = "0193a6f0-1111-7000-8000-000000000002"
		keyB     = "0193a6f0-1111-7000-8000-000000000003"
		prinA    = "0193a6f0-2222-7000-8000-000000000001"
		prinOwn  = "0193a6f0-2222-7000-8000-000000000002"
		prinB    = "0193a6f0-2222-7000-8000-000000000003"
	)
	s := func(v string) *string { return &v }
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %v: %v", v, err)
		}
		return b
	}
	row := func(key, label *string, md map[string]any) auditRowJSON {
		m := map[string]json.RawMessage{}
		for k, v := range md {
			if r, ok := v.(json.RawMessage); ok {
				m[k] = r
				continue
			}
			m[k] = raw(v)
		}
		return auditRowJSON{
			ID: "0193a6f0-3333-7000-8000-000000000001", EntityKey: "k", Scope: "t:main", Action: "update",
			OldValue: json.RawMessage(`{"daily_calls":1}`), NewValue: json.RawMessage(`{"daily_calls":2}`),
			APIKeyID: key, ActorLabel: label, Metadata: m,
			CreatedAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		}
	}
	base := map[string]any{"via": "api", "request_id": "ab12", "tenant_id": tenantT}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	const (
		masked  = "masked"  // actor_label "server-admin", api_key_id null, no actor_* metadata
		visible = "visible" // label + key id kept, actor_principal kept, actor_tenant_id/_server_admin stripped
		none    = "none"    // no actor before, none after; actor_* metadata stripped
	)

	cases := []struct {
		name   string
		in     auditRowJSON
		tenant string // the tenant-admin reader's tenant
		want   string // outcome for the tenant-admin reader
	}{
		{"server_admin_actor_of_another_tenant", row(s(keyAdmin), s("root-admin"), with(map[string]any{
			"actor_principal": prinA, "actor_tenant_id": tenantD, "actor_server_admin": true})), tenantT, masked},
		{"server_admin_actor_whose_key_lives_in_the_readers_tenant", row(s(keyAdmin), s("root-admin"), with(map[string]any{
			"actor_principal": prinA, "actor_tenant_id": tenantT, "actor_server_admin": true})), tenantT, masked},
		{"tenant_admin_of_the_same_tenant", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": tenantT, "actor_server_admin": false})), tenantT, visible},
		{"same_tenant_in_another_spelling", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": tenantT, "actor_server_admin": false})), "0193A6F0-0000-7000-8000-00000000000A", visible},
		{"actor_of_a_foreign_tenant", row(s(keyB), s("b-owner"), with(map[string]any{
			"actor_principal": prinB, "actor_tenant_id": tenantB, "actor_server_admin": false})), tenantT, masked},
		{"no_actor_via_sql", row(nil, nil, with(map[string]any{"via": "sql"})), tenantT, none},
		{"no_actor_via_migration", row(nil, nil, map[string]any{"via": "migration", "tenant_id": tenantT}), tenantT, none},
		{"no_actor_via_bootstrap", row(nil, nil, map[string]any{"via": "bootstrap", "tenant_id": tenantT}), tenantT, none},
		{"actor_key_without_identity_fields", row(s(keyB), nil, with(nil)), tenantT, masked},
		{"label_without_key_id", row(nil, s("someone"), with(nil)), tenantT, masked},
		{"server_admin_flag_missing", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": tenantT})), tenantT, masked},
		{"server_admin_flag_malformed", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": tenantT, "actor_server_admin": "false"})), tenantT, masked},
		{"server_admin_flag_null", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": tenantT, "actor_server_admin": json.RawMessage(`null`)})), tenantT, masked},
		{"actor_tenant_malformed", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": json.RawMessage(`42`), "actor_server_admin": false})), tenantT, masked},
		{"only_identity_metadata_left", row(nil, nil, with(map[string]any{"actor_principal": prinB})), tenantT, masked},
		{"unknown_future_actor_field_of_an_own_actor", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": tenantT, "actor_server_admin": false, "actor_email": "x@y"})), tenantT, visible},
		{"actor_keys_in_another_case", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": tenantT, "actor_server_admin": false,
			"Actor_Principal": prinB, "ACTOR_LABEL": "root-admin"})), tenantT, visible},
		{"empty_reader_tenant", row(s(keyOwn), s("t-owner"), with(map[string]any{
			"actor_principal": prinOwn, "actor_tenant_id": "", "actor_server_admin": false})), "", masked},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := c.in.copyForTest()

			t.Run("server_admin_reader", func(t *testing.T) {
				got := redactActor(c.in, tenantD, true)
				if !reflect.DeepEqual(got, c.in) {
					t.Errorf("server-admin reader changed the row:\n got  %+v\n want %+v", got, c.in)
				}
			})

			t.Run("tenant_admin_reader", func(t *testing.T) {
				got := redactActor(c.in, c.tenant, false)
				var actorKeys []string
				for k := range got.Metadata {
					if strings.HasPrefix(strings.ToLower(k), "actor_") {
						actorKeys = append(actorKeys, k)
					}
				}
				sort.Strings(actorKeys)
				switch c.want {
				case masked:
					if got.ActorLabel == nil || *got.ActorLabel != "server-admin" || got.APIKeyID != nil || len(actorKeys) != 0 {
						t.Errorf("want masked: label %v key %v actor metadata %v", ptrStr(got.ActorLabel), ptrStr(got.APIKeyID), actorKeys)
					}
				case visible:
					if ptrStr(got.ActorLabel) != ptrStr(c.in.ActorLabel) || ptrStr(got.APIKeyID) != ptrStr(c.in.APIKeyID) ||
						len(actorKeys) != 1 || actorKeys[0] != "actor_principal" ||
						string(got.Metadata["actor_principal"]) != string(c.in.Metadata["actor_principal"]) {
						t.Errorf("want visible: label %v key %v actor metadata %v", ptrStr(got.ActorLabel), ptrStr(got.APIKeyID), actorKeys)
					}
				case none:
					if got.ActorLabel != nil || got.APIKeyID != nil || len(actorKeys) != 0 {
						t.Errorf("want no actor: label %v key %v actor metadata %v", ptrStr(got.ActorLabel), ptrStr(got.APIKeyID), actorKeys)
					}
				}
				// Everything that is not actor identity passes through.
				for _, k := range []string{"via", "request_id", "tenant_id"} {
					if string(got.Metadata[k]) != string(c.in.Metadata[k]) {
						t.Errorf("metadata.%s %s, want %s", k, got.Metadata[k], c.in.Metadata[k])
					}
				}
				if got.ID != c.in.ID || got.EntityKey != c.in.EntityKey || got.Scope != c.in.Scope || got.Action != c.in.Action ||
					string(got.OldValue) != string(c.in.OldValue) || string(got.NewValue) != string(c.in.NewValue) || !got.CreatedAt.Equal(c.in.CreatedAt) {
					t.Errorf("non-actor fields changed: %+v", got)
				}
			})

			if !reflect.DeepEqual(c.in, before) {
				t.Errorf("redactActor mutated its input:\n now    %+v\n before %+v", c.in, before)
			}
		})
	}
}

func ptrStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// copyForTest deep-copies a row so the purity check compares against the
// state before the call, not against an alias of it.
func (r auditRowJSON) copyForTest() auditRowJSON {
	out := r
	if r.APIKeyID != nil {
		v := *r.APIKeyID
		out.APIKeyID = &v
	}
	if r.ActorLabel != nil {
		v := *r.ActorLabel
		out.ActorLabel = &v
	}
	out.Metadata = make(map[string]json.RawMessage, len(r.Metadata))
	for k, v := range r.Metadata {
		out.Metadata[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

// TestRedactKeyState: for a reader below server-admin, the state of an api_key
// row carries neither is_admin nor principal_id — api-key-list never shows
// them to a tenant-admin, and a server-admin key living in the reader's tenant
// would otherwise name its person and its tier. Dropped on every key row, not
// only on admin keys: a gap only on admin keys would itself be the signal.
// Other entities and the server-admin reader pass unchanged; a key state that
// is not a JSON object cannot be filtered and reads as null.
func TestRedactKeyState(t *testing.T) {
	adminKey := json.RawMessage(`{"label":"root","is_admin":true,"principal_id":"0193a6f0-2222-7000-8000-000000000001","tenant_id":"t","active":true}`)
	plainKey := json.RawMessage(`{"label":"m","is_admin":false,"principal_id":"0193a6f0-2222-7000-8000-000000000002","tenant_id":"t","active":false}`)
	quota := json.RawMessage(`{"scope":"t:main","daily_calls":5}`)
	mk := func(oldV, newV json.RawMessage) auditRowJSON {
		return auditRowJSON{ID: "r", EntityKey: "k", Action: "update", OldValue: oldV, NewValue: newV, Metadata: map[string]json.RawMessage{}}
	}
	keysOfObj := func(t *testing.T, raw json.RawMessage) []string {
		t.Helper()
		if raw == nil {
			return nil
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("value %s is not an object: %v", raw, err)
		}
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	want := []string{"active", "label", "tenant_id"}

	for _, c := range []struct {
		name string
		oldV json.RawMessage
		newV json.RawMessage
	}{
		{"admin_key_create", nil, adminKey},
		{"plain_key_update", plainKey, plainKey},
		{"key_promoted_to_server_admin", plainKey, adminKey},
		{"key_delete", adminKey, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := mk(c.oldV, c.newV)
			got := redactKeyState("api_key", in, false)
			for _, v := range []json.RawMessage{got.OldValue, got.NewValue} {
				if v == nil {
					continue
				}
				if k := keysOfObj(t, v); strings.Join(k, ",") != strings.Join(want, ",") {
					t.Errorf("tenant reader: value keys %v, want %v (no is_admin, no principal_id)", k, want)
				}
			}
			if (got.OldValue == nil) != (c.oldV == nil) || (got.NewValue == nil) != (c.newV == nil) {
				t.Errorf("tenant reader: null-ness changed: old %s new %s", got.OldValue, got.NewValue)
			}
			if got := redactKeyState("api_key", in, true); string(got.OldValue) != string(c.oldV) || string(got.NewValue) != string(c.newV) {
				t.Errorf("server-admin reader: values changed: old %s new %s", got.OldValue, got.NewValue)
			}
			if string(in.OldValue) != string(c.oldV) || string(in.NewValue) != string(c.newV) {
				t.Errorf("redactKeyState mutated its input")
			}
		})
	}
	t.Run("other_entities_pass", func(t *testing.T) {
		in := mk(quota, quota)
		if got := redactKeyState("tenant_quota", in, false); string(got.OldValue) != string(quota) || string(got.NewValue) != string(quota) {
			t.Errorf("tenant_quota changed: %s / %s", got.OldValue, got.NewValue)
		}
	})
	t.Run("non_object_key_state_reads_as_null", func(t *testing.T) {
		got := redactKeyState("api_key", mk(json.RawMessage(`"principal 0193a6f0"`), json.RawMessage(`[1]`)), false)
		if got.OldValue != nil || got.NewValue != nil {
			t.Errorf("non-object key state: old %s new %s, want null", got.OldValue, got.NewValue)
		}
	})
}

// TestAuditInvisibleUpdate (W06-7 Härtung 2, Fremd-Review 2 F3): an api_key
// update whose change lies only in the fields redactKeyState removes reads,
// for a reader below server-admin, as an update with equal old and new — the
// row itself says "a hidden field changed" (a psql promotion to server-admin).
// Such rows leave the answer; equality is semantic (canonical JSON, key order
// and spacing ignored), never byte equality.
func TestAuditInvisibleUpdate(t *testing.T) {
	row := func(entity, action, oldV, newV string) (string, auditRowJSON) {
		r := auditRowJSON{ID: "r", EntityKey: "k", Action: action, Metadata: map[string]json.RawMessage{}}
		if oldV != "" {
			r.OldValue = json.RawMessage(oldV)
		}
		if newV != "" {
			r.NewValue = json.RawMessage(newV)
		}
		return entity, r
	}
	const (
		stateA  = `{"label":"m","active":true,"allowed_scopes":["a:main","a:x"],"tenant_id":"t","n":1}`
		stateA2 = `{ "n": 1.0, "tenant_id":"t", "allowed_scopes":["a:main","a:x"], "active":true, "label":"m" }`
		stateB  = `{"label":"m","active":true,"allowed_scopes":["a:x","a:main"],"tenant_id":"t","n":1}`
		stateC  = `{"label":"renamed","active":true,"allowed_scopes":["a:main","a:x"],"tenant_id":"t","n":1}`
	)
	for _, c := range []struct {
		name                     string
		entity, action, old, new string
		wantTenant               bool // left out for a tenant-admin reader
	}{
		{"equal_state_update", "api_key", "update", stateA, stateA, true},
		{"equal_in_another_key_order_and_spacing", "api_key", "update", stateA, stateA2, true},
		{"array_order_is_a_change", "api_key", "update", stateA, stateB, false},
		{"visible_change", "api_key", "update", stateA, stateC, false},
		{"both_states_unreadable_null", "api_key", "update", "", "", true},
		{"one_side_null", "api_key", "update", "", stateA, false},
		{"create_is_kept", "api_key", "create", "", stateA, false},
		{"revoke_is_kept", "api_key", "revoke", stateA, stateA, false},
		{"snapshot_is_kept", "api_key", "snapshot", "", stateA, false},
		{"entity_without_hidden_fields", "tenant_quota", "update", stateA, stateA, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			entity, r := row(c.entity, c.action, c.old, c.new)
			if got := auditInvisibleUpdate(entity, r, false); got != c.wantTenant {
				t.Errorf("tenant-admin reader: left out = %v, want %v", got, c.wantTenant)
			}
			if auditInvisibleUpdate(entity, r, true) {
				t.Errorf("server-admin reader: a row was left out")
			}
		})
	}
}
