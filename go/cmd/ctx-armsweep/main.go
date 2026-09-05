// Package main — ctx-armsweep: der Treiber des Arm-Gewichts-Sweeps (Design 04 §4.6-§4.9,
// Welle B-W5).
//
// Drei Unterkommandos, absichtlich getrennte Läufe statt eines langen Jobs:
//
//	prime  jede Gold-Query einmal ohne Pins über die admin-gegatete
//	       arm_ranks-Naht; sammelt Übersetzung und temporale Expansion als
//	       Pins, wärmt den Embed-Cache, poolt die Solo-Arm-Kandidaten von
//	       G-REAL. Nichts wird gescort.
//	dump   dieselben Queries MIT Pins; schreibt Ränge, Fusionsreihenfolge und
//	       ausgelieferte Reihenfolge als JSONL, eingeklammert von je einem
//	       Drift-Stempel.
//	score  einen Dump (oder ein Dump-Paar) unter 16 Konfigurationen neu
//	       fusioniert, je Slice gescort, über G-NOISE und G-WIN gegattert.
//	       Mit -damping-type zusätzlich die Damping-Kurve eines Blocktyps über
//	       zehn Stützstellen — eigene Report-Sektion, kein G-WIN-Urteil, und
//	       nur über Dumps ab Migration 142 (sonst Exit 4). Mit -regime-labels
//	       trägt der Report G-REAL zusätzlich als -local/-global-Zeilen aus
//	       (X-W0b); ein Fall ohne Label bricht ab (Exit 4).
//	compare zwei BEDINGUNGEN gegeneinander (design/05 §4.3, M-W3d), gegen den
//	       Rauschboden des V0/V0'-Paars derselben Kampagne. Eigenes
//	       Unterkommando, weil -dump-b in `score` das REPLIKAT ist: dort ist
//	       eine Differenz Rauschen, hier ist sie das Signal.
//
//	go build ./cmd/ctx-armsweep
//	./ctx-armsweep prime -slices G-KI,G-Q,G-REAL
//	./ctx-armsweep dump  -pins pins-20260826T090000Z.jsonl
//	./ctx-armsweep score -dump dumps/A.jsonl -dump-b dumps/B.jsonl
//	./ctx-armsweep score -dump dumps/A.jsonl -damping-type checkpoint
//	./ctx-armsweep score -dump dumps/A.jsonl -regime-labels x-w0-labels.jsonl
//	./ctx-armsweep compare -dump-base B0.jsonl -dump-cond B1.jsonl \
//	    -noise-pair V0.jsonl,V0p.jsonl
//
// Jeder Schreibvorgang ist auf `dumps/` bzw. `reports/` eingegrenzt; Override
// ist -allow-outside-goldset, und er steht im Report.
//
// `dump` kennt zusätzlich -shadow-types (M-W2, design/05 §4.2): die genannten
// Typen werden für die zwei Mess-Statements sichtbar geschaltet. Das verlangt
// eine als Mess-Kopie gestempelte Instanz (§5 B4b) — Exit 5, wenn nicht;
// Override -allow-live-instance, ebenfalls im Report ausgewiesen.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/clientconfig"
	"github.com/GottZ/ctx/internal/goldset"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ctx-armsweep:", err)
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor is the exit cascade of the whole driver, in one testable place.
//
//	2 outside the gold roots · 3 a gate refused · 4 a dump was discarded ·
//	5 a shadow dump was aimed at a production instance · 1 everything else.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, goldset.ErrOutsideGoldset):
		return 2
	case errors.Is(err, armsweep.ErrGateRefused):
		return 3
	case errors.Is(err, errDumpAborted),
		errors.Is(err, armsweep.ErrDumpPredatesTypeName),
		errors.Is(err, armsweep.ErrRegimeLabelMissing),
		errors.Is(err, armsweep.ErrStampIncongruent):
		return 4
	case errors.Is(err, armsweep.ErrNotMeasureCopy):
		// Its own code, not 3: the seam did not refuse anything here — the
		// DRIVER refused to point a shadow dump at a production corpus, and
		// a scheduler that retries on a gate refusal must not retry on this.
		return 5
	}
	return 1
}

