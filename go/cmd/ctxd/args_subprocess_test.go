package main

// #246 — the command line of the real binary, run as a child process: no
// argument may ever be ignored into a boot. The child is this test binary
// re-executed into TestArgsHelperProcess, which sets os.Args and calls the
// production main() — the same entry the image runs as /ctx, so what is
// probed is the dispatch order of main itself, not a function next to it.
//
// The "database" is a TCP listener on loopback that counts the connections it
// is offered and closes them. Every child gets a COMPLETE, valid DB
// configuration pointing at that listener, so a child that boots does reach
// the pool and is counted; a child that answers its argument before the boot
// leaves the count at 0. The boot control case proves the harness itself: the
// empty command line must be counted, otherwise a 0 elsewhere would prove
// nothing (wrong env names, wrong port).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/provenance"
)

// argsHelperEnv routes TestArgsHelperProcess into the helper body when a probe
// below re-execs the test binary; argsHelperArgvEnv carries the child's
// command line (JSON array, so empty and space-carrying arguments survive).
const (
	argsHelperEnv     = "CTX_TEST_ARGS_HELPER"
	argsHelperArgvEnv = "CTX_TEST_ARGS_HELPER_ARGV"
)

// TestArgsHelperProcess is not a test: it is the child body of the #246
// probes. It installs the requested command line and runs the production
// main(). Every answer to an argument ends in os.Exit; a return means main
// served and shut down, which no probe here asks for.
func TestArgsHelperProcess(t *testing.T) {
	if os.Getenv(argsHelperEnv) == "" {
		t.Skip("helper process body — only meaningful when spawned by the #246 command-line probes")
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(argsHelperArgvEnv)), &args); err != nil {
		fmt.Fprintf(os.Stderr, "args helper: argv: %v\n", err)
		os.Exit(9)
	}
	os.Args = append([]string{"ctxd"}, args...)
	main()
	fmt.Println("main returned")
	os.Exit(3)
}

// countingDB is the stand-in database: a loopback listener that counts every
// connection it accepts and closes it at once.
type countingDB struct {
	ln    net.Listener
	conns atomic.Int64
}

func newCountingDB(t *testing.T) *countingDB {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	db := &countingDB{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			db.conns.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return db
}

func (db *countingDB) port() int { return db.ln.Addr().(*net.TCPAddr).Port }

// sinkAddr returns the address of a loopback listener that answers every
// connection with a close — the children's LISTEN_ADDR, so the -health case's
// probe fails fast with a transport error. A listener held open for the whole
// test rather than a closed port: a released port can be handed to a parallel
// subtest's counting database, and a health probe dialing it would be counted
// there as a database connection.
func sinkAddr(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("127.0.0.1:%d", newCountingDB(t).port())
}

// argsChildEnv is the child's environment: the parent's, minus everything that
// could configure ctxd or libpq on its own (CONTEXT_*, CTX_*, PG*,
// LISTEN_ADDR), plus a complete DB configuration aimed at db and the helper
// routing. A child's config therefore depends on this function alone.
func argsChildEnv(db *countingDB, args []string, listenAddr string) []string {
	env := make([]string, 0, len(os.Environ())+10)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "CONTEXT_") || strings.HasPrefix(name, "CTX_") ||
			strings.HasPrefix(name, "PG") || name == "LISTEN_ADDR" {
			continue
		}
		env = append(env, kv)
	}
	argv, _ := json.Marshal(args)
	return append(env,
		argsHelperEnv+"=1",
		argsHelperArgvEnv+"="+string(argv),
		"CONTEXT_DB_HOST=127.0.0.1",
		fmt.Sprintf("CONTEXT_DB_PORT=%d", db.port()),
		"CONTEXT_DB=ctx_i246_probe",
		"CONTEXT_DB_USER=ctx_i246_probe",
		"CONTEXT_DB_PASSWORD=ctx_i246_probe",
		"CONTEXT_DB_SSLMODE=disable",
		"LISTEN_ADDR="+listenAddr,
	)
}

// argsChildBudget bounds how long a probe waits for a child. A child that
// answers its argument exits in milliseconds; one that boots instead sits in
// the pool's connect retries for minutes (store.NewPool: 10 attempts, back-off
// up to 30 s). The budget is generous because a PASSING run never waits it out
// — it only pays for a slow test-binary start under -race on a loaded runner —
// and clamped under the test binary's own -timeout so the probe fails as
// itself (the probeDeadline precedent of the worker priority probe).
func argsChildBudget(t *testing.T) time.Duration {
	t.Helper()
	budget := 30 * time.Second
	if td, ok := t.Deadline(); ok {
		if margin := time.Until(td) - 10*time.Second; margin < budget {
			budget = margin
		}
	}
	if budget < 5*time.Second {
		budget = 5 * time.Second
	}
	return budget
}

type argsChildResult struct {
	exited         bool // false: still running at the budget and killed
	code           int
	stdout, stderr string
	elapsed        time.Duration
}

// runArgsChild runs the binary with args against db and waits up to the
// budget; a child still running then is killed and reported as not exited.
func runArgsChild(t *testing.T, db *countingDB, args []string, listenAddr string) argsChildResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestArgsHelperProcess$") //nolint:gosec // re-exec of the own test binary
	cmd.Env = argsChildEnv(db, args, listenAddr)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	begin := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	res := argsChildResult{exited: true}
	select {
	case err := <-done:
		res.elapsed = time.Since(begin)
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("waiting for helper: %v", err)
			}
			res.code = exitErr.ExitCode()
		}
	case <-time.After(argsChildBudget(t)):
		res.elapsed = time.Since(begin)
		res.exited = false
		_ = cmd.Process.Kill()
		<-done
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

