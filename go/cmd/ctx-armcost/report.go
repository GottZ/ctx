package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// occupancyExpr ist der Belegungs-Term aus design/05 §4.7 — die Währung des
// Mess-Programms. Er steht als eigene Konstante, damit die Negativ-Probe der
// Welle (eine Variante OHNE den Abzug) ihn im fertigen SQL ersetzen und real
// gegen dieselbe Fixture fahren kann, statt ihn nachzubauen.
const occupancyExpr = `(duration_ms - COALESCE(queue_wait_ms, 0))`

// bucketMetrics ist der gemeinsame Kennzahlen-Rumpf beider Gruppierungen. Er
// ist eine Konstante und wird NIE zur Laufzeit zusammengesetzt: die
// Gruppierungsspalte kommt aus zwei fertigen Konstanten (pipelineBucketSQL /
// classBucketSQL), nicht aus einem Format-String.
const bucketMetrics = `
	       count(*)                                                                 AS n,
	       COALESCE(sum(` + occupancyExpr + `), 0)::double precision / 1000.0        AS occupancy_seconds,
	       COALESCE(sum(duration_ms), 0)::double precision / 1000.0                  AS wire_seconds,
	       COALESCE(percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms::double precision), 0) AS p50_ms,
	       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms::double precision), 0) AS p95_ms,
	       COALESCE(sum(prompt_tokens), 0)                                           AS prompt_tokens,
	       count(*) FILTER (WHERE prompt_tokens IS NULL)                             AS prompt_tokens_null,
	       COALESCE(sum(completion_tokens), 0)                                       AS completion_tokens,
	       count(*) FILTER (WHERE completion_tokens IS NULL)                         AS completion_tokens_null,
	       count(*) FILTER (WHERE error IS NOT NULL)                                 AS errors,
	       count(*) FILTER (WHERE dispatch_abort IS NOT NULL)                        AS dispatch_aborts,
	       count(*) FILTER (WHERE duration_ms IS NULL)                               AS duration_null,
	       count(*) FILTER (WHERE queue_wait_ms IS NULL)                             AS queue_wait_null
	  FROM context_llm_log
	 WHERE created_at >= $1 AND created_at < $2
	 GROUP BY 1`

// pipelineBucketSQL und classBucketSQL sind die beiden erlaubten
// Gruppierungen — vollständig konstant, keine dynamische Spaltenwahl.
const pipelineBucketSQL = `SELECT COALESCE(pipeline, '(null)') AS bucket,` + bucketMetrics

// classBucketSQL gruppiert dieselben Kennzahlen je dispatch_class.
const classBucketSQL = `SELECT COALESCE(dispatch_class, '(null)') AS bucket,` + bucketMetrics

// windowStatsSQL liefert das Zähl-Gate (count(*) über dasselbe Fenster) und
// die cost_usd-Provenienz in einem Durchgang. count(cost_usd) zählt genau die
// Zeilen mit gesetztem Wert — das ist die Zahl der Pflicht-Zeile.
const windowStatsSQL = `
	SELECT count(*), count(cost_usd)
	  FROM context_llm_log
	 WHERE created_at >= $1 AND created_at < $2`

// interactiveP95SQL misst die Nicht-Störungs-Kennzahl (design/05 §4.7). Die
// Basis ist count(duration_ms), nicht count(*): Zeilen ohne Wire-Call tragen
// NULL und gehen in kein Perzentil ein.
const interactiveP95SQL = `
	SELECT count(duration_ms),
	       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms::double precision), 0)
	  FROM context_llm_log
	 WHERE dispatch_class = 'interactive'
	   AND created_at >= $1 AND created_at < $2`

// pinUntilSQL deckelt das Fenster auf DB-now() minus Commit-Marge — dieselbe
// Begründung wie beim Export-Muster: created_at ist die Startzeit der
// Insert-Transaktion, der Commit landet bis zum Insert-Timeout später.
const pinUntilSQL = `SELECT now() - $1::interval`

// abortThreshold ist die verbindliche Abbruchschranke des Mess-Programms:
// p95 der interactive-Klasse im Fenster gegen p95 des gleich langen Fensters
// davor (design/05 §4.7, gleiche Regel wie design/04 §5.5).
const abortThreshold = 1.5

// untilMargin ist die Commit-Marge des Fenster-Pins.
const untilMargin = time.Minute

