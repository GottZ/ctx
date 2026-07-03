//go:build integration

// W5 CLI-against-API gate (design/03-workflow-api-cli.md §4.7/§7-W5): the
// `ctx project` + `ctx api` commands driven END-TO-END through the FULL cobra
// tree (cli.RegisterCommands) against the PRODUCTION MountProject router on a
// real PG18 (testcontainers). This proves the wire contract the unit tests
// cannot: the CLI's envelope handling, the list/init round-trip, idempotent
// re-init, `ctx api` passthrough, and the success:false ⇒ exit-1 rule — all
// against exactly what server.go wires.
//
//	go test -tags=integration ./internal/handler/ -run TestProjectW5CLI -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/cli"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
)

// w5Server mounts the production MountProject chain behind an AuthResult injector
// (same house pattern as w4Do) and returns a live httptest server.
func w5Server(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
		})
	})
	MountProject(r, NewProjectHandler(pool))
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

	// A tenant-admin whose read set already contains the scope init will create
	// (slug "w5cli" + name "docs" ⇒ "w5cli:docs"), so a later list/show sees it.
	tn := be5SeedTenant(t, pool, "w5cli")
	ar := &auth.AuthResult{
		IsValid: true, TenantID: tn, TenantRole: auth.RoleAdmin,
		ReadScopes: []string{"w5cli:docs"},
	}
	srv := w5Server(t, pool, ar)

	// Run the CLI from a scratch dir that is its OWN git repo, so init's
	// .ctx-project (written to the git toplevel) lands here in isolation, never
	// in the ctx repo. init uses --identity, so detection itself is not consulted.
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

	// ── init creates, writes .ctx-project, prints the row ───────────────────
	t.Run("Init", func(t *testing.T) {
		out, err := w5RunCLI(t, srv.URL, "k", "project", "init", "--identity", "manual:docs-thing", "--scope", "docs")
		if err != nil {
			t.Fatalf("project init: %v\n%s", err, out)
		}
		if !strings.Contains(out, "manual:docs-thing") || !strings.Contains(out, "w5cli:docs") {
			t.Errorf("init output = %q, want the identity + scope", out)
		}
		if _, statErr := os.Stat(".ctx-project"); statErr != nil {
			t.Errorf(".ctx-project not written: %v", statErr)
		}
	})

	// ── re-init with the SAME identity is idempotent (no duplicate, 200) ─────
	t.Run("InitIdempotent", func(t *testing.T) {
		out, err := w5RunCLI(t, srv.URL, "k", "project", "init", "--identity", "manual:docs-thing", "--scope", "docs")
		if err != nil {
			t.Fatalf("re-init: %v\n%s", err, out)
		}
		if !strings.Contains(out, "already_registered") && !strings.Contains(out, "manual:docs-thing") {
			t.Errorf("re-init output = %q, want the existing project", out)
		}
		// list now has exactly one project.
		listOut, _ := w5RunCLI(t, srv.URL, "k", "project", "list")
		if strings.Count(listOut, "manual:docs-thing") != 1 {
			t.Errorf("after re-init, list shows %d copies, want 1:\n%s",
				strings.Count(listOut, "manual:docs-thing"), listOut)
		}
	})

	// ── ctx api passthrough hits the same route and prints the envelope ──────
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
		out, err := w5RunCLI(t, srv.URL, "k", "api", "POST", "/api/project", `{"identity":"nope","scope":"x"}`)
		if err == nil {
			t.Fatalf("bad create ⇒ want a non-nil error (exit 1), got nil\n%s", out)
		}
	})

	// ── init against a scope beyond the tenant quota ⇒ exit-1 (quota gate) ───
	t.Run("APIForbiddenIsExit1", func(t *testing.T) {
		// A member (no admin role) cannot create — the CLI must surface the 403
		// as a non-nil error, never a silent exit 0.
		member := &auth.AuthResult{IsValid: true, TenantID: tn, TenantRole: auth.RoleMember, ReadScopes: []string{"w5cli:docs"}}
		msrv := w5Server(t, pool, member)
		_, err := w5RunCLI(t, msrv.URL, "k", "api", "POST", "/api/project", `{"identity":"manual:x","scope":"y"}`)
		if err == nil {
			t.Fatal("member create via ctx api ⇒ want error (exit 1), got nil")
		}
	})
}