// bootLogMarkers are the message prefixes of the boot's first log lines —
// the config report and the pool open (toolboot.Open, store.NewPool) — of
// which a boot writes at least one before anything else. Not every JSON line:
// the worker mode logs its own (deprioritization or OOM-score warnings in a
// restricted sandbox) without booting.
var bootLogMarkers = []string{`"msg":"config: `, `"msg":"database `, `"msg":"failed to create database pool"`}

// TestCommandLineNeverBoots is the #246 acceptance probe on the real binary:
// every argument is answered — version, help, a known mode, or a usage error —
// and no answer reaches for the database. The version answer is the value
// GET /health reports, after the program name, on stdout.
func TestCommandLineNeverBoots(t *testing.T) {
	wantVersion := "ctxd " + provenance.BuildVersion() + "\n"
	cases := []struct {
		name string
		args []string
		code int
		// check inspects the answer beyond exit code and connection count.
		check func(t *testing.T, r argsChildResult)
	}{
		{"--version", []string{"--version"}, 0, versionAnswer(wantVersion)},
		{"-version", []string{"-version"}, 0, versionAnswer(wantVersion)},
		{"version", []string{"version"}, 0, versionAnswer(wantVersion)},
		{"--help", []string{"--help"}, 0, helpAnswer},
		{"-h", []string{"-h"}, 0, helpAnswer},
		// 2 is the usage-error code (exitUsage), pinned as a literal: it is
		// the contract a calling script sees, not whatever the constant says.
		{"unknown flag", []string{"--bogus"}, 2, usageAnswer(`"--bogus"`)},
		{"empty argument", []string{""}, 2, usageAnswer(`""`)},
		{"argument after a mode", []string{"--version", "extra"}, 2, usageAnswer(`"extra"`)},
		// The modes the repository's own callers start: the compose
		// healthcheck (`/ctx -health`), break-glass.sh (`-secret-decrypt`) and
		// the scheduler's re-exec (`overview-rebuild-worker`). Each must still
		// land in its mode — here each mode's own failure on a bare
		// environment, which names the mode — and never in a boot.
		{"-health", []string{"-health"}, 1, stderrNames("health check failed")},
		{"-secret-decrypt", []string{"-secret-decrypt"}, 1, stderrNames("secret-decrypt:")},
		{"overview worker", []string{overview.WorkerCommand}, 1, stderrNames("overview-worker: options:")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newCountingDB(t)
			r := runArgsChild(t, db, tc.args, sinkAddr(t))
			if n := db.conns.Load(); n != 0 {
				t.Errorf("ctxd %q opened %d database connection(s) — it booted instead of answering the argument", tc.args, n)
			}
			for _, marker := range bootLogMarkers {
				if strings.Contains(r.stderr, marker) {
					t.Errorf("ctxd %q logged %s — a boot step ran", tc.args, marker)
				}
			}
			if !r.exited {
				t.Fatalf("ctxd %q still running after %s (killed) — it booted instead of answering the argument\nstdout:\n%s\nstderr:\n%.4000s",
					tc.args, r.elapsed.Round(time.Millisecond), r.stdout, r.stderr)
			}
			if r.code != tc.code {
				t.Errorf("ctxd %q exit code = %d, want %d\nstdout:\n%s\nstderr:\n%.4000s", tc.args, r.code, tc.code, r.stdout, r.stderr)
			}
			tc.check(t, r)
		})
	}
}

func versionAnswer(want string) func(*testing.T, argsChildResult) {
	return func(t *testing.T, r argsChildResult) {
		t.Helper()
		if r.stdout != want {
			t.Errorf("stdout = %q, want %q", r.stdout, want)
		}
	}
}

func helpAnswer(t *testing.T, r argsChildResult) {
	t.Helper()
	for _, want := range []string{"usage:", "--version", "--help", "-health", "-secret-decrypt"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help on stdout lacks %q:\n%s", want, r.stdout)
		}
	}
}

func usageAnswer(quotedArg string) func(*testing.T, argsChildResult) {
	return func(t *testing.T, r argsChildResult) {
		t.Helper()
		if r.stdout != "" {
			t.Errorf("a usage error printed to stdout: %q", r.stdout)
		}
		for _, want := range []string{quotedArg, "usage:"} {
			if !strings.Contains(r.stderr, want) {
				t.Errorf("stderr lacks %s:\n%s", want, r.stderr)
			}
		}
	}
}

func stderrNames(want string) func(*testing.T, argsChildResult) {
	return func(t *testing.T, r argsChildResult) {
		t.Helper()
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q — the mode did not run:\n%.4000s", want, r.stderr)
		}
	}
}

// TestCommandLineEmptyBoots is the harness control: the empty command line is
// the boot, and the boot must reach the counting listener. Without this, the
// zero counts above could come from a child that never knew where its
// database was.
func TestCommandLineEmptyBoots(t *testing.T) {
	db := newCountingDB(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestArgsHelperProcess$") //nolint:gosec // re-exec of the own test binary
	cmd.Env = argsChildEnv(db, []string{}, sinkAddr(t))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	deadline := time.Now().Add(argsChildBudget(t))
	for db.conns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Kill and reap BEFORE reading out: the copy goroutine writes it until Wait.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if db.conns.Load() == 0 {
		t.Fatalf("the empty command line never reached the database listener — the harness proves nothing\nchild output:\n%.4000s", out.String())
	}
}