// errDumpAborted signals that the drift protocol discarded a dump. Its own exit
// code because a scheduler has to tell "the run failed" from "the run was
// clean but the corpus moved underneath it" — the second is a finding, not a
// bug, and the artefact on disk is evidence either way.
//
// armsweep.ErrDumpPredatesTypeName shares exit 4: it is the same class of
// verdict — the run was well-formed and the DUMP was rejected, here because a
// damping curve was asked of an artefact measured before migration 142.
// armsweep.ErrStampIncongruent shares it for the third time (M-W3d): the four
// dumps of a comparison were readable and were rejected as a SET, because their
// stamps do not describe one campaign. armsweep.ErrRegimeLabelMissing is the
// fourth (X-W0b): the artefacts were fine and the requested G-REAL split was
// refused, because the X-W0 labels do not cover every case of the dump.
var errDumpAborted = errors.New("dump verworfen (Drift-Protokoll)")

// regimeLabelsUsage is the one wording of the X-W0b flag, shared by `score` and
// `compare` so the two subcommands cannot describe the same file differently.
// conditionFieldUsage names the closed list in the usage text, so an operator
// never has to guess (wave X-W3a). Not a constant: the list lives in the
// armsweep package next to the congruence table it is derived from.
func conditionFieldUsage() string {
	return "EIN Kongruenz-Feld als deklarierte BEDINGUNG dieses Vergleichs (deklarierbar: " +
		strings.Join(armsweep.DeclarableConditionFields(), ", ") + "). " +
		"Kein Freibrief: das genannte Feld darf zwischen Basis und Bedingung abweichen, jedes andere " +
		"verwirft den Dump-Satz weiterhin (Exit 4). Die Deklaration entscheidet mit, WORAUF gerechnet " +
		"wird — " + armsweep.ConditionFieldPostFusionStages + " misst die AUSGELIEFERTE Rangliste, " +
		"weil eine Post-Fusion-Stufe in den Arm-Rängen nicht existiert"
}

const regimeLabelsUsage = "X-W0-Label-Datei im Gold-Verzeichnis (üblich: " + goldset.FileRegimeLabels +
	"); trägt G-REAL zusätzlich als -local/-global-Zeilen aus. Ohne sie ist der Report der bisherige; " +
	"ein G-REAL-Fall ohne Label bricht ab (Exit 4) statt still eine Rest-Hälfte zu bilden"

// common carries the flags every subcommand shares.
type common struct {
	baseURL      string
	apiKey       string
	dir          string
	allowOutside bool
	seed         int64
	concurrency  int
	dryRun       bool
	timeout      int
	slices       string
	limit        int
	runID        string
	retries      int
	quiet        bool
	// shadowTypes/allowLive are the M-W2 measurement widening and its
	// instance-kind override (design/05 §4.2/§5 B4b).
	shadowTypes string
	allowLive   bool
	// regimeLabels is the X-W0b stratification (design/05 §4.4b). Bound ONLY by
	// the two offline subcommands: `prime` and `dump` measure, they do not
	// report slices, and a flag they accept but ignore is a lie in the usage.
	regimeLabels string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.baseURL, "base-url", "", "API-Basis-URL (Vorgabe: CTX_BASE_URL bzw. ~/.config/ctx/config)")
	fs.StringVar(&c.apiKey, "api-key", "", "Admin-API-Key (Vorgabe: CTX_KEY bzw. ~/.config/ctx/config)")
	fs.StringVar(&c.dir, "gold-dir", "", "Gold-Verzeichnis (Vorgabe: .project/"+goldset.DirName+" neben dem Repo)")
	fs.BoolVar(&c.allowOutside, "allow-outside-goldset", false, "Schreiben außerhalb der Guard-Wurzeln erlauben (wird im Report ausgewiesen)")
	fs.Int64Var(&c.seed, "seed", 20260812, "Seed für Bootstrap-CIs und Störproben")
	fs.IntVar(&c.concurrency, "concurrency", 1, "parallele Messanfragen (Vorgabe 1 — die Mess-Tx ist RepeatableRead, Off-Peak)")
	fs.BoolVar(&c.dryRun, "dry-run", false, "kein HTTP; prüft Laden, Pfad-Guard und Artefakt-Schreiben")
	fs.IntVar(&c.timeout, "timeout", 120, "HTTP-Timeout je Anfrage in Sekunden")
	// The default is the WHOLE registry. A default that measures a subset makes
	// the undercount the omission case — the one nobody checks — and that is
	// precisely how wave X-W1 ended up priming 650 of 1000 cases.
	fs.StringVar(&c.slices, "slices", strings.Join(armsweep.CanonicalSlices(), ","),
		"zu fahrende Slices als CSV (Vorgabe: alle Registry-Slices; ein unbekannter Name wird abgewiesen)")
	fs.IntVar(&c.limit, "limit", 0, "limit je Query (0 = Server-Vorgabe)")
	fs.StringVar(&c.runID, "run-id", "", "Lauf-Kennung (Vorgabe: UTC-Zeitstempel)")
	fs.IntVar(&c.retries, "retries", armsweep.DefaultRetries, "Retry-Budget je Query; danach Ausschluss")
	fs.BoolVar(&c.quiet, "quiet", false, "keine Fortschrittsmeldungen")
	fs.StringVar(&c.shadowTypes, "shadow-types", "",
		"Schatten-Typen als CSV (nur `dump`; admin-gegatet, verlangt eine Mess-Kopie)")
	fs.BoolVar(&c.allowLive, "allow-live-instance", false,
		"Schatten-Dump gegen eine NICHT als Mess-Kopie gestempelte Instanz erlauben (wird im Report ausgewiesen)")
}

