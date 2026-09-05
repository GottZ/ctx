// Package main — ctx-llmlog-export: SELECT-only-Sicherung von context_llm_log als JSONL.
//
// Drafter-Training design/02 §4.1 (KW1): rettet die Prompt-/Response-Bodies
// VOR jeder Retention-Aktivierung (EvictBodies ist hot-mutable). Eine Zeile
// pro Row, 1:1 mit dem Live-Schema (to_jsonb), Datei 0600 in einem 0700-
// Verzeichnis außerhalb von /tmp; das Fenster ist auf DB-now() − 1 min
// gepinnt und das Zähl-Gate vergleicht gegen count(*) über dasselbe Fenster.
//
//	set -a; . .env; set +a
//	CONTEXT_DB_HOST=<db-ip> ctx-llmlog-export -out staging/llmlog-2026-08-19.jsonl
//	# Delta-Lauf: exakt hinter der Watermark des Vorlaufs fortsetzen
//	ctx-llmlog-export -out staging/llmlog-delta.jsonl -since <watermark> -since-id <watermark_id>
//
// Exit-Codes: 0 = sauber · 2 = NULL-Bodies im Fenster (Export trotzdem
// VOLLSTÄNDIG geschrieben — rescue-first; mit -strict: Sofort-Abbruch) ·
// 3 = Count-Gate verletzt · 1 = alles andere (Perimeter, Config, DB,
// I/O). Das Summary geht in jedem Fall zuerst auf stderr.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run trägt die komplette CLI-Logik hinter einem eigenen FlagSet, damit der
// Exit-Code-Kontrakt testbar ist (Perimeter ⇒ 1, ohne DB-Berührung).
//
//nolint:cyclop // lineare Flag-/Exit-Verarbeitung, keine echte Verzweigungstiefe
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("ctx-llmlog-export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		outPath   = fs.String("out", "", "Ziel-JSONL (Pflicht; 0600, Verzeichnis 0700, nicht unter /tmp; existierende Datei wird nicht überschrieben)")
		sinceStr  = fs.String("since", "", "created_at >= (RFC3339; leer = kein linker Rand; Watermark des Vorlaufs für Delta-Exporte)")
		sinceID   = fs.String("since-id", "", "zusammen mit -since: Cursor EXAKT hinter (since, since-id) fortsetzen (watermark_id des Vorlaufs) — ohne -since-id landet die Gleichstands-Gruppe der Watermark erneut im Export")
		untilStr  = fs.String("until", "", "created_at < (RFC3339; leer = DB-now() − 1 min beim Start — Fenster-Pinning mit Commit-Marge)")
		pipelines = fs.String("pipeline", "", "CSV-Filter auf pipeline (leer = alle)")
		batch     = fs.Int("batch", 5000, "Keyset-Seitengröße")
		strict    = fs.Bool("strict", false, "beim ERSTEN NULL-Body sofort abbrechen (Gate-/Test-Pfad); Default = rescue-first")
		summary   = fs.String("summary", "", "optional: Zähl-Kontrakt zusätzlich als JSON-Datei (0600, gleicher Perimeter)")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	say := func(a ...any) { _, _ = fmt.Fprintln(stderr, a...) }
	fail := func(what string, err error) int {
		say("ctx-llmlog-export:", what+":", err)
		return 1
	}

	if *outPath == "" {
		say("ctx-llmlog-export: -out ist Pflicht")
		return 1
	}
	opts := llmlog.ExportOptions{BatchSize: *batch, Strict: *strict, SinceID: *sinceID}
	var err error
	if *sinceStr != "" {
		if opts.Since, err = time.Parse(time.RFC3339Nano, *sinceStr); err != nil {
			return fail("-since", err)
		}
	}
	if *sinceID != "" && *sinceStr == "" {
		return fail("-since-id", llmlog.ErrSinceID)
	}
	if *untilStr != "" {
		if opts.Until, err = time.Parse(time.RFC3339Nano, *untilStr); err != nil {
			return fail("-until", err)
		}
	}
	for p := range strings.SplitSeq(*pipelines, ",") {
		if p = strings.TrimSpace(p); p != "" {
			opts.Pipelines = append(opts.Pipelines, p)
		}
	}

	// Perimeter ZUERST — vor jeder DB-Verbindung (fail-closed); die Datei
	// selbst entsteht erst nach erfolgreichem Verbindungsaufbau, damit ein
	// Config-/DB-Fehler keine leere O_EXCL-Leiche hinterlässt.
	if err := llmlog.CheckExportDir(filepath.Dir(*outPath)); err != nil {
		return fail("perimeter", err)
	}
	if *summary != "" {
		if err := llmlog.CheckExportDir(filepath.Dir(*summary)); err != nil {
			return fail("perimeter (-summary)", err)
		}
	}

	cc, issues := config.FromEnv()
	issues = append(issues, config.Validate(cc)...)
	if config.HasErrors(issues) {
		for _, is := range issues {
			say("ctx-llmlog-export: config:", is.Field+":", is.Msg)
		}
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx, cc.DSN())
	if err != nil {
		return fail("db", err)
	}
	defer pool.Close()

	out, err := llmlog.CreateExportFile(*outPath)
	if err != nil {
		return fail("out", err)
	}
	defer func() { _ = out.Close() }()

	sum, exportErr := llmlog.Export(ctx, pool, out, opts)
	syncErr := out.Sync()

	// Zähl-Kontrakt IMMER zuerst — gerade auf Fehlerpfaden ist er der Beleg.
	line := llmlog.MarshalSummary(sum)
	say(string(line))
	if syncErr != nil {
		say("ctx-llmlog-export: fsync:", syncErr)
	}
	var summaryErr error
	if *summary != "" {
		summaryErr = writeSummary(*summary, line)
		if summaryErr != nil {
			say("ctx-llmlog-export: summary:", summaryErr)
		}
	}

	switch {
	case syncErr != nil || summaryErr != nil:
		// Vor den Export-Klassen (Review F4): Exit 2/3 bedeuten „Datei
		// vollständig, behalten" — das ist ohne erfolgreichen fsync nicht
		// belegt; ein Persistenz-Fehler ist immer Exit 1.
		return 1
	case errors.Is(exportErr, llmlog.ErrBodiesEvicted):
		say("ctx-llmlog-export: ALARM:", exportErr)
		return 2
	case errors.Is(exportErr, llmlog.ErrCountGate):
		say("ctx-llmlog-export: GATE:", exportErr)
		return 3
	case exportErr != nil:
		say("ctx-llmlog-export:", exportErr)
		return 1
	}
	return 0
}

func writeSummary(path string, line []byte) error {
	sf, err := llmlog.CreateExportFile(path)
	if err != nil {
		return err
	}
	if _, err := sf.Write(append(line, '\n')); err != nil {
		_ = sf.Close()
		return err
	}
	return sf.Close()
}
