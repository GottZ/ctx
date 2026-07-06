package dream

// MW19 principal plumbing (design/02 §7 P2): the consumer-semantics pins fire
// the REAL dispatcher preempt path (MW18) against the chatJSON seam — a
// preempt only triggers on an interactive acquire, and interactive is a
// ctx-bound privilege since MW4 (B8: no principal on the ctx = downgrade to
// background = no trigger). This file installs the ONE test-binary principal
// hook, mirroring the boot-once discipline of cmd/ctxd/main.go and the
// dispatch package's own TestMain (principal_test.go). All pre-MW19 dream
// tests acquire with plain contexts and resolve to the zero principal —
// byte-identical to the previous nil-hook behavior.
//
// NO build tag: WithInteractivePrincipal is exported for the integration
// files in package dream_test (export_test.go pattern), which compile in both
// unit and -tags integration builds.

import (
	"context"
	"os"
	"testing"

	"github.com/GottZ/ctx/internal/dispatch"
)

// preemptPrincipalKey is the test twin of the handler auth ctx key.
type preemptPrincipalKey struct{}

// WithInteractivePrincipal binds a synthetic authenticated principal to ctx —
// the only way an Acquire in the dream test binary earns ClassInteractive
// (exactly the structural claim of design/03 §4.1.1: the principal travels in
// the request ctx, never as a parameter).
func WithInteractivePrincipal(ctx context.Context) context.Context {
	return context.WithValue(ctx, preemptPrincipalKey{}, dispatch.Principal{
		ApiKeyID: "key-mw19", TenantID: "tenant-mw19", HomeScope: "scope-mw19",
	})
}

func TestMain(m *testing.M) {
	dispatch.SetPrincipalHook(func(ctx context.Context) dispatch.Principal {
		p, _ := ctx.Value(preemptPrincipalKey{}).(dispatch.Principal)
		return p
	})
	os.Exit(m.Run())
}
