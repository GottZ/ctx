package oai_test

// The order pins of the rerank routes' ledger (W02-10, after FOLGE-E's
// embed_order_pins_test.go): a booking that slides behind the allocation
// (or the stage) it covers changes no answer — the request is 503
// server_overloaded either way, or served either way — so each pin finds the
// least budget at which a stage is reached (a bisection over
// gateway.inflight_budget: the reservation only grows, the first booking
// that does not fit refuses) and checks what the request did one byte below
// it: which stage it reached, what it allocated.

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// leastBudget is the least gateway.inflight_budget in [1, 1 GiB] at which
// reached holds for the request run makes (reached is monotone in the
// budget: a larger budget reaches every stage a smaller one does).
func leastBudget(t *testing.T, rig *chatRig, run func() bool) int64 {
	t.Helper()
	lo, hi := int64(1), int64(1<<30)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = hi })
	if !run() {
		t.Fatal("the request does not reach the stage at 1 GiB")
	}
	for lo < hi {
		m := lo + (hi-lo)/2
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = m })
		if run() {
			hi = m
		} else {
			lo = m + 1
		}
	}
	return lo
}

// TestRerankDepthIsBookedBeforeTheCheck: the JSON check's nesting state —
// 24 B per level — is booked before the check runs (FOLGE-E E05 at this
// route): a body nested one level past the decoder's limit, to the
// credentials alias, one byte short of its read and that term is 503
// server_overloaded with less than half the term allocated beyond the body;
// at the booking the check runs and refuses the body (400).
func TestRerankDepthIsBookedBeforeTheCheck(t *testing.T) {
	const (
		levels     = 10000 + 1
		depthState = 24
	)
	body := `{"model":"rr","query":"q","documents":["x"],"top_n":` + strings.Repeat("[", levels-1) + strings.Repeat("]", levels-1) + `}`
	term := int64(levels * depthState)
	need := int64(len(body)) + term
	e := newRerankEngine(t)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 33, 1)
	at := func(budget int64) (uint64, chatAnswer) {
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = budget })
		return allocOf(func() chatAnswer { return rig.postRerank(t, "/v1/rerank", body) })
	}
	refused, a := at(need - 1)
	ran, r := at(need)
	t.Logf("W0210 depth order: body %d B, term %d B; one byte short: %d, %d B allocated; at the booking: %d, %d B allocated",
		len(body), term, a.status, refused, r.status, ran)
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded || e.hits.Load() != 0 {
		t.Fatalf("one byte short of the nesting state: %d %s — want 503 server_overloaded", a.status, a.body)
	}
	if beyond := int64(refused) - int64(len(body)); beyond >= term/2 {
		t.Errorf("refused at the nesting state, the request allocated %d B beyond its body — the check ran before its booking (term %d B)", beyond, term)
	}
	if code, _, _ := r.envelope(t); r.status != http.StatusBadRequest || code != oai.CodeInvalidJSON {
		t.Errorf("at the nesting state's booking: %d %s — want the check's 400 invalid_json", r.status, r.body)
	}
}

// TestRerankTextsAreBookedBeforeTheMeter: the certain part of the wire calls
// is booked at the entry — after the chain (the tenant gate saw the request),
// before the meter and the queue. At the least budget the meter admits the
// request, one byte less is 503 server_overloaded with the chain built, no
// admit, no acquire and no wire call.
func TestRerankTextsAreBookedBeforeTheMeter(t *testing.T) {
	docs := rerankDocsOf(64, 4<<10) // 256 KiB of documents: their wire term (7 × the texts) passes the share of 3 × the body
	body := rerankBody("rr", "q", docs)
	e := newRerankEngine(t)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 200, 1)
	admitted := func() bool {
		before := rig.limiter.admits.Load()
		rig.postRerank(t, "/v1/rerank", body)
		return rig.limiter.admits.Load() > before
	}
	least := leastBudget(t, rig, admitted)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = least - 1 })
	gates, admits, hits := rig.gate.calls.Load(), rig.limiter.admits.Load(), e.hits.Load()
	_ = rig.recorder.Take()
	a := rig.postRerank(t, "/v1/rerank", body)
	n := acquires(rig.recorder.Take())
	t.Logf("W0210 texts order: least admitting budget %d B (body %d B); one byte short: %d %.80s, gate calls +%d, admits +%d, acquires %d, wire +%d",
		least, len(body), a.status, a.body, rig.gate.calls.Load()-gates, rig.limiter.admits.Load()-admits, n, e.hits.Load()-hits)
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
		t.Fatalf("one byte short: %d %s, want 503 server_overloaded", a.status, a.body)
	}
	if rig.gate.calls.Load()-gates != 1 || rig.limiter.admits.Load() != admits || n != 0 || e.hits.Load() != hits {
		t.Errorf("one byte short: gate calls +%d admits +%d acquires %d wire +%d — want the chain built, then the refusal before the meter",
			rig.gate.calls.Load()-gates, rig.limiter.admits.Load()-admits, n, e.hits.Load()-hits)
	}
}

