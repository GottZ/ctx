package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelkey"
)

// modelKeyShape is one spelling of a model key as it can arrive on a
// knowledge path.
type modelKeyShape struct{ name, raw string }

// modelKeyShapes are the spellings the knowledge paths refuse without a
// database round-trip (W01-4, design/01 §4.3): the canonical key, the same key
// with the surrounding whitespace a paste carries, behind the invisible
// characters a copy from a web page or a file saved with a byte-order mark
// carries (W01-2 review F-3), and a 64-hex knowledge key glued behind the
// prefix — SanitizeKey would have stripped that to "c" + key.
func modelKeyShapes(t *testing.T) []modelKeyShape {
	t.Helper()
	k, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("modelkey.Generate: %v", err)
	}
	key := k.Plaintext()
	return []modelKeyShape{
		{"canonical", key},
		{"leading-space", " " + key},
		{"trailing-newline", key + "\r\n"},
		{"byte-order-mark", "\ufeff" + key},
		{"zero-width-space", "\u200b" + key},
		{"prefix-glued-hex", modelkey.Prefix + strings.Repeat("ab", 32)},
	}
}

// refusedWithoutPool runs resolve against a nil pool: touching it panics, so a
// clean return proves no query was even attempted.
func refusedWithoutPool(t *testing.T, resolve func() (*auth.AuthResult, error)) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("model-key shape reached the database pool: %v", r)
		}
	}()
	ar, err := resolve()
	if err != nil {
		t.Fatalf("err = %v, want a plain invalid result", err)
	}
	if ar == nil || ar.IsValid || ar.ApiKeyID != "" {
		t.Fatalf("result = %+v, want IsValid=false without a key id", ar)
	}
}

// TestModelKeyShapeRefusedWithoutDB is the unit half of the W01-4 gate: the
// credential gate (header auth and DCR admin) and both SSE re-auth seams
// refuse every model-key shape before the database (the integration halves
// count the queries: TestModelKeyShapeSSEReauth_Integration here,
// TestModelKeyShapeNeverReachesKnowledgeAuth in cmd/ctxd).
func TestModelKeyShapeRefusedWithoutDB(t *testing.T) {
	ctx := context.Background()
	events := NewEventsHandler(ctx, nil, nil, nil)
	project := NewProjectEventsHandler(nil, nil, nil)
	for _, s := range modelKeyShapes(t) {
		t.Run(s.name, func(t *testing.T) {
			t.Run("resolveCredential", func(t *testing.T) {
				refusedWithoutPool(t, func() (*auth.AuthResult, error) { return resolveCredential(ctx, nil, s.raw) })
			})
			t.Run("resolveRequestCredential", func(t *testing.T) {
				refusedWithoutPool(t, func() (*auth.AuthResult, error) {
					ar, csrf, err := resolveRequestCredential(ctx, nil, s.raw, false)
					if csrf != "" {
						t.Errorf("csrf secret %q on a header credential", csrf)
					}
					return ar, err
				})
			})
			t.Run("sse-admin-reauth", func(t *testing.T) {
				refusedWithoutPool(t, func() (*auth.AuthResult, error) { return events.hub.authenticate(ctx, s.raw, false) })
			})
			t.Run("sse-project-reauth", func(t *testing.T) {
				refusedWithoutPool(t, func() (*auth.AuthResult, error) { return project.authenticate(ctx, s.raw, false) })
			})
		})
	}
}
