package oai_test

// Gate 3 of FOLGE-P (P-4) and the route pins of the reservation ledger
// (P-3/P-4/P-5 halves of gate 1): a client value reaches the engine with the
// bytes it sent — no HTML or JS escaping of <, >, &, U+2028, U+2029 — the
// upstream body is at most the request body plus the frame of the row, and
// every copy the route makes in proportion to the body (the read of an
// undeclared body, a rewritten message array, every attempt's upstream
// body) is covered by the request's reservation before it exists; past the
// budget the route answers 503 server_overloaded without the copy.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// htmlText is a text a json.Marshal re-encoding changes: <, >, & and the
// line and paragraph separators U+2028/U+2029 as raw UTF-8.
const htmlText = "a<b>c&d e f</s>"

// ledgerPost sends body to the rig's router (chunked: without a declared
// length) and returns the answer and what the request's reservation held at
// the end of the handler.
func ledgerPost(t *testing.T, rig *chatRig, body []byte, chunked bool) (*httptest.ResponseRecorder, int64) {
	t.Helper()
	var held heldSum
	router := memMount(rig, oai.ChatCompletions(rig.deps), &held)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if chunked {
		req.ContentLength = -1
		req.Body = io.NopCloser(struct{ io.Reader }{bytes.NewReader(body)})
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	sum, _ := held.sum()
	return w, sum
}

// rawsOf is everything u saw, as sent.
func rawsOf(u *upstream) [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.raws...)
}

// ledgerMark is in every body the ledger tests send: an engine counts only
// what carries it (a loopback port can be handed out again while a request of
// another test process on the host still dials it — one such stray hit was
// seen once in 200 runs under a host load of 20).
const ledgerMark = "FOLGEP-LEDGER-MARK"

// ownRaws is what u saw of the ledger tests' own requests.
func ownRaws(u *upstream) [][]byte {
	var own [][]byte
	for _, raw := range rawsOf(u) {
		if bytes.Contains(raw, []byte(ledgerMark)) {
			own = append(own, raw)
		}
	}
	return own
}

// upstreamFrame is what the row adds to a request's messages: the upstream
// body of the minimal request less its message array (the frame keys
// max_tokens, model, stream, the reasoning-off object and the row stages).
func upstreamFrame(t *testing.T) int {
	t.Helper()
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
		t.Fatalf("minimal request: %d %s", a.status, a.body)
	}
	return len(rawsOf(u)[0]) - len(`[{"role":"user","content":"hi"}]`)
}

