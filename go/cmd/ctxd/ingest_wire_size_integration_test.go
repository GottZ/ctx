//go:build integration

// #323 on the PRODUCTION mount: `ctx ingest` cut its batches by chunk count
// only, and ctxd mounts POST /api/ingest behind a 10 MB body limit
// (handler.MaxBodySize(IngestMaxBodySize) in server.go). Every chunk of a
// file carries the file's metadata.links, so a note with many links made a
// batch over the limit out of few chunks; the server refused it whole, and
// the chunks of the other files in it were lost with it, on every run.
//
// The CLI now cuts a batch by its size on the wire as well, and holds back a
// file with a chunk whose request alone is over the limit. These probes drive
// the FULL cobra tree against cmd/ctxd.NewRouter on a real PG18, so the
// request passes the Auth, LoopGuard and body-cap middleware the route has in
// production, and the handler's answer to a body over the cap is the one a
// client gets.
//
// RED against the parent commit:
//   - many_links_note: the normal notes of the first batch have no block, a
//     request was over the limit and answered 400, and the run errs.
//   - chunk_over_request_limit: the normal notes have no block, and stderr does
//     not name the held-back file.
//   - TestIngestBodyCapBoundary_Integration/one_byte_over: 400 "Invalid request
//     body", which does not say why.
//
//	CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18 go test -tags=integration ./cmd/ctxd/ -run 'TestIngestCLIWireSize|TestIngestBodyCapBoundary' -count=1 -v
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/GottZ/ctx/internal/cli"
)

// ingestCall is one POST /api/ingest as the server saw it.
type ingestCall struct {
	bytes  int64 // the declared body length
	status int
}

// ingestCalls records the ingest requests that pass through observeIngest.
type ingestCalls struct {
	mu    sync.Mutex
	calls []ingestCall
}

func (c *ingestCalls) all() []ingestCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ingestCall(nil), c.calls...)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// observeIngest serves h and records the length and the answer status of
// every POST /api/ingest; it changes nothing about the request.
func observeIngest(h http.Handler, log *ingestCalls) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ingest" {
			h.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		log.mu.Lock()
		log.calls = append(log.calls, ingestCall{bytes: r.ContentLength, status: rec.status})
		log.mu.Unlock()
	})
}

// wireSizeServer serves the production router over real HTTP and returns its
// URL, the pool, the admin key (home scope 'private') and the request log.
func wireSizeServer(t *testing.T) (string, *pgxpool.Pool, string, *ingestCalls) {
	t.Helper()
	router, pool, key := mcpBodyCapRouter(t)
	log := &ingestCalls{}
	srv := httptest.NewServer(observeIngest(router, log))
	t.Cleanup(srv.Close)
	return srv.URL, pool, key, log
}

