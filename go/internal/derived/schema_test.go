package derived

import (
	"errors"
	"testing"
)

// TestGate7_LineWithoutQuoteIsRefused is gate 7 of §7 W01-1.
//
// The quote is the only field a deterministic gate can verify. A schema that
// makes it optional lets the computed class — aggregates, counts, absence
// statements — back into the model's half of the system, and every gate
// downstream would then be verifying a claim against nothing.
//
// Red probe: drop the c.Quote == "" branch in decodeClaim, i.e. decode quote
// as optional — the aggregate line is admitted and this test fails.
func TestGate7_LineWithoutQuoteIsRefused(t *testing.T) {
	raw := []byte(`{"claims":[
		{"claim":"26 Quellblöcke, ältester 2026-03-14.","source_id":"7c3e1f00-0000-4000-8000-000000000000","kind":"finding"},
		{"claim":"Das Embed-Backfill überspringt excluded-Typen.","quote":"excluded-Typen werden vom Embed-Backfill nicht","source_id":"7c3e1f00-0000-4000-8000-000000000000","kind":"finding"}
	]}`)

	r, err := DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if r.Offered != 2 {
		t.Fatalf("Offered = %d, want 2 — a dropped line was still offered", r.Offered)
	}
	if len(r.Claims) != 1 {
		t.Fatalf("decoded %d claims, want 1 — the line without a quote must be dropped", len(r.Claims))
	}
	if r.Claims[0].Quote == "" {
		t.Fatal("the surviving claim has no quote")
	}
	if r.Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", r.Dropped)
	}
}

// TestDecodeResponseDropsMalformedLines covers the rest of the schema: the
// computed class must not be expressible in any of its shapes.
func TestDecodeResponseDropsMalformedLines(t *testing.T) {
	const good = `{"claim":"Aussage.","quote":"excluded-Typen werden vom Embed-Backfill nicht","source_id":"7c3e1f00-0000-4000-8000-000000000000","kind":"finding"}`
	cases := []struct{ name, line string }{
		{"no quote", `{"claim":"Aussage.","source_id":"7c3e1f00-0000-4000-8000-000000000000","kind":"finding"}`},
		{"empty quote", `{"claim":"Aussage.","quote":"","source_id":"7c3e1f00-0000-4000-8000-000000000000","kind":"finding"}`},
		{"no source_id", `{"claim":"Aussage.","quote":"ein Zitat","kind":"finding"}`},
		{"no claim", `{"quote":"ein Zitat","source_id":"7c3e1f00-0000-4000-8000-000000000000","kind":"finding"}`},
		{"kind aggregate", `{"claim":"26 Quellen.","quote":"ein Zitat","source_id":"7c3e1f00-0000-4000-8000-000000000000","kind":"aggregate"}`},
		{"kind missing", `{"claim":"Aussage.","quote":"ein Zitat","source_id":"7c3e1f00-0000-4000-8000-000000000000"}`},
		{"smuggled count field", `{"claim":"Aussage.","quote":"ein Zitat","source_id":"7c3e1f00-0000-4000-8000-000000000000","kind":"finding","count":26}`},
		{"not an object", `"Aussage."`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := DecodeResponse([]byte(`{"claims":[` + c.line + `,` + good + `]}`))
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			if len(r.Claims) != 1 {
				t.Fatalf("decoded %d claims, want 1 — %s must be dropped", len(r.Claims), c.name)
			}
			if r.Offered != 2 || r.Dropped != 1 {
				t.Errorf("Offered=%d Dropped=%d, want 2/1", r.Offered, r.Dropped)
			}
		})
	}
}

// TestDecodeResponseAcceptsEveryDeclaredKind — the five kinds of §4.4.1, and
// only those.
func TestDecodeResponseAcceptsEveryDeclaredKind(t *testing.T) {
	for _, k := range []string{KindFinding, KindState, KindFailure, KindDecision, KindTopic} {
		raw := []byte(`{"claims":[{"claim":"Aussage.","quote":"ein Zitat","source_id":"x","kind":"` + k + `"}]}`)
		r, err := DecodeResponse(raw)
		if err != nil {
			t.Fatalf("DecodeResponse(%s): %v", k, err)
		}
		if len(r.Claims) != 1 {
			t.Errorf("kind %q was dropped; it is one of the five declared kinds", k)
		}
	}
}

// TestDecodeResponseWithoutClaimsArray — a payload that is not an answer of
// this schema at all is an error, not an empty result: silently returning zero
// claims would let a broken call look like an unproductive one.
func TestDecodeResponseWithoutClaimsArray(t *testing.T) {
	if _, err := DecodeResponse([]byte(`{"summary":"…"}`)); !errors.Is(err, ErrNoClaims) {
		t.Errorf("DecodeResponse without claims: err = %v, want ErrNoClaims", err)
	}
	if _, err := DecodeResponse([]byte(`{"claims":[]}`)); err != nil {
		t.Errorf("an EMPTY claims array is a productive shape, got %v", err)
	}
	if _, err := DecodeResponse([]byte(`not json`)); err == nil {
		t.Error("DecodeResponse accepted non-JSON")
	}
}
