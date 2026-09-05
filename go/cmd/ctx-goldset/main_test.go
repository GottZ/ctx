package main

// DSN resolution gate.
//
// ctx-goldset used to carry the only .env FILE parser in the tree: a tool-local
// reader behind a hard-coded absolute path, which also honoured a tool-local
// second host name, CONTEXT_GOLDSET_DB_HOST. Both are gone — the tool reads the
// CONTEXT_DB_* process environment, exactly like ctxd and the three neighbouring
// DB-direct tools, and the caller sources the file itself
// (`set -a; . .env; set +a`, docs/operations.md).
//
// These tests pin the two halves of that cut: the defaults and the escaping
// survive the move byte-for-byte, and the retired name is answered
// fail-closed. Fail-closed is the load-bearing half — reading CONTEXT_DB_HOST
// instead would connect to a DIFFERENT database than the one named on the
// command line, without a word on stderr.

import (
	"strings"
	"testing"
)

// clearDBEnv detaches a test from whatever CONTEXT_DB_* the runner happens to
// carry. Empty is the same as unset for get(), so this is the neutral ground
// every case below starts from.
func clearDBEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CONTEXT_DB_USER", "CONTEXT_DB_PASSWORD", "CONTEXT_DB",
		"CONTEXT_DB_HOST", "CONTEXT_DB_PORT", "CONTEXT_GOLDSET_DB_HOST",
	} {
		t.Setenv(k, "")
	}
}

// TestDSNFromProcessEnvUsesTheDocumentedDefaults pins the three defaults the
// old file parser had — context_store, localhost, 5432 — against the process
// environment as the only source.
func TestDSNFromProcessEnvUsesTheDocumentedDefaults(t *testing.T) {
	clearDBEnv(t)
	t.Setenv("CONTEXT_DB_USER", "u")
	t.Setenv("CONTEXT_DB_PASSWORD", "p")

	got, err := dsnFromProcessEnv()
	if err != nil {
		t.Fatalf("dsnFromProcessEnv: %v", err)
	}
	const want = "postgres://u:p@localhost:5432/context_store?sslmode=disable"
	if got != want {
		t.Errorf("DSN with defaults\n got %q\nwant %q", got, want)
	}
}

// TestDSNFromProcessEnvHonoursEveryName checks that each CONTEXT_DB_* name
// still lands in the position it held before the cut.
func TestDSNFromProcessEnvHonoursEveryName(t *testing.T) {
	clearDBEnv(t)
	t.Setenv("CONTEXT_DB_USER", "reader")
	t.Setenv("CONTEXT_DB_PASSWORD", "secret")
	t.Setenv("CONTEXT_DB", "other_store")
	t.Setenv("CONTEXT_DB_HOST", "172.32.16.3")
	t.Setenv("CONTEXT_DB_PORT", "55432")

	got, err := dsnFromProcessEnv()
	if err != nil {
		t.Fatalf("dsnFromProcessEnv: %v", err)
	}
	const want = "postgres://reader:secret@172.32.16.3:55432/other_store?sslmode=disable"
	if got != want {
		t.Errorf("DSN from a full environment\n got %q\nwant %q", got, want)
	}
}

// TestDSNFromProcessEnvEscapesCredentials keeps urlEscape on the credential
// pair. A password carrying @ or / would otherwise cut the DSN in two and the
// tool would connect somewhere else entirely.
func TestDSNFromProcessEnvEscapesCredentials(t *testing.T) {
	clearDBEnv(t)
	t.Setenv("CONTEXT_DB_USER", "u@x")
	t.Setenv("CONTEXT_DB_PASSWORD", "p:/?#%")
	t.Setenv("CONTEXT_DB_HOST", "127.0.0.1")

	got, err := dsnFromProcessEnv()
	if err != nil {
		t.Fatalf("dsnFromProcessEnv: %v", err)
	}
	const want = "postgres://u%40x:p%3A%2F%3F%23%25@127.0.0.1:5432/context_store?sslmode=disable"
	if got != want {
		t.Errorf("DSN with escaped credentials\n got %q\nwant %q", got, want)
	}
}

// TestDSNFromProcessEnvNamesTheMissingCredentials pins the error the operator
// sees without CONTEXT_DB_USER/CONTEXT_DB_PASSWORD. The old text named the env
// file as a second place to look; that place is gone, the rest is unchanged.
func TestDSNFromProcessEnvNamesTheMissingCredentials(t *testing.T) {
	for _, tc := range []struct{ name, user, pass string }{
		{"no user", "", "p"},
		{"no password", "u", ""},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearDBEnv(t)
			t.Setenv("CONTEXT_DB_USER", tc.user)
			t.Setenv("CONTEXT_DB_PASSWORD", tc.pass)

			got, err := dsnFromProcessEnv()
			if err == nil {
				t.Fatalf("expected an error, got DSN %q", got)
			}
			const want = "CONTEXT_DB_USER/CONTEXT_DB_PASSWORD missing (env)"
			if err.Error() != want {
				t.Errorf("error text\n got %q\nwant %q", err.Error(), want)
			}
		})
	}
}

// TestRetiredGoldsetHostFailsClosed is the tombstone gate: the retired name
// must stop the run, not be ignored. It fires before the credential check, so
// an operator repeating the old recipe hears about the name and not about
// something else that happens to be missing too.
func TestRetiredGoldsetHostFailsClosed(t *testing.T) {
	clearDBEnv(t)
	t.Setenv("CONTEXT_DB_USER", "u")
	t.Setenv("CONTEXT_DB_PASSWORD", "p")
	t.Setenv("CONTEXT_GOLDSET_DB_HOST", "172.32.16.3")

	got, err := dsnFromProcessEnv()
	if err == nil {
		t.Fatalf("CONTEXT_GOLDSET_DB_HOST was ignored, DSN %q — the cut is fail-open", got)
	}
	const want = "CONTEXT_GOLDSET_DB_HOST ist entfallen — CONTEXT_DB_HOST setzen"
	if err.Error() != want {
		t.Errorf("tombstone text\n got %q\nwant %q", err.Error(), want)
	}
	// The tombstone must not leak the value it refuses: the recipes in the
	// measurement reports pass a container IP there.
	if strings.Contains(err.Error(), "172.32.16.3") {
		t.Errorf("tombstone repeats the refused host: %q", err.Error())
	}
}

// TestRetiredGoldsetHostBeatsMissingCredentials fixes the ORDER of the two
// checks. Without it a later refactor could move the credential guard up, and
// the operator on the old recipe would be sent chasing credentials that are
// not the problem.
func TestRetiredGoldsetHostBeatsMissingCredentials(t *testing.T) {
	clearDBEnv(t)
	t.Setenv("CONTEXT_GOLDSET_DB_HOST", "172.32.16.3")

	_, err := dsnFromProcessEnv()
	if err == nil {
		t.Fatal("expected the tombstone error")
	}
	if !strings.HasPrefix(err.Error(), "CONTEXT_GOLDSET_DB_HOST") {
		t.Errorf("credential check won over the tombstone: %q", err.Error())
	}
}
