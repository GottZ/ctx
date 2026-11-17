package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// #324: the scan walked the vault with filepath.WalkDir, which does not follow
// a symbolic link, so a linked folder (an Obsidian vault sharing a folder with
// another one) was visited as one entry: its notes were missing from the run,
// nothing was reported and the run exited 0. These tests build the links and
// run the command against a fake server.

// symlinkOrSkip links at to target; a system that refuses to create a symlink
// (Windows without the privilege) skips the test.
func symlinkOrSkip(t *testing.T, target, at string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Skipf("symlink: %v", err)
	}
}

// linkVault is base/vault with good.md in it; base is the place for targets
// outside the vault.
func linkVault(t *testing.T) (base, vault string) {
	t.Helper()
	// Resolved, so the paths the report names start with base (macOS keeps
	// the temporary directory behind a link).
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault = filepath.Join(base, "vault")
	writeIngestFixture(t, vault, "good.md", ingestNote("good"))
	return base, vault
}

// ingestedPaths lists the vault_path of every accepted chunk, sorted.
func (f *fakeIngest) ingestedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.accepted {
		p, _ := c.Metadata["vault_path"].(string)
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// chunkOf is the accepted chunk of the file at vault path rel.
func (f *fakeIngest) chunkOf(t *testing.T, rel string) ingestChunkPayload {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.accepted {
		if p, _ := c.Metadata["vault_path"].(string); p == rel {
			return c
		}
	}
	t.Fatalf("no chunk of %s was written", rel)
	return ingestChunkPayload{}
}

// runLinkVault runs the command on vault, upload and --dry-run, and checks
// what both report and that both exit as wantErr says; it returns the upload's
// fake server and stderr.
func runLinkVault(t *testing.T, base, vault string, wantErr bool, want ...string) (*fakeIngest, string) {
	t.Helper()
	var upload *fakeIngest
	var uploadOut string
	for _, dry := range []bool{false, true} {
		f := &fakeIngest{}
		srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
		var args []string
		if dry {
			args = []string{"--dry-run"}
		}
		stderr, err := runIngestCmd(t, srv.URL, vault, args...)
		srv.Close()
		stderr = strings.ReplaceAll(stderr, base, "<base>")
		if (err != nil) != wantErr {
			t.Errorf("dry run %v: err = %v, want error: %v:\n%s", dry, err, wantErr, stderr)
		}
		wantInReport(t, stderr, want...)
		if !dry {
			upload, uploadOut = f, stderr
		}
	}
	return upload, uploadOut
}

// A linked folder is walked like a folder of the vault, and its notes keep
// the path through the link: vault_path, title and the category from the
// first folder. The target's own name may start with a dot, as the vault's
// may; a link named like a note that leads to a folder is a folder, as a
// folder of that name is (before, it was read as a file and failed). A link
// followed out of the vault is named in the summary.
func TestIngestFollowsLinkedFolder(t *testing.T) {
	cases := []struct{ target, link string }{
		{"outside", "shared"},
		{".other-vault", "shared"},
		{"outside", "shared.md"},
	}
	for _, tc := range cases {
		t.Run(tc.target+"_as_"+tc.link, func(t *testing.T) {
			base, vault := linkVault(t)
			writeIngestFixture(t, base, tc.target+"/s.md", ingestNote("s"))
			writeIngestFixture(t, base, tc.target+"/sub/t.md", ingestNote("t"))
			symlinkOrSkip(t, filepath.Join(base, filepath.FromSlash(tc.target)), filepath.Join(vault, tc.link))

			f, stderr := runLinkVault(t, base, vault, false,
				"Followed out of the vault (1):", tc.link+" -> <base>/"+tc.target)

			deep := tc.link + "/sub/t.md"
			if got, want := f.ingestedPaths(), []string{"good.md", tc.link + "/s.md", deep}; !slices.Equal(got, want) {
				t.Fatalf("ingested %q, want %q:\n%s", got, want, stderr)
			}
			if c := f.chunkOf(t, deep); c.Title != tc.link+"/sub/t" || c.Category != tc.link {
				t.Errorf("%s: title %q, category %q; want %s/sub/t, %s", deep, c.Title, c.Category, tc.link, tc.link)
			}
			if strings.Contains(stderr, "Not followed") || strings.Contains(stderr, "Errors") {
				t.Errorf("a followed link is reported:\n%s", stderr)
			}
		})
	}
}

// Every real folder is walked once. A folder reached without a link wins over
// a link to it, even a link whose name sorts first; between links, the one
// with fewer links on its way, then the one whose path sorts first in byte
// order, which is not the order of the walk (a-b sorts before a/l1, which the
// walk meets first). Every other way is named with its target and the way the
// notes were ingested through, and the run still exits 0.
func TestIngestWalksEachFolderOnce(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, base, vault string)
		paths   []string // vault_path of every chunk written
		want    []string // on stderr, base replaced by <base>
	}{
		{"two_links_to_one_folder", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "outside/x.md", ingestNote("x"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "t2"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "t1"))
		}, []string{"good.md", "t1/x.md"}, []string{"Not followed (1)", "t2 -> <base>/outside: already ingested through t1"}},
		{"tie_by_path_not_by_walk", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "outside/x.md", ingestNote("x"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "a/l1"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "a-b"))
		}, []string{"a-b/x.md", "good.md"}, []string{"a/l1 -> <base>/outside: already ingested through a-b"}},
		{"link_to_a_folder_of_the_vault", func(t *testing.T, _, vault string) {
			writeIngestFixture(t, vault, "notes/a.md", ingestNote("a"))
			symlinkOrSkip(t, filepath.Join(vault, "notes"), filepath.Join(vault, "aa"))
		}, []string{"good.md", "notes/a.md"}, []string{"aa -> <base>/vault/notes: already ingested through notes"}},
		{"fewer_links_win", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "outside/sub/x.md", ingestNote("x"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "b"))
			symlinkOrSkip(t, filepath.Join(base, "hop"), filepath.Join(vault, "a"))
			symlinkOrSkip(t, filepath.Join(base, "outside/sub"), filepath.Join(base, "hop/sub"))
		}, []string{"b/sub/x.md", "good.md"}, []string{"a/sub -> <base>/outside/sub: already ingested through b/sub"}},
		{"overlapping_links", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "outside/s.md", ingestNote("s"))
			writeIngestFixture(t, base, "outside/sub/x.md", ingestNote("x"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "shared"))
			symlinkOrSkip(t, filepath.Join(base, "outside/sub"), filepath.Join(vault, "deep"))
		}, []string{"deep/x.md", "good.md", "shared/s.md"}, []string{"shared/sub -> <base>/outside/sub: already ingested through deep"}},
		{"link_to_the_vault", func(t *testing.T, _, vault string) {
			writeIngestFixture(t, vault, "notes/a.md", ingestNote("a"))
			symlinkOrSkip(t, vault, filepath.Join(vault, "notes/up"))
		}, []string{"good.md", "notes/a.md"}, []string{"notes/up -> <base>/vault: the vault or a folder that holds it"}},
		{"link_above_the_vault", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "elsewhere/e.md", ingestNote("e"))
			symlinkOrSkip(t, base, filepath.Join(vault, "home"))
		}, []string{"good.md"}, []string{"home -> <base>: the vault or a folder that holds it"}},
		{"cycle_behind_a_link", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "outside/s.md", ingestNote("s"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(base, "outside/again"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "shared"))
		}, []string{"good.md", "shared/s.md"}, []string{"shared/again -> <base>/outside: already ingested through shared"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, vault := linkVault(t)
			tc.prepare(t, base, vault)

			f, stderr := runLinkVault(t, base, vault, false, tc.want...)

			if got := f.ingestedPaths(); !slices.Equal(got, tc.paths) {
				t.Errorf("ingested %q, want %q:\n%s", got, tc.paths, stderr)
			}
		})
	}
}

