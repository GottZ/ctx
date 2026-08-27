//go:build integration

// Wave W01-7 — the invariant guard for D-INV (design D-01 §7 W01-7, §5.2 B15,
// §4.8.3, §1.3 I1-I7) against a real PG18 testcontainer and the registry
// production actually loads.
//
// WHY THIS TEST LIVES IN package handler AND NOT IN blocktype. Clause 8 is the
// I7 write lock, and the one place that decides "may a client claim this type"
// is validateTypeNameAgainstSet (stage_gates.go:140) — unexported. A guard that
// re-implemented the predicate would assert its own copy, which is the class of
// evidence this wave exists to replace. Everything else the guard needs
// (blocktype, derived, testdb) is importable from here; the reverse is not.
//
// WHAT MAKES IT GROW WITH THE TREE. The guard never names a derived type in a
// clause. It sweeps EVERY scope that carries a registry row or a block, resolves
// the effective *Set for that scope (base for _global, base+overlay for a tenant
// — D6: the tenant row WINS, registry.go:252-267) and filters the loaded names
// through derived.StratumOf. A third derived type, a tenant that overlays an
// existing one, a new block-carrying scope: all arrive without an edit here. The
// only compiled-in names are the clause-11 anchors below, and they are a LOWER
// bound (a name StratumOf knows but no row and no block carries), self-checked
// so a rename in production turns them red instead of silently vacuous.
//
// THE TWELVE CLAUSES, each with its own negative probe (a clause without a red
// run counts as not built — the probe names what the mutation does):
//
//	 1 guard.check=false        RED: catalog row guard.check -> true
//	 2 guard.candidate=false    RED: catalog row guard.candidate -> true
//	 3 dream.linkable=false     RED: catalog row dream.linkable -> true
//	 4 overview.include=false   RED: catalog row overview.include -> true
//	 5 digest.include=false     RED: catalog row digest.include -> true
//	 6 retrieval.policy         RED: catalog row policy -> full-pass
//	 7 damping_factor in (0,1]  RED: hand-built Set, catalog damped@0.0 (the DB
//	                                route is refused one layer earlier — proved
//	                                in the same subtest)
//	 8 I7 write lock refuses    RED: clause fed a name the real gate ADMITS
//	 9 untrusted => !overview   RED: checkpoint row overview.include -> true
//	10 untrusted => !digest     RED: checkpoint row digest.include -> true
//	11 registry ROW exists      RED: catalog row DELETEd (the loaded Set still
//	                                resolves it off the compiled-in floor — that
//	                                gap IS B15)
//	12 no orphan type_name      RED: block inserted with type_name 'ghost-type'
//	 + grows_with               RED: a THIRD registry row (catalog in a tenant
//	                                scope) with overview.include=true, caught
//	                                without a line changed here
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestDerivedInvariantGuard -count=1 -v
package handler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/testdb"
)

// w017GlobalScope is the shipped registry namespace. A local literal: blocktype's
// own constant is unexported and importing store for GlobalScope would pull a
// package this test has no business in.
const w017GlobalScope = "_global"

// w017Anchors are the derived type names the compiled-in StratumOf switch knows.
// They exist for clause 11 ONLY, which has to notice a name whose row vanished —
// a name with no row and no block cannot be discovered from data. Every other
// clause enumerates from the loaded registry, so this list is a lower bound, not
// the enumeration; the anchors_are_derived subtest keeps it honest.
var w017Anchors = []string{derived.TypeInsight, derived.TypeCatalog}

// w017Clause identifies one clause of D-INV; the numbers are the design's.
type w017Clause int

const (
	w017GuardCheck w017Clause = iota + 1
	w017GuardCandidate
	w017DreamLinkable
	w017OverviewInclude
	w017DigestInclude
	w017RetrievalPolicy
	w017DampingFactor
	w017WriteLock
	w017UntrustedOverview
	w017UntrustedDigest
	w017RegistryRow
	w017CorpusOrphan
)

