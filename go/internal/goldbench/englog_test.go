package goldbench

import (
	"bufio"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// engLogDir ist das Bestands-Log-Verzeichnis (privates Submodule); fehlt es,
// werden die Bestands-Proben übersprungen — die synthetischen laufen immer.
func engLogDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join("..", "..", "..", ".project", "bench-goldbench-2026-08-12", "spark-results-v2")
	if _, err := os.Stat(d); err != nil {
		t.Skipf("bestands-logs nicht verfügbar: %v", err)
	}
	return d
}

func parseFile(t *testing.T, p string) (*EngLogResult, error) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return ParseEngLog(f)
}

// TestParseEngLogInventory pinnt das E3-Gate (design/04 §7, estimator-gleich):
// unweighted_line_mean reproduziert die handgerechnete Inventur-04-§3.4-Tabelle
// (±0,05) für beide Formate; alle Bestands-Logs mit Messfenstern parsen.
func TestParseEngLogInventory(t *testing.T) {
	d := engLogDir(t)
	// Inventur 04 §3.4 (ungewichtete Zeilen-Mittel; AR in %, SGLang-rate 0..1).
	want := []struct {
		file string
		n    int
		tau  float64
		ar   float64
	}{
		{"vlllog-qwen38-27b-mtp2-nightly-v4.log", 18, 2.71, 0.855},
		{"englog-qwen38-mtp2-c1.log", 38, 2.66, 0.829},
		{"englog-qwen38-mtp2-c12.log", 12, 2.70, 0.849},
		{"vlllog-qwen38-27b-mtp4-vllm-v4.log", 18, 3.98, 0.745},
		{"vlllog-qwen38-27b-mtp8-vllm-v4.log", 21, 5.17, 0.522},
		{"englog-qwen38-mtp8-c1.log", 45, 4.60, 0.450},
		{"englog-qwen38-mtp8-c12.log", 13, 5.24, 0.529},
		{"vlllog-qwen38-27b-dspark3-vllm-v4.log", 19, 2.66, 0.553},
		{"vlllog-qwen38-27b-dspark5-vllm-v4.log", 16, 2.97, 0.394},
		{"vlllog-qwen38-27b-dspark5-nightly-v4.log", 17, 3.10, 0.419},
		{"vlllog-qwen38-27b-dspark7-vllm-v4.log", 18, 3.17, 0.310},
		{"vlllog-qwen38-27b-dspark7-nightly-v4.log", 18, 3.30, 0.328},
		{"englog-qwen38-dspark5-c1.log", 35, 2.82, 0.364},
		{"englog-qwen38-dspark5-c12.log", 11, 3.14, 0.428},
		{"englog-qwen38-dspark7-c1.log", 34, 2.90, 0.271},
		{"englog-qwen38-dspark7-c12.log", 12, 3.31, 0.330},
		{"englog-qwen38-dspark7-nightly-c1.log", 34, 2.99, 0.284},
		{"englog-qwen38-dspark7-nightly-c12.log", 12, 3.34, 0.334},
		{"sgllog-sglang517-qwen38-nvfp4-dsparkauto.log", 10, 2.65, 0.24},
		{"sgllog-sglang517-qwen38-nvfp4-dsparkg3.log", 11, 2.40, 0.47},
		{"sgllog-qwen38-sgl-dsparkC-c1.log", 19, 2.57, 0.22},
		{"sgllog-qwen38-sgl-dsparkC-c4.log", 8, 2.62, 0.23},
		{"sgllog-qwen38-sgl-eagleC-c1.log", 18, 3.11, 0.70},
		{"sgllog-qwen38-sgl-eagleC-c4.log", 7, 3.12, 0.71},
		{"sgllog-qwen38-sgl-eagleHT-c4.log", 7, 3.16, 0.72},
		{"sgllog-qwen38-sgl-eagleA-c1.log", 27, 1.00, 0.00},
		{"sgllog-qwen36-sgl-dspark-c1.log", 12, 2.84, 0.12},
		{"sgllog-qwen36-sgl-dspark-c4.log", 5, 2.75, 0.12},
		{"sgllog-qwen36-sgl512-eagle3-c1.log", 15, 2.17, 0.39},
		{"sgllog-qwen36-sgl512-eagle3-c4.log", 7, 1.95, 0.32},
	}
	found := 0
	for _, w := range want {
		p := filepath.Join(d, w.file)
		if _, err := os.Stat(p); err != nil {
			t.Logf("%s fehlt — übersprungen", w.file)
			continue
		}
		found++
		res, err := parseFile(t, p)
		if err != nil {
			t.Errorf("%s: %v", w.file, err)
			continue
		}
		if res.Lines != w.n {
			t.Errorf("%s: lines=%d want %d", w.file, res.Lines, w.n)
		}
		if math.Abs(res.UnweightedLineMean-w.tau) > 0.05 {
			t.Errorf("%s: unweighted τ=%.3f want %.2f±0.05", w.file, res.UnweightedLineMean, w.tau)
		}
		if math.Abs(res.UnweightedARLineMean-w.ar) > 0.01 {
			t.Errorf("%s: unweighted AR=%.3f want %.3f±0.01", w.file, res.UnweightedARLineMean, w.ar)
		}
		if res.Source != "log-parse" || res.SchemaVersion != 1 || res.BootMarkers != 0 {
			t.Errorf("%s: source=%s schema=%d boot=%d (Bestands-Logs sind Tail-Captures)", w.file, res.Source, res.SchemaVersion, res.BootMarkers)
		}
		t.Logf("%-48s n=%-3d τ̄=%.3f τw=%.3f Δ=%+.3f AR̄=%.3f ARw=%.3f tps=%.1f γ=%d", w.file, res.Lines, res.UnweightedLineMean, res.Tau, res.WeightedMinusUnweighted, res.UnweightedARLineMean, res.AR, res.SteadyDecodeTPS, res.Gamma)
	}
	// Vakuum-Schutz: ein Stub-Submodule darf das Gate nicht leer-grün machen.
	if found < 28 {
		t.Fatalf("nur %d/%d Bestands-Logs gefunden — Gate nicht belegbar", found, len(want))
	}
	// Bestands-SGLang-Log: τ (gewichtet) und Steady-TPS gegen gepinnte Werte
	// (Regressionsschutz des Δt-Pfads; Werte aus dem Erstlauf 2026-08-19).
	if res, err := parseFile(t, filepath.Join(d, "sgllog-qwen38-sgl-dsparkC-c1.log")); err == nil {
		if math.Abs(res.Tau-2.608) > 0.01 || math.Abs(res.SteadyDecodeTPS-19.2) > 0.3 {
			t.Fatalf("dsparkC-c1 pinned: τw=%.3f (2.608) tps=%.1f (19.2)", res.Tau, res.SteadyDecodeTPS)
		}
	}
	// no-spec-Baseline: kein Fehler, Flag + Steady-TPS.
	if res, err := parseFile(t, filepath.Join(d, "englog-qwen38-sgl-base-c1.log")); err == nil {
		if !res.NoSpecWindows || res.SteadyDecodeTPS <= 0 || res.Lines != 0 {
			t.Fatalf("no-spec baseline: %+v", res)
		}
	} else {
		t.Fatalf("no-spec baseline must parse without error: %v", err)
	}
}

