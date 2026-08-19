package llmlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExportOptions steuert einen SELECT-only-Bulk-Export von context_llm_log
// (Drafter-Training design/02 §4.1 KW1). Das Fenster ist links offen
// (Since = Zeitpunkt Null) und rechts IMMER gepinnt: Until wird beim Start auf
// das DB-now() gesetzt, wenn der Aufrufer es leer lässt — llmlog schreibt
// asynchron weiter, und jedes Zähl-Gate vergleicht gegen count(*) mit
// demselben Prädikat, nie gegen eine historische Zahl.
type ExportOptions struct {
	Since time.Time // inklusiv; Zeitpunkt Null = kein linker Rand
	// SinceID setzt den Keyset-Cursor EXAKT hinter (Since, SinceID) fort —
	// die Watermark-Zeile des Vorlaufs und alle mit gleichem created_at davor
	// werden nicht erneut exportiert. Nur zusammen mit Since gültig.
	SinceID   string
	Until     time.Time // exklusiv; Zeitpunkt Null = DB-now() − UntilMargin beim Start
	Pipelines []string  // leer = alle
	BatchSize int       // Keyset-Seitengröße; <=0 = 5000
	// UntilMargin ist der Sicherheitsabstand des Pins zu now(): created_at ist
	// die Insert-Transaktions-Startzeit, der Commit liegt bis insertTimeout
	// später — ohne Marge wäre eine gerade committende Zeile mit
	// created_at < until für Seiten UND Gate unsichtbar und für jeden
	// Delta-Lauf verloren. <=0 = 1 Minute. Wirkt nur bei Until == Zeitpunkt Null.
	UntilMargin time.Duration
	// Strict bricht beim ERSTEN NULL-Body sofort ab (Gate-/Test-Pfad).
	// Default ist rescue-first: der Export läuft vollständig durch und der
	// Aufrufer sieht den Alarm im Summary (design/02 §5.1b).
	Strict bool
}

// ExportSummary ist der Zähl-Kontrakt des Exports (stderr-JSON des CLI).
type ExportSummary struct {
	Since            *time.Time `json:"since,omitempty"`
	SinceID          string     `json:"since_id,omitempty"`
	Until            time.Time  `json:"until"`
	Pipelines        []string   `json:"pipelines,omitempty"`
	RowsTotal        int64      `json:"rows_total"`
	RowsBody         int64      `json:"rows_body"`
	RowsBodylessSlim int64      `json:"rows_bodyless_slim"`
	RowsBodylessNull int64      `json:"rows_bodyless_null"`
	// CountGate ist count(*) mit demselben Fenster im selben Snapshot —
	// muss RowsTotal gleichen (Live-Gate KW1).
	CountGate int64      `json:"count_gate"`
	Watermark *time.Time `json:"watermark,omitempty"` // created_at der letzten Zeile
	// WatermarkID ist die id der letzten Zeile — zusammen mit Watermark der
	// exakte Cursor (-since/-since-id) des nächsten Delta-Exports.
	WatermarkID string `json:"watermark_id,omitempty"`
	Bytes       int64  `json:"bytes"` // tatsächlich geschriebene Bytes (auch auf Abbruchpfaden)
}

// ErrBodiesEvicted meldet mindestens einen NULL-Body im Fenster: Retention
// (EvictBodies) hat begonnen. Ohne Strict ist der Export beim Auftreten
// dieses Fehlers trotzdem VOLLSTÄNDIG geschrieben (rescue-first) — der
// Aufrufer setzt Exit ≠ 0, verwirft die Datei aber nicht.
var ErrBodiesEvicted = errors.New("llmlog export: NULL bodies in window — retention has begun")

// ErrCountGate meldet rows_total ≠ count(*) über dasselbe Fenster-Prädikat.
var ErrCountGate = errors.New("llmlog export: rows_total != count(*) over the same window")

// ErrSinceID meldet SinceID ohne Since oder eine unparsebare id.
var ErrSinceID = errors.New("llmlog export: -since-id requires -since and a valid uuid")

// ErrExportPerimeter meldet ein Zielverzeichnis außerhalb des
// Containment-Perimeters (world-/group-lesbar oder unter einem
// Shared-Temp-Pfad).
var ErrExportPerimeter = errors.New("llmlog export: target directory outside containment perimeter")

// sharedTempRoots sind welt-lesbare Ablagen, in die kein Prompt-Body darf
// (CLAUDE.md-Verbot: /tmp ist jederzeit anderen Prozessen exposed).
var sharedTempRoots = []string{"/tmp", "/var/tmp", "/dev/shm"}

