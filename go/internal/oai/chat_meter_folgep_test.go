package oai_test

// FOLGE-P over the real meter (rebase onto W06-10): the per-consumer meter
// (keyquota, the rig's real planner and dispatcher) and the parse's
// reservation ledger together. (1) A6 — an attempt's upstream body past the
// budget: before any wire call the meter reservation goes back in full
// (Abort) and there is no llmlog row; on a failover attempt after a row that
// was written to, it keeps its charge — never a full refund after the wire.
// (2) A5 — the meter's estimate counts the messages as they go upstream: as
// sent, and a rewritten developer list in its raw form, not the HTML-escaped
// one the base sent. (3) The scan's booking (H-8) and the cut echo (H-9) come
// before the meter: a request refused there costs the meter nothing — no
// Admit. (4) The ledger over mixed outcomes with the meter in the path: the
// budget back at 0, every meter reservation ended exactly once.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// estMeter is an endMeter that counts its admissions and keeps the last
// estimate it was asked for.
type estMeter struct {
	*endMeter
	admits atomic.Int64
	mu     sync.Mutex
	last   oai.Estimate
}

func newEstMeter(lim keyquota.Limits) *estMeter {
	return &estMeter{endMeter: &endMeter{rigMeter: newRigMeter(lim, true)}}
}

func (m *estMeter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	m.mu.Lock()
	m.last = est
	m.mu.Unlock()
	res, ref := m.endMeter.Admit(ctx, ca, a, est)
	if res != nil {
		m.admits.Add(1)
	}
	return res, ref
}

// ends is the number of meter reservations ended so far.
func (m *estMeter) ends() int {
	m.endMeter.mu.Lock()
	defer m.endMeter.mu.Unlock()
	return len(m.endMeter.ends)
}

// meterRig is a rig over rows with an estMeter as its limiter.
func meterRig(t *testing.T, lim keyquota.Limits, rows ...backends.Backend) (*chatRig, *estMeter) {
	t.Helper()
	m := newEstMeter(lim)
	return newChatRig(t, rows, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = m })), m
}

var wideMeter = keyquota.Limits{RPM: 1 << 30, TPM: 1 << 50, CompletionReserve: 1024}

// TestMeterUpstreamCopyPastTheBudget (1): A6 with the real meter.
func TestMeterUpstreamCopyPastTheBudget(t *testing.T) {
	small := []byte(chatBody("chat", `"stop":["`+ledgerMark+`"]`))
	big := []byte(`{"model":"chat","messages":[{"role":"user","content":"` + ledgerMark + strings.Repeat("a", 1<<20) + `"}]}`)
	for _, tc := range []struct {
		name    string
		body    []byte
		failing int
		extra   map[string]any
		ends    string
		rows    int
		charged bool
	}{
		{name: "first attempt: no wire call, a full refund", body: small, extra: map[string]any{"vendor_blob": strings.Repeat("z", 64<<10)},
			ends: "abort", rows: 0},
		{name: "failover after a written row: the charge stays", body: big, failing: 1, ends: "keep", rows: 1, charged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows []backends.Backend
			for i := 0; i <= tc.failing; i++ {
				u := newUpstream(t, answerSGLang)
				if i < tc.failing {
					u.setRespond(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
				}
				b := u.row("row"+string(rune('a'+i)), backends.TrustFull, 100-i)
				if i == tc.failing {
					b.ExtraBody = tc.extra
				}
				rows = append(rows, b)
			}
			rig, m := meterRig(t, wideMeter, rows...)
			rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 3*int64(len(tc.body)) + 2*s.MaxResponseBytes })
			w, _ := ledgerPost(t, rig, tc.body, false)
			var e struct {
				Error struct{ Code string } `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != http.StatusServiceUnavailable || e.Error.Code != oai.CodeServerOverloaded {
				t.Fatalf("answer %d %s, want 503 server_overloaded", w.Code, w.Body.Bytes())
			}
			if got := m.endsOf(); got != tc.ends {
				t.Errorf("the meter reservation ended %q, want %q", got, tc.ends)
			}
			if got := len(rig.rows.all()); got != tc.rows {
				t.Errorf("%d llmlog rows, want %d", got, tc.rows)
			}
			if held := m.held(t, rig); tc.charged != (held > 0) {
				t.Errorf("the bucket holds %d tokens after the request, want charged %v", held, tc.charged)
			}
			if rig.budget.InUse() != 0 {
				t.Errorf("the budget holds %d bytes after the request", rig.budget.InUse())
			}
		})
	}
}

// TestMeterEstimateCountsTheUpstreamForm (2): A5 — the estimate is the
// message array as it goes upstream; for a rewritten developer list that is
// its raw form (FOLGE-P P-4), up to six times smaller than the HTML-escaped
// json.Marshal form the base counted for "<", ">" and "&".
func TestMeterEstimateCountsTheUpstreamForm(t *testing.T) {
	lt := strings.Repeat("<>&", 1000)
	for _, tc := range []struct {
		name, body, upstream string
		escaped              bool // the base sent (and counted) the array HTML-escaped
	}{
		{"as sent", `{"model":"chat","messages":[{"role":"user","content":"` + lt + `"}]}`, `[{"role":"user","content":"` + lt + `"}]`, false},
		{"a rewritten developer list", `{"model":"chat","messages":[{"role":"developer","content":"` + lt + `","name":"<n>"}]}`,
			`[{"content":"` + lt + `","name":"<n>","role":"system"}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig, m := meterRig(t, wideMeter, u.row("spark", backends.TrustFull, 10))
			if a := rig.post(t, tc.body); a.status != http.StatusOK {
				t.Fatalf("status %d %s", a.status, a.body)
			}
			m.mu.Lock()
			est := m.last
			m.mu.Unlock()
			base := []byte(tc.upstream)
			if tc.escaped {
				base, _ = json.Marshal(json.RawMessage(tc.upstream)) // json.Marshal of the rewritten array, as the base built it
			}
			t.Logf("FOLGEP estimate %s: prompt_bytes %d (base %d), prompt_runes %d (base %d)", tc.name,
				est.PromptBytes, len(base), est.PromptRunes, utf8.RuneCount(base))
			if est.PromptBytes != len(tc.upstream) || est.PromptRunes != utf8.RuneCount([]byte(tc.upstream)) {
				t.Errorf("estimate %d bytes, %d runes; want the upstream form's %d, %d", est.PromptBytes, est.PromptRunes,
					len(tc.upstream), utf8.RuneCount([]byte(tc.upstream)))
			}
		})
	}
}

