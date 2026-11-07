package oai

// The route side of FOLGE-R (RC-D S1, cluster C8): the rerank route books
// the wire body the wire writes. The length half is a consistency pin
// (green on the base: both are json.Marshal's length); the factor half is
// red against the S1 stub — the route still books 29/4 × the body, the
// cold json.Marshal's growth, where the byte writer needs one allocation of
// the body's length (FOLGE-R S2 sets the factor to 1 and rerankWireBodyLen
// to rerank.WireBodyLen).

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/rerank"
)

func TestRerankRouteBooksTheByteWriter(t *testing.T) {
	for _, doc := range []string{"plain text", strings.Repeat("<&>", 50), "\x01\x1f\t\n", `"\\`, "\xff\xfe", "  ", "äöü€😀"} {
		for _, n := range []int{1, 3, 128} {
			docs := make([]string, n)
			for i := range docs {
				docs[i] = doc
			}
			if got, want := rerankWireBodyLen("bge-"+doc, "q "+doc, docs), rerank.WireBodyLen("bge-"+doc, "q "+doc, docs); got != want {
				t.Errorf("doc %q n=%d: the route books a %d B body, the wire writes %d B", doc, n, got, want)
			}
		}
	}
	if 2*RerankWireBodyNum > 3*RerankWireBodyDen {
		t.Errorf("the route books %d/%d × the wire body for its build, want at most 3/2 (FOLGE-R S-W1: the byte writer writes the body in one allocation of its length)", RerankWireBodyNum, RerankWireBodyDen)
	}
}
