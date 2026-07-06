package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// attrHandler records message + rendered attrs per line — the coverage WARN
// assertions need origin/roles, which recordingHandler drops.
type attrHandler struct {
	mu    sync.Mutex
	lines []string
}

func (h *attrHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *attrHandler) Handle(_ context.Context, r slog.Record) error {
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += fmt.Sprintf(" %s=%v", a.Key, a.Value)
		return true
	})
	h.mu.Lock()
	h.lines = append(h.lines, line)
	h.mu.Unlock()
	return nil
}
func (h *attrHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *attrHandler) WithGroup(string) slog.Handler      { return h }

func (h *attrHandler) matching(sub string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, l := range h.lines {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

const coverageWarn = "background-reachable local target without slots cap"

// The live-shaped fixture (inventory/02 §11): a local GPU chat target
// carrying background roles, a local embed target, plus optionally the
// deliberately uncapped external openrouter row.
func enforcingRows(capChat, capEmbed, withExternal bool) []BackendRow {
	chat := BackendRow{Name: "herbert-chat", Scope: GlobalScope,
		BaseURL: "http://gpu:8089/v1",
		Roles:   []string{"synthesis", "translate", "chat", "digest", "dream", "classify"}}
	embed := BackendRow{Name: "llama-embed", Scope: GlobalScope,
		BaseURL: "http://llama-embed:8081",
		Roles:   []string{"embed", "dream-embed"}}
	if capChat {
		chat.Limits = map[string]any{"slots": float64(1)}
	}
	if capEmbed {
		embed.Limits = map[string]any{"slots": float64(4)}
	}
	rows := []BackendRow{chat, embed}
	if withExternal {
		rows = append(rows, BackendRow{Name: "openrouter", Scope: GlobalScope,
			BaseURL:  "https://openrouter.ai/api",
			Roles:    []string{"synthesis", "translate", "chat", "digest", "dream"},
			External: true})
	}
	return rows
}

func newEnforcingDispatcher(t *testing.T, s Settings, rows []BackendRow) (*Dispatcher, *attrHandler) {
	t.Helper()
	h := &attrHandler{}
	logger := slog.New(h)
	d := New(logger, s)
	t.Cleanup(d.Close)
	d.UpdatePolicy(DerivePolicy(rows, logger))
	return d, h
}

// A5-W1 gate: enabled=false ⇒ false, even under a fully covering policy —
// the emergency stop always degrades the arms to the legacy fallback.
func TestEnforcingDisabled(t *testing.T) {
	s := DefaultSettings()
	s.Enabled = false
	d, _ := newEnforcingDispatcher(t, s, enforcingRows(true, true, true))
	if d.Enforcing() {
		t.Fatalf("Enforcing() = true with dispatch disabled")
	}
}

// A5-W1 gate: empty policy ⇒ false — both the constructor default and a
// derivation over rows without dispatch keys (pass-through everywhere).
func TestEnforcingEmptyPolicy(t *testing.T) {
	d, _ := newEnforcingDispatcher(t, DefaultSettings(), enforcingRows(false, false, false))
	if d.Enforcing() {
		t.Fatalf("Enforcing() = true on an empty derived policy")
	}
	fresh := New(slog.Default(), DefaultSettings())
	defer fresh.Close()
	if fresh.Enforcing() {
		t.Fatalf("Enforcing() = true on the constructor default policy")
	}
}

// A5-W1 gate: full coverage of the local background-reachable targets ⇒
// true, and NO coverage WARN on the reload.
func TestEnforcingFullCoverage(t *testing.T) {
	d, h := newEnforcingDispatcher(t, DefaultSettings(), enforcingRows(true, true, false))
	if !d.Enforcing() {
		t.Fatalf("Enforcing() = false under full local coverage")
	}
	if warns := h.matching(coverageWarn); len(warns) != 0 {
		t.Fatalf("unexpected coverage WARN under full coverage: %v", warns)
	}
}

// A5-W1 gate (the B-R2 hole): a partial policy — slots only on the chat
// target, embed target uncapped — must degrade Enforcing to false AND emit
// exactly ONE WARN per reload naming the embed origin and its roles.
func TestEnforcingPartialPolicy(t *testing.T) {
	d, h := newEnforcingDispatcher(t, DefaultSettings(), enforcingRows(true, false, false))
	if d.Enforcing() {
		t.Fatalf("Enforcing() = true under a partial policy (B-R2 fail-open)")
	}
	warns := h.matching(coverageWarn)
	if len(warns) != 1 {
		t.Fatalf("want exactly 1 coverage WARN per reload, got %d: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], "http://llama-embed:8081") {
		t.Fatalf("coverage WARN misses the embed origin: %s", warns[0])
	}
	if !strings.Contains(warns[0], "dream-embed") || !strings.Contains(warns[0], "embed") {
		t.Fatalf("coverage WARN misses the background roles: %s", warns[0])
	}
}

// A5-W1 gate: a deliberately uncapped EXTERNAL target (openrouter) is exempt
// from the coverage term — Enforcing stays true, no WARN for it.
func TestEnforcingExternalExempt(t *testing.T) {
	d, h := newEnforcingDispatcher(t, DefaultSettings(), enforcingRows(true, true, true))
	if !d.Enforcing() {
		t.Fatalf("uncapped external target forced Enforcing off (exemption broken)")
	}
	if warns := h.matching("openrouter.ai"); len(warns) != 0 {
		t.Fatalf("coverage WARN for an exempt external target: %v", warns)
	}
}

// A5-W1 gate: hot flip via reload within a tick — partial ⇒ false, full
// policy swap ⇒ true immediately, emergency-stop flip ⇒ false, re-enable ⇒
// true (no restart, no consumer involvement).
func TestEnforcingHotFlip(t *testing.T) {
	d, h := newEnforcingDispatcher(t, DefaultSettings(), enforcingRows(true, false, false))
	if d.Enforcing() {
		t.Fatalf("precondition: partial policy must not enforce")
	}
	logger := slog.New(h)
	d.UpdatePolicy(DerivePolicy(enforcingRows(true, true, false), logger))
	if !d.Enforcing() {
		t.Fatalf("full-coverage reload did not flip Enforcing to true")
	}
	s := DefaultSettings()
	s.Enabled = false
	d.UpdateSettings(s)
	if d.Enforcing() {
		t.Fatalf("enabled=false flip did not turn Enforcing off")
	}
	s.Enabled = true
	d.UpdateSettings(s)
	if !d.Enforcing() {
		t.Fatalf("re-enable did not restore Enforcing")
	}
}
