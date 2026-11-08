package oai_test

// The budget dimensioning harness (RC-D cluster C8 "hardening", before the
// gateway.enabled flip): what one request at the default caps books in the
// in-flight budget per route and class, how long the sensitivity scan holds
// its booking, how the heap behaves under GOMEMLIMIT while the budget is
// full, and the upstream frame the floor (V39) does not count. Numbers, not
// gates: every point writes one JSON line per measurement to
// $CTX_BUDGET_DIM_OUT (appended) and to the test log. Outside the suite —
// eight-MiB bodies over the public scan, child processes, load-sensitive:
//
//	CTX_BUDGET_DIM=1 CTX_BUDGET_DIM_OUT=<file> [CTX_BUDGET_DIM_REP=<n>] \
//	  [CTX_BUDGET_DIM_SECS=10] go test -run '^TestBudgetDimension$' -timeout 10m ./internal/oai/
//
// The arithmetic of the floor itself (V39 at the configuration's defaults)
// is internal/config's TestBudgetDimensionCalc: internal/oai may not import
// internal/config (config-layering).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
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

// dimProbeBudget is the budget the floor points measure under: large enough
// that no booking of one request at the cap is refused, so the reservation
// at the handler's end is the request's whole need (the ledger books, never
// releases, until the request ends).
const dimProbeBudget = 2 << 30

// dimRespShare is the largest response share of the floor at the default
// caps: 2 × gateway.max_response_bytes (8 MiB) beats 2 × the frame cap +
// the relay buffer (4 MiB) and 4 B × embedding_max_floats + 64 KiB.
const dimRespShare = 2 * memRespCap

func budgetDimOn(t *testing.T) {
	t.Helper()
	if os.Getenv("CTX_BUDGET_DIM") != "1" {
		t.Skip("CTX_BUDGET_DIM=1 not set (measurement harness, see the file comment)")
	}
	if testing.Short() {
		t.Skip("measurement harness: outside -short")
	}
	skipUnderRace(t, "the heap and the time of eight-MiB requests")
}

var dimOutMu sync.Mutex

// dimLoad is /proc/loadavg's first three fields.
func dimLoad() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return ""
	}
	f := strings.Fields(string(raw))
	if len(f) < 3 {
		return ""
	}
	return strings.Join(f[:3], " ")
}

// dimEmit writes one measurement as a JSON line.
func dimEmit(t testing.TB, point string, fields map[string]any) {
	t.Helper()
	fields["point"] = point
	fields["utc"] = time.Now().UTC().Format(time.RFC3339)
	fields["load"] = dimLoad()
	if r := os.Getenv("CTX_BUDGET_DIM_REP"); r != "" {
		fields["rep"] = r
	}
	line, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("emit %s: %v", point, err)
	}
	t.Logf("BUDGETDIM %s", line)
	p := os.Getenv("CTX_BUDGET_DIM_OUT")
	if p == "" {
		return
	}
	dimOutMu.Lock()
	defer dimOutMu.Unlock()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("emit %s: %v", point, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("emit %s: %v", point, err)
	}
}

func dimMiB(v int64) float64 { return float64(v) / (1 << 20) }

// dimChatFloor is V39's floor at the default caps as the oai terms spell it
// (chatFloor: 26 × the cap + the read term + the fixed rest + 2 × the
// response cap).
func dimChatFloor() int64 { return chatFloor(memCap, memRespCap) }

// dimChatText is a chat request of size bytes to model: one user message of
// fill repeated.
func dimChatText(model string, size int, fill string) []byte {
	pre, suf := `{"model":"`+model+`","messages":[{"role":"user","content":"`, `"}]}`
	n := max((size-len(pre)-len(suf))/len(fill), 1)
	return []byte(pre + strings.Repeat(fill, n) + suf)
}

func TestBudgetDimension(t *testing.T) {
	budgetDimOn(t)
	t.Run("embed_floor", dimEmbedFloor)
	t.Run("rerank_floor", dimRerankFloor)
	t.Run("scan_hold", dimScanHold)
	t.Run("scan_hold_slow", dimScanHoldSlow)
	t.Run("scan_cap_floor", dimScanCapFloor)
	t.Run("small_share", dimSmallShare)
	t.Run("upstream_frame", dimUpstreamFrame)
	t.Run("gomemlimit", dimGomemlimit)
}

