// Unit half of the A02-6 gate: the selection decisions that need no database.
// The ledger, dedup, dump and watermark halves live in
// distill_select_integration_test.go.
package events

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/safepath"
	"github.com/GottZ/ctx/internal/sensitivity"
)

func a6Item(block string, chunk int, text string) distillsource.Item {
	return distillsource.Item{
		Text:   text,
		Origin: distillsource.Origin{BlockID: block, ChunkIndex: chunk},
	}
}

// TestDistillParts pins the grouping the part scan stands on. The interesting
// case is not the ordinary part but the one the live corpus contains: the same
// part listed twice, which arrives as two runs of the same block id.
func TestDistillParts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []distillsource.Item
		want  []int // chunk counts per part
	}{
		{"one part", []distillsource.Item{a6Item("a", 1, "x"), a6Item("a", 2, "y")}, []int{2}},
		{"two parts", []distillsource.Item{a6Item("a", 1, "x"), a6Item("b", 1, "y")}, []int{1, 1}},
		{
			"the same part twice — two groups, not one doubled body",
			[]distillsource.Item{a6Item("a", 1, "x"), a6Item("a", 2, "y"), a6Item("a", 1, "x"), a6Item("a", 2, "y")},
			[]int{2, 2},
		},
		{"empty", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := distillParts(tc.items)
			if len(got) != len(tc.want) {
				t.Fatalf("parts = %d, want %d", len(got), len(tc.want))
			}
			for i, n := range tc.want {
				if len(got[i]) != n {
					t.Fatalf("part %d has %d chunks, want %d", i, len(got[i]), n)
				}
			}
		})
	}
}

// TestDistillSelectDropsAndCounts is §4.2.5 as a table: the part scan, the
// chunk scan and the substance floor, each with its counter.
func TestDistillSelectDropsAndCounts(t *testing.T) {
	long := strings.Repeat("substanz ", 40) // 360 runes

	t.Run("a credential drops the WHOLE part", func(t *testing.T) {
		items := []distillsource.Item{
			a6Item("a", 1, long),
			a6Item("a", 2, long+" AKIAIOSFODNN7EXAMPLE"),
			a6Item("b", 1, long),
		}
		kept, l := distillSelect(items, 200)
		if l.seen != 3 || l.droppedCred != 2 || len(kept) != 1 {
			t.Fatalf("seen/cred/kept = %d/%d/%d, want 3/2/1", l.seen, l.droppedCred, len(kept))
		}
		if kept[0].Origin.BlockID != "b" {
			t.Fatalf("kept %q, want the clean part b", kept[0].Origin.BlockID)
		}
	})

	// The stage-(a) property, and the reason the arm reassembles at all: a
	// 64-hex run cut by a chunk boundary is under the detector's length gate in
	// BOTH halves. The red state is asserted in the same test, on the chunks
	// themselves, so the contrast needs no second build.
	t.Run("a secret split across a chunk boundary still drops the part", func(t *testing.T) {
		secret := strings.Repeat("a1b2c3d4", 8) // 64 hex characters
		head, tail := secret[:30], secret[30:]
		items := []distillsource.Item{
			a6Item("a", 1, long+" "+head),
			a6Item("a", 2, tail+" "+long),
		}
		for _, it := range items {
			if _, hit := sensitivity.Scan(it.Text); hit {
				t.Fatalf("RED precondition broken: the chunk alone already flags — %q", it.Text[:20])
			}
		}
		kept, l := distillSelect(items, 200)
		if l.droppedCred != 2 || len(kept) != 0 {
			t.Fatalf("cred/kept = %d/%d, want 2/0 — the part scan missed a seam secret", l.droppedCred, len(kept))
		}
	})

	t.Run("the substance floor drops without a counter of its own", func(t *testing.T) {
		items := []distillsource.Item{a6Item("a", 1, "ok"), a6Item("b", 1, long)}
		kept, l := distillSelect(items, 200)
		if l.seen != 2 || l.droppedCred != 0 || len(kept) != 1 {
			t.Fatalf("seen/cred/kept = %d/%d/%d, want 2/0/1", l.seen, l.droppedCred, len(kept))
		}
	})
}

