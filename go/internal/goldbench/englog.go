package goldbench

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SpecStats ist die normalisierte Speculative-Decoding-Messung eines Laufs
// (design/04 §3.2). Quelle je Engine verschieden (Source); Felder ohne
// Quelle bleiben leer. Normalisierung §3.1: τ = emittierte Token pro
// Verify-Step INKL. Bonus, AR = akzeptierte Draft-Token ÷ gedraftete OHNE Bonus.
type SpecStats struct {
	SchemaVersion   int       `json:"schema_version"`
	Engine          string    `json:"engine"` // "vllm" | "sglang"
	Source          string    `json:"source"` // "metrics-diff" | "metrics-sampling" | "log-parse"
	FormulaVerified bool      `json:"formula_verified"`
	VerifySteps     int64     `json:"verify_steps"`
	DraftTokens     int64     `json:"draft_tokens,omitempty"`
	AcceptedTokens  int64     `json:"accepted_tokens"`
	Tau             float64   `json:"tau"`
	AR              float64   `json:"ar"`
	ARLowerBound    bool      `json:"ar_lower_bound,omitempty"`
	TauTimeWeighted bool      `json:"tau_time_weighted,omitempty"`
	PerPosition     []float64 `json:"per_position,omitempty"`
	SteadyDecodeTPS float64   `json:"steady_decode_tps,omitempty"`
	Windows         int       `json:"windows,omitempty"`
	CrossCheckTau   float64   `json:"cross_check_tau,omitempty"`
}

// EngLogResult ist die Ausgabe von -parse-englog (design/04 §4.2): die
// SpecStats (source "log-parse") plus die Debug-/Validierungsfelder, an
// denen das E3-Gate hängt.
type EngLogResult struct {
	SpecStats
	Format string `json:"format"` // "vllm-stdout" | "sglang-stdout"
	// BootMarkers zählt Engine-Boot-Signaturen im Log: 0 = Tail-Capture
	// (Bestands-Logs, `docker logs | tail -300`), 1 = vollständiger Lauf,
	// >1 ⇒ Parser verweigert (Fenster nicht attribuierbar).
	BootMarkers int `json:"boot_markers"`
	Lines       int `json:"lines"` // geparste Spec-Zeilen
	// UnweightedLineMean ist das UNgewichtete Zeilen-Mittel von τ — der
	// Schätzer der Inventur-§3.4-Tabelle; das Gate vergleicht ihn (±0,05)
	// getrennt vom gewichteten τ.
	UnweightedLineMean   float64 `json:"unweighted_line_mean"`
	UnweightedARLineMean float64 `json:"unweighted_ar_line_mean"`
	LineTauMin           float64 `json:"line_tau_min"`
	LineTauMax           float64 `json:"line_tau_max"`
	// WeightedMinusUnweighted = Tau − UnweightedLineMean (Nebenbefund §4.2).
	WeightedMinusUnweighted float64 `json:"weighted_minus_unweighted"`
	// TauDerivedDrafts: vLLM loggt keine Verify-Step-Zahl; sie wird je
	// Intervall aus Accepted/(MAL−1) rekonstruiert (MAL auf 2 Dezimalen
	// gerundet ⇒ τ ist eine enge Näherung, AR dagegen exakt summiert).
	TauDerivedDrafts bool    `json:"tau_derived_drafts,omitempty"`
	SteadySeconds    float64 `json:"steady_seconds,omitempty"` // Zeitbasis der Steady-TPS
	// Gamma ist das aus den vLLM-Zeilen rekonstruierte, über alle Zeilen
	// konsistente γ (drafted/steps) — dann ist τ exakt (steps = ΣDrafted/γ).
	Gamma int `json:"gamma,omitempty"`
	// NoSpecWindows: Log ohne Spec-Zeilen, aber mit gültiger Steady-TPS
	// (no-spec-Baseline §4.7) — kein Fehler, Flag für den Leser.
	NoSpecWindows bool `json:"no_spec_windows,omitempty"`
	// UnparsedSpecLines zählt Zeilen mit Spec-Signatur, die am Regex
	// scheitern (teilweiser Format-Drift) — >0 ⇒ ErrFormat.
	UnparsedSpecLines int `json:"unparsed_spec_lines,omitempty"`
	// SteadyDroppedWindows: Fenster mit tps>0, aber Running==0 (nicht gewertet).
	SteadyDroppedWindows int `json:"steady_dropped_windows,omitempty"`
	// LinesWithoutTimestamp: Spec-/Decode-Zeilen ohne auswertbaren Zeitstempel
	// (Δt fiel auf den Default) — >0 heißt: Zeitgewichtung ist Näherung.
	LinesWithoutTimestamp int    `json:"lines_without_timestamp,omitempty"`
	Error                 string `json:"error,omitempty"` // gesetzt, wenn der Parser mit Fehler endet
}