// TestParseEngLogVLLMExactAR: das gewichtete vLLM-AR ist die exakte Summation
// Accepted/Drafted derselben Zeilen (Toleranz 0 bis auf float-Rundung).
func TestParseEngLogVLLMExactAR(t *testing.T) {
	d := engLogDir(t)
	re := regexp.MustCompile(`Accepted: (\d+) tokens, Drafted: (\d+) tokens`)
	for _, name := range []string{"vlllog-qwen38-27b-dspark7-nightly-v4.log", "englog-qwen38-mtp8-c1.log"} {
		p := filepath.Join(d, name)
		f, err := os.Open(p)
		if err != nil {
			t.Skip(err)
		}
		var acc, dr int64
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				a, _ := strconv.ParseInt(m[1], 10, 64)
				b, _ := strconv.ParseInt(m[2], 10, 64)
				acc, dr = acc+a, dr+b
			}
		}
		_ = f.Close()
		res, err := parseFile(t, p)
		if err != nil {
			t.Fatal(err)
		}
		if res.AcceptedTokens != acc || res.DraftTokens != dr {
			t.Fatalf("%s: sums acc=%d/%d drafted=%d/%d", name, res.AcceptedTokens, acc, res.DraftTokens, dr)
		}
		if math.Abs(res.AR-float64(acc)/float64(dr)) > 1e-12 {
			t.Fatalf("%s: AR %.6f != %d/%d", name, res.AR, acc, dr)
		}
		// γ ist je Lauf konstant ⇒ τ EXAKT: 1 + γ·ΣAcc/ΣDrafted (Toleranz 0 bis float).
		if res.Gamma == 0 || res.TauDerivedDrafts {
			t.Fatalf("%s: γ must be reconstructed consistently (gamma=%d derived=%v)", name, res.Gamma, res.TauDerivedDrafts)
		}
		if want := 1 + float64(res.Gamma)*float64(acc)/float64(dr); math.Abs(res.Tau-want) > 1e-12 {
			t.Fatalf("%s: τ %.9f != 1+γ·acc/drafted %.9f", name, res.Tau, want)
		}
	}
}

