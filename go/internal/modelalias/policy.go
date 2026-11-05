package modelalias

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// Reasoning is the policy value of "reasoning" (design/02 §3.2): off drops a
// client's reasoning request and pins the row's own state; passthrough
// forwards it.
type Reasoning string

const (
	ReasoningOff         Reasoning = "off"
	ReasoningPassthrough Reasoning = "passthrough"
)

// Opt is one policy entry: Set reports whether the stored policy names the
// key; V is its value then, the zero value otherwise. A value type on
// purpose — a Policy copied out of a snapshot shares nothing with it.
type Opt[T any] struct {
	V   T
	Set bool
}

// Policy is the parsed policy object of an alias. An unset entry means "the
// default", which the reading endpoint resolves — several defaults are
// settings or derive from the pool (design/02 §3.2 column "Default").
type Policy struct {
	MaxOutputTokens        Opt[int]
	DefaultMaxTokens       Opt[int]
	MaxN                   Opt[int]
	TimeoutS               Opt[int]
	Reasoning              Opt[Reasoning]
	Vision                 Opt[bool]
	VisionMaxBytes         Opt[int]
	ContextLength          Opt[int]
	ToolsMaxCount          Opt[int]
	ToolsMaxBytes          Opt[int]
	ResponseFormatMaxBytes Opt[int]
	LogitBiasMaxEntries    Opt[int]
	StopMaxEntries         Opt[int]
	EmbeddingNativeDims    Opt[int]
	EmbeddingMatryoshkaMin Opt[int]
	EmbeddingMaxInputs     Opt[int]
	EmbeddingSubBatch      Opt[int]
	EmbeddingMaxInputRunes Opt[int]
	RerankMaxDocuments     Opt[int]
	RerankSubBatch         Opt[int]
}

// Static ceilings: the column "Decke (Code)" of design/02 §3.2 — no policy
// value may exceed them, whatever any setting says. The setting-bound caps
// (gateway.max_output_tokens_ceiling for max_output_tokens and
// default_max_tokens, the request deadline for a non-stream timeout_s, the
// request cap for vision_max_bytes, gateway.embedding_max_floats for
// embedding.max_inputs × embedding.native_dims, gateway.tenant_alias_ceiling.*
// for tenant aliases) belong to the endpoints that read them, not here.
const (
	CeilingMaxN                   = 8
	CeilingTimeoutS               = 3600 // the stream case; non-stream is capped lower by the deadline
	CeilingVisionMaxBytes         = 8 << 20
	CeilingToolsMaxCount          = 256
	CeilingToolsMaxBytes          = 1 << 20
	CeilingResponseFormatMaxBytes = 256 << 10
	CeilingLogitBiasMaxEntries    = 1024
	CeilingStopMaxEntries         = 16
	CeilingEmbeddingMaxInputs     = 256
	CeilingEmbeddingSubBatch      = 256
	CeilingEmbeddingMaxInputRunes = 32768
	CeilingRerankMaxDocuments     = 1000
	CeilingRerankSubBatch         = 1000

	// MaxSubBlocks bounds ⌈max_inputs / sub_batch⌉ and ⌈max_documents /
	// sub_batch⌉: the upstream requests (each one lease) a single embeddings
	// or rerank request may fan out into.
	MaxSubBlocks = 8

	// The chat defaults of an unset key (design/02 §3.2 column "Default";
	// the W02-03 handover: named next to the ceilings, read by the chat
	// endpoint, W02-07). Data of the design, not policy knobs: an operator
	// sets the alias key instead.
	DefaultToolsMaxCount          = 64
	DefaultToolsMaxBytes          = 256 << 10
	DefaultResponseFormatMaxBytes = 64 << 10
	DefaultLogitBiasMaxEntries    = 64
	DefaultStopMaxEntries         = 4
	DefaultVisionMaxBytes         = 4 << 20

	// The defaults the sub-block invariant assumes for an unset key.
	DefaultEmbeddingMaxInputs = 32
	DefaultEmbeddingSubBatch  = 32
	DefaultRerankMaxDocuments = 128
	DefaultRerankSubBatch     = 128

	// policyKeyClip bounds the key a PolicyError names, in runes: the key of
	// an unknown or duplicate entry is input (a psql edit, an admin request —
	// the column has no size cap), and the error reaches a WARN line per
	// reload and a 422 body. Longer keys are cut and end in "…".
	policyKeyClip = 64

	// policyIntMax bounds every integer entry, ceiling or not: a value past
	// int32 is no count, size or token budget anyone means, and a consumer
	// multiplying two of them (max_inputs × native_dims) stays far from
	// overflow.
	policyIntMax = math.MaxInt32
)

// PolicyError is a refused policy: Key names the offending key ("" for the
// document as a whole — not an object, malformed, an unknown alias kind),
// cut to policyKeyClip runes.
// The admin action answers it with 422 at that field.
type PolicyError struct {
	Key    string
	Reason string
}

