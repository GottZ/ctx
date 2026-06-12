package llm

import (
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestParseClassifyAnswer: only a JSON object with a boolean "answer" field
// is a verdict. Everything else — prose, wrong types, missing field — is
// ErrClassifyParse, which the audit treats as no-verdict, never as a default
// (masterplan G41 negative probe: parse garbage ⇒ no downgrade).
func TestParseClassifyAnswer(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{"true", `{"answer": true}`, true, false},
		{"false", `{"answer": false}`, false, false},
		{"whitespace", "  {\"answer\": true}\n", true, false},
		{"extra fields", `{"answer": false, "reason": "no creds"}`, false, false},
		{"prose", `Ja, der Block enthält Credentials.`, false, true},
		{"empty", ``, false, true},
		{"missing field", `{"confidence": 0.9}`, false, true},
		{"string answer", `{"answer": "ja"}`, false, true},
		{"numeric answer", `{"answer": 1}`, false, true},
		{"null answer", `{"answer": null}`, false, true},
		{"bare bool", `true`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseClassifyAnswer(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseClassifyAnswer(%q) = %v, want error", tc.raw, got)
				}
				if !errors.Is(err, ErrClassifyParse) {
					t.Fatalf("error %v does not wrap ErrClassifyParse — the audit loop would abort instead of cooling down", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseClassifyAnswer(%q) error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseClassifyAnswer(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestDropExternal (G41 defense in depth): a full-trust external row passes
// the trust gate by definition — the locality filter must still drop it from
// a LocalOnly chain, even when that empties the chain.
func TestDropExternal(t *testing.T) {
	chain := []backends.Backend{
		{Name: "herbert-chat", Locality: backends.LocalityLAN},
		{Name: "zdr-cloud", Locality: backends.LocalityExternal},
		{Name: "llama-cpu", Locality: backends.LocalityLocal},
	}
	kept := dropExternal(chain, "classify")
	if len(kept) != 2 {
		t.Fatalf("kept %d backends, want 2", len(kept))
	}
	for _, b := range kept {
		if b.Locality == backends.LocalityExternal {
			t.Fatalf("external backend %q survived the local-only filter", b.Name)
		}
	}

	onlyExternal := []backends.Backend{{Name: "zdr-cloud", Locality: backends.LocalityExternal}}
	if kept := dropExternal(onlyExternal, "classify"); len(kept) != 0 {
		t.Fatalf("all-external chain kept %d backends, want 0", len(kept))
	}
}
