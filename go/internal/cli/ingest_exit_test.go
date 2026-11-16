package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/ingest"
)

// `ctx ingest` reported a refused or failed batch on stderr and still exited
// 0, and the server refuses a whole request when one chunk breaks a per-chunk
// limit, so one over-long title failed the chunks of every other file in its
// batch (#318). These tests run the command against fake servers; the
// integration test in internal/handler runs it against the real handler.

// fakeIngest answers POST /api/ingest like the production handler validates:
// a request with a chunk over a per-chunk limit is refused whole with 400,
// any other request is accepted chunk for chunk.
type fakeIngest struct {
	mu       sync.Mutex
	accepted []ingestChunkPayload
	requests int
}

func (f *fakeIngest) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var req ingestRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"success":false,"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	for i, c := range req.Chunks {
		var reason string
		switch {
		case strings.TrimSpace(c.Category) == "":
			reason = "category is required"
		case len(c.Category) > ingest.MaxCategoryBytes:
			reason = "category too long"
		case strings.TrimSpace(c.Title) == "":
			reason = "title is required"
		case len(c.Title) > ingest.MaxTitleBytes:
			reason = "title too long"
		case len(c.Content) > ingest.MaxContentBytes:
			reason = "content too long"
		}
		if reason != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": fmt.Sprintf("chunk[%d]: %s", i, reason)})
			return
		}
	}
	f.accepted = append(f.accepted, req.Chunks...)
	_ = json.NewEncoder(w).Encode(ingestResponsePayload{Success: true, Total: len(req.Chunks), Inserted: len(req.Chunks)})
}

// acceptedFrom counts the accepted chunks of the file at vault path rel.
func (f *fakeIngest) acceptedFrom(rel string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.accepted {
		if p, _ := c.Metadata["vault_path"].(string); p == rel {
			n++
		}
	}
	return n
}

