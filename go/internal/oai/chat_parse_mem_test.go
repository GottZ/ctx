package oai_test

// The heap gate of the chat parse (FOLGE-P, gate 1): the request parse of
// POST /v1/chat/completions holds on the heap only what its reservation in
// the in-flight budget covers. Measured over the real route — the /v1 stack
// of the harness plus the strict body cap (handler.MaxBodySizeStrictWith) in
// front of the budget, as cmd/ctxd/gateway.go mounts it — with the
// production defaults (8 MiB body cap, 256 MiB budget, 4 MiB response cap)
// and a live HeapInuse sampler, one form and one concurrency per process:
//
//	CTX_FOLGEP_FORM=f7b_text_8m_lt CTX_FOLGEP_N=10 [CTX_FOLGEP_BYTES=1048576] \
//	  [GOMEMLIMIT=200MiB] go test -run '^TestChatParseHeapGate$' ./internal/oai/
//
// The bound: peak HeapInuse − the heap before the requests ≤ the sum of the
// reservations the requests held (read from each request's reservation at
// the end of its handler — the route's own numbers, not copied ones) plus n ×
// heapGateBaseLoad, the measured per-request load of the harness that no
// parse causes (the recorder, the fake engine's server and client side, the
// answer). Load-sensitive, so it is a measurement test outside the default
// suite (skipped without CTX_FOLGEP_FORM); the deterministic halves — bytes
// allocated per parse, the reservation covering the second copy — are the
// pins of chat_parse_alloc_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

// heapGateBaseLoad is the measured harness load per request (FOLGE-P report
// §Messung: the tiny form's heap growth per request, rounded up): what one
// request of the harness allocates besides its parse and its upstream body.
const heapGateBaseLoad = 256 << 10

// The production defaults the gate runs with (gateway.max_request_bytes,
// gateway.inflight_budget, gateway.max_response_bytes).
const (
	memCap      = 8 << 20
	memBudget   = 256 << 20
	memRespCap  = 4 << 20
	memVisionID = "00000000-0000-4000-8000-0000000c4b01"
	memTcapID   = "00000000-0000-4000-8000-0000000c4b02"
)

// memEngine is a fake engine that counts and discards what it gets; fail
// makes it answer 404 (a row that does not know the model: the walk moves on).
type memEngine struct {
	srv   *httptest.Server
	hits  atomic.Int64
	max   atomic.Int64
	total atomic.Int64
}

func newMemEngine(t testing.TB, fail bool) *memEngine {
	u := &memEngine{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		u.total.Add(n)
		for {
			p := u.max.Load()
			if n <= p || u.max.CompareAndSwap(p, n) {
				break
			}
		}
		if fail {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answerSGLang)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *memEngine) row(name string, prio int) backends.Backend {
	return backends.Backend{
		ID: "id-" + name, Name: name, Host: u.srv.URL, Protocol: backends.ProtocolOpenAI, Model: "MODEL-CANARY",
		Roles: []string{"chat"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: prio, Timeouts: map[string]int{"chat": 30},
	}
}

// heldSum collects the reservation every request held at the end of its
// handler.
type heldSum struct {
	mu   sync.Mutex
	vals []int64
}

func (h *heldSum) add(v int64) {
	h.mu.Lock()
	h.vals = append(h.vals, v)
	h.mu.Unlock()
}

func (h *heldSum) sum() (s int64, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, v := range h.vals {
		s += v
	}
	return s, len(h.vals)
}

// memMount is chatRig.mount plus the strict body cap before the budget and
// the reservation probe right in front of the handler.
func memMount(r *chatRig, h http.Handler, held *heldSum) chi.Router {
	rt := chi.NewRouter()
	rt.Route("/v1", func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rec := &oai.AccessRecord{}
				ctx := oai.WithAccessRecord(fedhop.WithIdentity(req.Context(), rigIdentity), rec)
				if ca := r.ca.Load(); ca != nil {
					ctx = context.WithValue(ctx, consumerKey{}, ca)
				}
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		g.Use(oai.Recover)
		g.Use(federation.PeerIngress(federation.PeerIngressConfig{Consumer: r.deps.Consumer, Logger: slog.New(slog.DiscardHandler)}))
		g.Use(handler.MaxBodySizeStrictWith(func() int64 { return r.settings.Load().MaxRequestBytes }, oai.WriteRequestTooLarge))
		g.Use(oai.InflightBudget(&r.budget, func() oai.Settings { return *r.settings.Load() }))
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req)
				if res := oai.ReservationFrom(req.Context()); res != nil {
					held.add(oai.ReservationHeld(res))
				}
			})
		})
		g.Method(http.MethodPost, "/chat/completions", h)
	})
	return rt
}

