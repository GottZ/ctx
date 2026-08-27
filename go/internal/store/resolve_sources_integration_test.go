//go:build integration

// Wave W01-5 (design/01 §4.5.4, §4.8.1a + §7 W01-5 gates 3, 4, 5 and the
// ScopeFloor obligation from the W01-1 review) against a real PG18
// testcontainer.
//
// What is under test is the SPLIT, not the query: §4.5.4 exists because the
// first draft had ONE "missing" set and therefore two contradictory answers to
// the same detection — V5 said "a source is missing ⇒ the write dies"
// (fail-closed) and §4.7.5 case 1 said "regenerate with the rest, no error"
// (fail-open). Whoever resolved that in favour of keeping the arm alive turned
// the scope check B7 into a silent swallow. Every probe here therefore runs
// BOTH directions; a one-sided probe would be green by luck.
//
//	go test -tags=integration ./internal/store/ -run TestResolveSources -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// rsSeed is one source block, written with a direct INSERT because the probes
// need states UpsertBlock does not produce: a foreign scope, an archived row,
// and a metadata.provenance that makes the row a level-1 derivative.
type rsSeed struct {
	title       string
	scope       string
	sensitivity string
	typeName    string
	archived    bool
	stratum     int // 0 = no provenance object at all
}

func rsInsert(t *testing.T, pool *pgxpool.Pool, s rsSeed) string {
	t.Helper()
	md := map[string]any{}
	if s.stratum > 0 {
		md[derived.MetadataKey] = map[string]any{
			"v":       derived.ContractVersion,
			"stratum": s.stratum,
		}
	}
	raw, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("marshal metadata for %s: %v", s.title, err)
	}
	typeName := s.typeName
	if typeName == "" {
		typeName = "knowledge"
	}
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity, type_name, is_archived, metadata)
		 VALUES ('learnings', $1, $2, $3, $4, $5, $6, $7::jsonb) RETURNING id::text`,
		s.title, "body of "+s.title, s.scope, s.sensitivity, typeName, s.archived, string(raw),
	).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", s.title, err)
	}
	return id
}

// rsProvenance builds a provenance that satisfies V1–V14 for the given resolve
// result, so every probe below fails for exactly the clause it is about and not
// for a fixture accident. kept is derived from the ids, three claims over three
// distinct sources (MinClaimsKept = 3, MinSourceCount = 3).
func rsProvenance(ids []string, flooredMax string, stratum derived.Stratum) (derived.Provenance, []derived.Claim) {
	kept := make([]derived.Claim, 0, 3)
	for i := 0; i < 3 && i < len(ids); i++ {
		kept = append(kept, derived.Claim{
			Claim:    "an assertion about source " + ids[i],
			Quote:    "a verbatim quote long enough to pass the gate elsewhere",
			SourceID: ids[i],
			Kind:     "cited",
		})
	}
	rejects := map[string]int{}
	for _, k := range derived.GateKeys {
		rejects[k] = 0
	}
	return derived.Provenance{
		V:              derived.ContractVersion,
		Stratum:        stratum,
		Arm:            "w015-probe",
		SourceBlockIDs: ids,
		SourceCount:    len(ids),
		SourceDigest:   derived.SourceDigest(ids),
		Anchor: derived.Anchor{
			Kind:     derived.AnchorClusterTopic,
			TopicID:  "0123456789abcdef0123456789abcdef",
			CoreHash: "sha256:core",
		},
		Generator: derived.Generator{
			Model:         "probe-model",
			PromptVersion: "v1",
			GateVersion:   derived.GateVersion,
		},
		Coverage: derived.Coverage{
			ClaimsOffered:  8,
			ClaimsKept:     len(kept),
			Rejects:        rejects,
			SourcesCovered: len(kept),
		},
		SensitivityMax: flooredMax,
	}, kept
}

// rsRegistry boots the block type registry off the migrated test database — the
// same snapshot production resolves retrieval.untrusted from.
func rsRegistry(t *testing.T, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg.Snapshot()
}

func TestResolveSources_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := rsRegistry(t, pool)

	// No floor entry for this scope: Apply is a pass-through, so every probe
	// that is NOT about the floor measures the raw fold.
	noFloor := config.ScopeFloor{}.Apply

	const own = "w015"

	t.Run("gate3a_foreign_scope_is_ForeignOrUnknown_and_V5", func(t *testing.T) {
		a := rsInsert(t, pool, rsSeed{title: "w015-3a-own-1", scope: own, sensitivity: "internal"})
		b := rsInsert(t, pool, rsSeed{title: "w015-3a-own-2", scope: own, sensitivity: "internal"})
		c := rsInsert(t, pool, rsSeed{title: "w015-3a-own-3", scope: own, sensitivity: "internal"})
		alien := rsInsert(t, pool, rsSeed{title: "w015-3a-alien", scope: "w015-other", sensitivity: "internal"})

		got, err := store.ResolveSources(ctx, pool, set, noFloor, []string{a, b, c, alien}, own)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(got.ForeignOrUnknown()) != 1 || got.ForeignOrUnknown()[0] != alien {
			t.Fatalf("ForeignOrUnknown = %v, want [%s] — the second query is what separates a foreign scope from an archived row",
				got.ForeignOrUnknown(), alien)
		}
		if len(got.MissingInScope()) != 0 {
			t.Errorf("MissingInScope = %v, want empty — a foreign source must NOT collapse into the droppable set", got.MissingInScope())
		}
		if _, leaked := got.Sources()[alien]; leaked {
			t.Error("the foreign block's CONTENT is in the source map — query one must stay scope-filtered")
		}

		// The declaration still names all four, which is the case the arm
		// produces: V5 has to kill the write.
		ids := got.ResolvedIDs()
		p, kept := rsProvenance(append(append([]string(nil), ids...), alien), got.FlooredMax(), derived.StratumDerived)
		// keep source_block_ids strictly ascending (V3)
		p.SourceBlockIDs = sortedIDs(p.SourceBlockIDs)
		p.SourceDigest = derived.SourceDigest(p.SourceBlockIDs)
		err = derived.Validate(p, kept, derived.Target{
			Stratum:  derived.StratumDerived,
			Required: got.FlooredMax(),
		}, got.Facts())
		if v := derived.Violation(err); v != "V5" {
			t.Fatalf("Validate = %v (clause %q), want V5 — B7, fail-closed", err, v)
		}
	})

	t.Run("gate3b_archived_in_own_scope_is_MissingInScope_and_no_error", func(t *testing.T) {
		a := rsInsert(t, pool, rsSeed{title: "w015-3b-own-1", scope: own, sensitivity: "internal"})
		b := rsInsert(t, pool, rsSeed{title: "w015-3b-own-2", scope: own, sensitivity: "internal"})
		c := rsInsert(t, pool, rsSeed{title: "w015-3b-own-3", scope: own, sensitivity: "internal"})
		gone := rsInsert(t, pool, rsSeed{title: "w015-3b-archived", scope: own, sensitivity: "internal", archived: true})

		got, err := store.ResolveSources(ctx, pool, set, noFloor, []string{a, b, c, gone}, own)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(got.MissingInScope()) != 1 || got.MissingInScope()[0] != gone {
			t.Fatalf("MissingInScope = %v, want [%s]", got.MissingInScope(), gone)
		}
		if len(got.ForeignOrUnknown()) != 0 {
			t.Fatalf("ForeignOrUnknown = %v, want empty — an archived own-scope row is §4.7.5 case 1, not B7", got.ForeignOrUnknown())
		}

		p, kept := rsProvenance(sortedIDs(append(got.ResolvedIDs(), gone)), got.FlooredMax(), derived.StratumDerived)
		p.SourceDigest = derived.SourceDigest(p.SourceBlockIDs)
		if err := derived.Validate(p, kept, derived.Target{
			Stratum:  derived.StratumDerived,
			Required: got.FlooredMax(),
		}, got.Facts()); err != nil {
			t.Fatalf("Validate = %v (clause %q), want nil — the arm drops the source and carries on",
				err, derived.Violation(err))
		}
	})

	t.Run("gate3c_an_id_that_exists_nowhere_is_ForeignOrUnknown", func(t *testing.T) {
		a := rsInsert(t, pool, rsSeed{title: "w015-3c-own-1", scope: own, sensitivity: "internal"})
		const nobody = "00000000-0000-4000-8000-0000000000ff"
		const garbage = "not-a-uuid"

		got, err := store.ResolveSources(ctx, pool, set, noFloor, []string{a, nobody, garbage}, own)
		if err != nil {
			t.Fatalf("resolve: %v — a malformed id must be a contract violation, not a driver error", err)
		}
		if len(got.ForeignOrUnknown()) != 2 {
			t.Fatalf("ForeignOrUnknown = %v, want both the unknown uuid and the malformed id", got.ForeignOrUnknown())
		}
		if len(got.MissingInScope()) != 0 {
			t.Errorf("MissingInScope = %v, want empty — 'nowhere' is not 'archived here'", got.MissingInScope())
		}
	})

	t.Run("gate4_a_level_one_source_under_a_level_one_target_is_V6", func(t *testing.T) {
		a := rsInsert(t, pool, rsSeed{title: "w015-4-own-1", scope: own, sensitivity: "internal"})
		b := rsInsert(t, pool, rsSeed{title: "w015-4-own-2", scope: own, sensitivity: "internal"})
		derivative := rsInsert(t, pool, rsSeed{
			title: "w015-4-derivative", scope: own, sensitivity: "internal",
			typeName: derived.TypeCatalog, stratum: int(derived.StratumDerived),
		})

		got, err := store.ResolveSources(ctx, pool, set, noFloor, []string{a, b, derivative}, own)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if s, ok := got.Facts().StratumOf(derivative); !ok || s != derived.StratumDerived {
			t.Fatalf("stratum of the derivative source = %d, want %d — it is read from the block's own provenance, not from its type name",
				s, derived.StratumDerived)
		}
		if s, ok := got.Facts().StratumOf(a); !ok || s != derived.StratumSource {
			t.Fatalf("stratum of an ordinary block = %d, want 0 (COALESCE default)", s)
		}

		ids := got.ResolvedIDs()
		p, kept := rsProvenance(ids, got.FlooredMax(), derived.StratumDerived)
		err = derived.Validate(p, kept, derived.Target{
			Stratum:  derived.StratumDerived,
			Required: got.FlooredMax(),
		}, got.Facts())
		if v := derived.Violation(err); v != "V6" {
			t.Fatalf("Validate = %v (clause %q), want V6 — B8, the level rule", err, v)
		}

		// Control: the same source set under a level-2 target validates. The
		// rule is the ORDER, not a ban on derived sources.
		p2, kept2 := rsProvenance(ids, got.FlooredMax(), derived.StratumSuper)
		if err := derived.Validate(p2, kept2, derived.Target{
			Stratum:  derived.StratumSuper,
			Required: got.FlooredMax(),
		}, got.Facts()); err != nil {
			t.Fatalf("level-2 target over level-1 sources = %v (clause %q), want nil",
				err, derived.Violation(err))
		}
	})

	t.Run("gate5_egress_one_credentials_source_under_nine_internal", func(t *testing.T) {
		ids := make([]string, 0, 10)
		for i := 0; i < 9; i++ {
			ids = append(ids, rsInsert(t, pool, rsSeed{
				title: "w015-5-internal-" + string(rune('a'+i)), scope: own, sensitivity: "internal",
			}))
		}
		ids = append(ids, rsInsert(t, pool, rsSeed{title: "w015-5-credentials", scope: own, sensitivity: "credentials"}))

		got, err := store.ResolveSources(ctx, pool, set, noFloor, ids, own)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.FlooredMax() != derived.SensitivityCredentials {
			t.Fatalf("FlooredMax = %q, want credentials — one credentials source among nine internal folds the whole block (§4.8.1)",
				got.FlooredMax())
		}

		p, kept := rsProvenance(got.ResolvedIDs(), got.FlooredMax(), derived.StratumDerived)
		// The call the arm will make: Required IS the floored maximum.
		if err := derived.Validate(p, kept, derived.Target{
			Stratum:  derived.StratumDerived,
			Required: got.FlooredMax(),
		}, got.Facts()); err != nil {
			t.Fatalf("Validate = %v (clause %q), want nil", err, derived.Violation(err))
		}
		// Negative: an arm that leaves Required at a default. Without V13 this
		// is the leak path of §4.8.1a — 26 verbatim credentials quotes to a
		// no-credentials backend, correctly folded on the WRITE side and
		// unguarded on the EGRESS side.
		err = derived.Validate(p, kept, derived.Target{
			Stratum:  derived.StratumDerived,
			Required: derived.SensitivityInternal,
		}, got.Facts())
		if v := derived.Violation(err); v != "V13" {
			t.Fatalf("Validate with Required=internal = %v (clause %q), want V13", err, v)
		}
	})

	t.Run("gate7a_ScopeFloor_Apply_raises_the_fold", func(t *testing.T) {
		const floored = "w015-floored"
		ids := []string{
			rsInsert(t, pool, rsSeed{title: "w015-7a-1", scope: floored, sensitivity: "internal"}),
			rsInsert(t, pool, rsSeed{title: "w015-7a-2", scope: floored, sensitivity: "internal"}),
			rsInsert(t, pool, rsSeed{title: "w015-7a-3", scope: floored, sensitivity: "internal"}),
		}
		// The REAL config.ScopeFloor, not a stand-in: the obligation from the
		// W01-1 review (#6) is that Apply runs HERE, and a hand-rolled fake
		// would prove that a function ran, not that the policy did.
		floor := config.ScopeFloor{floored: backends.SensCredentials}.Apply

		raised, err := store.ResolveSources(ctx, pool, set, floor, ids, floored)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if raised.FlooredMax() != derived.SensitivityCredentials {
			t.Fatalf("FlooredMax = %q, want credentials — the scope floor raises a set of internal sources; without Apply this is %q",
				raised.FlooredMax(), derived.SensitivityInternal)
		}
		// Same sources, same query, no floor entry: the value MUST differ, or
		// the probe above would be green for the wrong reason.
		flat, err := store.ResolveSources(ctx, pool, set, noFloor, ids, floored)
		if err != nil {
			t.Fatalf("resolve without floor: %v", err)
		}
		if flat.FlooredMax() != derived.SensitivityInternal {
			t.Fatalf("FlooredMax without a floor entry = %q, want internal", flat.FlooredMax())
		}

		// And the floored value is what the contract carries — provenance,
		// call.Required and the written column are ONE value (§4.8.1a).
		p, kept := rsProvenance(raised.ResolvedIDs(), raised.FlooredMax(), derived.StratumDerived)
		if err := derived.Validate(p, kept, derived.Target{
			Stratum:  derived.StratumDerived,
			Required: raised.FlooredMax(),
		}, raised.Facts()); err != nil {
			t.Fatalf("Validate against the floored maximum = %v (clause %q), want nil", err, derived.Violation(err))
		}
	})

	t.Run("gate7b_facts_and_sources_come_out_of_one_resolution", func(t *testing.T) {
		ids := []string{
			rsInsert(t, pool, rsSeed{title: "w015-7b-1", scope: own, sensitivity: "internal"}),
			rsInsert(t, pool, rsSeed{title: "w015-7b-2", scope: own, sensitivity: "credentials"}),
			rsInsert(t, pool, rsSeed{title: "w015-7b-3", scope: own, sensitivity: "internal"}),
		}
		got, err := store.ResolveSources(ctx, pool, set, noFloor, ids, own)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		srcs, facts := got.Sources(), got.Facts()
		if len(srcs) != facts.Len() {
			t.Fatalf("source map has %d entries, fact map has %d — the two views must cover the same set (W01-1 review #7)",
				len(srcs), facts.Len())
		}
		for id, s := range srcs {
			if fs, ok := facts.SensitivityOf(id); !ok || s.Sensitivity != fs {
				fs, _ := facts.SensitivityOf(id)
				t.Errorf("source %s: gate sees %q, validator sees %q", id, s.Sensitivity, fs)
			}
			if s.Content == "" || s.Title == "" {
				t.Errorf("source %s carries no title/content — CiteGate cannot check a quote against nothing", id)
			}
		}
		// The credentials source is in the map, which is what lets G7 index its
		// title. A second, independent resolution is exactly how it goes
		// missing there while still being validated here.
		if got.Sources()[ids[1]].Sensitivity != derived.SensitivityCredentials {
			t.Error("the credentials source is not carrying its level into the gate")
		}
		if n := got.UntrustedCount(); n != 0 {
			t.Errorf("UntrustedCount = %d, want 0 for ordinary sources", n)
		}
	})

	t.Run("gate7c_untrusted_falls_out_of_the_registry_snapshot", func(t *testing.T) {
		untrusted := set.UntrustedTypes()
		if len(untrusted) == 0 {
			t.Skip("no untrusted type in the registry — V11 has nothing to inherit from")
		}
		u := rsInsert(t, pool, rsSeed{title: "w015-7c-untrusted", scope: own, sensitivity: "internal", typeName: untrusted[0]})
		plain := rsInsert(t, pool, rsSeed{title: "w015-7c-plain", scope: own, sensitivity: "internal"})

		got, err := store.ResolveSources(ctx, pool, set, noFloor, []string{u, plain}, own)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if u2, ok := got.Facts().IsUntrusted(u); !ok || !u2 {
			t.Errorf("source of type %q is not marked untrusted — V11 is then blind", untrusted[0])
		}
		if p2, _ := got.Facts().IsUntrusted(plain); p2 {
			t.Errorf("an ordinary source is marked untrusted")
		}
		if n := got.UntrustedCount(); n != 1 {
			t.Errorf("UntrustedCount = %d, want 1 (provenance.untrusted_sources, §4.8.3)", n)
		}
	})

	t.Run("gate7d_a_missing_floor_or_snapshot_fails_closed", func(t *testing.T) {
		a := rsInsert(t, pool, rsSeed{title: "w015-7d-1", scope: own, sensitivity: "internal"})
		if _, err := store.ResolveSources(ctx, pool, set, nil, []string{a}, own); err == nil {
			t.Error("a nil floor was accepted — §4.8.1a would then be carried by nobody")
		}
		if _, err := store.ResolveSources(ctx, pool, nil, noFloor, []string{a}, own); err == nil {
			t.Error("a nil registry snapshot was accepted — V11 would be unverifiable")
		}
		if _, err := store.ResolveSources(ctx, pool, set, noFloor, []string{a}, ""); err == nil {
			t.Error("an empty scope was accepted — the resolution would cross scopes by construction")
		}
	})

	t.Run("gate7e_an_empty_resolution_folds_to_credentials", func(t *testing.T) {
		got, err := store.ResolveSources(ctx, pool, set, noFloor, nil, own)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.FlooredMax() != derived.SensitivityCredentials {
			t.Fatalf("FlooredMax over an empty source set = %q, want credentials (§4.8.1a, fail-closed)", got.FlooredMax())
		}
		if got.Len() != 0 {
			t.Errorf("Len = %d, want 0", got.Len())
		}
	})
}

// sortedIDs returns the ids ascending — provenance.source_block_ids has to be
// strictly ascending (V3) and the digest hashes the sorted join (V4).
func sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
