//go:build integration

// Issue #421: POST /api/store answers and then runs the temporal afterwork of
// the write (store.RefreshTemporal) on a goroutine of its own. Before, that
// afterwork wrote content_times with an UPDATE of the block row and held the
// row to its COMMIT, and the query path's failure memo, which locks the row
// FOR NO KEY UPDATE SKIP LOCKED, skipped it: an inline embed failure of the
// freshly written block left no memo (issue #393 forced it with a trigger).
// Here the afterwork is held at its COMMIT, with every lock it took
// (testdb.HoldCommitPool), while the query runs — the two probes of #393 with
// that window forced open. The write sets content_times in its own statement
// now, and the afterwork holds no lock on the block row the memo needs.
//
//	go test -tags=integration ./internal/handler/ -run TestWriteTailLeavesTheFailureMemoItsRow -count=1 -v
package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// holdWriteTail puts the fixture's store handler on a pool whose first
// temporal afterwork is held at its COMMIT.
func (f *bfsFixture) holdWriteTail(t *testing.T) *testdb.CommitHold {
	t.Helper()
	pool, hold := testdb.HoldCommitPool(t, f.dsn, func(sql string) bool {
		return strings.Contains(sql, "DELETE FROM context_temporal")
	})
	f.storeH = NewStoreHandler(pool, staticConfigStore{cfg: &config.Config{
		Pool: config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
	}}, f.reg)
	return hold
}

func TestWriteTailLeavesTheFailureMemoItsRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	// arm_off_old_end_knows_the_backend_of_a_scope: the refused loopback dial
	// of the full-trust backend of `private` leaves a wire memo.
	t.Run("inline failure", func(t *testing.T) {
		f := bfsSetup(t)
		hold := f.holdWriteTail(t)
		f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
		f.privateEmbed = backends.TrustFull
		f.post(t, f.callerCtx, bfsCallerSecret, "", string(backends.SensCredentials))
		hold.Wait(t)

		f.query(t, 1, 0)
		if !f.memoized(t, bfsCallerSecret) {
			t.Error("the inline embed failure left no memo while the write's afterwork was held at its commit")
		}
		hold.Release()
	})

	// cancel_on_a_wire_memo_keeps_its_level/arm_on up to its first assertion:
	// the cancelled request hands the block to the arm (caller_timeout, due
	// at once).
	t.Run("cancel on a wire memo", func(t *testing.T) {
		f, b := bfcSetup(t)
		hold := f.holdWriteTail(t)
		f.post(t, f.callerCtx, bfcSlowTitle, "", "")
		hold.Wait(t)
		f.wireMemo(t, bfcSlowTitle, 3)

		f.cancelMidEmbed(t, b, f.callerCtx, bfcConfig(10*time.Second, bfcArmOn))
		callerTimeout := "caller_timeout"
		if class, attempts, due := f.memo(t, bfcSlowTitle); class != callerTimeout || attempts != 3 || !due {
			t.Errorf("after the cancel, with the write's afterwork held at its commit: memo class %q, attempts %d, due at once %v; want %q, 3, true",
				class, attempts, due, callerTimeout)
		}
		hold.Release()
	})
}
