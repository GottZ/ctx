// Package federation is the ingress policy of a ctx instance that serves
// other instances and apps over its model API (design/04 §4.5–§4.7, wave
// F-W9): the PeerIngress middleware behind the LoopGuard on /v1 and /v2, the
// policy of a foreign prompt (required sensitivity, egress) with its
// fail-closed default, the credentials scan over a request body and the
// chain filter in its fixed order.
//
// It imports the hop contract (fedhop), the consumer (auth), the pool's row
// type and chain filters (backends), the credentials scanner (sensitivity)
// and the OpenAI error writer (oai) — never handler, store or config: policy
// values arrive as arguments, the consumer through an injected accessor.
package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/sensitivity"
)

// Policy is the policy of one model-API request (design/04 §4.7): the
// sensitivity every row of its chain must admit, and whether it may leave
// this instance's own engines (Ctx-Egress: local ⇒ no further instance hop,
// no external row).
type Policy struct {
	Required    backends.Sensitivity
	EgressLocal bool
}

// defaultPolicy is the policy of a request that never passed a PeerIngress
// (design/04 §4.6 "Fail-closed ohne Policy", Finding 4.b): credentials and
// egress local — a handler reachable without the ingress works in the
// narrowest mode instead of the zero value's public + any.
var defaultPolicy = Policy{Required: backends.SensCredentials, EgressLocal: true}

type policyKey struct{}

// WithPolicy returns ctx carrying p — set by the PeerIngress (step 10).
func WithPolicy(ctx context.Context, p Policy) context.Context {
	return context.WithValue(ctx, policyKey{}, p)
}

// PolicyFrom returns the request's policy; without one the fail-closed
// default {credentials, egress local}. A set policy whose requirement is
// outside the vocabulary reads as credentials (backends' own rule for an
// unknown sensitivity, made explicit here).
func PolicyFrom(ctx context.Context) Policy {
	p, ok := ctx.Value(policyKey{}).(Policy)
	if !ok {
		return defaultPolicy
	}
	if !backends.ValidSensitivity(p.Required) {
		p.Required = backends.SensCredentials
	}
	return p
}

// ResolvePolicy is the consumer-and-header half of design/04 §4.7:
//
//	Required    = MergeSensitivity(ca.PromptSensitivity, in.Sensitivity)
//	EgressLocal = in.EgressLocal
//
// The consumer's prompt_sensitivity is the floor (NOT NULL, default
// credentials; only server-admin lowers it, design/01 §4.6); an empty or
// unknown floor is credentials. Ctx-Sensitivity raises it and never lowers
// it, from EVERY consumer — app and ctx-peer alike (§4.7 restrictive, lead
// decision 1): a header can only narrow its own request's chain. A missing
// header keeps the floor (Finding 25: the Rev.-0 "missing ⇒ credentials"
// would void every lowering). Ctx-Egress: local (or an unreadable value,
// fedhop's fail-closed parse) keeps the request on this instance's own
// engines; absent is any. The alias and tenant-overlay terms and the scan
// are added by the chain build (W02-07 RequiredSensitivity starts at
// PolicyFrom(ctx).Required; ScanRaise after the parse). A nil consumer —
// PeerIngress refuses that — is the fail-closed default.
func ResolvePolicy(ca *auth.ConsumerAuth, in fedhop.Inbound) Policy {
	if ca == nil {
		return defaultPolicy
	}
	return Policy{
		Required:    backends.Sensitivity(fedhop.MergeSensitivity(ca.PromptSensitivity, in.Sensitivity)),
		EgressLocal: in.EgressLocal,
	}
}

