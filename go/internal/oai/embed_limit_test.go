package oai_test

// FOLGE-EMBLIMIT: the embeddings route lets the backend judge the length of
// an input. Without embedding.max_input_runes the route sets no length of its
// own — no rune budget derived from a row's window, no fallback window, no
// code ceiling (G1); with it the alias's value is the early 400 it was (G2).
// The backend's refusal of a sub-block's inputs is the client's 400 at every
// sub-block, naming the input (G4, G5); every other failure of a later
// sub-block stays the sticky 503 (G5). An Ollama row is asked not to cut an
// input (truncate:false) and its refusal is recognized (G7). The body of G4
// is the fixture the client side classifies (G8, internal/store).

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// llamaOversizeBody is llama.cpp's refusal of an input past its context
// size, as the live embedder answered it (2026-11-10, --ctx-size 32768).
const llamaOversizeBody = `{"error":{"code":400,"message":"request (33177 tokens) exceeds the available context size (32768 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":33177,"n_ctx":32768}}`

// embedOversizeFixture is the body G4's single-input request gets; the
// client side reads the same file (internal/store, G8).
var embedOversizeFixture = filepath.Join("testdata", "embed_context_length_exceeded.json")

// refuseCall answers call n of the engine with status and body.
func refuseCall(n int64, status int, body string) func(http.ResponseWriter, *http.Request, int64, []string) bool {
	return func(w http.ResponseWriter, _ *http.Request, got int64, _ []string) bool {
		if got != n {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return true
	}
}

// lastRecord is the access record of the latest request on rig.
func lastRecord(t *testing.T, rig *chatRig) *oai.AccessRecord {
	t.Helper()
	var rec *oai.AccessRecord
	for {
		select {
		case r := <-rig.recs:
			rec = r
		default:
			if rec == nil {
				t.Fatal("no access record")
			}
			return rec
		}
	}
}

// TestEmbedNoRuneLimitReachesTheBackend (G1): an alias without
// embedding.max_input_runes sets no length — an input of 200 000 characters
// reaches the backend on a row of a known window (2048 tokens, whose 9/5
// budget was 3686 characters) and on a row of an unknown one (the fallback
// window), whole.
func TestEmbedNoRuneLimitReachesTheBackend(t *testing.T) {
	long := strings.Repeat("x", 200_000)
	for _, numCtx := range []int{2048, 0} {
		e := newEmbedEngine(t, 8)
		row := e.row("spark-embed", backends.ProtocolOpenAI, 10)
		row.NumCtx = numCtx
		rig := newEmbedRig(t, []backends.Backend{row}, 2, 1)
		a := rig.postEmbed(t, embedBody("emb", []string{long}))
		if a.status != http.StatusOK {
			t.Fatalf("num_ctx %d: answer %d %.300s, want 200 (the backend judges the length)", numCtx, a.status, a.body)
		}
		calls := e.allCalls()
		if len(calls) != 1 || len(calls[0]) != 1 || calls[0][0] != long {
			t.Fatalf("num_ctx %d: the engine got %d calls, want one with the whole input", numCtx, len(calls))
		}
		if r := decodeEmbed(t, a); len(r.Data) != 1 {
			t.Errorf("num_ctx %d: %d vectors, want 1", numCtx, len(r.Data))
		}
	}
}

// TestEmbedAliasRuneLimitIsTheEarlyRefusal (G2): with
// embedding.max_input_runes N (embsmall: 16) an input of N characters is
// served and one of N + 1 is 400 unsupported_value at its input[i], with no
// wire call — the early check before the reserve (TestEmbedRuneLimitBeforeTheReserve).
func TestEmbedAliasRuneLimitIsTheEarlyRefusal(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	if r := decodeEmbed(t, rig.postEmbed(t, embedBody("embsmall", []string{"ok", strings.Repeat("ä", 16)}))); len(r.Data) != 2 {
		t.Fatalf("%d vectors, want 2 (16 characters at max_input_runes 16)", len(r.Data))
	}
	hits := e.hits.Load()
	a := rig.postEmbed(t, embedBody("embsmall", []string{"ok", strings.Repeat("ä", 17)}))
	code, param, _ := a.envelope(t)
	if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedValue || param != "input[1]" || !bytes.Contains(a.body, []byte("at most 16 characters")) {
		t.Fatalf("%d %s, want 400 unsupported_value input[1] \"at most 16 characters\"", a.status, a.body)
	}
	if got := e.hits.Load(); got != hits {
		t.Errorf("wire hits %d after the refusal, want %d (no backend for an input over the alias's limit)", got, hits)
	}
}

// TestEmbedFirstBlockOversizeNamesTheInput (G4): the backend refuses the
// first sub-block with llama.cpp's oversize body ⇒ 400
// context_length_exceeded with the alias's (here the row's) window in ctx's
// own text, no number of the upstream body, no Retry-After, x-should-retry
// false — param input[0] for a sub-block of one input, input for a sub-block
// of several. The single-input body is the fixture of G8.
func TestEmbedFirstBlockOversizeNamesTheInput(t *testing.T) {
	cases := []struct {
		name   string
		inputs int
		param  string
	}{
		{"one input", 1, "input[0]"},
		{"a block of three", 3, "input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			row := e.row("spark-embed", backends.ProtocolOpenAI, 10)
			row.NumCtx = 8192
			rig := newEmbedRig(t, []backends.Backend{row}, 4, 1) // E = 3: the inputs in one block
			e.setRespond(refuseCall(1, http.StatusBadRequest, llamaOversizeBody))
			a := rig.postEmbed(t, embedBody("emb", embedInputs(tc.inputs)))
			code, param, typ := a.envelope(t)
			if a.status != http.StatusBadRequest || code != oai.CodeContextLengthExceeded || param != tc.param || typ != oai.TypeInvalidRequest {
				t.Fatalf("%d %s, want 400 context_length_exceeded param %q", a.status, a.body, tc.param)
			}
			if ra := a.header.Get("Retry-After"); ra != "" || a.header.Get("x-should-retry") != "false" {
				t.Errorf("Retry-After %q x-should-retry %q, want none and false (a retry cannot fix the input)", ra, a.header.Get("x-should-retry"))
			}
			if !bytes.Contains(a.body, []byte("8192")) {
				t.Errorf("body %s, want the row's window 8192 in ctx's text", a.body)
			}
			chNoneIn(t, "the answer", string(a.body), "33177", "32768")
			if e.hits.Load() != 1 {
				t.Errorf("wire hits %d, want 1", e.hits.Load())
			}
			if rec := lastRecord(t, rig); rec.Reason != oai.CodeContextLengthExceeded {
				t.Errorf("access record reason %q, want %q", rec.Reason, oai.CodeContextLengthExceeded)
			}
			if tc.inputs == 1 {
				want, err := os.ReadFile(embedOversizeFixture)
				if err != nil {
					t.Fatalf("the fixture of G8: %v", err)
				}
				if !bytes.Equal(a.body, want) {
					t.Errorf("body\n%q\nthe fixture %s (read by internal/store's G8)\n%q", a.body, embedOversizeFixture, want)
				}
			}
		})
	}
}

