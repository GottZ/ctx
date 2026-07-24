//go:build integration

// Evokoa-Clean-Room design/03 §7 W03-3 Gates 1 and 3, against a real PG18
// testcontainer:
//
//	go test -tags=integration ./cmd/ctxd/ -run TestSchemaContractBoot -count=1 -v
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/schemacontract"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// contractBootHelperEnv routes TestSchemaContractBootHelperProcess into the
// helper body when the enforce-exit probe below re-execs the test binary.
// Unset in a normal run — the helper then skips (same idiom as
// overviewworker_priority_linux_test.go's priorityHelperEnv).
const contractBootHelperEnv = "CTX_TEST_CONTRACT_BOOT_HELPER"

// contractBootHelperDSNEnv carries the already-migrated (and, in the
// enforce-exit probe, already drift-induced) testcontainer DSN from the
// parent to the child — the child cannot share the parent's in-process
// *pgxpool.Pool across the process boundary.
const contractBootHelperDSNEnv = "CTX_TEST_CONTRACT_BOOT_DSN"

// TestSchemaContractBootHelperProcess is not a test: it is the child body
// of the G1 Helper-Process probe (pattern:
// cmd/ctxd/overviewworker_priority_linux_test.go's
// TestOverviewWorkerPriorityHelperProcess). It calls schemaContractBoot
// DIRECTLY — not the full main() — because schemaContractBoot itself is
// where the enforce+breaking os.Exit(1) lives (design/03 §4.5); invoking
// exactly that function against a real pool is a faithful, minimal re-exec
// target for "before Router-Start" without needing to also stand up the
// scheduler/backends/HTTP server this test has no interest in.
func TestSchemaContractBootHelperProcess(t *testing.T) {
	if os.Getenv(contractBootHelperEnv) == "" {
		t.Skip("helper process body — only meaningful when spawned by the contract-boot probe")
	}
	dsn := os.Getenv(contractBootHelperDSNEnv)
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "contract boot helper: missing "+contractBootHelperDSNEnv)
		os.Exit(9)
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contract boot helper: pool: %v\n", err)
		os.Exit(9)
	}
	defer pool.Close()

	cfgStore := config.NewStore(&config.Config{})
	schemaContractBoot(ctx, pool, cfgStore)
	fmt.Println("schemaContractBoot returned") // unreachable on the enforce+breaking exit path
	os.Exit(0)
}

// runContractBootHelper re-execs the test binary into
// TestSchemaContractBootHelperProcess with CTX_CONTRACT_MODE=mode and the
// given DSN, capturing combined output and the process exit code.
func runContractBootHelper(t *testing.T, dsn, mode string) (output string, exitCode int) {
	t.Helper()
	env := filterEnv(os.Environ(), schemacontract.EnvContractMode, contractBootHelperDSNEnv, contractBootHelperEnv)
	env = append(env,
		contractBootHelperEnv+"=1",
		contractBootHelperDSNEnv+"="+dsn,
		schemacontract.EnvContractMode+"="+mode,
	)

	cmd := exec.Command(os.Args[0], "-test.run=^TestSchemaContractBootHelperProcess$") //nolint:gosec // re-exec of the own test binary
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), exitErr.ExitCode()
	}
	t.Fatalf("running contract-boot helper: %v\noutput:\n%s", err, out.String())
	return "", -1
}

// filterEnv drops any existing entries for the given var names from env,
// so a parent-process ambient value can never bleed into (or silently
// duplicate against) the deliberate overrides the caller appends next.
func filterEnv(env []string, drop ...string) []string {
	dropSet := make(map[string]bool, len(drop))
	for _, k := range drop {
		dropSet[k] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !dropSet[name] {
			out = append(out, kv)
		}
	}
	return out
}

