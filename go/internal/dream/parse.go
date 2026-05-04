package dream

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Format tokens for parseLinks, persisted in context_llm_log.metadata.parse_format
// to track LLM drift patterns over time. Empty string means "no parseable shape"
// (parse error or empty/sentinel input — distinguishable via the returned error).
const (
	formatArray        = "array"
	formatObject       = "object"
	formatFencedArray  = "fenced-array"
	formatFencedObject = "fenced-object"
)

// parseLinks parses the LLM JSON response into Link structs.
// Tolerates two qwen3.6:27b drift patterns observed in V3 production
// (audit S25, 2026-05-03):
//   - Object-map form: {"<uuid>": {"type": "...", "confidence": <float|string>}, ...}
//   - String-encoded confidence labels: "high"|"medium"|"low" → 0.9/0.6/0.3
//
// Strips a leading ```json fence if present (43/1481 historical cases). The fence
// presence is detected BEFORE stripping so "fenced-X" / "X" can be distinguished
// in the returned format token.
//
// Returns (links, format, err). format is one of formatArray | formatObject |
// formatFencedArray | formatFencedObject; empty for empty/sentinel inputs and
// for parse errors. Map→slice conversion sorts by TargetID for deterministic
// downstream behavior.
func parseLinks(raw string) ([]Link, string, error) {
	trimmed := strings.TrimSpace(raw)
	wasFenced := strings.HasPrefix(trimmed, "```")
	body := stripCodeFence(trimmed)
	if body == "" || body == "[]" || body == "{}" {
		return nil, "", nil
	}

	var links []Link
	arrErr := json.Unmarshal([]byte(body), &links)
	if arrErr == nil {
		if wasFenced {
			return links, formatFencedArray, nil
		}
		return links, formatArray, nil
	}

	if strings.HasPrefix(body, "{") {
		var obj map[string]struct {
			Type       string          `json:"type"`
			Confidence json.RawMessage `json:"confidence"`
		}
		if err := json.Unmarshal([]byte(body), &obj); err == nil {
			ids := make([]string, 0, len(obj))
			for id := range obj {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			out := make([]Link, 0, len(obj))
			for _, id := range ids {
				v := obj[id]
				conf, ok := coerceConfidence(v.Confidence)
				if !ok {
					continue
				}
				out = append(out, Link{TargetID: id, Relationship: v.Type, Confidence: conf})
			}
			if wasFenced {
				return out, formatFencedObject, nil
			}
			return out, formatObject, nil
		}
	}

	return nil, "", fmt.Errorf("parse links: %w", arrErr)
}

// stripCodeFence removes a leading ```json (or ```) fence and trailing ```
// from LLM responses that wrap their JSON output. Idempotent on plain JSON.
func stripCodeFence(raw string) string {
	if !strings.HasPrefix(raw, "```") {
		return raw
	}
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}

// coerceConfidence accepts a json.RawMessage and returns a float64 confidence.
// Tolerates float (canonical) and string labels emitted under format drift.
// Mapping: "high"=0.9, "medium"=0.6, "low"=0.3 — chosen so that "medium"/"low"
// land below the 0.7 minRawConfidence gate and get dropped downstream rather
// than synthesising a bogus pass-through value. Unparseable input returns
// ok=false so the caller can drop the entry.
func coerceConfidence(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "high":
			return 0.9, true
		case "medium":
			return 0.6, true
		case "low":
			return 0.3, true
		}
	}
	return 0, false
}