// TestRefusedBeforeTheMeter (3): a request the scan's booking refuses (H-8)
// or whose refusal names a cut key (H-9) is answered before the meter: no
// Admit, the bucket untouched.
func TestRefusedBeforeTheMeter(t *testing.T) {
	_, forms := scanForms(256 << 10)
	pub := forms["text_a_space"]
	echo := `{"model":"chat","messages":[{"role":"user","content":"x"}],"` + strings.Repeat("<", 8<<20-128) + `":0}`
	for _, tc := range []struct {
		name   string
		body   []byte
		budget int64
		status int
	}{
		{"the scan's booking", pub, 3*int64(len(pub)) + 2*chatSettings().MaxResponseBytes + int64(len(pub)) + 64<<10, http.StatusServiceUnavailable},
		{"a cut echo", []byte(echo), 256 << 20, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig, m := meterRig(t, wideMeter, u.row("spark", backends.TrustFull, 10))
			rig.setSettings(func(s *oai.Settings) { s.InflightBudget = tc.budget })
			w, _ := ledgerPost(t, rig, tc.body, false)
			if w.Code != tc.status || w.Body.Len() > echoAnswerMax {
				t.Fatalf("answer %d (%d bytes), want %d", w.Code, w.Body.Len(), tc.status)
			}
			if n, held := m.admits.Load(), m.held(t, rig); n != 0 || held != 0 {
				t.Errorf("%d meter admits, %d tokens held, want none", n, held)
			}
		})
	}
}

// TestMeterLedgerOverMixedOutcomes (4): the ledger probe of
// chat_ledger_test.go with the real meter as the limiter.
func TestMeterLedgerOverMixedOutcomes(t *testing.T) {
	m := newEstMeter(wideMeter)
	// The first row's 1 MiB frame makes its upstream body pass the request
	// share: under contention its booking fails before any wire call (the
	// meter's Abort, through close), behind it the 404 row was written to
	// (Keep), and the second row answers (Settle).
	outcomes := ledgerProbe(t, map[string]any{"vendor_blob": strings.Repeat("z", 1<<20)}, withDeps(func(d *oai.Deps) { d.Limiter = m }))
	if admits, ends := m.admits.Load(), m.ends(); int64(ends) != admits || admits == 0 {
		t.Errorf("%d meter reservations admitted, %d ended (%s…)", admits, ends, fmt.Sprintf("%.200s", m.endsOf()))
	}
	kinds := map[string]int{}
	for _, k := range strings.Split(m.endsOf(), ",") {
		kinds[k]++
	}
	t.Logf("FOLGEP meter ledger: %d admits, %d ends %v, %d outcome kinds", m.admits.Load(), m.ends(), kinds, len(outcomes))
	if kinds["abort"] == 0 || kinds["settle"] == 0 {
		t.Errorf("the probe ended no reservation by %v: every end path must be exercised", kinds)
	}
}
