package store

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
)

// TestSearchTypesExclude pins the rule of search.leave_out_never_ranked (#342)
// on a registry snapshot without a database: what it adds, in which order, and
// every case in which it adds nothing.
func TestSearchTypesExclude(t *testing.T) {
	pol := func(name, kind string, isDefault bool) blocktype.Policy {
		return blocktype.Policy{Name: name, Scope: "_global", IsDefault: isDefault, Retrieval: blocktype.RetrievalPolicy{Kind: kind}}
	}
	set, err := blocktype.NewSet([]blocktype.Policy{
		pol("knowledge", blocktype.RetrievalFullPass, true),
		pol("system-meta", blocktype.RetrievalExcluded, false),
		pol("checkpoint", blocktype.RetrievalExcluded, false),
		pol("audit-trail", blocktype.RetrievalDamped, false),
		pol("insight", blocktype.RetrievalExcluded, false),
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	neverRanked := []string{"checkpoint", "insight", "system-meta"}
	for _, tc := range []struct {
		name    string
		set     *blocktype.Set
		on      bool
		query   string
		types   []string
		exclude []string
		want    []string
	}{
		{"key off", set, false, "wort", nil, []string{"audit-trail"}, []string{"audit-trail"}},
		{"key off, nothing to exclude", set, false, "wort", nil, nil, nil},
		{"key on, full text without types", set, true, "wort", nil, nil, neverRanked},
		{"key on, the request's list first", set, true, "wort", nil, []string{"audit-trail", "checkpoint"}, []string{"audit-trail", "checkpoint", "insight", "system-meta"}},
		{"key on, a listing", set, true, "", nil, []string{"audit-trail"}, []string{"audit-trail"}},
		{"key on, types named", set, true, "wort", []string{"checkpoint"}, nil, nil},
		{"key on, no registry", nil, true, "wort", nil, []string{"audit-trail"}, []string{"audit-trail"}},
	} {
		got := SearchTypesExclude(tc.set, tc.on, tc.query, tc.types, tc.exclude)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// The request's list is never written to.
	req := []string{"audit-trail"}
	_ = SearchTypesExclude(set, true, "wort", nil, req)
	if !reflect.DeepEqual(req, []string{"audit-trail"}) {
		t.Errorf("the request's list was changed: %v", req)
	}
	// Both names of the partial indexes' deny-list are in the stock answer, so
	// the statement declares the index predicate.
	if !impliesHardFTSDeny(nil, SearchTypesExclude(set, true, "wort", nil, nil)) {
		t.Error("the exclusion with the key on does not imply the partial indexes' predicate")
	}
}
