// WF T10 DB-less wire pins: the types_exclude alias semantics (seam 17) and
// the union helper. The /api/query wiring passes the UNION of types_exclude
// (canonical) and block_roles_exclude (legacy alias) into rrf.Search — both
// names present must never silently prefer one (monotone-restrictive union).
package handler

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestUnionExcludes(t *testing.T) {
	cases := []struct {
		name             string
		canonical, alias []string
		want             []string
	}{
		{"both nil", nil, nil, nil},
		{"canonical only", []string{"a"}, nil, []string{"a"}},
		{"alias only", nil, []string{"b"}, []string{"b"}},
		{"union dedup", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
	}
	for _, c := range cases {
		if got := unionExcludes(c.canonical, c.alias); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: unionExcludes(%v, %v) = %v, want %v", c.name, c.canonical, c.alias, got, c.want)
		}
	}
}

// TestQueryRequest_TypesExcludeAlias pins that BOTH wire names decode on
// /api/query (types_exclude canonical since T10, block_roles_exclude legacy).
func TestQueryRequest_TypesExcludeAlias(t *testing.T) {
	var req queryRequest
	body := `{"query":"q","types_exclude":["audit-trail"],"block_roles_exclude":["synthesis"]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(req.TypesExclude, []string{"audit-trail"}) {
		t.Errorf("TypesExclude = %v", req.TypesExclude)
	}
	if !reflect.DeepEqual(req.BlockRolesExclude, []string{"synthesis"}) {
		t.Errorf("BlockRolesExclude = %v", req.BlockRolesExclude)
	}
	if got := unionExcludes(req.TypesExclude, req.BlockRolesExclude); !reflect.DeepEqual(got, []string{"audit-trail", "synthesis"}) {
		t.Errorf("effective exclude = %v, want the union", got)
	}
}

// TestSearchRequest_TypeFilterFields pins the /api/search request shape.
func TestSearchRequest_TypeFilterFields(t *testing.T) {
	var req searchRequest
	body := `{"types":["knowledge"],"types_exclude":["audit-trail"],"block_roles_exclude":["audit-trail","synthesis"]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := unionExcludes(req.TypesExclude, req.BlockRolesExclude); !reflect.DeepEqual(got, []string{"audit-trail", "synthesis"}) {
		t.Errorf("effective exclude = %v", got)
	}
	if !reflect.DeepEqual(req.Types, []string{"knowledge"}) {
		t.Errorf("Types = %v", req.Types)
	}
}
