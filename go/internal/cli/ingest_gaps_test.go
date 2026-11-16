package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/ingest"
)

// #319 made `ctx ingest` exit 1 when a file was not ingested or a chunk not
// written. These are the ways a run still lost files or a batch without that
// (#320): a directory the walk cannot read, a file in a reserved category and
// a --batch the server refuses.

// unreadableDir creates the directory rel in vault with a note in it and takes
// every permission away. It skips the test when this process still lists the
// directory: root (CAP_DAC_OVERRIDE) and Windows read a directory with mode
// 000. As root, run the compiled test binary in a user namespace without a
// mapping (`unshare -U`), where root has no override on the host's files.
func unreadableDir(t *testing.T, vault, rel string) {
	t.Helper()
	dir := filepath.Join(vault, filepath.FromSlash(rel))
	writeIngestFixture(t, vault, rel+"/inner.md", ingestNote("inner"))
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir, so it runs first and the removal can enter.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("this process lists a directory with mode 000 (root or Windows); see unreadableDir")
	}
}

// A directory the walk cannot read is reported like an unreadable file, the
// scan goes on, and the run exits non-zero: on upload, on --dry-run, and when
// no file was collected at all. Before #320 the walk callback dropped the
// error: the files under the directory were missing and the run exited 0.
func TestIngestReportsUnreadableDirectory(t *testing.T) {
	cases := []struct {
		name     string
		withGood bool
		args     []string
		want     []string
	}{
		{"upload", true, nil, []string{"Inserted: 1"}},
		{"dry_run", true, []string{"--dry-run"}, []string{"Dry run complete."}},
		{"no_file_collected", false, nil, []string{"No .md files found."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			if tc.withGood {
				writeIngestFixture(t, vault, "good.md", ingestNote("good"))
			}
			unreadableDir(t, vault, "notes/locked")
			f := &fakeIngest{}
			srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
			defer srv.Close()

			stderr, err := runIngestCmd(t, srv.URL, vault, append([]string{"--category", "walk"}, tc.args...)...)

			if err == nil {
				t.Errorf("run returned nil, want an error: the files under notes/locked were not ingested")
			}
			if tc.withGood && tc.args == nil && f.acceptedFrom("good.md") != 1 {
				t.Errorf("good.md was not written: the scan must go on past the unreadable directory")
			}
			wantInReport(t, stderr, append([]string{"notes/locked: "}, tc.want...)...)
		})
	}
}

// The walk callback with WalkDir's second call for a directory it could not
// read, synthesised, so the branch is covered where the chmod test skips
// (root, Windows): the error is recorded with the vault-relative path and the
// scan goes on. A hidden directory on its first call is skipped and recorded
// nowhere.
func TestMarkdownWalkRecordsWhatItCannotRead(t *testing.T) {
	vault := t.TempDir()
	writeIngestFixture(t, vault, "notes/a.md", ingestNote("a"))
	writeIngestFixture(t, vault, ".git/b.md", ingestNote("b"))
	entry := func(rel string) (string, fs.DirEntry) {
		p := filepath.Join(vault, filepath.FromSlash(rel))
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return p, fs.FileInfoToDirEntry(info)
	}
	w := &markdownWalk{root: vault}

	notes, notesEntry := entry("notes")
	if err := w.visit(notes, notesEntry, fs.ErrPermission); err != nil {
		t.Errorf("visit with a read error = %v, want nil: the scan goes on", err)
	}
	git, gitEntry := entry(".git")
	if err := w.visit(git, gitEntry, nil); !errors.Is(err, filepath.SkipDir) {
		t.Errorf("visit of .git = %v, want filepath.SkipDir", err)
	}
	file, fileEntry := entry("notes/a.md")
	if err := w.visit(file, fileEntry, nil); err != nil {
		t.Errorf("visit of a .md file = %v, want nil", err)
	}

	if len(w.unreadable) != 1 || w.unreadable[0].File != "notes" || !strings.Contains(w.unreadable[0].Error, fs.ErrPermission.Error()) {
		t.Errorf("unreadable = %+v, want one entry for notes carrying the error", w.unreadable)
	}
	if len(w.files) != 1 || w.files[0] != file {
		t.Errorf("files = %q, want [%q]", w.files, file)
	}
}

// A hidden directory is skipped on purpose before it is read, so an
// unreadable one is no error: the walk never lists it.
func TestIngestSkipsUnreadableHiddenDirectory(t *testing.T) {
	vault := t.TempDir()
	writeIngestFixture(t, vault, "good.md", ingestNote("good"))
	unreadableDir(t, vault, ".private")
	f := &fakeIngest{}
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	defer srv.Close()

	stderr, err := runIngestCmd(t, srv.URL, vault, "--category", "walk")

	if err != nil || f.acceptedFrom("good.md") != 1 {
		t.Errorf("err = %v, good.md written %d times; want nil and 1:\n%s", err, f.acceptedFrom("good.md"), stderr)
	}
	if strings.Contains(stderr, ".private") {
		t.Errorf("a skipped hidden directory is reported:\n%s", stderr)
	}
}

