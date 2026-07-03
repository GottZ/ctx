package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ── fixtures ──────────────────────────────────────────────────────────────────.

// fixtureBase returns a fresh empty fixture directory for one detect test.
//
// It deliberately does NOT use t.TempDir() (/tmp): GOTMPDIR points inside the ctx
// repo, and a fixture nested in ANY parent git repo would make detectFromGit
// climb into the outer repo and mis-detect the non-git case. We create fixtures
// under GOTMPDIR (off world-readable /tmp, briefing rule) AND pin
// GIT_CEILING_DIRECTORIES to the base so git never climbs above a fixture —
// hermetic regardless of where the base lives.
func fixtureBase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("GOTMPDIR")
	if base == "" {
		base = t.TempDir()
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	d, err := os.MkdirTemp(base, "ctxdetect-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// git runs a git command in dir with a fixed identity, failing the test on error.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepo makes dir a git repo with one commit (a single root).
func initRepo(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
}

// writeMarker writes a .ctx-project file with the given identity line.
func writeMarker(t *testing.T, dir, identity string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ctxProjectFile), []byte("identity="+identity+"\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

// fakePrompter is the test double for the interactive channel.
type fakePrompter struct {
	isPiped      bool
	confirmOK    bool
	slug         string
	confirmCalls int
	askCalls     int
}

func (f *fakePrompter) piped() bool { return f.isPiped }
func (f *fakePrompter) confirm(string) (bool, error) {
	f.confirmCalls++
	return f.confirmOK, nil
}
func (f *fakePrompter) askSlug() (string, error) {
	f.askCalls++
	return f.slug, nil
}

// ── githubSlug parsing ────────────────────────────────────────────────────────.

func TestGithubSlug(t *testing.T) {
	cases := []struct {
		url, want string
		ok        bool
	}{
		{"git@github.com:acme/widget.git", "acme/widget", true},
		{"https://github.com/acme/widget.git", "acme/widget", true},
		{"https://github.com/acme/widget", "acme/widget", true},
		{"ssh://git@github.com/acme/widget.git", "acme/widget", true},
		{"https://github.com/acme/widget/", "acme/widget", true},
		{"git@gitlab.com:acme/widget.git", "", false},
		{"https://example.com/acme/widget", "", false},
	}
	for _, c := range cases {
		got, ok := githubSlug(c.url)
		if ok != c.ok || got != c.want {
			t.Errorf("githubSlug(%q) = (%q,%v), want (%q,%v)", c.url, got, ok, c.want, c.ok)
		}
	}
}

// ── detect matrix (§4.3 / §7-W5) ──────────────────────────────────────────────.

func TestDetectMatrix_GithubRemote(t *testing.T) {
	for _, remote := range []string{
		"git@github.com:acme/widget.git",
		"https://github.com/acme/widget.git",
	} {
		dir := fixtureBase(t)
		initRepo(t, dir)
		git(t, dir, "remote", "add", "origin", remote)
		got, err := resolveIdentity(dir, &fakePrompter{isPiped: true})
		if err != nil {
			t.Fatalf("remote %q: %v", remote, err)
		}
		if got.Identity != "github:acme/widget" || got.Source != "github-remote" {
			t.Errorf("remote %q ⇒ %+v, want github:acme/widget/github-remote", remote, got)
		}
	}
}

func TestDetectMatrix_GitRoot(t *testing.T) {
	dir := fixtureBase(t)
	initRepo(t, dir)
	// The independent truth for the sha:
	sha, ok := gitOutput(dir, "rev-list", "--max-parents=0", "HEAD")
	if !ok {
		t.Fatal("could not read root sha")
	}
	got, err := resolveIdentity(dir, &fakePrompter{isPiped: true})
	if err != nil {
		t.Fatalf("git-root detect: %v", err)
	}
	if got.Identity != "git-root:"+sha || got.Source != "git-root" {
		t.Errorf("git-root ⇒ %+v, want git-root:%s/git-root", got, sha)
	}
}

func TestDetectMatrix_NonGithubRemoteFallsToRoot(t *testing.T) {
	dir := fixtureBase(t)
	initRepo(t, dir)
	git(t, dir, "remote", "add", "origin", "git@gitlab.com:acme/widget.git")
	got, err := resolveIdentity(dir, &fakePrompter{isPiped: true})
	if err != nil {
		t.Fatalf("gitlab remote: %v", err)
	}
	if got.Source != "git-root" || !strings.HasPrefix(got.Identity, "git-root:") {
		t.Errorf("gitlab remote ⇒ %+v, want a git-root identity", got)
	}
}

func TestDetectMatrix_NonGitPipeError(t *testing.T) {
	dir := fixtureBase(t) // no git init → not a repo
	_, err := resolveIdentity(dir, &fakePrompter{isPiped: true})
	if err == nil {
		t.Fatal("non-git + pipe ⇒ want error, got nil")
	}
	if !strings.Contains(err.Error(), "pipe mode") {
		t.Errorf("non-git pipe error = %q, want a pipe-mode message", err)
	}
}

func TestDetectMatrix_NonGitTTYManualPrompt(t *testing.T) {
	dir := fixtureBase(t)
	fp := &fakePrompter{isPiped: false, slug: "internal-docs"}
	got, err := resolveIdentity(dir, fp)
	if err != nil {
		t.Fatalf("non-git tty: %v", err)
	}
	if got.Identity != "manual:internal-docs" || got.Source != "manual" {
		t.Errorf("non-git tty ⇒ %+v, want manual:internal-docs/manual", got)
	}
	if fp.askCalls != 1 {
		t.Errorf("askSlug calls = %d, want 1", fp.askCalls)
	}
}

func TestDetectMatrix_MultiRootFallsToManual(t *testing.T) {
	dir := fixtureBase(t)
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root-a")
	git(t, dir, "checkout", "-q", "--orphan", "second")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root-b")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--allow-unrelated-histories", "--no-edit", "second")
	// Two root commits, no GitHub remote ⇒ manual (pipe ⇒ error).
	_, err := resolveIdentity(dir, &fakePrompter{isPiped: true})
	if err == nil {
		t.Fatal("multi-root + pipe ⇒ want manual/pipe error, got nil")
	}
}

// ── .ctx-project precedence: the confused-deputy gate (§4.3 / §7-W5) ───────────.

// TestCtxProjectMismatch is the mismatch probe. A planted .ctx-project that
// DISAGREES with the git remote must NOT be trusted: pipe ⇒ error, TTY ⇒
// confirmation. This is the test that goes RED against a file-first
// implementation (one that returns the file identity without verifying it).
func TestCtxProjectMismatch(t *testing.T) {
	newMismatchRepo := func(t *testing.T) string {
		dir := fixtureBase(t)
		initRepo(t, dir)
		git(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
		writeMarker(t, dir, "github:evil/repo") // planted, ≠ git remote
		return dir
	}

	t.Run("pipe_errors", func(t *testing.T) {
		dir := newMismatchRepo(t)
		_, err := resolveIdentity(dir, &fakePrompter{isPiped: true})
		if err == nil {
			t.Fatal("mismatch + pipe ⇒ want error, got nil (file was trusted — file-first bug)")
		}
		if !strings.Contains(err.Error(), "git detection") {
			t.Errorf("mismatch pipe error = %q, want a detection-mismatch message", err)
		}
	})

	t.Run("tty_reject_aborts", func(t *testing.T) {
		dir := newMismatchRepo(t)
		fp := &fakePrompter{isPiped: false, confirmOK: false}
		_, err := resolveIdentity(dir, fp)
		if err == nil {
			t.Fatal("mismatch + reject ⇒ want error, got nil (file was trusted — file-first bug)")
		}
		if fp.confirmCalls != 1 {
			t.Errorf("confirm calls = %d, want 1 (a mismatch must PROMPT)", fp.confirmCalls)
		}
	})

	t.Run("tty_confirm_uses_file", func(t *testing.T) {
		dir := newMismatchRepo(t)
		fp := &fakePrompter{isPiped: false, confirmOK: true}
		got, err := resolveIdentity(dir, fp)
		if err != nil {
			t.Fatalf("mismatch + confirm: %v", err)
		}
		if got.Identity != "github:evil/repo" || got.Source != "ctx-project-file-confirmed" {
			t.Errorf("confirmed ⇒ %+v, want github:evil/repo/ctx-project-file-confirmed", got)
		}
	})
}

func TestCtxProjectMatchIsShortcut(t *testing.T) {
	dir := fixtureBase(t)
	initRepo(t, dir)
	git(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	writeMarker(t, dir, "github:acme/widget") // matches
	fp := &fakePrompter{isPiped: true}
	got, err := resolveIdentity(dir, fp)
	if err != nil {
		t.Fatalf("matching marker: %v", err)
	}
	if got.Identity != "github:acme/widget" || got.Source != "ctx-project-file" {
		t.Errorf("match ⇒ %+v, want github:acme/widget/ctx-project-file", got)
	}
	if fp.confirmCalls != 0 {
		t.Errorf("a MATCHING file must not prompt, confirm calls = %d", fp.confirmCalls)
	}
}

func TestCtxProjectManualIsAuthoritative(t *testing.T) {
	dir := fixtureBase(t) // no git at all
	writeMarker(t, dir, "manual:my-thing")
	fp := &fakePrompter{isPiped: true} // pipe: would error if it fell to the manual prompt
	got, err := resolveIdentity(dir, fp)
	if err != nil {
		t.Fatalf("manual marker: %v", err)
	}
	if got.Identity != "manual:my-thing" || got.Source != "ctx-project-file" {
		t.Errorf("manual file ⇒ %+v, want manual:my-thing/ctx-project-file", got)
	}
}

// ── pipe JSON golden (§7-W5) ──────────────────────────────────────────────────.

// TestEmitIdentityPipeGolden freezes the pipe-mode JSON shape byte-for-byte.
// stdout under `go test` is not a TTY, so emitIdentity takes the JSON branch.
func TestEmitIdentityPipeGolden(t *testing.T) {
	const want = `{
  "identity": "github:acme/widget",
  "source": "github-remote"
}
`
	got := captureStdout(t, func() {
		emitIdentity(resolvedIdentity{Identity: "github:acme/widget", Source: "github-remote"})
	})
	if got != want {
		t.Errorf("pipe golden mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = orig
	var buf strings.Builder
	dec := make([]byte, 0)
	tmp := make([]byte, 4096)
	for {
		n, e := r.Read(tmp)
		dec = append(dec, tmp[:n]...)
		if e != nil {
			break
		}
	}
	buf.Write(dec)
	return buf.String()
}

// ── scope-name derivation ─────────────────────────────────────────────────────.

func TestDeriveScopeName(t *testing.T) {
	cases := []struct{ id, want string }{
		{"github:acme/My.Widget", "my-widget"},
		{"manual:Internal Docs!", "internal-docs"},
		{"git-root:abcdef0123456789", "repo-abcdef012345"},
	}
	for _, c := range cases {
		if got := deriveScopeName(c.id); got != c.want {
			t.Errorf("deriveScopeName(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// ── checkAPIEnvelope ──────────────────────────────────────────────────────────.

func TestCheckAPIEnvelope(t *testing.T) {
	if err := checkAPIEnvelope([]byte(`{"success":true,"projects":[]}`)); err != nil {
		t.Errorf("success:true ⇒ %v, want nil", err)
	}
	if err := checkAPIEnvelope([]byte(`{"success":false,"error":"nope"}`)); err == nil || err.Error() != "nope" {
		t.Errorf("success:false ⇒ %v, want \"nope\"", err)
	}
	// A non-envelope body (no success field) is not an error.
	if err := checkAPIEnvelope([]byte(`{"status":"ok"}`)); err != nil {
		t.Errorf("no-success body ⇒ %v, want nil", err)
	}
}