// TestUpstreamCarriesTheSentBytes (gate 3): the text, a stop sequence and a
// rewritten developer message reach the engine with the bytes the client
// sent; the engine decodes the same text; the body stays within the request
// plus the frame — also for 1 MiB of "<", which json.Marshal turned into
// 6 MiB.
func TestUpstreamCarriesTheSentBytes(t *testing.T) {
	frame := upstreamFrame(t)
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"text and stop", `{"model":"chat","messages":[{"role":"user","content":"` + htmlText + `"}],"stop":["<x>","&"]}`,
			[]string{`"content":"` + htmlText + `"`, `"stop":["<x>","&"]`}},
		{"a rewritten developer message", `{"model":"chat","messages":[{"role":"developer","content":"` + htmlText + `","name":"<n>"}]}`,
			[]string{`{"content":"` + htmlText + `","name":"<n>","role":"system"}`}},
		{"a client escape stays an escape", `{"model":"chat","messages":[{"role":"user","content":"<\/"}]}`,
			[]string{`"content":"<\/"`}},
		{"1 MiB of <", `{"model":"chat","messages":[{"role":"user","content":"` + strings.Repeat("<", 1<<20) + `"}]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			if a := rig.post(t, tc.body); a.status != http.StatusOK {
				t.Fatalf("status %d %s", a.status, a.body)
			}
			raw := rawsOf(u)[0]
			for _, w := range tc.want {
				if !bytes.Contains(raw, []byte(w)) {
					t.Errorf("upstream body %.400s does not carry %s as sent", raw, w)
				}
			}
			var sent, got struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			if err := json.Unmarshal([]byte(tc.body), &sent); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &got); err != nil || len(got.Messages) != 1 || got.Messages[0].Content != sent.Messages[0].Content {
				t.Errorf("the engine decodes another text (%v)", err)
			}
			if len(raw) > len(tc.body)+frame {
				t.Errorf("upstream body %d bytes > request %d + frame %d", len(raw), len(tc.body), frame)
			}
		})
	}
}

// TestReservationCoversEveryCopy (P-3, P-4): what the request share held at
// the end — the reservation less the answer share — covers the body read,
// the rewritten message array and every attempt's upstream body.
func TestReservationCoversEveryCopy(t *testing.T) {
	const size = 256 << 10
	text := []byte(`{"model":"chat","messages":[{"role":"user","content":"` + ledgerMark + strings.Repeat("a", size) + `"}]}`)
	dev := []byte(`{"model":"chat","messages":[` + strings.Repeat(`{"role":"developer","content":"x"},`, size/36) + `{"role":"user","content":"` + ledgerMark + `"}]}`)
	answerShare := 2 * chatSettings().MaxResponseBytes
	for _, tc := range []struct {
		name       string
		body       []byte
		chunked    bool
		failing    int            // rows answering 404 first
		extra      map[string]any // the serving row's extra_body
		readFactor int64          // body reads the ledger books at least (an undeclared body: chunks + join)
		rewrite    bool
	}{
		{name: "declared text", body: text, readFactor: 1},
		{name: "undeclared text", body: text, chunked: true, readFactor: 2},
		{name: "three attempts", body: text, failing: 2, readFactor: 1},
		{name: "undeclared, rewritten developer messages", body: dev, chunked: true, readFactor: 2, rewrite: true},
		{name: "a small body and a large row frame", body: []byte(chatBody("chat", `"stop":["`+ledgerMark+`"]`)), readFactor: 1,
			extra: map[string]any{"vendor_blob": strings.Repeat("z", 64<<10)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ups []*upstream
			var rows []backends.Backend
			for i := 0; i <= tc.failing; i++ {
				u := newUpstream(t, answerSGLang)
				if i < tc.failing {
					u.setRespond(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
				}
				b := u.row("row"+string(rune('a'+i)), backends.TrustFull, 100-i) // the chain walks higher priorities first
				if i == tc.failing {
					b.ExtraBody = tc.extra
				}
				ups, rows = append(ups, u), append(rows, b)
			}
			rig := newChatRig(t, rows, 4, 1)
			w, held := ledgerPost(t, rig, tc.body, tc.chunked)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d %s", w.Code, w.Body.Bytes())
			}
			want := tc.readFactor * int64(len(tc.body))
			for _, u := range ups {
				for _, raw := range ownRaws(u) {
					want += int64(len(raw))
					if tc.rewrite {
						var m map[string]json.RawMessage
						_ = json.Unmarshal(raw, &m)
						want += int64(len(m["messages"])) // the rewritten array the upstream body copied
					}
				}
			}
			if got := held - answerShare; got < want {
				t.Errorf("the request share held %d bytes at the end, want ≥ %d (reads + copies)", got, want)
			}
		})
	}
}

// TestUpstreamCopyPastTheBudgetIs503 (P-4, P-5): a budget that holds the
// door's share and the answer share but not an attempt's upstream body is the
// stage's 503 server_overloaded with Retry-After 1, before the body exists:
// on the first attempt no wire call, no row, the limiter's reservation
// aborted; on a failover attempt after the first row's wire call the walk
// ends there.
func TestUpstreamCopyPastTheBudgetIs503(t *testing.T) {
	small := []byte(chatBody("chat", `"stop":["`+ledgerMark+`"]`))
	big := []byte(`{"model":"chat","messages":[{"role":"user","content":"` + ledgerMark + strings.Repeat("a", 1<<20) + `"}]}`)
	for _, tc := range []struct {
		name    string
		body    []byte
		failing int
		extra   map[string]any
		hits    []int64 // wire hits per row
		rows    int     // llmlog rows
	}{
		{name: "first attempt, a large row frame", body: small, extra: map[string]any{"vendor_blob": strings.Repeat("z", 64<<10)},
			hits: []int64{0}, rows: 0},
		{name: "second attempt", body: big, failing: 1, hits: []int64{1, 0}, rows: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ups []*upstream
			var rows []backends.Backend
			for i := 0; i <= tc.failing; i++ {
				u := newUpstream(t, answerSGLang)
				if i < tc.failing {
					u.setRespond(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
				}
				b := u.row("row"+string(rune('a'+i)), backends.TrustFull, 100-i) // the chain walks higher priorities first
				if i == tc.failing {
					b.ExtraBody = tc.extra
				}
				ups, rows = append(ups, u), append(rows, b)
			}
			rig := newChatRig(t, rows, 4, 1)
			rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 3*int64(len(tc.body)) + 2*s.MaxResponseBytes })
			w, _ := ledgerPost(t, rig, tc.body, false)
			var e struct {
				Error struct{ Code string } `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != http.StatusServiceUnavailable || e.Error.Code != oai.CodeServerOverloaded || w.Header().Get("Retry-After") != "1" {
				t.Fatalf("answer %d %s (Retry-After %q), want 503 server_overloaded, Retry-After 1", w.Code, w.Body.Bytes(), w.Header().Get("Retry-After"))
			}
			for i, u := range ups {
				if got := int64(len(ownRaws(u))); got != tc.hits[i] {
					t.Errorf("row %d: %d wire hits (%d in all), want %d", i, got, u.hits.Load(), tc.hits[i])
				}
			}
			if got := len(rig.rows.all()); got != tc.rows {
				t.Errorf("%d llmlog rows, want %d", got, tc.rows)
			}
			if tc.rows == 0 && (rig.limiter.aborts.Load() != 1 || rig.limiter.settles.Load() != 0) {
				t.Errorf("limiter: %d aborts, %d settles, want 1 and 0", rig.limiter.aborts.Load(), rig.limiter.settles.Load())
			}
			if rig.budget.InUse() != 0 {
				t.Errorf("the budget holds %d bytes after the request", rig.budget.InUse())
			}
		})
	}
}

// TestJSONStateIsCoveredOverTheRoute: a small body nested past the decoder's
// limit is 400 invalid_json, and its reservation held the body read plus the
// JSON check's state (24 bytes per level) — more than the body's share of
// three times its length.
func TestJSONStateIsCoveredOverTheRoute(t *testing.T) {
	body := []byte(`{"model":"chat","messages":[{"role":"user","content":"x"}],"response_format":` + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + `}`)
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	w, held := ledgerPost(t, rig, body, false)
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(oai.CodeInvalidJSON)) {
		t.Fatalf("answer %d %s, want 400 invalid_json", w.Code, w.Body.Bytes())
	}
	if want := int64(len(body)) + 10001*24; held < want || held <= 3*int64(len(body)) {
		t.Errorf("the reservation held %d bytes, want ≥ %d (the body + the check's nesting state) and > 3 × body", held, want)
	}
}
