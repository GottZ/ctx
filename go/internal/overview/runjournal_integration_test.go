//go:build integration

// Achse 04 / Welle S2 — Gates S2-G1 … S2-G5 (design/04 §6.7).
//
// Die Welle behauptet eine einzige Sache: JEDER Rebuild-Versuch hinterlaesst
// genau eine Journal-Zeile — auch der, der per SIGKILL oder cgroup-OOM stirbt.
// Genau dieser Fall ist der Grund fuer die zweiphasige Schreibung, und genau er
// ist der Fall, den ein einphasiges Journal verliert. Die Gates pruefen ihn
// deshalb nicht am Rand, sondern in der Mitte.
package overview

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

func journalRow(t *testing.T, pool *pgxpool.Pool, runID string) (outcome string, skipReason *string, finished *time.Time) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT outcome, skip_reason, finished_at FROM graph_overview_run WHERE run_id = $1::uuid`,
		runID).Scan(&outcome, &skipReason, &finished)
	if err != nil {
		t.Fatalf("reading journal row %s: %v", runID, err)
	}
	return outcome, skipReason, finished
}

func journalCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph_overview_run`).Scan(&n); err != nil {
		t.Fatalf("counting journal rows: %v", err)
	}
	return n
}

// TestRunJournal_G1_RowSurvivesAKilledChild ist das TRAGENDE Gate.
//
// Es bildet den Ablauf nach, den scheduler.rebuildOverviewOnce fuehrt: Zeile
// oeffnen, Kind laeuft (hier: stirbt), Zeile schliessen. Die ROT-PROBE steckt
// in der Mitte: zwischen StartRun und FinishRun passiert NICHTS, was das Kind
// haette liefern koennen — und die Zeile existiert trotzdem, mit NULL in allen
// kind-seitigen Feldern.
//
// Waere die Schreibung einphasig (nur FinishRun), gaebe es hier keine Zeile.
// Das ist kein hypothetischer Vorwurf: der Timeout-Pfad ist ein
// CommandContext-SIGKILL ohne SIGTERM-Grace (events/overview_worker.go:89-97),
// und ein getoetetes Kind schreibt nichts mehr auf stdout.
func TestRunJournal_G1_RowSurvivesAKilledChild(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	runID, err := StartRun(ctx, pool, RunStart{
		ScopeSet: []string{"private"}, Engine: EngineGonum, Resolution: 1.0,
		MaxNodesEff: 200000, ParentRSSKb: 4242,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Zustand WAEHREND des Laufs: genau eine Zeile, offen. Diese Zusicherung
	// steht ZUERST und als eigene Zeile, weil sie die eigentliche Aussage der
	// Welle ist — die Zeile existiert, BEVOR das Kind irgendetwas geliefert hat.
	// Bei einphasiger Schreibung ist hier 0, und der Fehlertext sagt das direkt.
	if n := journalCount(t, pool); n != 1 {
		t.Fatalf("waehrend des Laufs %d Journal-Zeilen, erwartet 1 — "+
			"eine einphasige Schreibung verliert genau diesen Lauf (SIGKILL/OOM liefert keine Stats)", n)
	}
	outcome, _, finished := journalRow(t, pool, runID)
	if outcome != "running" || finished != nil {
		t.Fatalf("waehrend des Laufs: outcome=%q finished_at=%v — erwartet running/NULL", outcome, finished)
	}

	// Das Kind stirbt. Der Elternprozess weiss den Ausgang und den Grund, sonst
	// nichts — Stats ist der Nullwert, wie nach einem SIGKILL.
	if err := FinishRun(ctx, pool, runID, RunResult{Outcome: "failed", SkipReason: "killed"}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	outcome, reason, finished := journalRow(t, pool, runID)
	if outcome != "failed" || reason == nil || *reason != "killed" || finished == nil {
		t.Fatalf("nach dem Kind-Tod: outcome=%q reason=%v finished=%v", outcome, reason, finished)
	}
	if n := journalCount(t, pool); n != 1 {
		t.Fatalf("genau eine Zeile erwartet, %d gefunden", n)
	}

	// Kind-seitige Felder MUESSEN NULL sein, nicht 0. Der Unterschied ist der
	// ganze Punkt: 0 Cluster ist eine Aussage (leerer Korpus), "nicht gemessen"
	// ist keine. Ein Journal, das 0 statt NULL schreibt, behauptet Messungen,
	// die niemand gemacht hat.
	var nodeN, clusterN, loadMS, lockMS *int
	var peak *int64
	var hash []byte
	if err := pool.QueryRow(ctx, `
		SELECT node_n, cluster_n, load_ms, lock_held_ms, peak_rss_kb, partition_hash
		  FROM graph_overview_run WHERE run_id = $1::uuid`, runID).
		Scan(&nodeN, &clusterN, &loadMS, &lockMS, &peak, &hash); err != nil {
		t.Fatalf("reading child-side fields: %v", err)
	}
	if nodeN != nil || clusterN != nil || loadMS != nil || lockMS != nil || peak != nil || hash != nil {
		t.Fatalf("kind-seitige Felder nach SIGKILL nicht NULL: node=%v cluster=%v load=%v lock=%v rss=%v hash=%v",
			nodeN, clusterN, loadMS, lockMS, peak, hash)
	}

	// Elternseitige Felder MUESSEN gefuellt sein — sie sind der Grund, aus dem
	// die Start-Zeile ueberhaupt vor dem Spawn geschrieben wird.
	var maxNodesEff *int
	var parentRSS *int64
	if err := pool.QueryRow(ctx, `
		SELECT max_nodes_eff, parent_rss_kb FROM graph_overview_run WHERE run_id = $1::uuid`, runID).
		Scan(&maxNodesEff, &parentRSS); err != nil {
		t.Fatalf("reading parent-side fields: %v", err)
	}
	if maxNodesEff == nil || *maxNodesEff != 200000 || parentRSS == nil || *parentRSS != 4242 {
		t.Fatalf("elternseitige Felder fehlen: max_nodes_eff=%v parent_rss_kb=%v", maxNodesEff, parentRSS)
	}
}

// TestRunJournal_G1_OutcomeCheckIsComplete belegt, dass der CHECK alle vier
// Ausgaenge kennt. Die Lehre stammt aus Migration 123 (skip_reason): ein zu
// enger CHECK macht genau den Lauf unaufzeichenbar, fuer den die Spalte da ist.
func TestRunJournal_G1_OutcomeCheckIsComplete(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, outcome := range []string{"ok", "skipped", "failed"} {
		runID, err := StartRun(ctx, pool, RunStart{Engine: EngineGonum, Resolution: 1.0})
		if err != nil {
			t.Fatalf("StartRun(%s): %v", outcome, err)
		}
		if err := FinishRun(ctx, pool, runID, RunResult{Outcome: outcome}); err != nil {
			t.Fatalf("FinishRun(%s): %v", outcome, err)
		}
	}
	// Negativ-Probe: ein unbekannter Ausgang MUSS scheitern, sonst belegt der
	// CHECK nichts.
	runID, err := StartRun(ctx, pool, RunStart{Engine: EngineGonum, Resolution: 1.0})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := FinishRun(ctx, pool, runID, RunResult{Outcome: "haeschenhuepf"}); err == nil {
		t.Fatal("unbekannter outcome wurde akzeptiert — der CHECK belegt nichts")
	}
}

// TestRunJournal_G1_FinishedIffDone pinnt die Invariante, auf der der
// Startup-Sweep steht: outcome='running' genau dann, wenn finished_at NULL ist.
// Ohne sie koennte eine abgeschlossene Zeile als verwaist gelten (und der Sweep
// wuerde sie ueberschreiben) oder eine offene als abgeschlossen (und der Sweep
// wuerde sie nie finden).
func TestRunJournal_G1_FinishedIffDone(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO graph_overview_run (scope_set, scope_key, engine, resolution, outcome, finished_at)
		VALUES ('{}', 1, 'gonum', 1.0, 'running', now())`)
	if err == nil {
		t.Fatal("running MIT finished_at wurde akzeptiert — gor_finished_iff_done greift nicht")
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO graph_overview_run (scope_set, scope_key, engine, resolution, outcome)
		VALUES ('{}', 1, 'gonum', 1.0, 'ok')`)
	if err == nil {
		t.Fatal("ok OHNE finished_at wurde akzeptiert — gor_finished_iff_done greift nicht")
	}
}

// TestRunJournal_G2_SurvivesARebuildRollback ist S2-G2: die Journal-Zeile lebt
// in ihrer EIGENEN Transaktion und wird von einem Rebuild-Fehler nicht mit
// zurueckgerollt.
//
// Die Probe faehrt eine fremde Transaktion, die abbricht, waehrend die
// Journal-Zeile bereits steht. Laege die Schreibung in der persist-Tx, waere die
// Zeile nach dem Rollback verschwunden — der Fehlerfall verloere seinen Beleg,
// und das Journal koennte ausgerechnet ueber Fehler nichts sagen.
func TestRunJournal_G2_SurvivesARebuildRollback(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	runID, err := StartRun(ctx, pool, RunStart{Engine: EngineGonum, Resolution: 1.0})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM graph_cluster_member`); err != nil {
		t.Fatalf("tx work: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n := journalCount(t, pool); n != 1 {
		t.Fatalf("Journal-Zeile hat den Rollback nicht ueberlebt: %d Zeilen", n)
	}
	if outcome, _, _ := journalRow(t, pool, runID); outcome != "running" {
		t.Fatalf("outcome nach fremdem Rollback: %q", outcome)
	}
}

// TestRunJournal_G3_SweepAndPurge deckt den Startup-Sweep und die Retention ab.
func TestRunJournal_G3_SweepAndPurge(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Eine verwaiste 'running'-Zeile, weit ausserhalb jedes Budgets.
	var stale string
	if err := pool.QueryRow(ctx, `
		INSERT INTO graph_overview_run (scope_set, scope_key, engine, resolution, outcome, started_at)
		VALUES ('{}', 1, 'gonum', 1.0, 'running', now() - interval '9 hours')
		RETURNING run_id::text`).Scan(&stale); err != nil {
		t.Fatalf("seeding stale row: %v", err)
	}
	// Und eine frische, die der Sweep NICHT anfassen darf — sonst erklaert er
	// laufende Rebuilds eines zweiten Prozesses fuer tot.
	fresh, err := StartRun(ctx, pool, RunStart{Engine: EngineGonum, Resolution: 1.0})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	n, err := SweepStaleRuns(ctx, pool, 15*time.Minute)
	if err != nil {
		t.Fatalf("SweepStaleRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("Sweep schloss %d Zeilen, erwartet genau 1", n)
	}
	if outcome, reason, _ := journalRow(t, pool, stale); outcome != "failed" || reason == nil || *reason != "killed" {
		t.Fatalf("verwaiste Zeile: outcome=%q reason=%v", outcome, reason)
	}
	if outcome, _, _ := journalRow(t, pool, fresh); outcome != "running" {
		t.Fatalf("frische Zeile wurde vom Sweep angefasst: outcome=%q", outcome)
	}

	// Retention: 0 ist der dokumentierte Aus-Zustand und MUSS ein no-op sein,
	// kein Fehler und kein Voll-Purge.
	if purged, err := PurgeRuns(ctx, pool, 0); err != nil || purged != 0 {
		t.Fatalf("PurgeRuns(0) = (%d, %v) — erwartet no-op", purged, err)
	}
	if got := journalCount(t, pool); got != 2 {
		t.Fatalf("PurgeRuns(0) hat geloescht: %d Zeilen uebrig", got)
	}
	purged, err := PurgeRuns(ctx, pool, time.Hour)
	if err != nil {
		t.Fatalf("PurgeRuns: %v", err)
	}
	if purged != 1 {
		t.Fatalf("Purge loeschte %d Zeilen, erwartet 1 (die 9 h alte)", purged)
	}
	if got := journalCount(t, pool); got != 1 {
		t.Fatalf("nach Purge %d Zeilen, erwartet 1", got)
	}
}

// TestRunJournal_G4_PartitionHashIsStableAndSensitive ist S2-G4.
//
// Zwei Haelften, weil eine allein nichts belegt: der Hash muss ueber zwei
// identische Laeufe GLEICH sein (sonst ist er kein Anker) und bei der kleinsten
// Partitionsaenderung VERSCHIEDEN (sonst ist er kein Detektor). Ein konstanter
// Hash bestuende die erste Haelfte muehelos.
func TestRunJournal_G4_PartitionHashIsStableAndSensitive(t *testing.T) {
	a := map[string]string{"b1": "c1", "b2": "c1", "b3": "c2"}
	b := map[string]string{"b3": "c2", "b1": "c1", "b2": "c1"} // andere Einfuegereihenfolge
	if !bytes.Equal(partitionHash(a), partitionHash(b)) {
		t.Fatal("partition_hash haengt an der Map-Ordnung — der Anker ist wertlos")
	}
	moved := map[string]string{"b1": "c1", "b2": "c2", "b3": "c2"} // b2 wechselt Cluster
	if bytes.Equal(partitionHash(a), partitionHash(moved)) {
		t.Fatal("partition_hash uebersieht einen Cluster-Wechsel — der Detektor ist blind")
	}
	// Praefix-Kollision: ohne Trennung von block_id und cluster_id waeren
	// ("ab","c") und ("a","bc") derselbe Hash. Der Test steht hier, weil genau
	// diese Klasse Fehler beim Hashen konkatenierter Strings ueblich ist.
	if bytes.Equal(partitionHash(map[string]string{"ab": "c"}), partitionHash(map[string]string{"a": "bc"})) {
		t.Fatal("partition_hash kollidiert ueber die Feldgrenze hinweg")
	}
}
