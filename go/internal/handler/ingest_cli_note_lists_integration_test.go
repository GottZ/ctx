//go:build integration

// `ctx ingest` wrote a note's link list and date list into the metadata of
// every chunk of the note (#329). The lists now go with the note's first
// chunk; the note's other chunks carry both keys as null. Block-mode ingest
// merges the metadata of a chunk whose content is unchanged into the stored
// block (store.UpsertBlock, identicalMetadataSQL), so a key the CLI left out
// would keep the list a block stored before: the null is what replaces it.
// Drives the FULL cobra tree against the production HandleIngest on a real
// PG18 (CLI chunker -> POST /api/ingest -> store.UpsertBlock).
//
// RED against the parent commit: every chunk carries both lists; a re-ingest
// of blocks written in the old form skips every chunk and leaves the lists;
// the chunk that stops being the first keeps its list.
//
//	go test -tags=integration ./internal/handler/ -run TestIngestCLINoteLists -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// noteListsSections is n sections "## Part i" of about 2.7 KB, one chunk
// each, with one wikilink per section.
func noteListsSections(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "## Part %d\n\n[[Target %d]] %s\n\n", i, i, strings.Repeat("Check the counts against the source and log the run. ", 50))
	}
	return b.String()
}

const noteListsFrontmatter = "---\ndate: 2026-03-04\n---\n"

// noteListsBlock is what one live block of a note stores.
type noteListsBlock struct {
	links, dates string // jsonb_typeof of metadata.links / .dates, "absent" without the key
	embedded     bool
	stale        bool
	model        string
	tsEN         string
}

