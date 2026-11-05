package oai_test

// The embeddings route (W02-09): raw vectors in input order (G10), float
// and base64 bit-identical (G1, the route half), dimensions with L2
// renormalization (G7), the input refusals (G7), the weight plan per lease
// at the real dispatcher (G12, O-09-1), the k = 1 rule without the
// continuation door (G5) and the sticky stop after block one (G6).

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// TestEmbeddingsServeRawVectorsInOrder (G10, route half): the texts reach
// the engine as sent — no instruct prefix, as a list — and every vector
// comes back at the native length, unnormalized, at its input's index; the
// answer names the alias, usage is the engine's sum, the row is one
// gateway-embeddings row.
func TestEmbeddingsServeRawVectorsInOrder(t *testing.T) {
	for _, proto := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama} {
		t.Run(string(proto), func(t *testing.T) {
			e := newEmbedEngine(t, 4096)
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", proto, 10)}, 33, 1)
			inputs := embedInputs(5)
			r := decodeEmbed(t, rig.postEmbed(t, embedBody("emb4k", inputs)))
			if r.Object != "list" || r.Model != "emb4k" || len(r.Data) != 5 {
				t.Fatalf("answer object=%q model=%q data=%d", r.Object, r.Model, len(r.Data))
			}
			for i, v := range r.vectors(t) {
				want := engineVec(inputs[i], 4096)
				if !slices.Equal(v, want) {
					t.Fatalf("data[%d] is not the engine's raw vector of %q (len %d)", i, inputs[i], len(v))
				}
			}
			if r.Usage.PromptTokens != 10 || r.Usage.TotalTokens != 10 {
				t.Errorf("usage %+v, want 10/10", r.Usage)
			}
			calls := e.allCalls()
			if len(calls) != 1 || !slices.Equal(calls[0], inputs) {
				t.Fatalf("engine calls %v, want one call with the texts as sent", calls)
			}
			for _, b := range e.bodies {
				if strings.Contains(b, "Instruct") || strings.Contains(b, `"input":"`) {
					t.Errorf("engine body %s: a prefix or a bare string on the raw path", b)
				}
			}
			rows := rig.rows.all()
			if len(rows) != 1 || rows[0].Pipeline != llmlog.PipelineGatewayEmbeddings || rows[0].Telemetry.PromptTokens != 10 {
				t.Fatalf("rows %+v, want one gateway-embeddings row with 10 prompt tokens", rows)
			}
			if rec := <-rig.recs; rec.Op != llmlog.GatewayOpEmbed || rec.Alias != "emb4k" || rec.PromptTokens != 10 {
				t.Errorf("access record %+v", rec)
			}
		})
	}
}

// TestEmbeddingsBase64IsTheFloatBits (G1, route half): base64 is the
// little-endian float32 bytes of exactly the vector the float answer lists —
// bit for bit, also after a dimensions cut.
func TestEmbeddingsBase64IsTheFloatBits(t *testing.T) {
	e := newEmbedEngine(t, 4096)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	for _, dims := range []string{"", `"dimensions":512`} {
		var extraF, extraB []string
		if dims != "" {
			extraF, extraB = []string{dims}, []string{dims}
		}
		extraB = append(extraB, `"encoding_format":"base64"`)
		extraF = append(extraF, `"encoding_format":"float"`)
		f := decodeEmbed(t, rig.postEmbed(t, embedBody("emb4k", embedInputs(3), extraF...))).vectors(t)
		b := decodeEmbed(t, rig.postEmbed(t, embedBody("emb4k", embedInputs(3), extraB...))).vectors(t)
		for i := range f {
			if len(f[i]) != len(b[i]) {
				t.Fatalf("%s: vector %d: float %d dims, base64 %d", dims, i, len(f[i]), len(b[i]))
			}
			for j := range f[i] {
				if math.Float32bits(f[i][j]) != math.Float32bits(b[i][j]) {
					t.Fatalf("%s: vector %d component %d: float %v, base64 %v", dims, i, j, f[i][j], b[i][j])
				}
			}
		}
	}
}

