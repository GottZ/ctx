package handler

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// TestDailyRouterLanguageIsHot pins the mut:"hot" contract of dream.language
// on the API path: the handler holds no config VALUE, it reads the store when
// it builds the router. A boot-captured value would make the key hot on the
// scheduler path (newRouter, per iteration) and cold here — the same pipeline
// answering differently depending on which trigger fired it, until a restart.
func TestDailyRouterLanguageIsHot(t *testing.T) {
	store := &swapStore{}
	store.p.Store(&config.Config{Dream: config.DreamConfig{Language: ""}})

	h := NewSynthesizeHandler(nil, nil, nil, nil, store)
	ctx := context.Background()

	if got := h.dailyRouter(ctx).Language; got != "" {
		t.Fatalf("router language before swap = %q, want empty (legacy)", got)
	}

	// Config replace between two requests — no restart, no re-wiring.
	store.p.Store(&config.Config{Dream: config.DreamConfig{Language: "tr"}})

	if got := h.dailyRouter(ctx).Language; got != "tr" {
		t.Fatalf("router language after swap = %q, want %q — value was captured, not read", got, "tr")
	}

	// And back: hot means hot in both directions.
	store.p.Store(&config.Config{Dream: config.DreamConfig{Language: ""}})
	if got := h.dailyRouter(ctx).Language; got != "" {
		t.Fatalf("router language after revert = %q, want empty", got)
	}
}

// TestDailyRouterLanguageUnwiredStore pins the nil-store fallback of the
// unit-test wiring: the legacy (empty) language, which is also the registry
// default — an unwired handler can never silently localize a report series.
func TestDailyRouterLanguageUnwiredStore(t *testing.T) {
	h := NewSynthesizeHandler(nil, nil, nil, nil, nil)
	if got := h.dailyRouter(context.Background()).Language; got != "" {
		t.Fatalf("unwired store language = %q, want empty", got)
	}
}