// The vault itself is walked whatever it is called and however it is reached:
// the hidden-directory rule is for directories inside the vault, and a vault
// path that is a symlink to a directory is followed. Before, both found no .md
// file and exited 0 with the whole vault missing.
func TestIngestWalksTheVaultRoot(t *testing.T) {
	cases := []struct {
		name  string
		vault func(t *testing.T) string
	}{
		{"hidden_name", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), ".notes")
		}},
		{"symlink", func(t *testing.T) string {
			base := t.TempDir()
			real := filepath.Join(base, "real")
			if err := os.Mkdir(real, 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(base, "link")
			if err := os.Symlink(real, link); err != nil {
				t.Skipf("symlink: %v", err)
			}
			return link
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := tc.vault(t)
			writeIngestFixture(t, vault, "good.md", ingestNote("good"))
			writeIngestFixture(t, vault, "sub/deep.md", ingestNote("deep"))
			f := &fakeIngest{}
			srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
			defer srv.Close()

			stderr, err := runIngestCmd(t, srv.URL, vault, "--category", "walk")

			if err != nil || f.acceptedFrom("good.md") != 1 || f.acceptedFrom("sub/deep.md") != 1 {
				t.Errorf("err = %v, good.md %d, sub/deep.md %d chunks written; want nil, 1, 1:\n%s",
					err, f.acceptedFrom("good.md"), f.acceptedFrom("sub/deep.md"), stderr)
			}
		})
	}
}

// A top-level folder named like a reserved category (derived.IsReservedCategory,
// any case) gives its files that category, and the server refuses a request
// with such a chunk whole with 403, the other files' chunks in it too. The CLI
// holds those files back and reports them like a chunk over a size limit.
func TestIngestHoldsBackFileInReservedCategory(t *testing.T) {
	for _, rel := range []string{"Catalog/note.md", "SESSION-INSIGHTS/deep/note.md"} {
		t.Run(rel, func(t *testing.T) {
			vault := t.TempDir()
			writeIngestFixture(t, vault, "good.md", ingestNote("good"))
			writeIngestFixture(t, vault, rel, ingestNote("reserved"))
			f := &fakeIngest{}
			srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
			defer srv.Close()

			stderr, err := runIngestCmd(t, srv.URL, vault)

			if n := f.acceptedFrom("good.md"); n != 1 {
				t.Errorf("good.md: %d chunks written, want 1 — the reserved file took its batch down", n)
			}
			if n := f.acceptedFrom(rel); n != 0 {
				t.Errorf("%s: %d chunks written, want 0", rel, n)
			}
			if err == nil {
				t.Errorf("run returned nil, want an error")
			}
			wantInReport(t, stderr, rel+": not uploaded", "reserved")
		})
	}
}

// --batch outside 1..MaxChunksPerRequest is refused as an argument before the
// vault is read. Above the limit every batch of a large vault was refused with
// 413; 0 and below silently became 50.
func TestIngestRefusesBatchOutsideServerLimit(t *testing.T) {
	over := ingest.MaxChunksPerRequest + 1
	vault := t.TempDir()
	for i := range over {
		writeIngestFixture(t, vault, fmt.Sprintf("n%03d.md", i), ingestNote(fmt.Sprintf("n%03d", i)))
	}
	for _, b := range []int{0, -1, over} {
		t.Run(strconv.Itoa(b), func(t *testing.T) {
			f := &fakeIngest{}
			srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
			defer srv.Close()

			_, err := runIngestCmd(t, srv.URL, vault, "--batch", strconv.Itoa(b))

			if err == nil || !strings.Contains(err.Error(), "--batch") {
				t.Errorf("err = %v, want a --batch argument error", err)
			}
			if f.requests != 0 {
				t.Errorf("%d requests sent, want 0", f.requests)
			}
		})
	}
	t.Run("at_the_limit", func(t *testing.T) {
		f := &fakeIngest{}
		srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
		defer srv.Close()

		stderr, err := runIngestCmd(t, srv.URL, vault, "--batch", strconv.Itoa(ingest.MaxChunksPerRequest))

		if err != nil || f.requests != 2 || len(f.accepted) != over {
			t.Errorf("err = %v, %d requests, %d chunks written; want nil, 2, %d:\n%s", err, f.requests, len(f.accepted), over, stderr)
		}
	})
}
