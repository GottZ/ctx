package main

// FOLGE-Z boot wiring (Fremd-Review 1 R1-5): bootCapabilityProbe installs the
// pool view on a registry that already loaded without one, and from then on
// an alias without policy.vision follows its chain — first through the
// immediate reload, then through every probe change. Without the wiring the
// alias stays VisionNone forever and every unit test of the parts stays
// green.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/pashagolub/pgxmock/v5"
)

func TestBootCapabilityProbeResolvesAliases(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_model_info" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"served_model_name":"qwen38-27b","has_image_understanding":true}`))
	}))
	t.Cleanup(engine.Close)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	mock.MatchExpectationsInOrder(false)
	ts := time.Date(2026, 11, 8, 16, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		mock.ExpectQuery(`SELECT .+ FROM context_model_aliases`).WillReturnRows(
			pgxmock.NewRows([]string{"id", "scope", "name", "kind", "role", "sensitivity", "policy", "description", "enabled", "created_at", "updated_at"}).
				AddRow("1", backends.GlobalScope, "chat", "chat", "chat", "credentials", []byte(`{}`), "", true, ts, ts))
	}
	reg := modelalias.NewRegistry(mock)
	if err := reg.Reload(context.Background()); err != nil { // the boot load, before any view
		t.Fatal(err)
	}
	if a, _ := modelalias.Lookup(reg.Snapshot(), "chat", backends.GlobalScope); a.VisionSource != modelalias.VisionNone {
		t.Fatalf("precondition: boot load without a view resolved %q", a.VisionSource)
	}

	pool := backends.NewPool(nil, nil)
	pool.SeedSnapshotForTest([]backends.Backend{{ID: "spark", Name: "spark-chat", Host: engine.URL, Protocol: backends.ProtocolOpenAI,
		ProviderClass: backends.ProviderLlamaCpp, Scope: backends.GlobalScope, Trust: backends.TrustFull,
		Roles: []string{backends.RoleChat}, Enabled: true, ModelMap: map[string]backends.ModelSpec{"default": {Model: "qwen38-27b"}}}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bootCapabilityProbe(ctx, pool, reg, config.NewStore(config.Defaults()))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a, ok := modelalias.Lookup(reg.Snapshot(), "chat", backends.GlobalScope); ok && a.EffectiveVision && a.VisionSource == modelalias.VisionBackend {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	a, _ := modelalias.Lookup(reg.Snapshot(), "chat", backends.GlobalScope)
	t.Fatalf("R1-5: 5 s after boot the alias chat reads effective_vision=%v source=%q, want true/backend (probe → hook → registry reload)", a.EffectiveVision, a.VisionSource)
}
