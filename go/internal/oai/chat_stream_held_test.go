package oai_test

// Part B of the W02-08 rebase round 2: a cut stream's estimate is weighed
// against what the admission charged — LimitReservation.Held (W02-09, one
// source in keyquota) — not against a rebuild of the charge in the route
// from the request's settings snapshot and the admission's headers
// (Delta-Review 4 F4-1 as built in hardening 3; that rebuild, the function
// reservedTokens, was removed by this part). The
// rebuild misses (1) a reserve that moved between the request's settings
// snapshot and the limiter's Admit (a hot reload of
// gateway.tpm_reserve_default) and (2) a limiter that reports no bucket
// capacity in x-ratelimit-limit-tokens: either way the estimate settled
// above the charge — more than Keep.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// headerlessMeter is the rig's endMeter whose reservations report no
// headers — a limiter without x-ratelimit-limit-tokens.
type headerlessMeter struct{ *endMeter }

func (m headerlessMeter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, ref := m.endMeter.Admit(ctx, ca, a, est)
	if res == nil {
		return nil, ref
	}
	return headerlessReservation{res}, nil
}

type headerlessReservation struct{ oai.LimitReservation }

func (headerlessReservation) Headers() http.Header { return nil }

// cutAfterAFrame runs one stream of body whose backend sends one frame with
// text and then holds, cut by its client after that frame; it returns how
// the reservation ended, the rig and its meter.
func cutAfterAFrame(t *testing.T, lim keyquota.Limits, headerless bool, body, text string) (string, *chatRig, *endMeter) {
	t.Helper()
	hold := make(chan struct{})
	defer close(hold)
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{data: sglangChunk(text, "")}, {hold: hold}}, nil, nil))
	m := &endMeter{rigMeter: newRigMeter(lim, true)}
	var limiter oai.Limiter = m
	if headerless {
		limiter = headerlessMeter{m}
	}
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withDeps(func(d *oai.Deps) { d.Limiter = limiter }))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ls, each := openAndRead(t, ctx, serveHTTP(t, rig.router), body)
	if _, ok := nextData(each, 5*time.Second); !ok {
		t.Fatal("no frame")
	}
	cancel()
	_ = ls.Close()
	if !waitEnded(m) {
		t.Fatal("the stream's reservation never ended")
	}
	return m.endsOf(), rig, m
}

// TestStreamCutEstimateWeighsAgainstHeld: the estimate of a cut stream books
// at most what the admission charged.
//
//   - the limiter's completion reserve 100 where the rebuild took the
//     settings' 1 024 (a reload between the request's snapshot and the
//     admission; red run with the rig's Meter.TPMReserve 1 024, which the
//     route read until this part): the admission charges 18 + 100 = 118;
//     a frame of 400 runes estimates 18 + 223 = 241 ⇒ Keep, 118 in the
//     bucket (red with the rebuild: 18 + 1 024 = 1 042 > 241 ⇒ Settle 241).
//   - TPM 2 000 and a limiter without x-ratelimit-limit-tokens: a prompt
//     estimate of 4 462 is charged at the capacity, 2 000; the estimate
//     4 462 + 3 ⇒ Keep — the next request (1 042 tokens) waits 31 260 ms,
//     as after a Keep (red with the rebuild: uncapped 4 462 + 1 024 ⇒
//     Settle 4 465, a debt past the capacity).
func TestStreamCutEstimateWeighsAgainstHeld(t *testing.T) {
	t.Run("a reserve that moved between the snapshot and the admission", func(t *testing.T) {
		body := streamBody("chat")
		prompt := promptEstimate(body)
		lim := keyquota.Limits{RPM: 1000, TPM: 1 << 30, CompletionReserve: 100, Concurrency: noL1}
		ends, rig, m := cutAfterAFrame(t, lim, false, body, strings.Repeat("abcd ", 80))
		held := m.held(t, rig)
		t.Logf("reserve 100 at the admission, 1024 in the snapshot: ends %s, the bucket holds %d (charged %d)", ends, held, prompt+100)
		if ends != "keep" || held != prompt+100 {
			t.Errorf("ends %s, the bucket holds %d tokens — want Keep of what the admission charged, %d + 100 = %d", ends, held, prompt, prompt+100)
		}
	})
	t.Run("a limiter without the capacity header", func(t *testing.T) {
		body := strings.Replace(streamBody("chat"), `"content":"hi"`, `"content":"`+strings.Repeat("abcdefgh", 1000)+`"`, 1)
		if p := promptEstimate(body); p != 4462 {
			t.Fatalf("prompt estimate %d, want 4462", p)
		}
		lim := keyquota.Limits{RPM: 1000, TPM: 2000, CompletionReserve: 1024, Concurrency: noL1}
		ends, rig, _ := cutAfterAFrame(t, lim, true, body, "a cat")
		a := rig.post(t, chatBody("chat"))
		wait := a.header.Get("Retry-After-Ms")
		t.Logf("TPM 2000, no capacity header: ends %s; the next request %d after %s ms (a Keep: 31260)", ends, a.status, wait)
		if ends != "keep" || a.status != http.StatusTooManyRequests || wait != "31260" {
			t.Errorf("ends %s, the next request %d after %s ms — want Keep of the capacity the admission charged and the wait of a kept reservation, 31260 ms",
				ends, a.status, wait)
		}
	})
}
