package schemacontract

// The parts of the generator (generate_test.go, -tags=genmanifest) that the
// ordinary unit run checks as well: the fingerprint of the migration chain
// and the declared extension minimums. They live in an untagged _test.go so
// both builds compile them; nothing in the binary needs them.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GottZ/ctx/migrations"
)

// migrationChainSHA256 computes GeneratedAgainst.MigrationChainSHA256 for
// the *.sql files at the top of fsys — every one go:embed takes, names
// starting with '.' or '_' included: sha256 over one line
// "<sha256 hex of the file>  <file name>\n" per file, in byte order of the
// names, i.e. what `sha256sum <those files> | sha256sum` prints in
// go/migrations. Each line's hash is the checksum the migration runner
// records for that file (store.applyMigration), so the fingerprint moves
// with every byte the runner would see move. .hooks/pre-commit (Gate 3)
// computes the same value over the staged files; keep the two alike.
func migrationChainSHA256(fsys fs.FS) (string, error) {
	entries, err := fs.ReadDir(fsys, ".") // sorted by name
	if err != nil {
		return "", fmt.Errorf("reading migrations: %w", err)
	}
	listing := sha256.New()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		content, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return "", fmt.Errorf("reading migration %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(content)
		fmt.Fprintf(listing, "%s  %s\n", hex.EncodeToString(sum[:]), e.Name())
	}
	return hex.EncodeToString(listing.Sum(nil)), nil
}

// extensionMinimums is the contract's minimum version per extension — policy,
// like hardcodedGucProbes, not what the generator's database happens to run.
// The generator writes exactly this set into the manifest, so a run against a
// newer local image (the trap of 2026-10-22: timescaledb 2.31.0 and vector
// 0.8.7 written over pins of 2.30.1 and 0.8.6) or against an image that ships
// further extensions (timescale/timescaledb-ha:pg18 carries
// timescaledb_toolkit) moves no pin; a database below a pin fails the
// generator's self-check. A pin moves by editing this map, in the change that
// moves db-image/Dockerfile to the version it names, and regenerating.
var extensionMinimums = map[string]string{
	"pg_trgm":     "1.6",
	"pgcrypto":    "1.4",
	"plpgsql":     "1.0",
	"timescaledb": "2.31.0",
	"vector":      "0.8.7",
}

// declaredExtensions renders extensionMinimums as the Manifest section.
func declaredExtensions() map[string]ExtSpec {
	out := make(map[string]ExtSpec, len(extensionMinimums))
	for name, v := range extensionMinimums {
		out[name] = ExtSpec{MinVersion: v}
	}
	return out
}

// TestMigrationChainSHA256Definition pins the definition the pre-commit hook
// shares: the expected value is the output of
// `sha256sum .dot.sql 101_alpha.sql 102_beta.sql 99_early.sql | sha256sum`
// (byte order of the names, not version order: 99_early sorts last, .dot
// first), and files that are not *.sql at the top level do not count.
func TestMigrationChainSHA256Definition(t *testing.T) {
	fsys := fstest.MapFS{
		".dot.sql":                {Data: []byte("SELECT 0;\n")},
		"101_alpha.sql":           {Data: []byte("CREATE TABLE alpha (id int);\n")},
		"102_beta.sql":            {Data: []byte("-- beta\nSELECT 1;\n")},
		"99_early.sql":            {Data: []byte("SELECT 99;\n")},
		"README":                  {Data: []byte("not sql\n")},
		"testdata/103_nested.sql": {Data: []byte("SELECT 3;\n")},
	}
	got, err := migrationChainSHA256(fsys)
	if err != nil {
		t.Fatal(err)
	}
	const want = "9437cf107fe32a3f2a9597ec79281a0ec0f0791dbd0e7bec90378421ebbfb8f2"
	if got != want {
		t.Errorf("migrationChainSHA256 = %s, want %s", got, want)
	}
}

// TestManifestNamesTheMigrationChain is the CI half of issue #397: the
// checked-in manifest must have been generated from the migrations beside
// it. TestCheck_FreshDB_NoDrift (integration) proves the manifest's schema
// matches what the chain builds; that cannot tell a regenerated manifest
// from a stale one after an edit that leaves the schema as it was — this
// test can, without a database.
func TestManifestNamesTheMigrationChain(t *testing.T) {
	want, err := migrationChainSHA256(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	got := Embedded().GeneratedAgainst.MigrationChainSHA256
	if got != want {
		t.Errorf("manifest.json names migration chain %q, go/migrations is %q: regenerate the manifest "+
			"(cd go && go test -tags=genmanifest ./internal/schemacontract -run TestGenerateManifest)", got, want)
	}
}

// TestManifestExtensionsAreTheDeclaredMinimums: the manifest's extension pins
// are extensionMinimums, not the versions of whatever image the generator
// last ran against (see extensionMinimums).
func TestManifestExtensionsAreTheDeclaredMinimums(t *testing.T) {
	if got, want := Embedded().Extensions, declaredExtensions(); !maps.Equal(got, want) {
		t.Errorf("manifest.json extensions = %v, extensionMinimums = %v: regenerate the manifest, "+
			"and move a pin only in extensionMinimums", got, want)
	}
}