// TestEmbeddingsDimensionsRenormalized (G7): dimensions d cuts v[:d] and
// renormalizes — norm 1 ± 1e-6, direction of the prefix kept.
func TestEmbeddingsDimensionsRenormalized(t *testing.T) {
	e := newEmbedEngine(t, 4096)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	inputs := embedInputs(4)
	for _, d := range []int{256, 1000, 4096} {
		vs := decodeEmbed(t, rig.postEmbed(t, embedBody("emb4k", inputs, `"dimensions":`+itoa(d)))).vectors(t)
		for i, v := range vs {
			if len(v) != d {
				t.Fatalf("d=%d: vector %d has %d dims", d, i, len(v))
			}
			if d == 4096 {
				if !slices.Equal(v, engineVec(inputs[i], 4096)) {
					t.Errorf("d=native: vector %d changed (no cut, no renormalization)", i)
				}
				continue
			}
			var n, dot, ref float64
			raw := engineVec(inputs[i], 4096)[:d]
			for j := range v {
				n += float64(v[j]) * float64(v[j])
				dot += float64(v[j]) * float64(raw[j])
				ref += float64(raw[j]) * float64(raw[j])
			}
			if math.Abs(math.Sqrt(n)-1) > 1e-6 {
				t.Errorf("d=%d: vector %d has norm %.9f, want 1 ± 1e-6", d, i, math.Sqrt(n))
			}
			if cos := dot / math.Sqrt(n*ref); cos < 1-1e-6 {
				t.Errorf("d=%d: vector %d left the direction of v[:d] (cos %.9f)", d, i, cos)
			}
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// TestEmbeddingsInputRefusals (G7): every refusal is a 400 with its param,
// before any wire contact and without a row.
func TestEmbeddingsInputRefusals(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	long := strings.Repeat("ä", 17)
	for _, tc := range []struct {
		name, body, code, param string
	}{
		{"dimensions above native", `{"model":"emb4k","input":"x","dimensions":4097}`, oai.CodeUnsupportedValue, "dimensions"},
		{"dimensions below matryoshka_min", `{"model":"emb4k","input":"x","dimensions":255}`, oai.CodeUnsupportedValue, "dimensions"},
		{"dimensions without a declared native", `{"model":"emb","input":"x","dimensions":8}`, oai.CodeUnsupportedValue, "dimensions"},
		{"dimensions cut without matryoshka_min", `{"model":"embsmall","input":"x","dimensions":4}`, oai.CodeUnsupportedValue, "dimensions"},
		{"dimensions zero", `{"model":"emb4k","input":"x","dimensions":0}`, oai.CodeUnsupportedValue, "dimensions"},
		{"dimensions a float", `{"model":"emb4k","input":"x","dimensions":256.5}`, oai.CodeInvalidType, "dimensions"},
		{"token array", `{"model":"emb4k","input":[1,2,3]}`, oai.CodeUnsupportedValue, "input"},
		{"token arrays", `{"model":"emb4k","input":[[1,2],[3]]}`, oai.CodeUnsupportedValue, "input"},
		{"empty string", `{"model":"emb4k","input":""}`, oai.CodeUnsupportedValue, "input"},
		{"empty element", `{"model":"emb4k","input":["a",""]}`, oai.CodeUnsupportedValue, "input[1]"},
		{"empty list", `{"model":"emb4k","input":[]}`, oai.CodeUnsupportedValue, "input"},
		{"mixed list", `{"model":"emb4k","input":["a",1]}`, oai.CodeInvalidType, "input[1]"},
		{"object", `{"model":"emb4k","input":{"a":1}}`, oai.CodeInvalidType, "input"},
		{"no input", `{"model":"emb4k"}`, oai.CodeMissingRequiredParameter, "input"},
		{"no model", `{"input":"x"}`, oai.CodeMissingRequiredParameter, "model"},
		{"too many inputs", embedBody("embsmall", embedInputs(5)), oai.CodeUnsupportedValue, "input"},
		{"input over max_input_runes", embedBody("embsmall", []string{"ok", long}), oai.CodeUnsupportedValue, "input[1]"},
		{"single string over max_input_runes", `{"model":"embsmall","input":"` + long + `"}`, oai.CodeUnsupportedValue, "input[0]"},
		{"unknown encoding_format", `{"model":"emb4k","input":"x","encoding_format":"int8"}`, oai.CodeUnsupportedValue, "encoding_format"},
		{"unknown parameter", `{"model":"emb4k","input":"x","truncate":true}`, oai.CodeUnsupportedParameter, "truncate"},
		{"case variant of a key", `{"model":"emb4k","Input":"x"}`, oai.CodeUnsupportedParameter, "Input"},
		{"repeated key", `{"model":"emb4k","input":"x","input":"y"}`, oai.CodeInvalidJSON, "input"},
		{"no object", `["x"]`, oai.CodeInvalidJSON, ""},
	} {
		a := rig.postEmbed(t, tc.body)
		code, param, _ := a.envelope(t)
		if a.status != http.StatusBadRequest || code != tc.code || param != tc.param {
			t.Errorf("%s: %d %s/%q (%s), want 400 %s/%q", tc.name, a.status, code, param, a.body, tc.code, tc.param)
		}
	}
	if h := e.hits.Load(); h != 0 {
		t.Errorf("wire hits %d, want 0", h)
	}
	if n := len(rig.rows.all()); n != 0 {
		t.Errorf("llmlog rows %d, want 0", n)
	}
	// The kind and the 404 come from the resolve.
	if a := rig.postEmbed(t, `{"model":"chat","input":"x"}`); a.status != http.StatusBadRequest {
		t.Errorf("chat alias: %d %s, want 400 model_not_supported", a.status, a.body)
	} else if code, _, _ := a.envelope(t); code != oai.CodeModelNotSupported {
		t.Errorf("chat alias: code %s, want model_not_supported", code)
	}
	if a := rig.postEmbed(t, `{"model":"nope","input":"x"}`); a.status != http.StatusNotFound {
		t.Errorf("unknown alias: %d, want 404", a.status)
	}
	// user is dropped and named; it never reaches the engine.
	a := rig.postEmbed(t, `{"model":"embsmall","input":"x","user":"u-123"}`)
	if a.status != http.StatusOK || a.header.Get("X-Ctx-Ignored-Params") != "user" {
		t.Errorf("user: %d ignored=%q, want 200 with X-Ctx-Ignored-Params: user", a.status, a.header.Get("X-Ctx-Ignored-Params"))
	}
	if strings.Contains(strings.Join(e.bodies, ""), "u-123") {
		t.Error("the user field reached the engine")
	}
}

// heldWeights records, per engine call, the external weight the dispatcher
// holds on host while the call runs (Snapshot, the real dispatcher).
func heldWeights(rig *chatRig, e *embedEngine) func() []int {
	var mu sync.Mutex
	var held []int
	e.setRespond(func(http.ResponseWriter, *http.Request, int64, []string) bool {
		for _, ts := range rig.disp.Snapshot().Targets {
			if ts.Origin == e.srv.URL {
				mu.Lock()
				held = append(held, ts.HeldExternalWeight)
				mu.Unlock()
			}
		}
		return false
	})
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), held...)
	}
}

