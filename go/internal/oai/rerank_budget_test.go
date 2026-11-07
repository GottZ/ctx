package oai_test

// The ledger pin of the rerank routes (W02-10, after FOLGE-E's
// TestEmbeddingResponseReserved): while a request holds in the engine, the
// in-flight budget holds EXACTLY the sum of its terms — computed here from
// the gate's own constants, not read off the booking (a mutated or dropped
// term moves the route's sum and not the expectation).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// The terms of the rerank route as the gate's own constants (design/02
// §4.2.1, bau report W02-10 §Term-Tabelle).
const (
	gateReserveFactor = 3           // the budget stage's request share per body byte
	gateDepthState    = 24          // the JSON check's state per nesting level
	gateDocSlot       = 16 + 16 + 8 // per document: two string headers and a rune count
	gateResult        = 64          // the answer's share per document
	gateEncodeBuffer  = 16 << 10    // the answer's encoder buffer
	gateScanFactor    = 23          // the scan's booking per body byte below credentials
	gateScanBase      = 128 << 10   // and its fixed part
	gateScanDepth     = 104         // and per nesting level
	gateBodyNum       = 1           // the wire body's build: one allocation of its length (FOLGE-R; 29/4 before)
	gateBodyDen       = 1
	gateBodySlack     = 8 << 10                        // and the allocation's size class
	gateCopy          = 32 << 10                       // the transport's copy, at most
	gateEcho          = 1                              // the answer's room for an echo: the body's length once more
	gateAnswerDoc     = 2*128 + 16                     // per document: the answer's entry share for both read buffers and the decode (176 before)
	gateCall          = 2*(4<<10) + 2*(8<<10) + 60<<10 // one call: the answer's frame and size class for both read buffers, the call's own (52 KiB before)
	gateRetry         = 30 << 10                       // the retry's connection state
)

// gateWireCall is the booking of one wire call of a body of b bytes over
// docs documents, its retry included.
func gateWireCall(b, docs int64) int64 {
	c := min(b, gateCopy)
	return (gateBodyNum*b+gateBodyDen-1)/gateBodyDen + gateBodySlack + c + gateEcho*b + docs*gateAnswerDoc + gateCall + c + gateRetry
}

// gateWireBody is the length of rerank.Score's body for model, query and
// docs as encoding/json writes it without HTML escaping (FOLGE-R's byte
// writer: model, query, documents, return_documents false, no newline) —
// the gate's own oracle, not rerank.RequestBodyLen.
func gateWireBody(t *testing.T, model, query string, docs []string) int64 {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Model           string   `json:"model"`
		Query           string   `json:"query"`
		Documents       []string `json:"documents"`
		ReturnDocuments bool     `json:"return_documents"`
	}{model, query, docs, false}); err != nil {
		t.Fatal(err)
	}
	return int64(buf.Len() - 1)
}

