package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// pool.go has no pure functions — NewPool and sleepCtx both depend on
// external resources (pgxpool connection, timer channels).
//
// sleepCtx is a thin wrapper around select{} and could be tested with
// a cancelled context, but it is not exported.

func TestNewPool_RequiresDB(t *testing.T) {
	t.Skip("requires database connection — NewPool connects to PostgreSQL")
	// Integration test plan:
	// 1. Test with valid DSN — verify pool is created and ping succeeds.
	// 2. Test with invalid DSN — verify error after maxRetries.
	// 3. Test with context cancellation during retry — verify clean abort.
}

func TestSleepCtx_CancelledContext(t *testing.T) {
	// sleepCtx is unexported but we can test it indirectly since we're
	// in the same package.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	err := sleepCtx(ctx, 10*time.Second)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestSleepCtx_ShortDuration(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	err := sleepCtx(ctx, 1*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Errorf("expected nil error for completed sleep, got %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("sleep took too long: %v", elapsed)
	}
}

func TestSleepCtx_ZeroDuration(t *testing.T) {
	ctx := context.Background()
	err := sleepCtx(ctx, 0)
	if err != nil {
		t.Errorf("expected nil error for zero-duration sleep, got %v", err)
	}
}