// ErrNoWindows: Log ohne Spec-Messzeilen UND ohne Steady-Fenster (Boot-only-Capture).
var ErrNoWindows = errors.New("parse-englog: keine Messfenster (keine SpecDecoding-/accept-len-Zeilen, keine Decode-Fenster)")

// ErrMultiBoot: Log trägt mehrere Boots — Fenster nicht attribuierbar.
var ErrMultiBoot = errors.New("parse-englog: Log trägt mehrere Läufe/Boots — Fenster nicht attribuierbar")

// ErrFormat: weder vLLM- noch SGLang-Signatur (oder beide gemischt).
var ErrFormat = errors.New("parse-englog: Log-Format nicht erkannt oder gemischt")

var (
	// vLLM metrics.py:120 (0.26.x/0.27.x): Intervallzeile mit absoluten Counts.
	reVLLMSpec = regexp.MustCompile(`SpecDecoding metrics: Mean acceptance length: ([0-9.]+), Accepted throughput: ([0-9.]+) tokens/s, Drafted throughput: ([0-9.]+) tokens/s, Accepted: (\d+) tokens, Drafted: (\d+) tokens(?:, Per-position acceptance rate: ([0-9., ]+?))?, Avg Draft acceptance rate: ([0-9.]+)%`)
	// vLLM loggers.py:310: Steady-Decode-TPS.
	reVLLMGen = regexp.MustCompile(`Avg generation throughput: ([0-9.]+) tokens/s, Running: (\d+) reqs`)
	// vLLM Zeitstempel "INFO 08-17 19:55:36".
	reVLLMTS = regexp.MustCompile(`(?:INFO|WARNING|ERROR) (\d\d)-(\d\d) (\d\d):(\d\d):(\d\d)`)
	// Boot-Signaturen vLLM: Init (Minuten VOR dem Start) und Start — gezählt
	// wird das MAXIMUM beider, damit ein angeschnittener Zweit-Boot (Init ohne
	// Start) nicht als „ein Boot" durchgeht. Banner = schwaches Format-Signal.
	reVLLMBoot   = regexp.MustCompile(`Starting vLLM server on |Initializing a V1 LLM engine`)
	reVLLMBanner = regexp.MustCompile(`\(APIServer pid=\d+\)|\(EngineCore pid=\d+\)|vllm\.|VLLM_`)

	// SGLang Decode-Batch-Zeile mit Spec-Feldern.
	reSGLSpec = regexp.MustCompile(`Decode batch, #running-req: (\d+),.*accept len: ([0-9.]+), accept rate: ([0-9.]+),.*gen throughput \(token/s\): ([0-9.]+)`)
	// SGLang Decode-Batch ohne Spec (Basis-Läufe) — nur Steady-TPS.
	reSGLGen = regexp.MustCompile(`Decode batch, #running-req: (\d+),.*gen throughput \(token/s\): ([0-9.]+)`)
	// SGLang Zeitstempel "[2026-08-19 00:02:25]" — sglang configure_logger
	// formatiert `[%(asctime)s{.ms}{prefix}]`: optionale Millisekunden
	// (SGLANG_LOG_MS) und Rank-Suffix (" TP0", " DP0 TP0" bei tp>1) müssen
	// toleriert werden, sonst fällt Δt still auf den 10-s-Default (Review F5).
	reSGLTS = regexp.MustCompile(`^\[(\d{4})-(\d\d)-(\d\d) (\d\d):(\d\d):(\d\d)(?:\.(\d{1,3}))?[^\]]*\]`)
	// Boot-Signaturen SGLang: server_args= (launch) sowie Uvicorn-running und
	// fired-up (beides Ready-Zeilen EINES Boots — getrennt gezählt, Maximum
	// über die drei Zähler; Review F1: die Summe zählte einen Boot doppelt).
	reSGLBoot = regexp.MustCompile(`server_args=|Uvicorn running on |The server is fired up and ready to roll`)
)