// TestParseEngLogRed pinnt die ROT-Proben des E3-Gates: Boot-only-Log ⇒
// ErrNoWindows (Exit ≠ 0 statt leerer Stats), ZWEI Boot-Marker ⇒ ErrMultiBoot,
// gemischte Formate ⇒ ErrFormat.
func TestParseEngLogRed(t *testing.T) {
	d := engLogDir(t)
	bootOnly := filepath.Join(d, "vlllog-qwen38-27b-mtp-vllm-v4.log")
	b, err := os.ReadFile(bootOnly)
	if err != nil {
		t.Skip(err)
	}
	res, err := ParseEngLog(strings.NewReader(string(b)))
	if !errors.Is(err, ErrNoWindows) || res == nil || res.BootMarkers != 1 || res.Lines != 0 {
		t.Fatalf("boot-only: err=%v res=%+v", err, res)
	}
	// Zwei Boots hintereinander (geteilter Container-Log) ⇒ verweigert.
	if _, err := ParseEngLog(strings.NewReader(string(b) + "\n" + string(b))); !errors.Is(err, ErrMultiBoot) {
		t.Fatalf("two boots must be refused, got %v", err)
	}
	// Mischformat ⇒ ErrFormat.
	mixed := "[2026-08-19 00:02:25] Decode batch, #running-req: 4, #full token: 1, full token usage: 0.00, mamba num: 1, mamba usage: 0.1, accept len: 2.36, accept rate: 0.19, cuda graph: True, gen throughput (token/s): 40.29, #queue-req: 0\n" +
		"(APIServer pid=1) INFO 08-17 19:55:36 [loggers.py:310] Engine 000: Avg prompt throughput: 1.0 tokens/s, Avg generation throughput: 58.7 tokens/s, Running: 1 reqs, Waiting: 0 reqs\n"
	if _, err := ParseEngLog(strings.NewReader(mixed)); !errors.Is(err, ErrFormat) {
		t.Fatalf("mixed formats must be refused, got %v", err)
	}
	if _, err := ParseEngLog(strings.NewReader("nothing here\n")); !errors.Is(err, ErrFormat) {
		t.Fatalf("unknown format must be refused, got %v", err)
	}
	// Angeschnittener Zweit-Boot: Init-Zeile ohne Start ⇒ trotzdem 2 Boots.
	cut := string(b) + "\n(EngineCore pid=999) INFO 08-16 21:00:00 [core.py:116] Initializing a V1 LLM engine (v0.26.1) with config: model='/model'\n"
	if _, err := ParseEngLog(strings.NewReader(cut)); !errors.Is(err, ErrMultiBoot) {
		t.Fatalf("cut second boot must be refused, got %v", err)
	}
	// Teilweiser Format-Drift: Spec-Signatur, aber Regex scheitert ⇒ ErrFormat.
	drift := "(APIServer pid=1) INFO 08-17 19:55:36 [metrics.py:120] SpecDecoding metrics: Mean acceptance length: nan, Accepted: 0 tokens\n"
	if _, err := ParseEngLog(strings.NewReader(drift)); !errors.Is(err, ErrFormat) {
		t.Fatalf("partial format drift must be refused, got %v", err)
	}
	// Wirkungsloser Drafter (MAL 1.00): τ = 1, kein Fehler, kein τ=0.
	flat := "(APIServer pid=1) INFO 08-17 19:55:36 [metrics.py:120] SpecDecoding metrics: Mean acceptance length: 1.00, Accepted throughput: 0 tokens/s, Drafted throughput: 1 tokens/s, Accepted: 0 tokens, Drafted: 700 tokens, Per-position acceptance rate: 0.0, Avg Draft acceptance rate: 0.0%\n"
	res, err = ParseEngLog(strings.NewReader(flat))
	if err != nil || res.Tau != 1 || res.AR != 0 {
		t.Fatalf("MAL 1.00 log: err=%v τ=%v AR=%v", err, res.Tau, res.AR)
	}
}

