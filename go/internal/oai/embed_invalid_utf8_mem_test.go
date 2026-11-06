package oai_test

// The heap of the embeddings route with invalid UTF-8 bytes and escapes
// against the sum of its bookings (FOLGE-E (h); W02-09 Delta-Review 6 F6-1,
// the template of W02-09 hardening 5): invalid bytes in "input" and "model"
// decode to U+FFFD of three bytes, and before the route's ledger nothing
// booked the decoded strings, the upstream body built from them, the log
// row's copy of the input or io.ReadAll's growth — 255 inputs of 32 KiB
// 0xff to a credentials alias held +161.4 MiB against 24.3 MiB booked, ten
// at once +739.6 MiB against 242.9 MiB (budget 256 MiB). One form and one
// concurrency per process, under the container's GOMEMLIMIT:
//
//	CTX_FOLGEE_FORM=input_ff CTX_FOLGEE_N=10 GOMEMLIMIT=200MiB \
//	  go test -run '^TestEmbedInvalidBytesHeapGate$' -count=1 ./internal/oai/
//
// The forms: input_ff (inputs of 32 KiB 0xff, at most 256), model_ff (a
// model name of 0xff), input_ascii (the ASCII twin of input_ff: what
// io.ReadAll's growth and the decoded strings hold, G3) — each to the
// credentials alias "embsec", where no scan booking covers them — and the
// class gate's forms "ecg:<slot>:<value form>[@pub]" (TestEmbedClassGateOverTheRoute,
// embed_class_gate_test.go). CTX_FOLGEE_BYTES sets the body size (default:
// the cap), CTX_FOLGEE_BODYLOG the body logging (none when unset).
//
// The bound: peak HeapInuse − the heap before the requests ≤ the sum of the
// reservations the requests held plus n × heapGateBaseLoad — no allowance
// for invalid bytes (lead decision 6: they are booked). The engine is
// leanEmbedEngine: it reads the upstream body through a fixed buffer and
// keeps nothing of it, as an engine in another process would — the
// harness's engine keeps every body it saw.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// leanEmbedEngine is an OpenAI embeddings engine that reads the request
// through a pooled 4-KiB buffer (a request of 256 one-input blocks is 256
// engine calls), counts the texts of "input" (the strings two levels deep)
// without keeping any, and answers dims components per text.
func leanEmbedEngine(t testing.TB, dims int) *httptest.Server {
	pool := sync.Pool{New: func() any { b := make([]byte, 4<<10); return &b }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bp := pool.Get().(*[]byte)
		defer pool.Put(bp)
		buf := *bp
		depth, texts := 0, 0
		inString, escaped := false, false
		for {
			n, err := r.Body.Read(buf)
			for _, c := range buf[:n] {
				switch {
				case inString:
					switch {
					case escaped:
						escaped = false
					case c == '\\':
						escaped = true
					case c == '"':
						inString = false
					}
				case c == '"':
					inString = true
					if depth == 2 {
						texts++
					}
				case c == '[' || c == '{':
					depth++
				case c == ']' || c == '}':
					depth--
				}
			}
			if err != nil {
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[`)
		vec := "[" + strings.TrimSuffix(strings.Repeat("0.5,", dims), ",") + "]"
		for i := range texts {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			_, _ = fmt.Fprintf(w, `{"object":"embedding","index":%d,"embedding":%s}`, i, vec)
		}
		_, _ = fmt.Fprintf(w, `],"model":"m","usage":{"prompt_tokens":%d,"total_tokens":%d}}`, 2*texts, 2*texts)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// embedInvalidForm is a body of about lim bytes to model: "input_ff" =
// inputs of 32 KiB of 0xff (at most 256), "input_ascii" = the same of 'a',
// "model_ff" = a model name of 0xff.
func embedInvalidForm(t testing.TB, form, model string, lim int) []byte {
	var b bytes.Buffer
	switch form {
	case "input_ff", "input_ascii":
		unit := map[string]string{"input_ff": "\xff", "input_ascii": "a"}[form]
		in := `"` + strings.Repeat(unit, 32<<10) + `"`
		b.WriteString(`{"model":"` + model + `","input":[`)
		for i := 0; i < 256 && b.Len()+len(in)+3 <= lim; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(in)
		}
		b.WriteString(`]}`)
	case "model_ff":
		pre, post := `{"model":"`, `","input":"x"}`
		b.WriteString(pre + strings.Repeat("\xff", lim-len(pre)-len(post)) + post)
	default:
		t.Fatalf("unknown form %q", form)
	}
	return b.Bytes()
}

// embedGateBody is the body of the gate's form at lim bytes: a form of
// embedInvalidForm to the credentials alias, or a class-gate form
// "ecg:<combo>[@pub]" to its alias (oai.EmbedClassGateBody).
func embedGateBody(t testing.TB, form string, lim int) []byte {
	if cg, ok := strings.CutPrefix(form, "ecg:"); ok {
		combo, pub := strings.CutSuffix(cg, "@pub")
		model := "embsec"
		if pub {
			model = "embpub"
		}
		body, found := oai.EmbedClassGateBody(combo, model, lim)
		if !found {
			t.Fatalf("unknown class-gate form %q", cg)
		}
		return body
	}
	return embedInvalidForm(t, form, "embsec", lim)
}

func TestEmbedInvalidBytesHeapGate(t *testing.T) {
	form := os.Getenv("CTX_FOLGEE_FORM")
	if form == "" {
		t.Skip("CTX_FOLGEE_FORM not set (measurement test, see the file comment)")
	}
	n, _ := strconv.Atoi(os.Getenv("CTX_FOLGEE_N"))
	n = max(n, 1)
	lim := memCap - 16
	if v, err := strconv.Atoi(os.Getenv("CTX_FOLGEE_BYTES")); err == nil && v > 0 {
		lim = v
	}
	srv := leanEmbedEngine(t, 8)
	row := backends.Backend{
		ID: "id-lean", Name: "lean", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "EMBED-MODEL",
		Roles: []string{"embed"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"embed": 30},
	}
	var held heldSum
	rig := newEmbedRig(t, []backends.Backend{row}, 4*n, 1, withoutCensus)
	rig.reg.reload(t, embedScanAliases())
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = memBudget, memCap, memRespCap
		if v := os.Getenv("CTX_FOLGEE_BODYLOG"); v != "" {
			s.BodyLogging = v
		}
	})
	// The log row goes where production sends it (the capped entry), not
	// into the harness's row list, which keeps every row.
	rig.deps.Record = func(row llmlog.GatewayRow) { _ = llmlog.GatewayEntry(row) }
	rig.router = embedMemMount(rig, oai.Embeddings(rig.deps), &held)
	body := embedGateBody(t, form, lim)

	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapInuse
	var peak atomic.Uint64
	var budgetPeak atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var s runtime.MemStats
		for {
			runtime.ReadMemStats(&s)
			memMax(&peak, s.HeapInuse)
			if u := rig.budget.InUse(); u > budgetPeak.Load() {
				budgetPeak.Store(u)
			}
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	statuses := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-start
			rig.router.ServeHTTP(w, req)
			var env struct{ Error struct{ Code *string } }
			code := ""
			if w.Code != http.StatusOK && json.Unmarshal(w.Body.Bytes(), &env) == nil && env.Error.Code != nil {
				code = *env.Error.Code
			}
			statuses[i] = fmt.Sprintf("%d/%s", w.Code, code)
		}()
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	dur := time.Since(t0)
	close(stop)
	<-done
	cnt := map[string]int{}
	for _, s := range statuses {
		cnt[s]++
	}
	keys := make([]string, 0, len(cnt))
	for k := range cnt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	answers := make([]string, 0, len(keys))
	for _, k := range keys {
		answers = append(answers, fmt.Sprintf("%s×%d", k, cnt[k]))
	}
	sum, handled := held.sum()
	growth := int64(peak.Load()) - int64(base)
	bound := sum + int64(n)*heapGateBaseLoad
	verdict := "ok"
	if growth > bound {
		verdict = "OVER"
	}
	t.Logf("FOLGEE form=%s n=%d bytes=%d bodylog=%q answers=%s handled=%d dur=%v growth=%.1fMiB held_sum=%.1fMiB bound=%.1fMiB budget_peak=%.1fMiB gomemlimit=%d verdict=%s",
		form, n, len(body), os.Getenv("CTX_FOLGEE_BODYLOG"), strings.Join(answers, ","), handled, dur.Round(time.Millisecond),
		float64(growth)/(1<<20), float64(sum)/(1<<20), float64(bound)/(1<<20), float64(budgetPeak.Load())/(1<<20), debug.SetMemoryLimit(-1), verdict)
	if growth > bound {
		t.Errorf("HeapInuse grew %.1f MiB, more than the %d requests' reservations (%.1f MiB) plus %d × %d KiB harness load = %.1f MiB",
			float64(growth)/(1<<20), handled, float64(sum)/(1<<20), n, heapGateBaseLoad>>10, float64(bound)/(1<<20))
	}
}
