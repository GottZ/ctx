package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
)

// rowsQuerier is the minimal pgx surface ResolveSources reads through —
// satisfied by BOTH *pgxpool.Pool and pgx.Tx, so the arm can resolve inside the
// transaction that later writes the derivative (the resolve result and the
// write then see the same snapshot) or stand alone on the pool. Read-only, so
// no Exec: the same one-level-narrower shape rrf.Querier (arms.go:18-20) uses.
type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SensitivityFloor is config.ScopeFloor.Apply, handed in as a function value.
//
// store must not import internal/config — config imports store, so the edge
// would be a cycle (`go list -deps ./internal/config` pulls store, llm,
// blocktype, rrf, visibility, migrations). The precedent for passing the floor
// instead of the map is topiclabel.Deps.Floor (topiclabel.go:133-135, wired at
// events/topic_label.go:100 and events/scheduler.go:609 as
// cfg.Pool.ScopeSensitivityFloor.Apply).
//
// Unlike there, a nil floor is NOT "no floor" here — see ResolveSources.
type SensitivityFloor func(backends.Sensitivity, string) backends.Sensitivity

// sourceInfo is one resolved source block, in the shape the two consumers of a
// resolve result need: the gate wants title/content/sensitivity, the validator
// wants stratum/untrusted/sensitivity. Unexported, like every field of
// SourceSet — see there.
type sourceInfo struct {
	title       string
	content     string
	sensitivity string
	typeName    string
	stratum     derived.Stratum
	untrusted   bool
}

// SourceSet is the ONE resolve result of design/01 §4.5.4 — Found plus the two
// separated failure sets — and it is the single origin of everything the
// derived contract says about sources.
//
// EVERY field is unexported, and that is the mechanism, not a style choice.
// Three obligations from the W01-1 review land on this type:
//
//   - #6: none of V13's three clauses can detect whether ScopeFloor.Apply ran,
//     because the floor only ever raises — a caller that skips it and reports
//     FlooredMax = max(raw) passes all three. The guarantee is therefore
//     structural: FlooredMax is produced HERE, by the one function that applies
//     the floor, and nothing outside this file can write it.
//   - #7: CiteGate's source map and Validate's SourceFacts must come from the
//     SAME resolution. Sources() and Facts() are two views of one SourceSet, so
//     a source cannot be present for the validator and absent from the echo
//     index (which is how a credentials title escapes G7).
//   - N4: MissingInScope and ForeignOrUnknown were caller CLAIMS. A caller that
//     declares every source missing switches V6, V11 and the monotonicity half
//     of V13 off, because checkFactsCoverDeclared accepts "reported
//     unresolvable" as the legal way to carry no facts. Here the two sets are
//     the outcome of the two queries and cannot be asserted.
//
// The zero value is inert: no sources, no claims, and an EMPTY FlooredMax that
// fails V10/V13 rather than validating. A SourceSet built by a caller (the only
// literal they can write is SourceSet{}) therefore validates nothing.
type SourceSet struct {
	found            map[string]sourceInfo
	missingInScope   []string
	foreignOrUnknown []string
	flooredMax       string
	scope            string
}

// Sources renders the resolve result for derived.CiteGate. The map carries the
// ORIGINAL text of every resolvable source (§4.4.2 rule 1) and the sensitivity
// that decides whether a title feeds the echo index (G7).
func (s SourceSet) Sources() map[string]derived.Source {
	out := make(map[string]derived.Source, len(s.found))
	for id, info := range s.found {
		out[id] = derived.Source{
			Title:       info.title,
			Content:     info.content,
			Sensitivity: info.sensitivity,
		}
	}
	return out
}

// Facts renders the same resolve result for derived.Validate. It is the ONLY
// caller of derived.NewSourceFacts in the tree (pinned over the syntax tree by
// TestSourceFactsHaveExactlyOneProducer), which is what makes FlooredMax and the
// two failure sets values the contract can rely on instead of caller
// assertions. NewSourceFacts copies what it is handed, so nothing a consumer
// does afterwards reaches back into this resolve result.
func (s SourceSet) Facts() derived.SourceFacts {
	strata := make(map[string]derived.Stratum, len(s.found))
	untrusted := make(map[string]bool, len(s.found))
	sensitivity := make(map[string]string, len(s.found))
	for id, info := range s.found {
		strata[id] = info.stratum
		untrusted[id] = info.untrusted
		sensitivity[id] = info.sensitivity
	}
	return derived.NewSourceFacts(strata, untrusted, sensitivity, s.flooredMax, s.missingInScope, s.foreignOrUnknown)
}

