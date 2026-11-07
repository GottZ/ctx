package oai_test

// The order pins of the embeddings route's ledger (FOLGE-E hardening 2,
// L-1; Fremd-Review 1 mutants E03–E05): a booking that slides behind the
// allocation (or the queue) it covers changes no answer — every one of
// these requests is 503 server_overloaded either way, or served either way
// — so each pin measures what the request allocated, or which stage it
// reached, until the refusal.

import (
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// allocOf is the least TotalAlloc of three calls of f, with f's last
// result.
func allocOf[T any](f func() T) (least uint64, last T) {
	least = ^uint64(0)
	for range 3 {
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		last = f()
		runtime.ReadMemStats(&m1)
		least = min(least, m1.TotalAlloc-m0.TotalAlloc)
	}
	return least, last
}

// TestEmbedDepthIsBookedBeforeTheCheck (E05): the JSON check's nesting
// state — 24 B per level — is booked before the check runs. json.Valid
// keeps that state on a body that fails at the decoder's nesting limit
// (190 144 B at the limit on go1.27.3, TestJSONStateIsBooked; a valid
// body of the same depth allocates about 2 KiB): a 20 KB body nested one
// level past the limit, to the credentials alias (no scan), at a budget one
// byte short of its read and that term is 503 server_overloaded with less
// than half the term allocated beyond the body — the check never ran; at
// the booking the check runs and refuses the body (400), its state
// allocated.
func TestEmbedDepthIsBookedBeforeTheCheck(t *testing.T) {
	const (
		levels     = 10000 + 1 // one past the decoder's nesting limit, as far as the route counts
		depthState = 24        // the check's state per level, the gate's own constant
	)
	body := `{"model":"embsec","input":"x","user":` + strings.Repeat("[", levels-1) + strings.Repeat("]", levels-1) + `}`
	term := int64(levels * depthState)
	need := int64(len(body)) + term // the read, then the nesting state — past the share of 3 × the body
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	rig.reg.reload(t, embedScanAliases())
	at := func(budget int64) (uint64, chatAnswer) {
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = budget })
		return allocOf(func() chatAnswer { return rig.postEmbed(t, body) })
	}
	refused, a := at(need - 1)
	ran, r := at(need)
	t.Logf("FOLGEE depth order: body %d B, term %d B; one byte short: %d, %d B allocated; at the booking: %d, %d B allocated",
		len(body), term, a.status, refused, r.status, ran)
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded || e.hits.Load() != 0 {
		t.Fatalf("one byte short of the nesting state: %d %s, %d wire hits — want 503 server_overloaded, none", a.status, a.body, e.hits.Load())
	}
	if beyond := int64(refused) - int64(len(body)); beyond >= term/2 {
		t.Errorf("refused at the nesting state, the request allocated %d B beyond its body — the check ran before its booking (term %d B)", beyond, term)
	}
	if code, _, _ := r.envelope(t); r.status != http.StatusBadRequest || code != oai.CodeInvalidJSON {
		t.Errorf("at the nesting state's booking: %d %s — want the check's 400 invalid_json", r.status, r.body)
	}
	if beyond := int64(ran) - int64(len(body)); beyond < term/2 {
		t.Errorf("past the nesting state's booking the request allocated only %d B beyond its body — the pin does not see the check (term %d B)", beyond, term)
	}
}

