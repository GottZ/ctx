package goldbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v1DumpReader ist der Bestands-Leser (label-thinking.py / Bias-Audit-
// Re-Scoring): liest ausschließlich axis/id/outputs und verlangt alle drei —
// ein axis-loses Record ist rot. Läuft unverändert über v1- UND v2-Dumps
// (design/02 §7 KW2: Abwärtskompatibilität ist das Gate, nicht die Behauptung).
func v1DumpReader(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var rec struct {
			Axis    *string  `json:"axis"`
			ID      *string  `json:"id"`
			Outputs []string `json:"outputs"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return n, err
		}
		if rec.Axis == nil || rec.ID == nil || rec.Outputs == nil {
			return n, errors.New("record ohne axis/id/outputs")
		}
		n++
	}
	return n, sc.Err()
}

// TestDumpV2RoundTrip pinnt Dump-v2 (design/02 §3.3/§7 KW2): der Bestands-
// Leser läuft grün über den v2-Dump, ein axis-loses Record macht ihn rot,
// system/user/params/usage sind parallel zu outputs indiziert (sensitivity:
// zwei Slots), der gen-Stempel steht je Zeile, und die Datei ist 0600.
func TestDumpV2RoundTrip(t *testing.T) {
	srv := fakeChatServer(t)
	defer srv.Close()

	dir := t.TempDir()
	dump := filepath.Join(dir, "dump.jsonl")
	old := setUmask(0o000)
	t.Cleanup(func() { setUmask(old) })

	gen := &GenStamp{Engine: "fake", EngineVersion: "0", Image: "img@sha256:0", TemplateSHA256: "t"}
	// Vorhandene 0644-Datei am Dump-Pfad (Bestands-Dumps aus regen.sh sind
	// 0644): nach dem Lauf MUSS sie 0600 sein — der Modus im OpenFile greift
	// nur bei Neuanlage, der Chmod danach ist die eigentliche Zusicherung.
	if err := os.WriteFile(dump, []byte("alt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DataDir: testDataDir(t), Endpoint: srv.URL, Model: "fake-model",
		N: 1, Concurrency: 2, Seed: 20260812, TimeoutSec: 30,
		DumpOutputs: dump, GenStamp: gen, MaxTokensMult: 2, TempOverride: -1,
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	st, err := os.Stat(dump)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("dump must be 0600 regardless of umask/pre-existing mode, got %04o", st.Mode().Perm())
	}
	// Symlink am Dump-Pfad wird NICHT gefolgt.
	target := filepath.Join(dir, "target.jsonl")
	link := filepath.Join(dir, "link.jsonl")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := dumpOutputs(link, nil, nil, nil); err == nil {
		t.Fatal("dump via symlink must be refused (O_NOFOLLOW)")
	}

	// Bestands-Leser über den v2-Dump: grün, eine Zeile je Achse (N=1).
	n, err := v1DumpReader(dump)
	if err != nil {
		t.Fatalf("v1 reader over v2 dump: %v", err)
	}
	if n != len(Axes) {
		t.Fatalf("v1 reader: %d records, want %d", n, len(Axes))
	}

	// v2-Felder: parallel indiziert, gen je Zeile, params = effektiv gesendet.
	f, err := os.Open(dump)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	// Basis-Budgets je Achse aus dem echten Builder (Slot 0): params müssen
	// EXAKT das Doppelte tragen (MaxTokensMult 2) — Parität allein wäre eine
	// tote Probe (alle Achsen-Budgets sind gerade).
	registry := axisRegistry()
	baseBudget := func(axis, id string) int {
		cases, err := LoadCases(testDataDir(t), axis)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			if c.ID == id {
				reqs, err := registry[axis].build(c)
				if err != nil {
					t.Fatal(err)
				}
				return reqs[0].Opts.MaxTokens
			}
		}
		t.Fatalf("case %s/%s not found", axis, id)
		return 0
	}
	seen := map[string]int{}
	for sc.Scan() {
		var rec dumpRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		k := len(rec.Outputs)
		if len(rec.System) != k || len(rec.User) != k || len(rec.Params) != k || len(rec.Usage) != k {
			t.Fatalf("%s/%s: slots not parallel: out=%d sys=%d user=%d params=%d usage=%d",
				rec.Axis, rec.ID, k, len(rec.System), len(rec.User), len(rec.Params), len(rec.Usage))
		}
		if rec.Gen == nil || rec.Gen.Engine != "fake" {
			t.Fatalf("%s: gen stamp missing", rec.Axis)
		}
		for i := range k {
			if rec.System[i] == "" && rec.User[i] == "" {
				t.Fatalf("%s slot %d: prompt bodies missing", rec.Axis, i)
			}
			if i == 0 {
				if want := applyBudgetMult(baseBudget(rec.Axis, rec.ID), 2); want == 0 || rec.Params[i].MaxTokens != want {
					// MaxTokensMult=2 wurde angewendet ⇒ params sind die EFFEKTIVEN Werte.
					t.Fatalf("%s slot %d: params not effective: max_tokens=%d want %d", rec.Axis, i, rec.Params[i].MaxTokens, want)
				}
			}
			u := rec.Usage[i]
			if u.Err != "" {
				t.Fatalf("%s slot %d: unexpected transport error %q", rec.Axis, i, u.Err)
			}
			// Fake-Server liefert usage + finish_reason: die Werte MÜSSEN ankommen.
			if u.Prompt < 100 || u.Completion != 7 || u.Reasoning != 3 || u.Finish != "stop" {
				t.Fatalf("%s slot %d: usage not carried: %+v", rec.Axis, i, u)
			}
			if rec.Axis == "title" && !u.ThinkStripped {
				t.Fatalf("title: client strip signal missing in usage")
			}
			if rec.Axis != "title" && u.ThinkStripped {
				t.Fatalf("%s: think_stripped must be false", rec.Axis)
			}
		}
		seen[rec.Axis] = k
	}
	if seen["sensitivity"] != 2 {
		t.Fatalf("sensitivity must carry 2 request slots, got %d", seen["sensitivity"])
	}

	// Dry-Run-Dump: keine usage-Slots (Leser unterscheidet Dry-Run von Nullwerten).
	dry := filepath.Join(dir, "dry.jsonl")
	if _, err := Run(context.Background(), Config{DataDir: testDataDir(t), DryRun: true, N: 1, Seed: 1, DumpOutputs: dry}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if b, _ := os.ReadFile(dry); strings.Contains(string(b), `"usage"`) {
		t.Fatal("dry-run dump must not carry usage slots")
	}

	// Echte v1-Bestands-Dumps (privates Submodule; Skip, wenn nicht vorhanden).
	if v1s, _ := filepath.Glob(filepath.Join("..", "..", "..", ".project", "bench-goldbench-2026-08-12", "dumps", "dump-*.jsonl")); len(v1s) > 0 {
		for _, p := range v1s {
			if n, err := v1DumpReader(p); err != nil || n == 0 {
				t.Fatalf("v1 reader over real v1 dump %s: n=%d err=%v", filepath.Base(p), n, err)
			}
		}
	} else {
		t.Log("keine v1-Bestands-Dumps gefunden — Bein übersprungen")
	}

	// Rot-Probe des Lesers: ein axis-loses Record ⇒ Fehler.
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte(`{"id":"x","outputs":["y"]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := v1DumpReader(bad); err == nil {
		t.Fatal("v1 reader must reject a record without axis")
	}
}

// TestDumpV1ByteShape: ohne Requests/Stempel ist die Zeile die v1-Form.
func TestDumpV1ByteShape(t *testing.T) {
	r := caseRun{c: &Case{ID: "c1"}, outputs: []string{"o"}}
	b, err := json.Marshal(newDumpRecord("title", r, nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"axis":"title","id":"c1","outputs":["o"]}` {
		t.Fatalf("v1 shape drifted: %s", b)
	}
}
