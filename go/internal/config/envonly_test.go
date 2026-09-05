package config

import (
	"go/build"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The gate over the env-only class (design/05 §4.6, masterplan K10).
//
// The scan area is not a directory glob but a PACKAGE SET: the module-local
// closure of cmd/ctxd, i.e. the server runtime itself. That definition is the
// whole point — internal/cli (CTX_BASE_URL, CTX_KEY), internal/clientconfig
// and internal/testdb (CTX_TEST_PG_IMAGE) hold non-test env literals that are
// neither registered nor env-only nor retired, and a glob over internal/ would
// go red with the wrong subject. cmd/ctx-* falls out for the same reason; the
// tooling names have their own fence.
//
// The closure is resolved with go/build rather than by shelling out to
// `go list -deps`: same answer (verified by hand, 47 packages either way), no
// subprocess, no toolchain assumption inside the test.
//
// Unlike retireddocs_test.go, this gate needs no -count=1 caveat: everything
// it reads lives inside the module, so the test cache invalidates on its own.
// Measured — a cached green run followed by a new os.Getenv file in
// internal/handler goes red without any flag.

const envScanModulePath = "github.com/GottZ/ctx"

// envOnlyFileBase is the one file excluded from the remainder comparison
// below. envonly.go lives INSIDE the scanned closure and spells every name it
// classifies, so counting it would make the map its own witness: a stale
// entry would find itself and stay green. Excluding it turns the comparison
// into the stronger statement — every declared name must appear at its real
// definition site somewhere else in the closure.
const envOnlyFileBase = "envonly.go"

// serverRuntimePackages returns the module-local packages reachable from
// cmd/ctxd, resolved to directories.
func serverRuntimePackages(t *testing.T) []ScanPackage {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	seen := map[string]bool{}
	var out []ScanPackage
	var walk func(importPath string)
	walk = func(importPath string) {
		if seen[importPath] {
			return
		}
		seen[importPath] = true
		rel := strings.TrimPrefix(strings.TrimPrefix(importPath, envScanModulePath), "/")
		dir := filepath.Join(root, filepath.FromSlash(rel))
		pkg, err := build.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("resolve package %s (%s): %v", importPath, dir, err)
		}
		out = append(out, ScanPackage{ImportPath: importPath, Dir: dir})
		for _, imp := range pkg.Imports {
			if imp == envScanModulePath || strings.HasPrefix(imp, envScanModulePath+"/") {
				walk(imp)
			}
		}
	}
	walk(envScanModulePath + "/cmd/ctxd")
	sort.Slice(out, func(i, j int) bool { return out[i].ImportPath < out[j].ImportPath })
	// A lower bound, not a fixed count: the closure grows and shrinks with
	// legitimate package work (47 on 2026-09-05), but a walk that lost the
	// tree collapses to a handful.
	if len(out) < 40 {
		t.Fatalf("server runtime closure has only %d packages — the walk lost the tree", len(out))
	}
	return out
}

// nameSet turns a name slice into a lookup set.
func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// classifiedEnvNames is the union of the three classes a server env name may
// belong to: registered, declared env-only, retired.
func classifiedEnvNames() map[string]bool {
	out := nameSet(EnvVars())
	for _, name := range EnvOnlyServerNames() {
		out[name] = true
	}
	for _, name := range RetiredEnvNames() {
		out[name] = true
	}
	return out
}

// TestServerRuntimeScopeIsTheCtxdClosure pins the scan area itself. Without
// it, the gate below could pass by scanning nothing, or fail with the wrong
// subject by scanning the client packages.
func TestServerRuntimeScopeIsTheCtxdClosure(t *testing.T) {
	paths := map[string]bool{}
	for _, pkg := range serverRuntimePackages(t) {
		paths[pkg.ImportPath] = true
	}
	for _, want := range []string{"cmd/ctxd", "internal/handler", "internal/camo", "internal/sealbox", "internal/settings"} {
		if !paths[envScanModulePath+"/"+want] {
			t.Errorf("server runtime closure misses %s — the class would not cover it", want)
		}
	}
	// The other half of the discrimination: a CTX_ literal in these packages
	// must NOT reach the gate. They are client and test harness code.
	for _, unwanted := range []string{"internal/cli", "internal/clientconfig", "internal/testdb"} {
		if paths[envScanModulePath+"/"+unwanted] {
			t.Errorf("server runtime closure contains %s — the scan area is no longer the server runtime", unwanted)
		}
	}
}

