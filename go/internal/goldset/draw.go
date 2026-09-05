package goldset

// The draw of wave C3-4a (amendment design/05a §C3-2-D05-3, -5, -7).
//
// The C2-6c calibration measured the wrong population: its sample WAS the
// uniform control draw, a set disjoint from the pooled candidates, with a judge
// positive rate of 0.0200 against 0.2409 on the real ones. On that base the
// chance agreement is 0.977440, so kappa's denominator is 0.0226 and a single
// cell moves it by 0.0591 — the stated threshold of 0.6 was arithmetically out
// of reach before the first judgement was made.
//
// This file draws the replacement: a fully judged CORE of 20 queries that
// anchors the metric computation, a STRATIFIED calibration sample over the
// remaining queries that measures the judge's error rates on the population
// where decisions are actually made, and a small slice of the old control set
// that keeps its own job — ControlHitRate — and nothing else.
//
// Two properties are structural rather than procedural. The draw is
// HASH-RANKED, not RNG-ranked: a later auditor can reproduce every selection in
// Go, Python or jq without this code. And the mapping cell → (stratum, weight,
// use, machine verdict) lives ONLY in the draw key, never in the sheet — the
// same separation PoolKey already holds, for the same reason: a stratum label
// is a judge proxy, because S1 and S2 mean "the machine said relevant".
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The strata of §C3-2-D05-3. S1/S2 split the machine-positive cells by arm
// overlap, S3/S4 the machine-negative ones by best arm rank — both are known
// BEFORE the judgement and both separate empirically (judge positive rate 0.186
// at one arm against 0.875 at four; 0.472 at rank 1-3 against 0.162 at 11-20).
//
// S3 carries the overdraw on purpose: it is the only set on which a judge error
// DOWNWARD — gold the machine threw away — can become visible at all, and the
// C2-6c run has exactly zero evidence for that direction.
const (
	StratumCore = "KERN"
	StratumS1   = "S1"
	StratumS2   = "S2"
	StratumS3   = "S3"
	StratumS4   = "S4"
	StratumS0   = "S0"
)

// stratumOrder fixes the key's row order so two draws of one seed are
// byte-identical regardless of map iteration.
var stratumOrder = map[string]int{
	StratumCore: 0, StratumS1: 1, StratumS2: 2, StratumS3: 3, StratumS4: 4, StratumS0: 5,
}

// armOverlapS1 is the arm count at which a machine-positive cell counts as
// well-covered, and headRank the rank below which a machine-negative cell
// counts as highly ranked. Both are the cut points of the tables in
// §C3-2-D05-3, not free parameters.
const (
	armOverlapS1 = 2
	headRank     = 10
)

// DrawSpec is the allocation of the draw. It is data rather than constants
// because the amendment's own open point 1 foresees re-allocating after the
// first batch measures the unknown Fable positive rate — and a re-allocation
// has to be visible in the key, not in a recompiled binary.
type DrawSpec struct {
	Seed       int64 `json:"seed"`
	CoreLocal  int   `json:"core_local"`
	CoreGlobal int   `json:"core_global"`
	S1         int   `json:"s1"`
	S2         int   `json:"s2"`
	S3         int   `json:"s3"`
	S4         int   `json:"s4"`
	S0         int   `json:"s0"`
}

// DefaultDrawSpec is the G-REAL allocation of §C3-2-D05-3: 14 local + 6 global
// core queries, 120/140/140/80 calibration cells and 60 control cells.
//
// The six global core queries are a deliberate over-weighting (19 global of
// 150; proportional would be 3): the Splits gate decides precisely over the
// local/global partition, and at n=3 the core says nothing about it. Every
// projection onto the slice takes the over-weighting back out through the
// stratum weights.
func DefaultDrawSpec(seed int64) DrawSpec {
	return DrawSpec{Seed: seed, CoreLocal: 14, CoreGlobal: 6, S1: 120, S2: 140, S3: 140, S4: 80, S0: 60}
}