// TestEmbedLaterBlockOversizeIsTheClients (G5): at E = 1 every input is its
// own sub-block; the backend refuses the third (llama.cpp's oversize body)
// after the first two were served ⇒ the answer the first sub-block would get:
// 400 context_length_exceeded at input[2], no Retry-After, x-should-retry
// false, nothing of the two served vectors in the body; one error row, the
// two served blocks' usage on the access record, the reservation settled
// (wire work happened) and the in-flight budget free again. A 503 or a
// transport error of the third sub-block stays the sticky stop: 503
// upstream_unavailable with Retry-After.
func TestEmbedLaterBlockOversizeIsTheClients(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
		e.setRespond(refuseCall(3, http.StatusBadRequest, llamaOversizeBody))
		a := rig.postEmbed(t, embedBody("emb", embedInputs(3)))
		code, param, _ := a.envelope(t)
		if a.status != http.StatusBadRequest || code != oai.CodeContextLengthExceeded || param != "input[2]" {
			t.Fatalf("%d %s (Retry-After %q), want 400 context_length_exceeded param input[2]", a.status, a.body, a.header.Get("Retry-After"))
		}
		if ra := a.header.Get("Retry-After"); ra != "" || a.header.Get("x-should-retry") != "false" {
			t.Errorf("Retry-After %q x-should-retry %q, want none and false", ra, a.header.Get("x-should-retry"))
		}
		if bytes.Contains(a.body, []byte(`"embedding"`)) || bytes.Contains(a.body, []byte(`"data"`)) {
			t.Errorf("body %s carries vectors of the served blocks", a.body)
		}
		chNoneIn(t, "the answer", string(a.body), "33177", "32768")
		if e.hits.Load() != 3 {
			t.Errorf("wire hits %d, want 3", e.hits.Load())
		}
		rows := rig.rows.all()
		if len(rows) != 1 || rows[0].Err == nil || rows[0].Err.Code != oai.CodeContextLengthExceeded || rows[0].Telemetry.Metadata["sub_batches_done"] != 2 {
			t.Fatalf("rows %+v, want one error row context_length_exceeded with two blocks done", rows)
		}
		rec := lastRecord(t, rig)
		if rec.Reason != oai.CodeContextLengthExceeded || rec.PromptTokens != 4 {
			t.Errorf("access record reason %q prompt tokens %d, want %q and 4 (the two served blocks)", rec.Reason, rec.PromptTokens, oai.CodeContextLengthExceeded)
		}
		if s := rig.limiter.settles.Load(); s != 1 {
			t.Errorf("limiter settles %d, want 1 (wire work happened)", s)
		}
		if n := rig.budget.InUse(); n != 0 {
			t.Errorf("the in-flight budget holds %d B after the request, want 0", n)
		}
	})
	retryable := []struct {
		name    string
		respond func(http.ResponseWriter, *http.Request, int64, []string) bool
	}{
		{"503", refuseCall(3, http.StatusServiceUnavailable, `{"error":{"message":"loading"}}`)},
		{"transport", func(w http.ResponseWriter, _ *http.Request, n int64, _ []string) bool {
			if n < 3 {
				return false
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				return false
			}
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
			return true
		}},
	}
	for _, tc := range retryable {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
			e.setRespond(tc.respond)
			a := rig.postEmbed(t, embedBody("emb", embedInputs(3)))
			code, param, _ := a.envelope(t)
			if a.status != http.StatusServiceUnavailable || code != oai.CodeUpstreamUnavailable || param != "" ||
				a.header.Get("Retry-After") == "" || a.header.Get("x-should-retry") != "true" {
				t.Fatalf("%d %s (Retry-After %q), want 503 upstream_unavailable with Retry-After, no param (the sticky stop)", a.status, a.body, a.header.Get("Retry-After"))
			}
		})
	}
}

