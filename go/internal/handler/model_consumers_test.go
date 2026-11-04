package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// TestTierSelfServiceAtTheTable pins the tierSelfService stage of the
// dispatch enforcer itself (design/01 §4.6): the self-service lock sits in the
// pinned table, so a handler that forgot its own check is not fail-open.
func TestTierSelfServiceAtTheTable(t *testing.T) {
	row := manageAction{tier: tierSelfService, handle: dropAuthReq(func(*ManageHandler, http.ResponseWriter, *http.Request) {})}
	tenant := "01a1a3e6-0000-7000-8000-000000000001"
	serverAdmin := &auth.AuthResult{IsValid: true, IsAdmin: true, TenantID: tenant}
	tenantAdmin := &auth.AuthResult{IsValid: true, TenantID: tenant, TenantRole: auth.RoleAdmin}
	member := &auth.AuthResult{IsValid: true, TenantID: tenant, TenantRole: auth.RoleMember}
	on := func() bool { return true }
	off := func() bool { return false }
	for _, tc := range []struct {
		name string
		ar   *auth.AuthResult
		open func() bool
		pass bool
	}{
		{"server_admin_switch_off", serverAdmin, off, true},
		{"server_admin_no_switch", serverAdmin, nil, true},
		{"tenant_admin_switch_on", tenantAdmin, on, true},
		{"tenant_admin_switch_off", tenantAdmin, off, false},
		{"tenant_admin_no_switch", tenantAdmin, nil, false},
		{"member_switch_on", member, on, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			got := enforceManageActionTier(rec, row, manageRequest{Action: "model-consumer-create"}, tc.ar, "req", tc.open)
			if got != tc.pass {
				t.Fatalf("enforceManageActionTier = %v, want %v (body %s)", got, tc.pass, rec.Body.String())
			}
			if !tc.pass && (rec.Code != http.StatusForbidden || rec.Body.String() != `{"error":"admin key required","success":false}`+"\n") {
				t.Fatalf("refusal %d %q, want the shared 403 admin key required", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestConsumerViewGoldenKeys pins the JSON keys of the consumer and key
// views (design/05 §3.2, precedent TestStatusGoldenKeys). No key of either
// may carry a hash or a plaintext — absent by structure.
func TestConsumerViewGoldenKeys(t *testing.T) {
	now := time.Now()
	scope := "s"
	raw, err := json.Marshal(consumerView{TenantScope: &scope, Keys: []keyView{{ExpiresAt: &now}}, Peer: &peerView{}})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var keys []map[string]json.RawMessage
	if err := json.Unmarshal(top["keys"], &keys); err != nil {
		t.Fatal(err)
	}
	var peer map[string]json.RawMessage
	if err := json.Unmarshal(top["peer"], &peer); err != nil {
		t.Fatal(err)
	}
	names := func(m map[string]json.RawMessage) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	want := map[string][]string{
		"consumer": {"active", "admission_lane", "all_models", "allow_plaintext", "allowed_cidrs", "created_at",
			"created_by_label", "description", "fair_weight", "id", "keys", "keys_total", "kind", "label",
			"last_used_at", "peer", "prompt_sensitivity", "status", "tenant_id", "tenant_scope", "tenant_slug"},
		"key":  {"created_at", "expires_at", "hint", "id", "last_used_at", "revoked_at", "rotated_from", "status"},
		"peer": {"bound_at", "instance_id"},
	}
	got := map[string][]string{"consumer": names(top), "key": names(keys[0]), "peer": names(peer)}
	secret := regexp.MustCompile(`(?i)hash|secret|token|^plaintext$|api_key|^key$`)
	for typ, w := range want {
		if !reflect.DeepEqual(got[typ], w) {
			t.Errorf("%s view keys %v, want %v", typ, got[typ], w)
		}
		for _, k := range got[typ] {
			if secret.MatchString(k) {
				t.Errorf("%s view key %q is secret-shaped", typ, k)
			}
		}
	}
}

// TestConsumerStatusGo is the Go half of the status table (design/05 §3.2)
// with the boundaries: expires_at == now is dead, expires_at == now + warn is
// expiring. TestConsumerStatusParity holds it against the SQL predicates.
func TestConsumerStatusGo(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	warn := 48 * time.Hour
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	live := func(exp *time.Time) store.ModelKeyInfo { return store.ModelKeyInfo{ID: "k", ExpiresAt: exp} }
	revoked := store.ModelKeyInfo{ID: "r", RevokedAt: at(-time.Hour)}
	for _, tc := range []struct {
		name   string
		active bool
		keys   []store.ModelKeyInfo
		want   string
	}{
		{"inactive", false, []store.ModelKeyInfo{live(nil)}, store.ConsumerStatusInactive},
		{"no_key_empty", true, nil, store.ConsumerStatusNoKey},
		{"no_key_revoked", true, []store.ModelKeyInfo{revoked}, store.ConsumerStatusNoKey},
		{"no_key_expires_now", true, []store.ModelKeyInfo{live(at(0))}, store.ConsumerStatusNoKey},
		{"rotating", true, []store.ModelKeyInfo{live(nil), live(at(time.Hour))}, store.ConsumerStatusRotating},
		{"expiring_inside", true, []store.ModelKeyInfo{live(at(time.Hour)), revoked}, store.ConsumerStatusExpiring},
		{"expiring_at_the_edge", true, []store.ModelKeyInfo{live(at(warn))}, store.ConsumerStatusExpiring},
		{"active_past_the_edge", true, []store.ModelKeyInfo{live(at(warn + time.Second))}, store.ConsumerStatusActive},
		{"active_no_expiry", true, []store.ModelKeyInfo{live(nil)}, store.ConsumerStatusActive},
	} {
		if got := consumerStatus(tc.active, tc.keys, now, warn); got != tc.want {
			t.Errorf("%s: consumerStatus = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCIDRsNarrow pins the prefix-wise containment of the allowed_cidrs
// field tier: a subset (or the same list) narrows, anything else widens.
func TestCIDRsNarrow(t *testing.T) {
	ps := func(ss ...string) []netip.Prefix {
		out := make([]netip.Prefix, len(ss))
		for i, s := range ss {
			out[i] = netip.MustParsePrefix(s)
		}
		return out
	}
	for _, tc := range []struct {
		cur  []string
		next []netip.Prefix
		want bool
	}{
		{[]string{"10.0.0.0/8"}, ps("10.0.0.0/8"), true},
		{[]string{"10.0.0.0/8"}, ps("10.1.0.0/16", "10.2.3.4/32"), true},
		{[]string{"10.0.0.0/8", "fd00::/8"}, ps("fd00:1::/32"), true},
		{[]string{"10.0.0.0/8"}, ps("10.0.0.0/7"), false},
		{[]string{"10.0.0.0/8"}, ps("10.1.0.0/16", "192.168.0.0/16"), false},
		{[]string{"10.0.0.0/8"}, ps("::ffff:10.0.0.0/104"), false},
		{[]string{"garbage"}, ps("10.0.0.0/8"), false},
	} {
		if got := cidrsNarrow(tc.cur, tc.next); got != tc.want {
			t.Errorf("cidrsNarrow(%v, %v) = %v, want %v", tc.cur, tc.next, got, tc.want)
		}
	}
}

// TestKeyExpiryPerKind pins E-01-4: default and ceiling per kind, 0 = none.
func TestKeyExpiryPerKind(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	year := 365 * 24 * time.Hour
	p := consumerPolicy{lifetimeApp: year}
	if at, err := p.keyExpiry("app", nil, now); err != nil || at == nil || !at.Equal(now.Add(year)) {
		t.Errorf("app without expiry: %v %v, want now + 365 d", at, err)
	}
	if at, err := p.keyExpiry("ctx-peer", nil, now); err != nil || at != nil {
		t.Errorf("ctx-peer without expiry, lifetime 0: %v %v, want none", at, err)
	}
	over := now.Add(year + time.Second)
	if _, err := p.keyExpiry("app", &over, now); err == nil {
		t.Error("app beyond the ceiling: accepted, want an error")
	}
	if at, err := p.keyExpiry("ctx-peer", &over, now); err != nil || !at.Equal(over) {
		t.Errorf("ctx-peer with an expiry, lifetime 0 (no ceiling): %v %v, want it kept", at, err)
	}
	past := now.Add(-time.Second)
	if _, err := p.keyExpiry("ctx-peer", &past, now); err == nil {
		t.Error("expiry in the past: accepted, want an error")
	}
	p.lifetimePeer = time.Hour
	if at, err := p.keyExpiry("ctx-peer", nil, now); err != nil || at == nil || !at.Equal(now.Add(time.Hour)) {
		t.Errorf("ctx-peer with lifetime 1 h: %v %v, want now + 1 h", at, err)
	}
}

// TestRotationGraceCeilingMatchesStore pins V40's grace ceiling to the
// store's (config may not import store): exactly store.MaxRotationGrace
// builds, one second more is refused.
func TestRotationGraceCeilingMatchesStore(t *testing.T) {
	t.Setenv("CONTEXT_DB_PASSWORD", "mc-test")
	secs := int64(store.MaxRotationGrace / time.Second)
	for _, tc := range []struct {
		v      int64
		refuse bool
	}{{secs, false}, {secs + 1, true}} {
		c, issues := config.Build([]config.Override{{Key: "gateway.key_rotation_grace", Value: strconv.FormatInt(tc.v, 10)}}, nil)
		refused := c.Gateway.Consumers.KeyRotationGrace != time.Duration(tc.v)*time.Second
		if refused != tc.refuse {
			t.Errorf("gateway.key_rotation_grace=%d: refused=%v, want %v (issues %+v)", tc.v, refused, tc.refuse, issues)
		}
	}
}