// Options steuert einen Report-Lauf.
type Options struct {
	Since   time.Time     // inklusiv; Zeitpunkt Null = Until − Window
	Until   time.Time     // exklusiv; Zeitpunkt Null oder später als der Pin = DB-now() − untilMargin
	Window  time.Duration // Fensterbreite, wenn Since leer ist
	ByClass bool          // zusätzlich je dispatch_class gruppieren
	// Arm schaltet die Per-Topic-Sicht eines einzelnen Arms zu (V-W5). Leer =
	// aus; die Zuordnungs-Regel ist arm-spezifisch, deshalb gibt es keinen
	// Default-Arm (run() weist alles außer armClusterLabel mit Exit 2 ab).
	Arm string
}

// Bucket sind die Kennzahlen einer Gruppe (pipeline oder dispatch_class).
// Es gibt hier bewusst KEIN cost_usd-Feld: der Dollar ist auf einem
// On-prem-Serving keine Kennzahl, nur eine Provenienz-Angabe (§4.7).
type Bucket struct {
	Key string `json:"key"`
	N   int64  `json:"n"`
	// OccupancySeconds ist die Belegungs-Summe nach der Design-Formel
	// (duration_ms − COALESCE(queue_wait_ms,0)) / 1000.
	OccupancySeconds float64 `json:"occupancy_seconds"`
	// WireSeconds ist Σ duration_ms / 1000 OHNE Abzug — siehe die
	// M-W7-Fußzeile: seit MW10 ist duration_ms auf jedem Pfad, der
	// queue_wait_ms setzt, bereits wartefrei gemessen.
	WireSeconds          float64 `json:"wire_seconds"`
	P50DurationMs        float64 `json:"p50_duration_ms"`
	P95DurationMs        float64 `json:"p95_duration_ms"`
	PromptTokens         int64   `json:"prompt_tokens"`
	PromptTokensNull     int64   `json:"prompt_tokens_null"`
	CompletionTokens     int64   `json:"completion_tokens"`
	CompletionTokensNull int64   `json:"completion_tokens_null"`
	Errors               int64   `json:"errors"`
	ErrorRate            float64 `json:"error_rate"`
	DispatchAborts       int64   `json:"dispatch_aborts"`
	DurationNull         int64   `json:"duration_null"`
	QueueWaitNull        int64   `json:"queue_wait_null"`
}

// InteractiveP95 ist die Nicht-Störungs-Kennzahl: p95 der interactive-Klasse
// im Fenster gegen p95 des gleich langen Fensters davor.
type InteractiveP95 struct {
	WindowMs   float64   `json:"window_ms"`
	WindowN    int64     `json:"window_n"`
	PriorMs    float64   `json:"prior_ms"`
	PriorN     int64     `json:"prior_n"`
	PriorSince time.Time `json:"prior_since"`
	// Factor ist WindowMs / PriorMs; 0, wenn der Vorher-Wert 0 ist (kein
	// Vergleich möglich — dann ist Exceeded immer false).
	Factor    float64 `json:"factor"`
	Threshold float64 `json:"threshold"`
	Exceeded  bool    `json:"exceeded"`
	Note      string  `json:"note"`
}

// Report ist der vollständige Kostenreport eines Fensters.
type Report struct {
	GeneratedAt  time.Time      `json:"generated_at"`
	Since        time.Time      `json:"since"`
	Until        time.Time      `json:"until"`
	RowsInWindow int64          `json:"rows_in_window"`
	CountGate    int64          `json:"count_gate"`
	Pipelines    []Bucket       `json:"pipelines"`
	Classes      []Bucket       `json:"dispatch_classes,omitempty"`
	Interactive  InteractiveP95 `json:"interactive_p95"`
	// PerTopic ist die V-W5-Sicht eines einzelnen Arms je Topic. NIL, wenn
	// -arm/-per-topic nicht gesetzt sind — dann ist der Report byte-identisch
	// zum M-W7-Stand (omitempty greift auf dem Zeiger).
	PerTopic *PerTopicReport `json:"per_topic,omitempty"`
	// CostUSDNote ist die Pflicht-Provenienz-Zeile. cost_usd erscheint im
	// ganzen Report NUR hier und NIE als Kennzahl.
	CostUSDNote string `json:"cost_usd_note"`
	// Footnotes sind die Pflicht-Fußzeilen: die beiden bekannten
	// llmlog-Verzerrungen (§4.7) plus der M-W7-Befund zur Doppel-Subtraktion.
	Footnotes []string `json:"footnotes"`
}

