//go:build integration

// The latency the recording of issue #571 adds to a request (opt-in probe,
// CTX_RECORD_PROBE=1): the production router over a throwaway database,
// a small chat request and a large embeddings request (32 inputs, 1.4 MiB, below the
// scan cap), each sent alternately by the same consumer with the recording
// switch off and on — the switch flipped in the consumer row between the
// two (the door reads it on every request) — and the end-to-end time of the
// router's ServeHTTP per request. p50 and p99 per form and switch.
//
//	CTX_RECORD_PROBE=1 go test -tags=integration -p 1 -count=1 -run TestV1RecordingLatency -v ./cmd/ctxd/
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

func v1RecPercentiles(d []time.Duration) (p50, p99 time.Duration) {
	s := slices.Clone(d)
	slices.Sort(s)
	at := func(q float64) time.Duration { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return at(0.50), at(0.99)
}

func TestV1RecordingLatency(t *testing.T) {
	if os.Getenv("CTX_RECORD_PROBE") != "1" {
		t.Skip("opt-in probe (CTX_RECORD_PROBE=1)")
	}
	const n = 200
	ctx := context.Background()
	measure := func(t *testing.T, name string, flip func(on bool), send func() (int, []byte)) {
		t.Helper()
		var off, on []time.Duration
		for i := 0; i < 10; i++ { // warm both paths
			flip(i%2 == 1)
			if st, b := send(); st != http.StatusOK {
				t.Fatalf("%s warm-up: status %d: %.300s", name, st, b)
			}
		}
		for i := 0; i < 2*n; i++ {
			rec := i%2 == 1
			flip(rec)
			start := time.Now()
			if st, _ := send(); st != http.StatusOK {
				t.Fatalf("%s request %d (recording %v): status %d", name, i, rec, st)
			}
			d := time.Since(start)
			if rec {
				on = append(on, d)
			} else {
				off = append(off, d)
			}
		}
		p50off, p99off := v1RecPercentiles(off)
		p50on, p99on := v1RecPercentiles(on)
		t.Logf("%s, %d requests each: off p50 %v p99 %v — on p50 %v p99 %v — Δp50 %v Δp99 %v", name, n,
			p50off, p99off, p50on, p99on, p50on-p50off, p99on-p99off)
	}
	t.Run("chat_small", func(t *testing.T) {
		e := newV1ChatEngine(t, http.StatusOK)
		f := newV1ChatFixture(t, []backends.Backend{e.row("eng", 10)})
		body := `{"model":"` + v1ChatAlias + `","messages":[{"role":"user","content":"a short question of about a hundred bytes, as a chat client sends it"}]}`
		flip := func(on bool) {
			if _, err := f.seed.Exec(ctx, `UPDATE context_model_consumers SET record_bodies = $2 WHERE id = $1::uuid`, f.urgentID, on); err != nil {
				t.Fatal(err)
			}
		}
		measure(t, fmt.Sprintf("chat, %d-byte body", len(body)), flip, func() (int, []byte) { a := f.post(t, f.urgentKey, body); return a.status, a.body })
	})
	t.Run("embeddings_large", func(t *testing.T) {
		eng := newV1EmbedEngine(t)
		f := newV1EmbedFixture(t, eng)
		inputs := make([]string, 32)
		for i := range inputs {
			inputs[i] = `"` + strings.Repeat(fmt.Sprintf("chunk %02d of a corpus document ", i), 1500) + `"`
		}
		body := `{"model":"` + v1EmbedAlias + `","input":[` + strings.Join(inputs, ",") + `]}`
		flip := func(on bool) {
			if _, err := f.seed.Exec(ctx, `UPDATE context_model_consumers SET record_bodies = $2 WHERE id = $1::uuid`, f.urgentID, on); err != nil {
				t.Fatal(err)
			}
		}
		measure(t, fmt.Sprintf("embeddings, %d-byte body", len(body)), flip, func() (int, []byte) { a := f.post(t, f.urgentKey, body); return a.status, a.body })
	})
}