// TestEmbedLogRowTextIsBookedBeforeItIsWritten (E04): the log row's text of
// the input is sized, booked, then written. With body logging full, a
// request whose reservation at its end holds H is served at a budget of H
// with the text in its row and at H − 1 without it — the text is the last
// booking — and at H − 1 it allocates at least half the text less: the text
// was never built.
func TestEmbedLogRowTextIsBookedBeforeItIsWritten(t *testing.T) {
	inputs := repeatInputs(strings.Repeat("a", 32<<10), 4) // 128 KiB of input: the row's text is cut at its cap
	srv := leanEmbedEngine(t, 8)
	row := backends.Backend{
		ID: "id-lean", Name: "lean", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "EMBED-MODEL",
		Roles: []string{"embed"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"embed": 30},
	}
	rig := newEmbedRig(t, []backends.Backend{row}, 33, 1)
	rig.reg.reload(t, embedScanAliases())
	var text atomic.Int64
	rig.deps.Record = func(r llmlog.GatewayRow) { text.Store(int64(len(r.Request))) }
	var held heldSum
	rig.router = embedMemMount(rig, oai.Embeddings(rig.deps), &held)
	body := embedBody("embsec", inputs)
	type run struct {
		status     int
		held, text int64
	}
	at := func(budget int64) (uint64, run) {
		rig.setSettings(func(s *oai.Settings) { s.BodyLogging, s.InflightBudget = llmlog.BodyLoggingFull, budget })
		return allocOf(func() run {
			before, _ := held.sum()
			a := rig.postEmbed(t, body)
			after, _ := held.sum()
			return run{a.status, after - before, text.Load()}
		})
	}
	_, big := at(memBudget)
	full, f := at(big.held)
	cut, c := at(big.held - 1)
	t.Logf("FOLGEE log text order: held %d B; at it: %d, text %d B, %d B allocated; one byte short: %d, text %d B, held %d B, %d B allocated",
		big.held, f.status, f.text, full, c.status, c.text, c.held, cut)
	if f.status != http.StatusOK || c.status != http.StatusOK || f.text == 0 || c.text != 0 || f.held-c.held != f.text {
		t.Fatalf("at the reservation: %d with a text of %d B; one byte short: %d with %d B, %d B less held — want both served, the text only at the reservation, held less by the text", f.status, f.text, c.status, c.text, f.held-c.held)
	}
	if saved := int64(full) - int64(cut); saved < f.text/2 {
		t.Errorf("without its booking the row's text saved %d B of allocation, the text is %d B — it was built before its booking", saved, f.text)
	}
}

// TestEmbedTextsAreBookedBeforeTheMeter (E03): the texts' part of the
// upstream bodies is booked at the entry, before the request meets the
// consumer's meter and the queue. One byte short of the entry's shares
// (TestEmbeddingsUnknownDimensionReservesTheCeiling's sum, the texts its
// last booking) is 503 server_overloaded with no meter admit, no acquire and
// no wire call; at the entry's shares the request is admitted.
func TestEmbedTextsAreBookedBeforeTheMeter(t *testing.T) {
	const (
		ceiling      = 1 << 24  // gateway.embedding_max_floats for the test
		floatBytes   = 4        // one float32 component held for the answer
		encodeBuffer = 64 << 10 // the answer's encoder buffer
		decodeFactor = 3        // the decoder buffer's doubling
		transportCap = 16 << 20 // httpx.MaxResponseBytes: a block's read cap at this ceiling
	)
	inputs := []string{strings.Repeat("x", 64)}
	body := embedBody("emb", inputs)
	answer := int64(ceiling*floatBytes+encodeBuffer) + decodeFactor*transportCap
	before := embedLedgerRequest(body, 2, inputs, false, 0, "") + answer
	entry := embedLedgerRequest(body, 2, inputs, false, -1, "") + answer
	if entry <= before {
		t.Fatalf("the texts (%d B with them, %d B without) do not grow the reservation past the share — the pin needs them to", entry, before)
	}
	for _, tc := range []struct {
		name   string
		budget int64
		admits int64
	}{
		{"one byte short of the entry's shares", entry - 1, 0},
		{"the entry's shares", entry, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
			rig.setSettings(func(s *oai.Settings) { s.EmbeddingMaxFloats = ceiling; s.InflightBudget = tc.budget })
			_ = rig.recorder.Take()
			a := rig.postEmbed(t, body)
			n := acquires(rig.recorder.Take())
			admits := rig.limiter.admits.Load()
			t.Logf("FOLGEE texts order: budget %d (entry %d, without the texts %d): %d %.100s; meter admits %d, acquires %d, wire hits %d", tc.budget, entry, before, a.status, a.body, admits, n, e.hits.Load())
			if admits != tc.admits {
				t.Errorf("meter admits %d, want %d — the texts are booked before the meter", admits, tc.admits)
			}
			if tc.admits == 0 {
				if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded || n != 0 || e.hits.Load() != 0 {
					t.Errorf("%d %s, acquires %d, wire hits %d — want 503 server_overloaded before the queue", a.status, a.body, n, e.hits.Load())
				}
			}
		})
	}
}
