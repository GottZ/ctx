//go:build integration

// Issues #366 and #370 with two real processes: the test binary re-executes
// itself twice as helper processes against one test database (the pattern of
// cmd/ctxd/identity_integration_test.go), each with its own pool, and each
// runs the production arm — the re-embed migration cycle, or the distiller
// tick — while the parent reads the database.
//
//	go test -tags=integration ./internal/events/ -run 'TestMultiProcess' -count=1 -v
package events

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

const (
	mpHelperEnv = "CTX_TEST_MULTI_PROCESS_HELPER"
	mpDSNEnv    = "CTX_TEST_MULTI_PROCESS_DSN"
	mpRootEnv   = "CTX_TEST_MULTI_PROCESS_ROOT"
)

// TestMultiProcessHelperProcess is the helper's body; it runs only when the
// parent test starts this binary with mpHelperEnv set to the arm's name.
func TestMultiProcessHelperProcess(t *testing.T) {
	role := os.Getenv(mpHelperEnv)
	if role == "" {
		t.Skip("helper process of TestMultiProcess")
	}
	ctx := context.Background()
	var logs bytes.Buffer
	var mu sync.Mutex
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	pool, err := pgxpool.New(ctx, os.Getenv(mpDSNEnv))
	if err != nil {
		t.Fatalf("helper pool: %v", err)
	}
	defer pool.Close()
	switch role {
	case "verify":
		s := newIndexTestScheduler(pool)
		s.embedVerifySync = true
		router := miRouter(t)
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if err := s.runEmbedMigrationCycle(ctx, router, verifyTestCfg()); err != nil {
				t.Fatalf("cycle: %v", err)
			}
			var green bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM context_embed_migrations WHERE verify_report->>'result' = 'green')`).Scan(&green); err != nil {
				t.Fatalf("verdict: %v", err)
			}
			if green {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	case "distill":
		root := os.Getenv(mpRootEnv)
		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: root}},
			head:     map[string]int64{root: 3000},
			hasNew:   map[string]bool{root: true},
		}
		var reads sync.Mutex
		src.readFn = func(after int64) (distillsource.Batch, error) {
			reads.Lock()
			defer reads.Unlock()
			// A run of three batches lasts longer than the other process's
			// start, so the two processes' ticks meet.
			time.Sleep(1500 * time.Millisecond)
			if after >= 3000 {
				return distillsource.Batch{Watermark: after, Complete: true}, nil
			}
			return a6Batch(after+1000, fmt.Sprintf("b%d", after), a6Para(fmt.Sprintf("p%d", after), 400)), nil
		}
		dir, err := os.MkdirTemp("/var/tmp", "ctx-k9-mp-")
		if err != nil {
			t.Fatalf("dump dir: %v", err)
		}
		defer os.RemoveAll(dir)
		s := dfScheduler(pool, a6Config(dir, 4000, 200), src)
		s.distillStartupSweep(ctx)
		for i := 0; i < 6; i++ {
			s.distillOnce(ctx, dfNoDemand)
			time.Sleep(50 * time.Millisecond)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, ln := range strings.Split(logs.String(), "\n") {
		if strings.Contains(ln, "scheduler:") {
			fmt.Println("HELPER-LOG " + ln)
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// mpStart starts one helper process of role against dsn.
func mpStart(t *testing.T, role, dsn string, extra ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^TestMultiProcessHelperProcess$", "-test.v") //nolint:gosec // re-exec of the own test binary
	cmd.Env = append(os.Environ(), mpHelperEnv+"="+role, mpDSNEnv+"="+dsn)
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper %s: %v", role, err)
	}
	return cmd, &out
}

func mpWait(t *testing.T, cmd *exec.Cmd, out *bytes.Buffer, label string) string {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("helper %s: %v\n%s", label, err, out.String())
		}
	case <-time.After(150 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("helper %s did not end within 150s\n%s", label, out.String())
	}
	return out.String()
}

func TestMultiProcess_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// Two processes run the migration cycle against one verifying migration
	// until a verdict stands. One gate runs: one "embed verify started" line
	// across both, one verdict.
	t.Run("two_processes_one_verify_scan", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBWithDSN(t)
		seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
		migID := seedMigrationRow(t, pool, "verifying")
		v := pgvec.NewVector(verifyUnitVec(11))
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_blocks (category, title, content, scope, sensitivity,
			        embedding, embed_model, embedding_next, embed_model_next, created_at, updated_at)
			SELECT 'learnings', 'mp-' || g, 'mp content', 'shared', 'internal',
			       $1, $2, $1, $3, now() - interval '1 day' + g * interval '1 millisecond', now()
			  FROM generate_series(1, 20000) g`, v, migFromModel, migToModel); err != nil {
			t.Fatalf("seed: %v", err)
		}
		createPlainIndexT(t, pool, migrationPendingIndexName)
		// A parallel HNSW build needs more dynamic shared memory than the test
		// container's /dev/shm holds (SQLSTATE 53100); the helpers' sessions
		// build serially.
		if _, err := pool.Exec(ctx, `DO $$ BEGIN
			EXECUTE format('ALTER DATABASE %I SET max_parallel_maintenance_workers = 0', current_database());
			END $$`); err != nil {
			t.Fatalf("serial index builds: %v", err)
		}
		a, outA := mpStart(t, "verify", dsn)
		b, outB := mpStart(t, "verify", dsn)
		logs := mpWait(t, a, outA, "A") + mpWait(t, b, outB, "B")
		started := strings.Count(logs, "embed verify started")
		awaited := strings.Count(logs, "another session runs the gate of this watermark")
		st := readVerifyRow(t, pool, migID)
		t.Logf("gates started %d, gate starts that awaited another session's run %d, status %q", started, awaited, st.Status)
		if rep := parseReport(t, st.Report); started != 1 || rep.Result != verifyGreen || rep.Integrity.RowsChecked != 20000 {
			t.Fatalf("two processes: %d gate(s) started, verdict %s — want one gate and its green verdict\n%s",
				started, rep.Result, logs)
		}
	})

	// Two processes start their distiller and tick the same root, each read
	// slowed down. At no point do two runs stand open, and no run is closed by
	// the other process's sweep.
	t.Run("two_processes_one_distiller_run_at_a_time", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBWithDSN(t)
		const root = "20261119_020000_mp0001"
		stop := make(chan struct{})
		maxOpen := 0
		var pollErr error
		polled := make(chan struct{})
		go func() {
			defer close(polled)
			for {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
				var n int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM distill_run WHERE outcome = 'running'`).Scan(&n); err != nil {
					pollErr = err
					return
				}
				if n > maxOpen {
					maxOpen = n
				}
			}
		}()
		a, outA := mpStart(t, "distill", dsn, mpRootEnv+"="+root)
		b, outB := mpStart(t, "distill", dsn, mpRootEnv+"="+root)
		logs := mpWait(t, a, outA, "A") + mpWait(t, b, outB, "B")
		close(stop)
		<-polled
		if pollErr != nil {
			t.Fatalf("poll: %v", pollErr)
		}
		rows := dfRows(t, pool)
		killed := 0
		for _, r := range rows {
			if r.outcome == "killed" {
				killed++
			}
		}
		t.Logf("max open runs %d, journal rows %d, killed %d", maxOpen, len(rows), killed)
		if maxOpen > 1 || killed != 0 || dfDerive(t, pool, distillSourceKey(dfLabel, dfScope, root)) != 3000 {
			t.Fatalf("two processes: max open runs %d, killed rows %d, derivation %d — want one run at a time, none killed, the range covered\nrows %+v\n%s",
				maxOpen, killed, dfDerive(t, pool, distillSourceKey(dfLabel, dfScope, root)), rows, logs)
		}
	})
}
