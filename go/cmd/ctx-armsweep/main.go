// ctx-armsweep — der Treiber des Arm-Gewichts-Sweeps (Design 04 §4.6-§4.9,
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
//
//	go build ./cmd/ctx-armsweep
//	./ctx-armsweep prime -slices G-KI,G-Q,G-REAL
//	./ctx-armsweep dump  -pins pins-20260826T090000Z.jsonl
//	./ctx-armsweep score -dump dumps/A.jsonl -dump-b dumps/B.jsonl
//
// Jeder Schreibvorgang ist auf `dumps/` bzw. `reports/` eingegrenzt; einziger
// Override ist -allow-outside-goldset, und er steht im Report.
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
	"github.com/GottZ/ctx/internal/cli"
	"github.com/GottZ/ctx/internal/goldset"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ctx-armsweep:", err)
		switch {
		case errors.Is(err, goldset.ErrOutsideGoldset):
			os.Exit(2)
		case errors.Is(err, armsweep.ErrGateRefused):
			os.Exit(3)
		case errors.Is(err, errDumpAborted):
			os.Exit(4)
		}
		os.Exit(1)
	}
}

// errDumpAborted signals that the drift protocol discarded a dump. Its own exit
// code because a scheduler has to tell "the run failed" from "the run was
// clean but the corpus moved underneath it" — the second is a finding, not a
// bug, and the artefact on disk is evidence either way.
var errDumpAborted = errors.New("dump verworfen (Drift-Protokoll)")

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
	fs.StringVar(&c.slices, "slices", "G-KI,G-Q,G-REAL", "zu fahrende Slices als CSV")
	fs.IntVar(&c.limit, "limit", 0, "limit je Query (0 = Server-Vorgabe)")
	fs.StringVar(&c.runID, "run-id", "", "Lauf-Kennung (Vorgabe: UTC-Zeitstempel)")
	fs.IntVar(&c.retries, "retries", armsweep.DefaultRetries, "Retry-Budget je Query; danach Ausschluss")
	fs.BoolVar(&c.quiet, "quiet", false, "keine Fortschrittsmeldungen")
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("kein Unterkommando — Aufruf: ctx-armsweep <prime|dump|score> [flags]")
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
	case "score":
		dumpA := fs.String("dump", "", "Dump-Datei (Pflicht)")
		dumpB := fs.String("dump-b", "", "zweiter Dump für die V0'-Replikate (ohne ihn ist G-NOISE nicht auswertbar)")
		outDir := fs.String("reports", defaultReportDir(), "Report-Verzeichnis")
		name := fs.String("name", "", "Basisname der Report-Dateien (Vorgabe: armsweep-<Lauf-ID>)")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdScore(&c, *dumpA, *dumpB, *outDir, *name)
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
		cfg, err := cli.LoadConfig()
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

func (c *common) sliceNames() []string {
	var out []string
	for _, s := range strings.Split(c.slices, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

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