// memGen writes prefix, then elem(0), elem(1), … comma-separated while the
// whole stays ≤ limit with suffix, then suffix.
func memGen(prefix, suffix string, limit int, elem func(i int) string) []byte {
	var b bytes.Buffer
	b.Grow(limit)
	b.WriteString(prefix)
	for i := 0; ; i++ {
		e := elem(i)
		sep := 0
		if i > 0 {
			sep = 1
		}
		if b.Len()+sep+len(e)+len(suffix) > limit {
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(e)
	}
	b.WriteString(suffix)
	return bytes.Clone(b.Bytes())
}

func memConst(s string) func(int) string { return func(int) string { return s } }

func memNest(d int) string { return strings.Repeat("[", d) + strings.Repeat("]", d) }

// memShape is one form of the gate: its body, how it is sent and the rows it
// needs.
type memShape struct {
	body     []byte
	model    string
	chunked  bool // sent without Content-Length
	failover int  // rows that answer 404 before the serving one
}

// memForm builds form name at lim bytes: the thirteen forms of the probe
// (2026-11-05, .gotmp/chatparse-probe) and the ones FOLGE-P found.
func memForm(t testing.TB, name string, lim int) memShape {
	model := "chat"
	if strings.HasSuffix(name, "_pub") {
		model = "pub"
	}
	msg := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"x"}]`, model)
	s := memShape{model: model}
	text := func(fill string) []byte {
		pre, suf := `{"model":"`+model+`","messages":[{"role":"user","content":"`, `"}]}`
		n := (lim - len(pre) - len(suf)) / len(fill)
		return []byte(pre + strings.Repeat(fill, n) + suf)
	}
	switch strings.TrimSuffix(name, "_pub") {
	case "f0_tiny":
		s.body = []byte(msg + `}`)
	case "f1_messages":
		s.body = memGen(`{"model":"`+model+`","messages":[`, `]}`, lim, memConst(`{"role":"user","content":"x"}`))
	case "f1b_messages_developer":
		s.body = memGen(`{"model":"`+model+`","messages":[`, `]}`, lim, memConst(`{"role":"developer","content":"x"}`))
	case "f1c_messages_reasoning":
		s.body = memGen(`{"model":"`+model+`","messages":[`, `]}`, lim, memConst(`{"role":"assistant","content":"x","reasoning_content":"r"}`))
	case "f2_parts":
		s.body = memGen(`{"model":"`+model+`","messages":[{"role":"user","content":[`, `]}]}`, lim, memConst(`{"type":"text","text":"x"}`))
	case "f3a_tools_8m":
		s.body = memGen(msg+`,"tools":[`, `]}`, lim, memConst(`{"type":"function","function":{"name":"f"}}`))
	case "f3b_tools_256k_overcount":
		tools := memGen(`[`, `]`, 256<<10, memConst(`{"type":"function","function":{"name":"f"}}`))
		s.body = []byte(msg + `,"tools":` + string(tools) + `}`)
	case "f3c_tools_64":
		tools := memGen(`[`, `]`, 64*44+1, memConst(`{"type":"function","function":{"name":"f"}}`))
		s.body = []byte(msg + `,"tools":` + string(tools) + `}`)
	case "f6a_depth_rf_ok", "f6a_depth_rf_over":
		d := 9996
		if strings.HasSuffix(name, "over") {
			d = 10001
		}
		s.body = []byte(msg + `,"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":` + memNest(d) + `}}}`)
		return s
	case "f6c_depth_tools_params":
		s.body = []byte(msg + `,"tools":[{"type":"function","function":{"name":"f","parameters":` + memNest(9990) + `}}]}`)
	case "f3d_tools_1m_overcount_tcap":
		s.model = "tcap"
		tools := memGen(`[`, `]`, min(lim, 1<<20)-128, memConst(`{"type":"function","function":{"name":"f"}}`))
		s.body = []byte(`{"model":"tcap","messages":[{"role":"user","content":"x"}],"tools":` + string(tools) + `}`)
	case "f4a_stop":
		s.body = memGen(msg+`,"stop":[`, `]}`, lim, memConst(`"x"`))
	case "f4b_logit_bias":
		s.body = memGen(msg+`,"logit_bias":{`, `}}`, lim, func(i int) string { return `"` + strconv.Itoa(i) + `":0` })
	case "f5_unknown_keys":
		s.body = memGen(msg+`,`, `}`, lim, func(i int) string { return fmt.Sprintf(`"k%07d":0`, i) })
	case "f5b_unknown_keys_escaped":
		s.body = memGen(msg+`,`, `}`, lim, func(i int) string { return fmt.Sprintf(`"k%07d":0`, i) })
	case "f6b_depth_toolcalls_8m":
		s.body = memGen(`{"model":"`+model+`","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[`,
			`]}]}`, lim, memConst(memNest(9990)))
	case "f6d_objdepth_toolcalls_8m":
		d := 9990
		s.body = memGen(`{"model":"`+model+`","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[`,
			`]}]}`, lim, memConst(strings.Repeat(`{"a":`, d)+"0"+strings.Repeat("}", d)))
	case "f7a_text_8m":
		s.body = text("a")
	case "f7b_text_8m_lt":
		s.body = text("<")
	case "f7c_text_8m_chunked":
		s.body, s.chunked = text("a"), true
	case "f7d_text_8m_failover":
		s.body, s.failover = text("a"), 2
	case "f8_tool_choice_keys":
		s.body = memGen(msg+`,"tool_choice":{"type":"function","function":{"name":"f"},`, `}}`, lim,
			func(i int) string { return fmt.Sprintf(`"k%07d":0`, i) })
	case "f9_modalities":
		s.body = memGen(msg+`,"modalities":[`, `]}`, lim, memConst(`"text"`))
	case "f10_images_escaped":
		s.model = "visbig"
		s.body = memGen(`{"model":"visbig","messages":[{"role":"user","content":[`, `]}]}`, lim,
			memConst(`{"type":"image_url","image_url":{"url":"data:image\/png;base64,AAAA"}}`))
	case "f11_n_huge":
		s.body = []byte(msg + `,"n":1` + strings.Repeat("0", lim-len(msg)-8) + `}`)
	// H-9: one key of the whole body, named in the refusal (an answer of
	// twelve times the body before the name was cut).
	case "f12a_echo_key_lt":
		s.body = []byte(msg + `,"` + strings.Repeat("<", lim-len(msg)-8) + `":0}`)
	case "f12b_echo_key_escaped":
		s.body = []byte(msg + `,"` + strings.Repeat(fmt.Sprintf("%cu003c", '\\'), (lim-len(msg)-8)/6) + `":0}`)
	case "f12c_echo_key_repeated":
		k := strings.Repeat("<", (lim-len(msg)-16)/2)
		s.body = []byte(msg + `,"` + k + `":0,"` + k + `":1}`)
	case "f12d_echo_message":
		pre := `{"model":"` + model + `","messages":[{"role":"user","content":"x","`
		s.body = []byte(pre + strings.Repeat("<", lim-len(pre)-8) + `":0}]}`)
	case "f12e_echo_part":
		pre := `{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"text","text":"x","`
		s.body = []byte(pre + strings.Repeat("<", lim-len(pre)-10) + `":0}]}]}`)
	case "f12f_echo_hull":
		k := strings.Repeat("<", (lim-len(msg)-48)/2)
		s.body = []byte(msg + `,"tool_choice":{"type":"function","` + k + `":0,"` + k + `":1}}`)
	case "f12g_echo_logit_bias":
		k := strings.Repeat("<", (lim-len(msg)-32)/2)
		s.body = []byte(msg + `,"logit_bias":{"` + k + `":0,"` + k + `":1}}`)
	// H-8: the scan's most expensive text per byte (short secret-like
	// assignments), for the public alias.
	case "f13_scan_assign":
		s.body = text("apikey:x ")
	default:
		t.Fatalf("unknown form %q", name)
	}
	if !json.Valid(s.body) {
		t.Fatalf("form %s: invalid JSON", name)
	}
	return s
}

