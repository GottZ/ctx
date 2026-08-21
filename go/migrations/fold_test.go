package migrations

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"testing"
)

// TestFoldedChainIntegrity is the unit-level guard on the fold: every folded
// migration must still be readable out of the baseline, byte for byte, and
// must still hash to the checksum the baseline's ledger writes into
// _migrations. It is what makes those checksums verifiable claims rather
// than numbers copied out of a dead tree — if a section is edited, this test
// goes red before any database sees the drift.
func TestFoldedChainIntegrity(t *testing.T) {
	folded := Folded()
	if len(folded) == 0 {
		t.Fatal("folded chain is empty")
	}

	prev := 0
	for _, f := range folded {
		if f.Version <= prev {
			t.Errorf("folded chain out of order at version %d (previous %d)", f.Version, prev)
		}
		prev = f.Version
		if f.Version > BaselineVersion {
			t.Errorf("folded version %d exceeds the baseline version %d", f.Version, BaselineVersion)
		}

		body, err := Section(f.Filename)
		if err != nil {
			t.Errorf("section %s: %v", f.Filename, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("section %s is empty", f.Filename)
			continue
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])

		want := f.Checksum
		if f.Normalized {
			// The two files that carried their own BEGIN;/COMMIT; block: the
			// section is the file minus those lines, so it hashes to
			// SectionChecksum while _migrations keeps the file's own hash.
			want = f.SectionChecksum
			if want == "" {
				t.Errorf("%s is marked normalized but carries no section checksum", f.Filename)
				continue
			}
		} else if f.SectionChecksum != "" {
			t.Errorf("%s is not normalized but carries a section checksum", f.Filename)
		}
		if got != want {
			t.Errorf("section %s hashes to %s, want %s", f.Filename, got, want)
		}
	}

	// Order matters as much as content: the sections are applied top to
	// bottom, so a section that moved would produce a different schema while
	// every checksum above still matched.
	body, err := FS.ReadFile(BaselineFile)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	prevOffset := -1
	for _, f := range folded {
		at := bytes.Index(body, []byte(markerBegin+f.Filename+"\n"))
		if at < 0 {
			t.Errorf("no section marker for %s", f.Filename)
			continue
		}
		if at <= prevOffset {
			t.Errorf("section %s sits at offset %d, out of version order (previous section at %d)", f.Filename, at, prevOffset)
		}
		prevOffset = at
	}

	if !IsFolded(BaselineVersion) {
		t.Errorf("version %d is the fold line but not in the folded chain", BaselineVersion)
	}
	if IsFolded(BaselineVersion + 1) {
		t.Errorf("version %d is above the fold line but reported as folded", BaselineVersion+1)
	}
}

// TestFoldLedgerMatchesFoldedChain pins the second half of the same claim:
// the SQL ledger at the end of the baseline and the Go-side folded chain are
// two spellings of one list. A version in one and not the other means either
// a database gets a row the contract does not expect, or the contract expects
// a row no database gets.
func TestFoldLedgerMatchesFoldedChain(t *testing.T) {
	body, err := FS.ReadFile(BaselineFile)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	// Ledger rows look like:  (51, '051_settings_store.sql', '<64 hex>'),
	re := regexp.MustCompile(`\((\d+), '([^']+)', '([0-9a-f]{64})'\)`)
	matches := re.FindAllStringSubmatch(string(body), -1)

	ledger := map[int][2]string{}
	for _, m := range matches {
		v, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("ledger version %q: %v", m[1], err)
		}
		if _, dup := ledger[v]; dup {
			t.Errorf("ledger records version %d twice", v)
		}
		ledger[v] = [2]string{m[2], m[3]}
	}

	folded := Folded()
	if len(ledger) != len(folded) {
		t.Errorf("ledger has %d rows, folded chain has %d entries", len(ledger), len(folded))
	}
	for _, f := range folded {
		row, ok := ledger[f.Version]
		if !ok {
			t.Errorf("version %d (%s) is folded but has no ledger row", f.Version, f.Filename)
			continue
		}
		if row[0] != f.Filename || row[1] != f.Checksum {
			t.Errorf("ledger row %d = (%s, %s), folded chain says (%s, %s)",
				f.Version, row[0], row[1], f.Filename, f.Checksum)
		}
	}
}
