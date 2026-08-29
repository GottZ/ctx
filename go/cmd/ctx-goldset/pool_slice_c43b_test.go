package main

// Wave C4-3b — `ctx-goldset pool` builds the template of the CHOSEN judged
// slice (design/05a §C3-2-D05-8 k, Voraussetzung für C3-4b).
//
// Befund N1 of the C4-3a report (reports/bau/c4-3-buildpool.md): C4-3a gave
// G-GLOB a pool, but the tool that turns a pool into a judgement template still
// read `g-real.jsonl` at pool.go:48, stamped `SliceReal` at pool.go:199 and
// titled every markdown template "G-REAL". The pool existed and nothing could
// read it.
//
// RED before this wave: the package does not compile — resolvePoolSlice,
// poolInputs, artefactID and poolOpts.slice do not exist. The CLI-level red is
// `ctx-goldset pool -slice glob` answering "flag provided but not defined:
// -slice" with exit 2.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/goldset"
)

// ------------------------------------------------------- flag semantics.

// TestPoolSliceFlagDefaultsToReal pins the byte-compatibility promise at its
// source: an unset flag is G-REAL out of g-real.jsonl, which is what the
// pre-C4-3b command had hard-wired.
func TestPoolSliceFlagDefaultsToReal(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "real", "REAL", "g-real", "G-REAL", "  G-Real  "} {
		slice, file, err := resolvePoolSlice(raw)
		if err != nil {
			t.Errorf("resolvePoolSlice(%q): %v", raw, err)
			continue
		}
		if slice != goldset.SliceReal || file != goldset.FileReal {
			t.Errorf("resolvePoolSlice(%q) = (%q, %q), want (%q, %q)",
				raw, slice, file, goldset.SliceReal, goldset.FileReal)
		}
	}
}

// TestPoolSliceFlagSelectsGlob is the gate of the wave.
func TestPoolSliceFlagSelectsGlob(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"glob", "GLOB", "g-glob", "G-GLOB", " Glob "} {
		slice, file, err := resolvePoolSlice(raw)
		if err != nil {
			t.Errorf("resolvePoolSlice(%q): %v", raw, err)
			continue
		}
		if slice != goldset.SliceGlob || file != goldset.FileGlob {
			t.Errorf("resolvePoolSlice(%q) = (%q, %q), want (%q, %q)",
				raw, slice, file, goldset.SliceGlob, goldset.FileGlob)
		}
	}
}

// TestPoolSliceRefusesAnUnpooledSlice is the negative probe. A slice whose gold
// is CONSTRUCTIVE has no pool, so a template for it would be a file full of
// candidates nobody ever pooled — or, worse, an ingest that overwrites
// constructive gold with judgements. The refusal names the slice and the
// alternatives; it never answers with an empty template.
func TestPoolSliceRefusesAnUnpooledSlice(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"glob-konstr", "G-GLOB-KONSTR", "ki", "q", "sess", "mh"} {
		slice, file, err := resolvePoolSlice(raw)
		if err == nil {
			t.Errorf("resolvePoolSlice(%q) = (%q, %q), want an error — that slice is not pooled",
				raw, slice, file)
			continue
		}
		for _, want := range []string{goldset.SliceReal, goldset.SliceGlob} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("resolvePoolSlice(%q) error %q nennt %q nicht", raw, err, want)
			}
		}
	}
}

// TestPoolSliceRefusesAnUnknownSlice separates the two failure modes: a name
// the registry does not know at all is a typo, and a typo that silently fell
// back to G-REAL would build the wrong template under the right-looking name.
func TestPoolSliceRefusesAnUnknownSlice(t *testing.T) {
	t.Parallel()
	if _, _, err := resolvePoolSlice("quatsch"); err == nil {
		t.Fatal("resolvePoolSlice(\"quatsch\") succeeded, want an error")
	} else if !strings.Contains(err.Error(), "G-QUATSCH") {
		t.Errorf("error %q nennt den unbekannten Slice nicht", err)
	}
}

// ------------------------------------------------------- the glob template.

