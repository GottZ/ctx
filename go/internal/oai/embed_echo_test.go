package oai_test

// The echo gate of the embeddings parse (W02-09 hardening 2, C-6 —
// Fremd-Review 3 F3-1; activated in rebase round 2, part B): a refusal that
// names a client's key names it cut to echoNameMax bytes and "…" (FOLGE-P
// H-9, decodeKey) — the answer stays below embedEchoAnswerMax, never a
// function of the body — and the refusal costs no more live heap than the
// request's own reserve (3 × the body) plus parseMemSlack, for one request
// and for ten at once, on both refusal shapes of the embeddings parse: a
// body of at most five members (the key read in full) and the window of
// more (the first six members). Red before the port onto FOLGE-P's key
// helpers (Fremd-Review 3, logs/mem/summary.txt): one 8-MiB key of "<" ⇒
// 400 unsupported_parameter with a 100 662 187-B answer, HeapInuse peak
// 285 MiB for one request (bound 47), 1 982 MiB for ten (bound 263); the
// escaped form 16,8 MB / 84 MiB / 289 MiB. Each case in its own process
// under GOMEMLIMIT=200MiB:
//
//	go test -run 'TestEmbedRefusalEchoIsBounded/<case>$' -count=1 ./internal/oai/
//
// Uses the parse-memory helpers of embed_parsemem_test.go (parseMemBodyCap,
// parseMemBudget, parseMemGoMemLimit, parseMemSlack, heapPeak) and the
// embeddings rig (newEmbedRig, embedEngine). TestEmbedRefusalNamesTheKeyCut
// pins the cut itself (the param and the message, byte for byte).

import (
	"encoding/json"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// embedEchoAnswerMax bounds the 400 that names a client's key: the envelope
// (≈ 300 B) and a name cut to a constant length; never a function of the
// body.
const embedEchoAnswerMax = 4 << 10

func TestEmbedRefusalEchoIsBounded(t *testing.T) {
	skipUnderRace(t, "the answer and the live heap of refusals that name an 8-MiB key (HeapInuse against 3 × the body + parseMemSlack; the cut itself is TestEmbedRefusalNamesTheKeyCut's in every build)")
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(parseMemGoMemLimit))
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = parseMemBudget; s.MaxRequestBytes = parseMemBodyCap })
	room := parseMemBodyCap - 256
	lt := strings.Repeat("<", room)
	esc := strings.Repeat(string(rune(92))+"u003c", room/6) // the escaped form: backslash-u-003c
	for _, tc := range []struct {
		name, body string
		n          int
	}{
		{"a key of at most five members, <", `{"model":"emb","input":"x","` + lt + `":0}`, 1},
		{"a key of at most five members, <, ten at once", `{"model":"emb","input":"x","` + lt + `":0}`, 10},
		{"a key of at most five members, escaped", `{"model":"emb","input":"x","` + esc + `":0}`, 1},
		{"a key of at most five members, escaped, ten at once", `{"model":"emb","input":"x","` + esc + `":0}`, 10},
		{"the window of six members, <", `{"model":"emb","input":"x","a":0,"b":0,"c":0,"` + lt[:room-32] + `":0}`, 1},
		{"the window of six members, <, ten at once", `{"model":"emb","input":"x","a":0,"b":0,"c":0,"` + lt[:room-32] + `":0}`, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.hits.Store(0)
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			base := m.HeapInuse
			var peak atomic.Uint64
			stop, sampled := make(chan struct{}), make(chan struct{})
			go heapPeak(stop, &peak, sampled)
			answers := make([]chatAnswer, tc.n)
			var wg sync.WaitGroup
			start := time.Now()
			for i := range tc.n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					answers[i] = rig.postEmbed(t, tc.body)
				}()
			}
			wg.Wait()
			took := time.Since(start)
			close(stop)
			<-sampled
			over := int64(peak.Load()) - int64(base)
			bound := int64(tc.n)*int64(3*len(tc.body)) + parseMemSlack
			t.Logf("%s: %d request(s) of %d B ⇒ %d with a %d-B answer after %v; HeapInuse peak %d MiB over a base of %d MiB (bound %d MiB)",
				tc.name, tc.n, len(tc.body), answers[0].status, len(answers[0].body), took.Round(time.Millisecond), over>>20, base>>20, bound>>20)
			for i, a := range answers {
				code, _, _ := a.envelope(t)
				if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedParameter {
					t.Fatalf("request %d: %d %s, want 400 unsupported_parameter", i, a.status, code)
				}
				if len(a.body) > embedEchoAnswerMax {
					t.Errorf("request %d: a %d-B answer to a %d-B body, want ≤ %d B (the client's key is named at a bounded length)", i, len(a.body), len(tc.body), embedEchoAnswerMax)
				}
			}
			if e.hits.Load() != 0 {
				t.Errorf("wire hits %d, want 0", e.hits.Load())
			}
			if over > bound {
				t.Errorf("HeapInuse peak %d MiB over the base, want ≤ %d × the request reserve + %d MiB = %d MiB", over>>20, tc.n, parseMemSlack>>20, bound>>20)
			}
		})
	}
}

