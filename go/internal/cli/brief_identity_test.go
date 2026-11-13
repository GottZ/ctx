package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The tests in this file pin the project identity of `ctx brief` / `ctx persist`
// (#146) against layouts built by real git, one per acceptance criterion:
// ordinary linked worktrees, bare repositories, submodules and
// --separate-git-dir. Hand-written .git files appear only where a layout is
// deliberately broken.

// identityBase returns a fresh fixture directory with symlinks resolved: git
// writes resolved absolute paths into gitdir files, so the expectations must
// be spelled the same way. GIT_CEILING_DIRECTORIES pins git's own repository
// discovery below the base, so no fixture command can reach a repository the
// temp directory happens to sit in.
func identityBase(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; these fixtures are built by real git")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	return base
}

// identityGit runs git in dir for a throwaway fixture repository below
// identityBase. The host's global and system git configuration are switched
// off, so a fixture neither inherits commit signing nor depends on a
// configured identity; the commits of these temp repositories carry a fixed
// fixture identity on the command line. GIT_* variables of the calling
// environment (GIT_DIR from a hook, for example) are dropped, except the
// ceiling identityBase sets.
func identityGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{
		"-c", "user.name=ctx fixture",
		"-c", "user.email=fixture@example.invalid",
		"-c", "init.defaultBranch=main",
		"-c", "protocol.file.allow=always",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "GIT_CEILING_DIRECTORIES=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// identityRepo creates dir (and parents) as a repository with one commit.
func identityRepo(t *testing.T, dir string) {
	t.Helper()
	identityMkdir(t, dir)
	identityGit(t, dir, "init", "-q")
	identityGit(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
}

func identityMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// expectIdentity asserts resolveProject for every dir.
func expectIdentity(t *testing.T, want string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if got := resolveProject(d); got != want {
			t.Errorf("resolveProject(%s) = %s, want %s", d, got, want)
		}
	}
}

// legacyResolveProject is resolveProject as it stood before #146, verbatim:
// the nearest .git DIRECTORY at or above dir, else dir itself.
func legacyResolveProject(dir string) string {
	d := dir
	for {
		if info, err := os.Stat(filepath.Join(d, ".git")); err == nil && info.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return dir
}

// TestResolveProjectGitDirectoryIdentityUnchanged pins that every layout the
// old resolver already answered through a .git DIRECTORY keeps its identity
// byte for byte — the tags existing briefings were written against.
func TestResolveProjectGitDirectoryIdentityUnchanged(t *testing.T) {
	base := identityBase(t)
	main := filepath.Join(base, "main")
	identityRepo(t, main)
	deep := filepath.Join(main, "go", "internal", "cli")
	identityMkdir(t, deep)
	// An old-style nested repository with its own .git directory (the
	// pre-1.7.8 submodule shape, or a vendored clone).
	nested := filepath.Join(main, "vendor", "lib")
	identityRepo(t, nested)
	nestedDeep := filepath.Join(nested, "src")
	identityMkdir(t, nestedDeep)
	plain := filepath.Join(base, "plain", "dir")
	identityMkdir(t, plain)

	cases := []struct{ dir, want string }{
		{main, main},
		{deep, main},
		{nested, nested},
		{nestedDeep, nested},
		{plain, legacyResolveProject(plain)},
		{"relative/path", "relative/path"},
	}
	for _, c := range cases {
		got := resolveProject(c.dir)
		if got != c.want {
			t.Errorf("resolveProject(%s) = %q, want %q", c.dir, got, c.want)
		}
		if legacy := legacyResolveProject(c.dir); got != legacy {
			t.Errorf("resolveProject(%s) = %q, pre-#146 resolver = %q: a .git-directory layout changed identity", c.dir, got, legacy)
		}
	}
}

// TestResolveProjectLinkedWorktreeFollowsCommondir — criterion 1: an ordinary
// linked worktree follows gitdir and commondir to the main checkout, wherever
// the worktree sits.
func TestResolveProjectLinkedWorktreeFollowsCommondir(t *testing.T) {
	base := identityBase(t)
	main := filepath.Join(base, "main")
	identityRepo(t, main)
	identityGit(t, main, "worktree", "add", "-q", "../main-feat", "-b", "feat")
	identityGit(t, main, "worktree", "add", "-q", ".wt/inner", "-b", "inner")
	sibling := filepath.Join(base, "main-feat")
	siblingDeep := filepath.Join(sibling, "go", "internal")
	identityMkdir(t, siblingDeep)

	expectIdentity(t, main, main, sibling, siblingDeep, filepath.Join(main, ".wt", "inner"))

	// A worktree placed inside an unrelated repository belongs to its own
	// repository, not to the one whose .git directory the walk would reach.
	host := filepath.Join(base, "host")
	identityRepo(t, host)
	identityGit(t, main, "worktree", "add", "-q", "../host/vendor-wt", "-b", "vendored")
	expectIdentity(t, main, filepath.Join(host, "vendor-wt"))
	expectIdentity(t, host, host)
}

// TestResolveProjectBareRepositoryKeepsItsOwnIdentity — criterion 2: a bare
// repository's commondir is the bare directory itself; its parent is shared by
// sibling repositories and is never the identity.
func TestResolveProjectBareRepositoryKeepsItsOwnIdentity(t *testing.T) {
	base := identityBase(t)
	src := filepath.Join(base, "src")
	identityRepo(t, src)
	repos := filepath.Join(base, "repos")
	identityMkdir(t, repos)
	identityGit(t, base, "clone", "-q", "--bare", src, filepath.Join(repos, "a.git"))
	identityGit(t, base, "clone", "-q", "--bare", src, filepath.Join(repos, "b.git"))
	aGit, bGit := filepath.Join(repos, "a.git"), filepath.Join(repos, "b.git")
	identityGit(t, aGit, "worktree", "add", "-q", filepath.Join(base, "a-wt"), "-b", "wa")
	identityGit(t, bGit, "worktree", "add", "-q", filepath.Join(base, "b-wt"), "-b", "wb")
	identityGit(t, aGit, "worktree", "add", "-q", filepath.Join(repos, "a-inside"), "-b", "wi")

	expectIdentity(t, aGit, filepath.Join(base, "a-wt"), filepath.Join(repos, "a-inside"))
	expectIdentity(t, bGit, filepath.Join(base, "b-wt"))
	for _, d := range []string{filepath.Join(base, "a-wt"), filepath.Join(base, "b-wt"), filepath.Join(repos, "a-inside")} {
		if got := resolveProject(d); got == repos {
			t.Errorf("resolveProject(%s) collapsed onto the shared parent %s", d, repos)
		}
	}

	// The container layout: proj/.bare holds the bare repository, proj/.git is
	// a file pointing at it, worktrees sit next to it. The container and every
	// worktree share the bare directory as identity.
	proj := filepath.Join(base, "proj")
	identityMkdir(t, proj)
	identityGit(t, base, "clone", "-q", "--bare", src, filepath.Join(proj, ".bare"))
	if err := os.WriteFile(filepath.Join(proj, ".git"), []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		t.Fatalf("write container .git file: %v", err)
	}
	identityGit(t, proj, "worktree", "add", "-q", "main", "-b", "m")
	expectIdentity(t, filepath.Join(proj, ".bare"), proj, filepath.Join(proj, "main"))

	// Documented case: a bare repository has no main checkout to name. A
	// working directory inside the bare directory itself carries no .git entry
	// and keeps the pre-#146 resolution.
	expectIdentity(t, legacyResolveProject(aGit), aGit)
}

// TestResolveProjectSubmoduleKeepsItsOwnIdentity — criterion 3: a submodule
// resolves to its own checkout (core.worktree of its git directory), never to
// the superproject, and so do the submodule's linked worktrees.
func TestResolveProjectSubmoduleKeepsItsOwnIdentity(t *testing.T) {
	base := identityBase(t)
	subSrc := filepath.Join(base, "subsrc")
	identityRepo(t, subSrc)
	super := filepath.Join(base, "super")
	identityRepo(t, super)
	identityGit(t, super, "submodule", "-q", "add", subSrc, "sub")
	identityGit(t, super, "commit", "-q", "-m", "add sub")
	sub := filepath.Join(super, "sub")
	subDeep := filepath.Join(sub, "pkg")
	identityMkdir(t, subDeep)
	identityGit(t, sub, "worktree", "add", "-q", filepath.Join(base, "sub-wt"), "-b", "swt")
	subWT := filepath.Join(base, "sub-wt")

	expectIdentity(t, sub, sub, subDeep, subWT)
	expectIdentity(t, super, super)

	// Documented case: a submodule inside a linked worktree of the superproject
	// is a separate clone (its git directory lives under the superproject
	// worktree's admin directory); its identity is that checkout — distinct
	// from the superproject, not the same as the main checkout's submodule.
	identityGit(t, super, "worktree", "add", "-q", "../super-wt", "-b", "swt2")
	superWT := filepath.Join(base, "super-wt")
	identityGit(t, superWT, "submodule", "-q", "update", "--init")
	expectIdentity(t, super, superWT)
	expectIdentity(t, filepath.Join(superWT, "sub"), filepath.Join(superWT, "sub"))

	// git sparse-checkout enables extensions.worktreeConfig and moves
	// core.worktree into config.worktree (git 2.36+); the identity must not
	// move with it. Last, because an older git skips here.
	identityGit(t, sub, "sparse-checkout", "init", "--cone")
	moved, err := os.ReadFile(filepath.Join(super, ".git", "modules", "sub", "config.worktree"))
	if err != nil || !strings.Contains(string(moved), "worktree =") {
		t.Skipf("this git does not move core.worktree into config.worktree (err=%v):\n%s", err, moved)
	}
	expectIdentity(t, sub, sub, subDeep, subWT)
}

// TestResolveProjectSeparateGitDirSharesOneIdentity — criterion 4: a
// --separate-git-dir checkout and its linked worktrees share one stable
// identity: the separate git directory.
func TestResolveProjectSeparateGitDirSharesOneIdentity(t *testing.T) {
	base := identityBase(t)
	store := filepath.Join(base, "store")
	identityMkdir(t, store)
	sepGit := filepath.Join(store, "sep.git")
	identityGit(t, base, "init", "-q", "--separate-git-dir", sepGit, "sep")
	sep := filepath.Join(base, "sep")
	identityGit(t, sep, "commit", "-q", "--allow-empty", "-m", "root")
	sepDeep := filepath.Join(sep, "pkg")
	identityMkdir(t, sepDeep)
	identityGit(t, sep, "worktree", "add", "-q", "../sep-wt", "-b", "swt")

	expectIdentity(t, sepGit, sep, sepDeep, filepath.Join(base, "sep-wt"))

	cloneGit := filepath.Join(store, "clone.git")
	identityGit(t, base, "clone", "-q", "--separate-git-dir", cloneGit, sep, "cloned")
	expectIdentity(t, cloneGit, filepath.Join(base, "cloned"))

	// Documented case: the main checkout path is not derivable. git records no
	// core.worktree for --separate-git-dir, and `git worktree list` names the
	// git directory as the first worktree, so a linked worktree has nothing that
	// leads back to "sep". The git directory is therefore the shared identity,
	// and the shared parent "store" is never used.
	cfg, err := os.ReadFile(filepath.Join(sepGit, "config"))
	if err != nil {
		t.Fatalf("read separate git dir config: %v", err)
	}
	if strings.Contains(string(cfg), "worktree") {
		t.Fatalf("git now records core.worktree for --separate-git-dir; the documented case changed:\n%s", cfg)
	}
}

// TestResolveProjectUnfollowableGitFileKeepsWalking pins the fallback: a .git
// file that cannot be followed to a git directory is skipped and the walk
// continues upward, exactly as before #146.
func TestResolveProjectUnfollowableGitFileKeepsWalking(t *testing.T) {
	base := identityBase(t)
	main := filepath.Join(base, "main")
	identityRepo(t, main)
	host := filepath.Join(base, "host")
	identityRepo(t, host)
	identityGit(t, main, "worktree", "add", "-q", "../host/wt", "-b", "wt")
	wt := filepath.Join(host, "wt")
	expectIdentity(t, main, wt)

	// The worktree's admin directory is gone (deleted by hand, not pruned):
	// the gitdir line points nowhere.
	if err := os.RemoveAll(filepath.Join(main, ".git", "worktrees", "wt")); err != nil {
		t.Fatalf("remove admin dir: %v", err)
	}
	garbage := filepath.Join(host, "garbage")
	identityMkdir(t, garbage)
	if err := os.WriteFile(filepath.Join(garbage, ".git"), []byte("not a gitdir line\n"), 0o644); err != nil {
		t.Fatalf("write garbage .git: %v", err)
	}
	// A gitdir line naming an existing directory that is no git directory.
	notGit := filepath.Join(host, "not-git")
	identityMkdir(t, filepath.Join(base, "plain", ".git"))
	identityMkdir(t, notGit)
	if err := os.WriteFile(filepath.Join(notGit, ".git"), []byte("gitdir: ../../plain/.git\n"), 0o644); err != nil {
		t.Fatalf("write not-git .git: %v", err)
	}
	// A .git file beyond gitMetaMaxBytes is not read, even though it names a
	// valid git directory once the trailing line ends are dropped.
	huge := filepath.Join(host, "huge")
	identityMkdir(t, huge)
	padded := "gitdir: " + filepath.Join(main, ".git") + strings.Repeat("\n", gitMetaMaxBytes)
	if err := os.WriteFile(filepath.Join(huge, ".git"), []byte(padded), 0o644); err != nil {
		t.Fatalf("write oversized .git: %v", err)
	}
	for _, d := range []string{wt, garbage, notGit, huge} {
		if got, legacy := resolveProject(d), legacyResolveProject(d); got != host || got != legacy {
			t.Errorf("resolveProject(%s) = %s, want %s (pre-#146 resolver: %s)", d, got, host, legacy)
		}
	}
}

// TestResolveProjectReadsGitFilesLikeGit pins the line format of .git and
// commondir files to what git 2.56.0 accepts: the prefix is exactly
// "gitdir: " and only trailing CR/LF is dropped. A file git refuses is
// skipped and the walk goes on; a file git follows is followed.
func TestResolveProjectReadsGitFilesLikeGit(t *testing.T) {
	base := identityBase(t)
	main := filepath.Join(base, "main")
	identityRepo(t, main)
	host := filepath.Join(base, "host")
	identityRepo(t, host)
	mainGit := filepath.Join(main, ".git")

	cases := []struct {
		name, content, want string
	}{
		{"CRLF line end (git follows)", "gitdir: " + mainGit + "\r\n", main},
		{"no blank after the colon (git: invalid gitfile format)", "gitdir:" + mainGit + "\n", host},
		{"two blanks after the colon (git: not a valid repository)", "gitdir:  " + mainGit + "\n", host},
		{"trailing blank after the path (git: not a valid repository)", "gitdir: " + mainGit + " \n", host},
	}
	for i, c := range cases {
		dir := filepath.Join(host, "case"+strconv.Itoa(i))
		identityMkdir(t, dir)
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(c.content), 0o644); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		if got := resolveProject(dir); got != c.want {
			t.Errorf("%s: resolveProject = %s, want %s", c.name, got, c.want)
		}
	}

	// commondir follows the same rule: git refuses "../.. " (trailing blank).
	identityGit(t, main, "worktree", "add", "-q", "../host/wt", "-b", "wt")
	if err := os.WriteFile(filepath.Join(mainGit, "worktrees", "wt", "commondir"), []byte("../.. \n"), 0o644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	expectIdentity(t, host, filepath.Join(host, "wt"))
}

// TestGitCoreWorktreeReadsGitConfigSyntax pins the config reader behind the
// submodule rule against the value syntax git writes and accepts: quoting,
// escapes, inline comments, case-insensitive names, a key on the header line,
// subsections kept apart, last value wins, and config.worktree only under
// extensions.worktreeConfig.
func TestGitCoreWorktreeReadsGitConfigSyntax(t *testing.T) {
	const moved = "[core]\n\tworktree = ../new\n"
	cases := []struct {
		name, config, configWorktree, want string
	}{
		{"plain", "[core]\n\tbare = false\n\tworktree = ../../../sub\n", "", "../../../sub"},
		{"quoted with blank and comment", "[core]\n\tworktree = \"../my sub\" # moved by hand\n", "", "../my sub"},
		{"escapes", "[core]\n\tworktree = \"a\\\\b \\\"c\\\"\"\n", "", `a\b "c"`},
		{"semicolon comment", "[core]\n\tworktree = ../x ; note\n", "", "../x"},
		{"comment lines", "# top\n[core]\n; inside\n\tworktree=../c\n", "", "../c"},
		{"case-insensitive names", "[Core]\n\tWorkTree = ../y\n", "", "../y"},
		{"key on the header line", "[core] worktree = ../z\n", "", "../z"},
		{"CRLF line endings", "[core]\r\n\tworktree = ../crlf\r\n", "", "../crlf"},
		{"subsection is not core", "[remote \"origin\"]\n\tworktree = ../no\n", "", ""},
		{"last value wins", "[core]\n\tworktree = ../a\n[core]\n\tworktree = ../b\n", "", "../b"},
		{"extension as bare key", "[core]\n\tworktree = ../old\n[extensions]\n\tworktreeConfig\n", moved, "../new"},
		{"extension = 1", "[core]\n\tworktree = ../old\n[extensions]\n\tworktreeConfig = 1\n", moved, "../new"},
		{"extension = YES", "[core]\n\tworktree = ../old\n[extensions]\n\tworktreeConfig = YES\n", moved, "../new"},
		{"extension = false", "[core]\n\tworktree = ../old\n[extensions]\n\tworktreeConfig = false\n", moved, "../old"},
		{"extension = 0", "[core]\n\tworktree = ../old\n[extensions]\n\tworktreeConfig = 0\n", moved, "../old"},
		{"no extension, stray config.worktree", "[core]\n\tworktree = ../old\n", moved, "../old"},
		{"extension, config.worktree without the key", "[core]\n\tworktree = ../old\n[extensions]\n\tworktreeConfig = true\n", "[core]\n\tsparseCheckout = true\n", "../old"},
		// Blanks: git keeps them inside quotes and drops only unquoted
		// leading and trailing ones (`git config --get`, git 2.56.0).
		{"quoted trailing blank", "[core]\n\tworktree = \"../../../sub \"\n", "", "../../../sub "},
		{"quoted leading blanks", "[core]\n\tworktree = \"  lead\"   \n", "", "  lead"},
		{"quoted blank before comment", "[core]\n\tworktree = \"a \" # c\n", "", "a "},
		{"unquoted blanks around a comment", "[core]\n\tworktree = a  ; c\n", "", "a"},
		{"inner unquoted blanks kept verbatim", "[core]\n\tworktree = a\t \tb\n", "", "a\t \tb"},
		{"quote in the middle", "[core]\n\tworktree = a\" \"b\n", "", "a b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config"), []byte(c.config), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if c.configWorktree != "" {
				if err := os.WriteFile(filepath.Join(dir, "config.worktree"), []byte(c.configWorktree), 0o644); err != nil {
					t.Fatalf("write config.worktree: %v", err)
				}
			}
			if got := gitCoreWorktree(dir); got != c.want {
				t.Errorf("gitCoreWorktree = %q, want %q", got, c.want)
			}
		})
	}
}