// runIngestCLI runs `ctx <args>` through the full cobra tree against baseURL
// and returns what it printed on stderr, where `ctx ingest` reports.
func runIngestCLI(t *testing.T, baseURL, key string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("CTX_BASE_URL", baseURL)
	t.Setenv("CTX_KEY", key)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	errFile, err := os.Create(filepath.Join(t.TempDir(), "stderr.txt"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = errFile
	root := &cobra.Command{Use: "ctx", SilenceUsage: true, SilenceErrors: true}
	cli.RegisterCommands(root)
	root.SetArgs(args)
	runErr := root.Execute()
	os.Stderr = old
	_ = errFile.Close()
	out, err := os.ReadFile(errFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

func writeNote(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// blocksOfFile counts the live blocks in 'private' from the file at vault path rel.
func blocksOfFile(t *testing.T, pool *pgxpool.Pool, rel string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks
		  WHERE scope = 'private' AND NOT is_archived AND metadata->>'vault_path' = $1`, rel).Scan(&n); err != nil {
		t.Fatalf("count blocks of %s: %v", rel, err)
	}
	return n
}

// plainNote is a one-chunk note carrying marker.
func plainNote(marker string) string {
	return "# Note\n\n" + marker + " " + strings.Repeat("Run the importer and compare the counts with the source. ", 6) + "\n"
}

// hubNote is a note with n distinct wikilinks, 150 to a "## Part" section,
// so that it is cut into n/150 chunks, every one of which carries all n link
// targets in metadata.links.
func hubNote(n int) string {
	var b strings.Builder
	b.WriteString("# Hub\n")
	for i := 0; i < n; i++ {
		if i%150 == 0 {
			fmt.Fprintf(&b, "\n## Part %d\n\n", i/150)
		}
		fmt.Fprintf(&b, "[[Linked note number %05d]] ", i)
	}
	b.WriteString("\n")
	return b.String()
}

// giantLinksNote is a one-chunk note whose n wikilinks have targets of
// targetBytes bytes and a one-letter display text: the chunk's content stays
// small, its metadata.links is about n*targetBytes bytes.
func giantLinksNote(n, targetBytes int) string {
	var b strings.Builder
	b.WriteString("# Giant\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "[[%05d-%s|x]] ", i, strings.Repeat("t", targetBytes-6))
	}
	b.WriteString("\n")
	return b.String()
}

func logIngestSummary(t *testing.T, out string, calls []ingestCall) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		for _, k := range []string{"Chunks:", "Inserted:", "Failed:", "Not sent:", "not uploaded", "batch-", "Errors", "Upload errors"} {
			if strings.Contains(line, k) {
				t.Logf("cli: %s", strings.TrimSpace(line))
				break
			}
		}
	}
	for i, c := range calls {
		t.Logf("request %d: %d bytes, status %d", i, c.bytes, c.status)
	}
}

func TestIngestCLIWireSize_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	// 9 000 links: 60 chunks of about 245 KB each on the wire. The five
	// normal notes sort before the hub, so a batch of 50 by count carries
	// them and 45 hub chunks, about 11 MB.
	t.Run("many_links_note", func(t *testing.T) {
		url, pool, key, calls := wireSizeServer(t)
		vault := t.TempDir()
		normals := []string{"a-note-1.md", "a-note-2.md", "a-note-3.md", "a-note-4.md", "a-note-5.md"}
		for _, rel := range normals {
			writeNote(t, vault, rel, plainNote("MARKER-"+rel))
		}
		writeNote(t, vault, "hub.md", hubNote(9000))

		out, err := runIngestCLI(t, url, key, "ingest", vault, "--category", "wiresize")
		logIngestSummary(t, out, calls.all())

		for _, rel := range normals {
			if n := blocksOfFile(t, pool, rel); n != 1 {
				t.Errorf("%s: %d live blocks, want 1 — the note with many links took its batch down", rel, n)
			}
		}
		if n := blocksOfFile(t, pool, "hub.md"); n != 60 {
			t.Errorf("hub.md: %d live blocks, want 60 (every chunk fits a request of its own)", n)
		}
		if err != nil {
			t.Errorf("ctx ingest returned %v, want nil: every chunk was written", err)
		}
		for i, c := range calls.all() {
			if c.bytes > IngestMaxBodySize {
				t.Errorf("request %d: %d bytes, over the mounted limit of %d", i, c.bytes, IngestMaxBodySize)
			}
			if c.status != http.StatusOK {
				t.Errorf("request %d: status %d, want 200", i, c.status)
			}
		}
	})

	// One chunk whose metadata.links alone is over 10 MB (1 100 targets of
	// 10 000 bytes): no request can carry it. The file is held back before
	// the upload and named; the other files are written; the run exits 1.
	t.Run("chunk_over_request_limit", func(t *testing.T) {
		url, pool, key, calls := wireSizeServer(t)
		vault := t.TempDir()
		normals := []string{"a-note-1.md", "a-note-2.md", "z-note-3.md"}
		for _, rel := range normals {
			writeNote(t, vault, rel, plainNote("MARKER-"+rel))
		}
		writeNote(t, vault, "giant.md", giantLinksNote(1100, 10_000))

		out, err := runIngestCLI(t, url, key, "ingest", vault, "--category", "wiresize")
		logIngestSummary(t, out, calls.all())

		for _, rel := range normals {
			if n := blocksOfFile(t, pool, rel); n != 1 {
				t.Errorf("%s: %d live blocks, want 1 — the file with a chunk over the request limit took its batch down", rel, n)
			}
		}
		if n := blocksOfFile(t, pool, "giant.md"); n != 0 {
			t.Errorf("giant.md: %d live blocks, want 0", n)
		}
		if err == nil {
			t.Errorf("ctx ingest returned nil, want an error: giant.md was not ingested")
		}
		const want = "giant.md: not uploaded: chunk 1 of 1: the request for this chunk alone is "
		if !strings.Contains(out, want) || !strings.Contains(out, fmt.Sprintf("the server accepts at most %d", IngestMaxBodySize)) {
			t.Errorf("stderr does not hold giant.md back with its size and the limit:\n%s", out)
		}
		for i, c := range calls.all() {
			if c.bytes > IngestMaxBodySize || c.status != http.StatusOK {
				t.Errorf("request %d: %d bytes, status %d; want every request within the limit and 200", i, c.bytes, c.status)
			}
		}
	})
}

// edgeBody is a valid one-chunk ingest request of exactly total bytes; the
// padding sits in the chunk's metadata, so the content stays within the
// per-chunk limit.
func edgeBody(t *testing.T, title string, total int) string {
	t.Helper()
	head := `{"source":"wire-edge","chunks":[{"category":"wireedge","title":"` + title +
		`","content":"edge content for the body cap probe","metadata":{"pad":"`
	tail := `"}}]}`
	pad := total - len(head) - len(tail)
	if pad < 0 {
		t.Fatalf("total %d too small", total)
	}
	return head + strings.Repeat("a", pad) + tail
}

// TestIngestBodyCapBoundary_Integration measures the mounted body limit at
// its edge: a request of exactly IngestMaxBodySize bytes, one byte under it
// and one byte over it, through the production router over real HTTP. The
// first two are written, which is why `ctx ingest` fills a batch up to the
// limit itself and keeps no margin; the third is refused before a chunk is
// written, and the answer names the cap.
func TestIngestBodyCapBoundary_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	url, pool, key, _ := wireSizeServer(t)
	cases := []struct {
		name   string
		total  int
		status int
	}{
		{"at_limit", IngestMaxBodySize, http.StatusOK},
		{"one_byte_under", IngestMaxBodySize - 1, http.StatusOK},
		{"one_byte_over", IngestMaxBodySize + 1, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title := "wire-edge-" + tc.name
			body := edgeBody(t, title, tc.total)
			if len(body) != tc.total {
				t.Fatalf("fixture is %d bytes, want %d", len(body), tc.total)
			}
			req, err := http.NewRequest(http.MethodPost, url+"/api/ingest", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Context-Key", key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Logf("%s: %d bytes -> status %d, body %.200s", tc.name, tc.total, resp.StatusCode, raw)

			var n int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM context_blocks WHERE title = $1 AND NOT is_archived`, title).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status {
				t.Errorf("status %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.status == http.StatusOK && n != 1 {
				t.Errorf("%d blocks titled %q, want 1", n, title)
			}
			if tc.status != http.StatusOK {
				if n != 0 {
					t.Errorf("%d blocks titled %q, want 0: the refused request wrote", n, title)
				}
				if want := fmt.Sprintf("request body exceeds %d MB cap", IngestMaxBodySize>>20); !strings.Contains(string(raw), want) {
					t.Errorf("answer %q does not name the cap (%q)", raw, want)
				}
			}
		})
	}
}
