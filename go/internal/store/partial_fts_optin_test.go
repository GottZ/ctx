package store

import (
	"strings"
	"testing"
)

// C2-2 (OPS-W1 review A2/A3) — the pure half of the gate: which statements
// DECLARE the predicate of the partial FTS GIN indexes and which must not.
//
// Migration 145 made idx_context_ts_de / idx_context_ts_en partial over
// `type_name NOT IN ('checkpoint','system-meta')`. A query only reaches such an
// index if the planner can PROVE the index predicate from the query's own
// quals. A bind parameter (`type_name = ANY($5)`) carries no proof under a
// GENERIC plan — and pgx runs the extended protocol with a statement cache, so
// the generic plan is reachable from the 6th execution per connection
// (plancache.c choose_custom_plan). The static conjunct is therefore what makes
// index usage independent of a per-connection cache decision.
//
// The literals in this file are spelled out on purpose: they must break when
// the production spelling drifts away from the index predicate, not travel
// with it.
const (
	c22SearchBlocksConjunct = `type_name NOT IN ('checkpoint','system-meta')`
	c22IssueConjunct        = `AND b.type_name NOT IN ('checkpoint','system-meta')`
)

// c22DenyNames is the deny-list as migration 145 froze it (145:300,334).
var c22DenyNames = []string{"checkpoint", "system-meta"}

// TestC22DenyListShapesAgree closes the one drift risk the fix introduces: the
// deny-list lives as a Go slice (the implication rule) AND as SQL text (the
// statements). Neither is derived from the other at runtime — the statements are
// consts — so the agreement is asserted here instead. A name added to one shape
// and forgotten in the other turns a silent half-fix into a red test.
func TestC22DenyListShapesAgree(t *testing.T) {
	if len(hardFTSDenyTypes) != len(c22DenyNames) {
		t.Fatalf("deny-list = %v, want %v", hardFTSDenyTypes, c22DenyNames)
	}
	for i, name := range c22DenyNames {
		if hardFTSDenyTypes[i] != name {
			t.Fatalf("deny-list = %v, want %v", hardFTSDenyTypes, c22DenyNames)
		}
	}
	wantValues := "('" + strings.Join(hardFTSDenyTypes, "','") + "')"
	if hardFTSDenyValues != wantValues {
		t.Errorf("hardFTSDenyValues = %s, want %s (SQL shape drifted from the Go shape)", hardFTSDenyValues, wantValues)
	}
	if hardFTSDenyConjunct != c22SearchBlocksConjunct {
		t.Errorf("hardFTSDenyConjunct = %s, want %s", hardFTSDenyConjunct, c22SearchBlocksConjunct)
	}
}

// TestC22SearchBlocksDeclaresIndexPredicateOnTypeOptIn pins the A2 rule: the FTS
// branch of the browse statement declares the deny-list conjunct exactly when
// the caller's own opt-in type filter already implies it — never otherwise.
//
// "Implies" has two shapes, both decidable in Go because both filters are known
// values at build time:
//
//   - types = [...] without any deny-listed name: `type_name = ANY(list)` already
//     restricts the rows to types outside the deny-list;
//   - typesExclude ⊇ {checkpoint, system-meta}: `NOT (type_name = ANY(list))`
//     does the same.
//
// Where neither holds the conjunct MUST stay away: adding it unconditionally
// would silently drop deny-listed blocks from /api/context/search and `ctx
// search`, which is the D5 asymmetry this layer deliberately keeps (browse
// stays complete, only retrieval ranks those types out — blocks.go:1276-1282).
func TestC22SearchBlocksDeclaresIndexPredicateOnTypeOptIn(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		types        []string
		typesExclude []string
		want         bool
	}{
		{name: "fts + types opt-in", query: "needle", types: []string{"knowledge", "reference"}, want: true},
		{name: "fts + single type opt-in", query: "needle", types: []string{"knowledge"}, want: true},
		{name: "fts + typesExclude covers the deny-list", query: "needle", typesExclude: []string{"checkpoint", "system-meta"}, want: true},
		{name: "fts + typesExclude covers deny-list and more", query: "needle", typesExclude: []string{"checkpoint", "system-meta", "audit-trail"}, want: true},
		{name: "fts + types and typesExclude", query: "needle", types: []string{"knowledge"}, typesExclude: []string{"issue"}, want: true},

		{name: "fts without any type filter", query: "needle", want: false},
		{name: "fts + types asks for a deny-listed type", query: "needle", types: []string{"knowledge", "checkpoint"}, want: false},
		{name: "fts + types is exactly a deny-listed type", query: "needle", types: []string{"checkpoint"}, want: false},
		{name: "fts + typesExclude covers only half the deny-list", query: "needle", typesExclude: []string{"checkpoint"}, want: false},
		{name: "fts + typesExclude is unrelated", query: "needle", typesExclude: []string{"issue"}, want: false},

		// The browse path (no q) never touches the FTS indexes — nothing to
		// declare, and the keyset plan must stay byte-identical.
		{name: "browse + types opt-in (no fts)", types: []string{"knowledge", "reference"}, want: false},
		{name: "browse without filters", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, _ := searchBlocksSQL(searchParams{
				Query: tc.query, ReadScopes: []string{"private"}, Limit: 20, Compact: true,
				Types: tc.types, TypesExclude: tc.typesExclude,
			})
			got := strings.Count(sql, c22SearchBlocksConjunct)
			if tc.want && got != 1 {
				t.Errorf("statement declares the index predicate %d times, want exactly 1\nSQL: %s", got, sql)
			}
			if !tc.want && got != 0 {
				t.Errorf("statement declares the index predicate %d times, want 0 (result set would change)\nSQL: %s", got, sql)
			}
		})
	}
}

