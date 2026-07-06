//go:build integration

package overview_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
)

// Env plumbing for the E-B kill-mid-persist probe: the helper flag routes the
// re-execed test binary into the child body, the DSN points it at the parent's
// throwaway container.
const (
	persistKillHelperEnv = "CTX_TEST_PERSIST_KILL_HELPER"
	persistKillDSNEnv    = "CTX_TEST_PERSIST_KILL_DSN"
)

// TestPersistKillHelperProcess is not a test: it is the child body of the
// kill-mid-persist probe — a REAL overview.Rebuild against the parent's
// database. By construction it blocks inside persist's teardown TRUNCATE on
// the row lock the parent holds, i.e. mid-tx after Begin + advisory lock —
// exactly where the parent's SIGKILL lands. If the rebuild ever RETURNS, the
// parent failed to kill us in time: exit distinctly.
func TestPersistKillHelperProcess(t *testing.T) {
	if os.Getenv(persistKillHelperEnv) == "" {
		t.Skip("helper process body — only meaningful when spawned by the kill-mid-persist probe")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv(persistKillDSNEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: pool: %v\n", err)
		os.Exit(1)
	}
	stats, err := overview.Rebuild(context.Background(), pool, overview.Options{
		Resolution: 1.0, VisibleTypes: []string{"knowledge"}, OverviewTypes: []string{"knowledge"},
	})
	fmt.Fprintf(os.Stderr, "helper: rebuild returned unexpectedly: stats=%+v err=%v\n", stats, err)
	os.Exit(3)
}

// TestPersistKill_MidTxRollback is the E-B negative gate (design/05 §4.7:
// "Kill mitten im Persist ⇒ Tx-Rollback, kein Teil-Zustand — die Persist-
// Phase ist eine Tx, Bestand"): a worker child SIGKILLed while its persist
// transaction is open (post-Begin, post-advisory-lock, blocked in the
// teardown TRUNCATE) leaves the previous run's rows byte-identically in
// place — Postgres aborts the dead client's tx, nothing partial survives.
//
// Determinism: the parent holds a FOR UPDATE row lock on graph_cluster_member,
// so the child's TRUNCATE (ACCESS EXCLUSIVE) provably queues mid-tx; the
// parent watches pg_stat_activity for that waiter before killing. After the
// kill and lock release the backend of the dead client finishes the TRUNCATE,
// fails to deliver its result, and aborts — the probe waits for the backend
// to vanish before diffing.
func TestPersistKill_MidTxRollback(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		A = "019d0000-0000-7000-9000-0000000000aa"
		B = "019d0000-0000-7000-9000-0000000000ab"
		C = "019d0000-0000-7000-9000-0000000000ac"
		D = "019d0000-0000-7000-9000-0000000000ad"
		E = "019d0000-0000-7000-9000-0000000000ae"
		F = "019d0000-0000-7000-9000-0000000000af"
	)
	insBlock(t, pool, A, "private", "learnings", "A")
	insBlock(t, pool, B, "private", "learnings", "B")
	insBlock(t, pool, C, "shared", "decisions", "C")
	insBlock(t, pool, D, "work", "infrastructure", "D")
	insBlock(t, pool, E, "work", "infrastructure", "E")
	insBlock(t, pool, F, "work", "infrastructure", "F")
	insLink(t, pool, A, B, 0.9)
	insLink(t, pool, B, C, 0.9)
	insLink(t, pool, A, C, 0.9)
	insLink(t, pool, D, E, 0.9)
	insLink(t, pool, E, F, 0.9)
	insLink(t, pool, D, F, 0.9)
	insLink(t, pool, C, F, 0.05)

	opts := overview.Options{Resolution: 1.0, VisibleTypes: []string{"knowledge"}, OverviewTypes: []string{"knowledge"}}
	stats, err := overview.Rebuild(ctx, pool, opts)
	if err != nil {
		t.Fatalf("run 1 (reference rebuild): %v", err)
	}
	if stats.NodeCount != 6 {
		t.Fatalf("fixture sanity: NodeCount = %d, want 6", stats.NodeCount)
	}
	before := dumpDeterministicRows(t, pool)
	if len(before["member"]) != 6 {
		t.Fatalf("fixture sanity: %d member rows, want 6", len(before["member"]))
	}

	// The trap: hold ONE member row locked so the child's teardown TRUNCATE
	// queues behind us — inside its open persist tx.
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("lock tx: %v", err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }()
	if _, err := lockTx.Exec(ctx, `SELECT 1 FROM graph_cluster_member LIMIT 1 FOR UPDATE`); err != nil {
		t.Fatalf("row lock: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPersistKillHelperProcess$") //nolint:gosec // re-exec of the own test binary
	cmd.Env = append(os.Environ(),
		persistKillHelperEnv+"=1",
		persistKillDSNEnv+"="+pool.Config().ConnString(),
	)
	var childErr bytes.Buffer
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}

	// Wait until the child's backend provably waits on our lock — it is then
	// mid-persist inside its tx (Begin + advisory lock already taken). The
	// wait_event_type filter also prevents this poll from matching its own
	// query text; the captured backend pid keys the gone-poll below.
	waitQ := `SELECT pid FROM pg_stat_activity
	           WHERE wait_event_type = 'Lock' AND query ILIKE '%TRUNCATE graph_cluster_member%'
	           LIMIT 1`
	var backendPid int
	for deadline := time.Now().Add(60 * time.Second); ; {
		if err := pool.QueryRow(ctx, waitQ).Scan(&backendPid); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("child never reached the persist TRUNCATE; stderr: %s", childErr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	// SIGKILL mid-persist — the E-B timeout semantics (CommandContext Cancel).
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing child: %v", err)
	}
	if err := cmd.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("child did not die by SIGKILL: %v (stderr: %s)", err, childErr.String())
	}

	// Release the trap; the dead client's backend then completes the TRUNCATE,
	// fails to deliver, and aborts its tx. Wait for the backend to vanish.
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("releasing row lock: %v", err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, backendPid).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity gone-poll: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dead client's backend %d never terminated", backendPid)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The heart of the gate: NOTHING changed — the teardown rolled back, no
	// partial state, run 1's rows are byte-identical.
	after := dumpDeterministicRows(t, pool)
	for _, table := range []string{"member", "node", "edge", "meta"} {
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Errorf("%s rows changed across the killed persist (partial state!):\n  before: %v\n  after : %v",
				table, before[table], after[table])
		}
	}
	fmt.Println("kill-mid-persist: tx rolled back,", len(after["member"]), "member rows intact — no partial state")
}
