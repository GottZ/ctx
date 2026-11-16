//go:build integration

// `ctx ingest` titles a chunk `<path> — <heading>`. A heading that repeats
// inside one file (two sections called "Notes" under different chapters, or
// the same chapter heading twice) gave two chunks one title, and block-mode
// ingest upserts by (category, title, scope): the server writes the chunks of
// a batch one after another, each in its own transaction, so the later chunk
// lands on the earlier one's row and replaces its content. Drives the FULL
// cobra tree against the production HandleIngest on a real PG18 (CLI chunker
// -> POST /api/ingest -> store.UpsertBlock).
//
// RED against the parent commit: in every repeated-heading case the earlier
// section's marker is in no live block.
//
//	go test -tags=integration ./internal/handler/ -run TestIngestCLIRepeatedHeading -count=1 -v
package handler

import (
	"os"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

// repeatedHeadingBody is a section body of roughly n bytes in paragraphs of
// about 1000 bytes, starting with marker, so the chunker can split it by
// paragraph and every part stays identifiable through its own marker.
func repeatedHeadingBody(marker string, n int) string {
	para := strings.Repeat("Run the importer, check the counts and compare them with the source. ", 14)
	var b strings.Builder
	b.WriteString(marker + " " + para)
	for b.Len() < n {
		b.WriteString("\n\n" + para)
	}
	return b.String()
}

// runIngestCLIStderr runs the CLI like w5RunCLI and returns what it printed
// on stderr: `ctx ingest` reports its summary there.
func runIngestCLIStderr(t *testing.T, baseURL string, args ...string) (string, error) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		var buf strings.Builder
		tmp := make([]byte, 4096)
		for {
			n, e := r.Read(tmp)
			buf.Write(tmp[:n])
			if e != nil {
				break
			}
		}
		done <- buf.String()
	}()
	_, runErr := w5RunCLI(t, baseURL, "k", args...)
	_ = w.Close()
	os.Stderr = orig
	return <-done, runErr
}

func TestIngestCLIRepeatedHeading(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	srv := ingestCLIServer(t, pool)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")

	intro := func(m string) string { return repeatedHeadingBody(m, 300) }
	notes := func(m string) string { return repeatedHeadingBody(m, 3800) }
	// A deep path and long headings: the title with the parent heading in it
	// would pass the server's 500-byte title limit, the title without it does not.
	longDir := strings.Repeat("d", 60) + "/" + strings.Repeat("e", 60) + "/" + strings.Repeat("f", 60)
	longParent := func(name string) string { return name + " " + strings.Repeat("p", 160) }
	longNotes := "Notes " + strings.Repeat("n", 140)

	cases := []struct {
		name    string
		file    string
		doc     func(m func(string) string) string
		markers []string
	}{
		{
			// The issue's shape: the same H2 under two H1 chapters.
			name: "h2_under_two_h1", file: "docs/guide/README.md",
			doc: func(m func(string) string) string {
				return "# Alpha\n\n" + intro(m("alpha")) + "\n\n## Notes\n\n" + notes(m("alpha-notes")) +
					"\n\n# Beta\n\n" + intro(m("beta")) + "\n\n## Notes\n\n" + notes(m("beta-notes")) + "\n"
			},
			markers: []string{"alpha", "alpha-notes", "beta", "beta-notes"},
		},
		{
			// A repeated H1 has no parent heading to tell the two apart.
			name: "repeated_h1", file: "notes.md",
			doc: func(m func(string) string) string {
				return "# Notes\n\n" + notes(m("first")) + "\n\n# Notes\n\n" + notes(m("second")) + "\n"
			},
			markers: []string{"first", "second"},
		},
		{
			// Each repeated section is over the chunk size and splits into
			// "(Teil N)" parts; the parts collide pairwise across the sections.
			name: "oversized_repeated_section", file: "guide.md",
			doc: func(m func(string) string) string {
				return "# Alpha\n\n## Notes\n\n" + repeatedHeadingBody(m("alpha-1"), 6000) + "\n\n" + repeatedHeadingBody(m("alpha-2"), 1000) +
					"\n\n# Beta\n\n## Notes\n\n" + repeatedHeadingBody(m("beta-1"), 6000) + "\n\n" + repeatedHeadingBody(m("beta-2"), 1000) + "\n"
			},
			markers: []string{"alpha-1", "alpha-2", "beta-1", "beta-2"},
		},
		{
			// The parent heading would push the title over the 500-byte limit,
			// which refuses the whole request; the title must stay inside it.
			name: "parent_would_pass_title_limit", file: longDir + "/README.md",
			doc: func(m func(string) string) string {
				return "# " + longParent("Alpha") + "\n\n" + intro(m("alpha")) + "\n\n## " + longNotes + "\n\n" + notes(m("alpha-notes")) +
					"\n\n# " + longParent("Beta") + "\n\n" + intro(m("beta")) + "\n\n## " + longNotes + "\n\n" + notes(m("beta-notes")) + "\n"
			},
			markers: []string{"alpha", "alpha-notes", "beta", "beta-notes"},
		},
		{
			// Guard, not a red case: the unique index compares titles byte for
			// byte, so headings that differ only in case are two blocks already.
			name: "case_differs", file: "case.md",
			doc: func(m func(string) string) string {
				return "## Notes\n\n" + notes(m("upper")) + "\n\n## notes\n\n" + notes(m("lower")) + "\n"
			},
			markers: []string{"upper", "lower"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			category := "rh-" + strings.ReplaceAll(tc.name, "_", "-")
			marker := func(s string) string { return "MARKER-" + tc.name + "-" + s + "-END" }
			writeVaultFile(t, vault, tc.file, tc.doc(marker))

			out, err := runIngestCLIStderr(t, srv.URL, "ingest", vault, "--category", category)
			if err != nil {
				t.Fatalf("ctx ingest: %v", err)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "Inserted:") || strings.Contains(line, "Updated:") ||
					strings.Contains(line, "Failed:") || strings.Contains(line, "error") || strings.Contains(line, "batch-") {
					t.Logf("cli: %s", strings.TrimSpace(line))
				}
			}

			titles := map[string]string{}
			for _, s := range tc.markers {
				got := blockByMarker(t, pool, category, marker(s))
				if len(got) != 1 {
					t.Errorf("section %q: %d live blocks carry its content, want 1 (blocks %+v)", s, len(got), got)
					continue
				}
				if got[0].vaultPath != tc.file {
					t.Errorf("section %q: vault_path = %q, want %q", s, got[0].vaultPath, tc.file)
				}
				if len(got[0].title) > maxTitleLen {
					t.Errorf("section %q: title is %d bytes, over the %d-byte limit", s, len(got[0].title), maxTitleLen)
				}
				if other, dup := titles[got[0].title]; dup {
					t.Errorf("sections %q and %q share title %q", other, s, got[0].title)
				}
				titles[got[0].title] = s
			}
			for title, s := range titles {
				t.Logf("block %q <- section %q", title, s)
			}
		})
	}
}
