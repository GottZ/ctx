package goldset

// The blind judging sheet of wave C3-4a (amendment design/05a §C3-2-D05-5).
//
// C2-6c blinded with `jq del(.llm_judgement)`. That is no longer enough: under
// the stratified draw the STRATUM itself gives the machine verdict away — S1
// and S2 mean "the machine said relevant", S3 and S4 mean it did not. So do the
// weight (one value per stratum), the arm count and the best rank. Every one of
// them is removed, and the sheet is checked against the list rather than
// trusted to have been built correctly: a sheet that leaked a proxy would be
// found out only after the judgements are worthless.
//
// The permutation is part of the blinding, not cosmetics. Core cells and
// calibration cells share one sheet in one hash-ranked order, so no row reveals
// what it will be used for.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// UnsureMark is the third verdict of §C3-2-D05-5: "not decidable on the excerpt
// at hand". It exists for two situations only — an excerpt that breaks off
// before the deciding passage, and a question that is ambiguous for this block.
// A borderline case is NOT one of them: it gets decided.
const UnsureMark = "?"

// SheetVerdict is the tri-state judging vocabulary of C3-4. It is a separate
// vocabulary from `verdict` (pool.go:506-516) rather than an extension of it:
// the machine template and the C2-6c control sheet are closed on 1/0/y/n, and
// widening that function would let a `?` become a label in an artefact whose
// consumers never agreed to a third state.
type SheetVerdict int

// The three states. SheetIrrelevant is the zero value on purpose: a verdict
// that was never set must not read as gold.
const (
	SheetIrrelevant SheetVerdict = iota
	SheetRelevant
	SheetUnsure
)

// Relevant reports whether the verdict produces gold. `?` counts as 0 —
// declared before the run, conservative in the direction that cannot invent
// gold (§C3-2-D05-5, rule 1).
func (v SheetVerdict) Relevant() bool { return v == SheetRelevant }

// ParseSheetVerdict maps one filled sheet cell onto the tri-state vocabulary. An
// empty cell stays ErrUnjudged: a skipped row and a negative verdict differ by
// the whole of the judge's attention.
func ParseSheetVerdict(s string) (SheetVerdict, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "y":
		return SheetRelevant, nil
	case "0", "n":
		return SheetIrrelevant, nil
	case UnsureMark:
		return SheetUnsure, nil
	case "", UnjudgedMark:
		return SheetIrrelevant, fmt.Errorf("%w (erlaubt: 1, 0, y, n, %s)", ErrUnjudged, UnsureMark)
	}
	return SheetIrrelevant, fmt.Errorf("ungültiges Urteil %q (erlaubt: 1, 0, y, n, %s)",
		strings.TrimSpace(s), UnsureMark)
}

// forbiddenSheetFields are the judge proxies of §C3-2-D05-5. `judgement` and
// `control_judgement` are in the list too: they are the field names of the
// machine template and of the C2-6c control sheet, and a sheet that carried one
// of them would be a sheet built from the wrong renderer.
var forbiddenSheetFields = []string{
	"llm_judgement", "judgement", "control_judgement",
	"stratum", "weight", "core_query", "is_control", "control",
	"arms", "best_rank", "rank", "gold_ids",
}

// ForbiddenSheetFields returns the blinding blacklist.
func ForbiddenSheetFields() []string {
	return append([]string(nil), forbiddenSheetFields...)
}

// fableRow is one JSONL row of the blind sheet. Every field it does NOT have is
// the point of the type.
type fableRow struct {
	Kind        string  `json:"kind"`
	Rubric      string  `json:"rubric,omitempty"`
	SourceRun   string  `json:"source_run,omitempty"`
	Rows        int     `json:"rows,omitempty"`
	Judge       string  `json:"judge,omitempty"`
	Slice       string  `json:"slice,omitempty"`
	Index       int     `json:"index,omitempty"`
	QuerySHA256 string  `json:"query_sha256,omitempty"`
	Query       string  `json:"query,omitempty"`
	BlockID     string  `json:"block_id,omitempty"`
	Title       string  `json:"title,omitempty"`
	Excerpt     string  `json:"excerpt,omitempty"`
	Verdict     *string `json:"verdict,omitempty"`
}

// FableJudge names who fills the sheet. Entscheid E4-4 ("fable-judge") made the
// Haupt-Lead the authoritative judge of this strecke, not the second opinion on
// a machine run — which is why the name lives in the sheet header and in the
// report rather than in a footnote.
const FableJudge = "Haupt-Lead (Fable), zielgeleitet — Entscheide E2-4 + E4-4"