// TestSchemaContractBoot_Enforce_ExitsOnBreakingDrift is design/03 §7
// W03-3 Gate 1's enforce half: a testcontainer with an induced
// missing_object drift (DROP INDEX idx_embedding_hnsw — the same object
// design/03 §2/§7 uses throughout as the canonical live-drift example) and
// CTX_CONTRACT_MODE=enforce must exit the process with code 1, BEFORE
// anything resembling router start, and the output must name the drift and
// both documented escape hatches.
//
// ROT (documented per wave contract, not re-run as a permanent regression):
// before this wave, schemaContractBoot does not exist — cmd/ctxd does not
// compile against this test file at all (the Nichtexistenz-Beweis). The
// behavioral rot the design also calls for — "warn-Boot auf gedropptem
// Index läuft heute OHNE jeden Contract-Log durch" — was verified manually
// against the pre-W03-3 main.go (no schemaContractBoot call existed on the
// boot path, so no contract log line could appear under any mode) and is
// not re-provable after this wave lands the exact log line it describes.
func TestSchemaContractBoot_Enforce_ExitsOnBreakingDrift(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP INDEX idx_embedding_hnsw`); err != nil {
		t.Fatalf("inducing missing_object drift: %v", err)
	}

	out, exitCode := runContractBootHelper(t, dsn, schemacontract.ModeEnforce)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1 — enforce+breaking must abort before router start\noutput:\n%s", exitCode, out)
	}
	if strings.Contains(out, "schemaContractBoot returned") {
		t.Errorf("helper reached past schemaContractBoot — os.Exit(1) did not run\noutput:\n%s", out)
	}
	for _, want := range []string{"missing_object", "idx_embedding_hnsw",
		schemacontract.EnvContractMode + "=off", "CTX_SETTINGS_DISABLE=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

// TestSchemaContractBoot_Warn_LogsButBoots is Gate 1's warn half: the SAME
// induced drift under CTX_CONTRACT_MODE=warn must NOT abort — the process
// reaches past schemaContractBoot and exits 0 — but the drift must still
// be LOUD (an ERROR line naming the breaking finding), proving warn is
// "visible, not silent" rather than merely "not fatal".
func TestSchemaContractBoot_Warn_LogsButBoots(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP INDEX idx_embedding_hnsw`); err != nil {
		t.Fatalf("inducing missing_object drift: %v", err)
	}

	out, exitCode := runContractBootHelper(t, dsn, schemacontract.ModeWarn)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 — warn must never abort boot\noutput:\n%s", exitCode, out)
	}
	if !strings.Contains(out, "schemaContractBoot returned") {
		t.Errorf("helper did not reach past schemaContractBoot under warn\noutput:\n%s", out)
	}
	for _, want := range []string{"missing_object", "idx_embedding_hnsw"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q (warn must still be loud):\n%s", want, out)
		}
	}
}

// TestSchemaContractBoot_RecheckTicker_FlipsWithoutRestart is design/03 §7
// W03-3 Gate 3's re-check probe: boot on a clean (drift-free) database
// stores an ok report; a drift is THEN induced at the running
// process/pool (no restart); LatestReport() must flip to drift within the
// (1s, test-shortened) re-check interval — proving the periodic ticker
// actually runs, not just the boot-time check. Runs in-process (mode=warn
// never exits, so no Helper-Process subprocess is needed here).
func TestSchemaContractBoot_RecheckTicker_FlipsWithoutRestart(t *testing.T) {
	t.Setenv(schemacontract.EnvContractMode, schemacontract.ModeWarn)
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfgStore := config.NewStore(&config.Config{Contract: config.ContractConfig{RecheckInterval: time.Second}})

	schemaContractBoot(ctx, pool, cfgStore)

	report, ok := schemacontract.LatestReport()
	if !ok || report.Status != schemacontract.StatusOK {
		t.Fatalf("boot report: ok=%v status=%v, want ok=true status=ok — drifts: %+v", ok, report.Status, report.Drifts)
	}

	if _, err := pool.Exec(ctx, `DROP INDEX idx_embedding_hnsw`); err != nil {
		t.Fatalf("inducing post-boot drift: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := schemacontract.LatestReport(); ok && r.Status == schemacontract.StatusDrift {
			return // flipped — gate satisfied
		}
		time.Sleep(50 * time.Millisecond)
	}
	last, _ := schemacontract.LatestReport()
	t.Fatalf("LatestReport never flipped to drift within the recheck window — status stayed %q; the re-check ticker did not run (rot against boot-only)", last.Status)
}
