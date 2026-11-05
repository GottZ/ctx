package dispatch

// Vocabulary pin of F-W3 (design/04 §4.1, Finding 30.2): the Ctx-Class
// grammar of internal/fedhop is the String() vocabulary of the dispatch
// classes, including design/03's external ones before they exist here. Every
// class of the class table must be a Ctx-Class value that a peer parses as
// itself — a class missing in fedhop would travel as "unknown ⇒ background".
// A03-W3 (external) and A03-W9 (external_bulk) add their urgency rows below.
//
//	go test -count=1 -run TestFedhopClassVocabularyCoversDispatchClasses ./internal/dispatch/

import (
	"net/http"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/fedhop"
)

func TestFedhopClassVocabularyCoversDispatchClasses(t *testing.T) {
	vocab := fedhop.ClassVocabulary()
	for c := Class(0); c < numClasses; c++ {
		name := c.String()
		if !slices.Contains(vocab, name) {
			t.Errorf("dispatch class %q is not in the fedhop Ctx-Class vocabulary %v", name, vocab)
			continue
		}
		h := http.Header{}
		h.Set(fedhop.HeaderClass, name)
		in, err := fedhop.ParseInbound(h)
		if err != nil || in.Class != name {
			t.Errorf("Ctx-Class: %s parses as %q (%v)", name, in.Class, err)
		}
	}
	// Urgency per class: merged under an interactive lease, an urgent class
	// keeps the lease (a tie), a bulk class lowers it to itself.
	urgent := map[Class]bool{ClassInteractive: true, ClassBackground: false, ClassExternal: true, ClassExternalBulk: false}
	for c := Class(0); c < numClasses; c++ {
		u, ok := urgent[c]
		if !ok {
			t.Errorf("class %q has no urgency row in this pin", c)
			continue
		}
		want := c.String()
		if u {
			want = ClassInteractive.String()
		}
		if got := fedhop.MergeClass(ClassInteractive.String(), c.String()); got != want {
			t.Errorf("MergeClass(interactive, %q) = %q, want %q (urgent: %v)", c, got, want, u)
		}
	}
}