// RenderFableSheetJSONL emits the blind sheet: a header with the rubric, then
// one row per drawn cell in hash-ranked order.
func RenderFableSheetJSONL(k DrawKey, cells []JudgeCell) ([]byte, error) {
	if len(k.Cells) == 0 {
		return nil, errors.New("Ziehungs-Schlüssel hält keine Zelle — der Bogen wäre leer")
	}
	byKey := make(map[string]JudgeCell, len(cells))
	for _, c := range cells {
		byKey[c.QuerySHA256+"/"+c.BlockID] = c
	}
	rows := append([]DrawCell(nil), k.Cells...)
	sort.Slice(rows, func(i, j int) bool {
		a := drawRank(k.Spec.Seed, "sheet", rows[i].QuerySHA256, rows[i].BlockID)
		b := drawRank(k.Spec.Seed, "sheet", rows[j].QuerySHA256, rows[j].BlockID)
		if a != b {
			return a < b
		}
		return rows[i].joinKey() < rows[j].joinKey()
	})
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(fableRow{
		Kind: "header", Rubric: k.Rubric, SourceRun: k.SourceRun,
		Rows: len(rows), Judge: FableJudge,
	}); err != nil {
		return nil, err
	}
	empty := ""
	for _, d := range rows {
		c, ok := byKey[d.joinKey()]
		if !ok {
			return nil, fmt.Errorf("gezogene Zelle %s/%s hat keine Vorlagen-Zeile — "+
				"ohne Frage und Auszug ist sie nicht urteilbar", d.QuerySHA256, d.BlockID)
		}
		v := empty
		if err := enc.Encode(fableRow{
			Kind: "cell", Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
			Query: c.Query, BlockID: c.BlockID, Title: c.Title, Excerpt: c.Excerpt, Verdict: &v,
		}); err != nil {
			return nil, err
		}
	}
	out := []byte(buf.String())
	if err := AssertSheetBlind(out); err != nil {
		return nil, fmt.Errorf("der gerenderte Bogen ist nicht blind: %w", err)
	}
	return out, nil
}

// AssertSheetBlind refuses a sheet that carries any judge proxy.
//
// It runs over the RAW document rather than over the typed row: a field this
// package does not know is exactly the field a later change would add, and a
// typed round trip would drop it silently instead of reporting it.
func AssertSheetBlind(b []byte) error {
	rows := 0
	for n, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return fmt.Errorf("ungültiges JSON in Bogenzeile %d: %w", n+1, err)
		}
		for _, f := range forbiddenSheetFields {
			if _, bad := raw[f]; bad {
				return fmt.Errorf("verbotenes Feld %q in Bogenzeile %d — es verrät das "+
					"Maschinen-Urteil oder die Verwendung der Zelle (§C3-2-D05-5)", f, n+1)
			}
		}
		rows++
	}
	if rows == 0 {
		return errors.New("der Bogen hält keine Zeile")
	}
	return nil
}

// FableJudgement is one filled sheet cell, keyed the way §C3-2-D05-5 (6)
// prescribes: by (query_sha256, block_id), never by line number.
type FableJudgement struct {
	QuerySHA256 string
	BlockID     string
	Verdict     SheetVerdict
}

// ParseFableSheet reads a filled blind sheet.
func ParseFableSheet(path string) ([]FableJudgement, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if berr := AssertSheetBlind(b); berr != nil {
		return nil, berr
	}
	var out []FableJudgement
	seen := map[string]SheetVerdict{}
	for n, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r fableRow
		if uerr := json.Unmarshal([]byte(line), &r); uerr != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n+1, uerr)
		}
		switch r.Kind {
		case "header":
			continue
		case "cell":
		default:
			return nil, fmt.Errorf("%s:%d: unbekannte Zeilenart %q", path, n+1, r.Kind)
		}
		cell := ""
		if r.Verdict != nil {
			cell = *r.Verdict
		}
		v, verr := ParseSheetVerdict(cell)
		if verr != nil {
			return nil, fmt.Errorf("%s:%d: Block %s: %w", path, n+1, r.BlockID, verr)
		}
		k := r.QuerySHA256 + "/" + r.BlockID
		if prev, dup := seen[k]; dup && prev != v {
			return nil, fmt.Errorf("%s:%d: Zelle %s zweimal mit verschiedenen Urteilen", path, n+1, k)
		}
		seen[k] = v
		out = append(out, FableJudgement{QuerySHA256: r.QuerySHA256, BlockID: r.BlockID, Verdict: v})
	}
	if len(out) == 0 {
		return nil, errors.New("der Bogen hält keine geurteilte Zeile")
	}
	return out, nil
}
