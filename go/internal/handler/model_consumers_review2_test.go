package handler

// Review-2 gates of W01-5 without a database (reports/review/W01-5-fremdreview-2.md):
// the E3 default of the self-service switch is pinned (F-C, mutant M21), and
// consumerPolicy reads every gateway consumer setting from the snapshot — the
// pin TestGatewayConsumerKeysReachConsumers proves the keys reach the config
// struct, this one proves the handler reads each of them (F-E, mutants
// M22/M23: a lifetime or the expiry warning silently fixed).

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
)

// F-C: DECISIONS E3 — gateway.tenant_self_service is OFF by default; a
// tenant-admin grants nothing until the operator opens it.
func TestTenantSelfServiceDefaultOff(t *testing.T) {
	cfg := config.Defaults()
	if cfg.Gateway.Consumers.TenantSelfService {
		t.Fatal("gateway.tenant_self_service defaults to true, want false (DECISIONS E3: server-admin only until opened)")
	}
	h := &ManageHandler{cfg: config.NewStore(cfg)}
	if h.selfServiceOpen() {
		t.Fatal("selfServiceOpen() is true on the defaults, want false (gateway.enabled and tenant_self_service are both off)")
	}
}

// F-E: every consumer setting reaches consumerPolicy with its own value —
// distinct non-default values per key, so a swapped, dropped or fixed field
// shows up.
func TestConsumerPolicyMapsEverySetting(t *testing.T) {
	cfg := config.Defaults()
	cfg.Gateway.Enabled = true
	c := &cfg.Gateway.Consumers
	c.TenantSelfService = true
	c.KeyMaxLifetimeApp = 11 * time.Hour
	c.KeyMaxLifetimePeer = 13 * time.Hour
	c.KeyRotationGrace = 17 * time.Minute
	c.KeyExpiryWarn = 19 * time.Hour
	c.ListMaxLimit = 23
	c.ChainMax = 29
	c.AuditTrailMax = 31
	h := &ManageHandler{cfg: config.NewStore(cfg)}
	got := h.consumerPolicy()
	want := consumerPolicy{
		enabled: true, selfService: true,
		lifetimeApp: 11 * time.Hour, lifetimePeer: 13 * time.Hour,
		rotationGrace: 17 * time.Minute, expiryWarn: 19 * time.Hour,
		listMax: 23, chainMax: 29, auditMax: 31,
	}
	if got != want {
		t.Fatalf("consumerPolicy() = %+v, want %+v (every gateway consumer key must reach the handler with its own value)", got, want)
	}
	if !h.selfServiceOpen() {
		t.Fatal("selfServiceOpen() false with gateway.enabled and tenant_self_service both on")
	}
}