// TestEmbedOllamaRefusesInsteadOfCutting (G7): an Ollama row is asked not to
// cut an input (truncate:false — Ollama's /api/embed cuts to its window and
// answers 200 by default) and its refusal is the client's 400
// context_length_exceeded at the input: the wording of Ollama's server check
// (400) and of its runner (413, also with the token counts of the MLX
// runner), and the older wording of the server check ("maximum context
// length", as Ollama v0.5.0 wrote it).
func TestEmbedOllamaRefusesInsteadOfCutting(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"server check", http.StatusBadRequest, `{"error":"the input length exceeds the context length"}`},
		{"runner", http.StatusRequestEntityTooLarge, `{"error":"the input length exceeds the context length"}`},
		{"mlx runner", http.StatusRequestEntityTooLarge, `{"error":"the input length (5000 tokens) exceeds the context length (4096)"}`},
		{"v0.5.0", http.StatusBadRequest, `{"error":"input length exceeds maximum context length"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			rig := newEmbedRig(t, []backends.Backend{e.row("ollama-embed", backends.ProtocolOllama, 10)}, 2, 1)
			e.setRespond(refuseCall(1, tc.status, tc.body))
			a := rig.postEmbed(t, embedBody("emb", embedInputs(1)))
			code, param, _ := a.envelope(t)
			if a.status != http.StatusBadRequest || code != oai.CodeContextLengthExceeded || param != "input[0]" {
				t.Fatalf("%d %s, want 400 context_length_exceeded param input[0]", a.status, a.body)
			}
			e.mu.Lock()
			body := e.bodies[0]
			e.mu.Unlock()
			if !strings.Contains(body, `"truncate":false`) {
				t.Errorf("the Ollama row got %s, want truncate:false (no silent cut)", body)
			}
		})
	}
}
