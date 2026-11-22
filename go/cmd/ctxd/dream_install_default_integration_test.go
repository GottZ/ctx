//go:build integration

// Issue #492: a new installation runs Dream without the operator setting
// anything; an upgraded one keeps what it had. The gate boots the production
// main() in a child process (this test binary re-executed into
// TestDreamBootHelperProcess, the #246 idiom) against a real database and
// reads the answer the way an operator would: /health lists a "dream" service
// exactly while dream.enabled is effectively true (handler.HealthStatus), the
// settings API names the value's source (install | default | env), and the
// scheduler logs "dream mode enabled" when it starts the dream workers.
//
//   - fresh_first_boot: an EMPTY database (no migration history) — the README
//     path and the operator with a minimal .env meet the same first boot.
//   - fresh_second_boot: the same database booted again — the default is a
//     property of the database, not of the first boot.
//   - upgrade_without_key: a database at migration 191 (the last release
//     before this change) with a block in it, CTX_DREAM_ENABLED never set —
//     Dream stays off.
//   - fresh_env_false / upgrade_env_true: an explicit CTX_DREAM_ENABLED wins
//     in both directions.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestDreamDefault' ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const dreamBootHelperEnv = "CTX_TEST_DREAM_BOOT_HELPER"

// TestDreamBootHelperProcess is not a test: it is the child body of the
// #492 boot probes. It runs the production main() on an empty command line,
// which serves until SIGTERM.
func TestDreamBootHelperProcess(t *testing.T) {
	if os.Getenv(dreamBootHelperEnv) == "" {
		t.Skip("helper process body — only meaningful when spawned by the #492 boot probes")
	}
	os.Args = []string{"ctxd"}
	main()
	os.Exit(0)
}

// lockedBuffer is a bytes.Buffer the child's output goroutines and the test
// read concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// dreamBootChild is one running ctxd child.
type dreamBootChild struct {
	cmd  *exec.Cmd
	out  *lockedBuffer
	addr string
	done chan error
}

// freeLoopbackAddr returns a loopback address whose port was free a moment
// ago — the child's LISTEN_ADDR.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// dreamBootChildEnv is the child's environment: the parent's minus
// everything that could configure ctxd or libpq on its own (CONTEXT_*, CTX_*,
// PG*, LISTEN_ADDR), plus the database of dsn, the listen address and extra.
// The overview rebuild is switched off: its worker is the own executable with
// a hidden argument, which in a test binary would be the whole test run.
func dreamBootChildEnv(t *testing.T, dsn, addr string, extra map[string]string) []string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	pass, _ := u.User.Password()
	env := make([]string, 0, len(os.Environ())+16)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "CONTEXT_") || strings.HasPrefix(name, "CTX_") ||
			strings.HasPrefix(name, "PG") || name == "LISTEN_ADDR" {
			continue
		}
		env = append(env, kv)
	}
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	env = append(env,
		dreamBootHelperEnv+"=1",
		"CONTEXT_DB_HOST="+u.Hostname(),
		"CONTEXT_DB_PORT="+u.Port(),
		"CONTEXT_DB="+strings.TrimPrefix(u.Path, "/"),
		"CONTEXT_DB_USER="+u.User.Username(),
		"CONTEXT_DB_PASSWORD="+pass,
		"CONTEXT_DB_SSLMODE="+sslmode,
		"LISTEN_ADDR="+addr,
		"CTX_GRAPH_OVERVIEW_ENABLED=false",
		"CTX_BOOTSTRAP_ADMIN_KEY="+dreamBootAdminKey,
		"CTX_BOOTSTRAP_RUN_ID=i492",
	)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// dreamBootAdminKey is the server-admin key the first boot of each database
// mints (bootstrapAdminKeyBoot: only into an empty api-key table) — the
// operator's key of the README path, for the settings API read below.
var dreamBootAdminKey = strings.Repeat("49", 32)

// settingSource reads GET /api/settings/{key} with the admin key and returns
// the effective value and its source.
func (c *dreamBootChild) settingSource(t *testing.T, key string) (value any, source string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+c.addr+"/api/settings/"+key, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("X-Context-Key", dreamBootAdminKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET /api/settings/%s: %v", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Success bool `json:"success"`
		Setting struct {
			Value  any    `json:"value"`
			Source string `json:"source"`
		} `json:"setting"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !body.Success {
		t.Fatalf("GET /api/settings/%s: HTTP %d, decode %v, success %v", key, resp.StatusCode, err, body.Success)
	}
	return body.Setting.Value, body.Setting.Source
}

// startDreamBootChild boots ctxd against dsn and returns once /health
// answers (any status: a fresh install without backends is "unhealthy").
func startDreamBootChild(t *testing.T, dsn string, extra map[string]string) *dreamBootChild {
	t.Helper()
	addr := freeLoopbackAddr(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDreamBootHelperProcess$", "-test.timeout=0") //nolint:gosec // re-exec of the own test binary
	cmd.Env = dreamBootChildEnv(t, dsn, addr, extra)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting ctxd child: %v", err)
	}
	c := &dreamBootChild{cmd: cmd, out: out, addr: addr, done: make(chan error, 1)}
	go func() { c.done <- cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			_ = cmd.Process.Kill()
			<-c.done
		}
	})

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case err := <-c.done:
			c.done <- err
			t.Fatalf("ctxd child exited before serving (%v), output:\n%s", err, out.String())
		default:
		}
		if _, _, ok := c.tryHealth(); ok {
			return c
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("ctxd child did not serve /health within 3 minutes, output:\n%s", out.String())
	return nil
}

// tryHealth asks /health once.
func (c *dreamBootChild) tryHealth() (status string, services map[string]string, ok bool) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + c.addr + "/health")
	if err != nil {
		return "", nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Status   string            `json:"status"`
		Services map[string]string `json:"services"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Status == "" {
		return "", nil, false
	}
	return body.Status, body.Services, true
}

// health is tryHealth that fails the test when /health does not answer.
func (c *dreamBootChild) health(t *testing.T) (string, map[string]string) {
	t.Helper()
	status, services, ok := c.tryHealth()
	if !ok {
		t.Fatalf("/health did not answer, output:\n%s", c.out.String())
	}
	return status, services
}

// stop sends SIGTERM, waits for the exit and returns the child's output.
func (c *dreamBootChild) stop(t *testing.T) string {
	t.Helper()
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-c.done:
		c.done <- err
		if err != nil {
			t.Errorf("ctxd child exited with %v after SIGTERM", err)
		}
	case <-time.After(90 * time.Second):
		_ = c.cmd.Process.Kill()
		t.Fatalf("ctxd child did not stop within 90 s of SIGTERM, output:\n%s", c.out.String())
	}
	return c.out.String()
}