// ScanRaise is the scan half of design/04 §4.7 (Finding 2, D6): when
// required is below credentials, sensitivity.Scan runs over EVERY string
// leaf of the JSON body — object values and keys, at any depth — and one
// hit raises the requirement to credentials. A superset of the inputs §4.7
// names (message content and text parts, tool_calls[].function.arguments,
// role: tool results, embedding input, rerank query and documents): route-
// independent, so a new route or field cannot fall outside it, and only
// raising — a hit in a non-prompt field (user, metadata) costs trust
// headroom, never confidentiality. Leaves are decoded, so a JSON escape
// cannot hide a key; a number literal is scanned as its text; an object
// member with a scalar value is also scanned as "key: value" (a
// secret-named key such as "client_secret" next to its value — not over an
// array or object value, a named limit, F2-3). The cost is
// bounded by the request cap gateway.max_request_bytes, which the body cap
// enforces before any handler parses (measured in the F-W9 report). A body
// that is not valid JSON is scanned raw as one string (fail-closed: an
// unparsable body never skips the scan).
//
// Short circuit: at credentials there is nothing left to raise — no scan;
// an empty or unknown requirement is credentials. The route handlers
// (W02-07/09/10) call it after their parse.
func ScanRaise(required backends.Sensitivity, body []byte) backends.Sensitivity {
	if required.Rank() >= backends.SensCredentials.Rank() {
		return backends.SensCredentials
	}
	if bodyHasCredentials(body) {
		return backends.SensCredentials
	}
	return required
}

// assignmentProbeBytes bounds the value part of the key-value probe of an
// object member (see bodyHasCredentials): a space-free megabyte value is not
// scanned a second time. A mechanism bound, not a policy value (A-E2): a
// secret longer than this still passes the assignment rule's length and
// entropy tests on its first bytes.
const assignmentProbeBytes = 256

// assignmentProbe is the value part of the key-value probe: the first token
// of v, the one the scanner's assignment rule captures after "key:" (it
// skips whitespace and one quote, then takes everything up to whitespace, a
// quote, a comma or a semicolon), at most assignmentProbeBytes. Every other
// rule of the scanner ignores the key and has already seen the whole value,
// so the probe adds the assignment rule and nothing else — at the cost of a
// short string per member, not a second scan of the value.
func assignmentProbe(v string) string {
	v = strings.TrimLeft(v, " \t\n\f\r")
	v = strings.TrimPrefix(strings.TrimPrefix(v, `"`), "'")
	if i := strings.IndexAny(v, " \t\n\f\r\"',;"); i >= 0 {
		v = v[:i]
	}
	return v[:min(len(v), assignmentProbeBytes)]
}

// jsonFrame is one open container of the body walk: an object alternates
// keys and values, an array holds values only.
type jsonFrame struct {
	object    bool
	expectKey bool   // object: the next string token is a key
	key       string // object: the key whose value comes next
}

// bodyHasCredentials scans every string and number token of a JSON body —
// object keys and values at any depth, each on its own — and, for every
// object member with a scalar value, the probe "key: <first value token>": the
// scanner's assignment rule (a secret-named key next to a high-entropy
// value) needs both, and in JSON they are two tokens (Fremd-Review FR-3).
// A body that does not decode is scanned raw.
func bodyHasCredentials(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var stack []jsonFrame
	top := func() *jsonFrame {
		if len(stack) == 0 {
			return nil
		}
		return &stack[len(stack)-1]
	}
	hit := func(s string) bool { _, h := sensitivity.Scan(s); return h }
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			return hit(string(body))
		}
		var text string
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				stack = append(stack, jsonFrame{object: v == '{', expectKey: v == '{'})
			default: // the container closed; if it was a member value, a key comes next
				stack = stack[:len(stack)-1]
				if f := top(); f != nil && f.object {
					f.expectKey = true
				}
			}
			continue
		case string:
			text = v
		case json.Number:
			text = v.String()
		default: // bool, null: a member value without text
			if f := top(); f != nil && f.object {
				f.expectKey = true
			}
			continue
		}
		f := top()
		if f != nil && f.object && f.expectKey {
			f.key, f.expectKey = text, false
			if hit(text) {
				return true
			}
			continue
		}
		if hit(text) {
			return true
		}
		if f != nil && f.object {
			if hit(f.key + ": " + assignmentProbe(text)) {
				return true
			}
			f.expectKey = true
		}
	}
}
