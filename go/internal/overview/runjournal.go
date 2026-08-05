package overview

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Das Lauf-Journal (Achse 04 / S2, Migration 130, design/04 §3.2 + §4.10).
//
// Zweiphasig UND im ELTERNPROZESS — beides ist Vertrag, nicht Geschmack:
//
//   - ZWEIPHASIG, weil der Rebuild im Kindprozess läuft und der
//     rebuild_timeout ein CommandContext-SIGKILL ohne SIGTERM-Grace ist
//     (events/overview_worker.go:89-97). Ein getötetes Kind liefert keine
//     Stats über stdout. Eine Zeile, die erst NACH dem Lauf entsteht, fehlt
//     also ausgerechnet für den Lauf, der am Zeit- oder Speicherbudget stirbt
//     — für den also, für den das Journal gebaut wird. StartRun schreibt vor
//     dem Spawn, FinishRun aktualisiert danach.
//
//   - IM ELTERNPROZESS, weil das Kind seine eigene Zeile nicht mehr schreiben
//     kann, wenn es stirbt. Der Elternprozess weiß in jedem Fall etwas: Wall-
//     Clock, Exit-Signal, stderr-Tail. Kind-seitige Felder bleiben dann NULL.
//
// Und ausdrücklich AUSSERHALB der persist-Tx: läge die Schreibung darin, würde
// lock_held_ms sich selbst mitmessen und ein Rebuild-Fehler würde die
// Journal-Zeile mit zurückrollen — der Fehlerfall verlöre seinen Beleg
// (Gates S2-G1/S2-G2).

// RunStart sind die Felder, die der Elternprozess VOR dem Spawn kennt.
type RunStart struct {
	ScopeSet    []string
	Engine      string
	Resolution  float64
	CandidateN  int
	MaxNodesEff int
	ParentRSSKb int64
}

// StartRun legt die 'running'-Zeile an und gibt ihre run_id zurück.
//
// Fehler werden zurückgegeben, nicht verschluckt — aber der Aufrufer behandelt
// sie als nicht-fatal: ein Rebuild darf nicht daran scheitern, dass sein
// Protokoll nicht schreibbar ist. Die leere run_id signalisiert dem späteren
// FinishRun, dass es nichts zu aktualisieren gibt.
func StartRun(ctx context.Context, pool *pgxpool.Pool, s RunStart) (string, error) {
	scopes := s.ScopeSet
	if scopes == nil {
		scopes = []string{} // globaler Lauf: leeres Array, nie NULL (Spalte ist NOT NULL)
	}
	// Ein unbekannter Engine-Wert darf die Journal-Zeile nicht verhindern: der
	// Rebuild scheitert daran ohnehin gleich laut, und eine fehlende Zeile
	// waere genau der Verlust, gegen den S2 gebaut ist. Deshalb wird der
	// Rohwert notfalls durchgereicht — die Spalte traegt keinen CHECK.
	engine := s.Engine
	if engine == "" {
		engine = EngineGonum
	}
	var runID string
	err := pool.QueryRow(ctx, `
		INSERT INTO graph_overview_run
		    (scope_set, scope_key, engine, resolution, outcome,
		     candidate_n, max_nodes_eff, parent_rss_kb)
		VALUES ($1, $2, $3, $4, 'running', $5, $6, $7)
		RETURNING run_id::text`,
		scopes, lockKeyForScopes(s.ScopeSet), engine, s.Resolution,
		nullIfZero(s.CandidateN), nullIfZero(s.MaxNodesEff), nullIfZero64(s.ParentRSSKb),
	).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("overview: opening run journal row: %w", err)
	}
	return runID, nil
}

// RunResult ist das, was der Elternprozess NACH der Rückkehr weiß. Outcome und
// SkipReason setzt der Aufrufer; alles andere stammt aus den Stats des Kindes
// und ist bei einem getöteten Kind der Nullwert — der als NULL landet.
type RunResult struct {
	Outcome    string
	SkipReason string
	Stats      Stats
}