// runIngestCmd runs `ctx ingest <vault> args...` against baseURL and returns
// what the run printed on stderr (the summary, and cobra's own lines) and the
// error that decides the exit status: cmd/ctx/main.go exits 1 on any error.
func runIngestCmd(t *testing.T, baseURL, vault string, args ...string) (string, error) {
	t.Helper()
	errFile, err := os.Create(filepath.Join(t.TempDir(), "stderr.txt"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = errFile
	cmd := IngestCmd(func() (*Client, error) {
		return &Client{BaseURL: baseURL, Key: "k", HTTPClient: &http.Client{Timeout: 10 * time.Second}}, nil
	})
	cmd.SetArgs(append([]string{vault}, args...))
	runErr := cmd.Execute()
	os.Stderr = old
	_ = errFile.Close()
	out, err := os.ReadFile(errFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

// ingestNote is a one-chunk note carrying marker.
func ingestNote(marker string) string {
	return "# Note\n\n" + marker + " " + strings.Repeat("Run the importer and compare the counts with the source. ", 6) + "\n"
}

// repeatedSections is a file of n "## <heading>" sections of about 800 bytes,
// long enough together to be split by heading.
func repeatedSections(heading string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "## %s\n\nsection-%d %s\n\n", heading, i, strings.Repeat("Check the counts against the source and log the run. ", 15))
	}
	return b.String()
}

// pathOfLen is a vault-relative path without .md of exactly n bytes, in
// folders short enough for any filesystem's 255-byte name limit.
func pathOfLen(t *testing.T, n int) string {
	t.Helper()
	var parts []string
	for n > 200 {
		parts = append(parts, strings.Repeat("d", 199))
		n -= 200
	}
	if n < 1 {
		t.Fatalf("pathOfLen: no room for a file name")
	}
	return strings.Join(append(parts, strings.Repeat("f", n)), "/")
}

// A file whose chunk breaks a per-chunk limit of POST /api/ingest is held
// back and reported with its path, the size and the limit; the other files
// of the run, in the same batch, are written, and the run exits non-zero.
// Red before #318: the file's chunks went out with the rest and the server
// refused the whole batch, so good.md was not written either, and the run
// exited 0.
func TestIngestHoldsBackFileOverServerLimit(t *testing.T) {
	// The first chunk title of the repeated file is exactly MaxTitleBytes:
	// the path leaves one byte for the heading "X" after " — ". Counters 2
	// to 9 fit as the bare number, the tenth chunk's "10" does not (#317).
	oneByteRoom := pathOfLen(t, ingest.MaxTitleBytes-len(" — ")-1)
	longTitle := pathOfLen(t, ingest.MaxTitleBytes+3)
	longDir := strings.Repeat("c", ingest.MaxCategoryBytes+1)
	bigParagraph := "# Dump\n\n" + strings.Repeat("x", ingest.MaxContentBytes+10) + "\n"

	cases := []struct {
		name  string
		rel   string   // the file that breaks a limit
		body  string   // its content
		args  []string // flags of the run
		size  int      // the size the report names
		limit int      // the limit the report names
	}{
		{"title_over_limit", longTitle + ".md", ingestNote("long"), []string{"--category", "limits"},
			ingest.MaxTitleBytes + 3, ingest.MaxTitleBytes},
		{"tenth_repeated_heading_with_one_byte_of_room", oneByteRoom + ".md", repeatedSections("X", 10), []string{"--category", "limits"},
			ingest.MaxTitleBytes + 1, ingest.MaxTitleBytes},
		{"category_over_limit", longDir + "/note.md", ingestNote("cat"), nil,
			ingest.MaxCategoryBytes + 1, ingest.MaxCategoryBytes},
		{"content_over_limit", "dump.md", bigParagraph, []string{"--category", "limits"},
			0, ingest.MaxContentBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			writeIngestFixture(t, vault, "good.md", ingestNote("good"))
			writeIngestFixture(t, vault, tc.rel, tc.body)
			f := &fakeIngest{}
			srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
			defer srv.Close()

			stderr, err := runIngestCmd(t, srv.URL, vault, tc.args...)

			if n := f.acceptedFrom("good.md"); n != 1 {
				t.Errorf("good.md: %d chunks written, want 1 — a file over a limit must not take the others' batch down", n)
			}
			if n := f.acceptedFrom(tc.rel); n != 0 {
				t.Errorf("%s: %d chunks written, want 0 — the whole file is held back", tc.name, n)
			}
			if err == nil {
				t.Errorf("run returned nil, want an error: a file was not ingested, the exit status must say so")
			}
			wantInReport(t, stderr, tc.rel, strconv.Itoa(tc.limit))
			if tc.size > 0 {
				wantInReport(t, stderr, strconv.Itoa(tc.size)+" bytes")
			}
		})
	}
}

// A file whose name leaves the title empty (".md" in the vault root) is held
// back the same way: the server refuses a request with an empty title whole.
func TestIngestHoldsBackFileWithEmptyTitle(t *testing.T) {
	vault := t.TempDir()
	writeIngestFixture(t, vault, "good.md", ingestNote("good"))
	writeIngestFixture(t, vault, ".md", ingestNote("nameless"))
	f := &fakeIngest{}
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	defer srv.Close()

	stderr, err := runIngestCmd(t, srv.URL, vault)

	if n := f.acceptedFrom("good.md"); n != 1 {
		t.Errorf("good.md: %d chunks written, want 1", n)
	}
	if err == nil {
		t.Errorf("run returned nil, want an error")
	}
	wantInReport(t, stderr, ".md:", "title is empty")
}

// --dry-run sends nothing, and it predicts the run: a file the upload would
// hold back, or one it cannot read, makes the dry run exit non-zero too.
func TestIngestDryRunReportsWhatTheUploadWouldHoldBack(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, vault string)
		wantErr bool
		want    []string
	}{
		{"clean", func(*testing.T, string) {}, false, nil},
		{"title_over_limit", func(t *testing.T, vault string) {
			writeIngestFixture(t, vault, pathOfLen(t, ingest.MaxTitleBytes+1)+".md", ingestNote("long"))
		}, true, []string{strconv.Itoa(ingest.MaxTitleBytes+1) + " bytes"}},
		{"unreadable_file", func(t *testing.T, vault string) {
			danglingMarkdown(t, vault, "broken.md")
		}, true, []string{"broken.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			writeIngestFixture(t, vault, "good.md", ingestNote("good"))
			tc.prepare(t, vault)
			f := &fakeIngest{}
			srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
			defer srv.Close()

			stderr, err := runIngestCmd(t, srv.URL, vault, "--dry-run", "--category", "limits")

			if f.requests != 0 {
				t.Errorf("dry run sent %d requests, want 0", f.requests)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
			wantInReport(t, stderr, tc.want...)
		})
	}
}

// danglingMarkdown puts a symlink named rel into vault that points nowhere:
// the walk lists it as a .md file and reading it fails, even as root.
func danglingMarkdown(t *testing.T, vault, rel string) {
	t.Helper()
	if err := os.Symlink(filepath.Join(vault, "missing-target"), filepath.Join(vault, rel)); err != nil {
		t.Fatal(err)
	}
}

