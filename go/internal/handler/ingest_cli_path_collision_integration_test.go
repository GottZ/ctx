//go:build integration

// `ctx ingest` titles every chunk after its file. Before the fix the CLI took
// the basename without .md, so two files with the same name in different
// folders produced the same title; block-mode ingest upserts by
// (category, title, scope), and the second file silently overwrote the first
// (content AND metadata.vault_path). Drives the FULL cobra tree against the
// production HandleIngest on a real PG18, so the fixture goes through the
// production write path (CLI chunker -> POST /api/ingest -> store.UpsertBlock).
//
// RED against the parent commit: each case finds one block where two belong,
// the surviving one carrying the second file's vault_path.
//
//	go test -tags=integration ./internal/handler/ -run TestIngestCLISameNameFiles -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// ingestCLIServer mounts the production ingest handler behind an AuthResult
// injector carrying a real minted key (home scope 'private').
func ingestCLIServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	_, ar := ingestKey(t, pool, "ingest-cli-collision")
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
		})
	})
	r.Post("/api/ingest", NewIngestHandler(pool, nil, nil).HandleIngest)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// sameNameDoc is a README-shaped file: identical heading, a body long enough
// to pass MinChunkChars, and a marker that identifies the file.
func sameNameDoc(marker string) string {
	return "# Setup\n\n" + marker + " " + strings.Repeat("Install the binary, point it at the server and run the first query. ", 6) + "\n"
}

func writeVaultFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

type ingestedBlock struct{ title, vaultPath string }

// blockByMarker returns every live block in category whose content carries marker.
func blockByMarker(t *testing.T, pool *pgxpool.Pool, category, marker string) []ingestedBlock {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT title, COALESCE(metadata->>'vault_path', '') FROM context_blocks
		  WHERE category = $1 AND scope = 'private' AND NOT is_archived AND content LIKE '%' || $2 || '%'`,
		category, marker)
	if err != nil {
		t.Fatalf("query blocks: %v", err)
	}
	defer rows.Close()
	var out []ingestedBlock
	for rows.Next() {
		var b ingestedBlock
		if err := rows.Scan(&b.title, &b.vaultPath); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestIngestCLISameNameFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	srv := ingestCLIServer(t, pool)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")

	cases := []struct {
		name     string
		files    [2]string
		args     []string
		category string
	}{
		// A repo ingest with one category for the whole tree.
		{"root_and_docs_with_category_override", [2]string{"README.md", "docs/README.md"}, []string{"--category", "reference"}, "reference"},
		// No override: both files fall into the folder category 'docs'.
		{"same_top_folder", [2]string{"docs/README.md", "docs/guide/README.md"}, nil, "docs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			markers := [2]string{"MARKER-" + tc.name + "-first", "MARKER-" + tc.name + "-second"}
			for i, rel := range tc.files {
				writeVaultFile(t, vault, rel, sameNameDoc(markers[i]))
			}

			args := append([]string{"ingest", vault}, tc.args...)
			if _, err := w5RunCLI(t, srv.URL, "k", args...); err != nil {
				t.Fatalf("ctx ingest: %v", err)
			}

			titles := map[string]string{}
			for i, rel := range tc.files {
				got := blockByMarker(t, pool, tc.category, markers[i])
				if len(got) != 1 {
					t.Errorf("%s: %d live blocks carry its content, want 1 (blocks %+v)", rel, len(got), got)
					continue
				}
				if got[0].vaultPath != rel {
					t.Errorf("%s: block vault_path = %q, want %q", rel, got[0].vaultPath, rel)
				}
				if other, dup := titles[got[0].title]; dup {
					t.Errorf("%s and %s share title %q", other, rel, got[0].title)
				}
				titles[got[0].title] = rel
			}
		})
	}
}
