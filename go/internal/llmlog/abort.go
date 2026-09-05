package llmlog

import (
	"context"
	"errors"

	"github.com/GottZ/ctx/internal/dispatch"
)

// AbortClass classifies a dispatcher-caused cancel of one attempt's runCtx
// (MW10/MW11, design/05 §4.4c/B-R8/B-R9): "preempted"/"reaped" via errors.Is
// over context.Cause — wrap-safe, NEVER sentinel identity (a decorated
// cause, e.g. fmt.Errorf with %w plus target origin, would silently fall to
// "" under ==) and never generic ctx.Err(). Everything else — parent cancel
// (shutdown, dream-off, client disconnect), plain wire errors — returns "".
//
// It lives next to the Abort* vocabulary it returns: both wire families (the
// chat chain walk and the embed sequence) apply the same §4.4c rule, and a
// per-package copy is how one of them would keep writing "canceled" after a
// change to the other.
func AbortClass(runCtx context.Context) string {
	cause := context.Cause(runCtx)
	switch {
	case errors.Is(cause, dispatch.ErrPreempted):
		return AbortPreempted
	case errors.Is(cause, dispatch.ErrReaped):
		return AbortReaped
	default:
		return ""
	}
}