// ResolvedIDs returns the ids that resolved, sorted ascending and deduplicated
// — the exact shape provenance.source_block_ids has to have (V3 checks strictly
// ascending, V4 hashes the sorted join). MissingInScope ids are NOT in it: a
// source the arm drops must also leave the declaration, otherwise
// coverage.sources_covered and source_count describe different sets.
func (s SourceSet) ResolvedIDs() []string {
	ids := make([]string, 0, len(s.found))
	for id := range s.found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// MissingInScope are the declared ids that are provably archived or gone in the
// OWN scope. §4.5.4/§4.7.5 case 1: the arm drops them, sources_covered falls,
// the run continues — explicitly NOT a V5 case.
func (s SourceSet) MissingInScope() []string { return append([]string(nil), s.missingInScope...) }

// ForeignOrUnknown are the declared ids that resolve in a FOREIGN scope or
// nowhere at all. V5, fail-closed: the write dies (B7).
func (s SourceSet) ForeignOrUnknown() []string {
	return append([]string(nil), s.foreignOrUnknown...)
}

// FlooredMax is ScopeFloor.Apply(max(sensitivity(resolved sources)), scope) —
// the value that goes into provenance.sensitivity_max, into the written
// sensitivity column (§4.8.1) AND into ChainCall.Required (§4.8.1a). One value,
// three uses, one producer.
func (s SourceSet) FlooredMax() string { return s.flooredMax }

// UntrustedCount is provenance.untrusted_sources (§4.8.3): how many resolved
// sources carry retrieval.untrusted on their TYPE. Counted off the same
// registry lookup the V11 facts come from, so the number in the metadata and
// the flag the validator sees can never disagree.
func (s SourceSet) UntrustedCount() int {
	n := 0
	for _, info := range s.found {
		if info.untrusted {
			n++
		}
	}
	return n
}

// Len is the number of resolved sources — the value coverage.sources_covered is
// bounded by and the one §4.7.5 compares against derived.MinSourceCount.
func (s SourceSet) Len() int { return len(s.found) }

// Scope is the scope the resolution ran in. A derivative is always written in
// the scope of its sources (§4.8.4).
func (s SourceSet) Scope() string { return s.scope }

// resolveFoundSQL is query one of §4.5.4: the scope-filtered PK scan. It reads
// the block DATA, so `scope = $2` is not optional — an unfiltered read would
// pull foreign-scope content into the process before anything decides whether
// it may be cited, and Modell C is feature-complete even where everything is
// `private` today.
//
// stratum comes out of the block's own provenance rather than from the type
// name: StratumOf says where a WRITER of that type starts, the block says what
// it IS (W01-1 §6), and V6 compares blocks.
//
// Scale: `id = ANY(...)` on the primary key is an index scan bounded by the
// number of sources (live maximum cluster: 147), not by the corpus — the shape
// stays flat at 1M and 10M blocks.
const resolveFoundSQL = `
	SELECT id::text,
	       title,
	       content,
	       sensitivity,
	       COALESCE(type_name, ''),
	       COALESCE(metadata->'` + derived.MetadataKey + `'->>'stratum', '0')::int
	  FROM context_blocks
	 WHERE id = ANY($1::uuid[])
	   AND NOT is_archived
	   AND scope = $2`

// resolveRestSQL is query two: the SAME ids, WITHOUT the scope filter and
// WITHOUT the archive filter, reading id and scope ONLY — never content.
//
// This is the whole point of the two-query split (§4.5.4). The first draft had
// a single "missing" set, and it made V5 ("a source is missing ⇒ the write
// dies", fail-closed) contradict §4.7.5 case 1 ("regenerate with the rest, no
// error", fail-open) over the SAME detection. Whoever resolved that in favour
// of keeping the arm alive turned the scope check B7 into a silent swallow of
// foreign sources. Splitting the detection removes the choice: what shows up
// here in a FOREIGN scope is ForeignOrUnknown (V5), what shows up in the OWN
// scope was filtered out by NOT is_archived and is MissingInScope, and what
// does not show up at all resolves nowhere and is ForeignOrUnknown too.
const resolveRestSQL = `
	SELECT id::text, scope
	  FROM context_blocks
	 WHERE id = ANY($1::uuid[])`

// ResolveSources is the DB-facing half of the derived contract (design/01
// §4.5.4): it turns a declared id list into the ONE result that feeds both
// derived.CiteGate and derived.Validate.
//
// set resolves retrieval.untrusted per source TYPE for V11 — it falls out of
// the registry snapshot without a second query (§4.8.3).
//
// floor is config.ScopeFloor.Apply and it is REQUIRED. topiclabel treats a nil
// floor as "no floor" (topiclabel.go:595) because there the floor is an extra
// tightening of a value that is already correct. Here it is the opposite: this
// function exists to be the single place §4.8.1a is honoured, so a nil floor
// would silently reproduce exactly the gap W01-1 review #6 describes — a
// FlooredMax that equals the raw maximum and passes all three V13 clauses
// without the floor ever having run. Fail-closed instead.
func ResolveSources(ctx context.Context, q rowsQuerier, set *blocktype.Set, floor SensitivityFloor, ids []string, scope string) (SourceSet, error) {
	if floor == nil {
		return SourceSet{}, fmt.Errorf("store: resolve sources: no sensitivity floor (design/01 §4.8.1a)")
	}
	if set == nil {
		return SourceSet{}, fmt.Errorf("store: resolve sources: no block type snapshot — untrusted inheritance (V11) would be unverifiable")
	}
	if scope == "" {
		return SourceSet{}, fmt.Errorf("store: resolve sources: no scope — an unscoped source resolution crosses scopes by construction (§4.5.4)")
	}

	// Deduplicate and split off the ids that are not uuids at all. An id that
	// cannot name a row resolves NOWHERE, which is the ForeignOrUnknown half of
	// §4.5.4 — the same answer the query would give for a well-formed id that
	// matches nothing. Filtering them here rather than letting pgx raise 22P02
	// keeps a stale provenance from killing the arm with a driver error instead
	// of the contract clause that is actually about it (V5).
	wanted := make(map[string]struct{}, len(ids))
	valid := make([]string, 0, len(ids))
	var foreign []string
	for _, id := range ids {
		if _, seen := wanted[id]; seen {
			continue
		}
		wanted[id] = struct{}{}
		if _, err := uuid.Parse(id); err != nil {
			foreign = append(foreign, id)
			continue
		}
		valid = append(valid, id)
	}

	s := SourceSet{found: make(map[string]sourceInfo, len(valid)), scope: scope}
	if len(valid) > 0 {
		if err := s.readFound(ctx, q, set, valid, scope); err != nil {
			return SourceSet{}, err
		}
	}

	// Everything declared, well-formed and not found by query one goes to query
	// two for the reason it is missing.
	rest := make([]string, 0, len(valid)-len(s.found))
	for _, id := range valid {
		if _, ok := s.found[id]; !ok {
			rest = append(rest, id)
		}
	}
	if len(rest) > 0 {
		missing, alien, err := classifyRest(ctx, q, rest, scope)
		if err != nil {
			return SourceSet{}, err
		}
		s.missingInScope = missing
		foreign = append(foreign, alien...)
	}
	sort.Strings(foreign)
	s.foreignOrUnknown = foreign

	// The fold, and the only place it happens. MaxSensitivity over an EMPTY
	// parts list is credentials (backends/trust.go:86-88), so a resolution that
	// found nothing floors to credentials rather than to the zero value — the
	// fail-closed branch §4.8.1a asks for, inherited rather than re-implemented.
	parts := make([]backends.Sensitivity, 0, len(s.found))
	for _, info := range s.found {
		parts = append(parts, backends.Sensitivity(info.sensitivity))
	}
	s.flooredMax = string(floor(backends.MaxSensitivity(parts...), scope))
	return s, nil
}

// readFound runs query one and fills s.found.
func (s *SourceSet) readFound(ctx context.Context, q rowsQuerier, set *blocktype.Set, ids []string, scope string) error {
	rows, err := q.Query(ctx, resolveFoundSQL, ids, scope)
	if err != nil {
		return fmt.Errorf("store: resolve sources: read sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var info sourceInfo
		var stratum int
		if err := rows.Scan(&id, &info.title, &info.content, &info.sensitivity, &info.typeName, &stratum); err != nil {
			return fmt.Errorf("store: resolve sources: scan source: %w", err)
		}
		info.stratum = derived.Stratum(stratum)
		info.untrusted = set.IsUntrusted(info.typeName)
		s.found[id] = info
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: resolve sources: read sources: %w", err)
	}
	return nil
}

// classifyRest runs query two and splits the leftovers per §4.5.4. Both returned
// slices are sorted, so a violation message and a test assertion are stable.
func classifyRest(ctx context.Context, q rowsQuerier, rest []string, scope string) (missing, foreign []string, err error) {
	rows, err := q.Query(ctx, resolveRestSQL, rest)
	if err != nil {
		return nil, nil, fmt.Errorf("store: resolve sources: classify unresolved: %w", err)
	}
	defer rows.Close()
	elsewhere := make(map[string]string, len(rest))
	for rows.Next() {
		var id, sc string
		if err := rows.Scan(&id, &sc); err != nil {
			return nil, nil, fmt.Errorf("store: resolve sources: scan unresolved: %w", err)
		}
		elsewhere[id] = sc
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: resolve sources: classify unresolved: %w", err)
	}
	for _, id := range rest {
		switch sc, ok := elsewhere[id]; {
		case ok && sc == scope:
			// Present in the own scope but filtered out by query one: the row is
			// archived. §4.7.5 case 1 — drop it, no error.
			missing = append(missing, id)
		default:
			// A foreign scope, or nowhere at all. Both are V5.
			foreign = append(foreign, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(foreign)
	return missing, foreign, nil
}
