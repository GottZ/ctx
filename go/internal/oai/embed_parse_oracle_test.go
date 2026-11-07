package oai

// The oracle of the embeddings parse (W02-09 rebase round 2, part A): the
// top-level parse as it stood before the port onto FOLGE-P's key helpers
// (levelKeys, keyIndex, decodeKey) — encoding/json's map of the body, the
// repeat found by a second decode (uniqueMembers/repeatedKey, root's
// jsonkeys.go@8e00a12e) and, past the five names the level knows, the first
// six members decoded one by one (tooManyMembers). Kept verbatim — on the
// chat oracle's copies of root's member walk and repeat check
// (parse_oracle_test.go: baseObjectMembers, baseNewMemberIter,
// baseUniqueMembers, baseRepeated) — so the
// CPU gate measures the port against it and the differential compares the
// answers (embed_parse_differential_test.go). The field readers
// readEncoding and readDimensions are the route's own; readModel and
// readInput are the ones before FOLGE-E (baseReadModel, baseReadInput:
// json.Unmarshal of the whole model and of every input), so the
// differential compares the ledger's size run and decode with them.

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/modelalias"
)

// baseEmbedTop is parseEmbedTop before the port.
func baseEmbedTop(body []byte) (*embedRequest, *paramError) {
	if !json.Valid(body) {
		return nil, notAnObject()
	}
	switch n := baseObjectMembers(body); {
	case n < 0:
		return nil, notAnObject()
	case n > len(embedKeys)+len(embedDropKeys):
		return nil, baseTooManyMembers(body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, notAnObject()
	}
	if perr := baseUniqueMembers("", body, raw); perr != nil {
		return nil, perr
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	req := &embedRequest{}
	for _, k := range keys {
		switch {
		case embedDropKeys[k]:
			req.ignored = append(req.ignored, k)
		case !embedKeys[k]:
			return nil, unsupportedParameter(k)
		}
	}
	if perr := req.baseReadModel(raw["model"]); perr != nil {
		return nil, perr
	}
	if perr := req.baseReadInput(raw["input"]); perr != nil {
		return nil, perr
	}
	if perr := req.readEncoding(raw["encoding_format"]); perr != nil {
		return nil, perr
	}
	if perr := req.readDimensions(raw["dimensions"]); perr != nil {
		return nil, perr
	}
	return req, nil
}

// baseTooManyMembers is tooManyMembers before the port.
func baseTooManyMembers(body []byte) *paramError {
	it, _ := baseNewMemberIter(body)
	limit := len(embedKeys) + len(embedDropKeys) + 1
	seen := make(map[string]bool, limit)
	var unknown []string
	repeatedName := ""
	for range limit {
		key, _, more := it.next()
		if !more {
			break
		}
		var k string
		if err := json.Unmarshal(append(append([]byte{'"'}, key...), '"'), &k); err != nil {
			k = string(key)
		}
		if seen[k] && repeatedName == "" {
			repeatedName = k
		}
		seen[k] = true
		if !embedKeys[k] && !embedDropKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if repeatedName != "" || len(unknown) == 0 {
		return baseRepeated("", repeatedName)
	}
	sort.Strings(unknown)
	return unsupportedParameter(unknown[0])
}

// baseReadModel and baseReadInput are readModel and readInput before FOLGE-E
// (W02-09 4abba96b), verbatim.
func (req *embedRequest) baseReadModel(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("model")
	}
	if err := json.Unmarshal(v, &req.model); err != nil {
		return invalidType("model", "a string")
	}
	if req.model == "" {
		return missingParameter("model")
	}
	return nil
}

// readInput reads input: a string ⇒ one input; an array of strings ⇒ that
// many; an empty text, an empty array, a token array (int[] or int[][]) or
// any other shape is refused. Token ids presume one tokenizer over the whole
// chain; a failover between two builds makes that uncheckable (design/02
// §4.8).
func (req *embedRequest) baseReadInput(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("input")
	}
	req.input = v
	switch kindOf(v) {
	case kindString:
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return invalidType("input", "a string or an array of strings")
		}
		if s == "" {
			return unsupportedValue("input", "an input must not be empty")
		}
		req.inputs = []string{s}
	case kindArray:
		// The elements are counted before one of them is decoded (H-1): a
		// count past the code ceiling of embedding.max_inputs is refused at
		// the cost of a scan; the alias's own max_inputs follows once it is
		// resolved (checkEmbedPolicy).
		n, over := arrayElems(v, modelalias.CeilingEmbeddingMaxInputs)
		switch {
		case over:
			// The code ceiling, before any alias is known: the text names no
			// alias's limit (Fremd-Review 3 F3-5).
			return unsupportedValue("input", fmt.Sprintf("more inputs than any model accepts (%d)", modelalias.CeilingEmbeddingMaxInputs))
		case n == 0:
			return unsupportedValue("input", "at least one input is required")
		}
		req.inputs = make([]string, 0, n)
		var perr *paramError
		eachArrayElem(v, func(i int, e []byte) bool {
			if i == 0 && hasKind(e, kindNumber|kindArray) {
				perr = unsupportedValue("input", "token arrays are not supported; send the texts")
				return false
			}
			param := fmt.Sprintf("input[%d]", i)
			var s string
			if kindOf(e) != kindString || json.Unmarshal(e, &s) != nil {
				perr = invalidType(param, "a string")
				return false
			}
			if s == "" {
				perr = unsupportedValue(param, "an input must not be empty")
				return false
			}
			req.inputs = append(req.inputs, s)
			return true
		})
		if perr != nil {
			return perr
		}
	default:
		return invalidType("input", "a string or an array of strings")
	}
	req.runes = make([]int, len(req.inputs))
	for i, s := range req.inputs {
		req.runes[i] = utf8.RuneCountInString(s)
		req.runesSum += req.runes[i]
		req.bytesSum += len(s)
	}
	return nil
}