// TestEmbedRefusalNamesTheKeyCut pins the name a refusal of the embeddings
// parse carries: a client's key of at most oai.EchoNameMax decoded bytes as
// it is, a longer one cut there at a character boundary and marked "…" — in
// param and in the message, for an unknown key and for a repeated one, on
// both refusal shapes (at most five members, the window of six).
func TestEmbedRefusalNamesTheKeyCut(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	k64 := strings.Repeat("k", oai.EchoNameMax)
	k100 := strings.Repeat("k", 100)
	cut := k64 + "…"
	esc := strings.Repeat(string(rune(92))+"u006b", 100) // k as an escape
	for _, tc := range []struct {
		name, body, code, param string
	}{
		{"an unknown key of exactly the bound", `{"model":"emb","input":"x","` + k64 + `":0}`, oai.CodeUnsupportedParameter, k64},
		{"an unknown key past the bound", `{"model":"emb","input":"x","` + k100 + `":0}`, oai.CodeUnsupportedParameter, cut},
		{"an unknown escaped key past the bound", `{"model":"emb","input":"x","` + esc + `":0}`, oai.CodeUnsupportedParameter, cut},
		{"a two-byte character across the bound", `{"model":"emb","input":"x","` + k64[1:] + `é":0}`, oai.CodeUnsupportedParameter, k64[1:] + "…"},
		{"a repeated key past the bound", `{"model":"emb","input":"x","` + k100 + `":0,"` + k100 + `":1}`, oai.CodeInvalidJSON, cut},
		{"the window of six members", `{"model":"emb","input":"x","x":0,"y":0,"z":0,"` + k100 + `":0}`, oai.CodeUnsupportedParameter, cut},
		{"a repeat in the window of six members", `{"model":"emb","` + k100 + `":0,"input":"x","` + k100 + `":0,"y":0,"z":0}`, oai.CodeInvalidJSON, cut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rig.postEmbed(t, tc.body)
			code, param, _ := a.envelope(t)
			if a.status != http.StatusBadRequest || code != tc.code || param != tc.param {
				t.Fatalf("%d %s param %q, want 400 %s param %q", a.status, code, param, tc.code, tc.param)
			}
			var env struct{ Error struct{ Message string } }
			if err := json.Unmarshal(a.body, &env); err != nil {
				t.Fatal(err)
			}
			want := "Unrecognized request argument supplied: " + tc.param + "."
			if tc.code == oai.CodeInvalidJSON {
				want = "The request repeats the key " + tc.param + "."
			}
			if env.Error.Message != want {
				t.Errorf("message %q, want %q", env.Error.Message, want)
			}
		})
	}
}
