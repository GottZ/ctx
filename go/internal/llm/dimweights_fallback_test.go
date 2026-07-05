// Package llm — D-B post-derivation unit tests (Vorhaben A, wave W2).
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// In-package on purpose: parseTemporalResponse is unexported, and this file
// must not import rrf (rrf imports llm — cycle). The vocabulary assertion
// against rrf.DimensionSigma lives in dimweights_vocab_test.go (llm_test).
//
// Source: https://github.com/GottZ/ctx
package llm

import (
	"math"
	"testing"
)

func dates(date string, hour *int) []TemporalDate {
	return []TemporalDate{{Ref: "probe", Date: date, Dir: "past", Hour: hour}}
}

func TestDeriveDimensionWeights(t *testing.T) {
	h := 9
	cases := []struct {
		name  string
		query string
		dates []TemporalDate
		want  map[string]float64
	}{
		// --- Rule 1: weekday ---
		{
			name:  "weekday single, phase-coherent (2026-06-30 is a Tuesday) — dwLinearWeekday",
			query: "was war am dienstag mit dem deploy",
			dates: dates("2026-06-30", nil),
			want:  map[string]float64{"linear": 0.6, "weekday": 0.4},
		},
		{
			name:  "weekday recurring, phase-coherent — capped, never pure-cyclic (gate ii)",
			query: "was passiert immer dienstags",
			dates: dates("2026-06-30", nil),
			want:  map[string]float64{"linear": minLinearFallback, "weekday": 1.0 - minLinearFallback},
		},
		{
			name:  "weekday plural alone implies recurrence (isWeekdayPlural via hasRecurrence)",
			query: "die dienstags meetings",
			dates: dates("2026-06-30", nil),
			want:  map[string]float64{"linear": minLinearFallback, "weekday": 1.0 - minLinearFallback},
		},
		{
			name:  "weekday phase-INcoherent (2026-07-01 is a Wednesday) — linear-only guard",
			query: "was war am dienstag mit dem deploy",
			dates: dates("2026-07-01", nil),
			want:  map[string]float64{"linear": 1.0},
		},
		// --- Rule 2: month ---
		{
			name:  "month token, phase-coherent — dwLinearMonth",
			query: "die architektur entscheidungen im märz",
			dates: dates("2026-03-15", nil),
			want:  map[string]float64{"linear": 0.5, "month": 0.5},
		},
		{
			name:  "month token EN, phase-coherent",
			query: "decisions from march",
			dates: dates("2026-03-02", nil),
			want:  map[string]float64{"linear": 0.5, "month": 0.5},
		},
		{
			name:  "month phase-INcoherent (date in April) — linear-only guard",
			query: "die architektur entscheidungen im märz",
			dates: dates("2026-04-15", nil),
			want:  map[string]float64{"linear": 1.0},
		},
		// --- Rule 3: recurrence classes ---
		{
			name:  "weekly recurrence — dwLinearWeek",
			query: "der wöchentlich rotierende bericht",
			dates: dates("2026-06-29", nil),
			want:  map[string]float64{"linear": 0.7, "week": 0.3},
		},
		{
			name:  "quarterly recurrence — canonical fallback split (no factory, R3)",
			query: "die quartalsweise abrechnung",
			dates: dates("2026-06-30", nil),
			want:  map[string]float64{"linear": 0.5, "quarter": 0.5},
		},
		// --- Rule 4: daily needs an Hour anchor ---
		{
			name:  "daily WITH hour anchor — capped daily",
			query: "das daily standup protokoll",
			dates: dates("2026-06-30", &h),
			want:  map[string]float64{"linear": minLinearFallback, "daily": 1.0 - minLinearFallback},
		},
		{
			name:  "daily WITHOUT hour anchor — phase incoherent (midnight), linear-only",
			query: "das daily standup protokoll",
			dates: dates("2026-06-30", nil),
			want:  map[string]float64{"linear": 1.0},
		},
		// --- Defaults / degenerate input ---
		{
			name:  "no detector token — linear-only default",
			query: "was ist vor zwei wochen passiert",
			dates: dates("2026-06-21", nil),
			want:  map[string]float64{"linear": 1.0},
		},
		{
			name:  "unparseable anchor date — linear-only (no phase to check)",
			query: "was war am dienstag",
			dates: dates("kein-datum", nil),
			want:  map[string]float64{"linear": 1.0},
		},
		{
			name:  "empty dates — linear-only",
			query: "was war am dienstag",
			dates: nil,
			want:  map[string]float64{"linear": 1.0},
		},
		{
			name:  "monatlich has no phase anchor — deliberately NOT derived, linear-only",
			query: "der monatlich erzeugte report",
			dates: dates("2026-06-01", nil),
			want:  map[string]float64{"linear": 1.0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveDimensionWeights(tc.query, tc.dates)
			if len(got) != len(tc.want) {
				t.Fatalf("weights = %v, want %v", got, tc.want)
			}
			for k, w := range tc.want {
				if math.Abs(got[k]-w) > 1e-9 {
					t.Fatalf("weights[%s] = %v, want %v (full: %v)", k, got[k], w, tc.want)
				}
			}
		})
	}
}

