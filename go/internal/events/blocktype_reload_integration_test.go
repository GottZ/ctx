//go:build integration

// Integration gate for the WF-T3 hot-reload path (design/01-type-registry.md
// §4.3, §7-T3): a context_block_types write must reach the registry through
// the settings-channel listener WITHOUT a restart. The dispatch is probed at
// the SettingsWriteHandler seam with the exact payload the 072 notify trigger
// emits (the trigger→channel side is pinned by
// blocktype.TestRegistryGolden_Integration/notify_trigger_fires_settings_channel).
//
// NEGATIVE probe: without the entity branch the payload falls through to
// settings.Reload — wirkungslos for the registry, the snapshot keeps the old
// damping and hot_reload_updates_snapshot goes red.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/events/ -run TestBlocktypeNotify -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// blocktypeNotify crafts the payload the 072 trigger emits via
// notify_settings_write (key = row name through the COALESCE, scope column).
func blocktypeNotify(name, scope string) *pgconn.Notification {
	payload, _ := json.Marshal(map[string]string{
		"entity": "context_block_types",
		"key":    name,
		"scope":  scope,
		"op":     "UPDATE",
	})
	return &pgconn.Notification{Channel: channelSettingsWrite, Payload: string(payload)}
}

// dampedFactor extracts audit-trail's effective damping from a snapshot via a
// query no intent pattern matches.
func dampedFactor(t *testing.T, s *blocktype.Set) float64 {
	t.Helper()
	names, factors := s.DampedTypesFor("zzz probe query zzz")
	for i, n := range names {
		if n == "audit-trail" {
			return factors[i]
		}
	}
	// Since M136 the arrays carry two more damped builtins (tool-evidence,
	// tool-overview); the probe reads audit-trail's slot, not the array length.
	t.Fatalf("damped types = %v, want audit-trail among them", names)
	return 0
}

func TestBlocktypeNotifyDispatch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)
	if got := dampedFactor(t, reg.Snapshot()); got != 0.6 {
		t.Fatalf("boot damping = %v, want the chain end state 0.6 (113 seeds 0.3, 146 lifts it)", got)
	}

	cfg := config.NewStore(&config.Config{})
	h := NewSettingsWriteHandler(pool, cfg, nil, reg)

	// Hot-reload probe: psql-style UPDATE damping 0.6→0.5, then the NOTIFY
	// payload through the handler ⇒ Snapshot() serves 0.5 without restart.
	t.Run("hot_reload_updates_snapshot", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = jsonb_set(config, '{retrieval,damping_factor}', '0.5')
			  WHERE name = 'audit-trail'`); err != nil {
			t.Fatalf("update damping: %v", err)
		}
		if err := h.HandleNotification(ctx, blocktypeNotify("audit-trail", "_global"), nil); err != nil {
			t.Fatalf("HandleNotification: %v", err)
		}
		if got := dampedFactor(t, reg.Snapshot()); got != 0.5 {
			t.Errorf("damping after NOTIFY dispatch = %v, want 0.5 (entity branch missing — write fell through to settings.Reload)", got)
		}
	})

	// Tenant-scope payload (tier 2+ shape): routes to InvalidateTenant, NOT a
	// base reload — the base snapshot must keep its generation untouched.
	t.Run("tenant_scope_does_not_touch_base", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = jsonb_set(config, '{retrieval,damping_factor}', '0.7')
			  WHERE name = 'audit-trail'`); err != nil {
			t.Fatalf("update damping: %v", err)
		}
		if err := h.HandleNotification(ctx, blocktypeNotify("audit-trail", "acme-tenant"), nil); err != nil {
			t.Fatalf("HandleNotification: %v", err)
		}
		if got := dampedFactor(t, reg.Snapshot()); got != 0.5 {
			t.Errorf("base snapshot changed on a tenant-scope payload: damping = %v, want 0.5", got)
		}
	})

	// Backlog reload (reconnect window): unknown entity ⇒ reload everything,
	// including the registry — the 0.7 write above becomes visible now.
	t.Run("backlog_reloads_registry", func(t *testing.T) {
		if err := h.HandleBacklog(ctx, channelSettingsWrite, nil); err != nil {
			t.Fatalf("HandleBacklog: %v", err)
		}
		if got := dampedFactor(t, reg.Snapshot()); got != 0.7 {
			t.Errorf("damping after backlog reload = %v, want 0.7", got)
		}
	})
}
