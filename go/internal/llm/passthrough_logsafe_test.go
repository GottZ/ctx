package llm

// Pin of Fremd-Review 2 F2-4 P2 (Härtung W02-07, design/02 B20): the WARN
// line of a failed gateway attempt names the upstream status, the body's
// length and its sha256 prefix — and not one byte of the body, no prefix
// either (TestOAIErrorNeverPersistsUpstreamBody in internal/oai checks a
// canary deep inside the body; a prefix next to the hash passed it).

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
)

type pinLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *pinLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *pinLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var pinErrorAttr = regexp.MustCompile(`error=("(?:[^"\\]|\\.)*"|\S+)`)

func TestWalkAttemptWarnCarriesNoUpstreamBodyByte(t *testing.T) {
	// Every byte of the body lies outside the alphabet of the safe form
	// ("upstream status N (body N bytes, sha256 HEX)"): any byte of it in
	// the logged error is a body byte.
	const body = `{"QZJXKVWGLNI":{"WIZ":["!~#|<>QZJX"]}}`
	const safeAlphabet = "upstream status (body bytes, sha256 )0123456789abcdef"
	if strings.ContainsAny(body, safeAlphabet) {
		t.Fatalf("fixture %q shares bytes with the safe form", body)
	}
	logs := &pinLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	se := &httpx.StatusError{Code: 502, Body: body}
	acq := newPTAcquirer(t)
	_, served, _, err := ptRun(t, acq, []backends.Backend{ptChainRow("a", 0)}, ptRules, map[string]ptStep{"a": {err: fmt.Errorf("llm: %w", se)}})
	if served != nil || err == nil {
		t.Fatalf("served %v err %v, want the 502", served, err)
	}
	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, `msg="llm: gateway attempt failed"`) {
			line = l
		}
	}
	m := pinErrorAttr.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no WARN line of the failed attempt with an error attribute:\n%s", logs.String())
	}
	val := m[1]
	if strings.HasPrefix(val, `"`) {
		if val, err = strconv.Unquote(val); err != nil {
			t.Fatalf("error attribute %s: %v", m[1], err)
		}
	}
	sum := sha256.Sum256([]byte(body))
	for _, want := range []string{"502", fmt.Sprintf("body %d bytes", len(body)), fmt.Sprintf("sha256 %x", sum[:8])} {
		if !strings.Contains(val, want) {
			t.Errorf("error attribute %q lacks %q", val, want)
		}
	}
	if i := strings.IndexAny(val, body); i >= 0 {
		t.Errorf("error attribute %q carries body byte %q at %d", val, val[i], i)
	}
	if strings.Contains(line, "QZJX") || strings.Contains(line, "WIZ") {
		t.Errorf("the WARN line carries body text: %s", line)
	}
}