// w017Finding is one violation: which clause, in which scope, about which type.
type w017Finding struct {
	Clause w017Clause
	Scope  string
	Type   string
	Detail string
}

func (f w017Finding) String() string {
	return fmt.Sprintf("clause %d [scope %s, type %s]: %s", f.Clause, f.Scope, f.Type, f.Detail)
}

// w017Subject is one scope's view of the world: the effective registry Set, the
// registry ROWS (which the Set deliberately does not tell apart from the
// compiled-in floor — clause 11) and the distinct type names of its corpus.
type w017Subject struct {
	Scope  string
	Set    *blocktype.Set
	Rows   map[string]map[string]bool // scope -> name present as a ROW
	Corpus []w017CorpusEntry
}

// w017CorpusEntry is one (scope, type_name) group of context_blocks with a
// sample id, so a finding points at a row an operator can look at.
type w017CorpusEntry struct {
	TypeName string
	SampleID string
	Count    int64
}

// w017DerivedNames returns the loaded names that belong to the derived layer.
// This is the whole "grows with it" mechanism: data in, clauses out.
func w017DerivedNames(set *blocktype.Set) []string {
	var out []string
	for _, n := range set.Names() {
		if derived.StratumOf(n) > derived.StratumSource {
			out = append(out, n)
		}
	}
	return out
}

// w017Check runs all twelve clauses over one scope.
func w017Check(sub w017Subject) []w017Finding {
	f := w017CheckTypes(sub, w017DerivedNames(sub.Set))
	f = append(f, w017CheckRegistryWide(sub)...)
	f = append(f, w017CheckRows(sub)...)
	return append(f, w017CheckCorpus(sub)...)
}

// w017CheckTypes runs clauses 1-8 over the named types. The name list is a
// PARAMETER so the clause-8 probe can hand it a type the real write gate admits;
// production callers pass w017DerivedNames.
func w017CheckTypes(sub w017Subject, names []string) []w017Finding {
	var out []w017Finding
	add := func(c w017Clause, name, detail string) {
		out = append(out, w017Finding{Clause: c, Scope: sub.Scope, Type: name, Detail: detail})
	}
	for _, name := range names {
		p, ok := sub.Set.Resolve(name)
		if !ok {
			add(w017RegistryRow, name, "not resolvable in the loaded registry")
			continue
		}
		if p.Guard.Check {
			add(w017GuardCheck, name, "guard.check=true — a derivative would enter the dedup batch (I4)")
		}
		if p.Guard.Candidate {
			add(w017GuardCandidate, name, "guard.candidate=true — a derivative could archive its own source (B1/I4)")
		}
		if p.Dream.Linkable {
			add(w017DreamLinkable, name, "dream.linkable=true — dream links are Louvain's only input (I4)")
		}
		if p.Overview.Include {
			add(w017OverviewInclude, name, "overview.include=true — the topic map would derive from itself (§0/K1, M20)")
		}
		if p.Digest.Include {
			add(w017DigestInclude, name, "digest.include=true — the topic map would derive from itself (I4)")
		}
		w017CheckVisibility(p, add)
		if rej := validateTypeNameAgainstSet(sub.Set, name); rej == nil || rej.Code != classReservedType.code {
			add(w017WriteLock, name, fmt.Sprintf(
				"the client write path does NOT refuse this type with %s (got %v) — every other clause is "+
					"writer convention while a client can claim the type (I7)", classReservedType.code, rej))
		}
	}
	return out
}

