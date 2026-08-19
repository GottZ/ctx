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
	Since     time.Time // inklusiv; Zeitpunkt Null = kein linker Rand
	Until     time.Time // exklusiv; Zeitpunkt Null = DB-now() beim Start
	Pipelines []string  // leer = alle
	BatchSize int       // Keyset-Seitengröße; <=0 = 5000
	// Strict bricht beim ERSTEN NULL-Body sofort ab (Gate-/Test-Pfad).
	// Default ist rescue-first: der Export läuft vollständig durch und der
	// Aufrufer sieht den Alarm im Summary (design/02 §5.1b).
	Strict bool
}

// ExportSummary ist der Zähl-Kontrakt des Exports (stderr-JSON des CLI).
type ExportSummary struct {
	Since            *time.Time `json:"since,omitempty"`
	Until            time.Time  `json:"until"`
	Pipelines        []string   `json:"pipelines,omitempty"`
	RowsTotal        int64      `json:"rows_total"`
	RowsBody         int64      `json:"rows_body"`
	RowsBodylessSlim int64      `json:"rows_bodyless_slim"`
	RowsBodylessNull int64      `json:"rows_bodyless_null"`
	// CountGate ist count(*) mit demselben Fenster im selben Snapshot —
	// muss RowsTotal gleichen (Live-Gate KW1).
	CountGate int64      `json:"count_gate"`
	Watermark *time.Time `json:"watermark,omitempty"` // max created_at im Export
	Bytes     int64      `json:"bytes"`
}

// ErrBodiesEvicted meldet mindestens einen NULL-Body im Fenster: Retention
// (EvictBodies) hat begonnen. Ohne Strict ist der Export beim Auftreten
// dieses Fehlers trotzdem VOLLSTÄNDIG geschrieben (rescue-first) — der
// Aufrufer setzt Exit ≠ 0, verwirft die Datei aber nicht.
var ErrBodiesEvicted = errors.New("llmlog export: NULL bodies in window — retention has begun")

// ErrCountGate meldet rows_total ≠ count(*) im selben Snapshot.
var ErrCountGate = errors.New("llmlog export: rows_total != count(*) in same snapshot")

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
	id        [16]byte
	body      []byte
	hasBody   bool
	anyNull   bool
}

// Export schreibt alle Zeilen des Fensters als JSONL nach w und liefert den
// Zähl-Kontrakt. Alles läuft in EINER READ ONLY / REPEATABLE READ
// Transaktion: kein Schreibpfad ist by construction möglich, und Keyset-
// Seiten plus count(*)-Gate sehen denselben Snapshot.
func Export(ctx context.Context, pool *pgxpool.Pool, w io.Writer, opts ExportOptions) (ExportSummary, error) {
	var sum ExportSummary
	if opts.BatchSize <= 0 {
		opts.BatchSize = 5000
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return sum, fmt.Errorf("llmlog export: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	until := opts.Until
	if until.IsZero() {
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&until); err != nil {
			return sum, fmt.Errorf("llmlog export: pin until: %w", err)
		}
	}
	sum.Until = until
	if !opts.Since.IsZero() {
		s := opts.Since
		sum.Since = &s
	}
	sum.Pipelines = opts.Pipelines

	// Nie nil: ein NULL-Array macht cardinality() NULL und das Fenster leer.
	pipes := append([]string{}, opts.Pipelines...)
	since := opts.Since // Zeitpunkt Null ⇒ Prädikat wird per NULL-Parameter neutralisiert
	var sinceArg *time.Time
	if !since.IsZero() {
		sinceArg = &since
	}

	bw := bufio.NewWriterSize(w, 1<<20)
	cw := &countingWriter{w: bw}

	var lastTS time.Time
	var lastID [16]byte
	first := true
	for {
		rows, err := tx.Query(ctx, exportPageSQL, sinceArg, until, pipes, lastTS, lastID, first, opts.BatchSize)
		if err != nil {
			return sum, fmt.Errorf("llmlog export: page: %w", err)
		}
		n := 0
		for rows.Next() {
			var r exportRow
			if err := rows.Scan(&r.createdAt, &r.id, &r.body, &r.hasBody, &r.anyNull); err != nil {
				rows.Close()
				return sum, fmt.Errorf("llmlog export: scan: %w", err)
			}
			n++
			sum.RowsTotal++
			switch {
			case r.anyNull:
				sum.RowsBodylessNull++
				if opts.Strict {
					rows.Close()
					_ = bw.Flush()
					return sum, fmt.Errorf("%w (strict, row %s)", ErrBodiesEvicted, r.createdAt.Format(time.RFC3339Nano))
				}
			case r.hasBody:
				sum.RowsBody++
			default:
				sum.RowsBodylessSlim++
			}
			if _, err := cw.Write(r.body); err != nil {
				rows.Close()
				return sum, fmt.Errorf("llmlog export: write: %w", err)
			}
			if _, err := cw.Write([]byte{'\n'}); err != nil {
				rows.Close()
				return sum, fmt.Errorf("llmlog export: write: %w", err)
			}
			lastTS, lastID = r.createdAt, r.id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return sum, fmt.Errorf("llmlog export: rows: %w", err)
		}
		first = false
		if n < opts.BatchSize {
			break
		}
	}
	if err := bw.Flush(); err != nil {
		return sum, fmt.Errorf("llmlog export: flush: %w", err)
	}
	sum.Bytes = cw.n
	if sum.RowsTotal > 0 {
		wm := lastTS
		sum.Watermark = &wm
	}

	if err := tx.QueryRow(ctx, exportCountSQL, sinceArg, until, pipes).Scan(&sum.CountGate); err != nil {
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

// exportWindowSQL ist das gemeinsame Fenster-Prädikat von Seiten-Cursor und
// count(*)-Gate — EINE Definition, damit beide dieselbe Menge sehen.
// $1 since (NULL = offen), $2 until (exklusiv), $3 pipelines (NULL/leer = alle).
const exportWindowSQL = `
	    created_at < $2
	AND ($1::timestamptz IS NULL OR created_at >= $1::timestamptz)
	AND (cardinality($3::text[]) = 0 OR pipeline = ANY($3::text[]))`

// exportPageSQL liefert eine Keyset-Seite (ORDER BY created_at, id — der
// Hypertable-Chunk-Ordnung folgend). $4/$5 = letzter Schlüssel, $6 = erste
// Seite (Schlüssel ignorieren), $7 = LIMIT. Die Klassifikation ist bewusst
// SQL-seitig neben dem Body: Leerstring UND NULL sind bodyless — credentials-
// Slim schreibt '' (Slimmed), EvictBodies schreibt NULL. Ein naiver
// IS NOT NULL-Filter ließe Leer-Prompts als Replay-Kandidaten durch
// (design/02 §5.2 — genau dieser Fehler steckte in der Erst-Inventur).
const exportPageSQL = `
	SELECT created_at, id, to_jsonb(t),
	       (coalesce(request_system, '') <> '' OR coalesce(request_user, '') <> '')     AS has_body,
	       (request_system IS NULL OR request_user IS NULL OR response_content IS NULL) AS any_null
	FROM context_llm_log t
	WHERE ` + exportWindowSQL + `
	  AND ($6::bool OR (created_at, id) > ($4::timestamptz, $5::uuid))
	ORDER BY created_at, id
	LIMIT $7`

const exportCountSQL = `SELECT count(*) FROM context_llm_log WHERE ` + exportWindowSQL

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
