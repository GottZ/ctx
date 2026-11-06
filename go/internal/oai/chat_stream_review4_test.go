package oai_test

// Delta-Review 4 of W02-08 (hardening 3). F4-1: the estimate of a stream cut
// short never books more than its reservation holds — the completion
// estimate stops at the max_tokens that went upstream, and an estimate at or
// above what Keep books ends the reservation with Keep. Every case runs over
// the rig's real planner, dispatcher and meter (endMeter, frozen clock).

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
)

// promptEstimate is the prompt estimate of body: TokenEstimate over the runes
// of its messages, as the meter reserves and the route estimates them.
func promptEstimate(body string) int64 {
	return int64(promptguard.TokenEstimate(len([]rune(messagesOf([]byte(body))))))
}

// statusless is an HTTP/1.1 backend that reads the request and then holds
// without a status line until the connection or the test ends: wire contact,
// never a 200. read gets one value per request it read.
func statusless(t *testing.T) (*rawBackend, <-chan struct{}) {
	t.Helper()
	read := make(chan struct{}, 4)
	b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
		if !b.read(br) {
			return
		}
		read <- struct{}{}
		gone := make(chan struct{})
		go func() { _, _ = c.Read(make([]byte, 1)); close(gone) }()
		select {
		case <-gone:
		case <-b.done:
		}
	})
	return b, read
}

// cutThenNext runs one stream of body over a rig whose meter has lim, cut by
// its client either before the backend's status (beforeStatus: the backend
// read the request and holds — Keep) or after the first frame of a backend
// that then holds (the estimate); then the same consumer's next non-streamed
// request. It returns how the stream's reservation ended and that request's
// status and retry-after-ms.
func cutThenNext(t *testing.T, lim keyquota.Limits, body string, beforeStatus bool, opts ...rigOption) (ends string, status int, retryMs string) {
	t.Helper()
	m := &endMeter{rigMeter: newRigMeter(lim, true)}
	hold := make(chan struct{})
	defer close(hold)
	var rows []backends.Backend
	var read <-chan struct{}
	if beforeStatus {
		var b *rawBackend
		b, read = statusless(t)
		rows = []backends.Backend{wireRow("hold", b.host(), 10)}
	} else {
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("a cat", "")}, {hold: hold}}, nil, nil))
		rows = []backends.Backend{u.row("spark", backends.TrustFull, 10)}
	}
	all := append([]rigOption{withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m })}, opts...)
	rig := newChatRig(t, rows, 4, 1, all...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ls, each := openAndRead(t, ctx, serveHTTP(t, rig.router), body)
	if beforeStatus {
		select {
		case <-read:
		case <-time.After(5 * time.Second):
			t.Fatal("the backend never read the request")
		}
	} else if _, ok := nextData(each, 5*time.Second); !ok {
		t.Fatal("no frame")
	}
	cancel()
	_ = ls.Close()
	if !waitEnded(m) {
		t.Fatal("the stream's reservation never ended")
	}
	a := rig.post(t, chatBody("chat"))
	return m.endsOf(), a.status, a.header.Get("Retry-After-Ms")
}

