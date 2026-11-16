package events

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

// TestEmbedExcludedReconcileArmSurvives: a pass that panics, then one that
// fails, then one that succeeds — the arm keeps ticking through all three.
func TestEmbedExcludedReconcileArmSurvives(t *testing.T) {
	var calls atomic.Int32
	prevFn, prevIv := reconcileEmbedExcluded, embedExcludedReconcileInterval
	t.Cleanup(func() { reconcileEmbedExcluded, embedExcludedReconcileInterval = prevFn, prevIv })
	embedExcludedReconcileInterval = 10 * time.Millisecond
	reconcileEmbedExcluded = func(context.Context, *pgxpool.Pool) (store.EmbedExcludedReconcileResult, error) {
		switch calls.Add(1) {
		case 1:
			panic("a pass that panics")
		case 2:
			return store.EmbedExcludedReconcileResult{}, errors.New("a pass that fails")
		}
		return store.EmbedExcludedReconcileResult{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Scheduler{}).runEmbedExcludedReconcile(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := calls.Load(); n < 3 {
		t.Errorf("the arm ran %d passes, want at least 3: it stopped after a panic or a failure", n)
	}
}
