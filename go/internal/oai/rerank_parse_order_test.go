package oai

// The order pin of the rerank parse's document term (W02-10 hardening 1,
// external review 2 F-6 a1): the documents' decoded texts and their slots
// are booked before one of them is decoded. Moved behind the decode the term
// changes no answer — the parse refuses with errParseOverloaded either way —
// so the pin reads what the parse allocated before the refusal.

import (
	"strings"
	"testing"
)

// rerankParseOrderSlack is what a parse refused at its document term may
// allocate (sollwert of the hardening: ≤ 4 KiB; the sized run and the
// key table, no text).
const rerankParseOrderSlack = 4 << 10

// TestRerankDocumentsAreBookedBeforeTheDecode: 64 documents of 4 KiB under a
// cover that refuses exactly the document term (every smaller booking — the
// model, the query — passes) ⇒ errParseOverloaded with at most
// rerankParseOrderSlack allocated; the 256 KiB of texts never decoded.
func TestRerankDocumentsAreBookedBeforeTheDecode(t *testing.T) {
	const (
		docs = 64
		size = 4 << 10
	)
	doc := `"` + strings.Repeat("d", size) + `"`
	body := []byte(`{"model":"rr","query":"q","documents":[` + strings.TrimSuffix(strings.Repeat(doc+",", docs), ",") + `]}`)
	texts := int64(docs * size)
	var refused []int64
	cover := func(n int64) bool {
		if n >= texts {
			refused = append(refused, n)
			return false
		}
		return true
	}
	var perr *paramError
	alloc := allocatedBy(func() {
		refused = refused[:0]
		_, perr = parseRerankTop(body, cover)
	})
	t.Logf("W0210 documents order: body %d B, texts %d B; refused bookings %v, the parse allocated %d B before %v (slack %d B)",
		len(body), texts, refused, alloc, perr, rerankParseOrderSlack)
	if perr != errParseOverloaded || len(refused) != 1 || refused[0] < texts {
		t.Fatalf("parse %v, refused bookings %v — want errParseOverloaded at the one document term (≥ %d B)", perr, refused, texts)
	}
	if alloc > rerankParseOrderSlack {
		t.Errorf("refused at the document term, the parse allocated %d B (> %d B) — the texts were decoded before their booking", alloc, rerankParseOrderSlack)
	}
}
