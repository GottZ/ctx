package oai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// embedLedgerRequest is the request part of an embeddings request's
// reservation (FOLGE-E, the route's ledger): the budget stage's share,
// requestReserveFactor × the body, or what the route booked against it when
// that is more — the read of the body, the JSON check's nesting state at
// depth, every text of inputs (plain ASCII: decoded and encoded as sent) and
// its slot, the scan's booking below credentials (scan) and the upstream
// bodies: blocks 0 none yet, −1 their texts (booked before the acquire), k
// the bodies of an attempt of k blocks to model on the openai wire — every
// text, a separator between two texts of a block, a frame with the model per
// block, each body once (FOLGE-E hardening 1: one allocation of its exact
// length) — with the allocation's slack, the transport's copy of the body
// (every block body of these tests is shorter than its 32 KiB buffer, so
// the copy is the body's length) and the wire call per block. The
// constants are the gate's own: a mutated term in the route must not move
// the expectation with it.
func embedLedgerRequest(body string, depth int, inputs []string, scan bool, blocks int, model string) int64 {
	const (
		reserveFactor  = 3       // the budget stage's request share per body byte
		depthState     = 24      // json.Valid's state per nesting level
		slot           = 16 + 8  // a string header and a rune count per input
		scanFactor     = 23      // the scan per body byte
		scanBase       = 128 << 10 // FOLGE-P hardening 8 (root d0d25650): the scan's base doubled
		scanDepth      = 104     // the scan per nesting level
		frame          = 23       // a block body's frame on the openai wire: {"model":"","input":[]}
		upstreamSlack  = 8 << 10  // the size-class rounding of a block body's one allocation
		wireCall       = 14 << 10 // one wire call beside its body and its copy
	)
	b := int64(len(body))
	spent := b + int64(depth)*depthState
	for _, s := range inputs {
		spent += int64(len(s)) + slot
	}
	if scan {
		spent += scanFactor*b + scanBase + int64(depth)*scanDepth
	}
	if blocks != 0 {
		var enc int64
		for _, s := range inputs {
			enc += int64(len(s)) + 2 + 1 // quotes and separator
		}
		if blocks > 0 {
			enc += int64(blocks)*int64(frame+len(model)) - int64(blocks) // a separator fewer per block
			spent += enc + int64(blocks)*(upstreamSlack+wireCall)         // the copy of each body, the slack, the call
		}
		spent += enc
	}
	return max(reserveFactor*b, spent)
}

// TestEmbedLogRowTextIsBooked: with body logging full the reservation a
// served request holds at its end is its request part, its answer and
// decode shares and the log row's text of the input (the input's raw JSON
// for plain ASCII texts) — booked before the row's text is written.
func TestEmbedLogRowTextIsBooked(t *testing.T) {
	inputs := embedInputs(4)
	e := newEmbedEngine(t, 8)
	var held heldSum
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	rig.reg.reload(t, embedScanAliases())
	body := embedBody("embsec", inputs)
	in, _ := json.Marshal(inputs)
	for _, tc := range []struct {
		logging string
		text    int64
	}{{llmlog.BodyLoggingNone, 0}, {llmlog.BodyLoggingFull, int64(len(in))}} {
		rig.setSettings(func(s *oai.Settings) { s.BodyLogging = tc.logging })
		rig.router = embedMemMount(rig, oai.Embeddings(rig.deps), &held)
		before, _ := held.sum()
		req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
		w := httptest.NewRecorder()
		rig.router.ServeHTTP(w, req)
		after, _ := held.sum()
		const answer = 4*8*4 + 64<<10                                   // 4 inputs × native 8 × 4 B + the encoder buffer
		const decode = 3 * (4*8*25 + 4*256 + 4<<10)                     // 3 × the read cap of the one block of 4
		want := embedLedgerRequest(body, 2, inputs, false, 1, "EMBED-MODEL") + tc.text + answer + decode
		if w.Code != http.StatusOK || after-before != want {
			t.Errorf("body logging %s: %d, the request held %d B at its end, want %d (log text %d B)", tc.logging, w.Code, after-before, want, tc.text)
		}
	}
}

// TestEmbedWireReplicaIsTheWire pins oai.EmbedWireReplica (the measured
// encoder of the ledger's pins) to the upstream bodies the route sends, byte
// for byte, on the openai and the ollama wire, for texts of
// every escape class.
func TestEmbedWireReplicaIsTheWire(t *testing.T) {
	inputs := []string{"a", "<b>&\"c\"", "\u2028\x01\n\t", "ä😀", "\ufffd\x7f", strings.Repeat("x", 3000)}
	for _, p := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama} {
		e := newEmbedEngine(t, 8)
		row := e.row("spark-embed", p, 10)
		rig := newEmbedRig(t, []backends.Backend{row}, 33, 1)
		rig.reg.reload(t, embedScanAliases())
		a := rig.postEmbed(t, embedBody("embsec", inputs))
		e.mu.Lock()
		bodies := append([]string(nil), e.bodies...)
		e.mu.Unlock()
		if a.status != http.StatusOK || len(bodies) != 1 {
			t.Fatalf("%s: %d, %d upstream bodies (want 200, one block)", p, a.status, len(bodies))
		}
		if want := oai.EmbedWireReplica(row.Model, inputs, p == backends.ProtocolOllama, row.NumCtx); bodies[0] != string(want) {
			t.Errorf("%s: the route sent %q, the replica writes %q", p, bodies[0], want)
		}
	}
}

// TestEmbedFailoverBooksEveryAttempt (FOLGE-E (d)): a first row that fails
// its block (503) and a second that serves it — the reservation at the end
// holds the upstream bodies of both attempts: the texts booked before the
// acquire serve the first attempt only, the second books its texts, its
// frame and its wire call again (the first attempt's body stays on the heap
// until the collector runs).
func TestEmbedFailoverBooksEveryAttempt(t *testing.T) {
	inputs := embedInputs(4)
	bad, good := newEmbedEngine(t, 8), newEmbedEngine(t, 8)
	bad.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	var held heldSum
	rig := newEmbedRig(t, []backends.Backend{bad.row("bad", backends.ProtocolOpenAI, 100), good.row("good", backends.ProtocolOpenAI, 10)}, 33, 1)
	rig.reg.reload(t, embedScanAliases())
	rig.setSettings(func(s *oai.Settings) { s.BodyLogging = llmlog.BodyLoggingNone })
	rig.router = embedMemMount(rig, oai.Embeddings(rig.deps), &held)
	body := embedBody("embsec", inputs)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)
	got, _ := held.sum()
	const answer = 4*8*4 + 64<<10               // 4 inputs × native 8 × 4 B + the encoder buffer
	const decode = 3 * (4*8*25 + 4*256 + 4<<10) // 3 × the read cap of the one block of 4
	one := embedLedgerRequest(body, 2, inputs, false, 1, "EMBED-MODEL")
	again := one - embedLedgerRequest(body, 2, inputs, false, 0, "") // the second attempt's texts, frame and wire call
	want := one + again + answer + decode
	if w.Code != http.StatusOK || bad.hits.Load() != 1 || good.hits.Load() != 1 || got != want {
		t.Errorf("%d, hits bad %d good %d; the request held %d B at its end, want %d (one attempt %d B more)", w.Code, bad.hits.Load(), good.hits.Load(), got, want, again)
	}
}