// TestEmbeddingsWeightPerLease (G12, O-09-1 (b)): on a target with E = 1
// every lease carries one input with w = 1 — 32 inputs are 200 over 32
// leases (one First, 31 continuations); with E = 4 the blocks are
// min(sub_batch, 4) — the planner's clamp, held at the real dispatcher.
func TestEmbeddingsWeightPerLease(t *testing.T) {
	for _, tc := range []struct {
		name            string
		slots, reserved int
		n               int
		block           int
	}{
		{"E=1, 32 inputs", 2, 1, 32, 1},
		{"E=4, 10 inputs", 5, 1, 10, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, tc.slots, tc.reserved)
			held := heldWeights(rig, e)
			_ = rig.recorder.Take()
			r := decodeEmbed(t, rig.postEmbed(t, embedBody("emb", embedInputs(tc.n))))
			if len(r.Data) != tc.n {
				t.Fatalf("%d vectors, want %d", len(r.Data), tc.n)
			}
			k := (tc.n + tc.block - 1) / tc.block
			var weights []int
			conts := 0
			for _, ev := range rig.recorder.Take() {
				if ev.Kind == v1census.KindAcquire {
					weights = append(weights, ev.Weight)
					if ev.Continuation {
						conts++
					}
				}
			}
			if len(weights) != k || conts != k-1 {
				t.Fatalf("acquires %d (continuations %d), want %d (%d)", len(weights), conts, k, k-1)
			}
			for i, w := range weights {
				want := min(tc.block, tc.n-i*tc.block)
				if w != want {
					t.Errorf("lease %d: weight %d, want %d", i, w, want)
				}
			}
			calls := e.allCalls()
			if len(calls) != k {
				t.Fatalf("engine calls %d, want %d", len(calls), k)
			}
			for i, c := range calls {
				if len(c) != weights[i] {
					t.Errorf("call %d carried %d inputs, its lease weighs %d", i, len(c), weights[i])
				}
			}
			if h := held(); !slices.Equal(h, weights) {
				t.Errorf("held external weight per call %v, want the lease weights %v", h, weights)
			}
			rows := rig.rows.all()
			if len(rows) != 1 || rows[0].Telemetry.Metadata["sub_batches"] != k || rows[0].Telemetry.Metadata["sub_batches_done"] != k {
				t.Fatalf("row metadata %v, want sub_batches = sub_batches_done = %d", rows[0].Telemetry.Metadata, k)
			}
			if rows[0].Telemetry.PromptTokens != 2*tc.n {
				t.Errorf("row tokens %d, want %d (Σ over the blocks)", rows[0].Telemetry.PromptTokens, 2*tc.n)
			}
		})
	}
}