// Every way a chunk can end up unwritten sets the exit status, and the
// summary stays on stderr before the command returns. Cases that write
// everything, or have nothing to write, stay at exit 0.
func TestIngestExitStatusPerFailurePath(t *testing.T) {
	jsonReply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	accepting := func() http.HandlerFunc { return (&fakeIngest{}).serveHTTP }
	cases := []struct {
		name    string
		prepare func(t *testing.T, vault string) // nil = good.md only
		server  http.HandlerFunc                 // nil = nothing listens
		wantErr bool
		want    []string // on stderr
	}{
		{name: "all_written", server: accepting(), want: []string{"Inserted: 1", "Failed:   0"}},
		{name: "no_markdown_files", prepare: func(t *testing.T, vault string) {
			if err := os.Remove(filepath.Join(vault, "good.md")); err != nil {
				t.Fatal(err)
			}
		}, server: accepting(), want: []string{"No .md files found."}},
		{name: "unreadable_file", prepare: func(t *testing.T, vault string) {
			danglingMarkdown(t, vault, "broken.md")
		}, server: accepting(), wantErr: true, want: []string{"broken.md", "Inserted: 1"}},
		{name: "unreadable_only_file", prepare: func(t *testing.T, vault string) {
			if err := os.Remove(filepath.Join(vault, "good.md")); err != nil {
				t.Fatal(err)
			}
			danglingMarkdown(t, vault, "broken.md")
		}, server: accepting(), wantErr: true, want: []string{"broken.md", "No chunks to upload."}},
		{name: "server_unreachable", server: nil, wantErr: true, want: []string{"batch-0", "Failed:   1"}},
		{name: "rate_limited", server: jsonReply(http.StatusTooManyRequests, `{"success":false,"error":"rate limit exceeded"}`),
			wantErr: true, want: []string{"rate limit exceeded", "Failed:   1"}},
		{name: "unauthorized", server: jsonReply(http.StatusUnauthorized, `{"error":"unauthorized"}`),
			wantErr: true, want: []string{"unauthorized", "Failed:   1"}},
		{name: "not_json", server: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		}, wantErr: true, want: []string{"decode response (status 502)", "Failed:   1"}},
		// The documented partial success (docs/api.md, POST /api/ingest): 200,
		// success:false, failed > 0 and a failed result per unwritten chunk next
		// to the written ones. The summary counts it chunk by chunk and names
		// the file of the failed chunk.
		{name: "chunk_failed_in_answer", prepare: func(t *testing.T, vault string) {
			writeIngestFixture(t, vault, "other.md", ingestNote("other"))
		}, server: failChunksOf("other.md"),
			wantErr: true, want: []string{"other.md: chunk 1: upsert failed", "Inserted: 1", "Failed:   1", "1 chunk(s) not written"}},
		// A refusal at 200 without per-chunk results stays a refusal of the batch.
		{name: "refused_without_results", server: jsonReply(http.StatusOK, `{"success":false,"error":"gate failure"}`),
			wantErr: true, want: []string{"batch-0: server: gate failure", "Failed:   1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			writeIngestFixture(t, vault, "good.md", ingestNote("good"))
			if tc.prepare != nil {
				tc.prepare(t, vault)
			}
			var url string
			if tc.server != nil {
				srv := httptest.NewServer(tc.server)
				defer srv.Close()
				url = srv.URL
			} else {
				srv := httptest.NewServer(http.NotFoundHandler())
				url = srv.URL
				srv.Close()
			}

			stderr, err := runIngestCmd(t, url, vault, "--category", "limits")

			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(stderr, "Usage:") {
				t.Errorf("a run that was called right printed its usage after the summary:\n%s", stderr)
			}
			wantInReport(t, stderr, tc.want...)
		})
	}
}

