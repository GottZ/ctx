package store

import (
	"context"
	"strings"
	"testing"
)

// CreateApiKey input validation runs before any DB call, so we can test
// the early-return error paths with a nil pool.

func TestCreateApiKey_EmptyLabel(t *testing.T) {
	_, _, err := CreateApiKey(context.Background(), nil, "", "private", nil)
	if err == nil {
		t.Fatal("expected error for empty label, got nil")
	}
	if !strings.Contains(err.Error(), "label is required") {
		t.Errorf("expected 'label is required' error, got: %v", err)
	}
}

func TestCreateApiKey_EmptyHomeScope(t *testing.T) {
	_, _, err := CreateApiKey(context.Background(), nil, "test", "", nil)
	if err == nil {
		t.Fatal("expected error for empty home_scope, got nil")
	}
	if !strings.Contains(err.Error(), "home_scope is required") {
		t.Errorf("expected 'home_scope is required' error, got: %v", err)
	}
}

func TestCreateApiKey_BothEmpty(t *testing.T) {
	// label is validated first → error mentions label.
	_, _, err := CreateApiKey(context.Background(), nil, "", "", nil)
	if err == nil {
		t.Fatal("expected error for empty inputs, got nil")
	}
	if !strings.Contains(err.Error(), "label is required") {
		t.Errorf("expected label error (validated first), got: %v", err)
	}
}
