//go:build integration

// CLI-against-API gate for `ctx project` (design/03 §4.7; W5 list/show/api +
// I-I init=provision, design/02 §4.6). Drives the FULL cobra tree
// (cli.RegisterCommands) against the PRODUCTION MountProject router AND the
// /api/manage HandleManage dispatcher on a real PG18 (testcontainers). It proves
// the wire contract the unit tests cannot: the CLI's envelope handling, the list
// round-trip, `ctx api` passthrough, the success:false ⇒ exit-1 rule — and, since
// I-I, that `ctx project init` runs the server-admin project-provision compound,
// stores the repo-agent key 0600 UNDER the config dir (never the CWD), and is
// idempotent (a re-run is a No-op).
//
//	go test -tags=integration ./internal/handler/ -run TestProjectW5CLI -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/cli"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
)

// w5Server mounts the production MountProject chain AND the /api/manage
// dispatcher behind an AuthResult injector (same house pattern as w4Do). init
// (I-I) hits /api/manage project-provision; list/show/api hit MountProject.
func w5Server(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult) *httptest.Server {
	t.Helper()
	mh := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
		})
	})
	MountProject(r, NewProjectHandler(pool))
	r.Post("/api/manage", mh.HandleManage)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// w5RunCLI builds a fresh cobra tree (as main.go does), points the CLI config at
// the test server via env, runs the args, and returns captured stdout + the exit
// error (cobra returns the RunE error; main.go turns it into exit 1).
func w5RunCLI(t *testing.T, baseURL, key string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("CTX_BASE_URL", baseURL)
	t.Setenv("CTX_KEY", key)

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	root := &cobra.Command{Use: "ctx", SilenceUsage: true, SilenceErrors: true}
	cli.RegisterCommands(root)
	root.SetArgs(args)
	runErr := root.Execute()

	_ = w.Close()
	os.Stdout = orig
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, e := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if e != nil {
			break
		}
	}
	return string(buf), runErr
}

