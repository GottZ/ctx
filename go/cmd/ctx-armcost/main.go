// Package main — ctx-armcost: SELECT-only-Kostenreport über context_llm_log in GPU-Sekunden.
//
// Wissens-Ebenen design/05 §4.7 (Welle M-W7): die Währung des Mess-Programms
// ist die Belegungs-Sekunde, nicht der Dollar. cost_usd ist auf einem
// On-prem-Serving praktisch nie gesetzt und wird hier ausdrücklich NICHT als
// Kennzahl geführt, sondern nur als eine Provenienz-Zeile ausgewiesen
// („in k von n Zeilen gesetzt — nicht verwendet").
//
// Muster wörtlich nach cmd/ctx-llmlog-export: SELECT-only in kurzen READ
// ONLY-Transaktionen, Fenster gepinnt auf DB-now() − 1 min, Zähl-Gate gegen
// count(*) über dasselbe Fenster, Ausgabedatei 0600 in einem 0700-Verzeichnis
// außerhalb von /tmp. Kein Endpunkt, kein Schreibpfad, kein Produktionsaufrufer.
//
//	set -a; . .env; set +a
//	CONTEXT_DB_HOST=<db-ip> ctx-armcost -out /secure/dir/armcost-2026-08-26.json
//	ctx-armcost -out r.json -since 2026-08-19T00:00:00Z -until 2026-08-26T00:00:00Z
//	ctx-armcost -out r.json -arm=cluster-label -per-topic
//
// Exit-Codes: 0 = sauber · 2 = Aufruffehler des Flag-Paars -arm/-per-topic
// (nur zusammen gültig, nur für einen Arm mit bekannter Zuordnungs-Regel) ·
// 3 = Zähl-Gate verletzt · 4 = die Nicht-Störungs-Kennzahl reißt
// (interactive-p95 > Faktor 1,5 gegen das gleich lange Fenster davor —
// Abbruchkriterium jeder Mess-Welle) · 1 = alles andere (Perimeter, Config,
// DB, I/O). Tabelle und JSON-Datei entstehen auch bei 3 und 4: sie sind der
// Beleg. Bei 2 entsteht nichts — der Lauf hat die Datenbank nie berührt.
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
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run trägt die komplette CLI-Logik hinter einem eigenen FlagSet, damit der
// Exit-Code-Kontrakt testbar ist (Perimeter ⇒ 1, ohne DB-Berührung).
//
//nolint:cyclop // lineare Flag-/Exit-Verarbeitung, keine echte Verzweigungstiefe
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ctx-armcost", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		outPath  = fs.String("out", "", "Ziel-JSON (Pflicht; 0600, Verzeichnis 0700, nicht unter /tmp; existierende Datei wird nicht überschrieben)")
		sinceStr = fs.String("since", "", "created_at >= (RFC3339; leer = until − -days)")
		untilStr = fs.String("until", "", "created_at < (RFC3339; leer oder später = DB-now() − 1 min — Fenster-Pinning mit Commit-Marge)")
		days     = fs.Float64("days", 7, "Fensterbreite in Tagen, wenn -since fehlt")
		byClass  = fs.Bool("by-class", true, "zusätzlich je dispatch_class gruppieren")
		arm      = fs.String("arm", "", "Arm der Per-Topic-Sicht (nur zusammen mit -per-topic; heute nur "+armClusterLabel+")")
		perTopic = fs.Bool("per-topic", false, "Per-Topic-Sicht des mit -arm gewählten Arms (V-W5)")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	say := func(a ...any) { _, _ = fmt.Fprintln(stderr, a...) }
	fail := func(what string, err error) int {
		say("ctx-armcost:", what+":", err)
		return 1
	}

	if *outPath == "" {
		say("ctx-armcost: -out ist Pflicht")
		return 1
	}
	// -arm/-per-topic: nur zusammen gültig, und nur für einen Arm, dessen
	// Zuordnungs-Regel dieses Werkzeug kennt. Fail-closed VOR jeder
	// DB-Berührung — Exit 2 ist ein Aufruffehler, kein Messergebnis.
	if code, err := checkArm(*arm, *perTopic); err != nil {
		say("ctx-armcost:", err)
		return code
	}
	opts := Options{ByClass: *byClass, Arm: *arm, Window: time.Duration(*days * float64(24*time.Hour))}
	var err error
	if *sinceStr != "" {
		if opts.Since, err = time.Parse(time.RFC3339Nano, *sinceStr); err != nil {
			return fail("-since", err)
		}
	}
	if *untilStr != "" {
		if opts.Until, err = time.Parse(time.RFC3339Nano, *untilStr); err != nil {
			return fail("-until", err)
		}
	}

	// Perimeter ZUERST — vor jeder DB-Verbindung (fail-closed); die Datei
	// selbst entsteht erst nach dem Report, damit ein Config-/DB-Fehler keine
	// leere O_EXCL-Leiche hinterlässt.
	if err := llmlog.CheckExportDir(filepath.Dir(*outPath)); err != nil {
		return fail("perimeter", err)
	}

	cc, issues := config.FromEnv()
	if config.HasErrors(issues) {
		for _, is := range issues {
			say("ctx-armcost: config:", is.Field+":", is.Msg)
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

	rep, repErr := buildReport(ctx, pool, opts)
	if repErr != nil && !errors.Is(repErr, errCountGate) {
		return fail("report", repErr)
	}

	// Tabelle und Datei IMMER — gerade auf den Gate-Pfaden sind sie der Beleg.
	if err := renderTable(stdout, rep); err != nil {
		return fail("stdout", err)
	}
	if err := writeReportFile(*outPath, rep); err != nil {
		return fail("out", err)
	}

	switch {
	case errors.Is(repErr, errCountGate):
		say("ctx-armcost: GATE:", repErr)
		return 3
	case rep.Interactive.Exceeded:
		say("ctx-armcost: ABBRUCHKRITERIUM:", rep.Interactive.Note)
		return 4
	}
	return 0
}

// checkArm prüft das Flag-Paar der Per-Topic-Sicht (V-W5) und liefert den
// Exit-Code, mit dem run() abbricht. Getrennt von run(), damit der Kontrakt
// ohne CLI-Rahmen prüfbar ist.
func checkArm(arm string, perTopic bool) (int, error) {
	switch {
	case arm == "" && !perTopic:
		return 0, nil
	case arm == "":
		return 2, fmt.Errorf("-per-topic verlangt -arm=%s", armClusterLabel)
	case !perTopic:
		return 2, fmt.Errorf("-arm=%s verlangt -per-topic (eine andere Sicht auf einen einzelnen Arm gibt es nicht)", arm)
	case arm != armClusterLabel:
		return 2, fmt.Errorf("%w (bekommen: %q)", errUnsupportedArm, arm)
	}
	return 0, nil
}

// writeReportFile schreibt den Report als eingerücktes JSON in eine frische
// 0600-Datei (O_EXCL, nie überschreibend) und synct sie.
func writeReportFile(path string, rep Report) error {
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	f, err := llmlog.CreateExportFile(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
