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
// pins of chat_parse_alloc_test.go. TestChatParseHeapGateSustained is the
// closed-loop case (CTX_FOLGEP_DUR seconds), run under GOMEMLIMIT=200MiB.

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
	"os/exec"
	"runtime"
	"runtime/debug"
	"slices"
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
	"github.com/GottZ/ctx/internal/llmlog"
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
	memCeilID   = "00000000-0000-4000-8000-0000000c4b03"
	memCeilPub  = "00000000-0000-4000-8000-0000000c4b04"
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
// handler, and counts the requests that reached it without a declared
// length — the ones readRequestBody reads in chunks (hardening 5, B-3).
type heldSum struct {
	mu         sync.Mutex
	vals       []int64
	undeclared atomic.Int64
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
				if req.ContentLength < 0 {
					held.undeclared.Add(1)
				}
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

func memBool(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

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
	// Hardening 6, the class gate: "cg:<table>.<key>:<value form>" is one
	// combination of chat_class_gate_test.go at lim bytes, for the alias
	// with the policy ceilings.
	if cg, ok := strings.CutPrefix(name, "cg:"); ok {
		cg, pub := strings.CutSuffix(cg, "@pub") // the public alias: the scan runs, the log row stores bodies
		body, found := oai.ClassGateBody(cg, lim)
		if !found || !json.Valid(body) {
			t.Fatalf("class-gate form %q: found %v, valid JSON %v", cg, found, found && json.Valid(body))
		}
		if pub {
			body = bytes.Replace(body, []byte(`"model":"`+oai.CGModel+`"`), []byte(`"model":"`+oai.CGModel+`pub"`), 1)
			return memShape{body: body, model: oai.CGModel + "pub"}
		}
		return memShape{body: body, model: oai.CGModel}
	}
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
	// Delta-Review 2 F-1: one key of the whole body with a scalar value — the
	// scan reads it twice in a row (as a key, then in the probe "key:
	// value"), about two bookings in total, one at a time.
	case "f14a_scan_hugekey_num", "f14b_scan_hugekey_str":
		val := "1"
		if strings.HasPrefix(name, "f14b") {
			val = `"v"`
		}
		pre := `{"model":"` + model + `","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[{"`
		suf := `":` + val + `}]}]}`
		s.body = []byte(pre + strings.Repeat("apikey:x ", (lim-len(pre)-len(suf))/9) + suf)
	// Hardening 5, B-1: a model name of the whole body — plain, invalid
	// UTF-8 (the decode made each byte a U+FFFD of 3 bytes), escaped.
	// Hardening 6: message text of invalid bytes between ASCII ones — with
	// body logging on, the log row's cleaning (each invalid byte one
	// U+FFFD) grew a copy of the whole array to 8 × its size.
	case "f16_text_ffa":
		s.body = text("\xffa")
	// Hardening 6: a developer message of non-ASCII text — the rewritten
	// array is the request's second copy, the upstream body its third, and
	// the meter's rune count (utf8.RuneCount) copied the array once more.
	case "f17_developer_utf8":
		pre, suf := `{"model":"`+model+`","messages":[{"role":"developer","content":"`, `"}]}`
		s.body = []byte(pre + strings.Repeat("é", (lim-len(pre)-len(suf))/2) + suf)
	case "f15a_model_long", "f15b_model_ff", "f15c_model_escaped":
		pre, suf := `{"model":"`, `","messages":[{"role":"user","content":"x"}]}`
		fill := map[string]string{"f15a": "a", "f15b": "\xff", "f15c": `\u00e9`}[name[:4]]
		s.body = []byte(pre + strings.Repeat(fill, (lim-len(pre)-len(suf))/len(fill)) + suf)
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

// newMemRig is the gate's route for n clients of shape: the shape's failover
// rows before the serving one, the production caps, the strict body cap and
// the reservation probe (memMount).
// memCGAliases are the two aliases of the class gate over the route, the
// policy ceilings at credentials (no sensitivity scan) and below it (public:
// the scan runs, the log row stores bodies) — TestClassGateOverTheRoute runs
// every combination with both.
var memCGAliases = []chatAlias{
	{memCeilID, backends.GlobalScope, oai.CGModel, "chat", "chat", backends.SensCredentials, oai.CGCeilPolicy, true},
	{memCeilPub, backends.GlobalScope, oai.CGModel + "pub", "chat", "chat", backends.SensPublic, oai.CGCeilPolicy, true},
}

func newMemRig(t *testing.T, shape memShape, n int, held *heldSum) (*chatRig, *memEngine) {
	var rows []backends.Backend
	for i := 0; i < shape.failover; i++ { // the chain walks higher priorities first
		rows = append(rows, newMemEngine(t, true).row(fmt.Sprintf("bad%d", i), 100-i))
	}
	up := newMemEngine(t, false)
	rows = append(rows, up.row("good", 10))
	rig := newChatRig(t, rows, 4*n, 1)
	rig.reg.reload(t, append(chatAliases(),
		chatAlias{memVisionID, backends.GlobalScope, "visbig", "chat", "chat", backends.SensCredentials, `{"vision":true,"vision_max_bytes":8388608}`, true},
		chatAlias{memTcapID, backends.GlobalScope, "tcap", "chat", "chat", backends.SensCredentials, `{"tools.max_bytes":1048576,"tools.max_count":256}`, true},
		memCGAliases[0], memCGAliases[1]))
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = memBudget, memCap, memRespCap
		// CTX_FOLGEP_BODYLOG (hardening 6): body logging on, so the route's
		// copy of the message array for the log row is measured too.
		if v := os.Getenv("CTX_FOLGEP_BODYLOG"); v != "" {
			s.BodyLogging = v
		}
	})
	// The log row goes where production sends it (llmlog.RecordGateway:
	// the capped entry, written asynchronously), not into the harness's
	// row list, which would keep every logged body for the test's life.
	rig.deps.Record = func(row llmlog.GatewayRow) { _ = llmlog.GatewayEntry(row) }
	rig.router = memMount(rig, oai.ChatCompletions(rig.deps), held)
	return rig, up
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
	var held heldSum
	rig, up := newMemRig(t, shape, n, &held)

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
	if want := int64(handled) * memBool(shape.chunked); held.undeclared.Load() != want {
		t.Errorf("form %s: %d of %d requests reached the handler without a declared length, want %d (chunked %v)", form, held.undeclared.Load(), handled, want, shape.chunked)
	}
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
	fmt.Printf("FOLGEP form=%s n=%d gomemlimit=%q bodylog=%q body=%d answers=[%s] growth_MiB=%.1f held_sum_MiB=%.1f handled=%d bound_MiB=%.1f "+
		"bound_noanswer_MiB=%.1f bound_peak_MiB=%.1f base_MiB=%.1f budget_peak_MiB=%.1f numgc=%d dur=%v up_hits=%d up_max_body=%d up_total=%d "+
		"answer_max_B=%d verdict=%s\n",
		form, n, os.Getenv("GOMEMLIMIT"), os.Getenv("CTX_FOLGEP_BODYLOG"), len(body), strings.TrimSpace(sb.String()), mib(growth), mib(sum), handled, mib(bound),
		mib(boundNoAnswer), mib(boundPeak), mib(int64(base)), mib(budgetPeak.Load()), ms.NumGC-gc0, dur.Round(time.Millisecond),
		up.hits.Load(), up.max.Load(), up.total.Load(), answerMax.Load(), verdict)
	if verdict != "ok" {
		t.Errorf("form %s, n=%d: heap growth %.1f MiB > reserved %.1f MiB + %d × base load %.0f KiB = %.1f MiB (without the answer share %.1f, the budget's peak %.1f): %s",
			form, n, mib(growth), mib(sum), n, float64(heapGateBaseLoad)/1024, mib(bound), mib(boundNoAnswer), mib(boundPeak), verdict)
	}
}