// splitCSV splits a comma-separated flag value, dropping empty entries — an
// empty flag simply names nothing and must not become an entry naming "".
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *common) shadowTypeNames() []string { return splitCSV(c.shadowTypes) }

func run() error {
	if len(os.Args) < 2 {
		return errors.New("kein Unterkommando — Aufruf: ctx-armsweep <prime|dump|compare|score|goldflip> [flags]")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var c common
	c.bind(fs)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "prime":
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdPrime(ctx, &c)
	case "dump":
		pins := fs.String("pins", "", "Pin-Datei im Gold-Verzeichnis (Vorgabe: jüngste pins-*.jsonl)")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdDump(ctx, &c, *pins)
	case "compare":
		base := fs.String("dump-base", "", "Basis-Dump (Pflicht)")
		cond := fs.String("dump-cond", "", "Bedingungs-Dump (Pflicht)")
		noise := fs.String("noise-pair", "",
			"V0/V0'-Dump-Paar DERSELBEN Kampagne als CSV (Pflicht — ohne gemessenen Rauschboden verweigert der Vergleich)")
		outDir := fs.String("reports", defaultReportDir(), "Report-Verzeichnis")
		name := fs.String("name", "", "Basisname der Report-Dateien (Vorgabe: compare-<Lauf-ID des Bedingungs-Dumps>)")
		condField := fs.String("condition-field", "", conditionFieldUsage())
		fs.StringVar(&c.regimeLabels, "regime-labels", "", regimeLabelsUsage)
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdCompare(&c, *base, *cond, *noise, *outDir, *name, *condField)
	case "goldflip":
		dump := fs.String("dump", "", "Dump-Datei unter dumps/ (Pflicht)")
		goldA := fs.String("gold-a", "", "erste Gold-Variante im Gold-Verzeichnis (Pflicht — C3-4a: fable-kern)")
		goldB := fs.String("gold-b", "", "zweite Gold-Variante (Pflicht — C3-4a: judge-uebertragen)")
		base := fs.String("base", "V0", "Basis-Konfiguration der Gate-Rechnung")
		variant := fs.String("variant", "V1", "Varianten-Konfiguration — V1 ist die EINE vorregistrierte Primär-Vergleichung (§4.9)")
		slice := fs.String("slice", "", "Slice-Name im Kipp-JSON (Vorgabe: "+goldset.SliceReal+")")
		out := fs.String("out", "flip-greal.json", "Kipp-JSON im Gold-Verzeichnis")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdGoldFlip(&c, goldflipOpts{dump: *dump, goldA: *goldA, goldB: *goldB,
			base: *base, variant: *variant, slice: *slice, out: *out, seed: c.seed})
	case "score":
		dumpA := fs.String("dump", "", "Dump-Datei (Pflicht)")
		dumpB := fs.String("dump-b", "", "zweiter Dump für die V0'-Replikate (ohne ihn ist G-NOISE nicht auswertbar)")
		outDir := fs.String("reports", defaultReportDir(), "Report-Verzeichnis")
		name := fs.String("name", "", "Basisname der Report-Dateien (Vorgabe: armsweep-<Lauf-ID>)")
		damping := fs.String("damping-type", "", "Blocktyp, dessen Damping-Kurve zusätzlich gefahren wird (10 Stützstellen; verlangt Dumps ab Migration 142)")
		fs.StringVar(&c.regimeLabels, "regime-labels", "", regimeLabelsUsage)
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdScore(&c, *dumpA, *dumpB, *outDir, *name, *damping)
	default:
		return fmt.Errorf("unbekanntes Unterkommando %q", cmd)
	}
}

