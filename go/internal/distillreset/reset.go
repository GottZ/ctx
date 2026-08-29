// Package distillreset gibt dem Destillat-Arm die Block-Identitäten zurück,
// die eine Schatten-Messung auf einer Mess-Kopie belegt hat.
//
// # Wozu
//
// Die X-W-Messreihen bauen ihren Schatten-Korpus, indem sie die Insight-Blöcke
// des Arms per SQL auf einen Schatten-Typ umtypisieren (X-W4-Report §11 Zeile
// f/h: Registry-Zeile `session-insight-shadow`, `_global`, `builtin=false`,
// `excluded` + `shadow_measurable=true`; der Retype selbst setzt nur
// `type_name`, Content und Metadata bleiben unberührt). Entscheid E4-5 hält
// diese Praxis ausdrücklich außerhalb des Produkts: „Retype bleibt
// Mess-Werkzeug".
//
// Der Arm findet seine Identität über (category, title, scope) OHNE Typ und
// verweigert danach jeden Lauf, der denselben Wasserzeichen-Bereich noch einmal
// abdeckt (events/distill_block.go, Festlegung 4(b)). Für die Messreihe ist das
// eine Einbahnstraße: der Hinweg ist ein Einzeiler, der Rückweg war bis zu
// dieser Welle undokumentiert und endete im Re-Pilot bei „für alle 16 Wurzeln
// failed" (reports/bau/c3-3-re-pilot.md §4.2 Nr. 3). Dieses Paket IST der
// Rückweg — und sonst nichts.
//
// # Was es genau tut, und was ausdrücklich nicht
//
// Es setzt `type_name` der betroffenen Zeilen auf den Typ zurück, den der Arm
// laut Konfiguration schreibt. Es fasst `updated_at`, `type_source`, Content,
// Metadata, Sensitivität und Archiv-Flag NICHT an, und das ist eine Festlegung,
// keine Auslassung:
//
//   - `updated_at` ist der Zeitanker des Drift-Stempels einer Kampagne
//     (armsweep/drift.go). Ein Rücksetzer, der ihn hochzieht, macht aus der
//     Wiederherstellung eines Zustands eine Bewegung im Korpus — und genau die
//     sucht der Stempel als Kontamination.
//   - `type_source` blieb beim Hinweg unberührt; die exakte Umkehrung eines
//     dokumentierten Eingriffs ist die einzige Form, die keinen neuen Zustand
//     erfindet.
//
// Deshalb schreibt das Paket auch nicht über store.UpdateBlock: dessen Vertrag
// ist der Client-Schreibpfad (type_source='manual', updated_at=now()), und
// beides wäre hier ein Nebeneffekt statt einer Absicht.
//
// # Drei Gates, alle vor jedem Schreibzugriff
//
//  1. Die Instanz muss sich selbst als Mess-Kopie ausweisen
//     (`server.instance_kind`). Ohne Override — anders als beim Dump-Treiber,
//     der `-allow-live-instance` kennt: ein Live-Bestand kommt gar nicht erst
//     in diese Lage, weil der Retype Kopie-Praxis ist.
//  2. Der Quelltyp muss in der Registry stehen UND
//     `retrieval.shadow_measurable` tragen (design/05 §4.2 Gate G5). Damit ist
//     das Werkzeug kein allgemeiner Retype-Hammer: ein FREMDER Typ auf der
//     Identität des Arms ist Festlegung 4(b) in ihrem ursprünglichen Sinn und
//     bleibt abgewiesen — vom Guard wie von hier.
//  3. Der Zieltyp ist der konfigurierte Blocktyp des Arms, kein freier
//     Parameter, und er muss in der Registry stehen.
//
// Abgeleitete Blöcke (metadata.provenance, D-01 §4.5) werden gemeldet und
// übersprungen: ihre Provenienz-Kette gehört nicht dem Arm.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package distillreset

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
)

// InstanceKindMeasureCopy ist der Wert, den `server.instance_kind` tragen muss.
//
// Eigene Konstante statt eines Imports von internal/armsweep: der Sweep-Treiber
// zieht rrf, goldset und evalscore mit, und dieses Werkzeug braucht nichts
// davon. Dass die beiden Zeichenketten identisch bleiben, sichert ein Test
// (reset_test.go) — dort ist der schwere Import folgenlos.
const InstanceKindMeasureCopy = "measure-copy"

