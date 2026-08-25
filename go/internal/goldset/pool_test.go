package goldset

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures below are synthetic. Real gold data never enters this suite:
// the slices carry query texts and block ids of a private corpus, and a test
// that needed them would be a test that cannot run in CI.

const fixtureSeed int64 = 20260812

// fixtureCases are three G-REAL cases.
func fixtureCases() []Case {
	var out []Case
	for i, q := range []string{"frage eins", "frage zwei", "frage drei"} {
		out = append(out, Case{
			Slice: SliceReal, Index: i, Query: q,
			QuerySHA256: SHA256Hex(q), Origin: "access-log",
		})
	}
	return out
}

// fixtureEntries give every case four arm heads with a deliberate overlap:
// p00 appears in three arms of case 0 and must reach the template once.
func fixtureEntries(cases []Case) []PoolEntry {
	var out []PoolEntry
	for i, c := range cases {
		base := i * 3
		out = append(out, PoolEntry{
			Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
			Semantic: []string{id("p", base), id("p", base+1)},
			FTSDe:    []string{id("p", base), id("p", base+2)},
			FTSEn:    []string{id("p", base+1)},
			Trigram:  []string{id("p", base)},
		})
	}
	return out
}

// fixtureControlPool is the retrievable corpus the control sample is drawn from.
func fixtureControlPool() []Block {
	out := make([]Block, 0, 40)
	for i := 0; i < 40; i++ {
		out = append(out, Block{
			ID: id("k", i), Title: "Titel " + id("k", i), TypeName: "note",
			Content: "Dieser Block trägt genug Text für einen Auszug und beschreibt einen Vorgang im System.",
		})
	}
	return out
}

// fixtureBlocks resolve every id the template can show.
func fixtureBlocks() map[string]Block {
	out := map[string]Block{}
	for _, b := range fixtureControlPool() {
		out[b.ID] = b
	}
	for i := 0; i < 12; i++ {
		out[id("p", i)] = Block{
			ID: id("p", i), Title: "Titel " + id("p", i), TypeName: "note",
			Content: "Ein weiterer Block mit ausreichend Inhalt für die Vorlage und eine Zeile Prosa.",
		}
	}
	return out
}

func id(prefix string, i int) string { return fmt.Sprintf("%s%02d", prefix, i) }

func buildFixture(t *testing.T, seed int64, controls int) ([]PooledCase, PoolKey) {
	t.Helper()
	cases := fixtureCases()
	pooled, key, err := BuildPool(cases, fixtureEntries(cases), fixtureControlPool(), controls, seed)
	if err != nil {
		t.Fatalf("BuildPool: %v", err)
	}
	return pooled, key
}

// --- Gate (a): the template is blind. A judge must not be able to read rank,
// arm or control membership out of the file they are judging (design 04 §4.5).

func TestTemplateIsBlind(t *testing.T) {
	t.Parallel()
	pooled, _ := buildFixture(t, fixtureSeed, 5)
	blocks := fixtureBlocks()
	jsonl, err := RenderTemplateJSONL(pooled, blocks, 200)
	if err != nil {
		t.Fatalf("render jsonl: %v", err)
	}
	md := RenderTemplateMarkdown(pooled, blocks, 200)

	forbidden := []string{
		"score", "rank", "semantic", "fts_de", "fts_en", "trigram",
		"control", "cos_sim", "mass_factor", "type_factor",
	}
	for _, form := range []struct {
		name string
		body []byte
	}{{"jsonl", jsonl}, {"markdown", md}} {
		low := strings.ToLower(string(form.body))
		for _, word := range forbidden {
			if strings.Contains(low, word) {
				t.Errorf("%s template leaks %q — the judgement must be blind to provenance", form.name, word)
			}
		}
	}
}

// TestTemplateHidesControlMembership pins the other half of the same rule: the
// control ids exist in the key file and nowhere else that marks them as such.
func TestTemplateHidesControlMembership(t *testing.T) {
	t.Parallel()
	pooled, key := buildFixture(t, fixtureSeed, 5)
	if len(key.ControlIDs) != len(pooled) {
		t.Fatalf("key covers %d of %d cases", len(key.ControlIDs), len(pooled))
	}
	md := string(RenderTemplateMarkdown(pooled, fixtureBlocks(), 200))
	for _, ids := range key.ControlIDs {
		if len(ids) != 5 {
			t.Fatalf("control sample is %d, want 5", len(ids))
		}
		for _, cid := range ids {
			// The id itself is of course in the template — that is the point.
			// What must not be there is a marker beside it.
			for _, line := range strings.Split(md, "\n") {
				if strings.Contains(line, cid) && !strings.HasPrefix(line, "| "+UnjudgedMark+" | "+cid+" |") {
					t.Errorf("control id %s appears on a row that is not a plain candidate row: %q", cid, line)
				}
			}
		}
	}
}