// w017CheckVisibility is clauses 6 and 7, and it is the one clause that departs
// from the §7 wording. §7 asks for retrieval.policy != "excluded" — that text
// describes the state AFTER the E-4 visibility switch. Masterplan K7, confirmed
// on the board as E-4, has BOTH derived types START excluded until the pilots
// (X-W4/X-W5/W-C3b) and then move to a MEASURED damping factor (sweep M-W8).
// Pinning the § wording today would make the guard red against the very state
// the board decided, so the clause is state-aware instead: excluded is the
// today-branch, damped is the after-branch and then the factor must be in (0,1],
// and full-pass / aggregate-to-parent are red in BOTH states — those are the
// positions K7 called irreversible. Deviation recorded in reports/bau/w01-7.md.
func w017CheckVisibility(p blocktype.Policy, add func(w017Clause, string, string)) {
	switch p.Retrieval.Kind {
	case blocktype.RetrievalExcluded:
		// K7/E-4 start state: nothing else to check, damping is not in play.
	case blocktype.RetrievalDamped:
		if f := p.Retrieval.DampingFactor; f <= 0 || f > 1 {
			add(w017DampingFactor, p.Name, fmt.Sprintf(
				"retrieval.damping_factor=%v is outside (0,1] on a damped derived type", f))
		}
	default:
		add(w017RetrievalPolicy, p.Name, fmt.Sprintf(
			"retrieval.policy=%q — a derived type is excluded (K7/E-4 start) or damped (after the "+
				"E-4 switch); full-pass and aggregate-to-parent are not reversible data positions",
			p.Retrieval.Kind))
	}
}

// w017CheckRegistryWide is clauses 9 and 10 over the WHOLE registry, not only
// the derived names: they close the premise §4.8.3 rests on ("no untrusted type
// has overview.include=true"), which nothing else makes red.
func w017CheckRegistryWide(sub w017Subject) []w017Finding {
	var out []w017Finding
	for _, name := range sub.Set.Names() {
		p, ok := sub.Set.Resolve(name)
		if !ok || !p.Retrieval.Untrusted {
			continue
		}
		if p.Overview.Include {
			out = append(out, w017Finding{w017UntrustedOverview, sub.Scope, name,
				"retrieval.untrusted=true AND overview.include=true — foreign text becomes a cluster node (§4.8.3, V11)"})
		}
		if p.Digest.Include {
			out = append(out, w017Finding{w017UntrustedDigest, sub.Scope, name,
				"retrieval.untrusted=true AND digest.include=true — foreign text feeds the topic map (§4.8.3, V11)"})
		}
	}
	return out
}

// w017CheckRows is clause 11 (B15): a derived type must exist as a registry ROW,
// not only in the compiled-in floor. Reload MERGES builtinPolicies() over the
// table (registry.go:393-394 seeds the floor, :410-411 lets the row win), so a
// DELETEd row still resolves — which is exactly why "the Set knows the name" is
// not evidence. A tenant scope satisfies the clause through its own row OR
// through _global (the overlay adds, it never removes).
func w017CheckRows(sub w017Subject) []w017Finding {
	var out []w017Finding
	for _, name := range w017DerivedUniverse(sub.Set) {
		if _, ok := sub.Set.Resolve(name); !ok {
			out = append(out, w017Finding{w017RegistryRow, sub.Scope, name,
				"derived type is not resolvable in the loaded registry at all"})
			continue
		}
		if sub.Rows[sub.Scope][name] || sub.Rows[w017GlobalScope][name] {
			continue
		}
		where := fmt.Sprintf("scope %q nor in %q", sub.Scope, w017GlobalScope)
		if sub.Scope == w017GlobalScope {
			where = fmt.Sprintf("scope %q", w017GlobalScope)
		}
		out = append(out, w017Finding{w017RegistryRow, sub.Scope, name, fmt.Sprintf(
			"no context_block_types row in %s — only the compiled-in floor carries the type; "+
				"type_name has no FK and sweepOrphans (registry.go:464) only warns (B15)", where)})
	}
	return out
}