// CheckExportDir prüft das Zielverzeichnis fail-closed: es muss existieren,
// darf keine group-/other-Bits tragen und darf (auch nach Symlink-Auflösung)
// nicht unter einem Shared-Temp-Pfad liegen.
func CheckExportDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrExportPerimeter, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrExportPerimeter, err)
	}
	for _, root := range sharedTempRoots {
		if real == root || strings.HasPrefix(real, root+string(filepath.Separator)) {
			return fmt.Errorf("%w: %s liegt unter %s", ErrExportPerimeter, real, root)
		}
	}
	st, err := os.Stat(real)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrExportPerimeter, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: %s ist kein Verzeichnis", ErrExportPerimeter, real)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s hat Modus %04o (group/other-Bits)", ErrExportPerimeter, real, st.Mode().Perm())
	}
	return nil
}

// CreateExportFile öffnet die Zieldatei mit O_CREATE|O_EXCL und Modus 0600
// (nie umask-Default) — nachdem CheckExportDir das Verzeichnis freigegeben
// hat. Eine bestehende Datei wird NICHT überschrieben.
func CreateExportFile(path string) (*os.File, error) {
	if err := CheckExportDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("llmlog export: create %s: %w", path, err)
	}
	// umask kann nur Bits entfernen — trotzdem den Zielzustand lesen (W16).
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("llmlog export: chmod %s: %w", path, err)
	}
	return f, nil
}

// exportRow ist die Zeile des Export-Cursors: die komplette Row als JSONB
// (to_jsonb(t) — 1:1 mit dem Live-Schema, keine gepflegte Spaltenliste)
// plus die drei Klassifikations-Signale, die das Gate zählt.
type exportRow struct {
	createdAt time.Time
	id        string
	body      []byte
	hasBody   bool
	anyNull   bool
}