// TestPoolBuildsTheGlobTemplate is the wave's gate: -slice glob produces a
// template whose every row is a G-GLOB row, read out of g-glob.jsonl, under an
// artefact name that cannot collide with the G-REAL template of the same run.
func TestPoolBuildsTheGlobTemplate(t *testing.T) {
	g := c43bGold(t)
	o := poolOpts{control: 5, excerpt: c43bExcerpt, slice: "glob"}
	c43bEmit(t, g, &o)

	base := "judge-glob-" + c43bRunID
	jsonl := c43bRead(t, g, base+".jsonl")
	for n, line := range strings.Split(strings.TrimSpace(string(jsonl)), "\n") {
		var row struct {
			Kind  string `json:"kind"`
			Slice string `json:"slice"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("Zeile %d: %v", n+1, err)
		}
		if row.Slice != goldset.SliceGlob {
			t.Errorf("Zeile %d (%s): slice = %q, want %q", n+1, row.Kind, row.Slice, goldset.SliceGlob)
		}
	}

	md := string(c43bRead(t, g, base+".md"))
	if !strings.HasPrefix(md, "# Relevanz-Urteile "+goldset.SliceGlob+"\n") {
		t.Errorf("die Markdown-Vorlage nennt den falschen Slice — erste Zeile: %q",
			strings.SplitN(md, "\n", 2)[0])
	}

	// The key belongs to THIS template, and judge -llm finds it the way it
	// finds the G-REAL one: by deriving the name from the template's own name.
	keyName := keyPrefix + "glob-" + c43bRunID + ".json"
	if _, err := os.Stat(filepath.Join(g.Root(), keyName)); err != nil {
		t.Fatalf("Kontroll-Schlüssel %s fehlt: %v", keyName, err)
	}
	got, err := resolveKey(g, "", base+".jsonl")
	if err != nil {
		t.Fatalf("resolveKey: %v", err)
	}
	if filepath.Base(got) != keyName {
		t.Errorf("resolveKey = %q, want %q — judge -llm fände den Schlüssel nicht",
			filepath.Base(got), keyName)
	}
	key, err := goldset.ReadPoolKey(got)
	if err != nil {
		t.Fatalf("ReadPoolKey: %v", err)
	}
	if key.PoolRunID != c43bRunID {
		t.Errorf("pool_run_id = %q, want %q — der Schlüssel muss den POOL-Lauf nennen, "+
			"nicht den Artefaktnamen", key.PoolRunID, c43bRunID)
	}
}

// TestPoolTemplatesOfOneRunDoNotCollide is the artefact-name gate. Both judged
// slices are pooled in ONE priming run and therefore share a run id; if both
// templates defaulted to judge-<run>.jsonl, building the second would overwrite
// the first template AND its control key — silently, because both writes
// succeed.
//
// It doubles as the second half of the byte-compatibility proof: the G-REAL
// artefacts built through the NEW resolution path are compared against the
// goldens the OLD command produced.
func TestPoolTemplatesOfOneRunDoNotCollide(t *testing.T) {
	g := c43bGold(t)
	real0 := poolOpts{control: 5, excerpt: c43bExcerpt}
	glob0 := poolOpts{control: 5, excerpt: c43bExcerpt, slice: "glob"}
	c43bEmit(t, g, &real0)
	c43bEmit(t, g, &glob0)

	for _, name := range []string{
		"judge-" + c43bRunID + ".jsonl", "judge-" + c43bRunID + ".md",
		keyPrefix + c43bRunID + ".json",
	} {
		c43bCompareGolden(t, name, c43bRead(t, g, name))
	}
	for _, name := range []string{
		"judge-glob-" + c43bRunID + ".jsonl", "judge-glob-" + c43bRunID + ".md",
		keyPrefix + "glob-" + c43bRunID + ".json",
	} {
		if _, err := os.Stat(filepath.Join(g.Root(), name)); err != nil {
			t.Errorf("%s fehlt: %v", name, err)
		}
	}
}

// TestPoolRefusesAPoolFileWithoutTheSlice covers the vintage case: a pool file
// written BEFORE wave C4-3a holds G-REAL entries only. Without this guard the
// run dies on the first case with "no pool entry for case G-GLOB/0/<digest>",
// which points at the case instead of at the file that is too old.
func TestPoolRefusesAPoolFileWithoutTheSlice(t *testing.T) {
	g := c43bGold(t)
	name := "pool-" + c43bRunID + ".jsonl"
	entries := []goldset.PoolEntry{c43bEntry(c43bCase(goldset.SliceReal, 0), 0)}
	c43bWritePool(t, g, name, entries)

	o := poolOpts{control: 5, excerpt: c43bExcerpt, slice: "glob"}
	_, _, _, err := poolInputs(g, &o)
	if err == nil {
		t.Fatal("poolInputs auf einer Pool-Datei ohne G-GLOB-Einträge war erfolgreich, want Fehler")
	}
	for _, want := range []string{goldset.SliceGlob, name, "prime"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Fehlertext %q nennt %q nicht", err, want)
		}
	}
}

// ------------------------------------------------------------ the ingest.

// TestIngestStampsTheJudgedSlice closes the round trip: a filled G-GLOB
// template must label g-glob.jsonl and land in the G-GLOB profile of the stamp.
// Before this wave stampIngest wrote SliceReal unconditionally — the G-GLOB
// figures would have overwritten the G-REAL profile of the standing C3-4a run.
func TestIngestStampsTheJudgedSlice(t *testing.T) {
	g := c43bGold(t)
	o := poolOpts{control: 5, excerpt: c43bExcerpt, slice: "glob"}
	c43bEmit(t, g, &o)

	judged := "judge-glob-" + c43bRunID + ".jsonl"
	c43bFillTemplate(t, g, judged)

	c := &common{dir: g.Root()}
	if err := cmdIngest(c, judged, "", goldset.FileGlob, goldset.FileStamp); err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}

	var stamp struct {
		Slices map[string]map[string]any `json:"slices"`
	}
	if err := json.Unmarshal(c43bRead(t, g, goldset.FileStamp), &stamp); err != nil {
		t.Fatalf("STAMP.json: %v", err)
	}
	entry, ok := stamp.Slices[goldset.SliceGlob]
	if !ok {
		t.Fatalf("STAMP.json trägt kein %s-Profil, sondern %v", goldset.SliceGlob, c43bKeys(stamp.Slices))
	}
	if got := entry["file"]; got != goldset.FileGlob {
		t.Errorf("Profil-Feld file = %v, want %q", got, goldset.FileGlob)
	}
	if _, wrong := stamp.Slices[goldset.SliceReal]; wrong {
		t.Errorf("der G-GLOB-Einlesevorgang hat ein %s-Profil geschrieben — genau der Fehler, "+
			"den diese Welle schließt", goldset.SliceReal)
	}

	cases, err := goldset.ReadJSONL(filepath.Join(g.Root(), goldset.FileGlob))
	if err != nil {
		t.Fatalf("g-glob.jsonl: %v", err)
	}
	for _, gc := range cases {
		if len(gc.GoldIDs) == 0 {
			t.Errorf("%s #%d hat kein Gold bekommen", gc.Slice, gc.Index)
		}
	}
}

// TestIngestRefusesAConstructiveSlice is the ingest-side negative probe. The
// slice file is chosen by a flag, and -out g-glob-konstr.jsonl would replace
// gold taken from graph_cluster_member with pooled judgements — a floor check
// silently turned into a judged slice.
func TestIngestRefusesAConstructiveSlice(t *testing.T) {
	g := c43bGold(t)
	o := poolOpts{control: 5, excerpt: c43bExcerpt, slice: "glob"}
	c43bEmit(t, g, &o)
	judged := "judge-glob-" + c43bRunID + ".jsonl"
	c43bFillTemplate(t, g, judged)

	before := c43bRead(t, g, goldset.FileGlobKonstr)
	c := &common{dir: g.Root()}
	err := cmdIngest(c, judged, "", goldset.FileGlobKonstr, goldset.FileStamp)
	if err == nil {
		t.Fatal("cmdIngest in einen konstruktiv gelabelten Slice war erfolgreich, want Fehler")
	}
	if !strings.Contains(err.Error(), goldset.SliceGlobKonstr) {
		t.Errorf("Fehlertext %q nennt den Slice nicht", err)
	}
	if after := c43bRead(t, g, goldset.FileGlobKonstr); string(after) != string(before) {
		t.Error("die Slice-Datei wurde trotz Abbruch verändert")
	}
}

// ----------------------------------------------------------------- Hilfen.

// c43bEmit runs the pooling pipeline of cmdPool minus its two database calls.
func c43bEmit(t *testing.T, g *goldset.Guard, o *poolOpts) {
	t.Helper()
	cases, entries, runID, err := poolInputs(g, o)
	if err != nil {
		t.Fatalf("poolInputs: %v", err)
	}
	pooled, key, err := goldset.BuildPool(cases, entries, c43bControlPool(), o.control, c43bSeed)
	if err != nil {
		t.Fatalf("BuildPool: %v", err)
	}
	key.PoolRunID, key.CreatedAt = runID, c43bCreatedAt
	ids := allCandidateIDs(pooled)
	blocks := c43bBlocksOf(ids)
	if err := emitTemplate(g, *o, pooled, blocks, key, runID, len(ids)-len(blocks)); err != nil {
		t.Fatalf("emitTemplate: %v", err)
	}
}

// c43bRead reads one artefact out of the gold directory.
func c43bRead(t *testing.T, g *goldset.Guard, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(g.Root(), name)) //nolint:gosec // G304: path built from the test's own guard root
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// c43bFillTemplate answers "relevant" on every open cell — the shape of a
// filled template, not a plausible judgement.
func c43bFillTemplate(t *testing.T, g *goldset.Guard, name string) {
	t.Helper()
	p := filepath.Join(g.Root(), name)
	b := c43bRead(t, g, name)
	filled := strings.ReplaceAll(string(b), `"judgement":""`, `"judgement":"1"`)
	if filled == string(b) {
		t.Fatalf("%s trägt keine offene Urteilsspalte", name)
	}
	if err := os.WriteFile(p, []byte(filled), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// c43bKeys names the slices a stamp carries, for a readable failure.
func c43bKeys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
