package main

// Byte-compatibility gate of wave C4-3b.
//
// C4-3b gives `ctx-goldset pool` a `-slice` flag. The flag's DEFAULT has to
// leave the tool exactly as it was — the G-REAL template of the standing
// C3-4a/C3-4b strecke was built by the old command, and a template whose bytes
// moved would silently invalidate every digest already stamped against it
// (STAMP.json carries judge_sha256 and judgement_sha256).
//
// The golden files under testdata/c4-3b/ were produced by running THIS test
// against the pre-C4-3b command (root 8e27cdef) with C43B_UPDATE_GOLDEN=1.
// Everything the test touches therefore exists in both versions, and the
// comparison is a real before/after one rather than a snapshot of the new
// behaviour describing itself as correct.

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GottZ/ctx/internal/goldset"
)

// c43bGoldenDir holds the artefacts the unchanged command wrote.
const c43bGoldenDir = "testdata/c4-3b"

// c43bExcerpt is short enough to force a truncation INSIDE a multi-byte rune,
// so the golden pins the rune-boundary rule of excerptOf and not just the
// happy path.
const c43bExcerpt = 40

// TestPoolDefaultArtefactsAreByteIdentical is the byte-compatibility gate: the
// default pooling run over a fixed fixture must reproduce the artefacts of the
// pre-C4-3b command bit for bit — the JSONL form, the markdown form, the
// control key and the summary the command prints.
func TestPoolDefaultArtefactsAreByteIdentical(t *testing.T) {
	g := c43bGold(t)
	o := poolOpts{control: 5, excerpt: c43bExcerpt}

	out := c43bCaptureStdout(t, func() {
		c43bEmitDefaultTemplate(t, g, o)
	})

	c43bCompareGolden(t, "stdout.txt", []byte(out))
	for _, name := range []string{
		"judge-" + c43bRunID + ".jsonl",
		"judge-" + c43bRunID + ".md",
		keyPrefix + c43bRunID + ".json",
	} {
		got, err := os.ReadFile(filepath.Join(g.Root(), name)) //nolint:gosec // G304: path built from the test's own guard root
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		c43bCompareGolden(t, name, got)
		c43bWantOwnerOnly(t, filepath.Join(g.Root(), name))
	}
}

// c43bEmitDefaultTemplate runs the pooling pipeline of cmdPool minus its two
// database calls: the control corpus and the block lookup are the fixture's,
// everything else is the production code path.
//
// The slice is named explicitly here — g-real.jsonl and G-REAL — because that
// is what the pre-C4-3b command had hard-wired at pool.go:48. That the DEFAULT
// of the new flag resolves to exactly this pair is a separate assertion in
// pool_slice_c43b_test.go; here it is the fixed input of the byte comparison.
func c43bEmitDefaultTemplate(t *testing.T, g *goldset.Guard, o poolOpts) {
	t.Helper()
	cases, err := readSlice(g, goldset.FileReal, goldset.SliceReal)
	if err != nil {
		t.Fatalf("readSlice: %v", err)
	}
	poolPath, runID, err := resolvePool(g, o.poolFile)
	if err != nil {
		t.Fatalf("resolvePool: %v", err)
	}
	entries, err := goldset.ReadPool(poolPath)
	if err != nil {
		t.Fatalf("ReadPool: %v", err)
	}
	pooled, key, err := goldset.BuildPool(cases, entries, c43bControlPool(), o.control, c43bSeed)
	if err != nil {
		t.Fatalf("BuildPool: %v", err)
	}
	key.PoolRunID, key.CreatedAt = runID, c43bCreatedAt
	ids := allCandidateIDs(pooled)
	blocks := c43bBlocksOf(ids)
	if err := emitTemplate(g, o, pooled, blocks, key, runID, len(ids)-len(blocks)); err != nil {
		t.Fatalf("emitTemplate: %v", err)
	}
}

// c43bCompareGolden compares one artefact against its frozen form, or rewrites
// that form when C43B_UPDATE_GOLDEN is set. The update path exists to freeze
// the OLD bytes once; a run that silently refreshed the goldens would turn the
// gate into a description of whatever the code does today.
func c43bCompareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join(c43bGoldenDir, name)
	if os.Getenv("C43B_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(c43bGoldenDir, 0o750); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(p, got, 0o600); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		t.Logf("golden %s geschrieben (%d Bytes)", name, len(got))
		return
	}
	want, err := os.ReadFile(p) //nolint:gosec // G304: fixed path under the package's testdata
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s ist nicht byte-identisch zur Vorlage des unveränderten Befehls\n"+
			"--- erwartet (%d Bytes)\n%s\n--- erhalten (%d Bytes)\n%s",
			name, len(want), want, len(got), got)
	}
}

// c43bWantOwnerOnly asserts the 0600 doctrine of the gold directory.
func c43bWantOwnerOnly(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("%s hat Modus %04o, erwartet 0600", filepath.Base(path), fi.Mode().Perm())
	}
}

// c43bCaptureStdout collects what fn prints. The summary line of `pool` names
// the counts a reader uses to decide whether a template is complete, so it is
// part of the artefact surface and belongs in the comparison.
func c43bCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = saved
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe read end: %v", err)
	}
	return out
}
