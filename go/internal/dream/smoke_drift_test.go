// Smoke test against three historical Object-form drift samples from
// production context_llm_log (2026-05-02). Removed after the parseLinks
// hardening commit lands; kept inline here only to verify real-world parses.

package dream

import "testing"

const driftSampleA = `{
  "019c515a-0eb0-7699-9e05-5cae96288f12": {
    "type": "topical",
    "confidence": 0.9
  },
  "019c515a-1339-7320-a689-d7623b7adbfa": {
    "type": "topical",
    "confidence": 0.85
  }
}`

const driftSampleB = `{
  "019d28e0-4311-7ee0-ac2a-681935c4e019": {
    "type": "supersedes",
    "confidence": "high"
  },
  "019d33de-e77f-7548-b7ad-7c003ec5831b": {
    "type": "factual",
    "confidence": "high"
  }
}`

const driftSampleC = `{
  "019d28e0-4311-7ee0-ac2a-681935c4e019": {
    "type": "topical",
    "confidence": 0.9
  },
  "019d314a-5f70-7f6c-9bff-39892456e471": {
    "type": "topical",
    "confidence": 0.9
  }
}`

func TestParseLinks_RealHistoricalDrift(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		min  int
	}{
		{"sampleA_float_conf", driftSampleA, 2},
		{"sampleB_string_conf_high", driftSampleB, 2},
		{"sampleC_float_conf", driftSampleC, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			links, _, err := parseLinks(c.raw)
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			if len(links) < c.min {
				t.Fatalf("want >=%d links, got %d", c.min, len(links))
			}
			for _, l := range links {
				if l.Confidence <= 0 || l.Confidence > 1 {
					t.Errorf("invalid confidence: %+v", l)
				}
				if l.TargetID == "" || l.Relationship == "" {
					t.Errorf("empty field: %+v", l)
				}
			}
		})
	}
}
