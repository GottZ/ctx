package goldbench

import (
	"encoding/json"
	"math"
	"testing"
)

func almostEqual(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func TestMicroF1(t *testing.T) {
	p, r, f1 := microF1(2, 1, 1)
	almostEqual(t, p, 2.0/3.0, "precision")
	almostEqual(t, r, 2.0/3.0, "recall")
	almostEqual(t, f1, 2.0/3.0, "f1")

	_, _, zero := microF1(0, 0, 0)
	almostEqual(t, zero, 0, "f1 leer")
}

func TestTokenF1(t *testing.T) {
	// Identische Token-Mengen → 1.0; Groß-/Kleinschreibung + Satzzeichen egal.
	almostEqual(t, tokenF1("Graph-Cache für pgvector", "graph cache FÜR pgvector!"), 1.0, "token f1 identisch")
	// Disjunkt → 0.
	almostEqual(t, tokenF1("alpha beta", "gamma delta"), 0.0, "token f1 disjunkt")
	// Umlaute sind Token-Zeichen: "läuft" ≠ "lauft".
	if tokenF1("läuft", "lauft") != 0 {
		t.Fatal("umlaut-token darf nicht mit entumlauteter form matchen")
	}
}

func TestKeywordOverlap(t *testing.T) {
	gold := []string{"pgvector", "graph-cache", "hnsw"}
	out := []string{"pgvector 0.8.5", "cache"} // Substring beidseitig: pgvector ⊂ out[0], out[1] ⊂ graph-cache
	recall, jaccard := keywordOverlap(gold, out)
	almostEqual(t, recall, 2.0/3.0, "recall")
	almostEqual(t, jaccard, 2.0/3.0, "jaccard") // 2 / (3+2-2)
}

func TestNdcgBinary(t *testing.T) {
	// Relevantes Doc auf Rang 1 → 1.0.
	almostEqual(t, ndcgBinary([]float64{9, 1, 0}, []int{0}, 15), 1.0, "ndcg perfekt")
	// Relevantes Doc auf Rang 2 von 2 → 1/log2(3).
	almostEqual(t, ndcgBinary([]float64{9, 1}, []int{1}, 15), 1.0/math.Log2(3), "ndcg rang 2")
	// Keine Relevanz-Indizes → 0.
	almostEqual(t, ndcgBinary([]float64{1, 2}, nil, 15), 0, "ndcg leer")
}

func TestTrgmSimilarity(t *testing.T) {
	// Identität → 1.0, Disjunkt → 0, dazwischen monoton plausibel.
	almostEqual(t, trgmSimilarity("Wochenbericht KW12", "Wochenbericht KW12"), 1.0, "trgm identisch")
	if s := trgmSimilarity("Wochenbericht KW12", "Wochenbericht KW13"); s <= 0.5 || s >= 1 {
		t.Fatalf("trgm ähnliche titel: %v außerhalb (0.5,1)", s)
	}
	almostEqual(t, trgmSimilarity("abc", "xyz"), 0, "trgm disjunkt")
}

// mkRun baut einen caseRun mit Gold/Input-JSON und Modell-Outputs.
func mkRun(t *testing.T, id, input, gold string, outputs ...string) caseRun {
	t.Helper()
	return caseRun{
		c: &Case{ID: id, Input: json.RawMessage(input), Gold: json.RawMessage(gold),
			LabelQuality: "gold"},
		outputs: outputs,
	}
}

func TestScoreTemporalBlock(t *testing.T) {
	runs := []caseRun{
		// Treffer + ein FP.
		mkRun(t, "a", `{}`, `{"dates":["2026-07-25"]}`,
			`{"dates":[{"date":"2026-07-25","source":"explicit"},{"date":"2026-01-01","source":"explicit"}]}`),
		// Parse-Fehler → FN.
		mkRun(t, "b", `{}`, `{"dates":["2026-07-12"]}`, `kein json`),
	}
	res, _ := scoreTemporalBlock(runs)
	almostEqual(t, res.ParseRate, 0.5, "parse rate")
	// tp=1 fp=1 fn=1 → P=0.5 R=0.5 F1=0.5.
	almostEqual(t, res.PrimaryScore, 0.5, "micro f1")
}

func TestScoreTemporalQueryEmptyGold(t *testing.T) {
	runs := []caseRun{
		// Leermengen-Korrektheit: gold leer, Output leer → Treffer.
		mkRun(t, "a", `{"query":"kein datum","today":"2026-08-12"}`, `{"dates":[]}`,
			`{"dates":[],"query":"kein datum"}`),
		// Exakter Tupel-Treffer.
		mkRun(t, "b", `{"query":"letzten Montag","today":"2026-08-12"}`,
			`{"dates":[{"date":"2026-08-10","end":null,"dir":"past"}]}`,
			`{"dates":[{"ref":"letzten Montag","date":"2026-08-10","end":null,"dir":"past"}],"query":"x"}`),
	}
	res, _ := scoreTemporalQuery(runs)
	almostEqual(t, res.ParseRate, 1.0, "parse rate")
	almostEqual(t, res.PrimaryScore, 1.0, "micro f1")
}

func TestScoreLinksContract(t *testing.T) {
	goldLink := `{"links":[{"target_id":"019d33de-e77f-7548-b7ad-7c003ec5831b","type":"topical"}]}`
	runs := []caseRun{
		// Exakt (target, type) → 1.0.
		mkRun(t, "a", `{}`, goldLink,
			`[{"target_id":"019d33de-e77f-7548-b7ad-7c003ec5831b","type":"topical","confidence":0.9}]`),
		// Richtiges target, falscher type → 0.5.
		mkRun(t, "b", `{}`, goldLink,
			`[{"target_id":"019d33de-e77f-7548-b7ad-7c003ec5831b","type":"causal","confidence":0.9}]`),
		// Gold leer, Output leer → 1.0.
		mkRun(t, "c", `{}`, `{"links":[]}`, `[]`),
		// Gold leer, Output gesetzt → 0.
		mkRun(t, "d", `{}`, `{"links":[]}`,
			`[{"target_id":"019d33de-e77f-7548-b7ad-7c003ec5831b","type":"topical","confidence":0.9}]`),
		// Leerer Output → kein Response, parse fail.
		mkRun(t, "e", `{}`, goldLink, ``),
	}
	res, _ := scoreLinks(runs)
	almostEqual(t, res.ParseRate, 0.8, "parse rate")
	almostEqual(t, res.PrimaryScore, (1.0+0.5+1.0+0+0)/5, "link score")
	if res.Confusion["topical"]["causal"] != 1 || res.Confusion["topical"]["topical"] != 1 ||
		res.Confusion["none"]["none"] != 1 || res.Confusion["none"]["topical"] != 1 {
		t.Fatalf("konfusionsmatrix unerwartet: %v", res.Confusion)
	}
}

func TestScoreRecurrence(t *testing.T) {
	runs := []caseRun{
		mkRun(t, "a", `{}`, `{"verdict":"recurrent"}`,
			`{"verdict":"recurrent","pattern":"parallel","confidence":0.9}`),
		// gold none, Prädiktion recurrent → falsch + none-FP.
		mkRun(t, "b", `{}`, `{"verdict":"none"}`,
			`{"verdict":"recurrent","pattern":"parallel","confidence":0.9}`),
		// gold none, Prädiktion none → korrekt.
		mkRun(t, "c", `{}`, `{"verdict":"none"}`,
			`{"verdict":"none","pattern":"none","confidence":0.8}`),
	}
	res, _ := scoreRecurrence(runs)
	almostEqual(t, res.PrimaryScore, 2.0/3.0, "accuracy")
	almostEqual(t, res.Secondary["none_fp_rate"], 0.5, "none fp rate")
}

func TestScoreSensitivityFNRate(t *testing.T) {
	runs := []caseRun{
		// Positiv-Fall credentials, Modell sagt false → FN.
		mkRun(t, "a", `{}`, `{"credentials":true,"personal":false}`,
			`{"answer": false}`, `{"answer": false}`),
		// Beide korrekt.
		mkRun(t, "b", `{}`, `{"credentials":false,"personal":true}`,
			`{"answer": false}`, `{"answer": true}`),
		// Zweite Antwort unparsebar → parse fail, Frage zählt als Fehler.
		mkRun(t, "c", `{}`, `{"credentials":false,"personal":false}`,
			`{"answer": false}`, `vielleicht`),
	}
	res, _ := scoreSensitivity(runs)
	almostEqual(t, res.ParseRate, 2.0/3.0, "parse rate")
	// Korrekt: a personal, b beide, c credentials = 4 von 6 Fragen.
	almostEqual(t, res.PrimaryScore, 4.0/6.0, "accuracy")
	almostEqual(t, res.Secondary["fn_rate_credentials"], 1.0, "fn credentials")
	almostEqual(t, res.Secondary["fn_rate_personal"], 0.0, "fn personal")
}

func TestScoreSynthesisContract(t *testing.T) {
	runs := []caseRun{
		// Antwort mit erlaubtem Zitat.
		mkRun(t, "a", `{}`, `{"expect":"answer","allowed_citations":[1,2]}`,
			`Der Service läuft auf Port 443 [1].`),
		// Zitat außerhalb allowed → 0.
		mkRun(t, "b", `{}`, `{"expect":"answer","allowed_citations":[1]}`,
			`Antwort [1] und [3].`),
		// Refusal via Sentinel.
		mkRun(t, "c", `{}`, `{"expect":"refusal","allowed_citations":[]}`,
			`NO_RELEVANT_SOURCES`),
		// Erwartete Antwort, aber Refusal → 0.
		mkRun(t, "d", `{}`, `{"expect":"answer","allowed_citations":[1]}`,
			`NO_RELEVANT_SOURCES`),
	}
	res, _ := scoreSynthesis(runs)
	almostEqual(t, res.PrimaryScore, 0.5, "contract pass")
	almostEqual(t, res.Secondary["refusal_accuracy"], 1.0, "refusal acc")
	almostEqual(t, res.Secondary["answer_accuracy"], 1.0/3.0, "answer acc")
}

func TestScoreTranslate(t *testing.T) {
	runs := []caseRun{
		// Sauberes Englisch mit Pflicht-Token.
		mkRun(t, "a", `{"query":"Sicherheitskopie der Datenbank wiederherstellen"}`,
			`{"must_contain_tokens":["backup"],"max_len_ratio":3.0}`,
			`restore database backup`),
		// Umlaut im Output → fail (Deutsch-Rest).
		mkRun(t, "b", `{"query":"Schreibschutz prüfen"}`,
			`{"must_contain_tokens":[],"max_len_ratio":3.0}`,
			`prüfe write guard`),
		// Mehrzeilig → validateTranslation fail.
		mkRun(t, "c", `{"query":"Kontextspeicher Statistik"}`,
			`{"must_contain_tokens":[],"max_len_ratio":3.0}`,
			"context store\nstatistics"),
	}
	res, _ := scoreTranslate(runs)
	almostEqual(t, res.PrimaryScore, 1.0/3.0, "pass rate")
	almostEqual(t, res.Secondary["umlaut_free_rate"], 2.0/3.0, "umlautfrei")
}

func TestScoreTitleConstraint(t *testing.T) {
	long := make([]byte, 0, 200)
	for i := 0; i < 130; i++ {
		long = append(long, 'a')
	}
	runs := []caseRun{
		mkRun(t, "a", `{}`, `{"title":"pgvector HNSW Tuning"}`, `{"title":"pgvector hnsw tuning"}`),
		// >120 Runen → Constraint-Verletzung, Score 0.
		mkRun(t, "b", `{}`, `{"title":"x"}`, `{"title":"`+string(long)+`"}`),
	}
	res, _ := scoreTitle(runs)
	almostEqual(t, res.PrimaryScore, 0.5, "token f1 mittel")
	almostEqual(t, res.Secondary["constraint_pass_rate"], 0.5, "constraint pass")
}

func TestSampleCasesDeterministic(t *testing.T) {
	cases := make([]*Case, 10)
	for i := range cases {
		cases[i] = &Case{ID: string(rune('a' + i))}
	}
	s1 := SampleCases(cases, 3, 42)
	s2 := SampleCases(cases, 3, 42)
	if len(s1) != 3 || len(s2) != 3 {
		t.Fatalf("sample länge: %d/%d", len(s1), len(s2))
	}
	for i := range s1 {
		if s1[i].ID != s2[i].ID {
			t.Fatalf("sampling nicht deterministisch: %v vs %v", s1[i].ID, s2[i].ID)
		}
	}
	if got := SampleCases(cases, 0, 42); len(got) != 10 {
		t.Fatalf("n=0 muss alle fälle liefern, got %d", len(got))
	}
}

// TestKeywordSetF1 prüft die v2-Primärmetrik: Prediction-Cap + Mindestlänge.
func TestKeywordSetF1(t *testing.T) {
	gold := []string{"pgvector", "hnsw"}
	// Exakter Doppel-Treffer → F1 1.0.
	almostEqual(t, keywordSetF1(gold, []string{"pgvector", "hnsw"}), 1.0, "perfekt")
	// Über-Generierung: 2 Treffer + 18 Müll-Terme. Cap 10 → precision 2/10,
	// recall 2/2 → F1 = 2*(0.2*1)/(1.2) = 1/3. v1-Recall wäre 1.0 gewesen.
	out := []string{"pgvector", "hnsw"}
	for i := 0; i < 18; i++ {
		out = append(out, "füllwort")
	}
	almostEqual(t, keywordSetF1(gold, out), 1.0/3.0, "cap bestraft über-generierung")
	// SC-2: Kurz-Token matchen nur exakt — "a" darf "pgvector" nicht treffen.
	if keywordSetF1([]string{"pgvector"}, []string{"a"}) != 0 {
		t.Fatal("kurz-token darf nicht per substring matchen")
	}
	almostEqual(t, keywordSetF1([]string{"ab"}, []string{"ab"}), 1.0, "kurz exakt ok")
}
