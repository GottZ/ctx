//go:build integration

// Issue #408, CLI against the production handlers: `ctx update`, `ctx get
// --field/--offset/--limit` and `ctx search --lines` run through the full
// cobra tree (cli.RegisterCommands) against HandleManage, HandleStore and
// HandleSearch on a real PG18. It proves what the CLI unit tests cannot: that
// the flags reach the fields the server reads (`edits`, `metadata_merge` of
// issue #406; `fields`, `content_offset`, `content_limit` of issue #407), and
// that a content file round-trips through `get --field content`.
//
//	go test -tags=integration ./internal/handler/ -run TestCLIUpdateGetSearch -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/cli"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
)

// i408RunCLI runs one argv on a fresh command tree, stdin from the given text
// (nil: untouched), and returns stdout, stderr and the command error.
func i408RunCLI(t *testing.T, stdin *string, args ...string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	outFile, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outFile.Close() }()
	errFile, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = errFile.Close() }()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	if stdin != nil {
		if err := os.WriteFile(filepath.Join(dir, "stdin"), []byte(*stdin), 0o600); err != nil {
			t.Fatal(err)
		}
		in, err := os.Open(filepath.Join(dir, "stdin"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = in.Close() }()
		os.Stdin = in
	}
	os.Stdout, os.Stderr = outFile, errFile
	root := &cobra.Command{Use: "ctx", SilenceUsage: true, SilenceErrors: true}
	cli.RegisterCommands(root)
	root.SetArgs(args)
	runErr := root.Execute()
	os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	o, _ := os.ReadFile(filepath.Join(dir, "stdout"))
	e, _ := os.ReadFile(filepath.Join(dir, "stderr"))
	return string(o), string(e), runErr
}

