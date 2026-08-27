package goldset

// The X-W0 stratification of G-REAL (design/05 §4.4b, §7 row X-W0): every real
// query carries a regime label — `local` (punctual, single-hop) or `global`
// (synthesising, multi-hop) — because the literature measures OPPOSITE winners
// in the two regimes. Without the split every uplift statement is a mean over
// two regimes in which the same layer works in opposite directions.
//
// The labels are DATA about existing cases, never a slice of their own: the
// file adds no case, it partitions one. That is why it is read here but does
// NOT enter STAMP.json's per-slice registry, whose entries mean "n cases live
// in this file" and carry n/file/sha256 for exactly that reading. The scoring
// side records the file and its digest in the REPORT instead, which is where
// the provenance of a scoring input belongs.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// FileRegimeLabels is the conventional name of the X-W0 label file inside the
// gold directory. It is untracked like every other file there and carries no
// query text — only digests, the two bits and the labelling rationale.
const FileRegimeLabels = "x-w0-labels.jsonl"

// The two regimes of the BenchmarkQED schema, as X-W0 wrote them.
const (
	RegimeLocal  = "local"
	RegimeGlobal = "global"
)

// RegimeLabel is one labelled query. Only the two fields the split is computed
// from are read: `session_bezogen`, `grenzfall` and the rationale stay in the
// file. The session bit is board-checkpoint material (design/05 §4.8, H0-A0)
// and is deliberately NOT a slice — a field this loader does not read cannot
// silently become one.
type RegimeLabel struct {
	QuerySHA256 string `json:"query_sha256"`
	Regime      string `json:"regime"`
}

// ReadRegimeLabels loads the label file into a query-digest → regime map.
//
// Every rejection here is fail-closed by design: a label file that is short a
// line, names an unknown regime or repeats a digest would silently move cases
// into the wrong half, and a half that is wrong by a handful of cases is not
// visible in any figure the report prints.
func ReadRegimeLabels(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for n, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var l RegimeLabel
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n+1, err)
		}
		if l.QuerySHA256 == "" {
			return nil, fmt.Errorf("%s:%d: Label ohne query_sha256 — es ließe sich keinem Fall zuordnen", path, n+1)
		}
		if l.Regime != RegimeLocal && l.Regime != RegimeGlobal {
			return nil, fmt.Errorf("%s:%d: regime=%q, erwartet %q oder %q",
				path, n+1, l.Regime, RegimeLocal, RegimeGlobal)
		}
		if prev, seen := out[l.QuerySHA256]; seen {
			return nil, fmt.Errorf("%s:%d: query_sha256 doppelt gelabelt (%q und %q)",
				path, n+1, prev, l.Regime)
		}
		out[l.QuerySHA256] = l.Regime
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: keine Labels gelesen", path)
	}
	return out, nil
}
