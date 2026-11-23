package handler

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
)

// waitHealthRounds waits until no probe round of the process is in flight
// (issue #519): a round runs on after its callers left, records what it finds
// and only then leaves the registry, so after this every line it writes is
// in. Tests whose callers leave before their round ends call it before they
// count lines, and in a cleanup, so no round of theirs writes into the next
// test's capture. The guard is a deadlock guard, not a timing assumption.
func waitHealthRounds(t *testing.T) {
	t.Helper()
	guard := time.Now().Add(30 * time.Second)
	for {
		healthLines.mu.Lock()
		n := len(healthLines.rounds)
		healthLines.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(guard) {
			t.Fatalf("%d probe rounds still in flight after 30 s", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// defaultDBConnectTimeoutForTest is the connect timeout ctxd's DSN carries.
var defaultDBConnectTimeoutForTest = config.DefaultDBConnectTimeout
