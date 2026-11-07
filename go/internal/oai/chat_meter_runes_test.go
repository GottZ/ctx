package oai

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/promptguard"
)

// runesLimiter admits every request with one reservation and keeps the
// estimate it was handed; neither allocates.
type runesLimiter struct {
	res runesReservation
	est Estimate
}

func (l *runesLimiter) Admit(_ context.Context, _ *auth.ConsumerAuth, _ modelalias.Alias, est Estimate) (LimitReservation, *LimitRefusal) {
	l.est = est
	return &l.res, nil
}

type runesReservation struct{}

func (*runesReservation) Settle(dispatch.Usage)        {}
func (*runesReservation) Keep()                        {}
func (*runesReservation) Abort()                       {}
func (*runesReservation) Reject(string, time.Duration) {}
func (*runesReservation) Headers() http.Header         { return nil }
func (*runesReservation) Held() int64                  { return 0 }

// runeCopySlack is what the two meter calls below may allocate: far below
// the copy they must not make (the bytes of the message array after its
// first non-ASCII one, 1 MiB here).
const runeCopySlack = 4 << 10

// TestMeterRuneCountsCopyNothing (W02-08 hardening 4, F5-4; Delta-Review 5
// mutant B24): the two rune counts of the meter path — admitLimiter's
// Estimate.PromptRunes and streamTokens' prompt estimate — count the parsed
// message array with runeCount. utf8.RuneCount converts the bytes after the
// first non-ASCII one into a string on go1.27: a copy of the array outside
// the reservation (the stream heap gate, which CI never runs, measured
// 32.2 against 26.0 MiB at 8 MiB). This pin holds both call sites in the
// -short path, the count unchanged.
func TestMeterRuneCountsCopyNothing(t *testing.T) {
	msgs := []byte(`[{"role":"developer","content":"` + strings.Repeat("é", 512<<10) + `"}]`)
	runes := utf8.RuneCount(msgs)
	lim := &runesLimiter{}
	c := &chatCall{d: Deps{Limiter: lim}, ctx: context.Background(), req: &chatRequest{messages: msgs}, maxTokens: 64}
	admitted := false
	admit := allocatedBy(func() { admitted = c.admitLimiter(64) })
	if admit > runeCopySlack {
		t.Errorf("admitLimiter over a %d-byte non-ASCII message array allocates %d bytes, want ≤ %d", len(msgs), admit, runeCopySlack)
	}
	if !admitted || lim.est.PromptRunes != runes || lim.est.PromptBytes != len(msgs) {
		t.Errorf("admitted %v, estimate %d runes / %d bytes, want true, %d / %d", admitted, lim.est.PromptRunes, lim.est.PromptBytes, runes, len(msgs))
	}
	st := &walkState{wired: 1, stream: &llm.RawStreamResult{}}
	stream := allocatedBy(func() { c.streamTokens(st) })
	if stream > runeCopySlack {
		t.Errorf("streamTokens over a %d-byte non-ASCII message array allocates %d bytes, want ≤ %d", len(msgs), stream, runeCopySlack)
	}
	if want := promptguard.TokenEstimate(runes); st.tokens.PromptTokens != want {
		t.Errorf("streamTokens estimates %d prompt tokens, want %d", st.tokens.PromptTokens, want)
	}
	t.Logf("%d-byte message array, %d runes: admitLimiter allocates %d bytes, streamTokens %d", len(msgs), runes, admit, stream)
}
