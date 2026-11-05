package oai_test

// The ledger of the in-flight budget over mixed outcomes (FOLGE-P, the probe
// of Fremd-Review 1 kept as a standing gate, hardening H-8): requests of
// every outcome over the real route stack (rig.router: Recover, PeerIngress,
// InflightBudget, the route) under budget contention — 400 in every parse
// phase, 404, 503 at every booking (the scan's of the public alias among
// them), failover, a read cut off, a body without a length, one shorter than
// it declares, a key named in a refusal. Afterwards the budget is back at 0
// and every meter reservation ended exactly once.
//
//	CTX_FOLGEP_LEDGER_N=100000 go test -run '^TestChatBudgetReturnsToZero$' ./internal/oai/

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

type ledgerAbortReader struct {
	data []byte
	i    int
}

func (r *ledgerAbortReader) Read(p []byte) (int, error) {
	if r.i >= len(r.data) {
		return 0, errors.New("client went away")
	}
	n := copy(p, r.data[r.i:])
	r.i += n
	return n, nil
}

func TestChatBudgetReturnsToZero(t *testing.T) {
	rig := &chatRig{}
	ledgerProbe(t, nil, func(r *chatRig) { rig = r })
	l := rig.limiter
	if ended := l.settles.Load() + l.keeps.Load() + l.aborts.Load() + l.rejects.Load(); ended != l.admits.Load() {
		t.Errorf("%d meter reservations admitted, %d ended (settle %d, keep %d, abort %d, reject %d)", l.admits.Load(), ended,
			l.settles.Load(), l.keeps.Load(), l.aborts.Load(), l.rejects.Load())
	}
}

// ledgerProbe sends CTX_FOLGEP_LEDGER_N (default 3000) requests of every form
// from 16 goroutines over a rig built with opts — its first row answering
// 404 and carrying extra as its extra_body — and checks that the budget is
// back at 0 and every form was answered; it returns the outcomes.
func ledgerProbe(t *testing.T, extra map[string]any, opts ...rigOption) map[string]int {
	t.Helper()
	n := 3000
	if v, err := strconv.Atoi(os.Getenv("CTX_FOLGEP_LEDGER_N")); err == nil {
		n = v
	}
	failing := newUpstream(t, answerSGLang)
	failing.setRespond(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ok := newUpstream(t, answerSGLang)
	rows := []backends.Backend{failing.row("rowa", backends.TrustFull, 100), ok.row("rowb", backends.TrustFull, 90)}
	rows[0].ExtraBody = extra
	rig := newChatRig(t, rows, 8, 1, opts...)
	var maxResp int64
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget = 6 * (3*(1<<20) + 2*s.MaxResponseBytes) // six served requests at a time, then contention
		maxResp = s.MaxResponseBytes
	})
	big := strings.Repeat("a", 1<<19)
	deep := `{"model":"chat","messages":[{"role":"user","content":"x"}],"response_format":` + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + `}`
	type form struct {
		name    string
		body    string
		chunked bool
		abort   bool
		short   bool
	}
	forms := []form{
		{name: "ok", body: chatBody("chat")},
		{name: "ok-failover-big", body: `{"model":"chat","messages":[{"role":"user","content":"` + big + `"}]}`},
		{name: "developer", body: `{"model":"chat","messages":[{"role":"developer","content":"` + big + `"},{"role":"user","content":"x","reasoning":"r"}]}`},
		{name: "invalid-json", body: `{"model":"chat",`},
		{name: "unknown-key", body: chatBody("chat", `"custom_logit_processor":"x"`)},
		{name: "repeat", body: chatBody("chat", `"stop":["a"],"stop":["b"]`)},
		{name: "unknown-model", body: chatBody("nope")},
		{name: "message-400", body: `{"model":"chat","messages":[{"role":"user","content":"x","foo":1}]}`},
		{name: "deep", body: deep},
		{name: "chunked-ok", body: chatBody("chat"), chunked: true},
		{name: "chunked-big", body: `{"model":"chat","messages":[{"role":"user","content":"` + big + big + big + `"}]}`, chunked: true},
		{name: "chunked-abort", body: `{"model":"chat","messages":[{"role":"user","content":"` + big + `"}]}`, chunked: true, abort: true},
		{name: "declared-short", body: chatBody("chat"), short: true},
		{name: "pub-ok", body: chatBody("pub")},
		{name: "pub-big", body: `{"model":"pub","messages":[{"role":"user","content":"` + big + `"}]}`},
		{name: "pub-assign", body: `{"model":"pub","messages":[{"role":"user","content":"` + strings.Repeat("apikey:x ", 1<<14) + `"}]}`},
		{name: "pub-vendor-param", body: chatBody("pub", `"custom_logit_processor":"x"`)},
		{name: "echo-key", body: `{"model":"chat","messages":[{"role":"user","content":"x"}],"` + strings.Repeat("<", 1<<19) + `":0}`},
		{name: "big-unknown-keys", body: func() string {
			var b strings.Builder
			b.WriteString(`{"model":"chat","messages":[{"role":"user","content":"x"}]`)
			for i := 0; b.Len() < 1<<19; i++ {
				fmt.Fprintf(&b, `,"k%07d":0`, i)
			}
			return b.String() + "}"
		}()},
	}
	var mu sync.Mutex
	outcomes := map[string]int{}
	var next atomic.Int64
	var wg sync.WaitGroup
	var peak atomic.Int64
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= int64(n) {
					return
				}
				f := forms[i%int64(len(forms))]
				var body io.Reader = strings.NewReader(f.body)
				if f.abort {
					body = &ledgerAbortReader{data: []byte(f.body[:len(f.body)/2])}
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
				switch {
				case f.chunked:
					req.ContentLength = -1
					req.Body = io.NopCloser(struct{ io.Reader }{body})
				case f.short:
					req.ContentLength = int64(len(f.body)) + 100
				}
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				rig.router.ServeHTTP(w, req)
				if u := rig.budget.InUse(); u > peak.Load() {
					peak.Store(u)
				}
				var e struct {
					Error struct{ Code string } `json:"error"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				mu.Lock()
				outcomes[fmt.Sprintf("%s %d %s", f.name, w.Code, e.Error.Code)]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	keys := make([]string, 0, len(outcomes))
	for k := range outcomes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&b, "%s ×%d\n", k, outcomes[k])
	}
	t.Logf("n=%d maxResp=%d peak InUse=%d outcomes:\n%s", n, maxResp, peak.Load(), b.String())
	t.Logf("limiter admits=%d settles=%d keeps=%d aborts=%d rejects=%d rows=%d", rig.limiter.admits.Load(), rig.limiter.settles.Load(),
		rig.limiter.keeps.Load(), rig.limiter.aborts.Load(), rig.limiter.rejects.Load(), len(rig.rows.all()))
	if got := rig.budget.InUse(); got != 0 {
		t.Fatalf("the budget holds %d bytes after %d requests", got, n)
	}
	for _, f := range forms {
		if !slices.ContainsFunc(keys, func(k string) bool { return strings.HasPrefix(k, f.name+" ") }) {
			t.Errorf("form %s never answered", f.name)
		}
	}
	return outcomes
}