// TestDeriveDimensionWeights_Invariants pins the two hard gates for EVERY
// derivable output: sum ~1.0 (budget invariant, query.go Step 6a) and an
// explicit linear share ≥ minLinearFallback (never pure-cyclic from the
// fallback — a missing/zero linear key would switch the linear path off).
// Red-proved: raising a rule's cyclic share to 1.0 (pure-cyclic) fails the
// linear-floor assert; de-normalizing a split (0.6/0.6) fails the sum assert.
func TestDeriveDimensionWeights_Invariants(t *testing.T) {
	h := 14
	probes := []struct {
		query string
		dates []TemporalDate
	}{
		{"was war am dienstag", dates("2026-06-30", nil)},
		{"immer dienstags", dates("2026-06-30", nil)},
		{"entscheidungen im märz", dates("2026-03-15", nil)},
		{"wöchentlich rotierender bericht", dates("2026-06-29", nil)},
		{"quartalsweise abrechnung", dates("2026-06-30", nil)},
		{"daily standup", dates("2026-06-30", &h)},
		{"vor zwei wochen", dates("2026-06-21", nil)},
		{"am dienstag", dates("2026-07-01", nil)}, // incoherent → linear
	}
	for _, p := range probes {
		got := DeriveDimensionWeights(p.query, p.dates)
		sum := 0.0
		for _, w := range got {
			sum += w
		}
		if math.Abs(sum-1.0) > 1e-9 {
			t.Errorf("query %q: weight sum = %v, want 1.0 (%v)", p.query, sum, got)
		}
		if got["linear"] < minLinearFallback {
			t.Errorf("query %q: linear share %v < floor %v — pure-cyclic from fallback (%v)",
				p.query, got["linear"], minLinearFallback, got)
		}
	}
}

// TestParseTemporalResponse_DiscardsHallucinatedWeights pins the D-B contract:
// the LLM is never asked for dimension_weights, and anything it hallucinates
// (including the invalid "year", which would inflate the consumer's boost
// budget — design 01 R5) is replaced by the deterministic derivation.
// Red-proved: keeping the unmarshalled map (derivation call removed) leaves
// year:1.0 in the result and fails both asserts.
func TestParseTemporalResponse_DiscardsHallucinatedWeights(t *testing.T) {
	raw := `{"dates":[{"ref":"am dienstag","date":"2026-06-30","end":null,"dir":"past"}],` +
		`"query":"was war am dienstag","dimension_weights":{"year":1.0,"weekday":3.5}}`

	res, err := parseTemporalResponse(raw, "was war am dienstag")
	if err != nil {
		t.Fatalf("parseTemporalResponse: %v", err)
	}
	if res == nil {
		t.Fatal("parseTemporalResponse returned nil result")
	}
	if _, ok := res.DimensionWeights["year"]; ok {
		t.Fatalf("hallucinated year key survived: %v", res.DimensionWeights)
	}
	want := map[string]float64{"linear": 0.6, "weekday": 0.4}
	for k, w := range want {
		if math.Abs(res.DimensionWeights[k]-w) > 1e-9 {
			t.Fatalf("weights = %v, want derived %v", res.DimensionWeights, want)
		}
	}
}

// TestParseTemporalResponse_Contract pins fence stripping and the
// empty-dates→nil contract that NormalizeTemporal callers rely on.
func TestParseTemporalResponse_Contract(t *testing.T) {
	fenced := "```json\n{\"dates\":[{\"ref\":\"im märz\",\"date\":\"2026-03-15\",\"end\":null,\"dir\":\"past\"}],\"query\":\"q\"}\n```"
	res, err := parseTemporalResponse(fenced, "entscheidungen im märz")
	if err != nil || res == nil {
		t.Fatalf("fenced parse failed: res=%v err=%v", res, err)
	}
	if math.Abs(res.DimensionWeights["month"]-0.5) > 1e-9 {
		t.Fatalf("fenced parse: weights = %v, want month:0.5", res.DimensionWeights)
	}

	res, err = parseTemporalResponse(`{"dates":[],"query":"q"}`, "q")
	if err != nil || res != nil {
		t.Fatalf("empty dates: want (nil, nil), got (%v, %v)", res, err)
	}

	if _, err := parseTemporalResponse(`not json`, "q"); err == nil {
		t.Fatal("invalid JSON: want error")
	}
}