// TestBriefHookLinkedWorktreeServesMainCheckoutBriefing drives the
// SubagentStart hook through stdin from inside a sibling worktree: the
// briefing tagged with the main checkout is served, not the global fallback.
func TestBriefHookLinkedWorktreeServesMainCheckoutBriefing(t *testing.T) {
	base := identityBase(t)
	main := filepath.Join(base, "main")
	identityRepo(t, main)
	identityGit(t, main, "worktree", "add", "-q", "../main-feat", "-b", "feat")
	cwd := filepath.Join(base, "main-feat", "go")
	identityMkdir(t, cwd)

	srv := newFakeBriefSearch(t, []fakeBriefBlock{
		{id: "b1", title: "Main checkout briefing", tags: []string{main}},
		{id: "b2", title: "Global briefing", tags: []string{briefGlobalTag}},
	}, nil)
	defer srv.Close()

	stdin, err := json.Marshal(map[string]string{"cwd": cwd, "agent_type": "general-purpose"})
	if err != nil {
		t.Fatalf("marshal hook input: %v", err)
	}
	var out string
	withStdin(t, string(stdin), func() { out = runBriefAgainst(t, srv, true, 20, "") })
	if !strings.Contains(out, "Main checkout briefing") || strings.Contains(out, "Global briefing") {
		t.Errorf("hook from a linked worktree must serve the main checkout's briefing only:\n%s", out)
	}
}