var (
	// ErrNotMeasureCopy ist Gate 1. Eine eigene Klasse, damit ein Aufrufer
	// „hier darf nicht geschrieben werden" von „das Schreiben ist gescheitert"
	// unterscheiden kann.
	ErrNotMeasureCopy = errors.New("distillreset: die Instanz ist keine Mess-Kopie")
	// ErrNotShadowType ist Gate 2: der Quelltyp trägt keine
	// Messbarkeits-Flagge, ist also kein Schatten-Typ dieser Ebene.
	ErrNotShadowType = errors.New("distillreset: der Quelltyp ist kein Schatten-Typ")
	// ErrIdentity ist Gate 3 plus die Aufruf-Vorprüfung: die Schreib-Identität
	// des Arms ist unvollständig oder der Quelltyp fehlt.
	ErrIdentity = errors.New("distillreset: unvollständige Identität")
)

// Identity ist die AUFGELÖSTE Schreib-Identität des Arms plus das
// Provenienz-Etikett der Instanz — als Werte, nicht als Konfigurations-Objekt.
//
// Das ist die F1-Schichtregel und nicht Geschmack: internal/config gehört
// cmd/**, handler, events und settings. Wer die Schlüssel liest, ist damit der
// Aufrufer (cmd/ctx-distillreset, identityFrom), und dieses Paket entscheidet
// nur noch über Werte, die es vollständig prüfen kann.
type Identity struct {
	// InstanceKind ist server.instance_kind der Instanz, an der der Pool hängt.
	InstanceKind string
	// Category, Scope und ToType sind distill.category, der aufgelöste Scope
	// des Arms und distill.block_type.
	Category string
	Scope    string
	ToType   string
}

// Options ist der Aufruf. FromType ist der einzige echte Parameter — alles
// andere kommt als Identity aus der Arm-Konfiguration, damit dieses Werkzeug
// und der Arm nicht zwei Meinungen über dieselbe Identität haben können.
type Options struct {
	// FromType ist der Schatten-Typ, auf den die Messreihe umtypisiert hat.
	FromType string
	// Apply schreibt. Ohne Apply listet der Lauf, was er schreiben würde —
	// Vorgabe, weil ein Mess-Eingriff sichtbar sein soll, bevor er passiert.
	Apply bool
}

// Row ist eine betroffene Zeile, so wie der Bericht sie ausweist.
type Row struct {
	ID    string
	Title string
}

// Result ist der vollständige Befund eines Laufs — auch der leere Lauf ist
// einer: „nichts zu tun" ist die Antwort, die den zweiten Aufruf idempotent
// macht.
type Result struct {
	InstanceKind string
	Category     string
	Scope        string
	FromType     string
	ToType       string
	// Rows sind die Zeilen, die der Lauf zurückgesetzt hat (mit Apply) bzw.
	// zurücksetzen würde (ohne).
	Rows []Row
	// Skipped sind abgeleitete Blöcke im selben Identitätsraum: gemeldet,
	// nicht angefasst.
	Skipped []Row
	Applied bool
}

// Run führt die drei Gates aus und danach genau ein Statement.
func Run(ctx context.Context, pool *pgxpool.Pool, id Identity, opts Options) (*Result, error) {
	res := &Result{
		InstanceKind: strings.TrimSpace(id.InstanceKind),
		Category:     strings.TrimSpace(id.Category),
		Scope:        strings.TrimSpace(id.Scope),
		FromType:     strings.TrimSpace(opts.FromType),
		ToType:       strings.TrimSpace(id.ToType),
	}

	// GATE 1 — die Instanz. Zuerst, vor jedem Lesezugriff auf den Korpus.
	if res.InstanceKind != InstanceKindMeasureCopy {
		return nil, fmt.Errorf("%w: server.instance_kind = %q", ErrNotMeasureCopy, res.InstanceKind)
	}
	switch {
	case res.Category == "":
		return nil, fmt.Errorf("%w: distill.category ist leer", ErrIdentity)
	case res.ToType == "":
		return nil, fmt.Errorf("%w: distill.block_type ist leer", ErrIdentity)
	case res.Scope == "":
		return nil, fmt.Errorf("%w: kein Scope aufgelöst", ErrIdentity)
	case res.FromType == "":
		return nil, fmt.Errorf("%w: kein Quelltyp genannt", ErrIdentity)
	case res.FromType == res.ToType:
		// Kein Nullvorgang, sondern ein Aufruffehler: wer den Zieltyp als
		// Quelle nennt, meint etwas anderes, als er schreibt.
		return nil, fmt.Errorf("%w: Quelltyp und Zieltyp sind beide %q", ErrIdentity, res.ToType)
	}

	// GATE 2 + 3 — die Registry, aus derselben Quelle wie der Server.
	set, err := loadTypes(ctx, pool)
	if err != nil {
		return nil, err
	}
	if !set.IsShadowMeasurable(res.FromType) {
		return nil, fmt.Errorf("%w: %q trägt kein retrieval.shadow_measurable", ErrNotShadowType, res.FromType)
	}
	if _, ok := set.Resolve(res.ToType); !ok {
		return nil, fmt.Errorf("%w: der Zieltyp %q steht nicht in der Registry", ErrIdentity, res.ToType)
	}

	if err := loadSkipped(ctx, pool, res); err != nil {
		return nil, err
	}
	if !opts.Apply {
		return res, listCandidates(ctx, pool, res)
	}
	res.Applied = true
	return res, applyReset(ctx, pool, res)
}

