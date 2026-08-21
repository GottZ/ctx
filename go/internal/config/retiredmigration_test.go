package config

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// migrationFile is the delete migration of the backend-tuple retirement
// (β11 / A05-W-D2). Named as a constant because two tests read it and a
// renamed file must fail loudly here, not silently stop being checked.
const retireMigrationFile = "133_retire_backend_tuple_rows.sql"

// retireRequestID is the audit marker the migration stamps onto every unset it
// causes (SET LOCAL ctx.request_id). It is quoted verbatim in the runbook's
// rollback query (docs/operations.md, "Migration 133") and in the migration's
// own third notice, so it is pinned rather than left to drift.
const retireRequestID = "migration-133-retire-backend-tuples"

// arrayLiteral matches the ARRAY[ … ] the migration deletes on, non-greedily
// up to the first closing bracket. Anchoring on ARRAY[ rather than scanning
// the whole file matters: the file also carries quoted strings in SET LOCAL
// and in three RAISE NOTICE texts, and none of them are key names.
var arrayLiteral = regexp.MustCompile(`(?s)ARRAY\[(.*?)\]`)

var quotedString = regexp.MustCompile(`'([^']*)'`)

// TestRetireMigrationDeletesExactlyTheRetiredKeys pins the SECOND transcript of
// the 29 names against the first. config.retiredSettingKeys is the one Go-side
// source of the retirement, but a SQL migration cannot call a Go function — the
// key list has to exist a second time inside the .sql file, and the two can
// drift in both directions with different damage:
//
//   - a key MISSING from the array leaves its rows behind on every foreign
//     installation. They are invisible after the registry cut (HandleList is
//     registry-driven, referencedBy filters on config.Keys(), a tenant-scoped
//     orphan does not even reach the global reload's WARN) and become live
//     configuration again the day someone re-registers the name.
//   - a key TOO MANY in the array deletes rows of a LIVING key on every
//     installation that runs the upgrade — silent config loss, no error.
//
// Set equality catches both. The test is a unit test on purpose: it reads the
// embedded FS, needs no database, and therefore runs in the `-short` loop that
// guards every commit.
func TestRetireMigrationDeletesExactlyTheRetiredKeys(t *testing.T) {
	sql := readRetireMigration(t)

	match := arrayLiteral.FindStringSubmatch(sql)
	if match == nil {
		t.Fatalf("%s: no ARRAY[ … ] literal found — the delete migration must carry the key list as an array literal, or this pin stops pinning anything", retireMigrationFile)
	}

	var fromSQL []string
	for _, q := range quotedString.FindAllStringSubmatch(match[1], -1) {
		fromSQL = append(fromSQL, q[1])
	}
	sort.Strings(fromSQL)

	want := RetiredKeyNames() // sorted by contract
	if len(fromSQL) != len(want) {
		t.Fatalf("%s: array lists %d key(s), config.RetiredKeyNames() has %d\n array: %v\n   map: %v",
			retireMigrationFile, len(fromSQL), len(want), fromSQL, want)
	}
	for i := range want {
		if fromSQL[i] != want[i] {
			t.Errorf("%s: array[%d] = %q, config.RetiredKeyNames()[%d] = %q — the two transcripts of the 29 have drifted apart",
				retireMigrationFile, i, fromSQL[i], i, want[i])
		}
	}

	// Duplicate guard: a doubled entry would keep the counts matching against
	// a shorter map only by accident, and it would make the migration's own
	// intent unreadable.
	seen := make(map[string]bool, len(fromSQL))
	for _, k := range fromSQL {
		if seen[k] {
			t.Errorf("%s: key %q listed more than once in the array", retireMigrationFile, k)
		}
		seen[k] = true
	}
}

// TestRetireMigrationCarriesTheUpgradeHopAndAuditMarker pins the two texts the
// migration owes its operator, both of which live nowhere else in Go code.
//
// The hop hint is a user requirement on decision E4 ("die 'migrate to last 4.x'
// meldung muss es tragen"): after E13 the API answers a bare 404 on all 29
// keys, so nothing on the wire explains the move. Four places carry it instead
// — BREAKING tag annotation, runbook, README hop paragraph, and this notice.
// This test owns the fourth.
//
// The audit marker is the rollback path: it is the only handle on the values
// the migration deletes (design/05 §5.3b), and the runbook quotes it verbatim.
func TestRetireMigrationCarriesTheUpgradeHopAndAuditMarker(t *testing.T) {
	sql := readRetireMigration(t)

	if !strings.Contains(sql, "migrate to last 4.x") {
		t.Errorf("%s: the operator message lost the %q hint — after E13 the API says nothing about the move, so this notice is one of only four places that do (DECISIONS.md, E4 comment)",
			retireMigrationFile, "migrate to last 4.x")
	}

	// The hint must live inside a RAISE, not merely in a SQL comment: a
	// comment reaches nobody at boot.
	var inRaise bool
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(trimmed, "RAISE NOTICE") && strings.Contains(trimmed, "migrate to last 4.x") {
			inRaise = true
		}
	}
	if !inRaise {
		t.Errorf("%s: %q appears only in comments — it has to be raised at migration time to reach the boot log",
			retireMigrationFile, "migrate to last 4.x")
	}

	if !strings.Contains(sql, "SET LOCAL ctx.request_id = '"+retireRequestID+"'") {
		t.Errorf("%s: audit marker %q not set — without it the deleted values are unattributable and the runbook's rollback query returns nothing",
			retireMigrationFile, retireRequestID)
	}
}

func readRetireMigration(t *testing.T) string {
	t.Helper()
	raw, err := migrations.FS.ReadFile(retireMigrationFile)
	if err != nil {
		t.Fatalf("read %s from the embedded migration FS: %v", retireMigrationFile, err)
	}
	return string(raw)
}