// noContinuationPlanner is the real planner whose plan says it has no
// continuation door (O-09-2: Plan.Continuation false).
type noContinuationPlanner struct{ oai.AdmissionPlanner }

func (p noContinuationPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	plan.Continuation = false
	return plan, err
}

// TestEmbeddingsKOneWithoutContinuation (G5, the masterplan gate "k > 1
// without continuation ⇒ 400"): 33 inputs at sub_batch 32 ⇒ 400
// unsupported_value naming the bound, before any acquire, 0 wire hits; and
// the clamp case — the weight plan has more than one block after First
// (E = 4 below sub_batch) ⇒ the same 400 after the acquire, still 0 wire
// hits, no row, the lease back.
func TestEmbeddingsKOneWithoutContinuation(t *testing.T) {
	t.Run("past sub_batch", func(t *testing.T) {
		e := newEmbedEngine(t, 4096)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 40, 1,
			func(r *chatRig) { r.deps.Planner = noContinuationPlanner{r.planner} })
		_ = rig.recorder.Take()
		a := rig.postEmbed(t, embedBody("emb4k", embedInputs(33)))
		code, param, _ := a.envelope(t)
		if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedValue || param != "input" || !strings.Contains(string(a.body), "at most 32 inputs per request") {
			t.Fatalf("%d %s, want 400 unsupported_value input \"at most 32 inputs per request\"", a.status, a.body)
		}
		if n := acquires(rig.recorder.Take()); n != 0 {
			t.Errorf("acquires %d, want 0", n)
		}
		if e.hits.Load() != 0 || len(rig.rows.all()) != 0 {
			t.Errorf("wire hits %d rows %d, want 0/0", e.hits.Load(), len(rig.rows.all()))
		}
		if l := rig.limiter; l.aborts.Load() != 1 || l.settles.Load() != 0 {
			t.Errorf("limiter aborts %d settles %d, want 1/0", l.aborts.Load(), l.settles.Load())
		}
		// 32 inputs fit one upstream request: served.
		decodeEmbed(t, rig.postEmbed(t, embedBody("emb4k", embedInputs(32))))
	})
	t.Run("past the clamp", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 5, 1,
			func(r *chatRig) { r.deps.Planner = noContinuationPlanner{r.planner} })
		a := rig.postEmbed(t, embedBody("emb", embedInputs(6)))
		code, param, _ := a.envelope(t)
		if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedValue || param != "input" || !strings.Contains(string(a.body), "at most 4 inputs per request") {
			t.Fatalf("%d %s, want 400 unsupported_value input \"at most 4 inputs per request\"", a.status, a.body)
		}
		if e.hits.Load() != 0 || len(rig.rows.all()) != 0 {
			t.Errorf("wire hits %d rows %d, want 0/0", e.hits.Load(), len(rig.rows.all()))
		}
		for _, ts := range rig.disp.Snapshot().Targets {
			if ts.Held != 0 {
				t.Errorf("%s holds %d leases after the refusal", ts.Origin, ts.Held)
			}
		}
	})
}