// TestPoolDeduplicatesAcrossArmsAndControls pins that an id nominated by
// several arms is one row, and that the control draw never repeats a pooled id.
func TestPoolDeduplicatesAcrossArmsAndControls(t *testing.T) {
	t.Parallel()
	pooled, key := buildFixture(t, fixtureSeed, 5)
	for _, p := range pooled {
		seen := map[string]bool{}
		for _, blockID := range p.BlockIDs {
			if seen[blockID] {
				t.Fatalf("case %s: id %s twice in the template", p.Key(), blockID)
			}
			seen[blockID] = true
		}
		// three distinct pooled ids (p00 is in three arms) plus five controls.
		if len(p.BlockIDs) != 8 {
			t.Fatalf("case %s: %d candidates, want 8", p.Key(), len(p.BlockIDs))
		}
		for _, cid := range key.ControlIDs[p.Key()] {
			if strings.HasPrefix(cid, "p") {
				t.Fatalf("case %s: pooled id %s drawn as control", p.Key(), cid)
			}
		}
	}
}

// TestControlDrawSkipsPooledIDs pins the exclusion when the control pool and
// the arm heads genuinely overlap.
func TestControlDrawSkipsPooledIDs(t *testing.T) {
	t.Parallel()
	cases := fixtureCases()[:1]
	entries := []PoolEntry{{
		Slice: cases[0].Slice, Index: cases[0].Index, QuerySHA256: cases[0].QuerySHA256,
		Semantic: []string{id("k", 0), id("k", 1)},
	}}
	pooled, key, err := BuildPool(cases, entries, fixtureControlPool(), 3, fixtureSeed)
	if err != nil {
		t.Fatalf("BuildPool: %v", err)
	}
	for _, cid := range key.ControlIDs[pooled[0].Key()] {
		if cid == id("k", 0) || cid == id("k", 1) {
			t.Fatalf("control draw repeated pooled id %s", cid)
		}
	}
	if len(pooled[0].BlockIDs) != 5 {
		t.Fatalf("%d candidates, want 2 pooled + 3 control", len(pooled[0].BlockIDs))
	}
}

// --- Gate (b): determinism. Same seed, byte-identical template; other seed,
// a different order — otherwise "seeded permutation" is a claim, not a fact.

func TestTemplateIsDeterministicInSeed(t *testing.T) {
	t.Parallel()
	blocks := fixtureBlocks()
	a, keyA := buildFixture(t, fixtureSeed, 5)
	b, keyB := buildFixture(t, fixtureSeed, 5)

	jsonlA, err := RenderTemplateJSONL(a, blocks, 200)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	jsonlB, err := RenderTemplateJSONL(b, blocks, 200)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(jsonlA) != string(jsonlB) {
		t.Error("same seed produced different JSONL templates")
	}
	if string(RenderTemplateMarkdown(a, blocks, 200)) != string(RenderTemplateMarkdown(b, blocks, 200)) {
		t.Error("same seed produced different markdown templates")
	}
	for k, ids := range keyA.ControlIDs {
		if strings.Join(ids, ",") != strings.Join(keyB.ControlIDs[k], ",") {
			t.Errorf("case %s: control sample not reproducible for the same seed", k)
		}
	}

	c, keyC := buildFixture(t, fixtureSeed+1, 5)
	orderMoved, controlMoved := false, false
	for i := range a {
		if strings.Join(a[i].BlockIDs, ",") != strings.Join(c[i].BlockIDs, ",") {
			orderMoved = true
		}
		if strings.Join(keyA.ControlIDs[a[i].Key()], ",") != strings.Join(keyC.ControlIDs[c[i].Key()], ",") {
			controlMoved = true
		}
	}
	if !orderMoved {
		t.Error("a different seed left every candidate order unchanged")
	}
	if !controlMoved {
		t.Error("a different seed drew the same control sample everywhere")
	}
}