// w017DerivedUniverse is the clause-11 name universe: the anchors plus every
// derived name the loaded registry carries. Sorted, deduped.
func w017DerivedUniverse(set *blocktype.Set) []string {
	seen := map[string]bool{}
	for _, n := range w017Anchors {
		seen[n] = true
	}
	for _, n := range w017DerivedNames(set) {
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// w017CheckCorpus is clause 12 — the W01-5 review finding #6 made data-side:
// Set.IsUntrusted answers false for a name it does not know (set.go:214-217), so
// a source whose registry row vanished reaches ResolveSources as trusted and V11
// never sees it. Clause 11 cannot catch that — the untrusted SOURCE types carry
// stratum 0. This one does: every type_name occurring in the corpus must resolve
// in the registry of that block's scope.
func w017CheckCorpus(sub w017Subject) []w017Finding {
	var out []w017Finding
	for _, e := range sub.Corpus {
		if e.TypeName == "" {
			continue
		}
		if _, ok := sub.Set.Resolve(e.TypeName); ok {
			continue
		}
		out = append(out, w017Finding{w017CorpusOrphan, sub.Scope, e.TypeName, fmt.Sprintf(
			"%d block(s) carry this type_name (sample %s) and the registry of scope %q does not know it — "+
				"IsUntrusted answers false for them (review #6, B15)", e.Count, e.SampleID, sub.Scope)})
	}
	return out
}

// w017Subjects boots a registry off the migrated DB and builds one subject per
// scope that carries a registry row or a block. The base generation serves
// _global and every reserved (_-prefixed) scope; every other scope gets its
// tenant generation.
func w017Subjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []w017Subject {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s — the guard would measure the compiled-in floor, not the live registry",
			reg.Health())
	}
	rows := w017RegistryRows(t, ctx, pool)
	corpus := w017Corpus(t, ctx, pool)

	scopes := map[string]bool{w017GlobalScope: true}
	for s := range rows {
		scopes[s] = true
	}
	for s := range corpus {
		scopes[s] = true
	}
	names := make([]string, 0, len(scopes))
	for s := range scopes {
		names = append(names, s)
	}
	sort.Strings(names)

	out := make([]w017Subject, 0, len(names))
	for _, s := range names {
		set := reg.Snapshot()
		if !strings.HasPrefix(s, "_") {
			set = reg.SnapshotForTenant(ctx, s)
		}
		out = append(out, w017Subject{Scope: s, Set: set, Rows: rows, Corpus: corpus[s]})
	}
	return out
}

// w017RegistryRows reads the registry TABLE — scope -> name -> present.
func w017RegistryRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT scope, name FROM context_block_types`)
	if err != nil {
		t.Fatalf("read context_block_types: %v", err)
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var scope, name string
		if err := rows.Scan(&scope, &name); err != nil {
			t.Fatalf("scan registry row: %v", err)
		}
		if out[scope] == nil {
			out[scope] = map[string]bool{}
		}
		out[scope][name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate registry rows: %v", err)
	}
	return out
}

// w017Corpus reads the distinct (scope, type_name) groups of context_blocks with
// a sample id and a count.
func w017Corpus(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string][]w017CorpusEntry {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT scope, COALESCE(type_name, ''), MIN(id::text), COUNT(*)
		   FROM context_blocks
		  GROUP BY scope, COALESCE(type_name, '')
		  ORDER BY scope, 2`)
	if err != nil {
		t.Fatalf("read context_blocks type census: %v", err)
	}
	defer rows.Close()
	out := map[string][]w017CorpusEntry{}
	for rows.Next() {
		var scope string
		var e w017CorpusEntry
		if err := rows.Scan(&scope, &e.TypeName, &e.SampleID, &e.Count); err != nil {
			t.Fatalf("scan type census: %v", err)
		}
		out[scope] = append(out[scope], e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate type census: %v", err)
	}
	return out
}

// w017Sweep runs the guard over every subject and returns every finding.
func w017Sweep(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []w017Finding {
	t.Helper()
	var out []w017Finding
	for _, sub := range w017Subjects(t, ctx, pool) {
		out = append(out, w017Check(sub)...)
	}
	return out
}

// w017Exec runs one mutation statement.
func w017Exec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("probe mutation failed: %v\n%s", err, sql)
	}
}

