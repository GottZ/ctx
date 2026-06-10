package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Validation runs before any DB call, so the early-return error paths are
// testable with a nil tx/pool (same pattern as api_keys_test.go).

func TestUpsertSetting_EmptyKey(t *testing.T) {
	err := UpsertSetting(context.Background(), nil, "", GlobalScope, json.RawMessage(`1`), nil)
	if err == nil || !strings.Contains(err.Error(), "key is required") {
		t.Errorf("expected 'key is required' error, got: %v", err)
	}
}

func TestUpsertSetting_EmptyScope(t *testing.T) {
	err := UpsertSetting(context.Background(), nil, "rerank.blend_weight", "", json.RawMessage(`1`), nil)
	if err == nil || !strings.Contains(err.Error(), "scope is required") {
		t.Errorf("expected 'scope is required' error, got: %v", err)
	}
}

func TestUpsertSetting_InvalidJSON(t *testing.T) {
	for _, val := range []json.RawMessage{nil, {}, json.RawMessage(`{"unclosed":`)} {
		err := UpsertSetting(context.Background(), nil, "k", GlobalScope, val, nil)
		if err == nil || !strings.Contains(err.Error(), "valid JSON") {
			t.Errorf("value %q: expected 'valid JSON' error, got: %v", val, err)
		}
	}
}

func TestDeleteSetting_EmptyKey(t *testing.T) {
	_, err := DeleteSetting(context.Background(), nil, "", GlobalScope, nil)
	if err == nil || !strings.Contains(err.Error(), "key is required") {
		t.Errorf("expected 'key is required' error, got: %v", err)
	}
}

func TestDeleteSetting_EmptyScope(t *testing.T) {
	_, err := DeleteSetting(context.Background(), nil, "k", "", nil)
	if err == nil || !strings.Contains(err.Error(), "scope is required") {
		t.Errorf("expected 'scope is required' error, got: %v", err)
	}
}

func TestLoadSettingOverrides_EmptyScope(t *testing.T) {
	_, err := LoadSettingOverrides(context.Background(), nil, "")
	if err == nil || !strings.Contains(err.Error(), "scope is required") {
		t.Errorf("expected 'scope is required' error, got: %v", err)
	}
}

// GlobalScope is the sentinel the 051 unique constraint anchors on — a
// rename would silently fork the settings identity.
func TestGlobalScope_Sentinel(t *testing.T) {
	if GlobalScope != "_global" {
		t.Errorf("GlobalScope = %q, want \"_global\"", GlobalScope)
	}
	if !strings.HasPrefix(GlobalScope, "_") {
		t.Error("GlobalScope must live in the '_'-reserved namespace")
	}
}
