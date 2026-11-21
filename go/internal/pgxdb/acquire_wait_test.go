package pgxdb

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

type countingNoter struct{ n int }

func (c *countingNoter) NoteAbandonedAcquire() { c.n++ }

// TestAcquireWaitTracerTellsOnlyAnAbandonedWait — issue #453: the tracer
// tells the noter of an acquire that began on a live context and ended on a
// cancellation, and of nothing else.
func TestAcquireWaitTracerTellsOnlyAnAbandonedWait(t *testing.T) {
	tr := AcquireWaitTracer{}
	for _, tc := range []struct {
		name   string
		err    error
		atDoor bool
		want   int
	}{
		{"acquired", nil, false, 0},
		{"cancelled while waiting", context.Canceled, false, 1},
		{"cancelled, wrapped", errors.Join(errors.New("pgxpool: acquire"), context.Canceled), false, 1},
		{"failed otherwise", errors.New("failed to connect: password authentication failed"), false, 0},
		{"deadline", context.DeadlineExceeded, false, 0},
		{"context ended before the acquire", context.Canceled, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noter := &countingNoter{}
			ctx, cancel := context.WithCancel(WithAcquireWaitNoter(context.Background(), noter))
			defer cancel()
			if tc.atDoor {
				cancel()
			}
			ctx = tr.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
			tr.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{Err: tc.err})
			if noter.n != tc.want {
				t.Errorf("noter told %d times, want %d", noter.n, tc.want)
			}
		})
	}
	// A context without a noter is not touched (no panic).
	tr.TraceAcquireEnd(context.Background(), nil, pgxpool.TraceAcquireEndData{Err: context.Canceled})
}
