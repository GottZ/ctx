// WF T10 wire pin (DB-less): the T5 `json:"-"` tag on SearchResult.TypeName
// is activated — the policy type serializes under the wire name `type`.
package rrf

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSearchResult_TypeWireVisible(t *testing.T) {
	raw, err := json.Marshal(SearchResult{ID: "x", TypeName: "audit-trail"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"audit-trail"`) {
		t.Errorf("SearchResult wire = %s, want a \"type\" field (T10 contract change)", raw)
	}
	raw, err = json.Marshal(SearchResult{ID: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"type"`) {
		t.Errorf("empty TypeName must stay off the wire (omitempty): %s", raw)
	}
}
