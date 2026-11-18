package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/ingest"
)

// `ctx ingest` wrote a note's link list and date list into the metadata of
// every chunk of the note, so a note with many links multiplied the list by
// its chunk count, in the request and in every stored block (#329). The lists
// now go with the note's first chunk; its other chunks carry both keys as
// null, which replaces a list a block stored before (an ingest of unchanged
// content merges the metadata, so a missing key would leave it in place).
// The integration test in internal/handler runs the command against the
// production handler on PG18.

// linkedNote is a note of sections "## Part 0".."## Part n-1" of about 2.7 KB,
// split into one chunk per section, with one wikilink per section and a date
// in its frontmatter.
func linkedNote(sections int) string {
	var b strings.Builder
	b.WriteString("---\ndate: 2026-03-04\n---\n")
	for i := 0; i < sections; i++ {
		fmt.Fprintf(&b, "## Part %d\n\n[[Target %d]] %s\n\n", i, i, strings.Repeat("Check the counts against the source and log the run. ", 50))
	}
	return b.String()
}

// fileChunks is the accepted chunks of the file at vault path rel, in chunk order.
func (f *fakeIngest) fileChunks(rel string) []ingestChunkPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ingestChunkPayload
	for _, c := range f.accepted {
		if p, _ := c.Metadata["vault_path"].(string); p == rel {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b ingestChunkPayload) int { return a.ChunkIndex - b.ChunkIndex })
	return out
}

// listOf is the metadata value under key as a string list, and whether it is one.
func listOf(meta map[string]any, key string) ([]string, bool) {
	raw, ok := meta[key].([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out, true
}

// The first chunk of a note carries its links and dates; every other chunk
// carries both keys as null. A note with one chunk and a note without links
// or dates are sent as before, byte for byte.
func TestIngestNoteListsOnFirstChunkOnly(t *testing.T) {
	vault := t.TempDir()
	writeIngestFixture(t, vault, "multi.md", linkedNote(4))
	writeIngestFixture(t, vault, "single.md", "---\ndate: 2026-05-06\n---\n# Single\n\n[[Only target]] "+
		strings.Repeat("Run the importer and compare the counts with the source. ", 6)+"\n")
	writeIngestFixture(t, vault, "plain.md", repeatedSections("Plain", 12))
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run_%v", dry), func(t *testing.T) {
			c := &cappedIngest{}
			srv := httptest.NewServer(http.HandlerFunc(c.serveHTTP))
			defer srv.Close()
			args := []string{"--category", "lists"}
			if dry {
				args = append(args, "--dry-run")
			}
			stderr, err := runIngestCmd(t, srv.URL, vault, args...)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, stderr)
			}
			if dry {
				if n := len(c.requests()); n != 0 {
					t.Errorf("dry run sent %d requests", n)
				}
				return
			}

			multi := c.fileChunks("multi.md")
			if len(multi) < 3 {
				t.Fatalf("multi.md: %d chunks, want at least 3", len(multi))
			}
			for i, ch := range multi {
				links, linksIsList := listOf(ch.Metadata, "links")
				dates, datesIsList := listOf(ch.Metadata, "dates")
				if i == 0 {
					if !linksIsList || len(links) != len(multi) || links[0] != "Target 0" {
						t.Errorf("multi.md chunk 1: links %v, want the note's %d targets", ch.Metadata["links"], len(multi))
					}
					if !datesIsList || !slices.Equal(dates, []string{"2026-03-04"}) {
						t.Errorf("multi.md chunk 1: dates %v, want [2026-03-04]", ch.Metadata["dates"])
					}
					continue
				}
				for _, key := range []string{"links", "dates"} {
					v, present := ch.Metadata[key]
					if !present || v != nil {
						t.Errorf("multi.md chunk %d: %s = %v (present %v), want null — the list goes with the first chunk only",
							i+1, key, v, present)
					}
				}
			}

			single := c.fileChunks("single.md")
			if len(single) != 1 {
				t.Fatalf("single.md: %d chunks, want 1", len(single))
			}
			wantMeta := `"metadata":{"dates":["2026-05-06"],"links":["Only target"],"source":"obsidian","vault_path":"single.md"}`
			if !c.sent(wantMeta) {
				t.Errorf("single.md: no request carries %s", wantMeta)
			}

			for i, ch := range c.fileChunks("plain.md") {
				for _, key := range []string{"links", "dates"} {
					if _, present := ch.Metadata[key]; present {
						t.Errorf("plain.md chunk %d: carries %s, the note has none", i+1, key)
					}
				}
			}
			if len(c.fileChunks("plain.md")) < 2 {
				t.Errorf("plain.md: %d chunks, want several", len(c.fileChunks("plain.md")))
			}
		})
	}
}

