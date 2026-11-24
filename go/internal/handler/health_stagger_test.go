package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// statusBackend answers GET / with status at once.
func statusBackend(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestHealthSlowFirstCandidateKeepsTheWholeBudget (issue #519, the review of
// #530 point 1) — the shares of a role's candidates cut a first candidate
// that answers slower than its share, and when every later one fails the
// role was reported down although its first candidate works (before #530 it
// had the whole budget and served). The first candidate now runs on while
// the later ones are tried, a share a later one leaves by failing fast goes
// back to it, and its answer counts within the round's whole budget. The
// table of the review, at a budget of 400 ms (the review's 4 s scaled):
// the first answers after 150, 250 or 390 ms; the others refuse the
// connection, answer 503 or hang.
func TestHealthSlowFirstCandidateKeepsTheWholeBudget(t *testing.T) {
	const budget = 400 * time.Millisecond
	setHealthBudget(t, budget)
	freshHealthNotice(t)
	others := map[string]func(t *testing.T) string{
		"refused": closedPortHost,
		"503":     func(t *testing.T) string { return statusBackend(t, http.StatusServiceUnavailable) },
		"hanging": func(t *testing.T) string { return slowBackend(t, time.Hour, nil) },
	}
	for _, n := range []int{2, 3} {
		for _, after := range []time.Duration{150 * time.Millisecond, 250 * time.Millisecond, 390 * time.Millisecond} {
			for kind, other := range others {
				t.Run(fmt.Sprintf("%d candidates, first after %v, others %s", n, after, kind), func(t *testing.T) {
					var firstHits atomic.Int32
					up := slowBackend(t, 0, nil)
					id := fmt.Sprintf("i519-slowfirst-%d-%d-%s", n, after.Milliseconds(), kind)
					snap := []backends.Backend{
						operatorRow(id+"-e", up, backends.RoleEmbed, backends.RoleDream),
						operatorRow(id+"-1", slowBackend(t, after, &firstHits), backends.RoleSynthesis),
					}
					for i := 2; i <= n; i++ {
						snap = append(snap, operatorRow(fmt.Sprintf("%s-%d", id, i), other(t), backends.RoleSynthesis))
					}
					h := roundHandler(brokenPool(t), seededBackends(snap...))
					logs := installRejectLogCapture(t)
					got := askHealth(h, 0)
					waitHealthRounds(t)
					if got["chat"] != "ok" {
						t.Errorf("services %v, want chat=ok — the first candidate answers within the budget", got)
					}
					if hits := firstHits.Load(); hits != 1 {
						t.Errorf("the first candidate saw %d requests, want 1", hits)
					}
					wantLines(t, logs, backends.RoleSynthesis, snap[1].Name, 0, 0)
				})
			}
		}
	}
}