// loadTypes lädt die Typ-Registry über den Produktpfad (blocktype.Registry),
// nicht über eine eigene Abfrage: die Policy-Auswertung inklusive der Builtins
// ist dieselbe, die der Server fährt.
func loadTypes(ctx context.Context, pool *pgxpool.Pool) (*blocktype.Set, error) {
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		return nil, fmt.Errorf("distillreset: Typ-Registry laden: %w", err)
	}
	set := reg.Snapshot()
	if set == nil {
		return nil, errors.New("distillreset: Typ-Registry ist leer")
	}
	return set, nil
}

// candidateSQL ist der Identitätsraum des Arms auf dieser Kopie: seine
// Kategorie, sein Scope, der Schatten-Typ, nicht archiviert.
//
// ABSICHTLICH OHNE TITEL-MUSTER. Der Titel-Präfix des Arms lebt in
// internal/events; ihn hier zu wiederholen hieße, eine zweite Kopie derselben
// Konstante zu pflegen, und ein Auseinanderlaufen wäre nicht sichtbar, sondern
// nur wirkungslos. Der Lauf weist stattdessen JEDE Zeile aus, die er anfasst.
const candidateWhere = `
	 WHERE category = $1 AND scope = $2 AND type_name = $3 AND NOT is_archived`

// ownRows grenzt den Raum auf die Zeilen ein, die dem Arm gehören können —
// abgeleitete Blöcke tragen eine fremde Provenienz-Kette und bleiben liegen.
const ownRows = ` AND NOT (metadata ? '` + derived.MetadataKey + `')`

func listCandidates(ctx context.Context, pool *pgxpool.Pool, res *Result) error {
	rows, err := pool.Query(ctx, `SELECT id::text, title FROM context_blocks`+
		candidateWhere+ownRows+` ORDER BY title`,
		res.Category, res.Scope, res.FromType)
	if err != nil {
		return fmt.Errorf("distillreset: Kandidaten lesen: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Title); err != nil {
			return fmt.Errorf("distillreset: Kandidat lesen: %w", err)
		}
		res.Rows = append(res.Rows, r)
	}
	return rows.Err()
}

// loadSkipped meldet die abgeleiteten Blöcke im selben Raum. Getrennt vom
// Schreib-Statement, weil ein übersprungener Block eine AUSSAGE ist und keine
// stille Auslassung.
func loadSkipped(ctx context.Context, pool *pgxpool.Pool, res *Result) error {
	rows, err := pool.Query(ctx, `SELECT id::text, title FROM context_blocks`+
		candidateWhere+` AND metadata ? '`+derived.MetadataKey+`' ORDER BY title`,
		res.Category, res.Scope, res.FromType)
	if err != nil {
		return fmt.Errorf("distillreset: abgeleitete Blöcke lesen: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Title); err != nil {
			return fmt.Errorf("distillreset: abgeleiteten Block lesen: %w", err)
		}
		res.Skipped = append(res.Skipped, r)
	}
	return rows.Err()
}

// applyReset ist der eine Schreibvorgang: EIN Statement mit RETURNING, damit
// der Bericht die tatsächlich geschriebenen Zeilen ausweist und nicht die
// vorher gelesenen (zwischen Lesen und Schreiben liegt sonst ein Fenster, und
// ein Bericht über ein Fenster hinweg ist eine Behauptung).
//
// Kollisionsfrei ohne Sonderfall: der partielle Unique-Index
// uq_context_category_title_scope steht auf (category, title, scope) WHERE NOT
// is_archived — die umtypisierte Zeile belegt die Identität also weiterhin
// allein, ein zweiter Bewerber kann gar nicht existieren, und ein reiner
// Typwechsel berührt keine Index-Spalte.
func applyReset(ctx context.Context, pool *pgxpool.Pool, res *Result) error {
	rows, err := pool.Query(ctx, `UPDATE context_blocks SET type_name = $4`+
		candidateWhere+ownRows+` RETURNING id::text, title`,
		res.Category, res.Scope, res.FromType, res.ToType)
	if err != nil {
		return fmt.Errorf("distillreset: Rücksetzen: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Title); err != nil {
			return fmt.Errorf("distillreset: zurückgesetzte Zeile lesen: %w", err)
		}
		res.Rows = append(res.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// RETURNING kennt keine Ordnung; der Bericht schon.
	slices.SortFunc(res.Rows, func(a, b Row) int { return strings.Compare(a.Title, b.Title) })
	return nil
}