// defaultIntervalSec ist der Gewichts-Fallback ohne auswertbaren Zeitstempel-
// Abstand: vLLM loggt fix alle 10 s; SGLang log_interval-abhängig (gemessen
// 4–22 s) — dort trägt der Zeitstempel, der Fallback greift nur für Ein-Zeilen-Logs.
const defaultIntervalSec = 10.0

// maxIntervalFactor deckelt ein Fenster-Δt auf das Vielfache der Median-
// Kadenz: eine Achsen-Pause im Voll-Log (E4) darf die erste Zeile danach
// nicht mit der Pausenlänge gewichten.
const maxIntervalFactor = 3.0

// stamp ist der optionale Zeitstempel einer Log-Zeile (Δt-Basis beider Zeilentypen).
type stamp struct {
	t    time.Time
	hasT bool
}

type specLine struct {
	stamp
	tau      float64
	ar       float64 // 0..1
	tps      float64 // SGLang: gen throughput der Zeile; vLLM: 0 (eigene Zeile)
	accepted int64   // vLLM
	drafted  int64   // vLLM
	perPos   []float64
}

type genLine struct {
	stamp
	tps  float64
	reqs int
}

// ParseEngLog liest einen Engine-Stdout-Log (vLLM oder SGLang, auto-erkannt)
// und liefert SpecStats (source "log-parse") plus Debug-Felder.
//
//nolint:cyclop // Format-Zweige + Gewichtung in einer linearen Funktion
func ParseEngLog(r io.Reader) (*EngLogResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	var (
		vSpec, sSpec            []specLine
		vGen, sGen              []genLine
		vInit, vStart           int
		sArgs, sUvicorn, sFired int
		vSig, sSig              bool
		vUnparsed, sUnparsed    int
	)
	for sc.Scan() {
		ln := sc.Text()
		if reVLLMBoot.MatchString(ln) {
			vSig = true
			if strings.Contains(ln, "Starting vLLM server on") {
				vStart++
			} else {
				vInit++
			}
		} else if reVLLMBanner.MatchString(ln) {
			vSig = true
		}
		if reSGLBoot.MatchString(ln) {
			sSig = true
			switch {
			case strings.Contains(ln, "server_args="):
				sArgs++
			case strings.Contains(ln, "Uvicorn running on "):
				sUvicorn++
			default:
				sFired++
			}
		}
		if strings.Contains(ln, "SpecDecoding metrics:") && !reVLLMSpec.MatchString(ln) {
			vUnparsed++
		}
		if strings.Contains(ln, "accept len:") && !reSGLSpec.MatchString(ln) {
			sUnparsed++
		}
		if m := reVLLMSpec.FindStringSubmatch(ln); m != nil {
			l := specLine{}
			l.t, l.hasT = vllmTime(ln)
			l.tau, _ = strconv.ParseFloat(m[1], 64)
			l.accepted, _ = strconv.ParseInt(m[4], 10, 64)
			l.drafted, _ = strconv.ParseInt(m[5], 10, 64)
			if m[6] != "" {
				for p := range strings.SplitSeq(m[6], ",") {
					if v, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err == nil {
						l.perPos = append(l.perPos, v)
					}
				}
			}
			arPct, _ := strconv.ParseFloat(m[7], 64)
			l.ar = arPct / 100
			vSpec = append(vSpec, l)
			continue
		}
		if m := reVLLMGen.FindStringSubmatch(ln); m != nil {
			g := genLine{}
			g.t, g.hasT = vllmTime(ln)
			g.tps, _ = strconv.ParseFloat(m[1], 64)
			g.reqs, _ = strconv.Atoi(m[2])
			vGen = append(vGen, g)
			continue
		}
		if m := reSGLSpec.FindStringSubmatch(ln); m != nil {
			l := specLine{}
			l.t, l.hasT = sglTime(ln)
			l.tau, _ = strconv.ParseFloat(m[2], 64)
			l.ar, _ = strconv.ParseFloat(m[3], 64)
			l.tps, _ = strconv.ParseFloat(m[4], 64)
			sSpec = append(sSpec, l)
			reqs, _ := strconv.Atoi(m[1])
			sGen = append(sGen, genLine{stamp: l.stamp, tps: l.tps, reqs: reqs})
			continue
		}
		if m := reSGLGen.FindStringSubmatch(ln); m != nil {
			g := genLine{}
			g.t, g.hasT = sglTime(ln)
			g.reqs, _ = strconv.Atoi(m[1])
			g.tps, _ = strconv.ParseFloat(m[2], 64)
			sGen = append(sGen, g)
		}
	}
	if err := sc.Err(); err != nil {
		// Immer ein Result liefern (Review F8): der CLI-Kontrakt verspricht
		// das JSON auch im Fehlerfall (mit `error`-Feld).
		return &EngLogResult{SpecStats: SpecStats{SchemaVersion: 1, Source: "log-parse"}}, fmt.Errorf("parse-englog: read: %w", err)
	}

	isV := len(vSpec)+len(vGen) > 0 || vSig
	isS := len(sSpec)+len(sGen) > 0 || sSig
	if isV == isS { // beide oder keins
		return &EngLogResult{SpecStats: SpecStats{SchemaVersion: 1, Source: "log-parse"}}, ErrFormat
	}
	res := &EngLogResult{}
	res.SchemaVersion = 1
	res.Source = "log-parse"
	res.FormulaVerified = false // §3.1-Vorbehalt: Formeln gegen main belegt, nicht gegen den gepinnten Tag
	var spec []specLine
	var gen []genLine
	if isV {
		res.Engine, res.Format, res.BootMarkers, spec, gen = "vllm", "vllm-stdout", max(vInit, vStart), vSpec, vGen
		res.UnparsedSpecLines = vUnparsed
	} else {
		res.Engine, res.Format, res.BootMarkers, spec, gen = "sglang", "sglang-stdout", max(sArgs, sUvicorn, sFired), sSpec, sGen
		res.UnparsedSpecLines = sUnparsed
	}
	if res.BootMarkers > 1 {
		return res, fmt.Errorf("%w (%d Boot-Marker)", ErrMultiBoot, res.BootMarkers)
	}
	if res.UnparsedSpecLines > 0 {
		return res, fmt.Errorf("%w: %d Spec-Zeilen mit Signatur, aber abweichendem Format (teilweiser Format-Drift)", ErrFormat, res.UnparsedSpecLines)
	}
	// vLLM: Spec- und Throughput-Zeilen sind disjunkt; SGLang: jede Spec-
	// Zeile ist zugleich Decode-Zeile (gen ⊇ spec) — nicht doppelt zählen.
	if isV {
		for _, l := range spec {
			if !l.hasT {
				res.LinesWithoutTimestamp++
			}
		}
	}
	for _, g := range gen {
		if !g.hasT {
			res.LinesWithoutTimestamp++
		}
	}
	// Steady-Decode-TPS: zeit-/token-gewichtet über Fenster mit aktiven Requests.
	res.SteadyDecodeTPS, res.SteadySeconds, res.SteadyDroppedWindows = steadyTPS(gen)
	res.Lines = len(spec)
	if len(spec) == 0 {
		if res.SteadySeconds > 0 || res.SteadyDroppedWindows > 0 {
			// no-spec-Baseline (§4.7): Decode-Fenster vorhanden — auch wenn
			// alle Running==0 waren (Abschluss-Intervalle einer kurzen c1-
			// Basis; Review F6): kein ErrNoWindows, TPS dann 0 + dropped-Zähler.
			res.NoSpecWindows = true
			return res, nil
		}
		return res, ErrNoWindows
	}

	// Ungewichtete Zeilen-Mittel (Inventur-Schätzer) + Spannweite.
	var sumTau, sumAR float64
	res.LineTauMin, res.LineTauMax = spec[0].tau, spec[0].tau
	for _, l := range spec {
		sumTau += l.tau
		sumAR += l.ar
		res.LineTauMin = min(res.LineTauMin, l.tau)
		res.LineTauMax = max(res.LineTauMax, l.tau)
	}
	res.UnweightedLineMean = sumTau / float64(len(spec))
	res.UnweightedARLineMean = sumAR / float64(len(spec))
	res.Windows = len(spec)

	if isV {
		// Exakte Summation absoluter Intervall-Counts. vLLM loggt num_drafts
		// nicht — aber γ ist je Lauf konstant: aus Zeilen mit MAL>1 folgt
		// γ_i = drafted/(accepted/(MAL−1)); ist γ über alle Zeilen konsistent
		// (±2 %, gerundet), dann steps = ΣDrafted/γ und τ ist EXAKT
		// (Toleranz 0 gegen die Summation). Sonst (inkonsistent) Näherung aus
		// accepted/(MAL−1) mit Marker tau_derived_drafts.
		var acc, drafted int64
		var stepsApprox float64
		gammaConsistent, gammaVal := true, 0
		var perPos []float64
		var perPosW float64
		for _, l := range spec {
			acc += l.accepted
			drafted += l.drafted
			if l.tau > 1 && l.accepted > 0 {
				s := float64(l.accepted) / (l.tau - 1)
				stepsApprox += s
				g := float64(l.drafted) / s
				gi := int(g + 0.5)
				if gi < 1 || (g-float64(gi))/float64(gi) > 0.02 || (float64(gi)-g)/float64(gi) > 0.02 {
					gammaConsistent = false
				} else if gammaVal == 0 {
					gammaVal = gi
				} else if gi != gammaVal {
					gammaConsistent = false
				}
				if len(l.perPos) > 0 {
					if perPos == nil {
						perPos = make([]float64, len(l.perPos))
					}
					for i := range min(len(perPos), len(l.perPos)) {
						perPos[i] += l.perPos[i] * s
					}
					perPosW += s
				}
			}
		}
		res.AcceptedTokens, res.DraftTokens = acc, drafted
		switch {
		case gammaConsistent && gammaVal > 0 && drafted > 0:
			res.Gamma = gammaVal
			steps := float64(drafted) / float64(gammaVal)
			res.VerifySteps = int64(steps + 0.5)
			res.Tau = 1 + float64(acc)/steps
		case stepsApprox > 0:
			res.VerifySteps = int64(stepsApprox + 0.5)
			res.Tau = 1 + float64(acc)/stepsApprox
			res.TauDerivedDrafts = true
		default:
			// Alle Zeilen MAL ≤ 1 (Drafter wirkungslos): τ = 1, Steps aus γ
			// nicht ableitbar — Drafts zählen, Accepted 0.
			res.Tau = 1
			res.TauDerivedDrafts = true
		}
		if drafted > 0 {
			res.AR = float64(acc) / float64(drafted)
		}
		if perPosW > 0 {
			for i := range perPos {
				perPos[i] /= perPosW
			}
			res.PerPosition = perPos
		}
	} else {
		// SGLang: nur Verhältnisse je Zeile → Gewicht = gen_throughput × Δt
		// (Token des Fensters), τ zeitgewichtet (§4.2-Degradations-Marker).
		dts := windowSeconds(stampsOf(spec))
		var wSum, tauW, arW, steps, acc float64
		for i, l := range spec {
			dt := dts[i]
			w := l.tps * dt // emittierte Token des Fensters
			wSum += w
			tauW += l.tau * w
			arW += l.ar * w
			// Rekonstruierte Zählungen (Ordnungsgröße): steps ≈ tokens/τ,
			// accepted ≈ tokens·(τ−1)/τ.
			if l.tau > 0 {
				steps += w / l.tau
				acc += w * (l.tau - 1) / l.tau
			}
		}
		if wSum > 0 {
			res.Tau, res.AR = tauW/wSum, arW/wSum
		} else {
			res.Tau, res.AR = res.UnweightedLineMean, res.UnweightedARLineMean
		}
		res.TauTimeWeighted = true
		res.ARLowerBound = true // Nenner unterstellt volles γ je Runde (§3.1)
		res.VerifySteps, res.AcceptedTokens = int64(steps+0.5), int64(acc+0.5)
	}
	res.WeightedMinusUnweighted = res.Tau - res.UnweightedLineMean
	return res, nil
}

