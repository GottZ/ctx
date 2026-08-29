package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillreset"
)

// TestIdentityFromResolvesScope hält die Reihenfolge fest, in der der Scope
// entsteht — Flag, dann distill.scope, dann der Home-Scope des Betreibers.
func TestIdentityFromResolvesScope(t *testing.T) {
	c := &config.Config{}
	c.Server.InstanceKind = "measure-copy"
	c.Distill.Category = "session-insights"
	c.Distill.BlockType = "insight"
	c.Distill.Scope = "aus-dem-schluessel"
	c.Scheduler.HomeScope = "private"

	if got := identityFrom(c, " aufruf "); got.Scope != "aufruf" {
		t.Errorf("Flag-Override = %q, want aufruf", got.Scope)
	}
	if got := identityFrom(c, ""); got.Scope != "aus-dem-schluessel" {
		t.Errorf("distill.scope = %q, want aus-dem-schluessel", got.Scope)
	}
	c.Distill.Scope = "   "
	got := identityFrom(c, "")
	if got.Scope != "private" {
		t.Errorf("Rückfall auf den Home-Scope = %q, want private", got.Scope)
	}
	if got.Category != "session-insights" || got.ToType != "insight" || got.InstanceKind != "measure-copy" {
		t.Errorf("Identität = %+v — Kategorie, Zieltyp und Etikett kommen aus der Konfiguration", got)
	}
}

// TestExitCodeFor hält die Kaskade fest — insbesondere die 5, die dieses
// Werkzeug mit dem Sweep-Treiber teilt: „auf eine Live-Instanz gezielt" ist
// etwas anderes als „ein Gate hat verweigert".
func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"sauber", nil, 0},
		{"keine Mess-Kopie", fmt.Errorf("x: %w", distillreset.ErrNotMeasureCopy), 5},
		{"kein Schatten-Typ", fmt.Errorf("x: %w", distillreset.ErrNotShadowType), 3},
		{"unvollständige Identität", fmt.Errorf("x: %w", distillreset.ErrIdentity), 3},
		{"alles andere", errors.New("db weg"), 1},
	}
	for _, tc := range cases {
		if got := exitCodeFor(tc.err); got != tc.want {
			t.Errorf("%s: exitCodeFor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestFromTypeIsRequired prüft den Aufruffehler VOR jeder DB-Berührung: der
// Test läuft ohne Datenbank und ohne Config-Umgebung, also beweist ein
// Rückgabewert 2 genau das.
func TestFromTypeIsRequired(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("run ohne -from-type = %d, want 2 (Aufruffehler)", code)
	}
	if !strings.Contains(errb.String(), "-from-type ist Pflicht") {
		t.Errorf("die Begründung fehlt: %q", errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("ein Aufruffehler hat nach stdout geschrieben: %q", out.String())
	}
}

// TestRenderNamesEveryRow — ein Rücksetzer, der nur eine Zahl meldet, ist auf
// einer Mess-Kopie nicht nachprüfbar.
func TestRenderNamesEveryRow(t *testing.T) {
	var out bytes.Buffer
	render(&out, &distillreset.Result{
		InstanceKind: "measure-copy", Category: "session-insights", Scope: "private",
		FromType: "session-insight-shadow", ToType: "insight", Applied: true,
		Rows:    []distillreset.Row{{ID: "id-1", Title: "Destillat aus Compaction a"}},
		Skipped: []distillreset.Row{{ID: "id-2", Title: "abgeleitet"}},
	})
	for _, want := range []string{
		"geschrieben", "measure-copy", "session-insights", "private",
		"session-insight-shadow → insight", "id-1", "Destillat aus Compaction a",
		"Übersprungen", "id-2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("der Bericht nennt %q nicht:\n%s", want, out.String())
		}
	}

	out.Reset()
	render(&out, &distillreset.Result{InstanceKind: "measure-copy"})
	if !strings.Contains(out.String(), "Trockenlauf") {
		t.Errorf("ein Lauf ohne -apply weist sich nicht als Trockenlauf aus:\n%s", out.String())
	}
}