// exportWindow baut das Fenster-Prädikat als Klausel-Liste + Argumente —
// EINE Definition für Seiten-Cursor und count(*)-Gate, damit beide dieselbe
// Menge sehen. Keine `$flag OR …`-Neutralisierung: unter einem generischen
// Plan (pgx prepared statements) faltet PostgreSQL das Flag nicht mehr und
// verliert Index-Startbedingung und Chunk-Exclusion.
func exportWindow(opts ExportOptions, until time.Time) (string, []any) {
	clauses := []string{"created_at < $1"}
	args := []any{until}
	if !opts.Since.IsZero() {
		args = append(args, opts.Since)
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", len(args)))
		if opts.SinceID != "" {
			args = append(args, opts.SinceID)
			clauses = append(clauses, fmt.Sprintf("(created_at, id) > ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args)))
		}
	}
	if len(opts.Pipelines) > 0 {
		args = append(args, opts.Pipelines)
		clauses = append(clauses, fmt.Sprintf("pipeline = ANY($%d::text[])", len(args)))
	}
	return strings.Join(clauses, " AND "), args
}

// exportSelect ist der Seiten-SELECT. Die Klassifikation ist bewusst
// SQL-seitig neben dem Body: Leerstring UND NULL sind bodyless — credentials-
// Slim schreibt den Leerstring (Slimmed), EvictBodies schreibt NULL. Ein naiver
// IS NOT NULL-Filter ließe Leer-Prompts als Replay-Kandidaten durch
// (design/02 §5.2 — genau dieser Fehler steckte in der Erst-Inventur).
const exportSelect = `
	SELECT created_at, id::text, to_jsonb(t),
	       (coalesce(request_system, '') <> '' OR coalesce(request_user, '') <> '')     AS has_body,
	       (request_system IS NULL OR request_user IS NULL OR response_content IS NULL) AS any_null
	FROM context_llm_log t
	WHERE `

// pageSQL liefert die Seiten-Query. Der Cursor trägt neben der Rowcompare das
// redundante `created_at >= $k`: logisch impliziert, aber die einzige Form,
// die der Planner als Startbedingung auf dem created_at-Index UND für die
// Hypertable-Chunk-Exclusion nutzt (Rowcompare allein scannt ab Chunk 1).
func pageSQL(where string, args []any, cursor bool, lastTS time.Time, lastID string, limit int) (string, []any) {
	if cursor {
		args = append(args, lastTS, lastID)
		where += fmt.Sprintf(" AND created_at >= $%d AND (created_at, id) > ($%d::timestamptz, $%d::uuid)",
			len(args)-1, len(args)-1, len(args))
	}
	args = append(args, limit)
	return exportSelect + where + fmt.Sprintf(" ORDER BY created_at, id LIMIT $%d", len(args)), args
}

// Export schreibt alle Zeilen des Fensters als JSONL nach w und liefert den
// Zähl-Kontrakt. Jede Seite und das count(*)-Gate laufen in einer eigenen,
// kurzen READ ONLY-Transaktion: kein Schreibpfad ist by construction
// möglich, und kein Mehrstunden-Snapshot pinnt den xmin-Horizont der
// Datenbank (Autovacuum, Retention-Janitor). Die Menge ist trotzdem stabil:
// das Fenster ist rechts geschlossen (Until mit Marge), die Tabelle im
// Fenster append-only, und Retention NULLt Bodies statt Zeilen zu löschen.
//
// Bytes und Watermark im Summary sind auf JEDEM Rückweg gültig — auch nach
// ctx-Cancel oder Schreibfehler steht die Datei geflusht da und der Cursor
// des nächsten Laufs ist bekannt.
func Export(ctx context.Context, pool *pgxpool.Pool, w io.Writer, opts ExportOptions) (sum ExportSummary, err error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 5000
	}
	if opts.SinceID != "" {
		if opts.Since.IsZero() {
			return sum, ErrSinceID
		}
		if _, perr := uuid.Parse(opts.SinceID); perr != nil {
			return sum, fmt.Errorf("%w: %w", ErrSinceID, perr)
		}
	}
	margin := opts.UntilMargin
	if margin <= 0 {
		margin = time.Minute
	}

	until := opts.Until
	if until.IsZero() {
		if err := pool.QueryRow(ctx, `SELECT now() - $1::interval`, margin).Scan(&until); err != nil {
			return sum, fmt.Errorf("llmlog export: pin until: %w", err)
		}
	}
	sum.Until = until
	if !opts.Since.IsZero() {
		s := opts.Since
		sum.Since = &s
		sum.SinceID = opts.SinceID
	}
	sum.Pipelines = opts.Pipelines
	where, args := exportWindow(opts, until)

	bw := bufio.NewWriterSize(w, 1<<20)
	cw := &countingWriter{w: bw}
	var lastTS time.Time
	var lastID string
	defer func() {
		if ferr := bw.Flush(); ferr != nil {
			err = errors.Join(err, fmt.Errorf("llmlog export: flush: %w", ferr))
		}
		sum.Bytes = cw.n
		if sum.RowsTotal > 0 && lastID != "" {
			wm := lastTS
			sum.Watermark = &wm
			sum.WatermarkID = lastID
		}
	}()

	cursor := false
	for {
		q, qargs := pageSQL(where, args, cursor, lastTS, lastID, opts.BatchSize)
		n, perr := exportPage(ctx, pool, cw, q, qargs, opts.Strict, &sum, &lastTS, &lastID)
		if perr != nil {
			return sum, perr
		}
		cursor = true
		if n < opts.BatchSize {
			break
		}
	}

	if err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM context_llm_log WHERE "+where, args...).Scan(&sum.CountGate)
	}); err != nil {
		return sum, fmt.Errorf("llmlog export: count gate: %w", err)
	}
	if sum.CountGate != sum.RowsTotal {
		return sum, fmt.Errorf("%w: rows_total=%d count=%d", ErrCountGate, sum.RowsTotal, sum.CountGate)
	}
	if sum.RowsBodylessNull > 0 {
		return sum, fmt.Errorf("%w: rows_bodyless_null=%d", ErrBodiesEvicted, sum.RowsBodylessNull)
	}
	return sum, nil
}

// readOnly führt fn in einer kurzen READ ONLY-Transaktion aus.
func readOnly(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// exportPage streamt eine Keyset-Seite in den Writer und zählt. Rückgabe =
// gelesene Zeilen der Seite (< BatchSize ⇒ letzte Seite).
func exportPage(ctx context.Context, pool *pgxpool.Pool, cw io.Writer, q string, args []any,
	strict bool, sum *ExportSummary, lastTS *time.Time, lastID *string,
) (int, error) {
	n := 0
	err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("llmlog export: page: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r exportRow
			if err := rows.Scan(&r.createdAt, &r.id, &r.body, &r.hasBody, &r.anyNull); err != nil {
				return fmt.Errorf("llmlog export: scan: %w", err)
			}
			n++
			sum.RowsTotal++
			switch {
			case r.anyNull:
				sum.RowsBodylessNull++
				if strict {
					return fmt.Errorf("%w (strict, row %s)", ErrBodiesEvicted, r.createdAt.Format(time.RFC3339Nano))
				}
			case r.hasBody:
				sum.RowsBody++
			default:
				sum.RowsBodylessSlim++
			}
			if _, err := cw.Write(append(r.body, '\n')); err != nil {
				return fmt.Errorf("llmlog export: write: %w", err)
			}
			*lastTS, *lastID = r.createdAt, r.id
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("llmlog export: rows: %w", err)
		}
		return nil
	})
	return n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// MarshalSummary rendert den Zähl-Kontrakt als eine JSON-Zeile.
func MarshalSummary(s ExportSummary) []byte {
	b, _ := json.Marshal(s)
	return b
}
