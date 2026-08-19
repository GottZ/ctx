package goldbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	cfg := Config{
		DataDir: testDataDir(t), Endpoint: srv.URL, Model: "fake-model",
		N: 1, Concurrency: 2, Seed: 20260812, TimeoutSec: 30,
		DumpOutputs: dump, GenStamp: gen, MaxTokensMult: 2,
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	st, err := os.Stat(dump)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("dump must be 0600 regardless of umask, got %04o", st.Mode().Perm())
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
			if rec.Params[i].MaxTokens%2 != 0 || rec.Params[i].MaxTokens == 0 {
				// MaxTokensMult=2 wurde angewendet ⇒ params sind die EFFEKTIVEN Werte.
				t.Fatalf("%s slot %d: params not effective (max_tokens=%d)", rec.Axis, i, rec.Params[i].MaxTokens)
			}
			if rec.Usage[i].Err != "" {
				t.Fatalf("%s slot %d: unexpected transport error %q", rec.Axis, i, rec.Usage[i].Err)
			}
		}
		seen[rec.Axis] = k
	}
	if seen["sensitivity"] != 2 {
		t.Fatalf("sensitivity must carry 2 request slots, got %d", seen["sensitivity"])
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
