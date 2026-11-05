package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/modelkey"
)

// TestAuthenticateRawRefusesModelKeyWithoutDB pins the W01-4 refusal
// (design/01 §4.3): every model-key shape — canonical, padded, behind a
// byte-order mark or a zero-width space, a hex key glued behind the prefix —
// is invalid before SanitizeKey could turn it into a hex remnant for
// ctx_auth. The pool is nil: touching it panics, so a clean return proves no
// query was attempted.
func TestAuthenticateRawRefusesModelKeyWithoutDB(t *testing.T) {
	k, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("modelkey.Generate: %v", err)
	}
	key := k.Plaintext()
	for name, raw := range map[string]string{
		"canonical":        key,
		"padded":           " \t" + key + "\r\n",
		"byte-order-mark":  "\ufeff" + key,
		"zero-width-space": "\u200b" + key,
		"prefix-glued-hex": modelkey.Prefix + strings.Repeat("ab", 32),
		// No hex survives SanitizeKey: Authenticate's own empty-key exit,
		// unchanged — AuthenticateRaw forwards everything that is not a
		// model key.
		"no-hex-garbage": "zz-yy",
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("reached the database pool: %v", r)
				}
			}()
			ar, err := AuthenticateRaw(context.Background(), nil, raw)
			if err != nil || ar == nil || ar.IsValid || ar.ApiKeyID != "" {
				t.Fatalf("AuthenticateRaw = %+v, %v; want a plain invalid result", ar, err)
			}
		})
	}
}
