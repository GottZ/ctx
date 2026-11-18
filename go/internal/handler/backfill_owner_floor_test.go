package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// TestScopeFloorSettingKeyIsTheRegistryKey pins scopeFloorSettingKey to the
// registry key of the floor and to its tenancy: backfillFloor reads the rows
// on that key, and only a tenant-overridable key has rows outside _global.
func TestScopeFloorSettingKeyIsTheRegistryKey(t *testing.T) {
	k, ok := config.KeyByName(scopeFloorSettingKey)
	if !ok {
		t.Fatalf("%s is not a registry key", scopeFloorSettingKey)
	}
	if k.Type != "scope_floor" || k.Tenancy != "tenant-overridable" {
		t.Errorf("%s: type %q tenancy %q, want scope_floor, tenant-overridable", scopeFloorSettingKey, k.Type, k.Tenancy)
	}
}
