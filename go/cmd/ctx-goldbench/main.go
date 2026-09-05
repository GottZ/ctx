// Package main — ctx-goldbench: Benchmark-Harness für die ctx-LLM-Pipelines.
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
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/GottZ/ctx/internal/goldbench"
	"github.com/GottZ/ctx/internal/provenance"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ctx-goldbench:", err)
		switch {
		case errors.Is(err, goldbench.ErrNoWindows):
			os.Exit(2)
		case errors.Is(err, goldbench.ErrMultiBoot), errors.Is(err, goldbench.ErrFormat):
			os.Exit(3)
		}
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
		samples     = flag.Int("samples", 1, "Samples je Request (KW4): Sample 0 = bisheriger Request, s>0 mit Seed+s; temp-0-Requests nur 1×; outputs/Report bleiben Sample 0, alle Samples im Dump (samples/samples_usage)")
		dumpAppend  = flag.Bool("dump-append", false, "mit -dump-outputs: inkrementeller Dump (Fall sofort geschrieben, O_APPEND) + Fall-Resume — bereits gedumpte (axis,id) werden übersprungen; gen-Stempel muss zur Datei passen (KW3)")
		dumpOut     = flag.String("dump-outputs", "", "JSONL-Pfad für die rohen Modell-Antworten (Dump-v2: axis,id,outputs + system/user/params/usage je Request-Slot, gen; 0600; Basis für offline-Re-Scoring)")
		specConfig  = flag.String("spec-config", "", "JSON-Objekt {algorithm,drafter_path,drafter_sha256,gamma,engine_build,target_quant,kv_cache_dtype,train_step} — strukturierte Spec-Provenienz im Report-Env (drafter_path lokal lesbar ⇒ sha256 wird selbst berechnet und gegen die Deklaration geprüft)")
		parseLog    = flag.String("parse-englog", "", "Standalone-Modus: Engine-Stdout-Log (vLLM/SGLang, auto-erkannt) parsen und SpecStats-JSON auf stdout ausgeben; kein Bench-Lauf (Exit 2 = keine Messfenster, 3 = mehrere Boots/Format)")
		genStamp    = flag.String("gen-stamp", "", "JSON-Objekt {engine,engine_version,image,template_sha256,model} — Engine-Stempel je Dump-Zeile (Dump-v2, Korpus-Homogenität; Pflicht mit -dump-append)")
	)
	flag.Parse()

	if *parseLog != "" {
		return parseEngLog(*parseLog)
	}

	if !*dryRun {
		if *endpoint == "" {
			return fmt.Errorf("-endpoint fehlt (oder -dry-run nutzen)")
		}
		if *model == "" {
			return fmt.Errorf("-model fehlt (oder -dry-run nutzen)")
		}
	}

	key := resolveAPIKey(*apiKey)

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

	// Der Env-Stamp nennt die Revision des ARBEITSVERZEICHNISSES (7 Zeichen,
	// ohne Dirty-Flag) — nicht die des Builds. Ist das cwd nicht ermittelbar,
	// bleibt der Stempel leer, wie bisher.
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		wd = ""
	}

	cfg := goldbench.Config{
		DataDir:       dir,
		Endpoint:      *endpoint,
		Model:         *model,
		APIKey:        key,
		Axes:          axes,
		N:             *n,
		Concurrency:   *concurrency,
		DryRun:        *dryRun,
		Seed:          *seed,
		TimeoutSec:    *timeoutSec,
		Verbose:       *verbose,
		ServerNote:    *serverNote,
		GitRev:        provenance.WorktreeRev(wd),
		MaxTokensMult: *maxTokMult,
		ExtraBody:     *extraBody,
		TempOverride:  *tempOv,
		DumpOutputs:   *dumpOut,
		DumpAppend:    *dumpAppend,
		Samples:       *samples,
	}

	if *samples < 1 || *samples > 64 {
		return fmt.Errorf("-samples muss zwischen 1 und 64 liegen (ist %d)", *samples)
	}
	if *samples > 1 && *dumpOut == "" && !*dryRun {
		return fmt.Errorf("-samples >1 braucht -dump-outputs (die Samples landen nur im Dump)")
	}
	if *samples > 1 && *seed < 0 {
		return fmt.Errorf("-samples >1 braucht -seed >= 0")
	}
	if *samples > 1 && *tempOv >= 0 {
		return fmt.Errorf("-samples >1 mit -temperature-override ist kein Korpus-Pfad (verfälschte Verteilung) — Pipeline-Temperaturen nutzen")
	}
	if *dumpAppend && *dumpOut == "" {
		return fmt.Errorf("-dump-append braucht -dump-outputs")
	}
	if *dumpAppend && *genStamp == "" {
		return fmt.Errorf("-dump-append braucht -gen-stamp (Stamp-Resume-Gate; engine/engine_version/image/template_sha256/model)")
	}
	if *dumpAppend && *dryRun {
		return fmt.Errorf("-dump-append mit -dry-run ist sinnlos (Dry-Run schreibt nie) — -dry-run ohne -dump-append fahren")
	}

	if *genStamp != "" {
		var gs goldbench.GenStamp
		if err := goldbench.DecodeStrictObject(*genStamp, &gs); err != nil {
			return fmt.Errorf("-gen-stamp: %w (erlaubt: engine, engine_version, image, template_sha256, model)", err)
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

// resolveAPIKey löst den API-Key auf: -api-key schlägt GOLDBENCH_API_KEY, und
// ohne beides bleibt er leer (der Endpoint entscheidet dann, ob er das
// akzeptiert). Der Key steht bewusst nicht in einem Flag-Default, damit er
// nicht in der Prozessliste landet.
func resolveAPIKey(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("GOLDBENCH_API_KEY")
}

// parseEngLog implementiert -parse-englog: Datei lesen, SpecStats-JSON auf
// stdout — auch bei Fehlern (dann mit `error`-Feld), Exit-Code über den Fehler.
func parseEngLog(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("-parse-englog: %w", err)
	}
	defer func() { _ = f.Close() }()
	res, perr := goldbench.ParseEngLog(f)
	if res != nil {
		if perr != nil {
			res.Error = perr.Error() // Leser von stdout sieht den Grund, nicht nur tau 0
		}
		b, _ := json.MarshalIndent(res, "", " ")
		fmt.Println(string(b))
	}
	return perr
}
