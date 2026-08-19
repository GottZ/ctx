package goldbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// countLines zählt nicht-leere JSONL-Zeilen und prüft (axis,id)-Eindeutigkeit.
func countLines(t *testing.T, path string) (n int, dups int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var rec struct {
			Axis string `json:"axis"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("bad line: %v", err)
		}
		n++
		if seen[rec.Axis+"\x00"+rec.ID] {
			dups++
		}
		seen[rec.Axis+"\x00"+rec.ID] = true
	}
	return n, dups
}

// TestDumpAppendResume pinnt KW3 (design/02 §4.3 + §7): ROT (a) der alte
// End-of-Run-Writer vernichtet beim Doppel-Lauf den ersten Dump und callt alles
// neu; ROT (b) Abbruch mitten im Lauf hinterlässt ohne -dump-append keine
// Zeile; GRÜN: mit -dump-append überlebt jeder fertige Fall den Abbruch, der
// identische Resume-Aufruf ergänzt nur die fehlenden Fälle (0 Duplikate, nur
// fehlende gecallt) und ein Doppel-Lauf callt gar nichts mehr; Stamp-Probe:
// Datei mit fremdem gen-Stempel ⇒ ErrDumpStamp statt Append.
func TestDumpAppendResume(t *testing.T) {
	inner := fakeChatServer(t)
	defer inner.Close()
	var calls atomic.Int64
	var cancelAfter atomic.Int64 // >0: nach so vielen Calls ctx abbrechen
	var cancelFn atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if lim := cancelAfter.Load(); lim > 0 && n > lim {
			if c, ok := cancelFn.Load().(context.CancelFunc); ok {
				c()
			}
			http.Error(w, "abgebrochen", http.StatusServiceUnavailable)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	gen := &GenStamp{Engine: "fake", EngineVersion: "0", Image: "img@sha256:0", TemplateSHA256: "t"}
	base := Config{
		DataDir: testDataDir(t), Endpoint: srv.URL, Model: "fake-model",
		N: 1, Concurrency: 4, Seed: 20260812, TimeoutSec: 30, GenStamp: gen, TempOverride: -1,
	}
	run := func(ctx context.Context, dump string, appendMode bool) (*Report, error) {
		cfg := base
		cfg.DumpOutputs, cfg.DumpAppend = dump, appendMode
		return Run(ctx, cfg)
	}

	// Referenz: voller Lauf, n Fälle, c Calls.
	ref := filepath.Join(dir, "ref.jsonl")
	refRep, err := run(context.Background(), ref, false)
	if err != nil {
		t.Fatal(err)
	}
	nAll, _ := countLines(t, ref)
	cAll := calls.Load()
	if nAll == 0 || cAll == 0 {
		t.Fatalf("Referenz leer: n=%d calls=%d", nAll, cAll)
	}

	// ROT (a): alter Writer, Doppel-Lauf ⇒ wieder n Zeilen (Lauf 2 ersetzt Lauf 1) und ALLE Calls erneut.
	calls.Store(0)
	if _, err := run(context.Background(), ref, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := countLines(t, ref); n != nAll || calls.Load() != cAll {
		t.Fatalf("legacy double run: lines=%d calls=%d (want %d/%d — truncate + full re-call)", n, calls.Load(), nAll, cAll)
	}

	// ROT (b): Abbruch nach der Hälfte der Calls, alter Writer ⇒ keine Zeile.
	legacy := filepath.Join(dir, "legacy.jsonl")
	half := cAll / 2
	ctx, cancel := context.WithCancel(context.Background())
	cancelFn.Store(cancel)
	cancelAfter.Store(half)
	calls.Store(0)
	if _, err := run(ctx, legacy, false); err == nil {
		t.Fatal("aborted legacy run must return an error")
	}
	cancel()
	if n, _ := countLines(t, legacy); n != 0 {
		t.Fatalf("legacy end-of-run dump must not survive an abort, got %d lines", n)
	}

	// GRÜN: -dump-append, gleicher Abbruch ⇒ fertige Fälle stehen in der Datei.
	app := filepath.Join(dir, "append.jsonl")
	ctx, cancel = context.WithCancel(context.Background())
	cancelFn.Store(cancel)
	calls.Store(0)
	if _, err := run(ctx, app, true); err == nil {
		t.Fatal("aborted append run must return an error")
	}
	cancel()
	nPart, dups := countLines(t, app)
	if nPart == 0 || nPart >= nAll || dups != 0 {
		t.Fatalf("append after abort: lines=%d (want 0<n<%d) dups=%d", nPart, nAll, dups)
	}
	// Resume = identischer Aufruf ⇒ nur fehlende Fälle, 0 Duplikate, Report vollständig.
	cancelAfter.Store(0)
	calls.Store(0)
	rep, err := run(context.Background(), app, true)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	nRes, dups := countLines(t, app)
	if nRes != nAll || dups != 0 {
		t.Fatalf("resume: lines=%d dups=%d (want %d/0)", nRes, dups, nAll)
	}
	if c := calls.Load(); c >= cAll || c == 0 {
		t.Fatalf("resume must call only the missing cases: calls=%d (full=%d)", c, cAll)
	}
	if rep == nil || len(rep.Axes) != len(Axes) {
		t.Fatalf("resume report incomplete: %+v", rep)
	}
	// Parität: ein resumed Report scored wie der Volllauf (adopt trägt Outputs UND usage) …
	if rep.Composite != refRep.Composite || rep.FailStats != refRep.FailStats {
		t.Fatalf("resume report must equal full run: composite %v vs %v, fail_stats %+v vs %+v", rep.Composite, refRep.Composite, rep.FailStats, refRep.FailStats)
	}
	for ax, r := range rep.Axes {
		if r.PrimaryScore != refRep.Axes[ax].PrimaryScore {
			t.Fatalf("axis %s: resume score %v != full %v", ax, r.PrimaryScore, refRep.Axes[ax].PrimaryScore)
		}
	}
	// … und ist als Resume gestempelt (Durchsatz misst nur den Restlauf).
	if rep.Env.ResumedCases != nPart || rep.Env.ExecutedCases != nAll-nPart {
		t.Fatalf("env resume stamp: resumed=%d executed=%d (want %d/%d)", rep.Env.ResumedCases, rep.Env.ExecutedCases, nPart, nAll-nPart)
	}
	// Doppel-Lauf mit -dump-append ⇒ 0 Calls, Datei unverändert.
	before, _ := os.ReadFile(app)
	calls.Store(0)
	if _, err := run(context.Background(), app, true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(app)
	if calls.Load() != 0 || string(before) != string(after) {
		t.Fatalf("append double run: calls=%d changed=%v (want 0/false)", calls.Load(), string(before) != string(after))
	}
	// Stamp-Probe: fremder Live-Stempel gegen die bestehende Datei ⇒ Abbruch, Datei unverändert.
	cfg := base
	cfg.DumpOutputs, cfg.DumpAppend = app, true
	cfg.GenStamp = &GenStamp{Engine: "other", EngineVersion: "1", Image: "img@sha256:1", TemplateSHA256: "u"}
	if _, err := Run(context.Background(), cfg); !errors.Is(err, ErrDumpStamp) {
		t.Fatalf("foreign gen stamp must abort with ErrDumpStamp, got %v", err)
	}
	cfg.GenStamp = nil
	if _, err := Run(context.Background(), cfg); err == nil || errors.Is(err, ErrDumpStamp) {
		t.Fatalf("dump-append without gen stamp must be refused before reading, got %v", err)
	}
	// Dry-Run + -dump-append schreibt NIE (Review F1: der End-of-Run-Writer würde truncaten).
	cfg.GenStamp = gen
	cfg.DryRun = true
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("dry-run with append: %v", err)
	}
	cfg.DryRun = false
	after2, _ := os.ReadFile(app)
	if string(after2) != string(after) {
		t.Fatal("stamp abort / dry-run must not touch the file")
	}
	// Resume-Dump ist v1-lesbar.
	if n, err := v1DumpReader(app); err != nil || n != nAll {
		t.Fatalf("v1 reader over append dump: n=%d err=%v", n, err)
	}
}

// TestDumpAppendLoaderGates pinnt die Datei-seitigen Gates des Resume (Review KW3 F2/F4/F7/F8 +
// Doku-Zusagen): gescheiterte Legacy-Records zählen nicht als erledigt; eine abgerissene
// Schlusszeile wird toleriert und abgeschnitten; zwei vollständige Records derselben (axis,id)
// und Zeilen ohne axis/id sind rot; eine zweite Instanz auf derselben Datei bekommt
// ErrDumpLocked; ein Record mit falscher Slot-Zahl ist rot.
func TestDumpAppendLoaderGates(t *testing.T) {
	gen := &GenStamp{Engine: "fake", EngineVersion: "0", Image: "img@sha256:0", TemplateSHA256: "t"}
	line := func(axis, id string, outputs []string, usage []CallUsage) string {
		b, _ := json.Marshal(dumpRecord{Axis: axis, ID: id, Outputs: outputs, Usage: usage, Gen: gen})
		return string(b) + "\n"
	}
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// F2: Fehl-Record (usage.err / leerer Output) ⇒ nicht done; späterer vollständiger gewinnt.
	p := write("legacy.jsonl",
		line("title", "a", []string{""}, []CallUsage{{Err: "transport: reset"}})+
			line("title", "b", []string{"ok"}, nil)+
			line("title", "a", []string{"ok-later"}, nil))
	d, err := loadDumpDone(p, gen)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.lookup("title", "a"); !ok || d.total != 2 {
		t.Fatalf("failed record must not be done, later complete one must win: total=%d", d.total)
	}
	if r, _ := d.lookup("title", "a"); r.Outputs[0] != "ok-later" {
		t.Fatalf("later complete record must win, got %q", r.Outputs[0])
	}
	// Duplikat zweier VOLLSTÄNDIGER Records ⇒ rot.
	p = write("dup.jsonl", line("title", "a", []string{"x"}, nil)+line("title", "a", []string{"y"}, nil))
	if _, err := loadDumpDone(p, gen); err == nil || !strings.Contains(err.Error(), "Duplikat") {
		t.Fatalf("duplicate complete records must be refused, got %v", err)
	}
	// Zeile ohne axis/id ⇒ rot.
	p = write("noaxis.jsonl", `{"id":"a","outputs":["x"]}`+"\n")
	if _, err := loadDumpDone(p, gen); err == nil || !strings.Contains(err.Error(), "ohne axis/id") {
		t.Fatalf("line without axis must be refused, got %v", err)
	}
	// F4: abgerissene Schlusszeile ⇒ toleriert, validLen = Ende der letzten vollständigen Zeile;
	// der Appender schneidet dort ab und hängt sauber an.
	good := line("title", "a", []string{"x"}, nil)
	p = write("torn.jsonl", good+`{"axis":"title","id":"b","outputs":["yy`)
	d, err = loadDumpDone(p, gen)
	if err != nil || d.total != 1 || d.validLen != int64(len(good)) {
		t.Fatalf("torn tail must be tolerated: err=%v total=%d validLen=%d want %d", err, d.total, d.validLen, len(good))
	}
	ap, err := openDumpAppender(p, d.validLen)
	if err != nil {
		t.Fatal(err)
	}
	if err := ap.write(dumpRecord{Axis: "title", ID: "b", Outputs: []string{"yes"}, Gen: gen}); err != nil {
		t.Fatal(err)
	}
	// F7: zweite Instanz auf derselben Datei ⇒ ErrDumpLocked.
	if _, err := openDumpAppender(p, -1); !errors.Is(err, ErrDumpLocked) {
		t.Fatalf("second appender must be refused by flock, got %v", err)
	}
	if err := ap.Close(); err != nil {
		t.Fatal(err)
	}
	if n, dups := countLines(t, p); n != 2 || dups != 0 {
		t.Fatalf("after torn-tail append: lines=%d dups=%d (want 2/0)", n, dups)
	}
	// Ein Parse-Fehler, der NICHT die Schlusszeile ist, bleibt rot.
	p = write("mid.jsonl", `{"axis":"title","id":"b","outputs":["yy`+"\n"+good)
	if _, err := loadDumpDone(p, gen); err == nil {
		t.Fatal("mid-file parse error must stay fatal")
	}
	// F8: Slot-Zahl ≠ Requests ⇒ rot (sensitivity hat 2 Slots).
	srv := fakeChatServer(t)
	defer srv.Close()
	cases, err := LoadCases(testDataDir(t), "sensitivity")
	if err != nil || len(cases) == 0 {
		t.Skipf("sensitivity cases: %v", err)
	}
	id := SampleCases(cases, 1, 20260812)[0].ID
	p = write("slots.jsonl", line("sensitivity", id, []string{"only-one-slot"}, nil))
	cfg := Config{DataDir: testDataDir(t), Endpoint: srv.URL, Model: "fake-model", Axes: []string{"sensitivity"},
		N: 1, Concurrency: 1, Seed: 20260812, TimeoutSec: 30, GenStamp: gen, TempOverride: -1, DumpOutputs: p, DumpAppend: true}
	if _, err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "Output-Slots") {
		t.Fatalf("slot mismatch must be refused, got %v", err)
	}
}

// TestDumpAppendSinkErrorCancels pinnt Review KW3 F5: ein Schreibfehler des Append-Sinks
// bricht den Lauf sofort ab (kein Weiterfahren gegen die GPU ohne Persistenz) und ist als
// ErrDumpWrite klassifiziert — nicht als Transport-Fehler des Falls.
func TestDumpAppendSinkErrorCancels(t *testing.T) {
	srv := fakeChatServer(t)
	defer srv.Close()
	var calls atomic.Int64
	cnt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer cnt.Close()
	gen := &GenStamp{Engine: "fake"}
	cfg := Config{DataDir: testDataDir(t), Endpoint: cnt.URL, Model: "fake-model",
		N: 1, Concurrency: 2, Seed: 20260812, TimeoutSec: 30, GenStamp: gen, TempOverride: -1}
	axes, _ := resolveAxes(nil, axisRegistry())
	axisRuns, jobs, _, _, err := buildRuns(cfg, axes, axisRegistry(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "sink.jsonl")
	ap, err := openDumpAppender(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = ap.f.Close() // Sink kaputt: jeder Flush scheitert (simuliert ENOSPC/EIO)
	ap.f = nil       // Close() idempotent halten
	err = executeJobs(context.Background(), cfg, jobs, axisRuns, &dumpAppender{f: os.NewFile(^uintptr(0), "broken"), buf: ap.buf, enc: ap.enc, path: p})
	if !errors.Is(err, ErrDumpWrite) {
		t.Fatalf("sink failure must surface as ErrDumpWrite, got %v", err)
	}
	if c := calls.Load(); c >= int64(len(jobs)) {
		t.Fatalf("run must stop after the first sink error: %d calls for %d jobs", c, len(jobs))
	}
	// Der Fall selbst trägt KEINEN Transport-Fehler (er wird beim Resume nachgeholt).
	for _, runs := range axisRuns {
		for _, r := range runs {
			if r.callErr != nil && !errors.Is(r.callErr, context.Canceled) {
				t.Fatalf("sink error must not be attributed to the case: %v", r.callErr)
			}
		}
	}
}