// cgRouteRun is one run of the class gate over the route: the heap gate's
// form, concurrency and body size.
type cgRouteRun struct {
	form  string
	n     int
	bytes int
}

// cgRouteSizes and cgRouteNs are the body sizes and concurrencies of the
// class gate over the route (hardening 6 P-3 (b)).
var (
	cgRouteSizes = []int{20100, 65600, 262144}
	cgRouteNs    = []int{1, 10}
)

// cgRouteRuns is the matrix of the class gate over the route: every
// combination with both aliases of memCGAliases — "cg:<combo>" at
// credentials, "cg:<combo>@pub" below it, where the sensitivity scan runs
// (hardening 7: the scan side was a special run, and its 211 red runs went
// unseen by the gate) — at every size and concurrency.
func cgRouteRuns(combos []string) []cgRouteRun {
	var runs []cgRouteRun
	for _, c := range combos {
		for _, alias := range []string{"", "@pub"} {
			for _, size := range cgRouteSizes {
				for _, n := range cgRouteNs {
					runs = append(runs, cgRouteRun{"cg:" + c + alias, n, size})
				}
			}
		}
	}
	return runs
}

// TestClassGateRouteRunsBothAliases pins the matrix of the class gate over
// the route (hardening 7): every combination runs with the alias at
// credentials and with the public one, at every size and concurrency, and
// the two forms reach those two aliases.
func TestClassGateRouteRunsBothAliases(t *testing.T) {
	combos := oai.ClassGateCombos()
	runs := cgRouteRuns(combos)
	per := len(cgRouteSizes) * len(cgRouteNs)
	sens := map[string]backends.Sensitivity{}
	for _, a := range memCGAliases {
		sens[a.name] = a.sens
	}
	forms := map[string]int{}
	for _, r := range runs {
		forms[r.form]++
	}
	count := map[backends.Sensitivity]int{}
	for f, k := range forms {
		if k != per {
			t.Errorf("form %s runs %d times, want %d (every size and concurrency)", f, k, per)
		}
		count[sens[memForm(t, f, cgRouteSizes[0]).model]]++
	}
	if len(runs) != 2*len(combos)*per || count[backends.SensCredentials] != len(combos) || count[backends.SensPublic] != len(combos) {
		t.Errorf("the route gate runs %d times (want %d): %d combinations at credentials, %d below (want %d each)",
			len(runs), 2*len(combos)*per, count[backends.SensCredentials], count[backends.SensPublic], len(combos))
	}
}

