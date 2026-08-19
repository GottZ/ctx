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
	for _, w := range want {
		p := filepath.Join(d, w.file)
		if _, err := os.Stat(p); err != nil {
			t.Logf("%s fehlt — übersprungen", w.file)
			continue
		}
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
		t.Logf("%-48s n=%-3d τ̄=%.3f τw=%.3f Δ=%+.3f AR̄=%.3f ARw=%.3f tps=%.1f", w.file, res.Lines, res.UnweightedLineMean, res.Tau, res.WeightedMinusUnweighted, res.UnweightedARLineMean, res.AR, res.SteadyDecodeTPS)
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
		if !res.TauDerivedDrafts || res.Tau <= 1 {
			t.Fatalf("%s: τ must be derived from accepted/(MAL-1): %+v", name, res.SpecStats)
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

	vllm := "(APIServer pid=1) INFO 08-17 19:55:36 [metrics.py:120] SpecDecoding metrics: Mean acceptance length: 3.00, Accepted throughput: 1 tokens/s, Drafted throughput: 1 tokens/s, Accepted: 200 tokens, Drafted: 400 tokens, Per-position acceptance rate: 0.8, 0.2, Avg Draft acceptance rate: 50.0%\n" +
		"(APIServer pid=1) INFO 08-17 19:55:46 [metrics.py:120] SpecDecoding metrics: Mean acceptance length: 2.00, Accepted throughput: 1 tokens/s, Drafted throughput: 1 tokens/s, Accepted: 100 tokens, Drafted: 400 tokens, Per-position acceptance rate: 0.4, 0.1, Avg Draft acceptance rate: 25.0%\n" +
		"(APIServer pid=1) INFO 08-17 19:55:46 [loggers.py:310] Engine 000: Avg prompt throughput: 0.0 tokens/s, Avg generation throughput: 40.0 tokens/s, Running: 1 reqs, Waiting: 0 reqs\n"
	res, err = ParseEngLog(strings.NewReader(vllm))
	if err != nil {
		t.Fatal(err)
	}
	// steps = 200/2 + 100/1 = 200 ⇒ τ = 1 + 300/200 = 2.5; AR = 300/800 = 0.375; Zeilen-Mittel 2.5/0.375.
	if math.Abs(res.Tau-2.5) > 1e-9 || math.Abs(res.AR-0.375) > 1e-9 || res.VerifySteps != 200 || res.AcceptedTokens != 300 {
		t.Fatalf("vllm sums: %+v", res.SpecStats)
	}
	// Per-Position step-gewichtet: (0.8·100 + 0.4·100)/200 = 0.6, (0.2·100+0.1·100)/200 = 0.15.
	if len(res.PerPosition) != 2 || math.Abs(res.PerPosition[0]-0.6) > 1e-9 || math.Abs(res.PerPosition[1]-0.15) > 1e-9 {
		t.Fatalf("per-position: %v", res.PerPosition)
	}
	if math.Abs(res.SteadyDecodeTPS-40.0) > 1e-9 {
		t.Fatalf("steady tps: %v", res.SteadyDecodeTPS)
	}
}