// A link to a folder inside the vault is followed like any other, also into a
// folder the walk skips by name; only links out of the vault are listed as
// such.
func TestIngestNamesOnlyLinksOutOfTheVault(t *testing.T) {
	base, vault := linkVault(t)
	writeIngestFixture(t, vault, ".private/p.md", ingestNote("p"))
	symlinkOrSkip(t, filepath.Join(vault, ".private"), filepath.Join(vault, "pub"))

	f, stderr := runLinkVault(t, base, vault, false)

	if got, want := f.ingestedPaths(), []string{"good.md", "pub/p.md"}; !slices.Equal(got, want) {
		t.Errorf("ingested %q, want %q:\n%s", got, want, stderr)
	}
	if strings.Contains(stderr, "Followed out of the vault") {
		t.Errorf("a link inside the vault is listed as one out of it:\n%s", stderr)
	}
}

// A scan that finds no note still names the ways it did not take.
func TestIngestNamesWaysNotTakenWithoutNotes(t *testing.T) {
	base, vault := linkVault(t)
	if err := os.Remove(filepath.Join(vault, "good.md")); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, vault, filepath.Join(vault, "up"))

	runLinkVault(t, base, vault, false, "No .md files found.", "Not followed (1)", "up -> <base>/vault: the vault or a folder that holds it")
}