// TestStreamMeterCutEstimateNeverAboveKeep is F4-1 (Delta-Review 4): a stream
// without a usage chunk that ends after the backend's 200 books its estimate
// only below what the reservation holds.
//
//   - max_tokens 8, one frame of 49 runes, the client gone: the completion
//     estimate is min(TokenEstimate(49) = 28, 8) = 8, the estimate 18 + 8 = 26
//     is the reservation 18 + 8 — not below it: Keep, 26 in the bucket, and
//     the row books 18 + 8 (red before: Settle 46).
//   - max_tokens 8, the backend's timings.prompt_n 1, one frame of 30 runes:
//     the estimate 1 + min(17, 8) = 9 is below the reservation 26: Settle 9
//     (red before: Settle 1 + 17 = 18).
//   - a prompt above the bucket (TPM 2 000, prompt estimate 4 462 > the
//     capacity 2 000; B-06-28 charges the capacity) and a vision stream under
//     the default chat TPM 60 000 (a data: image of 300 000 characters,
//     prompt estimate 166 741): the stream cut after its first frame leaves
//     the bucket where a stream cut before the backend's status (Keep) leaves
//     it — the next request waits 31 260 ms resp. 1 042 ms (red before:
//     105 210 ms and 107 786 ms).
func TestStreamMeterCutEstimateNeverAboveKeep(t *testing.T) {
	t.Run("max_tokens 8, a frame of 49 runes, client gone", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		body := streamBody("chat", `"max_tokens":8`)
		text := "The quick brown fox jumps over the lazy dog again"
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk(text, "")}, {hold: hold}}, nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		ctx, cancel := context.WithCancel(context.Background())
		ls, each := openAndRead(t, ctx, serveHTTP(t, rig.router), body)
		if _, ok := nextData(each, 5*time.Second); !ok {
			cancel()
			t.Fatal("no frame")
		}
		cancel()
		_ = ls.Close()
		if !waitEnded(m) || !waitFor(2*time.Second, func() bool { return len(rig.rows.all()) == 1 }) {
			t.Fatal("no end or no row")
		}
		prompt, held, r := promptEstimate(body), m.held(t, rig), rig.rows.all()[0]
		t.Logf("max_tokens 8, %d runes streamed, client gone: ends %s; the bucket holds %d tokens (reservation %d); row %q %d/%d",
			len([]rune(text)), m.endsOf(), held, prompt+8, r.Gateway.Usage, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens)
		if r.Gateway.Usage != llmlog.UsageEstimated || int64(r.Telemetry.PromptTokens) != prompt || r.Telemetry.CompletionTokens != 8 {
			t.Errorf("row %q %d/%d, want the estimate %d/8 (the completion estimate stops at max_tokens 8)",
				r.Gateway.Usage, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens, prompt)
		}
		if m.endsOf() != "keep" || held != prompt+8 {
			t.Errorf("ends %s, the bucket holds %d tokens — want Keep of the reservation %d (the estimate %d is not below it)",
				m.endsOf(), held, prompt+8, prompt+8)
		}
	})
	t.Run("max_tokens 8, prompt_n 1, a frame of 30 runes, client gone", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		body := streamBody("chat", `"max_tokens":8`)
		text := "abcdefghij abcdefghij abcdefgh"
		frame := strings.TrimSuffix(llamaChunk(text, ""), "}") + `,"timings":{"prompt_n":1}}`
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: frame}, {hold: hold}}, nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		ctx, cancel := context.WithCancel(context.Background())
		ls, each := openAndRead(t, ctx, serveHTTP(t, rig.router), body)
		if _, ok := nextData(each, 5*time.Second); !ok {
			cancel()
			t.Fatal("no frame")
		}
		cancel()
		_ = ls.Close()
		if !waitEnded(m) || !waitFor(2*time.Second, func() bool { return len(rig.rows.all()) == 1 }) {
			t.Fatal("no end or no row")
		}
		held, r := m.held(t, rig), rig.rows.all()[0]
		t.Logf("max_tokens 8, prompt_n 1, %d runes streamed (TokenEstimate %d), client gone: ends %s; the bucket holds %d tokens (reservation %d); row %q %d/%d",
			len([]rune(text)), promptguard.TokenEstimate(len([]rune(text))), m.endsOf(), held, promptEstimate(body)+8, r.Gateway.Usage,
			r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens)
		// The estimate 1 + min(17, 8) = 9 is below the reservation 18 + 8 =
		// 26: Settle 9 (without the max_tokens bound: Settle 1 + 17 = 18).
		if m.endsOf() != "settle" || held != 9 || r.Telemetry.PromptTokens != 1 || r.Telemetry.CompletionTokens != 8 {
			t.Errorf("ends %s, the bucket holds %d tokens, row %d/%d — want Settle of the estimate 1 + 8 = 9",
				m.endsOf(), held, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens)
		}
	})
	big := strings.Replace(streamBody("chat"), `"content":"hi"`, `"content":"`+strings.Repeat("abcdefgh", 1000)+`"`, 1)
	image := `{"model":"visbig","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 300000) + `"}}]}]}`
	visbig := func(r *chatRig) {
		r.reg.reload(t, append(chatAliases(), chatAlias{"00000000-0000-4000-8000-0000000c4a7b", backends.GlobalScope, "visbig", "chat", "chat",
			backends.SensCredentials, `{"vision":true,"vision_max_bytes":4000000}`, true}))
	}
	for _, tc := range []struct {
		name   string
		lim    keyquota.Limits
		body   string
		opts   []rigOption
		prompt int64
		wait   string
	}{
		{"a prompt above the bucket, TPM 2 000", keyquota.Limits{RPM: 1000, TPM: 2000, CompletionReserve: 1024}, big, nil, 4462, "31260"},
		{"vision under the default chat TPM 60 000", keyquota.Limits{RPM: 30, TPM: 60000, CompletionReserve: 1024}, image, []rigOption{visbig}, 166741, "1042"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if p := promptEstimate(tc.body); p != tc.prompt {
				t.Fatalf("prompt estimate %d, want %d", p, tc.prompt)
			}
			keepEnds, keepStatus, keepWait := cutThenNext(t, tc.lim, tc.body, true, tc.opts...)
			cutEnds, cutStatus, cutWait := cutThenNext(t, tc.lim, tc.body, false, tc.opts...)
			t.Logf("%s: cut before the status: ends %s, next request %d retry-after-ms %s; cut after the first frame: ends %s, next request %d retry-after-ms %s",
				tc.name, keepEnds, keepStatus, keepWait, cutEnds, cutStatus, cutWait)
			if keepEnds != "keep" || keepStatus != http.StatusTooManyRequests || keepWait != tc.wait {
				t.Fatalf("cut before the status: ends %s, next %d after %s ms — want Keep and a 429 after %s ms", keepEnds, keepStatus, keepWait, tc.wait)
			}
			if cutEnds != "keep" || cutStatus != http.StatusTooManyRequests || cutWait != tc.wait {
				t.Errorf("cut after the first frame: ends %s, next %d after %s ms — want Keep and the wait of a kept reservation, %s ms",
					cutEnds, cutStatus, cutWait, tc.wait)
			}
		})
	}
}