func (e *PolicyError) Error() string {
	if e.Key == "" {
		return "policy: " + e.Reason
	}
	return fmt.Sprintf("policy: key %q: %s", e.Key, e.Reason)
}

// kindSet is the set of alias kinds a key applies to.
type kindSet uint8

const (
	forChat kindSet = 1 << iota
	forEmbedding
	forRerank
	forAll = forChat | forEmbedding | forRerank
)

func (s kindSet) has(k Kind) bool {
	switch k {
	case KindChat:
		return s&forChat != 0
	case KindEmbedding:
		return s&forEmbedding != 0
	case KindRerank:
		return s&forRerank != 0
	}
	return false
}

type valueType uint8

const (
	typeInt valueType = iota
	typeBool
	typeReasoning
)

// policyKey is one row of design/02 §3.2.
type policyKey struct {
	name    string
	kinds   kindSet
	typ     valueType
	ceiling int // static ceiling of an int; 0 = none (policyIntMax still holds)
	intOf   func(*Policy) *Opt[int]
}

// policyKeys is the closed key set — data, pinned by TestPolicyKeyTablePin.
var policyKeys = []policyKey{
	{"max_output_tokens", forChat, typeInt, 0, func(p *Policy) *Opt[int] { return &p.MaxOutputTokens }},
	{"default_max_tokens", forChat, typeInt, 0, func(p *Policy) *Opt[int] { return &p.DefaultMaxTokens }},
	{"max_n", forChat, typeInt, CeilingMaxN, func(p *Policy) *Opt[int] { return &p.MaxN }},
	{"timeout_s", forAll, typeInt, CeilingTimeoutS, func(p *Policy) *Opt[int] { return &p.TimeoutS }},
	{"reasoning", forChat, typeReasoning, 0, nil},
	{"vision", forChat, typeBool, 0, nil},
	{"vision_max_bytes", forChat, typeInt, CeilingVisionMaxBytes, func(p *Policy) *Opt[int] { return &p.VisionMaxBytes }},
	{"context_length", forChat, typeInt, 0, func(p *Policy) *Opt[int] { return &p.ContextLength }},
	{"tools.max_count", forChat, typeInt, CeilingToolsMaxCount, func(p *Policy) *Opt[int] { return &p.ToolsMaxCount }},
	{"tools.max_bytes", forChat, typeInt, CeilingToolsMaxBytes, func(p *Policy) *Opt[int] { return &p.ToolsMaxBytes }},
	{"response_format.max_bytes", forChat, typeInt, CeilingResponseFormatMaxBytes, func(p *Policy) *Opt[int] { return &p.ResponseFormatMaxBytes }},
	{"logit_bias.max_entries", forChat, typeInt, CeilingLogitBiasMaxEntries, func(p *Policy) *Opt[int] { return &p.LogitBiasMaxEntries }},
	{"stop.max_entries", forChat, typeInt, CeilingStopMaxEntries, func(p *Policy) *Opt[int] { return &p.StopMaxEntries }},
	{"embedding.native_dims", forEmbedding, typeInt, 0, func(p *Policy) *Opt[int] { return &p.EmbeddingNativeDims }},
	{"embedding.matryoshka_min", forEmbedding, typeInt, 0, func(p *Policy) *Opt[int] { return &p.EmbeddingMatryoshkaMin }},
	{"embedding.max_inputs", forEmbedding, typeInt, CeilingEmbeddingMaxInputs, func(p *Policy) *Opt[int] { return &p.EmbeddingMaxInputs }},
	{"embedding.sub_batch", forEmbedding, typeInt, CeilingEmbeddingSubBatch, func(p *Policy) *Opt[int] { return &p.EmbeddingSubBatch }},
	{"embedding.max_input_runes", forEmbedding, typeInt, CeilingEmbeddingMaxInputRunes, func(p *Policy) *Opt[int] { return &p.EmbeddingMaxInputRunes }},
	{"rerank.max_documents", forRerank, typeInt, CeilingRerankMaxDocuments, func(p *Policy) *Opt[int] { return &p.RerankMaxDocuments }},
	{"rerank.sub_batch", forRerank, typeInt, CeilingRerankSubBatch, func(p *Policy) *Opt[int] { return &p.RerankSubBatch }},
}

// clipKey cuts key to policyKeyClip runes (rune-safe; invalid UTF-8 counts
// one rune per byte, as range does).
func clipKey(key string) string {
	n := 0
	for i := range key {
		if n == policyKeyClip {
			return key[:i] + "…"
		}
		n++
	}
	return key
}

func lookupKey(name string) (policyKey, bool) {
	for _, k := range policyKeys {
		if k.name == name {
			return k, true
		}
	}
	return policyKey{}, false
}