// TestPermuteIgnoresInputOrder pins that the shuffle depends on the SET, not on
// how the caller happened to assemble it.
func TestPermuteIgnoresInputOrder(t *testing.T) {
	t.Parallel()
	sha := SHA256Hex("frage eins")
	forward := []string{"a", "b", "c", "d", "e", "f"}
	backward := []string{"f", "e", "d", "c", "b", "a"}
	if got, want := strings.Join(Permute(backward, fixtureSeed, sha), ","),
		strings.Join(Permute(forward, fixtureSeed, sha), ","); got != want {
		t.Errorf("input order changed the permutation: %q vs %q", got, want)
	}
}

// TestPermuteDiffersPerQuery pins that two queries of one run do not share an
// order — a judge who learns one order would otherwise know all of them.
func TestPermuteDiffersPerQuery(t *testing.T) {
	t.Parallel()
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	one := strings.Join(Permute(ids, fixtureSeed, SHA256Hex("frage eins")), ",")
	two := strings.Join(Permute(ids, fixtureSeed, SHA256Hex("frage zwei")), ",")
	if one == two {
		t.Error("two queries share one candidate order")
	}
}

// TestPermuteDiffersPerSeed pins the other half of gate (b) at the level where
// the order is decided, independent of the control draw: the same id set under
// two seeds must not come out in the same order.
func TestPermuteDiffersPerSeed(t *testing.T) {
	t.Parallel()
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	sha := SHA256Hex("frage eins")
	if strings.Join(Permute(ids, fixtureSeed, sha), ",") == strings.Join(Permute(ids, fixtureSeed+1, sha), ",") {
		t.Error("two seeds produced the same candidate order")
	}
}

// --- Gate (c): the control hit rate is a NUMBER in the stamp, and it is not
// computable without the key file.

func TestControlHitRateIsANumberInTheStamp(t *testing.T) {
	t.Parallel()
	pooled, key := buildFixture(t, fixtureSeed, 5)
	single := pooled[:1]
	key.ControlIDs = map[string][]string{single[0].Key(): key.ControlIDs[single[0].Key()]}

	// Two of the five control blocks are called relevant.
	relevant := map[string]bool{}
	for i, cid := range key.ControlIDs[single[0].Key()] {
		relevant[cid] = i < 2
	}
	judged := synthesise(single, relevant)

	rate, hits, total, err := ControlHitRate(judged, key)
	if err != nil {
		t.Fatalf("ControlHitRate: %v", err)
	}
	if hits != 2 || total != 5 {
		t.Fatalf("hits=%d total=%d, want 2/5", hits, total)
	}
	if rate != 0.4 {
		t.Fatalf("rate=%v, want 0.4", rate)
	}

	stamp := filepath.Join(t.TempDir(), FileStamp)
	if err := MergeStampSlice(stamp, SliceReal, map[string]any{"control_hit_rate": rate}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var doc map[string]any
	raw, err := os.ReadFile(stamp)
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("stamp is not JSON: %v", err)
	}
	got, ok := doc["slices"].(map[string]any)[SliceReal].(map[string]any)["control_hit_rate"].(float64)
	if !ok {
		t.Fatalf("control_hit_rate is not a number: %#v", doc)
	}
	if got != 0.4 {
		t.Fatalf("stamped control_hit_rate = %v, want 0.4", got)
	}
}

// TestControlHitRateNeedsTheKey is the red half of gate (c): without the key,
// the rate must be refused, never stamped as zero.
func TestControlHitRateNeedsTheKey(t *testing.T) {
	t.Parallel()
	pooled, key := buildFixture(t, fixtureSeed, 5)
	judged := synthesise(pooled, map[string]bool{})
	if _, _, _, err := ControlHitRate(judged, PoolKey{}); err == nil {
		t.Fatal("an empty key computed a control hit rate")
	}
	// A key naming a block nobody judged is equally not a measurement.
	key.ControlIDs[pooled[0].Key()] = append(key.ControlIDs[pooled[0].Key()], "never-judged")
	if _, _, _, err := ControlHitRate(judged, key); err == nil {
		t.Fatal("an unjudged control block still produced a rate")
	}
}

// --- Ingest: verdict vocabulary, missing rows, no_relevant, stamp merge.

