package oai_test

// RC-D cluster C8, budget package (S2): the gate of the floor's route terms
// (oai.EmbedRequestFloor, oai.RerankRequestFloor, and the chat need the
// configuration computes, chatNeedOf here). Per route, class and body cap
// the worst form the floor describes runs once under a budget no booking
// passes, so the reservation at the handler's end is the request's whole
// need: it must not pass the route's floor need plus
// oai.UpstreamFrameReserve (the bound holds; the rows' model name is one
// byte, its share in every block is the reserve's), and it must reach at
// least floorTightness × the need (the bound is not loose).

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// floorTightness is the least share of its floor need a worst form must
// hold. The floor counts every term at its worst; the worst forms reach all
// of them but the two nesting states (at most 1 280 128 B — the forms are
// flat) and the read term where a cap is no sum of the read's chunks.
// Measured after FOLGE-R (bau report RCD-c8-hardening-S2): 0.81 and above in
// every row. A floor that books the blocks of a smaller block
// count (e.g. rerank's 1000 documents in blocks of 128, the S1 rig's alias:
// 373 of 532 MiB at 8 MiB) or forgets a route's answer share would fall
// under it.
const floorTightness = 0.7

// floorProbeBudget is large enough that no booking of one request at the
// largest cap is refused.
const floorProbeBudget = 4 << 30

// floorCaps are the body caps of the gate.
var floorCaps = []int64{64 << 10, 1 << 20, 2 << 20, 8 << 20}

// The worst forms' rigs run every input or document as its own block: the
// row's external capacity E is 1 (2 slots, 1 reserved), and a block is
// min(units, sub_batch, E) units (handler.ExternalWeightPlan) — the
// alias's sub_batch splits a request into at most modelalias.MaxSubBlocks
// blocks, the capacity into one per unit. The aliases: embsec0/embpub0
// (rdumpNoDims: 256 inputs, no native dimension), rrwide/rrpub (1000
// documents).
const floorSlots, floorReserved = 2, 1

// floorEmbedBody is 256 inputs of bytes 0xff (each decoded to U+FFFD, three
// bytes, written as three) filling size bytes.
func floorEmbedBody(model string, size int) []byte {
	pre, post := `{"model":"`+model+`","input":[`, `]}`
	per := (size-len(pre)-len(post))/256 - 3
	var b bytes.Buffer
	b.WriteString(pre)
	for i := range 256 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + strings.Repeat("\xff", per) + `"`)
	}
	b.WriteString(post)
	return b.Bytes()
}

// floorRerankBody is 1000 documents of bytes 0xff (each decoded to U+FFFD,
// three bytes, written as three — since FOLGE-R the rerank wire writes '<'
// as one byte, the embeddings wire's encoding) filling size bytes, the
// query one byte.
func floorRerankBody(model string, size int) []byte {
	pre, post := `{"model":"`+model+`","query":"q","documents":[`, `]}`
	per := (size-len(pre)-len(post))/1000 - 3
	var b bytes.Buffer
	b.WriteString(pre)
	for i := range 1000 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + strings.Repeat("\xff", per) + `"`)
	}
	b.WriteString(post)
	return b.Bytes()
}

// chatNeedOf is the configuration's chat need (config gatewayChatNeed plus
// the response share, 2 × the response cap here), hand-computed.
func chatNeedOf(n int64, scanned bool, resp int64) int64 {
	need := oai.RequestReserveFactor*n + chunkedReadTerm(n) + oai.JSONStateReserve + 2*resp
	if scanned {
		need += oai.ScanReserveFactor*n + oai.ScanReserveBase + oai.ScanDepthReserve
	}
	return need
}

// floorPost sends body to path without a declared length (the read the
// floor counts at its worst).
func floorPost(h http.Handler, path string, body []byte) (int, string) {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.ContentLength = -1
	req.Body = io.NopCloser(struct{ io.Reader }{bytes.NewReader(body)})
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, fmt.Sprintf("%.200s", w.Body.String())
}