// w017SetField is the mutation shape every registry probe uses: overlay ONE
// field into ONE config section, creating the section if the row has none.
// jsonb_set alone is a no-op on an absent section.
func w017SetField(name, scope, section, field, value string) string {
	return fmt.Sprintf(
		`UPDATE context_block_types
		    SET config = jsonb_set(config, '{%s}', COALESCE(config->'%s', '{}'::jsonb) || '{"%s":%s}'::jsonb)
		  WHERE name = '%s' AND scope = '%s'`, section, section, field, value, name, scope)
}

// w017Seed plants a small valid corpus so clause 12 has something to say on the
// green run: an ordinary block, a derived one and an untrusted one.
const w017Seed = `INSERT INTO context_blocks (category, title, content, scope, type_name) VALUES
	('knowledge', 'W01-7 anchor block',      'anchor',     'private', 'knowledge'),
	('catalog',   'Katalog #w017probe',      'derivative', 'private', 'catalog'),
	('reference', 'Compaction source w01-7', 'evidence',   'private', 'checkpoint')`

// w017Filter returns the findings of one clause.
func w017Filter(f []w017Finding, c w017Clause) []w017Finding {
	var out []w017Finding
	for _, v := range f {
		if v.Clause == c {
			out = append(out, v)
		}
	}
	return out
}