// The hidden-folder rule holds behind a link: a link named with a leading dot
// or node_modules is not followed, and such folders under the target are
// skipped. Neither is reported, as a skipped folder of the vault is not.
func TestIngestSkipsHiddenNamesBehindALink(t *testing.T) {
	base, vault := linkVault(t)
	writeIngestFixture(t, base, "outside/s.md", ingestNote("s"))
	writeIngestFixture(t, base, "outside/.obsidian/o.md", ingestNote("o"))
	writeIngestFixture(t, base, "outside/node_modules/m.md", ingestNote("m"))
	writeIngestFixture(t, base, "hidden/h.md", ingestNote("h"))
	symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "shared"))
	symlinkOrSkip(t, filepath.Join(base, "hidden"), filepath.Join(vault, ".hidden"))
	symlinkOrSkip(t, filepath.Join(base, "hidden"), filepath.Join(vault, "node_modules"))

	f, stderr := runLinkVault(t, base, vault, false)

	if got, want := f.ingestedPaths(), []string{"good.md", "shared/s.md"}; !slices.Equal(got, want) {
		t.Errorf("ingested %q, want %q:\n%s", got, want, stderr)
	}
	if strings.Contains(stderr, "Not followed") || strings.Contains(stderr, "Errors") {
		t.Errorf("a skipped folder is reported:\n%s", stderr)
	}
}

