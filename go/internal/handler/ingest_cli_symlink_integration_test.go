//go:build integration

// `ctx ingest` walked the vault without following a symbolic link, so a folder
// shared into the vault by a link was left out without a word (#324). Drives
// the full cobra tree against the production HandleIngest on a real PG18: the
// notes behind a link are written under the path through the link, a second
// link to the same folder and a link back to the vault add nothing, and a
// second run writes into the same blocks.
//
// RED against the parent commit: the notes of the linked folder are missing.
//
//	go test -tags=integration ./internal/handler/ -run TestIngestCLIFollowsLinkedFolder -count=1 -v
package handler

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// vaultBlock is a live block written by ctx ingest.
type vaultBlock struct{ id, category, title, vaultPath string }

// obsidianBlocks lists the live blocks ctx ingest wrote to 'private', by vault_path.
func obsidianBlocks(t *testing.T, pool *pgxpool.Pool) []vaultBlock {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, category, title, COALESCE(metadata->>'vault_path', '') FROM context_blocks
		  WHERE scope = 'private' AND NOT is_archived AND metadata->>'source' = 'obsidian'
		  ORDER BY metadata->>'vault_path', title`)
	if err != nil {
		t.Fatalf("query blocks: %v", err)
	}
	defer rows.Close()
	var out []vaultBlock
	for rows.Next() {
		var b vaultBlock
		if err := rows.Scan(&b.id, &b.category, &b.title, &b.vaultPath); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestIngestCLIFollowsLinkedFolder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	srv := ingestCLIServer(t, pool)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(base, "vault")
	writeVaultFile(t, vault, "good.md", sameNameDoc("MARKER-good"))
	writeVaultFile(t, base, "outside/s.md", sameNameDoc("MARKER-s"))
	writeVaultFile(t, base, "outside/sub/t.md", sameNameDoc("MARKER-t"))
	writeVaultFile(t, base, "single/f.md", sameNameDoc("MARKER-f"))
	for _, l := range []struct{ target, at string }{
		{filepath.Join(base, "outside"), filepath.Join(vault, "shared")},
		{filepath.Join(base, "outside"), filepath.Join(vault, "twin")},
		{filepath.Join(base, "single/f.md"), filepath.Join(vault, "alias.md")},
		{vault, filepath.Join(vault, "shared-back")},
	} {
		if err := os.Symlink(l.target, l.at); err != nil {
			t.Skipf("symlink: %v", err)
		}
	}

	want := []vaultBlock{
		{category: "uncategorized", title: "alias", vaultPath: "alias.md"},
		{category: "uncategorized", title: "good", vaultPath: "good.md"},
		{category: "shared", title: "shared/s", vaultPath: "shared/s.md"},
		{category: "shared", title: "shared/sub/t", vaultPath: "shared/sub/t.md"},
	}
	var first []vaultBlock
	for run := 1; run <= 2; run++ {
		if _, err := w5RunCLI(t, srv.URL, "k", "ingest", vault); err != nil {
			t.Fatalf("run %d: ctx ingest: %v", run, err)
		}
		got := obsidianBlocks(t, pool)
		bare := make([]vaultBlock, len(got))
		for i, b := range got {
			bare[i] = vaultBlock{category: b.category, title: b.title, vaultPath: b.vaultPath}
		}
		if !slices.Equal(bare, want) {
			t.Fatalf("run %d: live blocks %+v, want %+v", run, bare, want)
		}
		if run == 1 {
			first = got
		} else if !slices.Equal(got, first) {
			t.Errorf("run 2 wrote other blocks than run 1:\n run 1 %+v\n run 2 %+v", first, got)
		}
	}
}