// TestDistillNormalizeAndHash pins the dedup comparison form: NFC and a
// whitespace collapse, and NOT case folding.
func TestDistillNormalizeAndHash(t *testing.T) {
	if a, b := distillNormalize("a  \n\tb "), "a b"; a != b {
		t.Fatalf("collapse = %q, want %q", a, b)
	}
	// U+00C5 vs U+0041 U+030A — the same text in two normalizations.
	if distillNormalize("Å") != distillNormalize("Å") {
		t.Fatal("NFC did not fold the two spellings of Å")
	}
	if hex.EncodeToString(distillRowHash("a b")) != hex.EncodeToString(distillRowHash("a\n\nb")) {
		t.Fatal("whitespace difference produced two hashes")
	}
	// Case folding would merge material that differs by a rename. A dedup key
	// must not err in the direction that DROPS material.
	if hex.EncodeToString(distillRowHash("Token")) == hex.EncodeToString(distillRowHash("token")) {
		t.Fatal("the dedup form case-folds — two distinct chunks would collapse")
	}
	if n := len(distillRowHash("x")); n != 32 {
		t.Fatalf("hash is %d bytes, want 32 (SHA-256, 135:210)", n)
	}
}

// a6DumpRoot is a fixture root that is guaranteed to lie OUTSIDE any git
// working copy — which t.TempDir() is not.
//
// t.TempDir() inherits TMPDIR, and `go test` passes GOTMPDIR through as the
// test binary's TMPDIR. Under this project's documented GOTMPDIR
// (/compose/n8n/.gotmp, inside the repository) every t.TempDir() sits in a
// working copy, so the BA13 positive cases and every dump target of the gate
// suite failed for a reason that has nothing to do with the code under test
// (review #5). The root is therefore chosen here rather than inherited.
func a6DumpRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "ctx-a02-6-")
	if err != nil {
		t.Fatalf("dump fixture root: %v", err)
	}
	// Resolved, because distillDumpDir answers a resolved path and the table
	// below compares against this value.
	if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
		dir = resolved
	}
	if root, inTree := distillGitWorkTree(dir); inTree {
		t.Fatalf("fixture root %q lies in the working copy %q — the probe needs one that does not", dir, root)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestDistillDumpDir is the BA13 gate as a decision table. The git probe is
// built against a REAL .git entry rather than a name check, and three shapes
// are covered: the directory of an ordinary clone, the FILE a submodule or a
// linked worktree carries — the .project case BA13 was written about — and a
// SYMLINK that points into either of them (review #1).
func TestDistillDumpDir(t *testing.T) {
	base := a6DumpRoot(t)
	clone := filepath.Join(base, "clone")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(base, "submodule")
	if err := os.MkdirAll(filepath.Join(sub, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../.git/modules/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// THE SYMLINK CASE (review #1). `outside` lies outside every working copy
	// by its lexical path and points INTO the submodule — a purely lexical
	// ancestor walk accepts it and the plaintext dump lands in the working
	// copy, because MkdirAll/OpenFile follow the link all the way.
	plain := filepath.Join(base, "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "outside")
	if err := os.Symlink(filepath.Join(sub, "inner"), link); err != nil {
		t.Fatal(err)
	}
	linkOK := filepath.Join(base, "outside-clean")
	if err := os.Symlink(plain, linkOK); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "empty turns the dump off", in: "", want: ""},
		{name: "blank turns the dump off", in: "   ", want: ""},
		{name: "an absolute path outside a working copy passes", in: base, want: base},
		{name: "trailing slash is cleaned", in: base + "/", want: base},
		{name: "relative is refused", in: "distill-dryrun", wantErr: true},
		{name: "inside a clone is refused", in: filepath.Join(clone, "dump"), wantErr: true},
		{name: "inside a submodule is refused", in: filepath.Join(sub, "deep", "dump"), wantErr: true},
		{name: "the clone root itself is refused", in: clone, wantErr: true},
		{name: "a symlink INTO a working copy is refused", in: link, wantErr: true},
		{name: "a path UNDER such a symlink is refused", in: filepath.Join(link, "deep"), wantErr: true},
		// The control: a symlink that resolves outside stays legal, so the
		// refusal above is about the TARGET and not about links as such. The
		// answer is the RESOLVED path — the same one MkdirAll would write to.
		{name: "a symlink outside a working copy passes, resolved", in: linkOK, want: plain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := distillDumpDir(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("distillDumpDir(%q) = %q, want a refusal", tc.in, got)
				}
				if got != "" {
					t.Fatalf("a refused target still answered a path: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("distillDumpDir(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("distillDumpDir(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDistillDumpSealsItsDirectory is the 0700 half of gate 7, and it probes
// the harder of the two states: an EXISTING directory that is world-readable.
// MkdirAll would leave such a directory untouched, and a permissive umask makes
// it the ordinary outcome of creating one — either way the dump would sit in
// 0755 with raw session prose in it.
func TestDistillDumpSealsItsDirectory(t *testing.T) {
	dir := filepath.Join(a6DumpRoot(t), "dryrun")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { // MkdirAll applied the umask
		t.Fatal(err)
	}
	d, err := distillOpenDump(dir, "0198f9d2-0000-7000-8000-00000000abcd")
	if err != nil {
		t.Fatalf("open dump: %v", err)
	}
	if err := d.write([]distillsource.Item{a6Item("a", 1, "text")}, [][]byte{{1, 2, 3}}); err != nil {
		t.Fatalf("write dump: %v", err)
	}
	d.close()

	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != safepath.DirMode {
		t.Fatalf("dump directory mode = %v, want %v", got, safepath.DirMode)
	}
	fi, err := os.Stat(filepath.Join(dir, "0198f9d2-0000-7000-8000-00000000abcd.ndjson"))
	if err != nil {
		t.Fatalf("dump file: %v", err)
	}
	if got := fi.Mode().Perm(); got != safepath.FileMode {
		t.Fatalf("dump file mode = %v, want %v", got, safepath.FileMode)
	}
}

// TestDistillDumpRefusesAForeignName closes the file-name half of BA13: a
// source key carries corpus text verbatim, so nothing but a code-owned id may
// reach a path.
func TestDistillDumpRefusesAForeignName(t *testing.T) {
	dir := a6DumpRoot(t)
	for _, id := range []string{"", "../escape", "ctx-checkpoint:private:root", "a/b"} {
		if d, err := distillOpenDump(dir, id); err == nil {
			d.close()
			t.Fatalf("distillOpenDump accepted %q as a file name", id)
		}
	}
	if d, err := distillOpenDump("", "irrelevant"); err != nil || d != nil {
		t.Fatalf("an empty dir must yield a nil dump without an error, got %v / %v", d, err)
	}
	// A nil dump is the "off" state and must stay callable.
	var off *distillDump
	if err := off.write([]distillsource.Item{a6Item("a", 1, "x")}, [][]byte{{1}}); err != nil {
		t.Fatalf("nil dump write: %v", err)
	}
	off.close()
}

// TestDistillSizingIsTheValidatorsAuthority replaces the rune-cap clamp this
// wave first carried (review #4). The clamp absorbed exactly the value
// config.validateDistillCounters refuses with SeverityError — two authorities
// for one question, and the absorbing one carried the opposite policy to the
// one the validator writes out ("the sizing keys have NO safe zero … a silent
// second off-switch that the settings surface renders as a configured size",
// validate.go:409-414).
//
// The clamp is gone; this probe BINDS the remaining authority, so removing the
// validator rule goes red HERE instead of silently re-creating the clamp's
// semantics. The arm-side half — a non-positive cap makes the arm process
// NOTHING rather than quietly read at 4000 — is the integration probe
// TestDistillSelection/ANonPositiveRuneCapProcessesNothing.
func TestDistillSizingIsTheValidatorsAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
		field  string
	}{
		{"max_row_runes 0", func(c *config.Config) { c.Distill.MaxRowRunes = 0 }, "distill.max_row_runes"},
		{"rows_per_read 0", func(c *config.Config) { c.Distill.RowsPerRead = 0 }, "distill.rows_per_read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := config.Defaults()
			tc.mutate(c)
			issues := config.Validate(c)
			var seen bool
			for _, i := range issues {
				if i.Field == tc.field && i.Severity == config.SeverityError {
					seen = true
				}
			}
			if !seen {
				t.Fatalf("no SeverityError on %s — the arm has no second guard for it: %+v", tc.field, issues)
			}
		})
	}
}
