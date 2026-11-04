package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// TestModelAliasCeilingsReadsGateway: the ceiling getter maps the gateway.*
// keys one to one (timeout in seconds ↔ policy timeout_s) and reads one
// snapshot per call, so a hot settings write reaches the next write check
// and the next registry reload.
func TestModelAliasCeilingsReadsGateway(t *testing.T) {
	store := config.NewStore(config.Defaults())
	get := modelAliasCeilings(store)
	want := modelalias.Ceilings{
		MaxOutputTokens: 32768, MaxRequestBytes: 8 << 20, EmbeddingMaxFloats: 1 << 20,
		Tenant: modelalias.TenantCeiling{MaxOutputTokens: 8192, MaxN: 1, TimeoutS: 600,
			EmbeddingMaxInputs: 32, RerankMaxDocuments: 128, ToolsMaxCount: 64},
	}
	if got := get(); got != want {
		t.Fatalf("defaults: %+v, want %+v", got, want)
	}
	next := config.Defaults()
	next.Server.DBPass = "test-password" // V11: Replace validates the whole generation
	next.Gateway.TenantAliasCeiling.Timeout = 120 * time.Second
	next.Gateway.TenantAliasCeiling.ToolsMaxCount = 0
	next.Gateway.MaxOutputTokensCeiling = 16384
	if err := store.Replace(next); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := get(); got.Tenant.TimeoutS != 120 || got.Tenant.ToolsMaxCount != 0 || got.MaxOutputTokens != 16384 {
		t.Errorf("after replace: %+v, want timeout_s 120, tools 0, max_output_tokens 16384", got)
	}
}

// TestModelAliasCeilingsCoverEveryDimension (Fremd-Review N-4): the getter
// maps oai.AliasCeiling — the W02-02 view of gateway.tenant_alias_ceiling.* —
// onto modelalias.TenantCeiling. A dimension added to one side without the
// other turns this red, and distinct values per field catch a swapped
// mapping.
func TestModelAliasCeilingsCoverEveryDimension(t *testing.T) {
	if a, b := reflect.TypeOf(oai.AliasCeiling{}).NumField(), reflect.TypeOf(modelalias.TenantCeiling{}).NumField(); a != b {
		t.Fatalf("oai.AliasCeiling has %d fields, modelalias.TenantCeiling %d — map the new dimension in modelAliasCeilings", a, b)
	}
	c := config.Defaults()
	c.Server.DBPass = "test-password"
	c.Gateway.TenantAliasCeiling = config.GatewayTenantAliasCeiling{MaxOutputTokens: 7001, MaxN: 3,
		Timeout: 301 * time.Second, EmbeddingMaxInputs: 17, RerankMaxDocuments: 99, ToolsMaxCount: 5}
	store := config.NewStore(c)
	want := modelalias.TenantCeiling{MaxOutputTokens: 7001, MaxN: 3, TimeoutS: 301,
		EmbeddingMaxInputs: 17, RerankMaxDocuments: 99, ToolsMaxCount: 5}
	if got := modelAliasCeilings(store)().Tenant; got != want {
		t.Errorf("tenant ceiling = %+v, want %+v", got, want)
	}
}
