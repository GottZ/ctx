package store

import (
	"context"
	"strings"
	"testing"
)

// --- secret name format (Go-side validation, no DB CHECK) ---.

func TestValidSecretName_Accepts(t *testing.T) {
	for _, name := range []string{
		"openrouter-main",
		"a",
		"0key",
		"with.dots_and-dashes",
		strings.Repeat("a", 128),
	} {
		if !ValidSecretName(name) {
			t.Errorf("ValidSecretName(%q) = false, want true", name)
		}
	}
}

func TestValidSecretName_Rejects(t *testing.T) {
	for _, name := range []string{
		"",
		"Upper",
		"-leading-dash",
		".leading-dot",
		"_leading-underscore",
		"has space",
		"has:colon", // ':' is the break-glass stdin field separator
		strings.Repeat("a", 129),
	} {
		if ValidSecretName(name) {
			t.Errorf("ValidSecretName(%q) = true, want false", name)
		}
	}
}

// --- validation early-returns (nil tx/pool, no DB touch) ---.

func TestPutSecret_InvalidName(t *testing.T) {
	_, err := PutSecret(context.Background(), nil, "Bad Name", GlobalScope, []byte{1}, []byte{2}, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Errorf("expected 'invalid name' error, got: %v", err)
	}
}

func TestPutSecret_EmptyScope(t *testing.T) {
	_, err := PutSecret(context.Background(), nil, "ok-name", "", []byte{1}, []byte{2}, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "scope is required") {
		t.Errorf("expected 'scope is required' error, got: %v", err)
	}
}

func TestPutSecret_MissingSealedBytes(t *testing.T) {
	if _, err := PutSecret(context.Background(), nil, "ok-name", GlobalScope, nil, []byte{2}, 1, nil); err == nil {
		t.Error("expected error for empty nonce, got nil")
	}
	if _, err := PutSecret(context.Background(), nil, "ok-name", GlobalScope, []byte{1}, nil, 1, nil); err == nil {
		t.Error("expected error for empty ciphertext, got nil")
	}
}

func TestPutSecret_KeyVersionFloor(t *testing.T) {
	_, err := PutSecret(context.Background(), nil, "ok-name", GlobalScope, []byte{1}, []byte{2}, 0, nil)
	if err == nil || !strings.Contains(err.Error(), "key_version") {
		t.Errorf("expected key_version error, got: %v", err)
	}
}

func TestDeleteSecret_EmptyName(t *testing.T) {
	_, err := DeleteSecret(context.Background(), nil, "", GlobalScope, nil)
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected 'name is required' error, got: %v", err)
	}
}

func TestListSecretMeta_EmptyScope(t *testing.T) {
	_, err := ListSecretMeta(context.Background(), nil, "")
	if err == nil || !strings.Contains(err.Error(), "scope is required") {
		t.Errorf("expected 'scope is required' error, got: %v", err)
	}
}
