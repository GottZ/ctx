// janitor.go — the two shapes every background arm of the scheduler repeats:
// the deferred recover (guardPanic) and the retention/GC skeleton (janitorArm).
// Both are pure refactoring homes; neither adds a policy of its own. The log
// texts and the count key stay with the caller because operations runbooks
// quote them verbatim.

package events

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// guardPanic is the deferred recover every background arm carries: one ERROR
// line naming the arm, plus the stack, and no re-panic — a panicking arm loses
// its tick, never the ctxd process.
//
// It MUST be deferred DIRECTLY, as the first statement of the arm:
//
//	defer guardPanic("llmlog retention")
//
// and NEVER wrapped in a closure. recover() returns the panic value only when
// it is called by the very function the defer names. In `defer func() {
// guardPanic("x") }()` that function is the closure; guardPanic sits one frame
// lower, recover() yields nil, and the panic keeps unwinding out of the arm and
// takes the process with it. That mistake compiles, lints, and passes every
// test that never panics — which is why TestJanitorArmsContainPanics drives a
// real panic through every janitor arm.
func guardPanic(name string) {
	if r := recover(); r != nil {
		slog.Error("scheduler: panic in "+name, "error", r, "stack", string(debug.Stack()))
	}
}

// janitorArm runs one retention/GC arm: the store call, a WARN on failure, an
// INFO when the sweep removed anything. Nothing aborts a neighbour — every arm
// of the six-hour bundle logs its own failure and returns.
//
// The arm NAME carries the failure line, because all nine arms spell it the
// same way ("scheduler: <name> failed"). doneMsg is a parameter because the
// success line does not follow from the name — "webchat retention" reports
// "scheduler: webchat sessions deleted" — and countKey is one because eight
// arms count "rows" while runPendingWriteEviction counts "chunks". All three
// are quoted in runbooks, so none of them is assembled from a format string.
//
// janitorArm does NOT recover: the caller's own `defer guardPanic(name)` covers
// this call AND the hot-config read that five arms do before it. Folding the
// recover in here would leave those five reads outside the guard, which is a
// semantic loss the pre-wave closures did not have.
func janitorArm(ctx context.Context, name, doneMsg, countKey string,
	run func(context.Context) (int64, error), attrs ...any,
) {
	n, err := run(ctx)
	if err != nil {
		slog.Warn("scheduler: "+name+" failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info(doneMsg, append([]any{countKey, n}, attrs...)...)
	}
}
