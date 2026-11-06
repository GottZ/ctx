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
// answers (embed_parse_differential_test.go). The field readers (readModel,
// readInput, readEncoding, readDimensions) are the route's own: the port
// does not touch them.

import (
	"encoding/json"
	"sort"
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
	if perr := req.readModel(raw["model"]); perr != nil {
		return nil, perr
	}
	if perr := req.readInput(raw["input"]); perr != nil {
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