// FinishRun schließt die Zeile ab. Eine leere runID ist ein no-op (StartRun ist
// gescheitert — dann fehlt die Zeile, und das ist besser als eine erfundene).
//
// ctx ist bewusst der ÄUSSERE Scheduler-Kontext, nie der Rebuild-Kontext: auf
// dem Timeout-Pfad ist letzterer genau dann abgelaufen, wenn der Abschluss
// fällig ist (dieselbe Regel wie bei stampOverviewAttempt).
func FinishRun(ctx context.Context, pool *pgxpool.Pool, runID string, r RunResult) error {
	if runID == "" {
		return nil
	}
	st := r.Stats
	_, err := pool.Exec(ctx, `
		UPDATE graph_overview_run SET
		    outcome         = $2,
		    finished_at     = now(),
		    skip_reason     = $3,
		    node_n          = $4,
		    edge_n          = $5,
		    dangling_n      = $6,
		    selfloop_n      = $7,
		    cluster_n       = $8,
		    modularity      = $9,
		    load_ms         = $10,
		    cluster_ms      = $11,
		    persist_ms      = $12,
		    lock_held_ms    = $13,
		    peak_rss_kb     = $14,
		    partition_hash  = $15,
		    component_n     = $16,
		    level_n         = $17,
		    sweep_n         = $18,
		    sigma_drift     = $19,
		    engine          = COALESCE($24, engine),
		    max_nodes_eff   = COALESCE($25, max_nodes_eff),
		    topics_reattached = $20,
		    topics_born     = $21,
		    members_changed = $22,
		    members_reassigned = $23
		WHERE run_id = $1::uuid`,
		runID, r.Outcome, nullIfEmpty(r.SkipReason),
		nullIfZero(st.NodeCount), nullIfZero(st.EdgeCount), nullIfZero(st.DanglingN), nullIfZero(st.SelfLoopN),
		nullIfZero(st.ClusterCount), nullIfZeroF(st.Modularity),
		nullIfZero(st.LoadMs), nullIfZero(st.ClusterMs), nullIfZero(st.PersistMs), nullIfZero(st.LockHeldMs),
		nullIfZero64(st.PeakRSSKb), nullIfEmptyBytes(st.PartitionHash),
		nullIfZero(st.ComponentN), nullIfZero(st.LevelN), nullIfZero(st.SweepN), nullIfZeroF(st.SigmaDrift),
		nullIfZero(st.TopicsReattached), nullIfZero(st.TopicsBorn),
		nullIfZero(st.MembersChanged), nullIfZero(st.MembersReassigned),
		// Engine und effektiver Cap koennen sich gegenueber der Start-Zeile
		// unterscheiden: das Kind normalisiert den Engine-Wert und leitet den
		// Cap daraus ab. COALESCE haelt die Eltern-Werte, wenn das Kind nichts
		// gemeldet hat (Kind-Tod) — NULL wuerde sie sonst ueberschreiben.
		nullIfEmpty(st.Engine), nullIfZero(st.MaxNodesEff),
	)
	if err != nil {
		return fmt.Errorf("overview: closing run journal row %s: %w", runID, err)
	}
	return nil
}

// SweepStaleRuns ist der Boot-Aufräumer (§4.10 Punkt 3): 'running'-Zeilen, die
// älter sind als das Rebuild-Budget, können nur von einem gestorbenen Daemon
// stammen — ein lebender Lauf hätte längst abgeschlossen. Sie werden als
// 'failed'/'killed' geschlossen.
//
// Das Fenster ist bewusst GROSSZÜGIG (2× rebuild_timeout, mindestens eine
// Stunde): eine zu enge Grenze würde einen laufenden Rebuild eines ZWEITEN
// Prozesses fälschlich für tot erklären. Die Kosten eines zu spät geschlossenen
// Eintrags sind eine Zeile mit falschem outcome; die Kosten eines zu früh
// geschlossenen wären ein Journal, das aktive Läufe als gestorben meldet.
func SweepStaleRuns(ctx context.Context, pool *pgxpool.Pool, rebuildTimeout time.Duration) (int64, error) {
	window := 2 * rebuildTimeout
	if window < time.Hour {
		window = time.Hour
	}
	tag, err := pool.Exec(ctx, `
		UPDATE graph_overview_run
		   SET outcome = 'failed', skip_reason = 'killed', finished_at = now()
		 WHERE outcome = 'running'
		   AND started_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(window.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("overview: sweeping stale run journal rows: %w", err)
	}
	return tag.RowsAffected(), nil
}

// runPurgeBatch deckelt eine Purge-Transaktion. Muster design/01 §4.8: die
// Retention räumt in kleinen Transaktionen, nie in einer großen — und
// ausdrücklich AUSSERHALB der persist-Tx und ihres Advisory-Locks (Gate S2-G3).
const runPurgeBatch = 5000

// PurgeRuns löscht Journal-Zeilen jenseits der Retention.
func PurgeRuns(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil // 0 = Journal wird nicht gepurgt (bewusster Aus-Zustand)
	}
	var total int64
	for {
		tag, err := pool.Exec(ctx, `
			DELETE FROM graph_overview_run
			 WHERE run_id IN (
			     SELECT run_id FROM graph_overview_run
			      WHERE started_at < now() - $1::interval
			      ORDER BY started_at
			      LIMIT $2)`,
			fmt.Sprintf("%d seconds", int(retention.Seconds())), runPurgeBatch)
		if err != nil {
			return total, fmt.Errorf("overview: purging run journal: %w", err)
		}
		n := tag.RowsAffected()
		total += n
		if n < runPurgeBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// ReadVmHWMkB liest den Hochwassermarker des residenten Speichers aus
// /proc/self/status.
//
// Warum VmHWM und nicht VmRSS: der Rebuild ist eine Sägezahn-Last (Laden,
// Symmetrisieren, Rechnen, Freigeben), und der Momentanwert am Ende sagt
// nichts über die Spitze, an der die cgroup zuschlägt. S1 hat genau das
// gemessen: 423 MB Spitze bei 200k Knoten gegen ein 512-MiB-Limit.
//
// 0 bei nicht-Linux oder unlesbarem procfs — nie ein Fehler: eine fehlende
// Speicherzahl darf keinen Rebuild scheitern lassen.
func ReadVmHWMkB() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		f := strings.Fields(line) // ["VmHWM:", "12345", "kB"]
		if len(f) < 2 {
			return 0
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	return 0
}

// Die nullIf*-Helfer bilden "nicht gemessen" auf SQL NULL ab statt auf 0.
//
// Der Unterschied ist nicht kosmetisch: 0 Cluster ist eine Aussage (leerer
// Korpus), "nicht gemessen" ist keine. Solange gonum weder Ebenen noch Sweeps
// meldet und S9a den CopyFrom-Anteil noch nicht trennt, muss die Spalte NULL
// tragen — sonst behauptet das Journal Messungen, die niemand gemacht hat.
func nullIfZero(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullIfZero64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullIfZeroF(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfEmptyBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