// ------------------------------------------------------------- plumbing.

// defaultGoldDir points at the private .project submodule. Absolute on purpose:
// agent worktrees carry no .project, and a relative default would quietly open
// a second gold directory inside a worktree — the goldset CLI takes the same
// decision for the same reason.
func defaultGoldDir() string {
	if v := os.Getenv("CTX_GOLDSET_DIR"); v != "" {
		return v
	}
	return "/compose/n8n/.project/" + goldset.DirName
}

// defaultReportDir is the plan's report sink.
func defaultReportDir() string {
	if v := os.Getenv("CTX_ARMSWEEP_REPORT_DIR"); v != "" {
		return v
	}
	return "/compose/n8n/.project/plan-lcm-lessons-2026-08-25/" + armsweep.ReportDirName
}

func (c *common) goldGuard() (*goldset.Guard, error) {
	dir := c.dir
	if dir == "" {
		dir = defaultGoldDir()
	}
	return goldset.NewGuard(dir, c.allowOutside)
}

// dumpGuard confines the dump sink to `dumps/` BENEATH the gold root. A second
// guard rather than a subpath of the first: a dump must never be writable next
// to the slice files, where a `g-*.jsonl` glob would pick it up as gold data.
func (c *common) dumpGuard(gold *goldset.Guard) (*goldset.Guard, error) {
	return goldset.NewNamedGuard(filepath.Join(gold.Root(), armsweep.DumpDirName), armsweep.DumpDirName, c.allowOutside)
}

func (c *common) reportGuard(dir string) (*goldset.Guard, error) {
	return goldset.NewNamedGuard(dir, armsweep.ReportDirName, c.allowOutside)
}

// client resolves credentials the way the ctx CLI does: explicit flags first,
// then CTX_BASE_URL/CTX_KEY, then ~/.config/ctx/config. One resolution rule for
// the whole toolchain, so an operator never has to remember which binary reads
// the config file and which does not.
func (c *common) client() (*armsweep.Client, error) {
	base, key := c.baseURL, c.apiKey
	if base == "" || key == "" {
		cfg, err := clientconfig.Load()
		if err != nil && !c.dryRun {
			return nil, err
		}
		if base == "" {
			base = cfg.BaseURL
		}
		if key == "" {
			key = cfg.Key
		}
	}
	if !c.dryRun && (base == "" || key == "") {
		return nil, fmt.Errorf("-base-url/-api-key fehlen (oder CTX_BASE_URL/CTX_KEY setzen)")
	}
	return armsweep.NewClient(base, key, time.Duration(c.timeout)*time.Second), nil
}

func (c *common) runner(gold, dumps *goldset.Guard) (*armsweep.Runner, error) {
	cl, err := c.client()
	if err != nil {
		return nil, err
	}
	r := &armsweep.Runner{
		Client: cl, GoldDir: gold, DumpDir: dumps,
		RunID: c.id(), Concurrency: c.concurrency, Retries: c.retries, DryRun: c.dryRun,
		ShadowTypes: c.shadowTypeNames(),
	}
	if c.limit > 0 {
		l := c.limit
		r.Limit = &l
	}
	if !c.quiet {
		r.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	return r, nil
}

func (c *common) id() string {
	if c.runID != "" {
		return c.runID
	}
	c.runID = armsweep.NewRunID(time.Now())
	return c.runID
}

func (c *common) sliceNames() []string { return splitCSV(c.slices) }

// buildRev reads the VCS stamp Go embedded at build time, dirty flag appended.
// Deliberately NOT `git rev-parse` in a subprocess: spawning one is an argued
// exception in this module (internal/llm/exec_ban_test.go) and a provenance
// field does not argue it. In a linked worktree Go's repository walk can land
// on the enclosing checkout, so the value identifies the BUILD.
func buildRev() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		return rev + "-dirty"
	}
	return rev
}