// ValidateCore checks the core allocation before it is drawn from.
//
// Exactly ONE of the two regimes may ask for 0. A slice can genuinely have no
// population in one of them: the local/global partition is an X-W0 property of
// G-REAL (regime.go), while G-GLOB is 80 corpus aggregations that carry `global`
// throughout — demanding a positive number from `local` there would make the
// draw impossible rather than careful. A regime asked for 0 is skipped, and its
// population, empty or not, never enters the key.
//
// Both at 0 stays refused, and that is the load-bearing half of this rule. The
// core is the CENSUS the metric anchor and every weight-1 row rest on; a key
// without one carries strata whose Horvitz-Thompson estimates have nothing to
// be anchored against, and it would be written without a single complaint.
//
// A negative number is refused here rather than further down, where it reaches
// a slice expression and panics — a crash is not a rejection.
func (s DrawSpec) ValidateCore() error {
	if s.CoreLocal < 0 || s.CoreGlobal < 0 {
		return fmt.Errorf("Kern-Anforderung negativ (local=%d, global=%d) — "+
			"eine Ziehung zieht keine negative Anzahl Queries", s.CoreLocal, s.CoreGlobal)
	}
	if s.CoreLocal == 0 && s.CoreGlobal == 0 {
		return errors.New("Kern-Anforderung in beiden Regimen 0 — der Kern ist der Zensus, auf dem " +
			"die Metrik-Verankerung und die Hochrechnung ruhen; ein Schlüssel ohne ihn misst nichts")
	}
	return nil
}

func (s DrawSpec) allocation() map[string]int {
	return map[string]int{StratumS1: s.S1, StratumS2: s.S2, StratumS3: s.S3, StratumS4: s.S4}
}

// CoreQuery is one fully judged query of the metric anchor.
type CoreQuery struct {
	Slice       string `json:"slice"`
	Index       int    `json:"index"`
	QuerySHA256 string `json:"query_sha256"`
	Regime      string `json:"regime"`
	Cells       int    `json:"cells"`
}

// DrawCell is one row of the answer key: everything the sheet must not carry.
type DrawCell struct {
	Slice       string  `json:"slice"`
	Index       int     `json:"index"`
	QuerySHA256 string  `json:"query_sha256"`
	BlockID     string  `json:"block_id"`
	Stratum     string  `json:"stratum"`
	Weight      float64 `json:"weight"`
	CoreQuery   bool    `json:"core_query"`
	Control     bool    `json:"is_control"`
	LLMRelevant bool    `json:"llm_judgement"`
	Arms        int     `json:"arms"`
	BestRank    int     `json:"best_rank"`
}

func (c DrawCell) joinKey() string { return c.QuerySHA256 + "/" + c.BlockID }

// DrawKey is the answer key of one draw: the allocation, the population sizes
// the Horvitz-Thompson weights are derived from, and one row per drawn cell.
//
// It carries NO creation timestamp, and that is the point rather than an
// omission: gate 3 of §C3-2-D05-7 requires two draws of one seed to be
// byte-identical, and a timestamp would make the key a function of the clock
// instead of a function of its inputs. Without reproducibility the HT weights
// are not provable, which is abort criterion 3. The creation time belongs in
// the stamp and in the report, where it is provenance rather than content.
type DrawKey struct {
	Version     int            `json:"version"`
	SourceRun   string         `json:"source_run"`
	Rubric      string         `json:"rubric"`
	Spec        DrawSpec       `json:"spec"`
	Population  map[string]int `json:"population"`
	Sampled     map[string]int `json:"sampled"`
	CoreQueries []CoreQuery    `json:"core_queries"`
	Cells       []DrawCell     `json:"cells"`
}

// DrawInput is what a draw reads: the judged run, its pool, its control key and
// the X-W0 regime labels.
type DrawInput struct {
	SourceRun string
	Cells     []JudgeCell
	Judged    map[string][]Judgement
	Pool      []PoolEntry
	Key       PoolKey
	Regimes   map[string]string
	Spec      DrawSpec
}

