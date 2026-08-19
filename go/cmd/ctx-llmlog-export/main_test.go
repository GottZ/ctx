package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunPerimeterExit pins the CLI-level gate of design/02 §7 KW1: `-out`
// under /tmp or in a group/other-readable directory ⇒ exit ≠ 0 — and it
// fails BEFORE any DB connection or file creation (no empty O_EXCL corpse).
func TestRunPerimeterExit(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(cwd, ".perimeter-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	t.Run("group bits", func(t *testing.T) {
		if err := os.Chmod(base, 0o750); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(base, "x.jsonl")
		var stderr bytes.Buffer
		if code := run([]string{"-out", out}, &stderr); code != 1 {
			t.Fatalf("exit=%d, want 1; stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "perimeter") {
			t.Fatalf("stderr must name the perimeter: %s", stderr.String())
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("no file may be created on perimeter failure (stat err=%v)", err)
		}
	})
	t.Run("/tmp", func(t *testing.T) {
		d, err := os.MkdirTemp("/tmp", "llmlog-perimeter-*")
		if err != nil {
			t.Skip("no /tmp:", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		var stderr bytes.Buffer
		if code := run([]string{"-out", filepath.Join(d, "x.jsonl")}, &stderr); code != 1 {
			t.Fatalf("exit=%d, want 1; stderr=%s", code, stderr.String())
		}
	})
	t.Run("-since-id without -since", func(t *testing.T) {
		if err := os.Chmod(base, 0o700); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if code := run([]string{"-out", filepath.Join(base, "y.jsonl"), "-since-id", "00000000-0000-0000-0000-000000000000"}, &stderr); code != 1 {
			t.Fatalf("exit=%d, want 1; stderr=%s", code, stderr.String())
		}
	})
	t.Run("missing -out", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := run(nil, &stderr); code != 1 {
			t.Fatalf("exit=%d, want 1", code)
		}
	})
}
