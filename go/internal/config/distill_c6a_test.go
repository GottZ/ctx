package config

import "testing"

// Welle C6-A — distill.concurrency: der eine Schluessel des Quellen-Fan-outs.
// Zwei Aussagen, jede an einem eigenen Fehlerfall: der Schluessel erreicht die
// Settings-Oberflaeche mit dem Default 1 (dem heutigen, seriellen Arm), und V34
// weist beide Enden ab, die als Zahl rendern und als etwas anderes wirken.

// TestDistillConcurrencyReachesSettings liest den Default aus der
// SETTINGS-OBERFLAECHE (KeyByName), nicht aus der Struktur: was ein Operator
// sieht, ist die Antwort und nicht die Dokumentation.
func TestDistillConcurrencyReachesSettings(t *testing.T) {
	ki, ok := KeyByName("distill.concurrency")
	if !ok {
		t.Fatal("distill.concurrency ist nicht in der Registry — der Schluessel erreicht " +
			"GET /api/settings nie")
	}
	if ki.Type != "int" {
		t.Errorf("Typ = %q, want int", ki.Type)
	}
	if ki.Default != 1 {
		t.Errorf("Default = %#v, want 1 — der Default MUSS der serielle Arm sein", ki.Default)
	}
	if ki.Mutability != "hot" {
		t.Errorf("Mutability = %q, want hot — der Arm loest den Wert je Tick neu auf", ki.Mutability)
	}
	if ki.Tenancy != "global-only" {
		t.Errorf("Tenancy = %q, want global-only — die ganze distill.*-Gruppe ist global", ki.Tenancy)
	}
	if ki.Desc == "" {
		t.Error("keine Operator-Beschreibung")
	}
	c := validCfg(t, map[string]string{})
	if c.Distill.Concurrency != 1 {
		t.Errorf("aufgeloester Default = %d, want 1", c.Distill.Concurrency)
	}
}

// TestDistillConcurrencyRange ist V34. Beide Enden des Intervalls bleiben legal;
// abgewiesen wird, was daneben liegt — die 0 als stiller zweiter Aus-Schalter
// neben distill.enabled, und jeder Wert oberhalb der Schranke, hinter der ein
// Hintergrund-Arm den 20er-Pool des Daemons fuer sich allein belegen wuerde.
func TestDistillConcurrencyRange(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  Severity
		note  string
	}{
		{"1", -1, "der Default validiert sauber"},
		{"4", -1, "ein gewoehnlicher Wert"},
		{"10", -1, "der Wert, den Live setzt"},
		{"16", -1, "die Schranke selbst bleibt legal"},
		{"0", SeverityError, "0 waere ein Tick, der keine einzige Quelle anfasst"},
		{"-1", SeverityError, "negativ rendert als Zahl und wirkt als Aus-Schalter"},
		{"17", SeverityError, "ueber der Schranke stauen sich die uebrigen Arme hinter dem Distiller"},
		{"64", SeverityError, "dito, deutlicher"},
	} {
		issues := Validate(validCfg(t, map[string]string{"distill.concurrency": tc.value}))
		if got := severityFor(issues, "distill.concurrency"); got != tc.want {
			t.Errorf("distill.concurrency %s: Severity = %v, want %v (%s): %v",
				tc.value, got, tc.want, tc.note, issuesOn(issues, "distill.concurrency"))
		}
	}
}