// TestRerankBudgetHoldsTheTerms: 128 documents of 4 KiB in one block (E and
// sub_batch ≥ 128), to the credentials alias rrwide (no scan) and to rrpub
// (the scan booked) — while the engine holds the request, the budget holds
// max(3 × the body, the read + the nesting state + the decoded model, query
// and documents + 40 B per document + the answer + [the scan] + the wire
// call of the block).
func TestRerankBudgetHoldsTheTerms(t *testing.T) {
	for _, alias := range []string{"rrwide", "rrpub"} {
		t.Run(alias, func(t *testing.T) {
			e := newRerankEngine(t)
			release := make(chan struct{})
			var once sync.Once
			open := func() { once.Do(func() { close(release) }) }
			defer open()
			e.setRespond(func(w http.ResponseWriter, r *http.Request, _ int64, c rerankCall) bool {
				<-release
				_, _ = w.Write([]byte(e.answer(c)))
				return true
			})
			rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 200, 1, withoutCensus)
			const query = "which document is relevant"
			docs := rerankDocsOf(128, 4<<10)
			body := rerankBody(alias, query, docs)
			// The wire body the route books its call on (rerank.Score's body:
			// model of the row, query, documents).
			wire := gateWireBody(t, "RERANK-MODEL", query, docs)
			n := int64(len(docs))
			texts := int64(len(alias) + len(query))
			for _, d := range docs {
				texts += int64(len(d))
			}
			const depth = 2
			spent := int64(len(body)) + depth*gateDepthState + texts + n*gateDocSlot + n*gateResult + gateEncodeBuffer +
				gateWireCall(wire, n)
			if alias == "rrpub" {
				spent += gateScanFactor*int64(len(body)) + gateScanBase + depth*gateScanDepth
			}
			want := max(gateReserveFactor*int64(len(body)), spent)
			done := make(chan chatAnswer)
			go func() { done <- rig.postRerank(t, "/v1/rerank", body) }()
			deadline := time.Now().Add(10 * time.Second)
			for e.hits.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			got := rig.budget.InUse()
			open()
			a := <-done
			t.Logf("W0210 budget %s: body %d B, wire body %d B; held in the engine %d B, want %d B (spent %d, share %d)",
				alias, len(body), wire, got, want, spent, gateReserveFactor*int64(len(body)))
			if a.status != http.StatusOK {
				t.Fatalf("answer %d %.200s", a.status, a.body)
			}
			if got != want {
				t.Errorf("the budget holds %d B while the request is in the engine, want exactly %d B (Δ %d)", got, want, got-want)
			}
			if strings.Count(string(a.body), `"relevance_score"`) != len(docs) {
				t.Errorf("answer without every document")
			}
		})
	}
}

// TestRerankFailoverBooksTheSecondAttempt (lead review 1 R-3): a failover
// attempt builds its wire bodies anew, so it books its whole wire term
// again — the first attempt's certain part was booked before the acquire,
// the second attempt's is not. Row A answers block one with 502 (no
// commit: the walk fails over), row B holds the request: the budget holds
// the request's terms with two wire calls.
func TestRerankFailoverBooksTheSecondAttempt(t *testing.T) {
	a := newRerankEngine(t)
	b := newRerankEngine(t)
	a.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ rerankCall) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	b.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, c rerankCall) bool {
		<-release
		_, _ = w.Write([]byte(b.answer(c)))
		return true
	})
	rig := newRerankRig(t, []backends.Backend{a.row("row-a", 20), b.row("row-b", 10)}, 200, 1, withoutCensus)
	const query = "which document is relevant"
	docs := rerankDocsOf(128, 4<<10)
	body := rerankBody("rrwide", query, docs)
	wire := gateWireBody(t, "RERANK-MODEL", query, docs)
	n := int64(len(docs))
	texts := int64(len("rrwide") + len(query))
	for _, d := range docs {
		texts += int64(len(d))
	}
	const depth = 2
	want := int64(len(body)) + depth*gateDepthState + texts + n*gateDocSlot + n*gateResult + gateEncodeBuffer +
		2*gateWireCall(wire, n)
	done := make(chan chatAnswer)
	go func() { done <- rig.postRerank(t, "/v1/rerank", body) }()
	deadline := time.Now().Add(10 * time.Second)
	for b.hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := rig.budget.InUse()
	open()
	ans := <-done
	t.Logf("W0210 failover budget: row A %d hits, row B %d; held on row B %d B, want %d B", a.hits.Load(), b.hits.Load(), got, want)
	if ans.status != http.StatusOK || a.hits.Load() != 1 {
		t.Fatalf("answer %d %.200s, row A hits %d — want 200 after one failover", ans.status, ans.body, a.hits.Load())
	}
	if got != want {
		t.Errorf("the budget holds %d B on the second attempt, want exactly %d B (Δ %d) — two wire calls booked", got, want, got-want)
	}
}