// failChunksOf answers like the handler when the chunks of the file at vault
// path rel fail in the store: 200, success:false, a failed result for each of
// them and inserted for the rest.
func failChunksOf(rel string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ingestRequestPayload
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := ingestResponsePayload{Total: len(req.Chunks)}
		for i, c := range req.Chunks {
			res := ingestResultPayload{Index: i, Status: "inserted"}
			if p, _ := c.Metadata["vault_path"].(string); p == rel {
				res = ingestResultPayload{Index: i, Status: "failed", Error: "upsert failed"}
				resp.Failed++
			} else {
				resp.Inserted++
			}
			resp.Results = append(resp.Results, res)
		}
		resp.Success = resp.Failed == 0
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// A chunk exactly at a limit is accepted by the server and uploaded, and a
// title or category of spaces only counts as empty, as the handler trims.
func TestIngestLimitBoundaries(t *testing.T) {
	header := len("Datei: dump | Sektion: Dump\n\n") // the context header of a long chunk
	cases := []struct {
		name     string
		rel      string
		body     string
		args     []string
		uploaded bool
		want     []string
	}{
		{"title_at_limit", pathOfLen(t, ingest.MaxTitleBytes) + ".md", ingestNote("t"), []string{"--category", "limits"}, true, nil},
		{"category_at_limit", strings.Repeat("c", ingest.MaxCategoryBytes) + "/note.md", ingestNote("c"), nil, true, nil},
		{"content_at_limit", "dump.md", "# Dump\n\n" + strings.Repeat("x", ingest.MaxContentBytes-header) + "\n", []string{"--category", "limits"}, true, nil},
		{"title_of_spaces", "  .md", ingestNote("s"), nil, false, []string{"the title is empty"}},
		{"category_of_spaces", "note.md", ingestNote("s"), []string{"--category", " "}, false, []string{"the category is empty"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			writeIngestFixture(t, vault, tc.rel, tc.body)
			f := &fakeIngest{}
			srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
			defer srv.Close()

			stderr, err := runIngestCmd(t, srv.URL, vault, tc.args...)

			got := f.acceptedFrom(tc.rel)
			if tc.uploaded {
				if got != 1 || err != nil {
					t.Fatalf("%s: %d chunks written, err %v; want 1 and nil:\n%s", tc.rel, got, err, stderr)
				}
				if tc.name == "content_at_limit" && len(f.accepted[0].Content) != ingest.MaxContentBytes {
					t.Fatalf("test setup: content is %d bytes, want exactly %d", len(f.accepted[0].Content), ingest.MaxContentBytes)
				}
				return
			}
			if got != 0 || err == nil {
				t.Errorf("%s: %d chunks written, err %v; want 0 and an error", tc.rel, got, err)
			}
			wantInReport(t, stderr, tc.want...)
		})
	}
}

// The limits hold for what the server decodes, not for the bytes on disk:
// json.Marshal sends every byte that is not valid UTF-8 as U+FFFD, three
// bytes. A Latin-1 heading or paragraph within the limits on disk is over them
// on the server and must be held back like any other.
func TestIngestMeasuresLimitsAsTheServerDecodes(t *testing.T) {
	latin1 := func(n int) string { return strings.Repeat("\xe4", n) }
	body := strings.Repeat("Check the counts against the source and log the run. ", 160)
	cases := []struct {
		name, rel, doc string
	}{
		// 200 bytes on disk, 600 on the server: the title passes 500 there.
		{"heading", "latin1-heading.md", "# " + latin1(200) + "\n\n" + body + "\n"},
		// 20 000 bytes on disk, 60 000 on the server: over 50 KB there.
		{"paragraph", "latin1-paragraph.md", "# Dump\n\n" + latin1(20000) + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			writeIngestFixture(t, vault, "good.md", ingestNote("good"))
			writeIngestFixture(t, vault, tc.rel, tc.doc)
			f := &fakeIngest{}
			srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
			defer srv.Close()

			stderr, err := runIngestCmd(t, srv.URL, vault, "--category", "limits")

			if n := f.acceptedFrom("good.md"); n != 1 {
				t.Errorf("good.md: %d chunks written, want 1:\n%s", n, stderr)
			}
			if err == nil {
				t.Errorf("run returned nil, want an error")
			}
			wantInReport(t, stderr, tc.rel+": not uploaded")
		})
	}
}

// --workers below 1 would start no upload at all and report nothing failed;
// it is refused as an argument before the vault is read.
func TestIngestRefusesWorkersBelowOne(t *testing.T) {
	vault := t.TempDir()
	writeIngestFixture(t, vault, "good.md", ingestNote("good"))
	f := &fakeIngest{}
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	defer srv.Close()

	_, err := runIngestCmd(t, srv.URL, vault, "--workers", "0")

	if err == nil || !strings.Contains(err.Error(), "--workers") {
		t.Errorf("err = %v, want a --workers argument error", err)
	}
	if f.requests != 0 {
		t.Errorf("%d requests sent, want 0", f.requests)
	}
}

func wantInReport(t *testing.T, stderr string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(stderr, w) {
			t.Errorf("stderr lacks %q:\n%s", w, stderr)
		}
	}
}
