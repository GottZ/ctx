package oai

// The answer writer of the rerank routes (W02-10) against encoding/json:
// the echo (writeJSONText) is the string json.Marshal writes without HTML
// escaping, the score (appendScore) the float64 json.Marshal writes; and the
// attempt timeout is capped by the rerank wire's own (rerank.Timeout).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/rerank"
)

// TestRerankEchoIsEncodingJSONs: writeJSONText(s) is json.Encoder's form
// of s with SetEscapeHTML(false) (its trailing newline cut) for every
// decoded text the parse can hand it — valid UTF-8 (invalid bytes became
// U+FFFD), quotes, backslashes, every control character, U+2028/U+2029,
// HTML characters, multi-byte runes.
func TestRerankEchoIsEncodingJSONs(t *testing.T) {
	var ctl strings.Builder
	for c := range 0x20 {
		ctl.WriteByte(byte(c))
	}
	forms := []string{"", "plain", `"q" \ b`, ctl.String(), "line sep end", "<b>&amp;</b>", "äöü ß 😀 �", strings.Repeat("x\"\\\n", 5000)}
	for _, s := range forms {
		var got bytes.Buffer
		bw := bufio.NewWriter(&got)
		writeJSONText(bw, s)
		_ = bw.Flush()
		var want bytes.Buffer
		enc := json.NewEncoder(&want)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(s); err != nil {
			t.Fatal(err)
		}
		if got.String() != strings.TrimSuffix(want.String(), "\n") {
			t.Errorf("%q: writeJSONText %q, encoding/json %q", s, got.String(), want.String())
		}
	}
}

// TestRerankScoreIsEncodingJSONs: appendScore(x) is json.Marshal(x) for
// relevance scores across [0, 1] — the 'e' form below 1e-6 included.
func TestRerankScoreIsEncodingJSONs(t *testing.T) {
	for _, logit := range []float64{-800, -40, -20, -13.9, -5, -0.5, 0, 0.75, 3, 16.118, 37, 40, 800} {
		x := sigmoid(logit)
		want, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(appendScore(nil, x)); got != string(want) {
			t.Errorf("logit %v → %v: appendScore %s, json.Marshal %s", logit, x, got, want)
		}
		if x < 0 || x > 1 || math.IsNaN(x) {
			t.Errorf("sigmoid(%v) = %v outside [0, 1]", logit, x)
		}
	}
}

// TestRerankAttemptTimeoutCappedByTheWire: a wire call's deadline is the
// shortest of the alias's timeout_s, the tenant ceiling, rerank.Timeout and
// what is left of the request deadline (design/02 §4.12: "Rerank
// zusätzlich min(·, 180 s)").
func TestRerankAttemptTimeoutCappedByTheWire(t *testing.T) {
	b := &backends.Backend{Timeouts: map[string]int{"rerank": 900}}
	c := &rerankCall{s: Settings{RequestTimeout: 900 * time.Second}, dl: time.Now().Add(time.Hour),
		alias: modelalias.Alias{Role: "rerank", Scope: backends.GlobalScope}}
	if got := c.attemptTimeout(b); got != rerank.Timeout {
		t.Errorf("row timeout 900 s: %v, want rerank.Timeout %v", got, rerank.Timeout)
	}
	c.alias.Policy.TimeoutS = modelalias.Opt[int]{V: 600, Set: true}
	if got := c.attemptTimeout(b); got != rerank.Timeout {
		t.Errorf("timeout_s 600: %v, want rerank.Timeout %v", got, rerank.Timeout)
	}
	c.alias.Policy.TimeoutS = modelalias.Opt[int]{V: 30, Set: true}
	if got := c.attemptTimeout(b); got != 30*time.Second {
		t.Errorf("timeout_s 30: %v, want 30 s", got)
	}
	c.dl = time.Now().Add(5 * time.Second)
	if got := c.attemptTimeout(b); got > 5*time.Second || got < 4*time.Second {
		t.Errorf("5 s left of the request deadline: %v", got)
	}
}

// TestRerankRanksByLogit (lead review 1 R-5): the order is the logits',
// finer than the scores where the sigmoid saturates — logits 40, 41 and 50
// are all 1.0 as a float64 score, ranked 50, 41, 40 — and equal logits keep
// the index order; the logits become the scores in place.
func TestRerankRanksByLogit(t *testing.T) {
	logits := []float64{40, 41, 50, 1.5, 1.5, -3}
	order := rankDocuments(logits, 0)
	if want := []int{2, 1, 0, 3, 4, 5}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	if logits[0] != 1 || logits[5] != sigmoid(-3) {
		t.Errorf("scores %v, want the sigmoid of every logit in place", logits)
	}
	if got := rankDocuments([]float64{0, 2, 1}, 2); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("top_n 2: %v, want [1 2]", got)
	}
}