func TestProjectW5CLI_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	// init provisions its OWN tenant (slug derived from the identity). For
	// 'manual:docs-thing' that is slug 'docs-thing' ⇒ scope 'docs-thing:main'. The
	// server-admin fixture (E4) carries that scope in ReadScopes so the later
	// list/show sees the provisioned project.
	const provScope = "docs-thing:main"
	ar := &auth.AuthResult{
		IsValid: true, IsAdmin: true, HomeScope: "_global",
		ReadScopes: []string{provScope},
	}
	srv := w5Server(t, pool, ar)

	// Isolate the CLI config dir (agent keys land under it, never the real
	// ~/.config) AND run from a scratch git repo (the .ctx-project marker lands at
	// its toplevel, never in the ctx repo).
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("APPDATA", "")
	scratch := t.TempDir()
	gitInit := exec.Command("git", "init", "-q")
	gitInit.Dir = scratch
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init scratch: %v\n%s", err, out)
	}
	t.Chdir(scratch)

	// ── list is empty at first (pipe ⇒ JSON envelope) ───────────────────────
	t.Run("ListEmpty", func(t *testing.T) {
		out, err := w5RunCLI(t, srv.URL, "k", "project", "list")
		if err != nil {
			t.Fatalf("project list: %v\n%s", err, out)
		}
		if !strings.Contains(out, `"success": true`) || !strings.Contains(out, `"projects"`) {
			t.Errorf("empty list JSON = %q, want success:true + projects", out)
		}
	})

	// ── init PROVISIONS: creates tenant+scope+keys, writes .ctx-project + the
	//    0600 agent key under the config dir (I-I) ────────────────────────────
	t.Run("InitProvisions", func(t *testing.T) {
		out, err := w5RunCLI(t, srv.URL, "k", "project", "init", "--identity", "manual:docs-thing")
		if err != nil {
			t.Fatalf("project init: %v\n%s", err, out)
		}
		if !strings.Contains(out, provScope) {
			t.Errorf("init output = %q, want the derived scope %q", out, provScope)
		}
		if _, statErr := os.Stat(".ctx-project"); statErr != nil {
			t.Errorf(".ctx-project not written: %v", statErr)
		}
		// The agent key file is 0600 under <cfg>/ctx/projects/, NEVER in the CWD.
		keyDir := filepath.Join(cfgDir, "ctx", "projects")
		entries, derr := os.ReadDir(keyDir)
		if derr != nil || len(entries) != 1 {
			t.Fatalf("agent key dir %q: err=%v entries=%d, want exactly 1 key file", keyDir, derr, len(entries))
		}
		fi, _ := os.Stat(filepath.Join(keyDir, entries[0].Name()))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("agent key file mode = %o, want 0600", fi.Mode().Perm())
		}
		// Negative: no key material leaked into the scratch (repo) dir.
		scratchEntries, _ := os.ReadDir(scratch)
		for _, e := range scratchEntries {
			if e.IsDir() {
				continue
			}
			data, _ := os.ReadFile(filepath.Join(scratch, e.Name()))
			if strings.Contains(string(data), "CTX_KEY=") {
				t.Errorf("a key file leaked into the repo dir: %q", e.Name())
			}
		}
	})

	// ── re-init with the SAME identity is idempotent (No-op, no duplicate) ────
	t.Run("InitIdempotent", func(t *testing.T) {
		out, err := w5RunCLI(t, srv.URL, "k", "project", "init", "--identity", "manual:docs-thing")
		if err != nil {
			t.Fatalf("re-init: %v\n%s", err, out)
		}
		if !strings.Contains(out, `"provisioned": false`) && !strings.Contains(out, "docs-thing") {
			t.Errorf("re-init output = %q, want the existing project (provisioned:false)", out)
		}
		// list now has exactly one project.
		listOut, _ := w5RunCLI(t, srv.URL, "k", "project", "list")
		// Count the identity FIELD precisely (the identity string also appears in
		// display_name, which defaults to the identity — so a bare substring count
		// would read 2 for a single row).
		if n := strings.Count(listOut, `"identity": "manual:docs-thing"`); n != 1 {
			t.Errorf("after re-init, list shows %d project rows, want 1:\n%s", n, listOut)
		}
	})

	// ── ctx api passthrough hits MountProject and prints the envelope ─────────
	t.Run("APIPassthrough", func(t *testing.T) {
		out, err := w5RunCLI(t, srv.URL, "k", "api", "GET", "/api/project")
		if err != nil {
			t.Fatalf("ctx api GET: %v\n%s", err, out)
		}
		if !strings.Contains(out, "manual:docs-thing") {
			t.Errorf("ctx api GET output = %q, want the registered project", out)
		}
	})

	// ── success:false ⇒ exit-1 (a bad create body: unknown identity prefix) ──
	t.Run("APIExitCodeOnFailure", func(t *testing.T) {
		_, err := w5RunCLI(t, srv.URL, "k", "api", "POST", "/api/project", `{"identity":"nope","scope":"x","tenant_id":"00000000-0000-0000-0000-0000000d3fa0"}`)
		if err == nil {
			t.Fatal("bad create ⇒ want a non-nil error (exit 1), got nil")
		}
	})

	// ── a member create via ctx api ⇒ 403 ⇒ exit-1 ──────────────────────────
	t.Run("APIForbiddenIsExit1", func(t *testing.T) {
		tn := be5SeedTenant(t, pool, "w5cli")
		member := &auth.AuthResult{IsValid: true, TenantID: tn, TenantRole: auth.RoleMember, ReadScopes: []string{"w5cli:docs"}}
		msrv := w5Server(t, pool, member)
		_, err := w5RunCLI(t, msrv.URL, "k", "api", "POST", "/api/project", `{"identity":"manual:x","scope":"y"}`)
		if err == nil {
			t.Fatal("member create via ctx api ⇒ want error (exit 1), got nil")
		}
	})
}