// TestPersistHookLinkedWorktreeTagsMainCheckout covers the second consumer of
// resolveProject: `ctx persist --hook` from a linked worktree tags its writes
// (and metadata.project) with the main checkout, the tag `ctx brief` reads.
func TestPersistHookLinkedWorktreeTagsMainCheckout(t *testing.T) {
	base := identityBase(t)
	main := filepath.Join(base, "main")
	identityRepo(t, main)
	identityGit(t, main, "worktree", "add", "-q", "../main-feat", "-b", "feat")
	cwd := filepath.Join(base, "main-feat", "go")
	identityMkdir(t, cwd)

	var (
		mu   sync.Mutex
		tags [][]string
		meta []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tags     []string          `json:"tags"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad store body: %v", err)
		}
		mu.Lock()
		tags = append(tags, req.Tags)
		meta = append(meta, req.Metadata["project"])
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":"b1","action":"created"}`))
	}))
	defer srv.Close()
	getClient := func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}

	stdin, err := json.Marshal(map[string]string{
		"cwd": cwd, "session_id": "s1", "agent_type": "general", "agent_id": "a1",
		"last_assistant_message": "done\n[PERSIST:learnings:One]\nfirst\n",
	})
	if err != nil {
		t.Fatalf("marshal hook input: %v", err)
	}
	cmd := persistCmd(getClient)
	cmd.SetArgs([]string{"--hook"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var runErr error
	withStdin(t, string(stdin), func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("persist --hook: %v", runErr)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tags) != 1 || len(tags[0]) != 1 || tags[0][0] != main || meta[0] != main {
		t.Errorf("persist from a linked worktree: tags %v, metadata.project %v; want [%s] and %s", tags, meta, main, main)
	}
}