func TestParseJudgementsRejectsMissingAndInvalid(t *testing.T) {
	t.Parallel()
	pooled, _ := buildFixture(t, fixtureSeed, 5)
	blocks := fixtureBlocks()
	body, err := RenderTemplateJSONL(pooled, blocks, 200)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	dir := t.TempDir()

	blank := filepath.Join(dir, "blank.jsonl")
	if err := os.WriteFile(blank, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = ParseJudgements(blank)
	if !errors.Is(err, ErrUnjudged) {
		t.Fatalf("untouched template parsed as judgements: %v", err)
	}
	if !strings.Contains(err.Error(), "blank.jsonl:2") {
		t.Errorf("error names no line: %v", err)
	}

	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte(strings.Replace(string(body), `"judgement":""`, `"judgement":"maybe"`, 1)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = ParseJudgements(bad)
	if err == nil || !strings.Contains(err.Error(), "invalid judgement") {
		t.Fatalf("invalid verdict accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "bad.jsonl:2") {
		t.Errorf("error names no line: %v", err)
	}
}

// TestJudgementRoundTripBothForms pins that the two template forms produce the
// same labels, so a judge may fill in whichever file they prefer.
func TestJudgementRoundTripBothForms(t *testing.T) {
	t.Parallel()
	pooled, _ := buildFixture(t, fixtureSeed, 5)
	blocks := fixtureBlocks()
	dir := t.TempDir()

	jsonl, err := RenderTemplateJSONL(pooled, blocks, 200)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	jPath := filepath.Join(dir, "judged.jsonl")
	if err := os.WriteFile(jPath, []byte(strings.ReplaceAll(string(jsonl), `"judgement":""`, `"judgement":"1"`)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mPath := filepath.Join(dir, "judged.md")
	md := strings.ReplaceAll(string(RenderTemplateMarkdown(pooled, blocks, 200)), "| "+UnjudgedMark+" |", "| y |")
	if err := os.WriteFile(mPath, []byte(md), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fromJSONL, err := ParseJudgements(jPath)
	if err != nil {
		t.Fatalf("parse jsonl: %v", err)
	}
	fromMD, err := ParseJudgements(mPath)
	if err != nil {
		t.Fatalf("parse markdown: %v", err)
	}
	if len(fromJSONL) != len(pooled) || len(fromMD) != len(pooled) {
		t.Fatalf("cases: jsonl=%d markdown=%d, want %d", len(fromJSONL), len(fromMD), len(pooled))
	}
	for k, js := range fromJSONL {
		if len(fromMD[k]) != len(js) {
			t.Fatalf("case %s: %d markdown rows vs %d jsonl rows", k, len(fromMD[k]), len(js))
		}
		for i := range js {
			if js[i].BlockID != fromMD[k][i].BlockID || js[i].Relevant != fromMD[k][i].Relevant {
				t.Fatalf("case %s row %d: forms disagree", k, i)
			}
		}
	}
}

func TestApplyLabelsCountsNoRelevantAndKeepsTheCase(t *testing.T) {
	t.Parallel()
	pooled, _ := buildFixture(t, fixtureSeed, 5)
	relevant := map[string]bool{}
	for _, blockID := range pooled[0].BlockIDs {
		relevant[blockID] = true
	}
	judged := synthesise(pooled, relevant)

	cases := fixtureCases()
	labelled, st, err := ApplyLabels(cases, judged)
	if err != nil {
		t.Fatalf("ApplyLabels: %v", err)
	}
	if len(labelled) != len(cases) {
		t.Fatalf("%d cases out, %d in — a case was dropped", len(labelled), len(cases))
	}
	if st.NoRelevant != 2 || st.Labelled != 1 {
		t.Fatalf("labelled=%d no_relevant=%d, want 1/2", st.Labelled, st.NoRelevant)
	}
	if st.PoolP50 != 8 || st.PoolMax != 8 {
		t.Fatalf("pool p50=%d max=%d, want 8/8", st.PoolP50, st.PoolMax)
	}
	if len(labelled[0].GoldIDs) != 8 {
		t.Fatalf("case 0 got %d labels, want 8", len(labelled[0].GoldIDs))
	}
	for i := 1; i < len(labelled); i++ {
		if len(labelled[i].GoldIDs) != 0 {
			t.Fatalf("case %d should carry no label", i)
		}
	}
}

func TestApplyLabelsRefusesAPartialFile(t *testing.T) {
	t.Parallel()
	pooled, _ := buildFixture(t, fixtureSeed, 5)
	judged := synthesise(pooled[:2], map[string]bool{})
	if _, _, err := ApplyLabels(fixtureCases(), judged); err == nil {
		t.Fatal("a judgement file missing a case was accepted")
	}
}

func TestMergeStampKeepsForeignFields(t *testing.T) {
	t.Parallel()
	stamp := filepath.Join(t.TempDir(), FileStamp)
	before := `{
	  "version": 1,
	  "corpus_max_created_at": "2026-08-01T00:00:00.000000Z",
	  "future_field_from_a_later_wave": {"deep": [1, 2]},
	  "slices": {
	    "G-KI": {"n": 300, "file": "g-ki.jsonl"},
	    "G-REAL": {"n": 150, "discarded_redaction": 4, "own_odd_key": "keep me"}
	  }
	}`
	if err := os.WriteFile(stamp, []byte(before), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := MergeStampSlice(stamp, SliceReal, map[string]any{"labelled": 148, "control_hit_rate": 0.4}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	raw, err := os.ReadFile(stamp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc["future_field_from_a_later_wave"]; !ok {
		t.Error("a foreign top-level field was dropped by the merge")
	}
	sl := doc["slices"].(map[string]any)
	if _, ok := sl["G-KI"]; !ok {
		t.Error("a foreign slice profile was dropped by the merge")
	}
	real := sl[SliceReal].(map[string]any)
	for _, k := range []string{"discarded_redaction", "own_odd_key", "labelled", "control_hit_rate"} {
		if _, ok := real[k]; !ok {
			t.Errorf("field %q missing after merge", k)
		}
	}
	if real["n"].(float64) != 150 {
		t.Errorf("n changed to %v", real["n"])
	}
}

func TestBackupFileKeepsThePreIngestSlice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, FileReal)
	if err := os.WriteFile(path, []byte("{\"slice\":\"G-REAL\"}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dst, err := BackupFile(path, "20260826")
	if err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if filepath.Base(dst) != FileReal+".bak-20260826" {
		t.Fatalf("backup named %q", filepath.Base(dst))
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v, want 0600", info.Mode().Perm())
	}
}

// TestWritersForceOwnerOnlyMode pins that rewriting an already world-readable
// file does not leave it that way — os.WriteFile applies its mode only on
// creation, and every artefact here carries private data or the answer key.
func TestWritersForceOwnerOnlyMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pooled, key := buildFixture(t, fixtureSeed, 5)

	paths := map[string]func(string) error{
		"judge.jsonl": nil,
		"judge.md":    nil,
		"key.json":    func(p string) error { return WritePoolKey(p, key) },
		FileStamp:     func(p string) error { return MergeStampSlice(p, SliceReal, map[string]any{"labelled": 1}) },
	}
	for name := range paths {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if err := WriteTemplate(filepath.Join(dir, "judge.jsonl"), filepath.Join(dir, "judge.md"),
		pooled, fixtureBlocks(), 200); err != nil {
		t.Fatalf("WriteTemplate: %v", err)
	}
	for name, write := range paths {
		if write != nil {
			if err := write(filepath.Join(dir, name)); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s left at %v, want 0600", name, info.Mode().Perm())
		}
	}
}

// TestGuardRefusesTemplatePathsOutside pins that the template and its key
// cannot be written beside the gold directory by a relative escape.
func TestGuardRefusesTemplatePathsOutside(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), DirName)
	g, err := NewGuard(root, false)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if _, err := g.Resolve("judge-abc.jsonl"); err != nil {
		t.Fatalf("a plain name inside the root was refused: %v", err)
	}
	for _, escape := range []string{"../judge-abc.jsonl", "/etc/judge-abc.jsonl", "../../pool-key-abc.json"} {
		if _, err := g.Resolve(escape); !errors.Is(err, ErrOutsideGoldset) {
			t.Errorf("%q was not refused: %v", escape, err)
		}
	}
}

func TestBuildPoolRefusesACaseWithoutAPoolEntry(t *testing.T) {
	t.Parallel()
	cases := fixtureCases()
	if _, _, err := BuildPool(cases, fixtureEntries(cases)[:1], fixtureControlPool(), 5, fixtureSeed); err == nil {
		t.Fatal("a case without a pool entry was templated anyway")
	}
}

// synthesise builds judgements straight from the pooled cases, marking the ids
// in `relevant` as relevant. It stands in for the human step this wave does not
// perform.
func synthesise(pooled []PooledCase, relevant map[string]bool) map[string][]Judgement {
	out := map[string][]Judgement{}
	for _, p := range pooled {
		for _, blockID := range p.BlockIDs {
			out[p.Key()] = append(out[p.Key()], Judgement{
				Slice: p.Slice, Index: p.Index, QuerySHA256: p.QuerySHA256,
				BlockID: blockID, Relevant: relevant[blockID],
			})
		}
	}
	return out
}