// TestParseEngLogSynthetic prüft die Gewichtung deterministisch: SGLang-Fenster
// mit ungleichen Token-Gewichten ⇒ gewichtetes τ ≠ Zeilen-Mittel, exakt
// nachgerechnet; vLLM-Intervalle ⇒ τ aus rekonstruierten Verify-Steps.
func TestParseEngLogSynthetic(t *testing.T) {
	sgl := "[2026-08-19 00:00:00] Decode batch, #running-req: 2, #full token: 1, full token usage: 0.00, mamba num: 1, mamba usage: 0.1, accept len: 2.00, accept rate: 0.20, cuda graph: True, gen throughput (token/s): 10.00, #queue-req: 0\n" +
		"[2026-08-19 00:00:10] Decode batch, #running-req: 2, #full token: 1, full token usage: 0.00, mamba num: 1, mamba usage: 0.1, accept len: 4.00, accept rate: 0.60, cuda graph: True, gen throughput (token/s): 30.00, #queue-req: 0\n"
	res, err := ParseEngLog(strings.NewReader(sgl))
	if err != nil {
		t.Fatal(err)
	}
	// Gewichte: 10×10 s = 100, 30×10 s = 300 ⇒ τw = (2·100+4·300)/400 = 3.5; Zeilen-Mittel 3.0.
	if math.Abs(res.Tau-3.5) > 1e-9 || math.Abs(res.UnweightedLineMean-3.0) > 1e-9 || !res.TauTimeWeighted || !res.ARLowerBound {
		t.Fatalf("sglang weighting: %+v", res)
	}
	if math.Abs(res.SteadyDecodeTPS-20.0) > 1e-9 {
		t.Fatalf("steady tps: %v", res.SteadyDecodeTPS)
	}
	// Ungleiche Kadenz (Δt trägt): Stempel 0/5/10/30 s ⇒ Abstände 5,5,20,
	// Median 5 ⇒ Cap 15 s für die Pause. Zeile 1 Δt=5 (Nachfolger), 2: 5, 3: 5,
	// 4: 20→15. Gewichte 50,50,50,150 ⇒ τw = (2·50+2·50+2·50+4·150)/300 = 3.0;
	// ohne Cap wäre es (300+800)/350 = 3.14, ohne Δt-Pfad 2.5.
	row := func(ts string, tau string) string {
		return "[2026-08-19 00:00:" + ts + "] Decode batch, #running-req: 1, #full token: 1, full token usage: 0.00, mamba num: 1, mamba usage: 0.1, accept len: " + tau + ", accept rate: 0.20, cuda graph: True, gen throughput (token/s): 10.00, #queue-req: 0\n"
	}
	uneven := row("00", "2.00") + row("05", "2.00") + row("10", "2.00") + row("30", "4.00")
	res, err = ParseEngLog(strings.NewReader(uneven))
	if err != nil || math.Abs(res.Tau-3.0) > 1e-9 {
		t.Fatalf("uneven Δt with median cap: err=%v τ=%v (want 3.0)", err, res.Tau)
	}

	vllm := "(APIServer pid=1) INFO 08-17 19:55:36 [metrics.py:120] SpecDecoding metrics: Mean acceptance length: 3.00, Accepted throughput: 1 tokens/s, Drafted throughput: 1 tokens/s, Accepted: 200 tokens, Drafted: 400 tokens, Per-position acceptance rate: 0.8, 0.2, Avg Draft acceptance rate: 50.0%\n" +
		"(APIServer pid=1) INFO 08-17 19:55:46 [metrics.py:120] SpecDecoding metrics: Mean acceptance length: 2.00, Accepted throughput: 1 tokens/s, Drafted throughput: 1 tokens/s, Accepted: 100 tokens, Drafted: 400 tokens, Per-position acceptance rate: 0.4, 0.1, Avg Draft acceptance rate: 25.0%\n" +
		"(APIServer pid=1) INFO 08-17 19:55:46 [loggers.py:310] Engine 000: Avg prompt throughput: 0.0 tokens/s, Avg generation throughput: 40.0 tokens/s, Running: 1 reqs, Waiting: 0 reqs\n"
	res, err = ParseEngLog(strings.NewReader(vllm))
	if err != nil {
		t.Fatal(err)
	}
	// γ: Zeile 1 400/(200/2)=4, Zeile 2 400/(100/1)=4 ⇒ konsistent γ=4 ⇒
	// steps = 800/4 = 200 ⇒ τ = 1 + 300/200 = 2.5 (exakt); AR = 300/800 = 0.375.
	if math.Abs(res.Tau-2.5) > 1e-9 || math.Abs(res.AR-0.375) > 1e-9 || res.VerifySteps != 200 || res.AcceptedTokens != 300 || res.Gamma != 4 || res.TauDerivedDrafts {
		t.Fatalf("vllm sums: %+v γ=%d derived=%v", res.SpecStats, res.Gamma, res.TauDerivedDrafts)
	}
	// Per-Position step-gewichtet: (0.8·100 + 0.4·100)/200 = 0.6, (0.2·100+0.1·100)/200 = 0.15.
	if len(res.PerPosition) != 2 || math.Abs(res.PerPosition[0]-0.6) > 1e-9 || math.Abs(res.PerPosition[1]-0.15) > 1e-9 {
		t.Fatalf("per-position: %v", res.PerPosition)
	}
	if math.Abs(res.SteadyDecodeTPS-40.0) > 1e-9 {
		t.Fatalf("steady tps: %v", res.SteadyDecodeTPS)
	}
}

