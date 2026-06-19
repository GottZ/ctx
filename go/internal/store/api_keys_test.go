package store

import (
	"context"
	"strings"
	"testing"
)

// CreateApiKey input validation runs before any DB call, so we can test
// the early-return error paths with a nil pool.

func TestCreateApiKey_EmptyLabel(t *testing.T) {
	_, _, err := CreateApiKey(context.Background(), nil, "", "private", nil, "")
	if err == nil {
		t.Fatal("expected error for empty label, got nil")
	}
	if !strings.Contains(err.Error(), "label is required") {
		t.Errorf("expected 'label is required' error, got: %v", err)
	}
}

func TestCreateApiKey_EmptyHomeScope(t *testing.T) {
	_, _, err := CreateApiKey(context.Background(), nil, "test", "", nil, "")
	if err == nil {
		t.Fatal("expected error for empty home_scope, got nil")
	}
	if !strings.Contains(err.Error(), "home_scope is required") {
		t.Errorf("expected 'home_scope is required' error, got: %v", err)
	}
}

func TestCreateApiKey_BothEmpty(t *testing.T) {
	// label is validated first → error mentions label.
	_, _, err := CreateApiKey(context.Background(), nil, "", "", nil, "")
	if err == nil {
		t.Fatal("expected error for empty inputs, got nil")
	}
	if !strings.Contains(err.Error(), "label is required") {
		t.Errorf("expected label error (validated first), got: %v", err)
	}
}

// Scope-format gate (G03): '_'-prefixed scopes are SYSTEM-RESERVED
// ('_global' settings sentinel, 051). Negative probe 2026-06-10: before the
// gate these inputs fell through to the INSERT (nil-pool panic in tests).

func TestCreateApiKey_ReservedHomeScope(t *testing.T) {
	for _, scope := range []string{"_global", "_x"} {
		_, _, err := CreateApiKey(context.Background(), nil, "label", scope, nil, "")
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("home_scope %q: expected 'reserved' error, got: %v", scope, err)
		}
	}
}

func TestCreateApiKey_ReservedAllowedScope(t *testing.T) {
	_, _, err := CreateApiKey(context.Background(), nil, "label", "tenant-a", []string{"shared", "_global"}, "")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("expected 'reserved' error for '_global' in allowed_scopes, got: %v", err)
	}
}