func TestCLIUpdateGetSearch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	_, plain, err := store.CreateApiKey(ctx, pool, "i408-cli", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	cfg := staticConfigStore{cfg: &config.Config{
		Pool: config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
	}}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
		})
	})
	r.Post("/api/manage", NewManageHandler(pool, cfg, nil, nil, nil, nil, nil, reg).HandleManage)
	r.Post("/api/store", NewStoreHandler(pool, cfg, reg).HandleStore)
	r.Post("/api/search", NewSearchHandler(pool, cfg, reg).HandleSearch)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CTX_BASE_URL", srv.URL)
	t.Setenv("CTX_KEY", plain)
	work := t.TempDir()
	t.Chdir(work)

	run := func(t *testing.T, stdin *string, args ...string) string {
		t.Helper()
		out, errOut, err := i408RunCLI(t, stdin, args...)
		if err != nil {
			t.Fatalf("`ctx %s`: %v\nstdout %q\nstderr %q", strings.Join(args, " "), err, out, errOut)
		}
		return out
	}
	content := func(t *testing.T, id string) string {
		t.Helper()
		return run(t, nil, "get", id, "--field", "content")
	}

	// The block, written through `ctx save` with the content on stdin.
	saved := run(t, strPtrI408("alpha\nbeta\ngamma\n"), "save", "i408", "Release notes")
	var stored struct {
		Block struct {
			ID string `json:"id"`
		} `json:"block"`
	}
	if err := json.Unmarshal([]byte(saved), &stored); err != nil || stored.Block.ID == "" {
		t.Fatalf("save answer %q: %v", saved, err)
	}
	id := stored.Block.ID

	t.Run("ContentFileRoundTrip", func(t *testing.T) {
		file := filepath.Join(work, "notes.md")
		if err := os.WriteFile(file, []byte("# Notes\n\nalpha beta\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		line := run(t, nil, "update", id, "--content-file", file)
		parts := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(parts) != 3 || parts[0] != id || parts[1] != "Release notes" || parts[2] == "" {
			t.Fatalf("answer line = %q, want id, title, updated_at", line)
		}
		if got := content(t, id); got != "# Notes\n\nalpha beta\n" {
			t.Fatalf("content = %q", got)
		}
		// What `get --field content` prints is what `--content-file` takes:
		// writing it back changes nothing.
		if err := os.WriteFile(file, []byte(content(t, id)), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, nil, "update", id, "--content-file", file)
		if got := content(t, id); got != "# Notes\n\nalpha beta\n" {
			t.Fatalf("content after the round trip = %q", got)
		}
	})

	t.Run("MissingContentFileWritesNothing", func(t *testing.T) {
		out, _, err := i408RunCLI(t, nil, "update", id, "--content-file", filepath.Join(work, "absent.md"))
		if err == nil || out != "" {
			t.Fatalf("err = %v, stdout = %q, want a refusal and no output", err, out)
		}
		if got := content(t, id); got != "# Notes\n\nalpha beta\n" {
			t.Fatalf("content after the refusal = %q, want it unchanged", got)
		}
	})

	t.Run("Edits", func(t *testing.T) {
		run(t, nil, "update", id, "--edit-old", "alpha", "--edit-new", "omega", "--edit-old", "Notes", "--edit-new", "Log")
		if got := content(t, id); got != "# Log\n\nomega beta\n" {
			t.Fatalf("content after the edits = %q", got)
		}
		out, _, err := i408RunCLI(t, nil, "update", id, "--edit-old", "missing", "--edit-new", "x")
		if err == nil || !strings.Contains(err.Error(), "edits[0]: old matches 0 times") || out != "" {
			t.Fatalf("err = %v, stdout = %q, want the edit_mismatch reason and no output", err, out)
		}
	})

	t.Run("MetaMergeAndTags", func(t *testing.T) {
		run(t, nil, "update", id, "--meta", "source=one", "--meta", "keep=yes")
		run(t, nil, "update", id, "--meta", "source=two", "--meta-unset", "keep", "--tags", "x,y")
		var meta map[string]any
		if err := json.Unmarshal([]byte(run(t, nil, "get", id, "--field", "metadata")), &meta); err != nil {
			t.Fatalf("metadata: %v", err)
		}
		if meta["source"] != "two" || meta["keep"] != nil {
			t.Fatalf("metadata = %v, want source=two and no keep", meta)
		}
		if got := run(t, nil, "get", id, "--field", "tags"); got != "[\n  \"x\",\n  \"y\"\n]\n" {
			t.Fatalf("tags = %q", got)
		}
		// An empty --tags (a failed "$(…)") is refused and clears nothing.
		if _, _, err := i408RunCLI(t, nil, "update", id, "--tags", ""); err == nil {
			t.Fatal("an empty --tags was sent, want a refusal")
		}
		if got := run(t, nil, "get", id, "--field", "tags"); got != "[\n  \"x\",\n  \"y\"\n]\n" {
			t.Fatalf("tags after the refused empty --tags = %q", got)
		}
	})

	t.Run("TitleCategoryAndJSON", func(t *testing.T) {
		out := run(t, nil, "update", id, "--title", "Renamed", "--category", "i408b", "--json")
		if !strings.Contains(out, `"success": true`) || !strings.Contains(out, `"title": "Renamed"`) {
			t.Fatalf("--json answer = %q", out)
		}
	})

	t.Run("GetWindowAndFields", func(t *testing.T) {
		out, errOut, err := i408RunCLI(t, nil, "get", id, "--field", "content", "--offset", "2", "--limit", "3")
		if err != nil {
			t.Fatalf("get window: %v", err)
		}
		if out != "Log\n" || !strings.Contains(errOut, "--offset 5") {
			t.Fatalf("stdout = %q, stderr = %q, want the window and the next offset", out, errOut)
		}
		var block map[string]json.RawMessage
		if err := json.Unmarshal([]byte(run(t, nil, "get", id, "--field", "title", "--field", "updated_at")), &block); err != nil {
			t.Fatalf("fields: %v", err)
		}
		if _, ok := block["content"]; ok || string(block["title"]) != `"Renamed"` || string(block["id"]) != `"`+id+`"` {
			t.Fatalf("fields answer = %v, want id and title without content", block)
		}
	})

	t.Run("SearchLines", func(t *testing.T) {
		if got, want := run(t, nil, "search", "i408b", "--lines"), id+"\ti408b\tRenamed\n"; got != want {
			t.Fatalf("search --lines = %q, want %q", got, want)
		}
	})
}

func strPtrI408(s string) *string { return &s }