// firstRunNoticeLog is the start of the line a boot logs when Dream runs on
// the installation's default rather than on CTX_DREAM_ENABLED.
const firstRunNoticeLog = `"msg":"dream: on by default`

// dreamWorkersStarted is the scheduler's line when it starts the dream
// workers (events.Scheduler.Run).
const dreamWorkersStarted = `"msg":"scheduler: dream mode enabled (continuous)"`

func TestDreamDefaultOnFreshInstallOffOnUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// fresh is an empty database: no migration history, the four extensions
	// the production init script creates as superuser (init-data.sh, section
	// 2 — store.NewPool registers the vector type on connect) and nothing else.
	fresh := func(t *testing.T) string {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 0)
		if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector;
			CREATE EXTENSION IF NOT EXISTS timescaledb;
			CREATE EXTENSION IF NOT EXISTS pgcrypto;
			CREATE EXTENSION IF NOT EXISTS pg_trgm`); err != nil {
			t.Fatalf("init-data extensions: %v", err)
		}
		return dsn
	}
	// existing is an installation of the release before this change
	// (migration 191) that holds a block written through the store.
	existing := func(t *testing.T) string {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 191)
		if _, err := store.UpsertBlock(ctx, pool, "decisions", "An existing installation", "written before the upgrade",
			nil, nil, "private", true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("seed existing block: %v", err)
		}
		return dsn
	}

	type boot struct {
		env        map[string]string
		wantDream  bool
		wantNote   bool   // the first-run notice: Dream on by the installation's default
		wantSource string // GET /api/settings/dream.enabled: install | default | env
	}
	for _, c := range []struct {
		name  string
		db    func(t *testing.T) string
		boots []boot
	}{
		{"fresh_first_and_second_boot", fresh, []boot{
			{env: nil, wantDream: true, wantNote: true, wantSource: "install"},
			{env: nil, wantDream: true, wantNote: true, wantSource: "install"},
		}},
		{"upgrade_without_key", existing, []boot{
			{env: nil, wantDream: false, wantSource: "default"},
			{env: nil, wantDream: false, wantSource: "default"},
		}},
		{"fresh_env_false", fresh, []boot{
			{env: map[string]string{"CTX_DREAM_ENABLED": "false"}, wantDream: false, wantSource: "env"},
			// The default survives an explicit false: unset again, it is on.
			{env: nil, wantDream: true, wantNote: true, wantSource: "install"},
		}},
		{"upgrade_env_true", existing, []boot{
			{env: map[string]string{"CTX_DREAM_ENABLED": "true"}, wantDream: true, wantSource: "env"},
			{env: nil, wantDream: false, wantSource: "default"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dsn := c.db(t)
			for i, b := range c.boots {
				child := startDreamBootChild(t, dsn, b.env)
				status, services := child.health(t)
				_, dreamListed := services["dream"]
				value, source := child.settingSource(t, "dream.enabled")
				out := child.stop(t)
				if value != b.wantDream || source != b.wantSource {
					t.Errorf("boot %d (env %v): settings API dream.enabled = %v (source %q), want %v (source %q)",
						i+1, b.env, value, source, b.wantDream, b.wantSource)
				}
				t.Logf("boot %d (env %v): /health status %q services %v", i+1, b.env, status, services)
				if dreamListed != b.wantDream {
					t.Errorf("boot %d (env %v): /health lists dream = %v, want %v (services %v)", i+1, b.env, dreamListed, b.wantDream, services)
				}
				if got := strings.Contains(out, dreamWorkersStarted); got != b.wantDream {
					t.Errorf("boot %d (env %v): dream workers started = %v, want %v", i+1, b.env, got, b.wantDream)
				}
				if got := strings.Contains(out, firstRunNoticeLog); got != b.wantNote {
					t.Errorf("boot %d (env %v): first-run notice logged = %v, want %v", i+1, b.env, got, b.wantNote)
				}
				if t.Failed() {
					t.Logf("boot %d output:\n%s", i+1, tailLines(out, 80))
					return
				}
			}
		})
	}
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return fmt.Sprintf("[… %d lines]\n%s", len(lines), strings.Join(lines, "\n"))
}
