package toolboot

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// cleanEnv blanks every CTX_/CONTEXT_ variable the test process happens to
// carry, so a shell with a live .env sourced into it cannot decide what these
// tests see. An empty value counts as unset in config.FromEnv, which is what
// makes this a reset rather than a second set of values.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "CTX_") || strings.HasPrefix(k, "CONTEXT_") {
			t.Setenv(k, "")
		}
	}
}

// deadCtx is an already-cancelled context: store.NewPool fails on its first
// wait instead of walking ten retries with a backoff up to 30s. The tests
// below care about WHICH callback fires, not about how long the pool tries.
func deadCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type record struct {
	reports  int
	issues   []config.Issue
	aborting bool
	poolErrs int
}

func (r *record) report(issues []config.Issue, aborting bool) {
	r.reports++
	r.issues = issues
	r.aborting = aborting
}

func (r *record) poolErr(error) { r.poolErrs++ }

func (r *record) field(name string) (config.Issue, bool) {
	for _, is := range r.issues {
		if is.Field == name {
			return is, true
		}
	}
	return config.Issue{}, false
}

// A config error reaches report ONCE, with aborting set, and the pool is
// never touched — the whole point of the order in G14.
func TestOpenReportsConfigErrorOnceAndOpensNoPool(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CONTEXT_DB_PASSWORD", "x")
	// Same label on both sides of the distill source split: one watermark
	// series for two sources, a cross-field error Validate alone can see.
	t.Setenv("CTX_DISTILL_CTX_SOURCE_LABEL", "hermes")

	var r record
	sess, ok := Open(deadCtx(t), r.report, r.poolErr)

	if ok {
		t.Fatalf("Open reported success on a config error")
	}
	if sess != nil {
		t.Fatalf("Open returned a session on the abort path: %+v", sess)
	}
	if r.reports != 1 {
		t.Errorf("report called %d times, want exactly 1", r.reports)
	}
	if !r.aborting {
		t.Errorf("report got aborting=false for a SeverityError issue")
	}
	if r.poolErrs != 0 {
		t.Errorf("poolErr called %d times on the config path, want 0", r.poolErrs)
	}
	is, found := r.field("distill.ctx_source_label")
	if !found {
		t.Fatalf("the label collision is missing from the reported issues: %+v", r.issues)
	}
	if is.Severity != config.SeverityError {
		t.Errorf("label collision has severity %v, want %v", is.Severity, config.SeverityError)
	}
}

// A warning-only config does NOT abort: report still fires exactly once, with
// aborting=false, and the boot proceeds to the pool. This is the case a
// per-issue callback could not express without every caller buffering — four
// of the five entry points would have started printing warnings they never
// printed before.
func TestOpenReportsWarnOnlyWithoutAborting(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CONTEXT_DB_PASSWORD", "x")
	t.Setenv("CONTEXT_DB_HOST", "nx.invalid")
	// Graph expansion on while blend_weight stays at 1.0: a warning, not an
	// error — the config is usable, the combination is just destructive.
	t.Setenv("CTX_GRAPH_EXPAND_ENABLED", "true")

	var r record
	sess, ok := Open(deadCtx(t), r.report, r.poolErr)

	if ok || sess != nil {
		t.Fatalf("Open succeeded against an unreachable host: ok=%v sess=%+v", ok, sess)
	}
	if r.reports != 1 {
		t.Errorf("report called %d times, want exactly 1", r.reports)
	}
	if r.aborting {
		t.Fatalf("report got aborting=true for a warning-only config: %+v", r.issues)
	}
	is, found := r.field("rerank.blend_weight")
	if !found {
		t.Fatalf("the blend_weight warning is missing from the reported issues: %+v", r.issues)
	}
	if is.Severity == config.SeverityError {
		t.Errorf("blend_weight came back as an error, want a warning")
	}
	if r.poolErrs != 1 {
		t.Errorf("poolErr called %d times after a warning-only config, want exactly 1", r.poolErrs)
	}
}

// A clean config that cannot reach a database: report fires once with nothing
// to say, poolErr fires once, and the two failures stay distinguishable.
func TestOpenReportsPoolFailureOnce(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CONTEXT_DB_PASSWORD", "x")
	t.Setenv("CONTEXT_DB_HOST", "nx.invalid")

	var r record
	sess, ok := Open(deadCtx(t), r.report, r.poolErr)

	if ok || sess != nil {
		t.Fatalf("Open succeeded against an unreachable host: ok=%v sess=%+v", ok, sess)
	}
	if r.reports != 1 {
		t.Errorf("report called %d times, want exactly 1", r.reports)
	}
	if r.aborting {
		t.Fatalf("report got aborting=true for a valid config: %+v", r.issues)
	}
	if len(r.issues) != 0 {
		t.Errorf("a clean env produced issues: %+v", r.issues)
	}
	if r.poolErrs != 1 {
		t.Errorf("poolErr called %d times, want exactly 1", r.poolErrs)
	}
}
