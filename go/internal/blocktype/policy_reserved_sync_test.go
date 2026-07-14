// W1-G2 sync arm (graph-structural GA6, design/01 §4.6a): the reservation
// list in this package is a deliberate LOCAL COPY of store.GraphRels
// (import-cycle mechanics, globalScope precedent). This test is the pin that
// keeps the two from drifting — it lives in the EXTERNAL test package because
// store imports blocktype (store/classify.go): blocktype_test → store →
// blocktype is acyclic, an internal test import would not be.
package blocktype_test

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

func TestReservedGraphClassesSyncWithStoreGraphRels(t *testing.T) {
	if !slices.Equal(blocktype.ReservedGraphClassesForTest, store.GraphRels) {
		t.Fatalf("reservedGraphClasses (blocktype, local copy) = %v, store.GraphRels = %v — the copies drifted; update reservedGraphClasses in policy.go",
			blocktype.ReservedGraphClassesForTest, store.GraphRels)
	}
}
