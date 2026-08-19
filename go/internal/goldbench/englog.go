package goldbench

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
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
}

// ErrNoWindows: Log ohne Spec-Messzeilen (Boot-only-Capture).
var ErrNoWindows = errors.New("parse-englog: keine Messfenster (keine SpecDecoding-/accept-len-Zeilen)")

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
	// Boot-Signatur vLLM (einmal pro API-Server-Start).
	reVLLMBoot = regexp.MustCompile(`Starting vLLM server on |Initializing a V1 LLM engine`)

	// SGLang Decode-Batch-Zeile mit Spec-Feldern.
	reSGLSpec = regexp.MustCompile(`Decode batch, #running-req: (\d+),.*accept len: ([0-9.]+), accept rate: ([0-9.]+),.*gen throughput \(token/s\): ([0-9.]+)`)
	// SGLang Decode-Batch ohne Spec (Basis-Läufe) — nur Steady-TPS.
	reSGLGen = regexp.MustCompile(`Decode batch, #running-req: (\d+),.*gen throughput \(token/s\): ([0-9.]+)`)
	// SGLang Zeitstempel "[2026-08-19 00:02:25]".
	reSGLTS = regexp.MustCompile(`^\[(\d{4})-(\d\d)-(\d\d) (\d\d):(\d\d):(\d\d)\]`)
	// Boot-Signatur SGLang (einmal pro launch_server).
	reSGLBoot = regexp.MustCompile(`server_args=|The server is fired up and ready to roll`)
)

// defaultIntervalSec ist die Log-Kadenz beider Engines (10 s); sie trägt die
// Gewichtung, wenn eine Zeile keinen auswertbaren Zeitstempel-Abstand hat.
const defaultIntervalSec = 10.0

type specLine struct {
	t        time.Time
	hasT     bool
	tau      float64
	ar       float64 // 0..1
	tps      float64 // SGLang: gen throughput der Zeile; vLLM: 0 (eigene Zeile)
	accepted int64   // vLLM
	drafted  int64   // vLLM
	perPos   []float64
}

type genLine struct {
	t    time.Time
	hasT bool
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
		vSpec, sSpec []specLine
		vGen, sGen   []genLine
		vBoot, sBoot int
		vSig, sSig   bool
	)
	for sc.Scan() {
		ln := sc.Text()
		if reVLLMBoot.MatchString(ln) {
			vSig = true
			if strings.Contains(ln, "Starting vLLM server on") {
				vBoot++ // genau eine Zeile je API-Server-Start
			}
		}
		if reSGLBoot.MatchString(ln) {
			sSig = true
			if strings.Contains(ln, "server_args=") {
				sBoot++ // genau eine Zeile je launch_server
			}
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
			sGen = append(sGen, genLine{t: l.t, hasT: l.hasT, tps: l.tps, reqs: reqs})
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
		return nil, fmt.Errorf("parse-englog: read: %w", err)
	}

	isV := len(vSpec)+len(vGen) > 0 || vSig
	isS := len(sSpec)+len(sGen) > 0 || sSig
	if isV == isS { // beide oder keins
		return nil, ErrFormat
	}
	res := &EngLogResult{}
	res.SchemaVersion = 1
	res.Source = "log-parse"
	res.FormulaVerified = false // §3.1-Vorbehalt: Formeln gegen main belegt, nicht gegen den gepinnten Tag
	var spec []specLine
	var gen []genLine
	if isV {
		res.Engine, res.Format, res.BootMarkers, spec, gen = "vllm", "vllm-stdout", vBoot, vSpec, vGen
	} else {
		res.Engine, res.Format, res.BootMarkers, spec, gen = "sglang", "sglang-stdout", sBoot, sSpec, sGen
	}
	if res.BootMarkers > 1 {
		return res, fmt.Errorf("%w (%d Boot-Marker)", ErrMultiBoot, res.BootMarkers)
	}
	// Steady-Decode-TPS: zeit-/token-gewichtet über Fenster mit aktiven Requests.
	res.SteadyDecodeTPS, res.SteadySeconds = steadyTPS(gen)
	res.Lines = len(spec)
	if len(spec) == 0 {
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
		// Exakte Summation absoluter Intervall-Counts; Verify-Steps aus
		// Accepted/(MAL−1) rekonstruiert (vLLM loggt num_drafts nicht).
		var acc, drafted int64
		var steps float64
		var perPos []float64
		var perPosW float64
		for _, l := range spec {
			acc += l.accepted
			drafted += l.drafted
			if l.tau > 1 {
				s := float64(l.accepted) / (l.tau - 1)
				steps += s
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
		res.VerifySteps = int64(steps + 0.5)
		if steps > 0 {
			res.Tau = 1 + float64(acc)/steps
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
		var wSum, tauW, arW float64
		for i, l := range spec {
			w := l.tps * intervalSec(spec, i)
			if w <= 0 {
				w = l.tps * defaultIntervalSec
			}
			wSum += w
			tauW += l.tau * w
			arW += l.ar * w
		}
		if wSum > 0 {
			res.Tau, res.AR = tauW/wSum, arW/wSum
		} else {
			res.Tau, res.AR = res.UnweightedLineMean, res.UnweightedARLineMean
		}
		res.TauTimeWeighted = true
		res.ARLowerBound = true // Nenner unterstellt volles γ je Runde (§3.1)
		// Rekonstruierte Token-Zahlen (Ordnungsgröße, aus den Fenster-Gewichten):
		// accepted ≈ Σ tokens × (τ−1)/τ, verify_steps ≈ Σ tokens/τ.
		var tok, steps, acc float64
		for i, l := range spec {
			w := l.tps * intervalSec(spec, i)
			if w <= 0 {
				w = l.tps * defaultIntervalSec
			}
			tok += w
			if l.tau > 0 {
				steps += w / l.tau
				acc += w * (l.tau - 1) / l.tau
			}
		}
		res.VerifySteps, res.AcceptedTokens = int64(steps+0.5), int64(acc+0.5)
		_ = tok
	}
	res.WeightedMinusUnweighted = res.Tau - res.UnweightedLineMean
	return res, nil
}

// intervalSec liefert den Zeitabstand des Fensters i (zur Vorgängerzeile,
// für i==0 zur Nachfolgerzeile); ohne Zeitstempel 0 (Aufrufer nutzt Default).
func intervalSec(ls []specLine, i int) float64 {
	if i > 0 && ls[i].hasT && ls[i-1].hasT {
		if d := ls[i].t.Sub(ls[i-1].t).Seconds(); d > 0 && d < 600 {
			return d
		}
	}
	if i == 0 && len(ls) > 1 && ls[0].hasT && ls[1].hasT {
		if d := ls[1].t.Sub(ls[0].t).Seconds(); d > 0 && d < 600 {
			return d
		}
	}
	return 0
}

// steadyTPS mittelt die Decode-Raten token-/zeitgewichtet über Fenster mit
// aktiven Requests (Running > 0); Gewicht = Δt (Default 10 s).
func steadyTPS(gen []genLine) (float64, float64) {
	var tok, sec float64
	for i, g := range gen {
		if g.reqs <= 0 || g.tps <= 0 {
			continue
		}
		dt := defaultIntervalSec
		if i > 0 && g.hasT && gen[i-1].hasT {
			if d := g.t.Sub(gen[i-1].t).Seconds(); d > 0 && d < 600 {
				dt = d
			}
		}
		tok += g.tps * dt
		sec += dt
	}
	if sec == 0 {
		return 0, 0
	}
	return tok / sec, sec
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
	return time.Date(y, time.Month(mo), d, h, mi, s, 0, time.UTC), true
}