// noteListsBlocks reads the live blocks in 'private' from the file at vault
// path rel, by title.
func noteListsBlocks(t *testing.T, pool *pgxpool.Pool, rel string) map[string]noteListsBlock {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT title,
		        COALESCE(jsonb_typeof(metadata->'links'), 'absent'),
		        COALESCE(jsonb_typeof(metadata->'dates'), 'absent'),
		        embedding IS NOT NULL, embedding_stale, COALESCE(embed_model, ''), ts_en::text
		   FROM context_blocks
		  WHERE scope = 'private' AND NOT is_archived AND metadata->>'vault_path' = $1`, rel)
	if err != nil {
		t.Fatalf("query blocks of %s: %v", rel, err)
	}
	defer rows.Close()
	out := map[string]noteListsBlock{}
	for rows.Next() {
		var title string
		var b noteListsBlock
		if err := rows.Scan(&title, &b.links, &b.dates, &b.embedded, &b.stale, &b.model, &b.tsEN); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[title] = b
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// wantListsOn checks that the block titled first carries both lists as arrays
// and every other block of the note carries both as null.
func wantListsOn(t *testing.T, blocks map[string]noteListsBlock, first string, total int) {
	t.Helper()
	if len(blocks) != total {
		t.Errorf("%d live blocks, want %d: %v", len(blocks), total, blocks)
	}
	for title, b := range blocks {
		want := "null"
		if title == first {
			want = "array"
		}
		if b.links != want || b.dates != want {
			t.Errorf("%q: links %s, dates %s; want %s for both", title, b.links, b.dates, want)
		}
	}
}

// captureBodies serves POST /api/ingest like a server that writes every chunk
// and keeps every request body; it writes nothing.
type captureBodies struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *captureBodies) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Chunks []json.RawMessage `json:"chunks"`
	}
	_ = json.Unmarshal(raw, &req)
	c.mu.Lock()
	c.bodies = append(c.bodies, raw)
	c.mu.Unlock()
	n := len(req.Chunks)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "scope": "private", "total": n, "inserted": n})
}

// writeOldForm writes the vault the way every `ctx ingest` before #329 did,
// through the production handler at url: the CLI's requests, captured, with
// the first chunk's links and dates copied onto every chunk of its file.
func writeOldForm(t *testing.T, url, vault string) {
	t.Helper()
	capture := &captureBodies{}
	srv := httptest.NewServer(capture)
	defer srv.Close()
	if out, err := runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", "lists"); err != nil {
		t.Fatalf("capture run: %v\n%s", err, out)
	}
	for _, raw := range capture.bodies {
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("decode captured body: %v", err)
		}
		chunks, _ := req["chunks"].([]any)
		lists := map[string]map[string]any{} // vault_path -> the first chunk's metadata
		for _, c := range chunks {
			meta := c.(map[string]any)["metadata"].(map[string]any)
			if c.(map[string]any)["chunk_index"].(float64) == 0 {
				lists[meta["vault_path"].(string)] = meta
			}
		}
		for _, c := range chunks {
			meta := c.(map[string]any)["metadata"].(map[string]any)
			first := lists[meta["vault_path"].(string)]
			if first == nil {
				t.Fatalf("captured body: the first chunk of %v is in another request", meta["vault_path"])
			}
			for _, key := range []string{"links", "dates"} {
				if v, ok := first[key]; ok {
					meta[key] = v
				}
			}
		}
		body, _ := json.Marshal(req)
		resp, err := http.Post(url+"/api/ingest", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post old form: %v", err)
		}
		answer, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(answer), `"failed":0`) {
			t.Fatalf("post old form: %d %s", resp.StatusCode, answer)
		}
	}
}

// embedAll gives every live block of the file at vault path rel a vector, as
// the embedding backfill does.
func embedAll(t *testing.T, pool *pgxpool.Pool, rel string) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT id FROM context_blocks WHERE NOT is_archived AND metadata->>'vault_path' = $1`, rel)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = float32(i%3) - 1
	}
	for _, id := range ids {
		if err := store.StoreEmbedding(ctx, pool, id, "test-embed", vec); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIngestCLINoteLists_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Setenv("APPDATA", "")

	// A new note: the lists are on its first chunk, null on the others; a
	// second run skips every chunk.
	t.Run("fresh", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		pool := testdb.SetupTestDB(t)
		srv := ingestCLIServer(t, pool)
		vault := t.TempDir()
		writeVaultFile(t, vault, "multi.md", noteListsFrontmatter+noteListsSections(4))

		if out, err := runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", "lists"); err != nil {
			t.Fatalf("ingest: %v\n%s", err, out)
		}
		wantListsOn(t, noteListsBlocks(t, pool, "multi.md"), "multi — Part 0", 4)

		out, err := runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", "lists")
		if err != nil {
			t.Fatalf("second ingest: %v\n%s", err, out)
		}
		wantReported(t, out, "Updated:  0", "Skipped:  4 ")
	})

	// Blocks written in the old form (both lists on every chunk) and embedded:
	// the first re-ingest updates the metadata of chunks 2..4 once, keeps their
	// vectors and full-text vectors, and the next one skips every chunk.
	t.Run("upgrade", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		pool := testdb.SetupTestDB(t)
		srv := ingestCLIServer(t, pool)
		vault := t.TempDir()
		writeVaultFile(t, vault, "multi.md", noteListsFrontmatter+noteListsSections(4))

		writeOldForm(t, srv.URL, vault)
		before := noteListsBlocks(t, pool, "multi.md")
		for title, b := range before {
			if b.links != "array" || b.dates != "array" {
				t.Fatalf("old form: %q has links %s, dates %s; want both lists on every chunk", title, b.links, b.dates)
			}
		}
		embedAll(t, pool, "multi.md")
		before = noteListsBlocks(t, pool, "multi.md")

		out, err := runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", "lists")
		if err != nil {
			t.Fatalf("re-ingest: %v\n%s", err, out)
		}
		wantReported(t, out, "Updated:  3", "Skipped:  1 ")
		after := noteListsBlocks(t, pool, "multi.md")
		wantListsOn(t, after, "multi — Part 0", 4)
		for title, b := range after {
			if !b.embedded || b.stale || b.model != "test-embed" {
				t.Errorf("%q: embedded %v, stale %v, model %q; want the vector kept", title, b.embedded, b.stale, b.model)
			}
			if b.tsEN != before[title].tsEN {
				t.Errorf("%q: the full-text vector changed", title)
			}
		}

		out, err = runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", "lists")
		if err != nil {
			t.Fatalf("third ingest: %v\n%s", err, out)
		}
		wantReported(t, out, "Updated:  0", "Skipped:  4 ")
	})

	// A preamble put in front of the note makes a new first chunk; the chunk
	// that was the first keeps its content and no longer carries the lists.
	t.Run("carrier_moves", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		pool := testdb.SetupTestDB(t)
		srv := ingestCLIServer(t, pool)
		vault := t.TempDir()
		writeVaultFile(t, vault, "moves.md", noteListsFrontmatter+noteListsSections(3))
		if out, err := runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", "lists"); err != nil {
			t.Fatalf("ingest v1: %v\n%s", err, out)
		}
		wantListsOn(t, noteListsBlocks(t, pool, "moves.md"), "moves — Part 0", 3)

		preamble := "Opening words of the note. " + strings.Repeat("Read this before the parts. ", 12) + "\n\n"
		writeVaultFile(t, vault, "moves.md", noteListsFrontmatter+preamble+noteListsSections(3))
		out, err := runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", "lists")
		if err != nil {
			t.Fatalf("ingest v2: %v\n%s", err, out)
		}
		wantListsOn(t, noteListsBlocks(t, pool, "moves.md"), "moves", 4)
	})
}

// wantReported checks that the run's summary contains every line part in want.
func wantReported(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("summary lacks %q:\n%s", w, out)
		}
	}
}