// errCountGate meldet, dass die Summe der Gruppen-Zähler nicht dem count(*)
// über dasselbe Fenster entspricht.
var errCountGate = fmt.Errorf("armcost: rows_in_window != count(*) über dasselbe Fenster")

// buildReport erhebt den kompletten Report. Jede Abfrage läuft in einer
// eigenen kurzen READ ONLY-Transaktion (Muster ctx-llmlog-export): kein
// Schreibpfad ist by construction möglich, und kein Langläufer-Snapshot pinnt
// den xmin-Horizont der Live-Datenbank.
func buildReport(ctx context.Context, pool *pgxpool.Pool, opts Options) (Report, error) {
	var rep Report
	rep.GeneratedAt = time.Now().UTC()

	var pin time.Time
	if err := pgxdb.Read(ctx, pool, pgxdb.Stages{Begin: "begin"}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, pinUntilSQL, untilMargin).Scan(&pin)
	}); err != nil {
		return rep, fmt.Errorf("armcost: pin until: %w", err)
	}
	until := opts.Until
	if until.IsZero() || until.After(pin) {
		until = pin
	}
	window := opts.Window
	if window <= 0 {
		window = 7 * 24 * time.Hour
	}
	since := opts.Since
	if since.IsZero() {
		since = until.Add(-window)
	}
	if !since.Before(until) {
		return rep, fmt.Errorf("armcost: leeres Fenster: since=%s until=%s",
			since.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	rep.Since, rep.Until = since, until

	var err error
	if rep.Pipelines, err = queryBuckets(ctx, pool, pipelineBucketSQL, since, until); err != nil {
		return rep, fmt.Errorf("armcost: pipelines: %w", err)
	}
	if opts.ByClass {
		if rep.Classes, err = queryBuckets(ctx, pool, classBucketSQL, since, until); err != nil {
			return rep, fmt.Errorf("armcost: dispatch_classes: %w", err)
		}
	}

	var costRows int64
	if err := pgxdb.Read(ctx, pool, pgxdb.Stages{Begin: "begin"}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, windowStatsSQL, since, until).Scan(&rep.CountGate, &costRows)
	}); err != nil {
		return rep, fmt.Errorf("armcost: window stats: %w", err)
	}
	for _, b := range rep.Pipelines {
		rep.RowsInWindow += b.N
	}
	rep.CostUSDNote = fmt.Sprintf("cost_usd: in %d von %d Zeilen gesetzt — nicht verwendet", costRows, rep.CountGate)
	rep.Footnotes = footnotes()

	if rep.Interactive, err = queryInteractive(ctx, pool, since, until); err != nil {
		return rep, fmt.Errorf("armcost: interactive p95: %w", err)
	}

	// V-W5: die Per-Topic-Sicht hängt an ihrem eigenen Fenster (Geburt des
	// ältesten lebenden Topics .. until), nicht am rollenden Kosten-Fenster.
	if opts.Arm != "" {
		pt, ptErr := buildPerTopic(ctx, pool, opts.Arm, until, since)
		if ptErr != nil {
			return rep, ptErr
		}
		rep.PerTopic = &pt
	}

	if rep.RowsInWindow != rep.CountGate {
		return rep, fmt.Errorf("%w: rows_in_window=%d count=%d", errCountGate, rep.RowsInWindow, rep.CountGate)
	}
	return rep, nil
}