// TestC22SearchBlocksOptInKeepsBindArgs guards the mechanical risk of the A2
// fix: the conjunct is STATIC text, so it must not consume a $-index. The FTS
// ORDER BY hardcodes the query arg at $2 and the grant uuid[] is allocated after
// every dynamic arg — a shifted index would either mis-rank or bind the wrong
// array (blocks.go:1332-1337, 1407-1414).
func TestC22SearchBlocksOptInKeepsBindArgs(t *testing.T) {
	base, baseArgs := searchBlocksSQL(searchParams{
		Query: "needle", ReadScopes: []string{"private"}, Limit: 20, Compact: true,
		Types: []string{"checkpoint"}, // no opt-in: no conjunct
	})
	optIn, optInArgs := searchBlocksSQL(searchParams{
		Query: "needle", ReadScopes: []string{"private"}, Limit: 20, Compact: true,
		Types: []string{"knowledge"}, // opt-in: conjunct
	})
	if len(baseArgs) != len(optInArgs) {
		t.Fatalf("opt-in changed the arg count: %d vs %d", len(baseArgs), len(optInArgs))
	}
	stripped := strings.Replace(optIn, " AND "+c22SearchBlocksConjunct, "", 1)
	if stripped == optIn {
		t.Fatalf("opt-in statement does not carry the conjunct at all:\n%s", optIn)
	}
	// The type list is a BIND parameter, so both statements are the same
	// characters apart from the static conjunct — every $-index in place.
	if stripped != base {
		t.Errorf("opt-in statement differs beyond the static conjunct:\ngot:  %s\nwant: %s", stripped, base)
	}
}

// TestC22IssueFTSDeclaresIndexPredicate pins the A3 half: both issue FTS
// statements declare the deny-list conjunct, and the two non-FTS issue
// statements do NOT (they never touch a tsvector index — a conjunct there would
// be dead weight on the board/created paths).
func TestC22IssueFTSDeclaresIndexPredicate(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want int
	}{
		{"IssueSearchUpdatedSQL", IssueSearchUpdatedSQL, 1},
		{"IssueSearchCreatedSQL", IssueSearchCreatedSQL, 1},
		{"IssueCreatedListSQL", IssueCreatedListSQL, 0},
		{"IssueBoardCountSQL", IssueBoardCountSQL, 0},
	} {
		if got := strings.Count(tc.sql, c22IssueConjunct); got != tc.want {
			t.Errorf("%s declares the index predicate %d times, want %d", tc.name, got, tc.want)
		}
	}
}

// TestC22IssueFTSConjunctIsRedundant is the semantic no-op proof for A3, in Go
// rather than in data: both statements bind type_name to IssueTypeName at their
// single call site (issues_read.go:210), and that name is not on the deny-list.
// A conjunct that is implied by an equality already in the same WHERE clause
// cannot change the row set. If IssueTypeName ever became a deny-listed name,
// this test turns the silent result-set change into a red test.
func TestC22IssueFTSConjunctIsRedundant(t *testing.T) {
	for _, deny := range c22DenyNames {
		if IssueTypeName == deny {
			t.Fatalf("IssueTypeName %q is on the hard deny-list — the A3 conjunct would empty the issue FTS result set", IssueTypeName)
		}
	}
	for _, sql := range []string{IssueSearchUpdatedSQL, IssueSearchCreatedSQL} {
		if !strings.Contains(sql, "b.type_name = $2") {
			t.Errorf("issue FTS statement no longer pins type_name to the bound $2 — the redundancy argument for the deny conjunct is gone:\n%s", sql)
		}
	}
}
