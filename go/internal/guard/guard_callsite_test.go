// Wave I-J call-site enumeration gate (design/02 §4.7/§5.3, §7-I-J):
// EVERY production ctx_guard_check call must pass ALL FIVE parameters
// explicitly — never rely on the p_same_scope_only SQL DEFAULT FALSE. A
// forgotten parameter is the fail-open trap (§5.3): a same-scope issue type
// would silently fall back to cross-scope matching. This source-scanning test
// goes RED the moment any production call site passes <5 arguments.
//
// Non-_test.go files only: the integration tests carry a DELIBERATE 1-arg
// ctx_guard_check($1::uuid) probe (the 42883 fail-closed negative gate,
// guard_t7_policy_integration_test.go) — that is not a call site the guard
// pipeline drives, so it is out of scope here.
//
// No DB, no build tag: runs under `go test -short`.
package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCtxGuardCheckCallSitesPassAllFiveArgs(t *testing.T) {
	// Walk the module root (this package sits at internal/guard).
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	const marker = "ctx_guard_check("
	callSites := 0

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip the test-vendor / build caches if present.
			base := info.Name()
			if base == "vendor" || base == ".git" || strings.HasPrefix(base, ".gocache") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(src)
		for idx := 0; ; {
			pos := strings.Index(text[idx:], marker)
			if pos < 0 {
				break
			}
			start := idx + pos + len(marker)
			nargs, ok := countArgs(text[start:])
			idx = start
			if !ok {
				t.Errorf("%s: unbalanced parens after ctx_guard_check( — cannot verify arg count", path)
				continue
			}
			callSites++
			if nargs < 5 {
				t.Errorf("%s: ctx_guard_check call passes %d args, want >=5 (explicit p_same_scope_only, §4.7)", path, nargs)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// Guard against the enumeration silently matching nothing (e.g. the SQL
	// literal was refactored out of a Go string): the production guard MUST
	// have at least the one checkBlock call site.
	if callSites == 0 {
		t.Fatalf("found 0 production ctx_guard_check call sites — enumeration is not scanning the guard code")
	}
}

// countArgs counts the top-level comma-separated arguments of a call whose
// opening paren has already been consumed. s begins at the first argument
// character. Returns (args, true) at the matching close paren; (0, false) if
// the parens never balance. An empty arg list yields 0.
func countArgs(s string) (int, bool) {
	depth := 0
	commas := 0
	sawArg := false
	for _, r := range s {
		switch r {
		case '(':
			depth++
			sawArg = true
		case ')':
			if depth == 0 {
				if !sawArg {
					return 0, true
				}
				return commas + 1, true
			}
			depth--
		case ',':
			if depth == 0 {
				commas++
			}
		default:
			if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
				sawArg = true
			}
		}
	}
	return 0, false
}
