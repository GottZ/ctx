package llmlog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// privateBase liefert ein 0700-Verzeichnis AUSSERHALB von /tmp (t.TempDir
// läge unter os.TempDir und würde vom Perimeter zu Recht abgewiesen).
func privateBase(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(cwd, ".export-perimeter-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCheckExportDir pinnt den Containment-Perimeter (design/02 §3.7):
// Shared-Temp-Pfade und group/other-Bits werden abgewiesen, 0700 nicht.
func TestCheckExportDir(t *testing.T) {
	base := privateBase(t)

	t.Run("0700 passes", func(t *testing.T) {
		if err := CheckExportDir(base); err != nil {
			t.Fatalf("0700 dir must pass: %v", err)
		}
	})
	t.Run("group bits rejected", func(t *testing.T) {
		d := filepath.Join(base, "grp")
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := CheckExportDir(d); !errors.Is(err, ErrExportPerimeter) {
			t.Fatalf("0750 dir must be rejected, got %v", err)
		}
	})
	t.Run("/tmp rejected", func(t *testing.T) {
		d, err := os.MkdirTemp("/tmp", "llmlog-perimeter-*")
		if err != nil {
			t.Skip("no /tmp:", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		if err := os.Chmod(d, 0o700); err != nil {
			t.Fatal(err)
		}
		// 0700 UND trotzdem rot — der Pfad allein disqualifiziert.
		if err := CheckExportDir(d); !errors.Is(err, ErrExportPerimeter) {
			t.Fatalf("/tmp dir must be rejected even at 0700, got %v", err)
		}
	})
	t.Run("symlink into /tmp rejected", func(t *testing.T) {
		target, err := os.MkdirTemp("/tmp", "llmlog-perimeter-*")
		if err != nil {
			t.Skip("no /tmp:", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(target) })
		link := filepath.Join(base, "lnk")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := CheckExportDir(link); !errors.Is(err, ErrExportPerimeter) {
			t.Fatalf("symlink into /tmp must be rejected, got %v", err)
		}
	})
	t.Run("missing dir rejected", func(t *testing.T) {
		if err := CheckExportDir(filepath.Join(base, "nope")); !errors.Is(err, ErrExportPerimeter) {
			t.Fatalf("missing dir must be rejected, got %v", err)
		}
	})
}

// TestCreateExportFile: 0600 unabhängig von der umask, kein Überschreiben.
func TestCreateExportFile(t *testing.T) {
	base := privateBase(t)
	old := setUmask(0o000)
	t.Cleanup(func() { setUmask(old) })

	p := filepath.Join(base, "export.jsonl")
	f, err := CreateExportFile(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = f.Close()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("export file must be 0600 regardless of umask, got %04o", st.Mode().Perm())
	}
	if _, err := CreateExportFile(p); err == nil {
		t.Fatal("existing export must not be overwritten (O_EXCL)")
	}
}