// TestBudgetFloorHoldsTheWorstForms is the gate of the floor's route needs:
// at each body cap, per route and class, the worst form's reservation lies
// within [floorTightness × need, need + oai.UpstreamFrameReserve].
func TestBudgetFloorHoldsTheWorstForms(t *testing.T) {
	if testing.Short() {
		t.Skip("eight-MiB bodies over the public scan and 1000 wire calls per request: outside -short")
	}
	if raceEnabled {
		t.Skip("the scan of eight MiB under the race detector takes minutes; the bound is arithmetic, not timing")
	}
	const resp = 4 << 20
	const floats = 1 << 20
	type form struct {
		route, model string
		scanned      bool
	}
	for _, f := range []form{
		{"chat", "chat", false}, {"chat", "pub", true},
		{"embeddings", "embsec0", false}, {"embeddings", "embpub0", true},
		{"rerank", "rrwide", false}, {"rerank", "rrpub", true},
	} {
		for _, c := range floorCaps {
			t.Run(fmt.Sprintf("%s/%s/cap=%d", f.route, f.model, c), func(t *testing.T) {
				var held atomic.Int64
				var h http.Handler
				var path string
				var body []byte
				var need int64
				settings := func(s *oai.Settings) {
					s.InflightBudget, s.MaxRequestBytes, s.ScanMaxRequestBytes, s.MaxResponseBytes = floorProbeBudget, c, c, resp
					s.EmbeddingMaxFloats, s.BodyLogging = floats, llmlog.BodyLoggingNone
				}
				switch f.route {
				case "chat":
					u := newUpstream(t, answerSGLang)
					row := u.row("spark", backends.TrustFull, 10)
					row.Model = "m"
					rig := newChatRig(t, []backends.Backend{row}, 4, 1)
					rig.setSettings(settings)
					h, path = rig.mount(scanCapHeld(oai.ChatCompletions(rig.deps), &held)), "/v1/chat/completions"
					body = []byte(deepestBody(int(c), f.model, "user"))
					need = chatNeedOf(c, f.scanned, resp)
				case "embeddings":
					e := newEmbedEngine(t, 8)
					row := e.row("spark-embed", backends.ProtocolOpenAI, 10)
					row.Model = "m"
					rig := newEmbedRig(t, []backends.Backend{row}, floorSlots, floorReserved, withoutCensus)
					rig.reg.reload(t, rdumpNoDims(false))
					rig.setSettings(settings)
					h, path = rig.mountEmbed(scanCapHeld(oai.Embeddings(rig.deps), &held)), "/v1/embeddings"
					body = floorEmbedBody(f.model, int(c))
					need = oai.EmbedRequestFloor(c, f.scanned, floats)
				case "rerank":
					srv := leanRerankEngine(t)
					row := backends.Backend{
						ID: "id-lean", Name: "lean", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m",
						Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
						Priority: 10, Timeouts: map[string]int{"rerank": 60},
					}
					rig := newRerankRig(t, []backends.Backend{row}, floorSlots, floorReserved, withoutCensus)
					rig.setSettings(settings)
					h, path = rig.mountRerank(scanCapHeld(oai.Rerank(rig.deps, oai.RerankJina), &held), oai.Rerank(rig.deps, oai.RerankCohere)), "/v1/rerank"
					body = floorRerankBody(f.model, int(c))
					need = oai.RerankRequestFloor(c, f.scanned)
				}
				if int64(len(body)) > c {
					t.Fatalf("body %d B past the cap %d", len(body), c)
				}
				held.Store(-1)
				st, msg := floorPost(h, path, body)
				if st != http.StatusOK {
					t.Fatalf("status %d %s, want 200 at a budget no booking passes", st, msg)
				}
				got := held.Load()
				ratio := float64(got) / float64(need)
				t.Logf("C8FLOOR route=%s model=%s scanned=%v cap=%d body=%d held=%d need=%d ratio=%.4f", f.route, f.model, f.scanned, c, len(body), got, need, ratio)
				if got > need+oai.UpstreamFrameReserve {
					t.Errorf("held %d B > need %d + oai.UpstreamFrameReserve %d: the floor does not hold the worst form", got, need, oai.UpstreamFrameReserve)
				}
				if ratio < floorTightness {
					t.Errorf("held %d B = %.3f × need %d < %.2f: the floor is loose", got, ratio, need, floorTightness)
				}
			})
		}
	}
}
