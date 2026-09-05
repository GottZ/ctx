// Package main — ctx-distillreset: der Rückweg des Schatten-Retype im Mess-Werkzeugkasten.
//
// Wissens-Ebenen, Welle C4-5 (Nebenbefund N-15 des C3-3-Re-Piloten, Entscheid
// E5-6): eine X-W-Messreihe typisiert die Insight-Blöcke des Destillat-Arms auf
// einer Mess-Kopie per SQL auf einen Schatten-Typ um. Der Arm findet seine
// Block-Identität über (category, title, scope) OHNE Typ und verweigert danach
// jeden Lauf über denselben Wasserzeichen-Bereich — im Re-Pilot „für alle 16
// Wurzeln". Der Hinweg war ein Einzeiler, der Rückweg undokumentiert. Dieses
// Werkzeug ist der Rückweg, und sonst nichts: es setzt `type_name` zurück, und
// keine andere Spalte (Begründung in internal/distillreset).
//
//	go build ./cmd/ctx-distillreset
//	set -a; . .env; set +a
//	CONTEXT_DB_HOST=<kopie-ip> ./ctx-distillreset -from-type session-insight-shadow
//	CONTEXT_DB_HOST=<kopie-ip> ./ctx-distillreset -from-type session-insight-shadow -apply
//
// Ohne -apply listet der Lauf, was er schreiben würde, und schreibt nichts.
// Zweimal mit -apply ist derselbe Zustand: der zweite Lauf findet nichts mehr.
//
// Drei Gates, alle vor dem Schreiben: die Instanz muss sich über
// `server.instance_kind` als Mess-Kopie ausweisen (kein Override — anders als
// beim Dump-Treiber, denn ein Live-Bestand kommt gar nicht erst in diese Lage),
// der Quelltyp muss in der Registry stehen und `retrieval.shadow_measurable`
// tragen, und der Zieltyp ist der konfigurierte Blocktyp des Arms, kein freier
// Parameter. Ein FREMDER Typ auf der Identität des Arms bleibt damit
// abgewiesen — vom Guard wie von hier.
//
// Exit-Codes: 0 = sauber · 2 = Aufruffehler (ohne DB-Berührung) ·
// 3 = ein Gate hat verweigert · 5 = die Instanz ist keine Mess-Kopie ·
// 1 = alles andere (Config, DB, I/O).
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
	"strings"
	"syscall"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillreset"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// exitCodeFor ist die Exit-Kaskade an einer prüfbaren Stelle — dieselbe Form,
// die ctx-armsweep fährt, und mit derselben 5 für „auf eine Live-Instanz
// gezielt".
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, distillreset.ErrNotMeasureCopy):
		return 5
	case errors.Is(err, distillreset.ErrNotShadowType), errors.Is(err, distillreset.ErrIdentity):
		return 3
	default:
		return 1
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ctx-distillreset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		fromType = fs.String("from-type", "",
			"Schatten-Typ, auf den die Messreihe umtypisiert hat (Pflicht; muss retrieval.shadow_measurable tragen)")
		scope = fs.String("scope", "",
			"Scope des Arms (leer = distill.scope, sonst scheduler.home_scope der Instanz)")
		apply = fs.Bool("apply", false,
			"schreiben; ohne dieses Flag listet der Lauf nur, was er zurücksetzen würde")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	say := func(a ...any) { _, _ = fmt.Fprintln(stderr, a...) }

	// Aufruf-Vorprüfung VOR jeder DB-Berührung: Exit 2 ist ein Aufruffehler,
	// kein Befund über einen Korpus.
	if *fromType == "" {
		say("ctx-distillreset: -from-type ist Pflicht")
		return 2
	}

	cc, issues := config.FromEnv()
	if config.HasErrors(issues) {
		for _, is := range issues {
			say("ctx-distillreset: config:", is.Field+":", is.Msg)
		}
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx, cc.DSN())
	if err != nil {
		say("ctx-distillreset: db:", err)
		return 1
	}
	defer pool.Close()

	// Die AUFGELÖSTE Konfiguration, wie der Daemon sie liest: Env zuerst, das
	// Settings-Overlay darüber. Ein Werkzeug, das server.instance_kind oder die
	// Schreib-Identität des Arms selbst zusammenreimt, würde eine zweite
	// Meinung über dieselben Schlüssel pflegen.
	cfg, issues := settings.Bootstrap(ctx, pool, cc, issues)
	if config.HasErrors(issues) {
		for _, is := range issues {
			say("ctx-distillreset: config:", is.Field+":", is.Msg)
		}
		return 1
	}

	res, err := distillreset.Run(ctx, pool, identityFrom(cfg, *scope), distillreset.Options{
		FromType: *fromType, Apply: *apply,
	})
	if err != nil {
		say("ctx-distillreset:", err)
		return exitCodeFor(err)
	}
	render(stdout, res)
	return 0
}

// identityFrom liest die Schreib-Identität des Arms aus der aufgelösten
// Konfiguration. Sie lebt HIER und nicht im Paket, weil internal/config laut
// F1-Schichtregel cmd/**, handler, events und settings gehört — das Werkzeug
// bekommt Werte, keine Konfiguration.
//
// Der Scope spiegelt events.distillScope: der explizit gesetzte Schlüssel
// gewinnt, sonst der Home-Scope des Betreibers. Das Flag steht davor, damit
// eine Kopie mit mehreren Scopes bedienbar bleibt.
func identityFrom(cfg *config.Config, scopeFlag string) distillreset.Identity {
	scope := strings.TrimSpace(scopeFlag)
	if scope == "" {
		scope = strings.TrimSpace(cfg.Distill.Scope)
	}
	if scope == "" {
		scope = strings.TrimSpace(cfg.Scheduler.HomeScope)
	}
	return distillreset.Identity{
		InstanceKind: cfg.Server.InstanceKind,
		Category:     cfg.Distill.Category,
		Scope:        scope,
		ToType:       cfg.Distill.BlockType,
	}
}

// render ist der Beleg des Laufs: jede angefasste Zeile mit Id und Titel, und
// die Identität, über die entschieden wurde. Ein Rücksetzer, der nur eine Zahl
// meldet, ist auf einer Mess-Kopie nicht nachprüfbar.
func render(w io.Writer, res *distillreset.Result) {
	mode := "Trockenlauf (ohne -apply wird nichts geschrieben)"
	if res.Applied {
		mode = "geschrieben"
	}
	_, _ = fmt.Fprintf(w, "ctx-distillreset — %s\n", mode)
	_, _ = fmt.Fprintf(w, "  Instanz     %s\n", res.InstanceKind)
	_, _ = fmt.Fprintf(w, "  Identität   category=%s scope=%s\n", res.Category, res.Scope)
	_, _ = fmt.Fprintf(w, "  Typwechsel  %s → %s\n", res.FromType, res.ToType)
	_, _ = fmt.Fprintf(w, "  Zeilen      %d\n", len(res.Rows))
	for _, r := range res.Rows {
		_, _ = fmt.Fprintf(w, "    %s  %s\n", r.ID, r.Title)
	}
	if len(res.Skipped) > 0 {
		_, _ = fmt.Fprintf(w, "  Übersprungen (abgeleitete Blöcke, fremde Provenienz-Kette): %d\n", len(res.Skipped))
		for _, r := range res.Skipped {
			_, _ = fmt.Fprintf(w, "    %s  %s\n", r.ID, r.Title)
		}
	}
}