// stampsOf extrahiert die Zeitstempel einer Zeilenfolge (generisch für
// specLine/genLine — eine Δt-Ableitung für τ-Gewichte UND Steady-TPS).
func stampsOf[T interface{ ts() stamp }](ls []T) []stamp {
	out := make([]stamp, len(ls))
	for i, l := range ls {
		out[i] = l.ts()
	}
	return out
}

func (s stamp) ts() stamp { return s }

// windowSeconds liefert das Fenster-Δt jeder Zeile: Abstand zur Vorgängerzeile
// (erste Zeile: zur Nachfolgerzeile), gedeckelt auf maxIntervalFactor × Median
// der positiven Abstände (Achsen-Pausen gewichten nicht), Fallback
// defaultIntervalSec ohne auswertbaren Stempel. EINE Implementierung für
// τ-Gewichte und Steady-TPS (Review F10) — beide rechnen auf derselben Zeitbasis.
func windowSeconds(st []stamp) []float64 {
	var ds []float64
	for i := 1; i < len(st); i++ {
		if st[i].hasT && st[i-1].hasT {
			if d := st[i].t.Sub(st[i-1].t).Seconds(); d > 0 {
				ds = append(ds, d)
			}
		}
	}
	med := 0.0
	if len(ds) > 0 {
		sort.Float64s(ds)
		med = ds[len(ds)/2]
	}
	out := make([]float64, len(st))
	for i := range st {
		d := 0.0
		if i > 0 && st[i].hasT && st[i-1].hasT {
			d = st[i].t.Sub(st[i-1].t).Seconds()
		} else if i == 0 && len(st) > 1 && st[0].hasT && st[1].hasT {
			d = st[1].t.Sub(st[0].t).Seconds()
		}
		switch {
		case d <= 0:
			d = defaultIntervalSec
		case med > 0 && d > maxIntervalFactor*med:
			d = maxIntervalFactor * med
		}
		out[i] = d
	}
	return out
}