func memMaxInt(a *atomic.Int64, v int64) {
	for {
		p := a.Load()
		if v <= p || a.CompareAndSwap(p, v) {
			return
		}
	}
}

type memRes struct {
	status int
	code   string
}

func memMax(a *atomic.Uint64, v uint64) {
	for {
		p := a.Load()
		if v <= p || a.CompareAndSwap(p, v) {
			return
		}
	}
}

func TestChatParseHeapGate(t *testing.T) {
	form := os.Getenv("CTX_FOLGEP_FORM")
	if form == "" {
		t.Skip("CTX_FOLGEP_FORM not set (measurement test, see the file comment)")
	}
	n, _ := strconv.Atoi(os.Getenv("CTX_FOLGEP_N"))
	n = max(n, 1)
	lim := memCap - 16
	if v, err := strconv.Atoi(os.Getenv("CTX_FOLGEP_BYTES")); err == nil && v > 0 {
		lim = v
	}
	shape := memForm(t, form, lim)
	var rows []backends.Backend
	for i := 0; i < shape.failover; i++ { // the chain walks higher priorities first
		rows = append(rows, newMemEngine(t, true).row(fmt.Sprintf("bad%d", i), 100-i))
	}
	up := newMemEngine(t, false)
	rows = append(rows, up.row("good", 10))
	rig := newChatRig(t, rows, 4*n, 1)
	rig.reg.reload(t, append(chatAliases(),
		chatAlias{memVisionID, backends.GlobalScope, "visbig", "chat", "chat", backends.SensCredentials, `{"vision":true,"vision_max_bytes":8388608}`, true},
		chatAlias{memTcapID, backends.GlobalScope, "tcap", "chat", "chat", backends.SensCredentials, `{"tools.max_bytes":1048576,"tools.max_count":256}`, true}))
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = memBudget, memCap, memRespCap
	})
	var held heldSum
	rig.router = memMount(rig, oai.ChatCompletions(rig.deps), &held)

	body := shape.body
	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, gc0 := ms.HeapInuse, ms.NumGC

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
	out := make([]memRes, n)
	var answerMax atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			if shape.chunked {
				req.ContentLength = -1
				req.Body = io.NopCloser(struct{ io.Reader }{bytes.NewReader(body)})
			}
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-start
			rig.router.ServeHTTP(w, req)
			var e struct {
				Error struct {
					Code *string `json:"code"`
				} `json:"error"`
			}
			code := ""
			if w.Code != http.StatusOK && json.Unmarshal(w.Body.Bytes(), &e) == nil && e.Error.Code != nil {
				code = *e.Error.Code
			}
			out[i] = memRes{w.Code, code}
			memMaxInt(&answerMax, int64(w.Body.Len()))
		}(i)
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	dur := time.Since(t0)
	close(stop)
	<-done
	runtime.ReadMemStats(&ms)

	cnt := map[string]int{}
	for _, r := range out {
		cnt[fmt.Sprintf("%d/%s", r.status, r.code)]++
	}
	keys := make([]string, 0, len(cnt))
	for k := range cnt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s×%d ", k, cnt[k])
	}
	sum, handled := held.sum()
	growth := int64(peak.Load()) - int64(base)
	bound := sum + int64(n)*heapGateBaseLoad
	// Two stricter bounds (hardening H-8): without the answer share of the
	// served requests — the parse and the scan are covered by the request
	// share alone, not by bytes meant for the answer — and, for requests that
	// all run at once (n ≤ 10), the budget's own peak. The harness load is
	// counted per wire attempt here (a failover row is one more fake engine
	// call: f7d's two 404 rows add 0.1 MiB).
	served := int64(cnt["200/"])
	attempts := int64(n) * int64(1+shape.failover)
	boundNoAnswer := sum - served*2*memRespCap + attempts*heapGateBaseLoad
	boundPeak := budgetPeak.Load() + attempts*heapGateBaseLoad
	mib := func(v int64) float64 { return float64(v) / (1 << 20) }
	verdict := "ok"
	switch {
	case growth > bound:
		verdict = "OVER"
	case n == 1 && growth > boundNoAnswer:
		verdict = "OVER_NOANSWER"
	case n > 1 && n <= 10 && growth > boundPeak:
		verdict = "OVER_PEAK"
	}
	fmt.Printf("FOLGEP form=%s n=%d gomemlimit=%q body=%d answers=[%s] growth_MiB=%.1f held_sum_MiB=%.1f handled=%d bound_MiB=%.1f "+
		"bound_noanswer_MiB=%.1f bound_peak_MiB=%.1f base_MiB=%.1f budget_peak_MiB=%.1f numgc=%d dur=%v up_hits=%d up_max_body=%d up_total=%d "+
		"answer_max_B=%d verdict=%s\n",
		form, n, os.Getenv("GOMEMLIMIT"), len(body), strings.TrimSpace(sb.String()), mib(growth), mib(sum), handled, mib(bound),
		mib(boundNoAnswer), mib(boundPeak), mib(int64(base)), mib(budgetPeak.Load()), ms.NumGC-gc0, dur.Round(time.Millisecond),
		up.hits.Load(), up.max.Load(), up.total.Load(), answerMax.Load(), verdict)
	if verdict != "ok" {
		t.Errorf("form %s, n=%d: heap growth %.1f MiB > reserved %.1f MiB + %d × base load %.0f KiB = %.1f MiB (without the answer share %.1f, the budget's peak %.1f): %s",
			form, n, mib(growth), mib(sum), n, float64(heapGateBaseLoad)/1024, mib(bound), mib(boundNoAnswer), mib(boundPeak), verdict)
	}
}