// TestEmbeddingsStickyAfterBlockOne (G6): block one served on row A, block
// two fails there (502) ⇒ 503 upstream_unavailable with Retry-After, no
// wire hit on row B — two vector builds never mix; the row says the block
// count reached. Block one failing instead fails over to row B (not yet
// committed).
func TestEmbeddingsStickyAfterBlockOne(t *testing.T) {
	a := newEmbedEngine(t, 8)
	b := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{a.row("row-a", backends.ProtocolOpenAI, 20), b.row("row-b", backends.ProtocolOpenAI, 10)}, 3, 1)
	a.setRespond(func(w http.ResponseWriter, _ *http.Request, n int64, _ []string) bool {
		if n == 2 {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		return false
	})
	ans := rig.postEmbed(t, embedBody("emb", embedInputs(5)))
	code, _, _ := ans.envelope(t)
	if ans.status != http.StatusServiceUnavailable || code != oai.CodeUpstreamUnavailable || ans.header.Get("Retry-After") == "" ||
		ans.header.Get("x-should-retry") != "true" {
		t.Fatalf("%d %s (Retry-After %q), want 503 upstream_unavailable with Retry-After", ans.status, ans.body, ans.header.Get("Retry-After"))
	}
	if b.hits.Load() != 0 {
		t.Errorf("row B saw %d wire hits after block one was served on row A", b.hits.Load())
	}
	rows := rig.rows.all()
	if len(rows) != 1 || rows[0].Telemetry.Metadata["sub_batches_done"] != 1 || rows[0].Err == nil || rows[0].Err.Code != oai.CodeUpstreamUnavailable {
		t.Fatalf("row %+v, want one error row, one block done", rows)
	}
	if rows[0].Telemetry.PromptTokens != 4 {
		t.Errorf("row tokens %d, want 4 (block one's two inputs)", rows[0].Telemetry.PromptTokens)
	}
	if l := rig.limiter; l.settles.Load() != 1 {
		t.Errorf("limiter settles %d, want 1 (wire work happened)", l.settles.Load())
	}
	// Block one failing on A (a fresh rig: the 502 above cooled A down):
	// failover to B before the commit, the whole request there.
	a2 := newEmbedEngine(t, 8)
	b2 := newEmbedEngine(t, 8)
	rig2 := newEmbedRig(t, []backends.Backend{a2.row("row-a", backends.ProtocolOpenAI, 20), b2.row("row-b", backends.ProtocolOpenAI, 10)}, 3, 1)
	a2.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	r := decodeEmbed(t, rig2.postEmbed(t, embedBody("emb", embedInputs(5))))
	if len(r.Data) != 5 || a2.hits.Load() != 1 || b2.hits.Load() != 3 {
		t.Errorf("failover before the commit: data %d, hits A %d B %d, want 5, 1, 3", len(r.Data), a2.hits.Load(), b2.hits.Load())
	}
}