// TestEveryServerEnvNameIsClassified is the tripwire: a new os.Getenv in the
// server runtime is a decision with one line in envonly.go, not a line
// without a decision.
func TestEveryServerEnvNameIsClassified(t *testing.T) {
	pkgs := serverRuntimePackages(t)
	start := time.Now()
	refs, err := ScanEnvNames(pkgs, classifiedEnvNames())
	if err != nil {
		t.Fatalf("scan server runtime: %v", err)
	}
	t.Logf("scanned %d packages in %s", len(pkgs), time.Since(start).Round(time.Millisecond))
	for _, ref := range refs {
		t.Errorf("unclassified server env name %s — register it (config.go struct tag), "+
			"declare it env-only with a reason (envonly.go) or retire it (retired.go)", ref)
	}
}

// TestEnvOnlyServerNamesIsTheRemainder states the count as an arithmetic
// identity rather than as a constant: the env-only class IS what is left of
// the closure's env literals after the registry and the retirement list. A
// constant would need hand-editing on every legitimate registry change and
// would drift into a transcript of the map it is supposed to check.
func TestEnvOnlyServerNamesIsTheRemainder(t *testing.T) {
	pkgs := serverRuntimePackages(t)
	refs, err := ScanEnvNames(pkgs, nil)
	if err != nil {
		t.Fatalf("scan server runtime: %v", err)
	}
	registry, retired := nameSet(EnvVars()), nameSet(RetiredEnvNames())
	literals, inRegistry, inRetired := map[string]bool{}, map[string]bool{}, map[string]bool{}
	remainder := map[string]bool{}
	for _, ref := range refs {
		if ref.ImportPath == envScanModulePath+"/internal/config" && filepath.Base(ref.File) == envOnlyFileBase {
			continue
		}
		literals[ref.Name] = true
		switch {
		case registry[ref.Name]:
			inRegistry[ref.Name] = true
		case retired[ref.Name]:
			inRetired[ref.Name] = true
		default:
			remainder[ref.Name] = true
		}
	}
	declared := EnvOnlyServerNames()
	t.Logf("literals %d − registry %d − retired %d = %d, declared %d",
		len(literals), len(inRegistry), len(inRetired), len(remainder), len(declared))
	if got := len(literals) - len(inRegistry) - len(inRetired); got != len(declared) {
		t.Errorf("remainder is %d but EnvOnlyServerNames() has %d entries", got, len(declared))
	}
	for _, name := range declared {
		if !remainder[name] {
			t.Errorf("%s is declared env-only but no longer appears in the server runtime — "+
				"drop the entry in the commit that removed its call site", name)
		}
		delete(remainder, name)
	}
	for name := range remainder {
		t.Errorf("%s bypasses the registry without a declared reason — add it to envonly.go", name)
	}
}

// TestEveryEnvOnlyNameCarriesAReason keeps the map from degenerating into a
// second name list. The reason is the entire content of the class: a name
// with an empty one has been silenced, not classified.
func TestEveryEnvOnlyNameCarriesAReason(t *testing.T) {
	for _, name := range EnvOnlyServerNames() {
		if strings.TrimSpace(envOnlyServerNames[name]) == "" {
			t.Errorf("%s is declared env-only without a reason — say why it may not be a DB row", name)
		}
	}
}

// TestRegistryEnvNamesHaveExactlyTwoDirectReaders pins the SET of packages
// that read a registered env name directly instead of through the loaded
// config (design/05 Naht 13): schemacontract (the break-glass over the DB
// writer) and overview (the rebuild child process, which has no config
// object). Both handle an empty value as unset, which is what carries the
// compose-declaration equivalence argument; a third reader with a different
// convention would break it silently. The SET is pinned rather than a call
// syntax — an os.Getenv pin would be trivially bypassed via os.LookupEnv or a
// helper.
func TestRegistryEnvNamesHaveExactlyTwoDirectReaders(t *testing.T) {
	pkgs := serverRuntimePackages(t)
	refs, err := ScanEnvNames(pkgs, nil)
	if err != nil {
		t.Fatalf("scan server runtime: %v", err)
	}
	registry := nameSet(EnvVars())
	got := map[string]string{}
	for _, ref := range refs {
		if ref.ImportPath == envScanModulePath+"/internal/config" || !registry[ref.Name] {
			continue
		}
		got[ref.ImportPath] = ref.Name
	}
	want := map[string]bool{
		envScanModulePath + "/internal/schemacontract": true,
		envScanModulePath + "/internal/overview":       true,
	}
	for path, name := range got {
		if !want[path] {
			t.Errorf("%s reads the registered env name %s directly — a third direct reader "+
				"has to be argued (design/05 Naht 13), not added", path, name)
		}
		delete(want, path)
	}
	for path := range want {
		t.Errorf("%s no longer reads a registered env name directly — drop it from the pinned set", path)
	}
}