// TestParseEngLogReviewRC0 pinnt die Review-Runde RC-0 (Findings F1/F5/F6/F8):
// ein VOLLSTÄNDIGER SGLang-Boot (server_args + Uvicorn + fired-up) ist EIN
// Boot; Zeitstempel mit Rank-Suffix/Millisekunden tragen Δt; ein no-spec-Log
// aus lauter Running==0-Fenstern ist keine leere Messung; ErrFormat liefert
// trotzdem ein Result (CLI druckt JSON mit error-Feld).
func TestParseEngLogReviewRC0(t *testing.T) {
	dec := func(ts string, reqs int, al string, tps string) string {
		return "[" + ts + "] Decode batch, #running-req: " + strconv.Itoa(reqs) + ", #full token: 1, full token usage: 0.00, mamba num: 1, mamba usage: 0.1, accept len: " + al + ", accept rate: 0.50, cuda graph: True, gen throughput (token/s): " + tps + ", #queue-req: 0\n"
	}
	// F1: voller Boot = drei Signaturen, aber EIN Boot.
	full := "[2026-08-19 10:00:00] server_args=ServerArgs(model_path='/m')\n" +
		"INFO:     Uvicorn running on http://127.0.0.1:30000 (Press CTRL+C to quit)\n" +
		"[2026-08-19 10:00:30] The server is fired up and ready to roll!\n" +
		dec("2026-08-19 10:01:00", 1, "3.00", "50.0") + dec("2026-08-19 10:01:10", 1, "3.00", "50.0")
	res, err := ParseEngLog(strings.NewReader(full))
	if err != nil || res.BootMarkers != 1 {
		t.Fatalf("F1 full SGLang boot: err=%v boot_markers=%d (want 1)", err, res.BootMarkers)
	}
	// F5: Rank-Suffix + Millisekunden — Δt aus Stempeln statt Default 10 s:
	// Zeilen bei t=0, 2, 4, 34 ⇒ Δt 2,2,2,30, Median 2 ⇒ Cap 6 ⇒ Gewichte
	// 2,2,2,6 (τ 3,3,3,4 ⇒ 42/12 = 3.5; steady 12 s). Mit Default-Δt wären es
	// Gewichte 10,10,10,10 ⇒ τ 3.25 / steady 40 — das alte, stille Fehlmaß.
	tp := dec("2026-08-19 10:00:00.000 TP0", 1, "3.00", "10.0") + dec("2026-08-19 10:00:02.000 TP0", 1, "3.00", "10.0") +
		dec("2026-08-19 10:00:04.000 TP0", 1, "3.00", "10.0") + dec("2026-08-19 10:00:34.000 TP0", 1, "4.00", "10.0")
	res, err = ParseEngLog(strings.NewReader(tp))
	if err != nil || res.LinesWithoutTimestamp != 0 || math.Abs(res.Tau-3.5) > 1e-9 || math.Abs(res.SteadySeconds-12) > 1e-9 {
		t.Fatalf("F5 TP-suffix timestamps: err=%v noTS=%d τ=%v steady=%v (want 0 / 3.5 / 12)", err, res.LinesWithoutTimestamp, res.Tau, res.SteadySeconds)
	}
	// Gegenprobe: ohne Zeitstempel zählt der Drift-Zähler.
	res, _ = ParseEngLog(strings.NewReader(strings.ReplaceAll(tp, "[2026-08-19 10:00:", "[x ")))
	if res == nil || res.LinesWithoutTimestamp != 4 {
		t.Fatalf("lines without timestamp must be counted, got %+v", res)
	}
	// F6: no-spec-vLLM-Log, alle Fenster Running: 0 ⇒ kein ErrNoWindows.
	idle := "(APIServer pid=1) INFO 08-17 19:55:36 [loggers.py:310] Engine 000: Avg prompt throughput: 0.0 tokens/s, Avg generation throughput: 12.5 tokens/s, Running: 0 reqs, Waiting: 0 reqs\n"
	res, err = ParseEngLog(strings.NewReader(idle + idle))
	if err != nil || !res.NoSpecWindows || res.SteadyDroppedWindows != 2 || res.SteadyDecodeTPS != 0 {
		t.Fatalf("F6 idle-only no-spec: err=%v res=%+v", err, res)
	}
	// F8: ErrFormat ⇒ Result nicht nil.
	res, err = ParseEngLog(strings.NewReader("nothing here\n"))
	if !errors.Is(err, ErrFormat) || res == nil || res.Source != "log-parse" {
		t.Fatalf("F8 ErrFormat must still return a result, got res=%v err=%v", res, err)
	}
}