// RubricGREAL is the goal-directed judging rubric of §C3-2-D05-5. It travels in
// the sheet header so the rule the verdicts were given under is auditable
// against them, and it is the operative difference to the machine prompt
// (judge.go:61-67), which judges against the QUESTION rather than against the
// Wissens-Ebenen goal.
const RubricGREAL = "G-REAL · 1 = der Block trägt zur Antwort auf genau diese Frage bei, " +
	"so dass eine Antwort ohne ihn sachlich ärmer wäre (Tatsache, Entscheidung, Zahl, Pfad, Zusammenhang); " +
	"er muss weder beste noch einzige noch vollständige Antwort sein. " +
	"0 = nur gemeinsames Vokabular, anderes System/Zeitraum/Vorgang, oder Wiederholung ohne Zusatz. " +
	"? = am vorliegenden Auszug nicht entscheidbar (Auszug bricht ab, oder Frage für diesen Block mehrdeutig) " +
	"— nicht für Grenzfälle, die werden entschieden. Leitfrage: Würde ich diesen Block in eine Antwort " +
	"auf genau diese Frage aufnehmen?"

// armFacts are the two stratification variables of a pooled cell.
type armFacts struct{ arms, bestRank int }

// armIndex reads arm overlap and best rank per (case, block) out of the pool.
// The per-arm order is the RANK — the pool file stores each arm head in rank
// order, which is what makes "best rank" derivable without a second dump.
func armIndex(pool []PoolEntry) map[string]armFacts {
	out := map[string]armFacts{}
	for _, e := range pool {
		k := e.Key()
		for _, arm := range [][]string{e.Semantic, e.FTSDe, e.FTSEn, e.Trigram} {
			for i, id := range arm {
				if id == "" {
					continue
				}
				f := out[k+"/"+id]
				f.arms++
				if f.bestRank == 0 || i+1 < f.bestRank {
					f.bestRank = i + 1
				}
				out[k+"/"+id] = f
			}
		}
	}
	return out
}

// drawRank is the hash rank a selection is sorted by. Deliberately a digest of
// the seed and the labelled parts rather than a seeded RNG: the same order can
// be recomputed with sha256sum and sort, so the draw is verifiable without this
// binary (§C3-2-D05-3).
func drawRank(seed int64, parts ...string) string {
	return SHA256Hex(strconv.FormatInt(seed, 10) + "\x00" + strings.Join(parts, "\x00"))
}

// stratumOf classifies one pooled cell. Control draws are never classified
// here: they are their own stratum and their own number.
func stratumOf(llm bool, f armFacts) string {
	switch {
	case llm && f.arms >= armOverlapS1:
		return StratumS1
	case llm:
		return StratumS2
	case f.bestRank >= 1 && f.bestRank <= headRank:
		return StratumS3
	default:
		return StratumS4
	}
}