// dimEmbedFloor: one embeddings request at the 8-MiB cap per alias class
// (native_dims 8 or unknown, credentials or public) and input form (FOLGE-E's
// gate bodies: 'a', the escape of U+0001, 0xff, the escape of a lone
// surrogate; 32-KiB inputs, as many as fit), under a budget no booking
// passes — the reservation at the end is the request's need — and once more
// at the default budget for the status a lone request gets there.
func dimEmbedFloor(t *testing.T) {
	pub := embedScanAliases()
	for i := range pub {
		if pub[i].name == "emb" {
			pub[i].sens = backends.SensPublic
		}
	}
	floor := dimChatFloor()
	for _, cl := range []struct {
		name, model, sens string
		dims              int
		aliases           []chatAlias
	}{
		{"cred", "embsec", "credentials", 8, embedScanAliases()},
		{"pub", "embpub", "public", 8, pub},
		{"cred0", "embsec0", "credentials", 0, rdumpNoDims(false)},
		{"pub0", "embpub0", "public", 0, rdumpNoDims(true)},
	} {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withoutCensus)
		rig.reg.reload(t, cl.aliases)
		var c rdumpCapture
		rig.router = rdumpMount(rig, oai.Embeddings(rig.deps), &c)
		for _, form := range []string{"input_ascii", "input_esc", "input_ff", "input_surr"} {
			body := rdumpGateBody(form, cl.model, memCap-16)
			post := func(budget int64) (int, int64, time.Duration) {
				rig.setSettings(func(s *oai.Settings) {
					s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = budget, memCap, memRespCap
					s.BodyLogging = llmlog.BodyLoggingNone
				})
				req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				t0 := time.Now()
				rig.router.ServeHTTP(w, req)
				return w.Code, c.held, time.Since(t0)
			}
			st, need, dur := post(dimProbeBudget)
			stDef, heldDef, durDef, measured := dimDerived(need), int64(-1), time.Duration(0), false
			if dimVerifyAtDefault(cl.sens, need) {
				stDef, heldDef, durDef = post(memBudget)
				measured = true
			}
			dimEmit(t, "embed_floor", map[string]any{
				"route": "embeddings", "class": cl.name, "sensitivity": cl.sens, "native_dims": cl.dims, "form": form,
				"body_bytes": len(body), "status_probe_budget": st, "need_bytes": need, "need_mib": dimMiB(need),
				"status_default_budget": stDef, "status_default_measured": measured, "held_at_default_bytes": heldDef, "default_budget_bytes": memBudget,
				"over_default_mib": dimMiB(need - memBudget), "v39_floor_bytes": floor, "over_v39_floor_mib": dimMiB(need - floor),
				"dur_ms": dur.Milliseconds(), "dur_default_ms": durDef.Milliseconds(),
			})
		}
	}
}

