package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// The driver half of wave X-W0b: the fail-closed refusal has to be LOUD, and
// on this seam loud means an exit code a scheduler can act on.

// TestRegimeRefusalIsExitFour pins the class the refusal belongs to: the
// artefacts were readable and the run was clean — what was rejected is the
// requested SPLIT, which is the same verdict class as a dump that predates
// type_name or a dump set whose stamps do not describe one campaign. A
// scheduler must not retry it as if a gate had gone red.
func TestRegimeRefusalIsExitFour(t *testing.T) {
	if got := exitCodeFor(fmt.Errorf("x: %w", armsweep.ErrRegimeLabelMissing)); got != 4 {
		t.Errorf("exit code for a missing regime label = %d, want 4", got)
	}
	if !errors.Is(fmt.Errorf("x: %w", armsweep.ErrRegimeLabelMissing), armsweep.ErrRegimeLabelMissing) {
		t.Error("the refusal does not survive wrapping — the cascade would read it as exit 1")
	}
}

// TestLoadRegimeSplitIsOptInAndGuarded pins the loader contract: no flag, no
// split (and therefore the report this instrument always wrote); a named file
// is resolved through the gold guard like every other gold artefact; a broken
// file refuses instead of producing a partial partition.
func TestLoadRegimeSplitIsOptInAndGuarded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), goldset.DirName)
	g, err := goldset.NewGuard(dir, false)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}

	empty, err := loadRegimeSplit(g, "")
	if err != nil {
		t.Fatalf("loadRegimeSplit(\"\"): %v", err)
	}
	if empty.Active() {
		t.Error("an unset flag produced an active split")
	}

	labels := filepath.Join(dir, goldset.FileRegimeLabels)
	body := "{\"query_sha256\":\"aa\",\"regime\":\"local\"}\n{\"query_sha256\":\"bb\",\"regime\":\"global\"}\n"
	if err := os.WriteFile(labels, []byte(body), 0o600); err != nil {
		t.Fatalf("write labels: %v", err)
	}
	split, err := loadRegimeSplit(g, goldset.FileRegimeLabels)
	if err != nil {
		t.Fatalf("loadRegimeSplit: %v", err)
	}
	if !split.Active() || len(split.Regimes) != 2 {
		t.Fatalf("split = %+v, want two labels", split)
	}
	if split.File != goldset.FileRegimeLabels || split.SHA256 != goldset.SHA256Hex(body) {
		t.Errorf("provenance = %s/%s, want %s/%s",
			split.File, split.SHA256, goldset.FileRegimeLabels, goldset.SHA256Hex(body))
	}

	if err := os.WriteFile(labels, []byte("{\"query_sha256\":\"aa\",\"regime\":\"lokal\"}\n"), 0o600); err != nil {
		t.Fatalf("rewrite labels: %v", err)
	}
	if _, err := loadRegimeSplit(g, goldset.FileRegimeLabels); err == nil {
		t.Error("an unknown regime was accepted — the split would silently lose a case")
	}
}
