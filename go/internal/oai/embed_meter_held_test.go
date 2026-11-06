package oai_test

// What the embeddings route weighs its booking against (W02-09 rebase round
// 2, part C; lead decision 2026-11-06, one source): the tokens the admission
// charged, read from the reservation (LimitReservation.Held), never rebuilt
// from the estimate and the limits. A limit reload between the admission and
// the end of the request — the meter's limits are settings, hot
// (gateway.embed_tpm_default; oai.Settings.Meter, read by the limiter
// cmd/ctxd wires in) — leaves the booking where the admission put it; a
// route that rebuilt the charge from the settings of the end would book
// against a ceiling no reservation holds.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// TestEmbedMeterHeldSurvivesALimitReload: emb1 (one input a block), the
// engine's block 1 reports u = 2 tokens and reloads the settings' embed TPM
// (the admission ran under 100) while it holds the block, the client goes
// away during block 2 (no usage there, its estimate counts).
//   - TPM 100 sinks to 50: inputs of 8 and 100 runes, estimate 60 (block 2:
//     56), charged 60 ⇒ 2 + 56 = 58 < 60 ⇒ settle 58 (against the limits of
//     the end, 50: keep).
//   - TPM 100 rises to 1000: inputs of 8 and 800 runes, estimate 449 (block
//     2: 445), charged 100 ⇒ 2 + 445 ≥ 100 ⇒ keep 100 (against the limits of
//     the end, 449: settle 447).
func TestEmbedMeterHeldSurvivesALimitReload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		long   int // runes of the second input
		tpm    int64
		end    string
		settle int64
	}{
		{"TPM sinks during the request", 100, 50, "settle", 58},
		{"TPM rises during the request", 800, 1000, "keep", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := newEmbedMeter(keyquota.Limits{RPM: 1000, TPM: 100, CompletionReserve: 1024, Concurrency: noL1})
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = m }))
			rig.setSettings(func(s *oai.Settings) { s.Meter.Embed = oai.OpLimits{RPM: 1000, TPM: 100} })
			var reloaded atomic.Bool
			e.setRespond(func(w http.ResponseWriter, r *http.Request, n int64, in []string) bool {
				if n == 2 {
					cancel()
					<-r.Context().Done()
					return true
				}
				rig.setSettings(func(s *oai.Settings) { s.Meter.Embed.TPM = tc.tpm }) // the reload, while block 1 is on the engine
				reloaded.Store(true)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, embedAnswerUsage(in, 8, 2))
				return true
			})
			status, body, err := postEmbedAndWait(t, rig, ctx, embedBody("emb1", []string{"text 000", strings.Repeat("x", tc.long)}))
			ends, settled := m.endsOf(), m.settledNow()
			t.Logf("%s: answer %d err=%v %.80s; engine hits %d; ends %s; settled %+v (want %s %d)", tc.name, status, err, body, e.hits.Load(), ends, settled, tc.end, tc.settle)
			if e.hits.Load() != 2 || !reloaded.Load() {
				t.Fatalf("engine hits %d, reloaded %v — want the client gone during block 2 after the reload", e.hits.Load(), reloaded.Load())
			}
			if ends != tc.end {
				t.Errorf("ends %q, want %s", ends, tc.end)
			}
			switch {
			case tc.end == "settle" && (len(settled) != 1 || int64(settled[0].PromptTokens) != tc.settle):
				t.Errorf("settled %+v, want one Settle of %d", settled, tc.settle)
			case tc.end == "keep" && len(settled) != 0:
				t.Errorf("settled %+v, want none — the reservation keeps what the admission charged", settled)
			}
		})
	}
}
