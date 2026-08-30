package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/derived"
)

// Welle C5-E — distill.novelty_floor: der eine Schluessel des
// Per-Claim-Substanz-Floors. Drei Aussagen, jede an einem eigenen Fehlerfall:
// der Schluessel erreicht die Settings-Oberflaeche mit dem richtigen Default,
// der Default IST die Grenze des Reports (keine zweite Politik), und V33 weist
// die beiden Werte ab, die als Schwelle rendern und als etwas anderes wirken.

// TestDistillNoveltyFloorReachesSettings: der Schluessel steht in der Registry,
// traegt eine Operator-Beschreibung, ist hot und startet auf 0,15.
//
// Der Default wird aus der SETTINGS-OBERFLAECHE gelesen (KeyByName), nicht aus
// der Struktur: was ein Operator sieht, ist die Antwort und nicht die
// Dokumentation.
func TestDistillNoveltyFloorReachesSettings(t *testing.T) {
	ki, ok := KeyByName("distill.novelty_floor")
	if !ok {
		t.Fatal("distill.novelty_floor ist nicht in der Registry — der Schluessel erreicht " +
			"GET /api/settings nie")
	}
	if ki.Type != "float" {
		t.Errorf("Typ = %q, want float", ki.Type)
	}
	if ki.Default != 0.15 {
		t.Errorf("Default = %#v, want 0.15", ki.Default)
	}
	if ki.Mutability != "hot" {
		t.Errorf("Mutability = %q, want hot — der Arm loest den Wert je Tick neu auf", ki.Mutability)
	}
	if ki.Desc == "" {
		t.Error("keine Operator-Beschreibung")
	}
}

// TestDistillNoveltyFloorMatchesReportFloor ist die Kopplung, die ein Struct-Tag
// nicht ausdruecken kann: der Default des Tores und die Grenze, an der
// derived.Report seinen below_floor_share zaehlt, sind DIESELBE Zahl.
//
// Driften sie auseinander, misst der Schreibpfad eine andere Grenze als das
// Instrument, mit dem die Mess-Wellen ihn bewerten — und jede Aussage der Form
// "so viel haette der Floor verworfen" waere ab da ein Vergleich zweier
// verschiedener Groessen. Die Sonde ist deshalb kein Stil-Check: sie haelt die
// Vergleichbarkeit der Mess-Reihe.
func TestDistillNoveltyFloorMatchesReportFloor(t *testing.T) {
	ki, ok := KeyByName("distill.novelty_floor")
	if !ok {
		t.Fatal("distill.novelty_floor fehlt in der Registry")
	}
	if ki.Default != derived.GoodhartMinNovelty {
		t.Errorf("Default %v != derived.GoodhartMinNovelty %v — Tor und Report messen zwei "+
			"verschiedene Grenzen", ki.Default, derived.GoodhartMinNovelty)
	}
	c := validCfg(t, map[string]string{})
	if c.Distill.NoveltyFloor != derived.GoodhartMinNovelty {
		t.Errorf("aufgeloester Default = %v, want %v", c.Distill.NoveltyFloor, derived.GoodhartMinNovelty)
	}
}

// TestDistillNoveltyFloorRange ist V33. Die beiden Grenzen des Intervalls
// bleiben legal — 0 ist der dokumentierte Aus-Schalter, 1 die extreme, aber
// lesbare Politik "jedes Token des Claims muss eigenes sein".
func TestDistillNoveltyFloorRange(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  Severity
		note  string
	}{
		{"0.15", -1, "der Default validiert sauber"},
		{"0", -1, "0 ist der dokumentierte Aus-Schalter"},
		{"1", -1, "1 ist eine extreme, aber lesbare Politik"},
		{"0.5", -1, "ein gewoehnlicher Wert"},
		{"-0.01", SeverityError, "negativ waere ein zweiter, stiller Aus-Schalter neben der 0"},
		{"1.01", SeverityError, "ueber 1 liesse den Arm rufen und alles verwerfen"},
		{"2", SeverityError, "dito, deutlicher"},
	} {
		issues := Validate(validCfg(t, map[string]string{"distill.novelty_floor": tc.value}))
		if got := severityFor(issues, "distill.novelty_floor"); got != tc.want {
			t.Errorf("distill.novelty_floor %s: Severity = %v, want %v (%s): %v",
				tc.value, got, tc.want, tc.note, issuesOn(issues, "distill.novelty_floor"))
		}
	}
}
