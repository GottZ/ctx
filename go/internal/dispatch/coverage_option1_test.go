package dispatch

import (
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
)

// coverageWithoutOption1 is deriveCoverage as of 79f39b30 (F-W7), without
// the logging: every origin's rows as projected, External as the projection
// set it. It is the oracle of the ctx-free invariance of Option 1 (F-W5) and
// the lower bound of the ctx case.
func coverageWithoutOption1(byOrigin map[string][]BackendRow, targets map[string]TargetPolicy) (bool, []string) {
	covered := true
	var warns []string
	for origin, group := range byOrigin {
		roles := localBackgroundRoles(group)
		if len(roles) == 0 || targets[origin].Slots > 0 {
			continue
		}
		covered = false
		warns = append(warns, origin+" "+strings.Join(roles, ","))
	}
	sort.Strings(warns)
	return covered, warns
}

var (
	optHosts   = []string{"http://gpu:8089", "http://gpu2:8080", "https://a.example.com", "https://openrouter.ai/api", "https://b.example.org"}
	optScopes  = []string{GlobalScope, GlobalScope, "acme", "beta"}
	optRoles   = []string{"chat", "synthesis", "translate", "rerank", "dream", "dream-embed", "embed", "digest", "classify"}
	optClasses = []string{"generic", "llamacpp", "openrouter", ""}
	optLimits  = []map[string]any{nil, {}, {"slots": float64(1)}, {"slots": float64(4)}, {"slots": float64(0)}, {"slots": "x"},
		{"slots": float64(2), "herald_scope": "exempt"}, {"external_offer": true}, {"preempt_background": true}}
)

// optRow draws one projected row. A ctx row is never External in the
// projection (events.DispatchBackendRows, F-W7); every other row's External
// flag is drawn.
func optRow(r *rand.Rand, i int, classes []string) BackendRow {
	var roles []string
	for _, role := range optRoles {
		if r.Intn(4) == 0 {
			roles = append(roles, role)
		}
	}
	class := classes[r.Intn(len(classes))]
	return BackendRow{
		Name: fmt.Sprintf("r%d", i), Scope: optScopes[r.Intn(len(optScopes))], BaseURL: optHosts[r.Intn(len(optHosts))],
		ProviderClass: class, Limits: optLimits[r.Intn(len(optLimits))], Roles: roles,
		External: class != providerClassCtx && r.Intn(2) == 0,
	}
}

// optCoverage derives the policy of rows and returns the coverage term with
// its WARN lines (origin + roles) as deriveCoverage logged them.
func optCoverage(rows []BackendRow) (bool, []string, map[string][]BackendRow, map[string]TargetPolicy) {
	h := &attrHandler{}
	pol := DerivePolicy(rows, slog.New(h))
	var warns []string
	for _, l := range h.matching(coverageWarn) {
		// "<msg> origin=<o> roles=<r>"
		f := strings.Fields(l[strings.Index(l, "origin="):])
		warns = append(warns, strings.TrimPrefix(f[0], "origin=")+" "+strings.TrimPrefix(f[1], "roles="))
	}
	sort.Strings(warns)
	byOrigin := make(map[string][]BackendRow)
	for _, row := range rows {
		if o, err := NormalizeOrigin(row.BaseURL); err == nil {
			byOrigin[o] = append(byOrigin[o], row)
		}
	}
	return pol.covered, warns, byOrigin, pol.Targets
}

// TestCoverageOption1CtxFreeInvariant is the Bestands-Pin of Option 1: on row
// sets without a ctx row the coverage term and its WARN set are value-equal
// to the F-W7 derivation (100 000 random sets: shared origins, three scopes,
// every class but ctx, malformed and foreign limits).
func TestCoverageOption1CtxFreeInvariant(t *testing.T) {
	r := rand.New(rand.NewSource(20261107))
	const iters = 100000
	coveredN := 0
	for i := 0; i < iters; i++ {
		rows := make([]BackendRow, 1+r.Intn(7))
		for j := range rows {
			rows[j] = optRow(r, j, optClasses)
		}
		covered, warns, byOrigin, targets := optCoverage(rows)
		wantCovered, wantWarns := coverageWithoutOption1(byOrigin, targets)
		if covered != wantCovered || !slices.Equal(warns, wantWarns) {
			t.Fatalf("iter %d: covered %v warns %v, F-W7 derivation %v %v (rows %+v)", i, covered, warns, wantCovered, wantWarns, rows)
		}
		if covered {
			coveredN++
		}
	}
	t.Logf("ctx-free: %d random row sets value-equal to the F-W7 coverage term; covered in %d", iters, coveredN)
}

// TestCoverageOption1OnlyExemptsForeignCtxRows bounds Option 1 on row sets
// WITH ctx rows: it only ever exempts (covered under F-W7 ⇒ covered now, the
// WARN set a subset), and an origin it stops warning about carries a ctx row
// of a non-authoritative scope (splitAuthority) — authoritative ctx rows stay
// coverage-bound.
func TestCoverageOption1OnlyExemptsForeignCtxRows(t *testing.T) {
	r := rand.New(rand.NewSource(611))
	classes := append([]string{providerClassCtx, providerClassCtx}, optClasses...)
	const iters = 100000
	exempted := 0
	for i := 0; i < iters; i++ {
		rows := make([]BackendRow, 1+r.Intn(7))
		for j := range rows {
			rows[j] = optRow(r, j, classes)
		}
		covered, warns, byOrigin, targets := optCoverage(rows)
		legacyCovered, legacyWarns := coverageWithoutOption1(byOrigin, targets)
		if legacyCovered && !covered {
			t.Fatalf("iter %d: Option 1 uncovered a covered derivation: %v (rows %+v)", i, warns, rows)
		}
		for _, w := range warns {
			if !slices.Contains(legacyWarns, w) {
				t.Fatalf("iter %d: Option 1 added the WARN %q (rows %+v)", i, w, rows)
			}
		}
		for _, w := range legacyWarns {
			if slices.Contains(warns, w) {
				continue
			}
			exempted++
			origin := strings.Fields(w)[0]
			_, foreign := splitAuthority(byOrigin[origin])
			hasForeignCtx := false
			for _, f := range foreign {
				if f.ProviderClass == providerClassCtx {
					hasForeignCtx = true
				}
			}
			if !hasForeignCtx {
				t.Fatalf("iter %d: origin %s exempted without a foreign ctx row (rows %+v)", i, origin, byOrigin[origin])
			}
		}
	}
	if exempted == 0 {
		t.Fatal("no origin exempted in 100 000 sets with ctx rows — the generator does not reach Option 1")
	}
	t.Logf("with ctx: %d random row sets; %d origins exempted, each carrying a foreign ctx row", iters, exempted)
}
