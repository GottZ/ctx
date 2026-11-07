//go:build !race

package rerankwire_test

// The wire gates of FOLGE-R (RC-D S1, cluster C8) over rerank.Score
// itself, with this package's harness (raw TCP upstream, pools cold, the
// largest of three): the factor of a wire call per body byte and the bound
// of a call that reads a hostile or echoing answer. Red against the S1
// stubs (json.Marshal, the 1-MiB read, the base's decode), green after S2.

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/rerank"
)

// folgeRFactorMax is the sollwert of the wire's allocation per body byte
// (dispatch C8: "Faktor ≈ 1", gate ≤ 1.5 against 6.6–7.1 × on the base).
const folgeRFactorMax = 1.5

// folgeRMost is the largest of three cold Traw measurements of c.
func folgeRMost(t *testing.T, h *wireHarness, c *wireCase) uint64 {
	t.Helper()
	h.rawUp.cur.Store(newWirePayload(c.resp))
	var most uint64
	for range 3 {
		b, _, err := h.measureOnce("Traw", c, false)
		if err != nil && !c.hostile {
			t.Fatalf("%s: %v", c.name, err)
		}
		most = max(most, b)
	}
	return most
}

// TestFolgeRWireFactorIsAboutOne is S-W1: what a wire call allocates per
// byte of its body beyond a small call over the same documents' count —
// ASCII documents per body byte (the build: 6.3 × on the base), documents of
// "<" per document byte (38.2 × on the base: six bytes on the wire per byte,
// then the encoder's factor), control characters per body byte (their six
// bytes are JSON's, not the writer's) — at most folgeRFactorMax.
func TestFolgeRWireFactorIsAboutOne(t *testing.T) {
	h := newWireHarness(t)
	small := newWireCase(t, h.query, "n128/64B", 128, 64, wireText(64), nil, false)
	aSmall := folgeRMost(t, h, small)
	lSmall := rerank.RequestBodyLen("M", h.query, small.docs)
	dSmall := small.d()
	for _, c := range []struct {
		name     string
		size     int
		doc      string
		perDocB  bool // per document byte instead of per body byte
		response []byte
	}{
		{"ascii n128/64KiB per body byte", 64 << 10, wireText(64 << 10), false, nil},
		{"lt n128/4KiB per document byte", 4 << 10, strings.Repeat("<", 4<<10), true, nil},
		{"ctl n128/4KiB per body byte", 4 << 10, strings.Repeat("\x01", 4<<10), false, nil},
	} {
		big := newWireCase(t, h.query, c.name, 128, c.size, c.doc, small.resp, false)
		a := folgeRMost(t, h, big)
		den := float64(rerank.RequestBodyLen("M", h.query, big.docs) - lSmall)
		if c.perDocB {
			den = float64(big.d() - dSmall)
		}
		f := (float64(a) - float64(aSmall)) / den
		t.Logf("FOLGE-R wire factor: %-32s allocated %d B (small %d B) over %.0f B ⇒ %.3f ×", c.name, a, aSmall, den, f)
		if f > folgeRFactorMax {
			t.Errorf("%s: a wire call allocates %.3f × per byte, want at most %.1f × (FOLGE-R S-W1: the body in one allocation of its length)", c.name, f, folgeRFactorMax)
		}
	}
}

// folgeREchoAnswer is a vLLM answer to n documents: every entry echoes its
// document (vLLM fills RerankResult.document whatever return_documents says).
func folgeREchoAnswer(docs []string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":"rerank-1","model":"m","usage":{"total_tokens":9},"results":[`)
	for i, d := range docs {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"index":%d,"document":{"text":%s,"multi_modal":null},"relevance_score":%s}`, i, strconv.Quote(d), strconv.FormatFloat(0.5-float64(i)/1e4, 'g', -1, 64))
	}
	b.WriteString("]}")
	return b.Bytes()
}

// TestFolgeRAnswerReadIsBooked is S-W2 at the call: a wire call that reads
// one of W02-10's hostile answers (the 1-MiB read cap of entries: up to
// 126.7 MB TotalAlloc on the base) or a vLLM answer that echoes every
// document allocates at most what the route books for the call — its
// wire-call term from oai's exported constants (wireCallBooking) plus the
// answer's read room of one body length (the echo of every document, the
// contract of rerank.answerReadCap) — refused or not.
func TestFolgeRAnswerReadIsBooked(t *testing.T) {
	h := newWireHarness(t)
	cases := wireHostileCases(t, h.query)
	echo := newWireCase(t, h.query, "echo-vllm n128/4KiB", 128, 4<<10, wireText(4<<10), nil, false)
	echo.resp = folgeREchoAnswer(echo.docs)
	cases = append(cases, echo)
	for _, c := range cases {
		a := folgeRMost(t, h, c)
		l := rerank.RequestBodyLen("M", h.query, c.docs)
		bound := wireCallBooking(l, c.n) + int64(l)
		t.Logf("FOLGE-R answer read: %-20s answer=%d B n=%d allocated=%d B bound=%d B (%.1f ×)", c.name, len(c.resp), c.n, a, bound, float64(a)/float64(bound))
		if int64(a) > bound {
			t.Errorf("%s: a wire call reading a %d B answer allocates %d B, the route books %d B (FOLGE-R S-W2: no unbooked decode)", c.name, len(c.resp), a, bound)
		}
	}
}

// TestFolgeRCallOnAFreshConnectionIsBooked pins the call's own term
// (oai.RerankWireCallReserve less the answer's read shares) at its largest:
// a call whose answer is refused at its length — W02-10's hostile 1-MiB
// answers past the read cap — reads nothing and closes its connection, so
// every call after the first dials a fresh one (the transport's connection
// state and buffers). Such a call allocates at most the route's term
// without the read (rerank.AnswerReadMax's shares beside the body): the
// body's build, its copy, the decode's share and the call's own. RC-D S2
// FOLGE-R (mutant M27: the call's own term at 44 KiB, under the 56–59 KB a
// fresh connection takes, passed every other gate).
func TestFolgeRCallOnAFreshConnectionIsBooked(t *testing.T) {
	h := newWireHarness(t)
	for _, c := range wireHostileCases(t, h.query) {
		a := folgeRMost(t, h, c)
		l := rerank.RequestBodyLen("M", h.query, c.docs)
		read := int64(c.n)*2*rerank.AnswerEntryMax + 2*rerank.AnswerFixed + 2*rerank.AnswerReadSlack
		bound := wireCallBooking(l, c.n) - read
		t.Logf("FOLGE-R fresh connection: %-18s allocated=%d B bound=%d B (the call's term without the read's %d B)", c.name, a, bound, read)
		if int64(a) > bound {
			t.Errorf("%s: a call on a fresh connection whose answer is refused at its length allocates %d B, the call's own term books %d B", c.name, a, bound)
		}
	}
}