// TestDerivedInvariantGuard_Integration is the wave. The green sweep is the
// gate; the probes are the proof that each clause can fail.
func TestDerivedInvariantGuard_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	t.Run("anchors_are_derived", func(t *testing.T) {
		// The clause-11 anchor list is compiled-in and would go silently vacuous
		// if production renamed a derived type. This makes that red.
		for _, n := range w017Anchors {
			if derived.StratumOf(n) <= derived.StratumSource {
				t.Errorf("anchor %q has stratum %d — the clause-11 lower bound no longer names a derived type",
					n, derived.StratumOf(n))
			}
		}
	})

	t.Run("green_full_chain", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		w017Exec(t, ctx, pool, w017Seed)
		subs := w017Subjects(t, ctx, pool)
		var findings []w017Finding
		for _, sub := range subs {
			names := w017DerivedNames(sub.Set)
			for _, n := range names {
				p, _ := sub.Set.Resolve(n)
				t.Logf("scope %s: derived type %q retrieval.policy=%q damping=%v untrusted=%v",
					sub.Scope, n, p.Retrieval.Kind, p.Retrieval.DampingFactor, p.Retrieval.Untrusted)
			}
			if len(names) == 0 && sub.Scope == w017GlobalScope {
				t.Fatal("no derived type in the loaded _global registry — the guard would assert nothing")
			}
			findings = append(findings, w017Check(sub)...)
		}
		for _, f := range findings {
			t.Errorf("D-INV violated at the Ist-Stand: %s", f)
		}
		t.Logf("swept %d scope(s), %d finding(s)", len(subs), len(findings))
	})

	t.Run("probes", func(t *testing.T) {
		for _, p := range []struct {
			name   string
			clause w017Clause
			sql    string
		}{
			{"c01_guard_check", w017GuardCheck, w017SetField("catalog", w017GlobalScope, "guard", "check", "true")},
			{"c02_guard_candidate", w017GuardCandidate, w017SetField("catalog", w017GlobalScope, "guard", "candidate", "true")},
			{"c03_dream_linkable", w017DreamLinkable, w017SetField("catalog", w017GlobalScope, "dream", "linkable", "true")},
			{"c04_overview_include", w017OverviewInclude, w017SetField("catalog", w017GlobalScope, "overview", "include", "true")},
			{"c05_digest_include", w017DigestInclude, w017SetField("catalog", w017GlobalScope, "digest", "include", "true")},
			{"c06_retrieval_policy", w017RetrievalPolicy, w017SetField("catalog", w017GlobalScope, "retrieval", "policy", `"full-pass"`)},
			{"c09_untrusted_overview", w017UntrustedOverview, w017SetField("checkpoint", w017GlobalScope, "overview", "include", "true")},
			{"c10_untrusted_digest", w017UntrustedDigest, w017SetField("checkpoint", w017GlobalScope, "digest", "include", "true")},
			{"c11_registry_row", w017RegistryRow,
				`DELETE FROM context_block_types WHERE name = 'catalog' AND scope = '_global'`},
			{"c12_corpus_orphan", w017CorpusOrphan,
				`INSERT INTO context_blocks (category, title, content, scope, type_name)
				 VALUES ('reference', 'W01-7 orphan probe', 'x', 'private', 'ghost-type')`},
		} {
			t.Run(p.name, func(t *testing.T) {
				pool := testdb.SetupTestDB(t)
				w017Exec(t, ctx, pool, w017Seed)
				if got := w017Filter(w017Sweep(t, ctx, pool), p.clause); len(got) != 0 {
					t.Fatalf("clause %d already red BEFORE the probe: %v — the red state is not the red state",
						p.clause, got)
				}
				w017Exec(t, ctx, pool, p.sql)
				got := w017Filter(w017Sweep(t, ctx, pool), p.clause)
				if len(got) == 0 {
					t.Fatalf("clause %d stayed green against a copy that violates it — the clause is not built", p.clause)
				}
				for _, f := range got {
					t.Logf("RED %s", f)
				}
			})
		}
	})

	t.Run("c07_damping_factor", func(t *testing.T) {
		// The DB route is closed one layer earlier and that is worth proving
		// rather than assuming: validatePolicy (policy.go:529) refuses damped
		// with a factor outside (0,1], and Reload turns a corrupt row into a
		// failed reload (registry.go:407-409) — the registry degrades and keeps
		// the previous snapshot. So the probe hands the clause the state the DB
		// cannot carry: a hand-built Set whose catalog policy is damped at 0.0.
		pool := testdb.SetupTestDB(t)
		w017Exec(t, ctx, pool, w017SetField("catalog", w017GlobalScope, "retrieval", "policy", `"damped"`))
		w017Exec(t, ctx, pool, w017SetField("catalog", w017GlobalScope, "retrieval", "damping_factor", "0"))
		dctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		reg := blocktype.NewRegistry()
		reg.Boot(dctx, pool)
		if reg.Health() == blocktype.HealthOK {
			t.Errorf("a damped catalog row with damping_factor=0 loaded cleanly — validatePolicy no longer "+
				"guards the factor, and clause %d is the only remaining line", w017DampingFactor)
		} else {
			t.Logf("DB route refused at decode: registry health = %s", reg.Health())
		}

		rows := map[string]map[string]bool{
			w017GlobalScope: {derived.TypeInsight: true, derived.TypeCatalog: true},
		}
		sub := w017Subject{Scope: w017GlobalScope, Set: w017DampedSet(t, 0), Rows: rows}
		got := w017Filter(w017Check(sub), w017DampingFactor)
		if len(got) == 0 {
			t.Fatalf("clause %d stayed green on a damped derived type with factor 0.0", w017DampingFactor)
		}
		for _, f := range got {
			t.Logf("RED %s", f)
		}
		// And the after-E-4 state the clause must ACCEPT, so the guard does not
		// block the visibility switch it was written to survive.
		ok := w017Subject{Scope: w017GlobalScope, Set: w017DampedSet(t, 0.5), Rows: rows}
		if f := w017Filter(w017Check(ok), w017DampingFactor); len(f) != 0 {
			t.Errorf("clause %d fired on a legitimate damped factor 0.5: %v", w017DampingFactor, f)
		}
	})

	t.Run("c08_write_lock", func(t *testing.T) {
		// The gate hangs on derived.StratumOf, a compiled-in mapping, so no DB
		// state can make the real gate admit a derived name. The probe therefore
		// feeds the clause the state it exists to catch — a name in the derived
		// list that the gate ADMITS. tool-evidence is the cleanest carrier: it
		// passes clauses 1-7 (guard/dream/digest/overview all false, damped at
		// 0.15) and is not derived, so exactly one clause may fire.
		pool := testdb.SetupTestDB(t)
		var base w017Subject
		for _, s := range w017Subjects(t, ctx, pool) {
			if s.Scope == w017GlobalScope {
				base = s
			}
		}
		if base.Set == nil {
			t.Fatal("no _global subject")
		}
		got := w017CheckTypes(base, []string{"tool-evidence"})
		if len(got) != 1 || got[0].Clause != w017WriteLock {
			t.Fatalf("clause %d probe produced %v, want exactly one write-lock finding", w017WriteLock, got)
		}
		t.Logf("RED %s", got[0])
		// GREEN against the real gate: every loaded derived type is refused.
		for _, n := range w017DerivedNames(base.Set) {
			rej := validateTypeNameAgainstSet(base.Set, n)
			if rej == nil || rej.Code != classReservedType.code {
				t.Errorf("write path does not refuse %q with %s (got %v)", n, classReservedType.code, rej)
			}
		}
	})

	t.Run("grows_with_a_third_row", func(t *testing.T) {
		// A THIRD registry row bearing a derived name, in a tenant scope the
		// guard was never told about. buildTenantSet merges it over the base and
		// the tenant row WINS (D6, registry.go:252-267), so this is a live path
		// for defeating the invariant per tenant — and the guard reaches it
		// without a line changed here, because it sweeps the scopes it FINDS.
		pool := testdb.SetupTestDB(t)
		w017Exec(t, ctx, pool, `INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
			VALUES ('catalog', 'w017probe', 'Tenant-Katalog', false, false,
			        '{"v":1,"retrieval":{"policy":"excluded"},"guard":{"check":false,"candidate":false},
			          "dream":{"linkable":false},"digest":{"include":false},"overview":{"include":true}}'::jsonb)`)
		got := w017Filter(w017Sweep(t, ctx, pool), w017OverviewInclude)
		if len(got) == 0 {
			t.Fatal("a tenant-scope catalog row with overview.include=true passed the sweep — the guard only " +
				"looks at _global and a third derived row escapes it")
		}
		for _, f := range got {
			if f.Scope != "w017probe" {
				t.Errorf("finding attributed to scope %q, want the tenant scope: %s", f.Scope, f)
			}
			t.Logf("RED %s", f)
		}
	})
}

// w017DampedSet hand-builds a minimal registry Set with a damped catalog policy
// at the given factor. NewSet performs no cross-field validation — which is what
// lets this express the state DecodePolicy refuses.
func w017DampedSet(t *testing.T, factor float64) *blocktype.Set {
	t.Helper()
	set, err := blocktype.NewSet([]blocktype.Policy{
		{
			Name: "knowledge", Scope: w017GlobalScope, Builtin: true, IsDefault: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
			Guard: blocktype.GuardPolicy{
				Check: true, Candidate: true,
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
			Dream:  blocktype.DreamPolicy{Linkable: true},
			Digest: blocktype.DigestPolicy{Include: true},
		},
		{
			Name: derived.TypeCatalog, Scope: w017GlobalScope, Builtin: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalDamped, DampingFactor: factor},
			Guard: blocktype.GuardPolicy{
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
		},
		{
			Name: derived.TypeInsight, Scope: w017GlobalScope, Builtin: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalExcluded, Untrusted: true},
			Guard: blocktype.GuardPolicy{
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
		},
	})
	if err != nil {
		t.Fatalf("build probe set: %v", err)
	}
	return set
}