// queryBuckets fährt eine der beiden Gruppierungs-Konstanten und sortiert die
// Gruppen absteigend nach Belegung (Kosten-Sicht), Gleichstand nach Namen.
func queryBuckets(ctx context.Context, pool *pgxpool.Pool, q string, since, until time.Time) ([]Bucket, error) {
	var out []Bucket
	err := pgxdb.Read(ctx, pool, pgxdb.Stages{Begin: "begin"}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, since, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b Bucket
			if err := rows.Scan(&b.Key, &b.N, &b.OccupancySeconds, &b.WireSeconds,
				&b.P50DurationMs, &b.P95DurationMs,
				&b.PromptTokens, &b.PromptTokensNull,
				&b.CompletionTokens, &b.CompletionTokensNull,
				&b.Errors, &b.DispatchAborts, &b.DurationNull, &b.QueueWaitNull); err != nil {
				return err
			}
			if b.N > 0 {
				b.ErrorRate = float64(b.Errors) / float64(b.N)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OccupancySeconds != out[j].OccupancySeconds {
			return out[i].OccupancySeconds > out[j].OccupancySeconds
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// queryInteractive misst das Fenster und das gleich lange Fenster davor.
func queryInteractive(ctx context.Context, pool *pgxpool.Pool, since, until time.Time) (InteractiveP95, error) {
	var ip InteractiveP95
	ip.Threshold = abortThreshold
	ip.PriorSince = since.Add(-until.Sub(since))
	err := pgxdb.Read(ctx, pool, pgxdb.Stages{Begin: "begin"}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, interactiveP95SQL, since, until).Scan(&ip.WindowN, &ip.WindowMs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, interactiveP95SQL, ip.PriorSince, since).Scan(&ip.PriorN, &ip.PriorMs)
	})
	if err != nil {
		return ip, err
	}
	if ip.PriorMs > 0 {
		ip.Factor = ip.WindowMs / ip.PriorMs
		ip.Exceeded = ip.Factor > ip.Threshold
	}
	ip.Note = interactiveNote(ip)
	return ip, nil
}

// interactiveNote rendert die Kennzahl als Klartext-Zeile. Die Markierung
// „> 1,5 ⇒ Abbruchkriterium" steht IMMER darin — auch im grünen Fall, damit
// die Regel im Report sichtbar ist und nicht erst beim Reißen auftaucht.
func interactiveNote(ip InteractiveP95) string {
	rule := fmt.Sprintf("> %s ⇒ Abbruchkriterium", de(ip.Threshold, 1))
	if ip.PriorMs <= 0 {
		return fmt.Sprintf("p95 interactive %s ms im Fenster (n=%d); kein Vorher-Wert (n=%d) — kein Vergleich (%s)",
			de(ip.WindowMs, 0), ip.WindowN, ip.PriorN, rule)
	}
	verdict := "im Rahmen"
	if ip.Exceeded {
		verdict = "ÜBERSCHRITTEN"
	}
	return fmt.Sprintf("p95 interactive %s ms (n=%d) gegen %s ms davor (n=%d) ⇒ Faktor %s — %s (%s)",
		de(ip.WindowMs, 0), ip.WindowN, de(ip.PriorMs, 0), ip.PriorN, de(ip.Factor, 2), verdict, rule)
}

// ffFootnote ist die Pflicht-Fußzeile zur fire-and-forget-Verzerrung.
const ffFootnote = "Verzerrung (fire-and-forget): llmlog.Record schreibt asynchron in einer eigenen " +
	"Goroutine mit 5-s-Deadline, Fehler nur slog.Debug (llmlog/llmlog.go:135-143) — eine Zählung aus " +
	"derselben Tabelle sieht laufende Calls nicht und misst systematisch zu niedrig."

// nullIntFootnote ist die Pflicht-Fußzeile zur nullInt-Lücke.
const nullIntFootnote = "Verzerrung (nullInt): 0 wird beim Insert zu NULL (llmlog/llmlog.go:192-197) — " +
	"Token-Summen sind systematisch lückenhaft; die (null)-Spalten zählen die betroffenen Zeilen."

// doubleSubtractFootnote ist der M-W7-Befund am Code: wo queue_wait_ms gesetzt
// ist, misst duration_ms bereits NUR den Wire-Call. Die Design-Formel zieht die
// Wartezeit dort ein zweites Mal ab. Der Report weist deshalb beide Summen aus.
const doubleSubtractFootnote = "Befund M-W7 (Code gegen design/05 §4.7): seit MW10 misst duration_ms auf " +
	"jedem Pfad, der queue_wait_ms setzt, bereits NUR den Wire-Call (llm/chain.go:284-295, " +
	"chat/engine.go:551-553, handler/query.go:1791-1794) — die Design-Formel zieht die Lease-Wartezeit dort " +
	"ein zweites Mal ab. belegung_s ist damit eine UNTERGRENZE, wire_s (Σ duration_ms) die Obergrenze; " +
	"die Differenz ist exakt Σ queue_wait_ms der Zeilen mit gesetztem Wert."

// footnotes liefert die Pflicht-Fußzeilen des Reports in fester Reihenfolge.
func footnotes() []string {
	return []string{ffFootnote, nullIntFootnote, doubleSubtractFootnote}
}
