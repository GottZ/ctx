package oai

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
)

// bodyLogForms are message texts for the log row: plain, every kind of
// invalid UTF-8, multi-byte runes across the row's cut, short and at the
// cut.
func bodyLogForms() map[string]string {
	cut := 64 << 10
	return map[string]string{
		"empty": "", "short": "hi", "ascii 1 MiB": strings.Repeat("a", 1<<20),
		"0xff between ASCII, 1 MiB": strings.Repeat("\xffa", 1<<19), "0xff run, 1 MiB": strings.Repeat("\xff", 1<<20),
		"0xff between ASCII, short": strings.Repeat("\xffa", 100), "0xff run then text": strings.Repeat("\xff", 1<<20) + strings.Repeat("b", 1<<17),
		"é across the cut": "x" + strings.Repeat("é", cut), "€ across the cut": strings.Repeat("€", cut/3+5),
		"4-byte across the cut": "ab" + strings.Repeat("\U0001F600", cut/4+3), "ascii at the cut": strings.Repeat("a", cut),
		"ascii one past the cut": strings.Repeat("a", cut+1), "ascii at the cut + 4": strings.Repeat("a", cut+4),
		"U+FFFD bytes and invalid": strings.Repeat("\xef\xbf\xbd\xff\xfe", cut/4), "lone continuation bytes": strings.Repeat("a\x80\x80", cut/2),
		"truncated lead at the end": strings.Repeat("a", cut-1) + "\xe2\x82", "truncated lead at the cut": strings.Repeat("a", cut-1) + "\xe2\x82" + "tail",
		"0xff between ASCII at the cut": strings.Repeat("a", cut-2) + strings.Repeat("\xffa", 10),
	}
}

func storedRequest(req string) string {
	row := llmlog.GatewayRow{Pipeline: chatPipeline, ConsumerID: "c", Request: req, BodyLogging: llmlog.BodyLoggingFull}
	row.Telemetry.RequiredSensitivity = string(backends.SensPublic)
	return llmlog.GatewayEntry(row).RequestUser
}

// TestBodyLogTextStoresWhatTheArrayStored (hardening 6, the class gate):
// the log row stores of bodyLogText(raw) exactly what it stored of the whole
// array, and neither the text nor the row's cleaning of it grows with the
// array — the copy of the whole array and the cleaning held 9 × it.
func TestBodyLogTextStoresWhatTheArrayStored(t *testing.T) {
	for name, s := range bodyLogForms() {
		raw := []byte(s)
		text := bodyLogText(raw, bodyLogSize(raw))
		if len(text) != bodyLogSize(raw) || len(text) > bodyLogTextMax+3 {
			t.Errorf("%s: text of %d bytes, sized %d, bound %d", name, len(text), bodyLogSize(raw), bodyLogTextMax+3)
		}
		if got, want := storedRequest(text), storedRequest(string(raw)); got != want {
			t.Errorf("%s: the row stores %d bytes %.40q…, want the whole array's %d bytes %.40q…", name, len(got), got, len(want), want)
		}
	}
	raw := []byte(strings.Repeat("\xffa", 4<<20))
	n := bodyLogSize(raw)
	if got := allocatedBy(func() { _ = bodyLogSize(raw) }); got > 0 {
		t.Errorf("bodyLogSize of %d bytes allocates %d bytes, want 0", len(raw), got)
	}
	// One allocation of the size it books (page-rounded past 32 KiB).
	if got := allocatedBy(func() { _ = bodyLogText(raw, n) }); got > uint64(n)+8<<10 {
		t.Errorf("bodyLogText of %d bytes allocates %d bytes, sized %d", len(raw), got, n)
	}
	text := bodyLogText(raw, n)
	if got := allocatedBy(func() { _ = storedRequest(text) }); got > 2*bodyLogTextMax {
		t.Errorf("the row of its text allocates %d bytes, want ≤ %d", got, 2*bodyLogTextMax)
	}
}