// steadyTPS mittelt die Decode-Raten token-/zeitgewichtet über Fenster mit
// aktiven Requests (Running > 0); Δt aus windowSeconds. Rückgabe: TPS,
// Sekunden, verworfene aktive Fenster (tps>0, Running==0 — Abschlussintervalle).
func steadyTPS(gen []genLine) (float64, float64, int) {
	dts := windowSeconds(stampsOf(gen))
	var tok, sec float64
	dropped := 0
	for i, g := range gen {
		if g.tps <= 0 {
			continue
		}
		if g.reqs <= 0 {
			dropped++
			continue
		}
		tok += g.tps * dts[i]
		sec += dts[i]
	}
	if sec == 0 {
		return 0, 0, dropped
	}
	return tok / sec, sec, dropped
}

func vllmTime(ln string) (time.Time, bool) {
	m := reVLLMTS.FindStringSubmatch(ln)
	if m == nil {
		return time.Time{}, false
	}
	mo, _ := strconv.Atoi(m[1])
	d, _ := strconv.Atoi(m[2])
	h, _ := strconv.Atoi(m[3])
	mi, _ := strconv.Atoi(m[4])
	s, _ := strconv.Atoi(m[5])
	// Jahr fehlt im vLLM-Stempel — Differenzen innerhalb eines Logs bleiben korrekt.
	return time.Date(2000, time.Month(mo), d, h, mi, s, 0, time.UTC), true
}

func sglTime(ln string) (time.Time, bool) {
	m := reSGLTS.FindStringSubmatch(ln)
	if m == nil {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	h, _ := strconv.Atoi(m[4])
	mi, _ := strconv.Atoi(m[5])
	s, _ := strconv.Atoi(m[6])
	ns := 0
	if m[7] != "" {
		ms, _ := strconv.Atoi((m[7] + "000")[:3])
		ns = ms * int(time.Millisecond)
	}
	return time.Date(y, time.Month(mo), d, h, mi, s, ns, time.UTC), true
}