// Draw builds the answer key for one G-REAL judging run.
//
// Every rejection is fail-closed. A query without a regime label, a stratum
// smaller than its allocation, a cell without a machine verdict: each of them
// would silently shrink a population the weights are computed from, and a
// Horvitz-Thompson estimate over a population that is not what the key says is
// not an estimate at all.
func Draw(in DrawInput) (DrawKey, error) {
	if len(in.Cells) == 0 {
		return DrawKey{}, errors.New("keine Zellen zum Ziehen — die geurteilte Vorlage fehlt")
	}
	verdicts, err := verdictIndex(in.Judged)
	if err != nil {
		return DrawKey{}, err
	}
	controls := controlIndex(in.Key)
	arms := armIndex(in.Pool)

	core, coreSet, err := drawCore(in)
	if err != nil {
		return DrawKey{}, err
	}
	pools := map[string][]DrawCell{}
	for _, c := range in.Cells {
		ck := c.Key() + "/" + c.BlockID
		llm, ok := verdicts[ck]
		if !ok {
			return DrawKey{}, fmt.Errorf("%s: kein Maschinen-Urteil — "+
				"die Schichtung ruht darauf und darf sie nicht raten", ck)
		}
		cell := DrawCell{
			Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256, BlockID: c.BlockID,
			LLMRelevant: llm, Control: controls[ck], Arms: arms[ck].arms, BestRank: arms[ck].bestRank,
		}
		switch {
		case cell.Control:
			cell.Stratum = StratumS0
		case coreSet[c.QuerySHA256]:
			cell.Stratum, cell.CoreQuery, cell.Weight = StratumCore, true, 1
		default:
			cell.Stratum = stratumOf(llm, arms[ck])
		}
		pools[cell.Stratum] = append(pools[cell.Stratum], cell)
	}

	key := DrawKey{
		Version: 1, SourceRun: in.SourceRun, Rubric: RubricGREAL, Spec: in.Spec,
		Population: map[string]int{}, Sampled: map[string]int{}, CoreQueries: core,
	}
	for s, cells := range pools {
		key.Population[s] = len(cells)
	}
	// The core is a census: every non-control cell of a core query is drawn.
	key.Cells = append(key.Cells, pools[StratumCore]...)
	key.Sampled[StratumCore] = len(pools[StratumCore])

	alloc := in.Spec.allocation()
	alloc[StratumS0] = in.Spec.S0
	for _, s := range []string{StratumS1, StratumS2, StratumS3, StratumS4, StratumS0} {
		role := "stratum"
		if s == StratumS0 {
			role = "control"
		}
		drawn, derr := takeByHash(pools[s], in.Spec.Seed, role, s, alloc[s])
		if derr != nil {
			return DrawKey{}, derr
		}
		w := float64(len(pools[s])) / float64(len(drawn))
		for i := range drawn {
			drawn[i].Weight = w
		}
		key.Sampled[s] = len(drawn)
		key.Cells = append(key.Cells, drawn...)
	}
	sortDrawCells(key.Cells)
	return key, nil
}

// drawCore picks the fully judged queries, per regime, by hash rank.
func drawCore(in DrawInput) ([]CoreQuery, map[string]bool, error) {
	if err := in.Spec.ValidateCore(); err != nil {
		return nil, nil, err
	}
	type q struct {
		slice  string
		index  int
		sha    string
		regime string
		cells  int
	}
	seen := map[string]*q{}
	var order []string
	for _, c := range in.Cells {
		e, ok := seen[c.QuerySHA256]
		if !ok {
			r, labelled := in.Regimes[c.QuerySHA256]
			if !labelled {
				return nil, nil, fmt.Errorf("%s: kein X-W0-Regime-Label — "+
					"der Kern wird je Regime gezogen und darf keines erfinden", c.QuerySHA256)
			}
			e = &q{slice: c.Slice, index: c.Index, sha: c.QuerySHA256, regime: r}
			seen[c.QuerySHA256] = e
			order = append(order, c.QuerySHA256)
		}
		e.cells++
	}
	want := map[string]int{RegimeLocal: in.Spec.CoreLocal, RegimeGlobal: in.Spec.CoreGlobal}
	set := map[string]bool{}
	var out []CoreQuery
	for _, regime := range []string{RegimeLocal, RegimeGlobal} {
		// A regime asked for 0 is skipped whole — no ranking, no selection, no
		// row in the key. That is the single-regime case (ValidateCore), and it
		// is deliberately independent of whether the regime HAS a population:
		// "draw nothing here" is a stated allocation, not a consequence of an
		// empty half.
		if want[regime] == 0 {
			continue
		}
		var pool []*q
		for _, sha := range order {
			if seen[sha].regime == regime {
				pool = append(pool, seen[sha])
			}
		}
		if len(pool) < want[regime] {
			return nil, nil, fmt.Errorf("%s: %d Queries im Regime, der Kern verlangt %d",
				regime, len(pool), want[regime])
		}
		sort.Slice(pool, func(i, j int) bool {
			a := drawRank(in.Spec.Seed, "core", regime, pool[i].sha)
			b := drawRank(in.Spec.Seed, "core", regime, pool[j].sha)
			if a != b {
				return a < b
			}
			return pool[i].sha < pool[j].sha
		})
		for _, e := range pool[:want[regime]] {
			set[e.sha] = true
			out = append(out, CoreQuery{Slice: e.slice, Index: e.index, QuerySHA256: e.sha, Regime: e.regime})
		}
	}
	// The cell counts are filled after the control cells are known, so the
	// number in the key is the number the sheet actually carries.
	controls := controlIndex(in.Key)
	counts := map[string]int{}
	for _, c := range in.Cells {
		if set[c.QuerySHA256] && !controls[c.Key()+"/"+c.BlockID] {
			counts[c.QuerySHA256]++
		}
	}
	for i := range out {
		out[i].Cells = counts[out[i].QuerySHA256]
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Regime != out[j].Regime {
			return out[i].Regime < out[j].Regime
		}
		return out[i].QuerySHA256 < out[j].QuerySHA256
	})
	return out, set, nil
}