// A link the scan cannot follow is reported like an unreadable folder and the
// run exits 1, on upload and on --dry-run: notes under it may be missing. A
// dead link whose name carries another extension than .md is taken for a
// missing attachment, which ingest would not read either: it does not change
// the exit status, and all such links share one line of the ways not taken,
// so a folder link named like v2.0 that leads nowhere is still named. A dead
// link named .md is reported as before.
func TestIngestReportsLinkItCannotFollow(t *testing.T) {
	cases := []struct {
		name    string
		link    string
		wantErr bool
		want    []string
	}{
		{"dead_folder_link", "Shared", true, []string{"Shared: could not be followed, files under it may be missing: ", "no such file or directory"}},
		{"dead_note_link", "gone.md", true, []string{"gone.md: open <base>/vault/gone.md: no such file or directory"}},
		{"dead_attachment_link", "img.png", false, []string{"Not followed (1)", "img.png: leads nowhere, taken for a missing attachment"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, vault := linkVault(t)
			symlinkOrSkip(t, filepath.Join(base, "gone"), filepath.Join(vault, tc.link))

			f, stderr := runLinkVault(t, base, vault, tc.wantErr, tc.want...)

			if n := f.acceptedFrom("good.md"); n != 1 {
				t.Errorf("good.md written %d times, want 1: the scan goes on past the link:\n%s", n, stderr)
			}
		})
	}
	t.Run("dead_attachment_links", func(t *testing.T) {
		base, vault := linkVault(t)
		symlinkOrSkip(t, filepath.Join(base, "gone.png"), filepath.Join(vault, "a.png"))
		symlinkOrSkip(t, filepath.Join(base, "gone"), filepath.Join(vault, "v2.0"))

		runLinkVault(t, base, vault, false, "Not followed (1)", "a.png and 1 more: lead nowhere, taken for missing attachments")
	})
	// A linked folder the process may enter but not list gives no files and
	// is reported; a link to a note in it still reads the note through the
	// link, like any link to a note.
	t.Run("unlistable_target_note_link", func(t *testing.T) {
		base, vault := linkVault(t)
		dir := filepath.Join(base, "ext/locked")
		writeIngestFixture(t, base, "ext/locked/n.md", ingestNote("n"))
		symlinkOrSkip(t, dir, filepath.Join(vault, "l"))
		symlinkOrSkip(t, filepath.Join(dir, "n.md"), filepath.Join(vault, "n.md"))
		if err := os.Chmod(dir, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if _, err := os.ReadDir(dir); err == nil {
			t.Skip("this process lists a directory with mode 0311 (root or Windows); see unreadableDir")
		}

		f, stderr := runLinkVault(t, base, vault, true, "l: could not be read, files under it may be missing: ")

		if n := f.acceptedFrom("n.md"); n != 1 {
			t.Errorf("n.md written %d times, want 1 through its link:\n%s", n, stderr)
		}
		if strings.Contains(stderr, "Not followed") {
			t.Errorf("a link to a note is reported as a way not taken:\n%s", stderr)
		}
	})
	// A link to a folder the process may not read: its notes are missing.
	t.Run("unreadable_target", func(t *testing.T) {
		base, vault := linkVault(t)
		unreadableDir(t, base, "locked")
		symlinkOrSkip(t, filepath.Join(base, "locked"), filepath.Join(vault, "shared"))

		runLinkVault(t, base, vault, true, "shared: could not be read, files under it may be missing: ")
	})
}

// A link to a note is ingested under the link's path, as before #324: title,
// category and vault_path come from where the link sits in the vault, not from
// the target. That holds also when the note is reachable another way, directly
// (a link before or after its folder in the walk), in a followed folder or
// through a second link: every way is ingested, none is reported. Only folders
// are walked once.
func TestIngestNoteLinks(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, base, vault string)
		paths   []string
	}{
		{"link_out_of_the_vault", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "outside/f.md", ingestNote("f"))
			symlinkOrSkip(t, filepath.Join(base, "outside/f.md"), filepath.Join(vault, "refs/alias.md"))
		}, []string{"good.md", "refs/alias.md"}},
		{"links_and_the_note", func(t *testing.T, _, vault string) {
			writeIngestFixture(t, vault, "notes/a.md", ingestNote("a"))
			symlinkOrSkip(t, filepath.Join(vault, "notes/a.md"), filepath.Join(vault, "a-link.md"))
			symlinkOrSkip(t, filepath.Join(vault, "notes/a.md"), filepath.Join(vault, "z-link.md"))
		}, []string{"a-link.md", "good.md", "notes/a.md", "z-link.md"}},
		{"link_into_a_followed_folder", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "outside/s.md", ingestNote("s"))
			symlinkOrSkip(t, filepath.Join(base, "outside/s.md"), filepath.Join(vault, "a.md"))
			symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(vault, "shared"))
		}, []string{"a.md", "good.md", "shared/s.md"}},
		{"two_links_to_one_note", func(t *testing.T, base, vault string) {
			writeIngestFixture(t, base, "outside/f.md", ingestNote("f"))
			symlinkOrSkip(t, filepath.Join(base, "outside/f.md"), filepath.Join(vault, "x2.md"))
			symlinkOrSkip(t, filepath.Join(base, "outside/f.md"), filepath.Join(vault, "x1.md"))
		}, []string{"good.md", "x1.md", "x2.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, vault := linkVault(t)
			tc.prepare(t, base, vault)

			f, stderr := runLinkVault(t, base, vault, false)

			if got := f.ingestedPaths(); !slices.Equal(got, tc.paths) {
				t.Errorf("ingested %q, want %q:\n%s", got, tc.paths, stderr)
			}
			if strings.Contains(stderr, "Not followed") {
				t.Errorf("a link to a note is reported as a way not taken:\n%s", stderr)
			}
			if tc.name == "link_out_of_the_vault" {
				if c := f.chunkOf(t, "refs/alias.md"); c.Title != "refs/alias" || c.Category != "refs" {
					t.Errorf("refs/alias.md: title %q, category %q; want refs/alias, refs", c.Title, c.Category)
				}
			}
		})
	}
}
