package derived

import (
	"bytes"
	"encoding/json"
	"errors"
)

// The claim kinds the answer schema admits (§4.4.1). There is deliberately no
// "aggregate": the computed class — counts, spans, absence, coverage — is
// produced by Go from the source set and enters the block through RenderBlock.
// A model never gets the opportunity to assert an aggregate, which is cheaper
// than any check of one and exact.
const (
	KindFinding  = "finding"
	KindState    = "state"
	KindFailure  = "failure"
	KindDecision = "decision"
	KindTopic    = "topic"
)

var claimKinds = map[string]struct{}{
	KindFinding:  {},
	KindState:    {},
	KindFailure:  {},
	KindDecision: {},
	KindTopic:    {},
}

// Response is the decoded model answer of one call.
type Response struct {
	// Claims are the schema-valid lines, in payload order.
	Claims []Claim

	// Offered is how many claim objects the payload carried — the value that
	// feeds provenance.coverage.claims_offered, summed over all map steps.
	// It counts the dropped lines too: a model that offers thirty lines of
	// which twenty are malformed has offered thirty.
	Offered int

	// Dropped is Offered - len(Claims): lines the SCHEMA refused, before any
	// gate ran.
	Dropped int
}

// ErrNoClaims is returned for a payload that is not an answer of this schema
// at all — no claims array, or unparsable JSON around it.
var ErrNoClaims = errors.New("derived: response carries no claims array")

// wireResponse is the outer envelope. claims is a pointer so a payload without
// the key is distinguishable from one with an empty array.
type wireResponse struct {
	Claims *[]json.RawMessage `json:"claims"`
}

// DecodeResponse decodes the model answer {"claims":[{claim,quote,source_id,kind}]}.
//
// A line WITHOUT a quote is dropped, not repaired and not admitted (§4.4.0,
// §7 W01-1 gate 7). The quote is the whole point: it is the only field a
// deterministic gate can verify, and a line without one is an assertion the
// module has no way to check. Making the field optional would let the computed
// class back into the model's half of the system through the schema, and every
// downstream gate would then be verifying a claim against nothing.
//
// The same holds for source_id (there would be nothing to verify AGAINST) and
// for kind (an unknown kind is how "aggregate" would arrive).
//
// Unknown fields drop the line as well, per line and not per payload: a model
// that smuggles {"claim":…,"quote":…,"count":26} has tried to express the
// computed class in a schema that does not have it, and the answer to that is
// to lose the line, not to lose the call.
func DecodeResponse(raw []byte) (Response, error) {
	var env wireResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return Response{}, err
	}
	if env.Claims == nil {
		return Response{}, ErrNoClaims
	}
	r := Response{Offered: len(*env.Claims)}
	for _, line := range *env.Claims {
		c, ok := decodeClaim(line)
		if !ok {
			continue
		}
		r.Claims = append(r.Claims, c)
	}
	r.Dropped = r.Offered - len(r.Claims)
	return r, nil
}

// decodeClaim decodes one line strictly and reports whether it satisfies the
// schema.
func decodeClaim(line json.RawMessage) (Claim, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var c Claim
	if err := dec.Decode(&c); err != nil {
		return Claim{}, false
	}
	if c.Quote == "" || c.SourceID == "" || c.Claim == "" {
		return Claim{}, false
	}
	if _, ok := claimKinds[c.Kind]; !ok {
		return Claim{}, false
	}
	return c, true
}
