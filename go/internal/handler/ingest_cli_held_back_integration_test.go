//go:build integration

// POST /api/ingest refuses a whole request when one chunk breaks a per-chunk
// limit (validateIngestRequest), and `ctx ingest` sent every file's chunks in
// shared batches: one file with an over-long title failed the chunks of every
// other file in its batch, and the run reported it and exited 0 (#318). The
// CLI now holds such a file back before the upload, reports it, writes the
// rest, and exits non-zero. A chunk in a category the derived layer reserves
// is refused whole the same way, by the claim gate with 403 (#320). Drives the
// FULL cobra tree against the production HandleIngest on a real PG18 (CLI
// chunker -> POST /api/ingest -> store.UpsertBlock).
//
// RED against the commit before each fix: the other file of each case has no
// block (and before #318 the run returned nil).
//
//	go test -tags=integration ./internal/handler/ -run TestIngestCLIHoldsBackFileOverLimit -count=1 -v
package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// liveBlocksOfFile counts the live blocks in 'private' that came from the file
// at vault path rel.
func liveBlocksOfFile(t *testing.T, pool *pgxpool.Pool, rel string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks
		  WHERE scope = 'private' AND NOT is_archived AND metadata->>'vault_path' = $1`, rel).Scan(&n); err != nil {
		t.Fatalf("count blocks of %s: %v", rel, err)
	}
	return n
}

// heldBackPath is a vault-relative path without .md of exactly n bytes, in
// folders that stay under the filesystem's 255-byte name limit.
func heldBackPath(n int, tag string) string {
	var parts []string
	for n-len(tag) > 200 {
		parts = append(parts, strings.Repeat("d", 199))
		n -= 200
	}
	return strings.Join(append(parts, tag+strings.Repeat("f", n-len(tag))), "/")
}

func TestIngestCLIHoldsBackFileOverLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	srv := ingestCLIServer(t, pool)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")

	note := func(marker string) string {
		return "# Note\n\n" + marker + " " + strings.Repeat("Run the importer and compare the counts with the source. ", 6) + "\n"
	}
	tenRepeats := func() string {
		var b strings.Builder
		for i := range 10 {
			b.WriteString("## X\n\nsection-" + string(rune('a'+i)) + " " +
				strings.Repeat("Check the counts against the source and log the run. ", 15) + "\n\n")
		}
		return b.String()
	}
	sep := len(" — ")

	cases := []struct {
		name string
		rel  string // the file that breaks a limit
		body string
		args []string
	}{
		{"title_over_limit", heldBackPath(maxTitleLen+3, "title") + ".md", note("long"), []string{"--category", "limits"}},
		// One byte of room after " — ": the tenth "## X" gets the counter "10"
		// and a 501-byte title (#317 numbered).
		{"tenth_repeat_one_byte_of_room", heldBackPath(maxTitleLen-sep-1, "tenth") + ".md", tenRepeats(), []string{"--category", "limits"}},
		{"category_over_limit", strings.Repeat("c", maxCategoryLen+1) + "/note.md", note("cat"), nil},
		{"content_over_limit", "dump.md", "# Dump\n\n" + strings.Repeat("x", maxContentSize+10) + "\n", []string{"--category", "limits"}},
		{"empty_title", ".md", note("nameless"), nil},
		// A top-level folder named like a reserved category: the claim gate
		// refuses the whole request with 403 (#320).
		{"reserved_category", "Catalog/note.md", note("reserved"), nil},
		{"reserved_category_other_case", "SESSION-INSIGHTS/note.md", note("reserved-2"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			good := "good-" + strings.ReplaceAll(tc.name, "_", "-") + ".md"
			writeVaultFile(t, vault, good, note("MARKER-"+tc.name))
			writeVaultFile(t, vault, tc.rel, tc.body)

			out, err := runIngestCLIStderr(t, srv.URL, append([]string{"ingest", vault}, tc.args...)...)
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "Inserted:") || strings.Contains(line, "Failed:") ||
					strings.Contains(line, "not uploaded") || strings.Contains(line, "batch-") {
					t.Logf("cli: %s", strings.TrimSpace(line))
				}
			}

			if n := liveBlocksOfFile(t, pool, good); n != 1 {
				t.Errorf("%s: %d live blocks, want 1 — the file over a limit took its batch down", good, n)
			}
			if n := liveBlocksOfFile(t, pool, tc.rel); n != 0 {
				t.Errorf("%s: %d live blocks, want 0 — the file is held back whole", tc.name, n)
			}
			if err == nil {
				t.Errorf("ctx ingest returned nil, want an error: a file was not ingested")
			}
			if !strings.Contains(out, tc.rel+": not uploaded") {
				t.Errorf("stderr does not report %q as not uploaded:\n%s", tc.rel, out)
			}
		})
	}
}

// A chunk the store cannot write fails alone: the handler answers 200 with
// success:false, failed 1 and a failed result for it, next to the written
// chunk of the other file (docs/api.md). A NUL byte is such a chunk, since a
// PostgreSQL text value cannot hold one. The CLI counts the batch chunk by
// chunk, names the failed chunk's file and exits non-zero. Before #318 it took
// the answer for a refused batch: "server: unknown error", Inserted 0, Failed 2.
func TestIngestCLIPartialBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	srv := ingestCLIServer(t, pool)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")

	vault := t.TempDir()
	note := func(marker string) string {
		return "# Note\n\n" + marker + " " + strings.Repeat("Run the importer and compare the counts with the source. ", 6) + "\n"
	}
	writeVaultFile(t, vault, "good-partial.md", note("MARKER-partial"))
	writeVaultFile(t, vault, "nul.md", note("before\x00after"))

	out, err := runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", "partial")

	if n := liveBlocksOfFile(t, pool, "good-partial.md"); n != 1 {
		t.Errorf("good-partial.md: %d live blocks, want 1", n)
	}
	if n := liveBlocksOfFile(t, pool, "nul.md"); n != 0 {
		t.Errorf("nul.md: %d live blocks, want 0 (test setup: the NUL byte must fail the store write)", n)
	}
	if err == nil {
		t.Errorf("ctx ingest returned nil, want an error: a chunk was not written")
	}
	for _, want := range []string{"nul.md: chunk 1: ", "Inserted: 0", "Updated:  1", "Failed:   1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
}