// TestClassGateOverTheRoute is the class gate over the real route (hardening
// 6 P-3 (b), hardening 7): every run of cgRouteRuns is TestChatParseHeapGate
// in a process of its own — this test binary run again with the form in its
// environment (GOMEMLIMIT, 200MiB when unset, and CTX_FOLGEP_BODYLOG pass
// through) — and every verdict must be ok. A measurement test outside the
// suite (load-sensitive, about 8 640 processes):
//
//	CTX_CLASSGATE_ROUTE=1 [CTX_FOLGEP_BODYLOG=full] [CTX_CLASSGATE_ROUTE_OUT=<file>] \
//	  go test -run '^TestClassGateOverTheRoute$' -timeout 2h ./internal/oai/
//
// CTX_CLASSGATE_ROUTE=<file> runs the combinations named in the file (one
// per line) instead of all; CTX_CLASSGATE_ROUTE_OUT gets the gate's line of
// every run.
func TestClassGateOverTheRoute(t *testing.T) {
	sel := os.Getenv("CTX_CLASSGATE_ROUTE")
	if sel == "" {
		t.Skip("CTX_CLASSGATE_ROUTE not set (measurement test, see the comment)")
	}
	combos := oai.ClassGateCombos()
	if sel != "1" {
		raw, err := os.ReadFile(sel)
		if err != nil {
			t.Fatal(err)
		}
		combos = strings.Fields(string(raw))
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CTX_CLASSGATE_ROUTE") && !strings.HasPrefix(kv, "CTX_FOLGEP_FORM=") &&
			!strings.HasPrefix(kv, "CTX_FOLGEP_N=") && !strings.HasPrefix(kv, "CTX_FOLGEP_BYTES=") {
			env = append(env, kv)
		}
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		env = append(env, "GOMEMLIMIT=200MiB")
	}
	var lines strings.Builder
	runs := cgRouteRuns(combos)
	bad := map[string]int{}
	for _, r := range runs {
		cmd := exec.Command(exe, "-test.run", "^TestChatParseHeapGate$", "-test.count=1")
		cmd.Env = append(slices.Clone(env), "CTX_FOLGEP_FORM="+r.form, "CTX_FOLGEP_N="+strconv.Itoa(r.n), "CTX_FOLGEP_BYTES="+strconv.Itoa(r.bytes))
		out, _ := cmd.CombinedOutput()
		line := ""
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "FOLGEP form=") {
				line = l
			}
		}
		alias := "credentials"
		if strings.HasSuffix(r.form, "@pub") {
			alias = "public"
		}
		if line == "" {
			line = fmt.Sprintf("NOLINE form=%s n=%d bytes=%d %.300s", r.form, r.n, r.bytes, out)
		}
		fmt.Fprintf(&lines, "size=%d %s\n", r.bytes, line)
		if !strings.HasSuffix(line, "verdict=ok") {
			bad[alias]++
			t.Errorf("%s n=%d at %d B: %s", r.form, r.n, r.bytes, line)
		}
	}
	if p := os.Getenv("CTX_CLASSGATE_ROUTE_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(lines.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("FOLGEP class gate route: combos=%d runs=%d (each alias %d) not ok: credentials %d, public %d, bodylog %q",
		len(combos), len(runs), len(runs)/2, bad["credentials"], bad["public"], os.Getenv("CTX_FOLGEP_BODYLOG"))
}