// dimRerankBody is a rerank request of model with docs documents of fill
// filling the 8-MiB cap, written raw: '<' stays one byte in the client's
// body (json.Marshal would write six) and becomes six on the wire.
func dimRerankBody(model string, docs int, fill string, limit int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"` + model + `","query":"which document is relevant","documents":[`)
	size := (limit - 4096) / docs
	for i := range docs {
		if i > 0 {
			b.WriteByte(',')
		}
		p := "document " + strconv.Itoa(i) + " "
		b.WriteString(`"` + p + strings.Repeat(fill, max(size-len(p)-4, 0)) + `"`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// dimRerankFloor: one rerank request at the 8-MiB cap per alias class (rrwide
// credentials, rrpub public — both 1000 documents, rrwide in blocks of 128,
// rrpub in one), document count and byte form ('x', and '<', which the wire
// writes as six bytes), measured as dimEmbedFloor.
func dimRerankFloor(t *testing.T) {
	srv := leanRerankEngine(t)
	row := backends.Backend{
		ID: "id-lean", Name: "lean", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "RERANK-MODEL",
		Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"rerank": 60},
	}
	floor := dimChatFloor()
	for _, alias := range []struct{ name, sens string }{{"rrwide", "credentials"}, {"rrpub", "public"}} {
		for _, docs := range []int{1, 1000} {
			for _, fill := range []string{"x", "<"} {
				rig := newRerankRig(t, []backends.Backend{row}, 16, 1, withoutCensus)
				var held heldSum
				rig.router = rerankMemMount(rig, oai.Rerank(rig.deps, oai.RerankJina), &held)
				body := dimRerankBody(alias.name, docs, fill, memCap)
				post := func(budget int64) (int, int64, string, time.Duration) {
					held = heldSum{}
					rig.setSettings(func(s *oai.Settings) {
						s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = budget, memCap, memRespCap
						s.BodyLogging = llmlog.BodyLoggingNone
					})
					req := httptest.NewRequest(http.MethodPost, "/v1/rerank", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					t0 := time.Now()
					rig.router.ServeHTTP(w, req)
					sum, _ := held.sum()
					code := ""
					if w.Code != http.StatusOK {
						code = fmt.Sprintf("%.160s", w.Body.String())
					}
					return w.Code, sum, code, time.Since(t0)
				}
				st, need, why, dur := post(dimProbeBudget)
				stDef, heldDef, whyDef, durDef, measured := dimDerived(need), int64(-1), "", time.Duration(0), false
				if dimVerifyAtDefault(alias.sens, need) {
					stDef, heldDef, whyDef, durDef = post(memBudget)
					measured = true
				}
				dimEmit(t, "rerank_floor", map[string]any{
					"route": "rerank", "alias": alias.name, "sensitivity": alias.sens, "docs": docs, "fill": fill,
					"body_bytes": len(body), "status_probe_budget": st, "refusal_probe_budget": why, "need_bytes": need, "need_mib": dimMiB(need),
					"status_default_budget": stDef, "status_default_measured": measured, "refusal_default_budget": whyDef, "held_at_default_bytes": heldDef,
					"over_default_mib": dimMiB(need - memBudget), "v39_floor_bytes": floor, "over_v39_floor_mib": dimMiB(need - floor),
					"dur_ms": dur.Milliseconds(), "dur_default_ms": durDef.Milliseconds(),
				})
			}
		}
	}
}

// dimServe posts body to the chat route of rig and reports the status, the
// time the handler (and so the reservation) took and the refusal's text.
// dimDerived is the status a lone request of need bytes gets at the default
// budget: the ledger only books, so it is served iff need fits (the trial
// run of 2026-11-07 held this in 12 of 12 measured cases).
func dimDerived(need int64) int {
	if need <= memBudget {
		return http.StatusOK
	}
	return http.StatusServiceUnavailable
}

// dimVerifyAtDefault says whether a floor point is measured again at the
// default budget: always at credentials (no scan, fast), below it only
// within 32 MiB of the budget (each run is a scan of eight MiB, seconds).
func dimVerifyAtDefault(sens string, need int64) bool {
	return sens == "credentials" || (need > memBudget-32<<20 && need < memBudget+32<<20)
}

func dimServe(rig *chatRig, body []byte) (int, time.Duration, string) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := &discardWriter{h: http.Header{}}
	var errBody bytes.Buffer
	t0 := time.Now()
	rig.router.ServeHTTP(&dimErrWriter{discardWriter: w, keep: &errBody}, req)
	d := time.Since(t0)
	st := w.status
	if st == 0 {
		st = http.StatusOK
	}
	why := ""
	if st != http.StatusOK {
		why = fmt.Sprintf("%.160s", errBody.String())
	}
	return st, d, why
}

// dimErrWriter keeps the first 512 bytes of the answer (the refusal's
// envelope) and discards the rest.
type dimErrWriter struct {
	*discardWriter
	keep *bytes.Buffer
}

func (d *dimErrWriter) Write(p []byte) (int, error) {
	if room := 512 - d.keep.Len(); room > 0 {
		d.keep.Write(p[:min(room, len(p))])
	}
	return d.discardWriter.Write(p)
}

// dimScanHold: a chat request at the 8-MiB cap to the public alias (the scan
// runs, booked before it) — what it books, how long it holds the booking
// over an engine that answers at once (the scan is the hold), and, while it
// holds, what other requests (another tenant's: the budget is the process's)
// get: small and larger ones at credentials and below. Under the image's
// GOMEMLIMIT (200 MiB), as the route runs in production.
func dimScanHold(t *testing.T) {
	old := debug.SetMemoryLimit(200 << 20)
	defer debug.SetMemoryLimit(old)
	cred, pub := oai.CGModel, oai.CGModel+"pub"
	for _, form := range []struct{ name, fill string }{{"text_a", "a"}, {"scan_assign", "apikey:x "}} {
		var held heldSum
		rig, _ := newMemRig(t, memShape{model: pub}, 16, &held)
		holder := dimChatText(pub, memCap-16, form.fill)

		// Alone: the booking and its hold.
		var peak atomic.Int64
		stop := make(chan struct{})
		var sampled sync.WaitGroup
		sampled.Add(1)
		go func() {
			defer sampled.Done()
			for {
				memMaxInt(&peak, rig.budget.InUse())
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
			}
		}()
		st, hold, why := dimServe(rig, holder)
		close(stop)
		sampled.Wait()
		sum, _ := held.sum()
		perMin := 60 / hold.Seconds()
		free := int64(memBudget) - sum
		dimEmit(t, "scan_hold", map[string]any{
			"phase": "alone", "form": form.name, "body_bytes": len(holder), "status": st, "refusal": why,
			"held_bytes": sum, "held_mib": dimMiB(sum), "budget_peak_bytes": peak.Load(), "hold_ms": hold.Milliseconds(),
			"requests_per_min_to_keep_held": perMin, "free_while_held_bytes": free, "free_while_held_mib": dimMiB(free),
			"largest_cred_body_while_held": max((free-dimRespShare-oai.JSONStateReserve-oai.ChunkedReadReserve(0))/3, 0),
			"largest_pub_body_while_held":  max((free-dimRespShare-oai.JSONStateReserve-oai.ScanReserveBase-oai.ScanDepthReserve)/26, 0),
			"scan_booking_bytes":           int64(oai.ScanReserveFactor)*int64(len(holder)) + oai.ScanReserveBase + oai.ScanDepthReserve,
			"gomemlimit":                   "200MiB (debug.SetMemoryLimit)",
		})

		// Held: probes of another tenant while the booking stands — one
		// holder for as many probes as its hold lasts (cheap ones first); a
		// probe served after the holder let go is run again under a new one
		// (at most three times), a 503 saw the booking by construction.
		probes := []struct {
			name, model string
			size        int
		}{
			{"cred_tiny", cred, 64}, {"cred_8MiB", cred, memCap - 16}, {"cred_4MiB", cred, 4 << 20}, {"cred_1MiB", cred, 1 << 20},
			{"pub_tiny", pub, 64}, {"pub_256KiB", pub, 256 << 10}, {"pub_1MiB", pub, 1 << 20},
		}
		tries := make([]int, len(probes))
		for i := 0; i < len(probes); {
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _, _ = dimServe(rig, holder)
			}()
			deadline := time.Now().Add(20 * time.Second)
			for rig.budget.InUse() < sum*9/10 && time.Now().Before(deadline) {
				time.Sleep(200 * time.Microsecond)
			}
			for i < len(probes) && rig.budget.InUse() >= sum*9/10 {
				p := probes[i]
				body := dimChatText(p.model, p.size, "a")
				inUse := rig.budget.InUse()
				pst, pd, pwhy := dimServe(rig, body)
				stillHeld := rig.budget.InUse() >= sum*9/10
				tries[i]++
				if !stillHeld && pst == http.StatusOK && tries[i] < 3 {
					break
				}
				dimEmit(t, "scan_hold", map[string]any{
					"phase": "probe", "form": form.name, "probe": p.name, "probe_bytes": len(body), "probe_status": pst, "probe_refusal": pwhy,
					"probe_ms": pd.Milliseconds(), "budget_in_use_at_probe": inUse, "holder_still_held_after_probe": stillHeld, "try": tries[i],
				})
				i++
			}
			<-done
		}
	}
}

// dimScanHoldSlow: the scan's booking over an engine that takes its time
// (dimSlowHold, a generation's length): the request's ledger only grows, so
// the booking taken before the scan stands until the request ends — the
// scan, the queue and the whole upstream call. Measured: how long the
// budget holds 90 % of the request's booking, and what another tenant's
// requests at the boundary of the rest get meanwhile (public 1 and 2 MiB,
// credentials tiny and 8 MiB). Under GOMEMLIMIT 200 MiB, as dimScanHold.
func dimScanHoldSlow(t *testing.T) {
	const dimSlowHold = 5 * time.Second
	old := debug.SetMemoryLimit(200 << 20)
	defer debug.SetMemoryLimit(old)
	srv := newDimEngine(t, dimSlowHold)
	row := backends.Backend{
		ID: "id-dim", Name: "dim", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "MODEL-CANARY",
		Roles: []string{"chat"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"chat": 30},
	}
	var held heldSum
	rig := newChatRig(t, []backends.Backend{row}, 16, 1, withoutCensus)
	rig.reg.reload(t, append(chatAliases(), memCGAliases[0], memCGAliases[1]))
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = memBudget, memCap, memRespCap
	})
	rig.deps.Record = func(row llmlog.GatewayRow) { _ = llmlog.GatewayEntry(row) }
	rig.router = memMount(rig, oai.ChatCompletions(rig.deps), &held)
	cred, pub := oai.CGModel, oai.CGModel+"pub"
	holder := dimChatText(pub, memCap-16, "a")
	booking := int64(oai.ScanReserveFactor)*int64(len(holder)) + oai.ScanReserveBase
	var bookedAt, releasedAt atomic.Int64
	t0 := time.Now()
	stop := make(chan struct{})
	var sampled sync.WaitGroup
	sampled.Add(1)
	go func() {
		defer sampled.Done()
		for {
			u := rig.budget.InUse()
			if u >= booking && bookedAt.Load() == 0 {
				bookedAt.Store(int64(time.Since(t0)))
			}
			if u < booking && bookedAt.Load() != 0 && releasedAt.Load() == 0 {
				releasedAt.Store(int64(time.Since(t0)))
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	done := make(chan struct{})
	var st int
	var total time.Duration
	go func() {
		defer close(done)
		st, total, _ = dimServe(rig, holder)
	}()
	for bookedAt.Load() == 0 && time.Since(t0) < 30*time.Second {
		time.Sleep(time.Millisecond)
	}
	type probeRes struct {
		Name   string `json:"name"`
		Bytes  int    `json:"bytes"`
		Status int    `json:"status"`
		Ms     int64  `json:"ms"`
		Held   bool   `json:"holder_held_after"`
	}
	var probes []probeRes
	for _, p := range []struct {
		name, model string
		size        int
	}{
		{"cred_tiny", cred, 64}, {"pub_tiny", pub, 64}, {"pub_2MiB", pub, 2 << 20}, {"pub_1MiB", pub, 1 << 20}, {"cred_8MiB", cred, memCap - 16},
	} {
		pst, pd, _ := dimServe(rig, dimChatText(p.model, p.size, "a"))
		probes = append(probes, probeRes{p.name, p.size, pst, pd.Milliseconds(), rig.budget.InUse() >= booking})
	}
	<-done
	// The reservation is released when the handler returns, right before
	// dimServe does: a release the 1-ms sampler did not see is that instant.
	releasedAt.CompareAndSwap(0, int64(time.Since(t0)))
	close(stop)
	sampled.Wait()
	sum, _ := held.sum()
	dimEmit(t, "scan_hold_slow", map[string]any{
		"form": "text_a", "body_bytes": len(holder), "status": st, "upstream_hold_ms": dimSlowHold.Milliseconds(),
		"total_ms": total.Milliseconds(), "scan_booking_bytes": booking, "booked_at_ms": time.Duration(bookedAt.Load()).Milliseconds(),
		"released_at_ms":  time.Duration(releasedAt.Load()).Milliseconds(),
		"booking_held_ms": time.Duration(releasedAt.Load() - bookedAt.Load()).Milliseconds(), "held_sum_bytes": sum, "probes": probes,
	})
}

// dimScanCapFloor: what a request below credentials needs per route at a
// body of 1 and 2 MiB — the floor's public half if such requests had a body
// cap of their own (a scan cap) below the 8-MiB request cap: chat (one text
// message), embeddings (FOLGE-E's four input forms, native_dims 8 and
// unknown) and rerank (one document, 'x' and '<'), each under a budget no
// booking passes.
func dimScanCapFloor(t *testing.T) {
	pub := embedScanAliases()
	for i := range pub {
		if pub[i].name == "emb" {
			pub[i].sens = backends.SensPublic
		}
	}
	srv := leanRerankEngine(t)
	rrow := backends.Backend{
		ID: "id-lean", Name: "lean", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "RERANK-MODEL",
		Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"rerank": 60},
	}
	for _, size := range []int{1 << 20, 2 << 20} {
		emit := func(route, class, form string, bodyLen, st int, need int64) {
			dimEmit(t, "scan_cap_floor", map[string]any{
				"scan_cap": size, "route": route, "class": class, "form": form, "body_bytes": bodyLen,
				"status_probe_budget": st, "need_bytes": need, "need_mib": dimMiB(need),
			})
		}
		{
			var held heldSum
			rig, _ := newMemRig(t, memShape{}, 4, &held)
			rig.setSettings(func(s *oai.Settings) { s.InflightBudget = dimProbeBudget })
			body := dimChatText(oai.CGModel+"pub", size-16, "a")
			st, _, _ := dimServe(rig, body)
			sum, _ := held.sum()
			emit("chat", "pub", "text_a", len(body), st, sum)
		}
		for _, cl := range []struct {
			name, model string
			aliases     []chatAlias
		}{{"pub", "embpub", pub}, {"pub0", "embpub0", rdumpNoDims(true)}} {
			e := newEmbedEngine(t, 8)
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withoutCensus)
			rig.reg.reload(t, cl.aliases)
			rig.setSettings(func(s *oai.Settings) {
				s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = dimProbeBudget, memCap, memRespCap
				s.BodyLogging = llmlog.BodyLoggingNone
			})
			var c rdumpCapture
			rig.router = rdumpMount(rig, oai.Embeddings(rig.deps), &c)
			for _, form := range []string{"input_ascii", "input_esc", "input_ff", "input_surr"} {
				body := rdumpGateBody(form, cl.model, size-16)
				req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				rig.router.ServeHTTP(w, req)
				emit("embeddings", cl.name, form, len(body), w.Code, c.held)
			}
		}
		for _, fill := range []string{"x", "<"} {
			rig := newRerankRig(t, []backends.Backend{rrow}, 16, 1, withoutCensus)
			rig.setSettings(func(s *oai.Settings) {
				s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = dimProbeBudget, memCap, memRespCap
				s.BodyLogging = llmlog.BodyLoggingNone
			})
			var held heldSum
			rig.router = rerankMemMount(rig, oai.Rerank(rig.deps, oai.RerankJina), &held)
			body := dimRerankBody("rrpub", 1, fill, size)
			req := httptest.NewRequest(http.MethodPost, "/v1/rerank", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			rig.router.ServeHTTP(w, req)
			sum, _ := held.sum()
			emit("rerank", "rrpub", "doc_"+fill, len(body), w.Code, sum)
		}
	}
}

// dimSmallShare: what a small chat request books — non-stream and stream —
// and so how many of them the default budget holds at once (the answer's
// share is booked at its cap whatever the body: 2 × max_response_bytes, or
// 2 × max_stream_frame_bytes plus the relay buffer for a stream; the
// booking stands while the request waits in the queue and while it runs).
func dimSmallShare(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var held heldSum
		var rig *chatRig
		var body []byte
		if stream {
			rig, _ = newMemStreamRig(t, 1, &held)
			body = streamForm(t, "f0_tiny", 1024)
		} else {
			rig, _ = newMemRig(t, memShape{}, 1, &held)
			body = memForm(t, "f0_tiny", 1024).body
		}
		st, d, why := dimServe(rig, body)
		sum, _ := held.sum()
		dimEmit(t, "small_share", map[string]any{
			"stream": stream, "body_bytes": len(body), "status": st, "refusal": why, "ms": d.Milliseconds(),
			"held_bytes": sum, "held_mib": dimMiB(sum), "concurrent_in_default_budget": int64(memBudget) / max(sum, 1),
		})
	}
}

// dimUpstreamFrame: the upstream frame the floor does not count (V39's
// comment; pinned by TestDeepestDeveloperRequestLacksOnlyTheUpstreamFrame
// with a 12- and a 212-byte backend model name) as a function of the name's
// length — the deepest rewritten request at the chunk sum 32 256 B, without
// and with a declared length, at a budget equal to the floor and at the
// floor plus the frame.
func dimUpstreamFrame(t *testing.T) {
	const resp = 4 << 20
	for _, pad := range []int{0, 200, 1000, 4000, 16000} {
		u := newUpstream(t, answerSGLang)
		row := u.row("spark", backends.TrustFull, 10)
		row.Model += strings.Repeat("M", pad)
		rig := newChatRig(t, []backends.Backend{row}, 4, 1)
		body := deepestBody(chunkSum6, "pub", "developer")
		floor := chatFloor(chunkSum6, resp)
		post := func(budget int64, undeclared bool) int {
			rig.setSettings(func(s *oai.Settings) {
				s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = budget, chunkSum6, resp
			})
			return postAs(t, rig, body, undeclared).status
		}
		if st := post(10*floor, true); st != http.StatusOK {
			t.Fatalf("pad %d: budget 10 × floor: %d", pad, st)
		}
		u.mu.Lock()
		upstream := len(u.raws[len(u.raws)-1])
		u.mu.Unlock()
		rewritten := len(body) - len(`{"model":"pub","messages":`) - len(`}`) - (len("developer") - len("system"))
		frame := int64(upstream - rewritten)
		dimEmit(t, "upstream_frame", map[string]any{
			"cap_bytes": chunkSum6, "body_bytes": len(body), "model_name_bytes": len(row.Model), "upstream_bytes": upstream,
			"frame_bytes": frame, "floor_bytes": floor,
			"status_floor_undeclared": post(floor, true), "status_floor_plus_frame_undeclared": post(floor+frame, true),
			"status_floor_declared": post(floor, false),
		})
	}
}

// dimGomemlimit runs dimGomemlimitChild in a process of its own per
// workload and GOMEMLIMIT (200MiB as the image sets it, 320MiB, 512MiB,
// off): the budget held full for CTX_BUDGET_DIM_SECS seconds (default 10) by
// hostile holders in a closed loop over a slow engine, a small probe every
// 100 ms.
func dimGomemlimit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GOMEMLIMIT=") && !strings.HasPrefix(kv, "CTX_BUDGET_DIM") {
			env = append(env, kv)
		}
	}
	secs := os.Getenv("CTX_BUDGET_DIM_SECS")
	if secs == "" {
		secs = "10"
	}
	type gcase struct{ workload, limit string }
	var cases []gcase
	for _, workload := range []string{"cred", "pub"} {
		for _, limit := range []string{"200MiB", "320MiB", "512MiB", "off"} {
			cases = append(cases, gcase{workload, limit})
		}
	}
	cases = append(cases, gcase{"cred_over", "200MiB"})
	for _, c := range cases {
		workload, limit := c.workload, c.limit
		{
			cmd := exec.Command(exe, "-test.run", "^TestBudgetDimensionChild$", "-test.count=1", "-test.timeout=3m")
			cmd.Env = append(slices.Clone(env), "GOMEMLIMIT="+limit, "CTX_BUDGET_DIM_CHILD="+workload, "CTX_BUDGET_DIM_SECS="+secs)
			out, err := cmd.CombinedOutput()
			var fields map[string]any
			for l := range strings.SplitSeq(string(out), "\n") {
				if s, ok := strings.CutPrefix(l, "BUDGETDIMCHILD "); ok {
					if jerr := json.Unmarshal([]byte(s), &fields); jerr != nil {
						t.Errorf("%s/%s: %v", workload, limit, jerr)
					}
				}
			}
			if fields == nil {
				t.Errorf("%s/%s: no result line (err %v): %.600s", workload, limit, err, out)
				continue
			}
			dimEmit(t, "gomemlimit", fields)
		}
	}
}

// dimEngine is a chat engine that holds every request of more than 64 KiB
// for hold before it answers (the hostile holders' slow upstream) and
// answers small ones at once (the probe).
func newDimEngine(t testing.TB, hold time.Duration) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		if n > 64<<10 {
			select {
			case <-time.After(hold):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answerSGLang)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dimProcStatus reads VmHWM and VmRSS (kB) of this process.
func dimProcStatus() (hwm, rss int64) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return -1, -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "VmHWM":
			hwm = n
		case "VmRSS":
			rss = n
		}
	}
	return hwm, rss
}

func dimPct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return -1
	}
	s := slices.Clone(v)
	sort.Float64s(s)
	i := int(p*float64(len(s))+0.5) - 1
	return s[min(max(i, 0), len(s)-1)]
}

// TestBudgetDimensionChild is one GOMEMLIMIT case of dimGomemlimit
// (CTX_BUDGET_DIM_CHILD=cred|pub): the chat route at the default caps and
// budget, holders that keep the budget full — "cred": 8-MiB requests to an
// alias at credentials (the booking is the request's live heap: body, its
// copy, the upstream body), "pub": 8-MiB requests below credentials (the
// sensitivity scan's booking, mostly garbage) — each again at once after an
// answer and 10 ms after a 503, over an engine that holds them 1 s; a tiny
// request every 100 ms as the probe. It prints one result line.
func TestBudgetDimensionChild(t *testing.T) {
	workload := os.Getenv("CTX_BUDGET_DIM_CHILD")
	if workload == "" {
		t.Skip("child of TestBudgetDimension/gomemlimit")
	}
	secs, _ := strconv.Atoi(os.Getenv("CTX_BUDGET_DIM_SECS"))
	secs = max(secs, 1)
	const hold = time.Second
	srv := newDimEngine(t, hold)
	row := backends.Backend{
		ID: "id-dim", Name: "dim", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "MODEL-CANARY",
		Roles: []string{"chat"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"chat": 30},
	}
	var held heldSum
	rig := newChatRig(t, []backends.Backend{row}, 64, 1, withoutCensus)
	rig.reg.reload(t, append(chatAliases(), memCGAliases[0], memCGAliases[1]))
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes, s.MaxStreamFrameBytes = memBudget, memCap, memRespCap, memStreamFrameCap
	})
	rig.deps.Record = func(row llmlog.GatewayRow) { _ = llmlog.GatewayEntry(row) }
	rig.router = memMount(rig, oai.ChatCompletions(rig.deps), &held)

	// cred: as many holders as fit with room left for the probe (a probe
	// that gets 503 measures the refusal, not the heap's latency); cred_over:
	// three more, the budget refused to everyone else; pub: one fits, three
	// try.
	model, holders := oai.CGModel, int((memBudget-9<<20)/(int64(oai.RequestReserveFactor)*memCap+dimRespShare+oai.JSONStateReserve))
	switch workload {
	case "cred_over":
		holders += 3
	case "pub":
		model, holders = oai.CGModel+"pub", 3
	}
	body := dimChatText(model, memCap-16, "a")
	probe := dimChatText(oai.CGModel, 64, "a")

	names := []string{
		"/gc/cycles/total:gc-cycles", "/gc/heap/live:bytes", "/memory/classes/heap/objects:bytes",
		"/memory/classes/heap/unused:bytes", "/cpu/classes/gc/total:cpu-seconds", "/cpu/classes/total:cpu-seconds",
		"/cpu/classes/user:cpu-seconds", "/gc/gomemlimit:bytes", "/memory/classes/total:bytes",
	}
	samples := make([]metrics.Sample, len(names))
	for i, n := range names {
		samples[i].Name = n
	}
	read := func() map[string]float64 {
		metrics.Read(samples)
		m := map[string]float64{}
		for _, s := range samples {
			switch s.Value.Kind() {
			case metrics.KindUint64:
				m[s.Name] = float64(s.Value.Uint64())
			case metrics.KindFloat64:
				m[s.Name] = s.Value.Float64()
			}
		}
		return m
	}

	runtime.GC()
	m0 := read()
	cpu0 := dimRusageCPU()
	t0 := time.Now()
	end := t0.Add(time.Duration(secs) * time.Second)

	var liveMax, objMax, inuseMax, totalMax, budgetMax atomic.Int64
	var budgetSum, budgetN, budgetLow atomic.Int64
	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			m := read()
			memMaxInt(&liveMax, int64(m["/gc/heap/live:bytes"]))
			memMaxInt(&objMax, int64(m["/memory/classes/heap/objects:bytes"]))
			memMaxInt(&inuseMax, int64(m["/memory/classes/heap/objects:bytes"]+m["/memory/classes/heap/unused:bytes"]))
			memMaxInt(&totalMax, int64(m["/memory/classes/total:bytes"]))
			u := rig.budget.InUse()
			memMaxInt(&budgetMax, u)
			budgetSum.Add(u)
			budgetN.Add(1)
			if memBudget-u < 9<<20 {
				budgetLow.Add(1)
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	var hServed, hRefused, hOther atomic.Int64
	var hw sync.WaitGroup
	for range holders {
		hw.Add(1)
		go func() {
			defer hw.Done()
			for time.Now().Before(end) {
				st, _, _ := dimServe(rig, body)
				switch st {
				case http.StatusOK:
					hServed.Add(1)
				case http.StatusServiceUnavailable:
					hRefused.Add(1)
					time.Sleep(10 * time.Millisecond)
				default:
					hOther.Add(1)
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()
	}
	var lat, lat200 []float64
	var p200, p503, pOther int
	tick := time.NewTicker(100 * time.Millisecond)
	for now := range tick.C {
		if now.After(end) {
			break
		}
		st, d, _ := dimServe(rig, probe)
		ms := float64(d.Microseconds()) / 1000
		lat = append(lat, ms)
		switch st {
		case http.StatusOK:
			p200++
			lat200 = append(lat200, ms)
		case http.StatusServiceUnavailable:
			p503++
		default:
			pOther++
		}
	}
	tick.Stop()
	hw.Wait()
	wall := time.Since(t0).Seconds()
	close(stop)
	bg.Wait()
	m1 := read()
	cpu1 := dimRusageCPU()
	hwm, rss := dimProcStatus()
	sum, handled := held.sum()
	gcCPU := m1["/cpu/classes/gc/total:cpu-seconds"] - m0["/cpu/classes/gc/total:cpu-seconds"]
	userCPU := m1["/cpu/classes/user:cpu-seconds"] - m0["/cpu/classes/user:cpu-seconds"]
	capCPU := m1["/cpu/classes/total:cpu-seconds"] - m0["/cpu/classes/total:cpu-seconds"]
	cycles := m1["/gc/cycles/total:gc-cycles"] - m0["/gc/cycles/total:gc-cycles"]
	res := map[string]any{
		"workload": workload, "gomemlimit_env": os.Getenv("GOMEMLIMIT"), "gomemlimit_bytes": m1["/gc/gomemlimit:bytes"],
		"secs": wall, "holders": holders, "hold_ms": hold.Milliseconds(), "gomaxprocs": runtime.GOMAXPROCS(0),
		"holder_served": hServed.Load(), "holder_503": hRefused.Load(), "holder_other": hOther.Load(),
		"held_mean_mib": dimMiB(sum / int64(max(handled, 1))), "held_max_mib": dimMiB(slices.Max(append(slices.Clone(held.vals), 0))), "handled": handled,
		"probe_n": len(lat), "probe_200": p200, "probe_503": p503, "probe_other": pOther,
		"probe_p50_ms": dimPct(lat, 0.5), "probe_p99_ms": dimPct(lat, 0.99), "probe_max_ms": dimPct(lat, 1),
		"probe200_p50_ms": dimPct(lat200, 0.5), "probe200_p99_ms": dimPct(lat200, 0.99),
		"gc_cycles": cycles, "gc_per_s": cycles / wall, "gc_cpu_s": gcCPU, "user_cpu_s": userCPU, "cap_cpu_s": capCPU,
		"gc_share_of_capacity": gcCPU / capCPU, "gc_share_of_busy": gcCPU / (gcCPU + userCPU), "rusage_cpu_s": cpu1 - cpu0,
		"live_heap_max_mib": dimMiB(liveMax.Load()), "heap_objects_max_mib": dimMiB(objMax.Load()), "heap_inuse_max_mib": dimMiB(inuseMax.Load()),
		"runtime_total_max_mib": dimMiB(totalMax.Load()), "budget_in_use_max_mib": dimMiB(budgetMax.Load()),
		"budget_in_use_mean_mib":       dimMiB(budgetSum.Load() / max(budgetN.Load(), 1)),
		"budget_free_below_9MiB_share": float64(budgetLow.Load()) / float64(max(budgetN.Load(), 1)),
		"vm_hwm_mib":                   float64(hwm) / 1024, "vm_rss_end_mib": float64(rss) / 1024,
	}
	line, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("BUDGETDIMCHILD %s\n", line)
}