// TestRerankAnswerIsBookedBeforeTheMeter (hardening 1, external review 2
// F-6 a2): the answer's share — n × 64 B and the 16 KiB encoder buffer — is
// booked at the entry, before the meter and the queue. The least budget at
// which the limiter is reached is exactly the sum of every term booked
// before it, computed from the gate's own constants: the read, the nesting
// state, the decoded model, query and documents with their slots, the
// answer, the scan (rrpub is public) and the wire calls' certain part. An
// answer booked after the admit lowers that least budget by its share.
func TestRerankAnswerIsBookedBeforeTheMeter(t *testing.T) {
	const query = "q"
	docs := rerankDocs(1000)
	body := rerankBody("rrpub", query, docs)
	e := newRerankEngine(t)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 2000, 1, withoutCensus)
	admitted := func() bool {
		before := rig.limiter.admits.Load()
		rig.postRerank(t, "/v1/rerank", body)
		return rig.limiter.admits.Load() > before
	}
	least := leastBudget(t, rig, admitted)
	n := int64(len(docs))
	texts := int64(len("rrpub") + len(query)) // decoded: model, query, documents
	wire := int64(len(query) + 2)               // escaped on the wire: query, documents and their commas
	for _, d := range docs {
		texts += int64(len(d))
		wire += int64(len(d)+2) + 1
	}
	const depth = 2
	answer := n*gateResult + gateEncodeBuffer
	certain := (gateBodyNum*wire+gateBodyDen-1)/gateBodyDen + n*gateAnswerDoc
	spent := int64(len(body)) + depth*gateDepthState + texts + n*gateDocSlot + answer +
		gateScanFactor*int64(len(body)) + gateScanBase + depth*gateScanDepth + certain
	want := max(gateReserveFactor*int64(len(body)), spent)
	t.Logf("W0210 answer order: least admitting budget %d B, want %d B (body %d B, answer share %d B, wire certain part %d B; Δ %d)",
		least, want, len(body), answer, certain, least-want)
	if least != want {
		t.Errorf("the limiter is reached at %d B, want exactly the terms before it %d B (Δ %d; the answer's share is %d B — booked after the admit?)",
			least, want, least-want, answer)
	}
}

// TestRerankWireTermIsBookedBeforeTheCall: the wire calls of an attempt are
// booked after the acquire, before the first call. At the least budget that
// serves the request, one byte less is 503 server_overloaded after the
// acquire with no wire call — and it allocated at least the body's twice
// less than the served request (rerank.Score's body never built).
func TestRerankWireTermIsBookedBeforeTheCall(t *testing.T) {
	docs := rerankDocsOf(128, 4<<10)
	body := rerankBody("rr", "q", docs)
	e := newRerankEngine(t)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 200, 1, withoutCensus)
	served := func() bool { return rig.postRerank(t, "/v1/rerank", body).status == http.StatusOK }
	least := leastBudget(t, rig, served)
	at := func(budget int64) (uint64, chatAnswer, int64) {
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = budget })
		hits := e.hits.Load()
		alloc, a := allocOf(func() chatAnswer { return rig.postRerank(t, "/v1/rerank", body) })
		return alloc, a, e.hits.Load() - hits
	}
	ok, okA, okHits := at(least)
	short, a, hits := at(least - 1)
	t.Logf("W0210 wire order: least serving budget %d B (body %d B); served %d, %d B allocated, %d wire calls; one byte short %d, %d B allocated, %d wire calls",
		least, len(body), okA.status, ok, okHits, a.status, short, hits)
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded || hits != 0 {
		t.Fatalf("one byte short: %d %s, %d wire calls — want 503 server_overloaded before the call", a.status, a.body, hits)
	}
	if rig.limiter.rejects.Load()+rig.limiter.aborts.Load() == 0 {
		t.Errorf("one byte short: the meter's reservation was not ended without wire contact")
	}
	if saved := int64(ok) - int64(short); saved < 2*int64(len(body)) {
		t.Errorf("one byte short of the wire term the request allocated only %d B less than served (body %d B) — the call ran before its booking", saved, len(body))
	}
}

// TestRerankLogTextIsBookedBeforeItIsWritten: the log row's text is sized,
// booked, then written — the last booking. With body logging full, at the
// least budget that writes the text the row carries it; one byte less the
// request is served with a row without it.
func TestRerankLogTextIsBookedBeforeItIsWritten(t *testing.T) {
	docs := rerankDocsOf(16, 4<<10)
	body := rerankBody("rr", "q", docs)
	e := newRerankEngine(t)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 33, 1)
	rig.setSettings(func(s *oai.Settings) { s.BodyLogging = llmlog.BodyLoggingFull })
	var text atomic.Int64
	rig.deps.Record = func(r llmlog.GatewayRow) { text.Store(int64(len(r.Request))) }
	rig.router = rig.mountRerank(oai.Rerank(rig.deps, oai.RerankJina), oai.Rerank(rig.deps, oai.RerankCohere))
	withText := func() bool {
		text.Store(0)
		return rig.postRerank(t, "/v1/rerank", body).status == http.StatusOK && text.Load() > 0
	}
	least := leastBudget(t, rig, withText)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = least })
	text.Store(0)
	full := rig.postRerank(t, "/v1/rerank", body)
	fullText := text.Load()
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = least - 1 })
	text.Store(-1)
	cut := rig.postRerank(t, "/v1/rerank", body)
	t.Logf("W0210 log text order: least budget with the text %d B; at it %d, text %d B; one byte short %d, text %d B", least, full.status, fullText, cut.status, text.Load())
	if full.status != http.StatusOK || cut.status != http.StatusOK || fullText == 0 || text.Load() != 0 {
		t.Fatalf("at the booking %d with %d B of text; one byte short %d with %d B — want both served, the text only at the booking", full.status, fullText, cut.status, text.Load())
	}
}

// rerankDocsOf are n distinct documents of size bytes each.
func rerankDocsOf(n, size int) []string {
	out := make([]string, n)
	for i := range out {
		p := "document " + itoa(i) + " "
		out[i] = p + strings.Repeat("x", max(size-len(p), 0))
	}
	return out
}
