// ctx-goldbench — Benchmark-Harness für die ctx-LLM-Pipelines.
//
// Spielt die echten ctx-Prompts (via bench_exports.go-Shims) gegen ein
// beliebiges OpenAI-kompatibles Modell ab, parst mit den ctx-treuen Parsern
// und scored gegen die Gold-Daten aus dem ctx-bench-Repo (github.com/GottZ/ctx-bench).
//
//	go run ./cmd/ctx-goldbench -endpoint http://host:8080 -model qwen3.5:9b -axes all
//	go run ./cmd/ctx-goldbench -data /path/to/ctx-bench/data -dry-run -axes all
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/GottZ/ctx/internal/goldbench"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ctx-goldbench:", err)
		os.Exit(1)
	}
}

//nolint:cyclop // lineare Flag-Verarbeitung, keine echte Verzweigungstiefe
func run() error {
	var (
		dataDir     = flag.String("data", "", "Verzeichnis der Gold-Daten (ctx-bench-Repo, Pflicht)")
		endpoint    = flag.String("endpoint", "", "OpenAI-kompatibler Endpoint (Basis-URL oder volle /v1/chat/completions-URL)")
		model       = flag.String("model", "", "Modellname für den Request-Body")
		apiKey      = flag.String("api-key", "", "API-Key (optional; sonst env GOLDBENCH_API_KEY)")
		axesFlag    = flag.String("axes", "all", "Achsen als CSV oder 'all'")
		n           = flag.Int("n", 0, "Limit pro Achse (0 = alle Fälle)")
		concurrency = flag.Int("concurrency", 4, "parallele LLM-Calls")
		outPath     = flag.String("out", "goldbench-report.json", "Pfad des JSON-Reports")
		mdPath      = flag.String("md", "", "Pfad des Markdown-Reports (optional)")
		dryRun      = flag.Bool("dry-run", false, "kein HTTP; validiert Daten + Prompt-Bau, Scorer sehen leere Outputs")
		seed        = flag.Int64("seed", 20260812, "Seed für Case-Sampling + Request-Seed")
		timeoutSec  = flag.Int("timeout", 120, "HTTP-Timeout pro Call in Sekunden")
		verbose     = flag.Bool("verbose", false, "per_case-Ergebnisse in den JSON-Report aufnehmen")
		serverNote  = flag.String("server-note", "", "Provenienz-Notiz (Server-Build/Flags) für den Env-Stamp")
		maxTokMult  = flag.Float64("max-tokens-mult", 1, "skaliert das per-Achse-max_tokens-Budget (>1 = dokumentierte Abweichung von der Pipeline-Treue, für Reasoning-Modelle)")
		extraBody   = flag.String("extra-body", "", "JSON-Objekt, das in jeden Chat-Request gemerged wird (z. B. '{\"chat_template_kwargs\":{\"enable_thinking\":false}}')")
		tempOv      = flag.Float64("temperature-override", -1, "fixe Temperatur statt der Pipeline-Temperaturen (<0 = aus; dokumentierte Mock-Treue-Abweichung für modellkarten-pure Läufe)")
		dumpOut     = flag.String("dump-outputs", "", "JSONL-Pfad für die rohen Modell-Antworten (Dump-v2: axis,id,outputs + system/user/params/usage je Request-Slot, gen; 0600; Basis für offline-Re-Scoring)")
		specConfig  = flag.String("spec-config", "", "JSON-Objekt {algorithm,drafter_path,drafter_sha256,gamma,engine_build,target_quant,kv_cache_dtype,train_step} — strukturierte Spec-Provenienz im Report-Env (drafter_path lokal lesbar ⇒ sha256 wird selbst berechnet und gegen die Deklaration geprüft)")
		genStamp    = flag.String("gen-stamp", "", "JSON-Objekt {engine,engine_version,image,template_sha256} — Engine-Stempel je Dump-Zeile (Dump-v2, Korpus-Homogenität)")
	)
	flag.Parse()

	if !*dryRun {
		if *endpoint == "" {
			return fmt.Errorf("-endpoint fehlt (oder -dry-run nutzen)")
		}
		if *model == "" {
			return fmt.Errorf("-model fehlt (oder -dry-run nutzen)")
		}
	}

	key := *apiKey
	if key == "" {
		key = os.Getenv("GOLDBENCH_API_KEY")
	}

	dir := *dataDir
	if dir == "" {
		return fmt.Errorf("-data ist Pflicht: Pfad zum data/-Verzeichnis des ctx-bench-Repos (github.com/GottZ/ctx-bench)")
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("gold-daten-verzeichnis: %w", err)
	}

	var axes []string
	if strings.TrimSpace(*axesFlag) != "all" {
		for _, a := range strings.Split(*axesFlag, ",") {
			if a = strings.TrimSpace(a); a != "" {
				axes = append(axes, a)
			}
		}
	}

	cfg := goldbench.Config{
		DataDir:     dir,
		Endpoint:    *endpoint,
		Model:       *model,
		APIKey:      key,
		Axes:        axes,
		N:           *n,
		Concurrency: *concurrency,
		DryRun:      *dryRun,
		Seed:        *seed,
		TimeoutSec:  *timeoutSec,
		Verbose:     *verbose,
		ServerNote:  *serverNote,
		GitRev:      gitRev(),
		MaxTokensMult: *maxTokMult,
		ExtraBody:     *extraBody,
		TempOverride:  *tempOv,
		DumpOutputs:   *dumpOut,
	}

	if *genStamp != "" {
		var gs goldbench.GenStamp
		dec := json.NewDecoder(strings.NewReader(*genStamp))
		dec.DisallowUnknownFields() // Tippfehler im Feldnamen ⇒ Fehler, kein leerer Stempel
		if err := dec.Decode(&gs); err != nil {
			return fmt.Errorf("-gen-stamp: %w (erlaubt: engine, engine_version, image, template_sha256)", err)
		}
		if gs == (goldbench.GenStamp{}) {
			return fmt.Errorf("-gen-stamp: leerer Stempel")
		}
		cfg.GenStamp = &gs
	}

	if *specConfig != "" {
		sc, err := goldbench.ParseSpecConfig(*specConfig)
		if err != nil {
			return err
		}
		if err := goldbench.ResolveDrafterSHA(sc); err != nil {
			return err
		}
		cfg.SpecConfig = sc
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	report, err := goldbench.Run(ctx, cfg)
	if err != nil {
		return err
	}

	if err := goldbench.WriteJSON(report, *outPath); err != nil {
		return err
	}
	if *mdPath != "" {
		if err := goldbench.WriteMarkdown(report, *mdPath); err != nil {
			return err
		}
	}

	fmt.Print(goldbench.Markdown(report))
	fmt.Printf("\nJSON-Report: %s\n", *outPath)
	return nil
}

// gitRev liefert die kurze Revision für den Env-Stamp ("" wenn nicht
// ermittelbar) — dateibasiert statt per git-Subprozess (exec-Ban-Gate):
// .git/HEAD lesen, symbolische Refs über die Ref-Datei bzw. packed-refs
// auflösen. Worktrees (".git"-DATEI mit gitdir:-Zeile) werden verfolgt.
func gitRev() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if rev := revFromGitDir(filepath.Join(dir, ".git")); rev != "" {
			return rev
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// revFromGitDir löst HEAD eines .git-Pfads (Verzeichnis oder Worktree-Datei) auf.
func revFromGitDir(gitPath string) string {
	st, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if !st.IsDir() {
		// Worktree/Submodule: Datei mit "gitdir: <pfad>".
		b, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		line := strings.TrimSpace(string(b))
		if !strings.HasPrefix(line, "gitdir:") {
			return ""
		}
		target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(gitPath), target)
		}
		gitPath = target
	}
	head, err := os.ReadFile(filepath.Join(gitPath, "HEAD")) //nolint:gosec // G703: repo-lokale git-Metadaten für den Env-Stamp, Pfad aus cwd-Aufstieg
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(head))
	if !strings.HasPrefix(ref, "ref:") {
		return shortRev(ref)
	}
	refName := strings.TrimSpace(strings.TrimPrefix(ref, "ref:"))
	// Direkte Ref-Datei (auch commondir-Fälle für Worktrees prüfen).
	for _, base := range []string{gitPath, commonGitDir(gitPath)} {
		if base == "" {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(refName))); err == nil { //nolint:gosec // G703: repo-lokale Ref-Datei, refName aus HEAD des eigenen Repos
			return shortRev(strings.TrimSpace(string(b)))
		}
		if rev := revFromPackedRefs(filepath.Join(base, "packed-refs"), refName); rev != "" {
			return rev
		}
	}
	return ""
}

// commonGitDir liest die commondir-Datei eines Worktree-gitdirs ("" wenn keine).
func commonGitDir(gitPath string) string {
	b, err := os.ReadFile(filepath.Join(gitPath, "commondir")) //nolint:gosec // G703: repo-lokale git-Metadaten, Pfad aus cwd-Aufstieg
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(b))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitPath, common)
	}
	return common
}

// revFromPackedRefs sucht refName in einer packed-refs-Datei.
func revFromPackedRefs(path, refName string) string {
	b, err := os.ReadFile(path) //nolint:gosec // G703: repo-lokale packed-refs, Pfad aus cwd-Aufstieg
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == refName {
			return shortRev(fields[0])
		}
	}
	return ""
}

// shortRev kürzt eine Commit-Hash auf die üblichen 7 Zeichen.
func shortRev(rev string) string {
	if len(rev) < 7 || strings.ContainsAny(rev, " \t") {
		return ""
	}
	return rev[:7]
}
