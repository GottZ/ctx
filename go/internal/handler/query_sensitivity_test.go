// F3-P3 sensitivity annotation tests (design 03 §5, negative-probed classes
// 7/8 + the floor): the batch lookup annotates EVERY candidate (a straggler
// from rank >50 carries its level into the gate — red against a top-N
// lookup), a lookup miss acts as credentials, and the scope floor only
// raises.
package handler

import (
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/store"
)

// TestAnnotateSensitivities_StragglerBeyondRank50 pins the full-breadth
// lookup: filterSuperseded/graph placement can advance a candidate from
// beyond any top-N window into the final llmSources — its sensitivity must
// already be annotated. A top-max(MaxDocs,limit) lookup leaves rank 55 as a
// zero value and fails here.
func TestAnnotateSensitivities_StragglerBeyondRank50(t *testing.T) {
	results := make([]rrf.SearchResult, 60)
	sensMap := make(map[string]store.BlockSensitivity, 60)
	for i := range results {
		id := fmt.Sprintf("id-%03d", i)
		results[i] = rrf.SearchResult{ID: id, Scope: "private"}
		sensMap[id] = store.BlockSensitivity{Sensitivity: backends.SensInternal, Scope: "private"}
	}
	// All blocks live in the caller's own scope (private) → none is grant-mediated
	// → the T43 grantee-floor arm never fires → byte-identical to the scope-only
	// owner floor (here: nil floor).
	annotateSensitivities(results, sensMap, nil, []string{"private"}, backends.SensPublic)

	for _, rank := range []int{0, 49, 55, 59} {
		if got := results[rank].Sensitivity; got != backends.SensInternal {
			t.Errorf("rank %d sensitivity = %q, want internal — lookup must cover ALL candidates, not top-N", rank, got)
		}
	}
}

// TestAnnotateSensitivities_LookupMissFailsClosed: an ID without a lookup row
// (block deleted/archived between RRF and lookup) acts as credentials. Red
// against "unknown ⇒ rank 0".
func TestAnnotateSensitivities_LookupMissFailsClosed(t *testing.T) {
	results := []rrf.SearchResult{{ID: "gone", Scope: "private"}}
	annotateSensitivities(results, map[string]store.BlockSensitivity{}, nil, []string{"private"}, backends.SensPublic)
	if got := results[0].Sensitivity; got != backends.SensCredentials {
		t.Errorf("miss sensitivity = %q, want credentials (fail-closed)", got)
	}
}

// TestAnnotateSensitivities_ScopeFloorOnlyRaises: floor[scope]=personal lifts
// a public block to personal at the gate; a credentials block stays
// credentials under an internal floor (monotone, never lowers); scopes
// without an entry stay unchanged.
func TestAnnotateSensitivities_ScopeFloorOnlyRaises(t *testing.T) {
	floor := config.ScopeFloor{
		"friend": backends.SensPersonal,
		"work":   backends.SensInternal,
	}
	results := []rrf.SearchResult{
		{ID: "a"}, // friend scope, stored public → floored to personal
		{ID: "b"}, // work scope, stored credentials → stays credentials
		{ID: "c"}, // private scope, no floor entry → unchanged public
	}
	sensMap := map[string]store.BlockSensitivity{
		"a": {Sensitivity: backends.SensPublic, Scope: "friend"},
		"b": {Sensitivity: backends.SensCredentials, Scope: "work"},
		"c": {Sensitivity: backends.SensPublic, Scope: "private"},
	}
	// Caller owns friend/work/private → none grant-mediated → owner-only floor
	// (the T43 arm is exercised separately in query_grant_floor_test.go).
	annotateSensitivities(results, sensMap, floor, []string{"friend", "work", "private"}, backends.SensPublic)

	if got := results[0].Sensitivity; got != backends.SensPersonal {
		t.Errorf("floored public block = %q, want personal", got)
	}
	if got := results[1].Sensitivity; got != backends.SensCredentials {
		t.Errorf("credentials under internal floor = %q, want credentials (floor never lowers)", got)
	}
	if got := results[2].Sensitivity; got != backends.SensPublic {
		t.Errorf("unfloored scope = %q, want public (unchanged)", got)
	}
}

// TestRerankRequired_TopDocsOnly: the rerank requirement folds the query and
// the candidates that actually GO to the reranker — a credentials block
// beyond maxDocs must not raise it.
func TestRerankRequired_TopDocsOnly(t *testing.T) {
	results := []rrf.SearchResult{
		{ID: "1", Sensitivity: backends.SensInternal},
		{ID: "2", Sensitivity: backends.SensPersonal},
		{ID: "3", Sensitivity: backends.SensCredentials}, // beyond maxDocs=2
	}
	if got := rerankRequired(backends.SensPublic, results, 2); got != backends.SensPersonal {
		t.Errorf("required = %q, want personal (rank-3 candidate is outside the rerank window)", got)
	}
	if got := rerankRequired(backends.SensPublic, results, 3); got != backends.SensCredentials {
		t.Errorf("required = %q, want credentials once the block is inside the window", got)
	}
}
