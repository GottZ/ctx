package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runIngestCapture runs `ctx ingest <vault> args...` against a fake
// /api/ingest that records every chunk payload and answers success.
func runIngestCapture(t *testing.T, vault string, args ...string) []ingestChunkPayload {
	t.Helper()
	var (
		mu     sync.Mutex
		chunks []ingestChunkPayload
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ingestRequestPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		chunks = append(chunks, req.Chunks...)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(ingestResponsePayload{Success: true, Total: len(req.Chunks), Inserted: len(req.Chunks)})
	}))
	defer srv.Close()

	cmd := IngestCmd(func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "k", HTTPClient: srv.Client()}, nil
	})
	cmd.SetArgs(append([]string{vault}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ctx ingest: %v", err)
	}
	return chunks
}

func writeIngestFixture(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Two files with the same name in different folders must not share a chunk
// title: block-mode ingest upserts by (category, title, scope), so a shared
// title lets the second file overwrite the first.
func TestIngestTitleCarriesRepoRelativePath(t *testing.T) {
	vault := t.TempDir()
	body := "# Setup\n\n" + strings.Repeat("Install the binary and run the first query. ", 8) + "\n"
	writeIngestFixture(t, vault, "README.md", body)
	writeIngestFixture(t, vault, "docs/README.md", body)
	writeIngestFixture(t, vault, "docs/guide/README.md", body)

	chunks := runIngestCapture(t, vault, "--category", "reference")

	want := map[string]string{
		"README.md":            "README",
		"docs/README.md":       "docs/README",
		"docs/guide/README.md": "docs/guide/README",
	}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d: %+v", len(chunks), len(want), chunks)
	}
	for _, ch := range chunks {
		path, _ := ch.Metadata["vault_path"].(string)
		if ch.Title != want[path] {
			t.Errorf("vault_path %q: title = %q, want %q", path, ch.Title, want[path])
		}
	}
}

// A long file splits by heading; every chunk title keeps the path prefix.
func TestIngestTitleKeepsPathOnHeadingChunks(t *testing.T) {
	vault := t.TempDir()
	section := strings.Repeat("Paragraph text that fills the section. ", 120)
	writeIngestFixture(t, vault, "docs/guide/README.md", "# Setup\n\n"+section+"\n\n# Usage\n\n"+section+"\n")

	chunks := runIngestCapture(t, vault)

	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want a heading split: %+v", len(chunks), chunks)
	}
	for _, ch := range chunks {
		if !strings.HasPrefix(ch.Title, "docs/guide/README — ") {
			t.Errorf("chunk %d title = %q, want prefix %q", ch.ChunkIndex, ch.Title, "docs/guide/README — ")
		}
	}
}
