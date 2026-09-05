package provenance

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

const (
	fixHash    = "88692b2f307ad351eca7e4644aa68886cf113a56"
	fixShort   = "88692b2"
	otherHex   = "0123456789abcdef0123456789abcdef01234567"
	otherShort = "0123456"
)

// write legt eine Fixture-Datei samt Elternverzeichnissen an.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// barrier legt im Fixture-Wurzelverzeichnis ein auflösbares Repository an.
// Die Negativ-Fälle brauchen es, weil der Aufstieg sonst aus dem Fixture
// herausläuft: t.TempDir folgt GOTMPDIR, und im ctx-Baum zeigt das in den
// Arbeitsbaum hinein — ein Fixture ohne Barriere fände dessen HEAD und der
// Test wäre eine Aussage über die Umgebung statt über den Code.
func barrier(t *testing.T, root string) {
	t.Helper()
	write(t, filepath.Join(root, ".git", "HEAD"), otherHex+"\n")
}

// TestWorktreeRev deckt die vier Auflösungszweige ab, die in `package main`
// eines Binaries ohne Testdatei nie geprüft werden konnten: Worktree-Datei mit
// gitdir:-Zeile, commondir, direkte Ref-Datei und packed-refs — jeder gegen ein
// eigenes t.TempDir()-Fixture, ohne echtes Repository und ohne Subprozess.
func TestWorktreeRev(t *testing.T) {
	tests := []struct {
		name string
		// setup baut das Fixture unter root und liefert das Startverzeichnis.
		setup func(t *testing.T, root string) string
		want  string
	}{
		{
			name: "worktree_datei_absoluter_gitdir_detached_head",
			setup: func(t *testing.T, root string) string {
				gitdir := filepath.Join(root, "gitdirs", "wt")
				write(t, filepath.Join(root, "wt", ".git"), "gitdir: "+gitdir+"\n")
				write(t, filepath.Join(gitdir, "HEAD"), fixHash+"\n")
				return filepath.Join(root, "wt")
			},
			want: fixShort,
		},
		{
			name: "worktree_datei_relativer_gitdir",
			setup: func(t *testing.T, root string) string {
				write(t, filepath.Join(root, "wt", ".git"), "gitdir: ../gitdirs/wt\n")
				write(t, filepath.Join(root, "gitdirs", "wt", "HEAD"), fixHash+"\n")
				return filepath.Join(root, "wt")
			},
			want: fixShort,
		},
		{
			name: "commondir_traegt_die_ref_des_worktrees",
			setup: func(t *testing.T, root string) string {
				gitdir := filepath.Join(root, "repo", ".git", "worktrees", "wt")
				common := filepath.Join(root, "repo", ".git")
				write(t, filepath.Join(root, "wt", ".git"), "gitdir: "+gitdir+"\n")
				write(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/main\n")
				// Relativer commondir-Eintrag, wie git ihn schreibt ("../..").
				write(t, filepath.Join(gitdir, "commondir"), "../..\n")
				write(t, filepath.Join(common, "refs", "heads", "main"), fixHash+"\n")
				return filepath.Join(root, "wt")
			},
			want: fixShort,
		},
		{
			name: "direkte_ref_datei",
			setup: func(t *testing.T, root string) string {
				git := filepath.Join(root, "repo", ".git")
				write(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
				write(t, filepath.Join(git, "refs", "heads", "main"), fixHash+"\n")
				return filepath.Join(root, "repo")
			},
			want: fixShort,
		},
		{
			name: "packed_refs",
			setup: func(t *testing.T, root string) string {
				git := filepath.Join(root, "repo", ".git")
				write(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
				write(t, filepath.Join(git, "packed-refs"),
					"# pack-refs with: peeled fully-peeled sorted \n"+
						otherHex+" refs/heads/andere\n"+
						fixHash+" refs/heads/main\n"+
						"^"+otherHex+"\n")
				return filepath.Join(root, "repo")
			},
			want: fixShort,
		},
		{
			name: "aufstieg_aus_tiefem_unterverzeichnis",
			setup: func(t *testing.T, root string) string {
				git := filepath.Join(root, "repo", ".git")
				write(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
				write(t, filepath.Join(git, "refs", "heads", "main"), fixHash+"\n")
				sub := filepath.Join(root, "repo", "go", "internal", "provenance")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatalf("MkdirAll(%s): %v", sub, err)
				}
				return sub
			},
			want: fixShort,
		},
		{
			name: "verzeichnis_ohne_git_steigt_weiter_auf",
			setup: func(t *testing.T, root string) string {
				barrier(t, root)
				sub := filepath.Join(root, "leer", "tiefer")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatalf("MkdirAll(%s): %v", sub, err)
				}
				return sub
			},
			want: otherShort,
		},
		{
			name: "gitdir_datei_ohne_praefix_steigt_weiter_auf",
			setup: func(t *testing.T, root string) string {
				barrier(t, root)
				write(t, filepath.Join(root, "wt", ".git"), "irgendwas anderes\n")
				return filepath.Join(root, "wt")
			},
			want: otherShort,
		},
		{
			name: "ref_zeigt_ins_leere_steigt_weiter_auf",
			setup: func(t *testing.T, root string) string {
				barrier(t, root)
				git := filepath.Join(root, "repo", ".git")
				write(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
				write(t, filepath.Join(git, "packed-refs"), otherHex+" refs/heads/andere\n")
				return filepath.Join(root, "repo")
			},
			want: otherShort,
		},
		{
			name: "detached_head_zu_kurz_steigt_weiter_auf",
			setup: func(t *testing.T, root string) string {
				barrier(t, root)
				write(t, filepath.Join(root, "repo", ".git", "HEAD"), "abc123\n")
				return filepath.Join(root, "repo")
			},
			want: otherShort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setup(t, t.TempDir())
			if got := WorktreeRev(dir); got != tt.want {
				t.Errorf("WorktreeRev(%q) = %q, want %q", dir, got, tt.want)
			}
		})
	}
}

// TestWorktreeRevLeeresVerzeichnis pinnt die Kante, die der Aufrufer braucht:
// ein nicht ermittelbares Arbeitsverzeichnis darf keinen Stempel erfinden.
func TestWorktreeRevLeeresVerzeichnis(t *testing.T) {
	if got := WorktreeRev(""); got != "" {
		t.Errorf(`WorktreeRev("") = %q, want ""`, got)
	}
}

// TestWorktreeRevRelativerPfad pinnt die Kante, an der ein naiver Aufstieg
// stillschweigend leer zurückkäme: filepath.Dir(".") ist wieder ".", der
// Aufstieg endete also nach einem Schritt. Der Produktionsaufrufer übergibt
// zwar os.Getwd() (absolut), aber ein Paket, das für "." nichts findet, wäre
// eine Falle für den nächsten Aufrufer.
func TestWorktreeRevRelativerPfad(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".git", "HEAD"), fixHash+"\n")
	sub := filepath.Join(root, "go", "cmd")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", sub, err)
	}
	t.Chdir(sub)

	for _, dir := range []string{".", "..", "../.."} {
		if got := WorktreeRev(dir); got != fixShort {
			t.Errorf("WorktreeRev(%q) = %q, want %q", dir, got, fixShort)
		}
	}
}

