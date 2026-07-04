package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteAgentKeyFile_ModeAndLocation is the I-I file-mode gate: the repo-agent
// key MUST land 0600 UNDER ~/.config/ctx/projects/, and NEVER in the working
// directory (a secret in the CWD is a commit-into-the-repo hazard, §4.6 step 4).
// The fixture pins the config dir via XDG_CONFIG_HOME.
func TestWriteAgentKeyFile_ModeAndLocation(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPDATA", "")

	// Run from a separate "repo" CWD so the "never in the CWD" probe is meaningful.
	cwd := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	const identity = "github:acme/api"
	const agentKey = "deadbeefsecretkeyplaintext000000"
	path, err := writeAgentKeyFile(identity, "https://ctx.example", agentKey, "gh-acme-api:main")
	if err != nil {
		t.Fatalf("writeAgentKeyFile: %v", err)
	}

	// (a) mode 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 0600", fi.Mode().Perm())
	}

	// (b) location: UNDER <XDG_CONFIG_HOME>/ctx/projects/, i.e. the config dir.
	wantDir := filepath.Join(cfg, "ctx", "projects")
	if filepath.Dir(path) != wantDir {
		t.Errorf("key file dir = %q, want %q", filepath.Dir(path), wantDir)
	}
	// The projects dir itself is 0700.
	if di, e := os.Stat(wantDir); e == nil && di.Mode().Perm() != 0o700 {
		t.Errorf("projects dir mode = %o, want 0700", di.Mode().Perm())
	}

	// (c) NEGATIVE: the key is NOT written into the CWD. Walk the CWD and assert no
	// file contains the plaintext key.
	entries, _ := os.ReadDir(cwd)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(cwd, e.Name()))
		if strings.Contains(string(data), agentKey) {
			t.Errorf("agent key leaked into the CWD file %q", e.Name())
		}
	}

	// (d) content carries the key + provenance, and documents the template.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "CTX_KEY="+agentKey) {
		t.Error("key file does not carry CTX_KEY=<agent key>")
	}
	if !strings.Contains(string(data), "allowed=[], write=[]") {
		t.Error("key file does not document the K12 template")
	}
}

// TestWriteProvisionMarker_Additive proves the .ctx-project marker keeps the W5
// `identity=` line (so detect still works) and adds only COMMENT pointers — no
// secret, additive-only.
func TestWriteProvisionMarker_Additive(t *testing.T) {
	dir := t.TempDir()
	const identity = "git-root:abcdef0123456789"
	const scope = "repo-abcdef012345:main"
	keyPath := "/home/u/.config/ctx/projects/deadbeef"
	if err := writeProvisionMarker(dir, identity, scope, keyPath); err != nil {
		t.Fatalf("writeProvisionMarker: %v", err)
	}
	// The detector must still resolve the identity from the file.
	got, ok, err := readCtxProjectFile(dir)
	if err != nil || !ok {
		t.Fatalf("readCtxProjectFile: ok=%v err=%v", ok, err)
	}
	if got != identity {
		t.Errorf("marker identity = %q, want %q (W5 line broken)", got, identity)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ctxProjectFile))
	// The scope pointer is present but as a comment (ignored by the reader).
	if !strings.Contains(string(raw), "# scope="+scope) {
		t.Error("marker missing the scope pointer comment")
	}
	// No secret ever in the marker (it is committed).
	if strings.Contains(string(raw), "CTX_KEY") {
		t.Error("marker must never contain a key")
	}
}

// TestProjectKeyPath_UnderConfigDir asserts the key path is deterministic and
// under the config dir, never the CWD.
func TestProjectKeyPath_UnderConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPDATA", "")
	p1 := projectKeyPath("github:acme/api")
	p2 := projectKeyPath("github:acme/api")
	if p1 != p2 {
		t.Errorf("projectKeyPath not deterministic: %q vs %q", p1, p2)
	}
	if !strings.HasPrefix(p1, filepath.Join(cfg, "ctx", "projects")) {
		t.Errorf("key path %q not under the config dir", p1)
	}
	if projectKeyPath("github:acme/other") == p1 {
		t.Error("distinct identities collided to the same key path")
	}
}