// TestChatParseHeapGateSustained is the gate's sustained case (FOLGE-P
// hardening 3, Delta-Review 3 R3-1): n hostile clients send the form in a
// closed loop for CTX_FOLGEP_DUR seconds — again at once after an answer,
// after a 503 too, without waiting for its Retry-After — so the budget a
// finished request frees is booked again while that request's garbage is
// still on the heap. The bound is the budget's peak plus n × the harness
// load, and the budget itself. It holds under a set process memory limit
// (GOMEMLIMIT=200MiB, as the image sets it); without one the pacer lets the
// garbage of finished requests grow to GOGC × the live heap and the heap
// passed the bound in 7 of 13 of the reviewer's runs of the form scanned
// twice (up to 294.0 MiB against 248.8 MiB). One case per process:
//
//	GOMEMLIMIT=200MiB CTX_FOLGEP_FORM=f14a_scan_hugekey_num_pub CTX_FOLGEP_N=40 \
//	  CTX_FOLGEP_BYTES=1048576 CTX_FOLGEP_DUR=20 \
//	  go test -run '^TestChatParseHeapGateSustained$' ./internal/oai/
func TestChatParseHeapGateSustained(t *testing.T) {
	form := os.Getenv("CTX_FOLGEP_FORM")
	dur, _ := strconv.Atoi(os.Getenv("CTX_FOLGEP_DUR"))
	if form == "" || dur <= 0 {
		t.Skip("CTX_FOLGEP_FORM or CTX_FOLGEP_DUR not set (measurement test, see the file comment)")
	}
	n, _ := strconv.Atoi(os.Getenv("CTX_FOLGEP_N"))
	n = max(n, 1)
	lim := memCap - 16
	if v, err := strconv.Atoi(os.Getenv("CTX_FOLGEP_BYTES")); err == nil && v > 0 {
		lim = v
	}
	shape := memForm(t, form, lim)
	var held heldSum
	rig, _ := newMemRig(t, shape, n, &held)

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
			memMaxInt(&budgetPeak, rig.budget.InUse())
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	var mu sync.Mutex
	cnt := map[string]int{}
	deadline := time.Now().Add(time.Duration(dur) * time.Second)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for time.Now().Before(deadline) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(shape.body))
				if shape.chunked { // as TestChatParseHeapGate: no declared length (hardening 5, B-3)
					req.ContentLength = -1
					req.Body = io.NopCloser(struct{ io.Reader }{bytes.NewReader(shape.body)})
				}
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
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
				mu.Lock()
				cnt[fmt.Sprintf("%d/%s", w.Code, code)]++
				mu.Unlock()
				if w.Code == http.StatusServiceUnavailable {
					time.Sleep(2 * time.Millisecond) // hostile: no Retry-After
				}
			}
		}()
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	el := time.Since(t0)
	close(stop)
	<-done
	runtime.ReadMemStats(&ms)

	keys := make([]string, 0, len(cnt))
	for k := range cnt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s×%d ", k, cnt[k])
	}
	mib := func(v int64) float64 { return float64(v) / (1 << 20) }
	growth := int64(peak.Load()) - int64(base)
	boundPeak := budgetPeak.Load() + int64(n)*heapGateBaseLoad
	verdict := "ok"
	switch {
	case growth > memBudget:
		verdict = "OVER_BUDGET"
	case growth > boundPeak:
		verdict = "OVER_PEAK"
	}
	_, handled := held.sum()
	fmt.Printf("FOLGEP sustained form=%s n=%d dur=%ds gomemlimit=%q body=%d answers=[%s] growth_MiB=%.1f budget_peak_MiB=%.1f bound_peak_MiB=%.1f "+
		"base_MiB=%.1f numgc=%d elapsed=%v handled=%d undeclared=%d verdict=%s\n",
		form, n, dur, os.Getenv("GOMEMLIMIT"), len(shape.body), strings.TrimSpace(sb.String()), mib(growth), mib(budgetPeak.Load()), mib(boundPeak),
		mib(int64(base)), ms.NumGC-gc0, el.Round(time.Millisecond), handled, held.undeclared.Load(), verdict)
	if want := int64(handled) * memBool(shape.chunked); held.undeclared.Load() != want || handled == 0 {
		t.Errorf("form %s: %d of %d requests reached the handler without a declared length, want %d (chunked %v)", form, held.undeclared.Load(), handled, want, shape.chunked)
	}
	if verdict != "ok" {
		t.Errorf("form %s, n=%d, %d s closed loop, GOMEMLIMIT %q: heap growth %.1f MiB > the budget's peak %.1f MiB + %d × base load %.0f KiB = %.1f MiB (budget %.0f MiB): %s",
			form, n, dur, os.Getenv("GOMEMLIMIT"), mib(growth), mib(budgetPeak.Load()), n, float64(heapGateBaseLoad)/1024, mib(boundPeak), mib(memBudget), verdict)
	}
}