// TestWorktreeRevAufstiegEndetAnDerWurzel pinnt den Abbruch des Aufstiegs:
// oberhalb des Dateisystem-Wurzelverzeichnisses gibt es kein Elternteil mehr,
// und der Stempel bleibt leer statt sich in einer Endlosschleife zu drehen.
func TestWorktreeRevAufstiegEndetAnDerWurzel(t *testing.T) {
	if rev := revFromGitDir(filepath.Join(string(filepath.Separator), ".git")); rev != "" {
		t.Skipf("das Dateisystem-Wurzelverzeichnis ist selbst ein Repository (%q)", rev)
	}
	if got := WorktreeRev(string(filepath.Separator)); got != "" {
		t.Errorf("WorktreeRev(%q) = %q, want \"\"", string(filepath.Separator), got)
	}
}

// TestWorktreeRevIstSiebenZeichen hält die Größenzusage fest: kurz, ohne
// Dirty-Flag — im Gegensatz zu BuildRev.
func TestWorktreeRevIstSiebenZeichen(t *testing.T) {
	root := t.TempDir()
	git := filepath.Join(root, ".git")
	write(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(git, "refs", "heads", "main"), fixHash+"\n")

	got := WorktreeRev(root)
	if len(got) != 7 {
		t.Errorf("WorktreeRev(%q) = %q (%d Zeichen), want 7", root, got, len(got))
	}
	if strings.Contains(got, "-dirty") {
		t.Errorf("WorktreeRev(%q) = %q trägt ein Dirty-Flag — das gehört zu BuildRev", root, got)
	}
	if !strings.HasPrefix(fixHash, got) {
		t.Errorf("WorktreeRev(%q) = %q ist kein Präfix von %q", root, got, fixHash)
	}
}

func TestShortRev(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"leer", "", ""},
		{"sechs_zeichen", "abc123", ""},
		{"sieben_zeichen", "abc1234", "abc1234"},
		{"voller_hash", fixHash, fixShort},
		{"mit_leerzeichen", fixHash + " refs/heads/main", ""},
		{"mit_tab", fixHash + "\tfoo", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shortRev(tt.in); got != tt.want {
				t.Errorf("shortRev(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRevFromGitDirFehlendeMetadaten(t *testing.T) {
	root := t.TempDir()

	if got := revFromGitDir(filepath.Join(root, "gibtsnicht")); got != "" {
		t.Errorf("revFromGitDir(fehlender Pfad) = %q, want \"\"", got)
	}
	if got := revFromGitDir(root); got != "" { // Verzeichnis ohne HEAD
		t.Errorf("revFromGitDir(Verzeichnis ohne HEAD) = %q, want \"\"", got)
	}
	if got := commonGitDir(root); got != "" {
		t.Errorf("commonGitDir(ohne commondir) = %q, want \"\"", got)
	}
	if got := revFromPackedRefs(filepath.Join(root, "packed-refs"), "refs/heads/main"); got != "" {
		t.Errorf("revFromPackedRefs(fehlende Datei) = %q, want \"\"", got)
	}
}

// TestBuildRevGegenBuildInfo prüft die zweite Größe an ihrer Vertragskante: ein
// Testbinary trägt keine VCS-Stempel (Go stempelt nur `go build`), deshalb wird
// nicht auf einen Wert gepinnt, sondern auf die Regeln — Wert genau dann leer,
// wenn vcs.revision fehlt, und "-dirty" genau dann, wenn vcs.modified gesetzt ist.
func TestBuildRevGegenBuildInfo(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("keine BuildInfo in diesem Binary")
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	got := BuildRev()
	switch {
	case rev == "":
		if got != "" {
			t.Errorf("BuildRev() = %q ohne vcs.revision in der BuildInfo, want \"\"", got)
		}
	case dirty:
		if got != rev+"-dirty" {
			t.Errorf("BuildRev() = %q, want %q", got, rev+"-dirty")
		}
	default:
		if got != rev {
			t.Errorf("BuildRev() = %q, want %q", got, rev)
		}
	}
	if got != "" && len(got) < len(fixHash) {
		t.Errorf("BuildRev() = %q ist gekürzt — BuildRev trägt den vollen Hash, WorktreeRev die 7 Zeichen", got)
	}
}