// ParsePolicy parses the policy object of an alias of kind — fail-closed
// (design/02 §3.2): the document must be exactly one JSON object; a key that
// is unknown, named twice, or not one of kind's keys, a value of the wrong
// type (integers are integer literals ≥ 1, no fraction, no exponent; booleans
// are booleans; reasoning is "off" or "passthrough"; null is no value), a
// value over its static ceiling, and a sub-batch that would split one
// request into more than MaxSubBlocks upstream requests are each a
// *PolicyError and yield the zero Policy.
func ParsePolicy(kind Kind, raw []byte) (Policy, error) {
	if !forAll.has(kind) {
		return Policy{}, &PolicyError{Reason: fmt.Sprintf("unknown alias kind %q", kind)}
	}
	p, err := parsePolicy(kind, raw)
	if err != nil {
		return Policy{}, err
	}
	if err := checkInvariants(&p); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func parsePolicy(kind Kind, raw []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	malformed := func(err error) error {
		return &PolicyError{Reason: "not one JSON object: " + err.Error()}
	}
	tok, err := dec.Token()
	if err != nil {
		return p, malformed(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return p, &PolicyError{Reason: "not a JSON object"}
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return p, malformed(err)
		}
		name, _ := tok.(string) // an object key is always a string token
		if seen[name] {
			return p, &PolicyError{Key: clipKey(name), Reason: "named twice"}
		}
		seen[name] = true
		k, ok := lookupKey(name)
		if !ok {
			return p, &PolicyError{Key: clipKey(name), Reason: "unknown key"}
		}
		if !k.kinds.has(kind) {
			return p, &PolicyError{Key: name, Reason: fmt.Sprintf("does not apply to a %s alias", kind)}
		}
		val, err := dec.Token()
		if err != nil {
			return p, malformed(err)
		}
		if err := setValue(&p, k, val); err != nil {
			return p, err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return p, malformed(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return p, &PolicyError{Reason: "trailing data after the object"}
	}
	return p, nil
}

// setValue stores one value token of key k into p.
func setValue(p *Policy, k policyKey, val json.Token) error {
	switch k.typ {
	case typeBool:
		b, ok := val.(bool)
		if !ok {
			return &PolicyError{Key: k.name, Reason: "want a boolean"}
		}
		p.Vision = Opt[bool]{V: b, Set: true}
	case typeReasoning:
		s, ok := val.(string)
		if !ok || (Reasoning(s) != ReasoningOff && Reasoning(s) != ReasoningPassthrough) {
			return &PolicyError{Key: k.name, Reason: `want "off" or "passthrough"`}
		}
		p.Reasoning = Opt[Reasoning]{V: Reasoning(s), Set: true}
	default:
		n, ok := val.(json.Number)
		if !ok {
			return &PolicyError{Key: k.name, Reason: "want an integer"}
		}
		v, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return &PolicyError{Key: k.name, Reason: "want an integer literal within range"}
		}
		if v < 1 || v > policyIntMax {
			return &PolicyError{Key: k.name, Reason: fmt.Sprintf("want 1..%d", policyIntMax)}
		}
		if k.ceiling > 0 && v > int64(k.ceiling) {
			return &PolicyError{Key: k.name, Reason: fmt.Sprintf("over the ceiling %d", k.ceiling)}
		}
		*k.intOf(p) = Opt[int]{V: int(v), Set: true}
	}
	return nil
}

// checkInvariants holds the cross-key rules: at most MaxSubBlocks upstream
// requests per embeddings / rerank request (unset sides at their defaults),
// and a non-empty dimensions interval when both ends are stated.
func checkInvariants(p *Policy) error {
	blocks := func(maxOpt, subOpt Opt[int], maxDefault, subDefault int) int {
		m, s := maxDefault, subDefault
		if maxOpt.Set {
			m = maxOpt.V
		}
		if subOpt.Set {
			s = subOpt.V
		}
		return (m + s - 1) / s
	}
	if n := blocks(p.EmbeddingMaxInputs, p.EmbeddingSubBatch, DefaultEmbeddingMaxInputs, DefaultEmbeddingSubBatch); n > MaxSubBlocks {
		return &PolicyError{Key: "embedding.sub_batch", Reason: fmt.Sprintf("splits embedding.max_inputs into %d upstream requests, at most %d", n, MaxSubBlocks)}
	}
	if n := blocks(p.RerankMaxDocuments, p.RerankSubBatch, DefaultRerankMaxDocuments, DefaultRerankSubBatch); n > MaxSubBlocks {
		return &PolicyError{Key: "rerank.sub_batch", Reason: fmt.Sprintf("splits rerank.max_documents into %d upstream requests, at most %d", n, MaxSubBlocks)}
	}
	if p.EmbeddingMatryoshkaMin.Set && p.EmbeddingNativeDims.Set && p.EmbeddingMatryoshkaMin.V > p.EmbeddingNativeDims.V {
		return &PolicyError{Key: "embedding.matryoshka_min", Reason: "above embedding.native_dims"}
	}
	return nil
}
