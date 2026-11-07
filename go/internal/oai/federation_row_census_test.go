package oai_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGatewayRowsCarryFederation is the census of F-W12's row attribution
// (design/04 §4.7): every llmlog.Gateway literal in the production files of
// internal/oai is built inside withFederation, so a route that forgets the
// claimed chain or the proved peer instance fails here — chat is covered by
// TestChatRowPeerAttribution, embeddings and rerank by this census.
func TestGatewayRowsCarryFederation(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	lit := regexp.MustCompile(`llmlog\.Gateway\{`)
	// The request's own context, nothing else: a row built over another
	// context (context.Background(), a detached one) would lose the claimed
	// chain and the proved instance (Fremd-Review T-2).
	wrapped := regexp.MustCompile(`withFederation\(c\.r\.Context\(\), llmlog\.Gateway\{`)
	// A GatewayRow whose Gateway comes from a variable would slip past the
	// literal count: the rows are counted too.
	rowLit := regexp.MustCompile(`llmlog\.GatewayRow\{`)
	total, rows := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rows += len(rowLit.FindAllIndex(b, -1))
		for i, line := range strings.Split(string(b), "\n") {
			n := len(lit.FindAllStringIndex(line, -1))
			if n == 0 {
				continue
			}
			total += n
			if w := len(wrapped.FindAllStringIndex(line, -1)); w != n {
				t.Errorf("%s:%d: llmlog.Gateway literal outside withFederation: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	// chat, embeddings, rerank: the forced row and the final row each.
	if total != 6 || rows != 6 {
		t.Errorf("%d llmlog.Gateway and %d llmlog.GatewayRow literals in internal/oai, the census expects 6 each — a new row site needs withFederation and this count", total, rows)
	}
}