// A note in a folder followed out of the vault by a link (#324) is sent like
// any other: its lists on its first chunk, under its path through the link.
func TestIngestNoteListsThroughFolderLink(t *testing.T) {
	outside := t.TempDir()
	writeIngestFixture(t, outside, "ext.md", linkedNote(3))
	vault := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(vault, "shared"))
	c := &cappedIngest{}
	srv := httptest.NewServer(http.HandlerFunc(c.serveHTTP))
	defer srv.Close()
	if stderr, err := runIngestCmd(t, srv.URL, vault, "--category", "lists"); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	ext := c.fileChunks("shared/ext.md")
	if len(ext) != 3 {
		t.Fatalf("shared/ext.md: %d chunks, want 3", len(ext))
	}
	for i, ch := range ext {
		_, isList := listOf(ch.Metadata, "links")
		v, present := ch.Metadata["links"]
		if (i == 0) != isList || (i > 0 && (!present || v != nil)) {
			t.Errorf("shared/ext.md chunk %d: links %v (present %v); want the list on chunk 1 only, null after", i+1, v, present)
		}
	}
}

// sent reports whether an accepted request body contains s.
func (c *cappedIngest) sent(s string) bool {
	for _, r := range c.requests() {
		if bytes.Contains(r.body, []byte(s)) {
			return true
		}
	}
	return false
}

// The #323 hub note (9 000 links, 60 chunks): its link list is sent once, on
// its first chunk, not 60 times.
func TestIngestHubNoteSendsItsLinksOnce(t *testing.T) {
	vault := t.TempDir()
	writeIngestFixture(t, vault, "hub.md", hubNote(9000))
	c := &cappedIngest{}
	srv := httptest.NewServer(http.HandlerFunc(c.serveHTTP))
	defer srv.Close()

	if stderr, err := runIngestCmd(t, srv.URL, vault, "--category", "hub"); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	hub := c.fileChunks("hub.md")
	if len(hub) != 60 {
		t.Fatalf("hub.md: %d chunks, want 60", len(hub))
	}
	carriers := 0
	for _, ch := range hub {
		if links, ok := listOf(ch.Metadata, "links"); ok {
			carriers++
			if ch.ChunkIndex != 0 || len(links) != 9000 {
				t.Errorf("chunk %d carries %d links, want only the first chunk with all 9000", ch.ChunkIndex+1, len(links))
			}
		}
	}
	if carriers != 1 {
		t.Errorf("%d chunks carry the link list, want 1", carriers)
	}
	total, reqs := 0, c.requests()
	target := []byte(`"Linked note number 04500"`)
	listed := 0
	for _, r := range reqs {
		total += r.bytes
		listed += bytes.Count(r.body, target)
	}
	if listed != 1 {
		t.Errorf("the link target is listed %d times in the requests, want 1", listed)
	}
	t.Logf("%d requests, %d bytes: %v", len(reqs), total, sizes(reqs))
}

// A note whose link list alone nearly fills a request (1 030 targets of
// 10 000 bytes, 10.3 MB) and whose second chunk is long (40 000 bytes of "<",
// 240 KB on the wire): the second chunk repeated the list and passed the 10 MB
// limit, so the file was held back. Now the first chunk goes in a request of
// its own and the second with the other notes; the run exits 0.
func TestIngestUploadsNoteWhoseListsFitTheFirstChunk(t *testing.T) {
	vault := t.TempDir()
	normals := []string{"a-note.md", "z-note.md"}
	for _, rel := range normals {
		writeIngestFixture(t, vault, rel, ingestNote("MARKER-"+rel))
	}
	writeIngestFixture(t, vault, "giant.md", "# Giant\n\n"+bigLinks(1030, 10_000)+"\n\n## Escaped\n\n"+strings.Repeat("<", 40_000)+"\n")
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run_%v", dry), func(t *testing.T) {
			c := &cappedIngest{}
			srv := httptest.NewServer(http.HandlerFunc(c.serveHTTP))
			defer srv.Close()
			args := []string{"--category", "wiresize"}
			if dry {
				args = append(args, "--dry-run")
			}
			stderr, err := runIngestCmd(t, srv.URL, vault, args...)
			if err != nil {
				t.Fatalf("run returned %v, want nil: every file fits\n%s", err, stderr)
			}
			if strings.Contains(stderr, "not uploaded") {
				t.Errorf("a file was held back:\n%s", stderr)
			}
			if dry {
				return
			}
			if n := c.acceptedFrom("giant.md"); n != 2 {
				t.Errorf("giant.md: %d chunks written, want 2", n)
			}
			for _, rel := range normals {
				if n := c.acceptedFrom(rel); n != 1 {
					t.Errorf("%s: %d chunks written, want 1", rel, n)
				}
			}
			for _, r := range c.requests() {
				if r.refused || r.bytes > ingest.MaxRequestBytes {
					t.Errorf("a request of %d bytes was sent over the limit", r.bytes)
				}
			}
			t.Logf("requests: %v", sizes(c.requests()))
		})
	}
}
