package blocktype

import (
	"strings"
	"testing"
)

// TestConfigSize565 pins the measure the size cap uses: the config as the
// database stores it, compact (PR #567 review, R2-4). The jsonb text form of a
// config — ", " and ": " — measures as its compact form; a string as jsonb
// writes it; a number as numeric writes it.
func TestConfigSize565(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{`{"v":1,"a":[1,2],"b":{"c":true,"d":null}}`, len(`{"v":1,"a":[1,2],"b":{"c":true,"d":null}}`)},
		{`{"v": 1, "a": [1, 2], "b": {"c": true, "d": null}}`, len(`{"v":1,"a":[1,2],"b":{"c":true,"d":null}}`)},
		{"{\n  \"v\" : 1\n}", len(`{"v":1}`)},
		{`["é\/x"]`, len(`["é/x"]`)},
		{`["a\"b\\c\n\t"]`, len(`["a\"b\\c\n\t"]`)},
		{`["\u0001"]`, len(`["\u0001"]`)},
		{`[1e-5]`, len(`[0.00001]`)},
		{`[1.5e2]`, len(`[150]`)},
		{`[1E+2]`, len(`[100]`)},
		{`[12.345e1]`, len(`[123.45]`)},
		{`[0.50]`, len(`[0.50]`)},
		{`[-0.0]`, len(`[0.0]`)},
		{`[-1.25]`, len(`[-1.25]`)},
		{`[0e5]`, len(`[0]`)},
		{`[0.001e2]`, len(`[0.1]`)},
		{`{}`, 2},
		{`not json`, len(`not json`)},
	} {
		if got := configSize([]byte(tc.raw)); got != tc.want {
			t.Errorf("configSize(%s) = %d, want %d", tc.raw, got, tc.want)
		}
	}
	if got := configSize([]byte(`[1e99999999]`)); got <= maxConfigBytes {
		t.Errorf("an exponent beyond any config: configSize = %d, want more than %d", got, maxConfigBytes)
	}
	// The cap applies to that measure: the jsonb text of a config at the cap
	// decodes; one byte more does not.
	at := sizedConfig(t, maxConfigBytes)
	spaced := strings.NewReplacer(`","`, `", "`, `":`, `": `).Replace(at)
	if len(spaced) <= maxConfigBytes {
		t.Fatalf("fixture premise: the spaced form is %d bytes, want more than %d", len(spaced), maxConfigBytes)
	}
	if _, err := DecodePolicy("sized", globalScope, false, false, []byte(spaced)); err != nil {
		t.Fatalf("a config at the cap in the jsonb text form (%d bytes): %v", len(spaced), err)
	}
	if _, err := DecodePolicy("sized", globalScope, false, false, []byte(sizedConfig(t, maxConfigBytes+1))); err == nil || !strings.Contains(err.Error(), "exceeds 16384 bytes (16385)") {
		t.Fatalf("one byte over the cap: %v", err)
	}
}
