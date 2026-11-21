package pgxdb

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AcquireWaitNoter is told when a pool acquire on its context ended because
// that context was cancelled: the request waited for a database connection
// (a full pool, a connect, the health ping of an idle one) and its caller
// went away meanwhile. The request log marks such a client abort as a wait
// (issue #453) — the server had no connection ready, an operator signal.
type AcquireWaitNoter interface {
	NoteAbandonedAcquire()
}

type acquireWaitKey struct{}

// acquireAtTheDoorKey marks an acquire whose context had already ended when
// it began: it waited for nothing (pgxpool's pool answers it at once).
type acquireAtTheDoorKey struct{}

// WithAcquireWaitNoter returns ctx carrying n for AcquireWaitTracer.
func WithAcquireWaitNoter(ctx context.Context, n AcquireWaitNoter) context.Context {
	return context.WithValue(ctx, acquireWaitKey{}, n)
}

// AcquireWaitTracer is the pool's tracer (store.NewPool): at the end of an
// acquire that began on a live context and failed on a cancellation it tells
// the context's AcquireWaitNoter. Nothing else — the query hooks are empty,
// and an acquire that succeeded, failed otherwise, began on a context that
// had already ended (it waited for nothing), or runs on a context without a
// noter, is not touched.
type AcquireWaitTracer struct{}

// TraceQueryStart is the empty query hook a pgx tracer must have.
func (AcquireWaitTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

// TraceQueryEnd is the empty query hook a pgx tracer must have.
func (AcquireWaitTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceAcquireStart hands the context through; one that has already ended
// is marked, so its failure is not told as a wait. The mark is the only
// allocation, on that path alone.
func (AcquireWaitTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if ctx.Err() != nil {
		return context.WithValue(ctx, acquireAtTheDoorKey{}, true)
	}
	return ctx
}

// TraceAcquireEnd tells the noter of an acquire that ended on a
// cancellation.
func (AcquireWaitTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if data.Err == nil || !errors.Is(data.Err, context.Canceled) || ctx.Value(acquireAtTheDoorKey{}) != nil {
		return
	}
	if n, ok := ctx.Value(acquireWaitKey{}).(AcquireWaitNoter); ok {
		n.NoteAbandonedAcquire()
	}
}
