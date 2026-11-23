package handler

// Issue #519 (a surviving mutant of #504): the status collector probes with
// /health's budget. Both feed the same healthNotice, and a collector on a
// budget of its own would judge a backend that answers between the two
// budgets reachable in one and unreachable in the other. Driven through the
// collector's production path (buildCheap) against a backend that answers
// only when the test lets it: with the budget the probe ends at the budget,
// the chat role is an error and the backend's line names the deadline; a
// collector off the budget waits until the backend answers.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

func TestStatusCollectorProbesWithTheHealthBudget(t *testing.T) {
	setHealthBudget(t, 200*time.Millisecond)
	var hold atomic.Bool
	var hits atomic.Int32
	hold.Store(true)
	up := slowBackend(t, 0, nil)
	chat := operatorRow("i519-collector", gateBackend(t, &hold, &hits), backends.RoleSynthesis)
	bp := seededBackends(operatorRow("i519-collector-e", up, backends.RoleEmbed, backends.RoleDream), chat)
	cfg := healthTestConfig()
	c := NewStatusCollector(brokenPool(t), bp, &fakeDreamController{}, config.NewStore(cfg), nil, nil)
	t.Cleanup(func() {
		hold.Store(false)
		waitHealthRounds(t)
	})
	logs := installRejectLogCapture(t)

	done := make(chan *cheapSnapshot, 1)
	go func() { done <- c.buildCheap(context.Background(), cfg) }()
	// The backend answers once the test lets it, ten budgets later: a
	// collector on a budget of its own (/health's 4 s, say) gets that answer.
	release := time.AfterFunc(2*time.Second, func() { hold.Store(false) })
	defer release.Stop()
	snap := <-done
	waitHealthRounds(t)

	if got := snap.health.Services["chat"]; got != "error" {
		t.Errorf("the collector's health block says chat=%q, want error — it did not probe with the budget", got)
	}
	recs := linesAt(logs, slog.LevelWarn, backends.RoleSynthesis, chat.Name)
	if len(recs) != 1 || !strings.Contains(fmt.Sprint(recs[0].attrs["error"]), context.DeadlineExceeded.Error()) {
		t.Errorf("the backend's lines %+v, want one WARN whose error is the budget's deadline", recs)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("the backend saw %d requests, want 1", n)
	}
}