// takeByHash draws n cells of one stratum by hash rank.
func takeByHash(pool []DrawCell, seed int64, role, stratum string, n int) ([]DrawCell, error) {
	if n <= 0 {
		return nil, fmt.Errorf("%s: Ziehungsumfang %d — eine Schicht ohne Zellen trägt keine Hochrechnung", stratum, n)
	}
	if len(pool) < n {
		return nil, fmt.Errorf("%s: hält %d Zellen, gezogen werden sollen %d", stratum, len(pool), n)
	}
	out := append([]DrawCell(nil), pool...)
	sort.Slice(out, func(i, j int) bool {
		a := drawRank(seed, role, stratum, out[i].QuerySHA256, out[i].BlockID)
		b := drawRank(seed, role, stratum, out[j].QuerySHA256, out[j].BlockID)
		if a != b {
			return a < b
		}
		return out[i].joinKey() < out[j].joinKey()
	})
	return out[:n], nil
}

func sortDrawCells(cells []DrawCell) {
	sort.Slice(cells, func(i, j int) bool {
		if stratumOrder[cells[i].Stratum] != stratumOrder[cells[j].Stratum] {
			return stratumOrder[cells[i].Stratum] < stratumOrder[cells[j].Stratum]
		}
		return cells[i].joinKey() < cells[j].joinKey()
	})
}

// verdictIndex flattens the parsed judgements into one verdict per cell.
func verdictIndex(judged map[string][]Judgement) (map[string]bool, error) {
	out := make(map[string]bool, 8192)
	for k, js := range judged {
		for _, j := range js {
			ck := k + "/" + j.BlockID
			if prev, dup := out[ck]; dup && prev != j.Relevant {
				return nil, fmt.Errorf("%s: zwei verschiedene Maschinen-Urteile", ck)
			}
			out[ck] = j.Relevant
		}
	}
	return out, nil
}

func controlIndex(key PoolKey) map[string]bool {
	out := make(map[string]bool, 1024)
	for k, ids := range key.ControlIDs {
		for _, id := range ids {
			out[k+"/"+id] = true
		}
	}
	return out
}

// MarshalDrawKey renders the key in the byte form written to disk.
func MarshalDrawKey(k DrawKey) ([]byte, error) {
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteDrawKey persists the answer key at mode 0600.
func WriteDrawKey(path string, k DrawKey) error {
	b, err := MarshalDrawKey(k)
	if err != nil {
		return err
	}
	return writeOwnerOnly(path, b)
}

// ReadDrawKey loads an answer key.
func ReadDrawKey(path string) (DrawKey, error) {
	var k DrawKey
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return k, fmt.Errorf("%s: %w", path, err)
	}
	if len(k.Cells) == 0 {
		return k, fmt.Errorf("%s: Ziehungs-Schlüssel ohne Zellen", path)
	}
	return k, nil
}
