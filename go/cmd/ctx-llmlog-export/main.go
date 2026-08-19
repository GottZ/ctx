// ctx-llmlog-export — SELECT-only-Sicherung von context_llm_log als JSONL.
//
// Drafter-Training design/02 §4.1 (KW1): rettet die Prompt-/Response-Bodies
// VOR jeder Retention-Aktivierung (EvictBodies ist hot-mutable). Eine Zeile
// pro Row, 1:1 mit dem Live-Schema (to_jsonb), Datei 0600 in einem 0700-
// Verzeichnis außerhalb von /tmp; das Fenster ist auf DB-now() gepinnt und
// das Zähl-Gate vergleicht gegen count(*) im selben Snapshot.
//
//	set -a; . .env; set +a
//	CONTEXT_DB_HOST=<db-ip> ctx-llmlog-export -out staging/llmlog-2026-08-19.jsonl
//
// Exit-Codes: 0 = sauber · 2 = NULL-Bodies im Fenster (Export trotzdem
// VOLLSTÄNDIG geschrieben — rescue-first; mit -strict: Sofort-Abbruch) ·
// 3 = Count-Gate verletzt · 1 = alles andere.
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
	"strings"
	"syscall"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		outPath   = flag.String("out", "", "Ziel-JSONL (Pflicht; 0600, Verzeichnis 0700, nicht unter /tmp; existierende Datei wird nicht überschrieben)")
		sinceStr  = flag.String("since", "", "created_at >= (RFC3339; leer = kein linker Rand; Watermark des Vorlaufs für Delta-Exporte)")
		untilStr  = flag.String("until", "", "created_at < (RFC3339; leer = DB-now() beim Start — Fenster-Pinning)")
		pipelines = flag.String("pipeline", "", "CSV-Filter auf pipeline (leer = alle)")
		batch     = flag.Int("batch", 5000, "Keyset-Seitengröße")
		strict    = flag.Bool("strict", false, "beim ERSTEN NULL-Body sofort abbrechen (Gate-/Test-Pfad); Default = rescue-first")
		summary   = flag.String("summary", "", "optional: Zähl-Kontrakt zusätzlich als JSON-Datei (0600, gleicher Perimeter)")
	)
	flag.Parse()

	if *outPath == "" {
		fmt.Fprintln(os.Stderr, "ctx-llmlog-export: -out ist Pflicht")
		return 1
	}
	opts := llmlog.ExportOptions{BatchSize: *batch, Strict: *strict}
	var err error
	if *sinceStr != "" {
		if opts.Since, err = time.Parse(time.RFC3339Nano, *sinceStr); err != nil {
			fmt.Fprintln(os.Stderr, "ctx-llmlog-export: -since:", err)
			return 1
		}
	}
	if *untilStr != "" {
		if opts.Until, err = time.Parse(time.RFC3339Nano, *untilStr); err != nil {
			fmt.Fprintln(os.Stderr, "ctx-llmlog-export: -until:", err)
			return 1
		}
	}
	for _, p := range strings.Split(*pipelines, ",") {
		if p = strings.TrimSpace(p); p != "" {
			opts.Pipelines = append(opts.Pipelines, p)
		}
	}

	// Perimeter ZUERST — vor jeder DB-Verbindung (fail-closed, kein
	// halb-geöffneter Zustand).
	out, err := llmlog.CreateExportFile(*outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ctx-llmlog-export:", err)
		return 1
	}
	defer func() { _ = out.Close() }()

	cc, issues := config.FromEnv()
	if config.HasErrors(issues) {
		for _, is := range issues {
			fmt.Fprintln(os.Stderr, "ctx-llmlog-export: config:", is)
		}
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx, cc.DSN())
	if err != nil {
		fmt.Fprintln(os.Stderr, "ctx-llmlog-export: db:", err)
		return 1
	}
	defer pool.Close()

	sum, exportErr := llmlog.Export(ctx, pool, out, opts)
	if err := out.Sync(); err != nil {
		fmt.Fprintln(os.Stderr, "ctx-llmlog-export: fsync:", err)
		return 1
	}
	line := llmlog.MarshalSummary(sum)
	fmt.Fprintln(os.Stderr, string(line))
	if *summary != "" {
		sf, err := llmlog.CreateExportFile(*summary)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ctx-llmlog-export: summary:", err)
			return 1
		}
		if _, err := sf.Write(append(line, '\n')); err != nil {
			fmt.Fprintln(os.Stderr, "ctx-llmlog-export: summary:", err)
			return 1
		}
		if err := sf.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "ctx-llmlog-export: summary:", err)
			return 1
		}
	}

	switch {
	case exportErr == nil:
		return 0
	case errors.Is(exportErr, llmlog.ErrBodiesEvicted):
		fmt.Fprintln(os.Stderr, "ctx-llmlog-export: ALARM:", exportErr)
		return 2
	case errors.Is(exportErr, llmlog.ErrCountGate):
		fmt.Fprintln(os.Stderr, "ctx-llmlog-export: GATE:", exportErr)
		return 3
	default:
		fmt.Fprintln(os.Stderr, "ctx-llmlog-export:", exportErr)
		return 1
	}
}
