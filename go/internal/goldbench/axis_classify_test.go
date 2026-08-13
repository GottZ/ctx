package goldbench

import (
	"encoding/json"
	"math"
	"testing"
)

// clusterRun baut einen caseRun der cluster-label-Achse: gold-Label + rohe
// Modell-Antwort (JSON-Contract {"label": "..."} wie parseLabel erwartet).
func clusterRun(t *testing.T, goldLabel, modelAnswer string) caseRun {
	t.Helper()
	gold, err := json.Marshal(clusterLabelGold{Label: goldLabel})
	if err != nil {
		t.Fatal(err)
	}
	return caseRun{
		c:       &Case{ID: "t", Gold: gold, LabelQuality: "silver"},
		outputs: []string{modelAnswer},
	}
}

func answer(label string) string { return `{"label": "` + label + `"}` }

func TestScoreClusterLabel(t *testing.T) {
	cases := []struct {
		name   string
		gold   string
		out    string
		score  float64
		parsed bool
	}{
		{"exakter Treffer", "Stratum Audio Architektur", answer("Stratum Audio Architektur"), 1, true},
		{"case-insensitiv", "Stratum Audio Architektur", answer("stratum audio architektur"), 1, true},
		// tp=2 (tagesberichte, 2024), fp=2, fn=0 → P 0.5, R 1 → F1 2/3.
		{"Teiltreffer", "Tagesberichte 2024", answer("Tagesberichte über Infrastruktur 2024"), 2.0 / 3, true},
		// Bindestriche splitten im Tokenizer: "home-net" → {home, net}.
		{"Bindestrich-Split", "home-net Betrieb", answer("Home-Net Betrieb"), 1, true},
		// Umlaute sind Token-Zeichen, keine Trenner.
		{"Umlaut-Token", "Größen Übersicht", answer("Größen Übersicht"), 1, true},
		{"Constraint-Bruch: kein JSON", "Egal", "Freitext ohne JSON", 0, false},
		{"Constraint-Bruch: leer", "Egal", answer(""), 0, false},
		// DOKUMENTIERTES GOLD-ARTEFAKT (Analyse 2026-08-13): 6/23 Datensatz-Golds
		// sind ·-gejointe Tag-Kopien und verletzen den eigenen Prompt-Contract
		// ("no punctuation", "Never copy an identifier … from the input").
		// Ein contract-treues Natural-Label kann gegen solches Gold nur
		// Token-Zufallstreffer landen — der Achsen-Score deckelt strukturell.
		// tp=2 (dotfiles, arch), fp=2, fn=3 → P 0.5, R 0.4 → F1 4/9 ≈ 0.44:
		// selbst ein thematisch korrektes Label bleibt unter der Hälfte.
		{"Tag-Join-Gold vs contract-treues Label", "dotfiles · arch-linux · git-branches",
			answer("Dotfiles und Arch-Setup"), 4.0 / 9, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, perCase := scoreClusterLabel([]caseRun{clusterRun(t, tc.gold, tc.out)})
			if len(perCase) != 1 {
				t.Fatalf("perCase: %d", len(perCase))
			}
			if perCase[0].Parsed != tc.parsed {
				t.Errorf("parsed = %v, erwartet %v", perCase[0].Parsed, tc.parsed)
			}
			if math.Abs(res.PrimaryScore-tc.score) > 1e-9 {
				t.Errorf("primary_score = %.4f, erwartet %.4f", res.PrimaryScore, tc.score)
			}
		})
	}
}

// Der v2-Primärwechsel (SC-3): constraint_pass_rate bleibt als Sekundär-Metrik
// erhalten und zählt NUR den Contract, nicht die Label-Qualität.
func TestScoreClusterLabelPassRateSecondary(t *testing.T) {
	runs := []caseRun{
		clusterRun(t, "A B", answer("völlig anderes Label")), // Contract ok, F1 0
		clusterRun(t, "A B", "kein JSON"),                    // Contract-Bruch
	}
	res, _ := scoreClusterLabel(runs)
	if res.PrimaryMetric != "constrained_token_f1" {
		t.Errorf("primary_metric = %q", res.PrimaryMetric)
	}
	if got := res.Secondary["constraint_pass_rate"]; math.Abs(got-0.5) > 1e-9 {
		t.Errorf("constraint_pass_rate = %.4f, erwartet 0.5", got)
	}
	if math.Abs(res.PrimaryScore-0) > 1e-9 {
		t.Errorf("primary_score = %.4f, erwartet 0", res.PrimaryScore)
	}
}
