//go:build integration

// Gate C5-B (Befund N-13 aus reports/bau/c3-3-re-pilot.md §10, design/02 §6.2
// und A.7 d): der Retention-Konsument von distill.retention_days und
// distill.seen_retention_days.
//
// DER ROTE AUSGANGSZUSTAND, gemessen statt behauptet (Wellen-Bericht, drei
// Sonden gegen den unveraenderten Baum):
//
//  1. Das vollstaendige 6h-Janitor-Buendel, Zeile fuer Zeile ueber Zeilen mit
//     Alter 400 Tage bei Fenstern 90/30 — distill_run=1, distill_seen=1 stehen
//     danach unveraendert. Die Konfiguration deklariert die Fenster, kein Code
//     liest sie (Sweep ueber den Baum: SeenRetentionDays kommt ausschliesslich
//     in internal/config vor).
//  2. Ein Chunk, den ein spaeteres Manifest erneut listet, faellt als Duplikat
//     — und seine Ledger-Zeile bleibt 400,0 Tage alt. Die Zusicherung des
//     gleitenden Fensters (distill_select.go) war nicht eingeloest, weil
//     distillDedup das Wiedersehen verwirft, bevor distillMarkSeen es sehen
//     koennte.
//  3. Der naive Purge — DELETE ueber die Zeitachse ohne Schutzregel — hat eine
//     Journalzeile geloescht und das Wasserzeichen von 1788063650102010 auf 0
//     zurueckgesetzt.
//
// Die Sonden unten fahren durch runSixHourJanitor und NICHT direkt durch
// runDistillRetention: die Fehlerklasse dieser Welle ist nicht "die Funktion
// loescht falsch", sondern "die Funktion wird nie gefeuert", und nur das
// Buendel ist das, was der Ticker ausfuehrt (scheduler.go, embedCacheTicker).
//
//	go test -tags=integration ./internal/events/ -run TestDistillRetentionN13 -count=1 -v
package events

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// c5bConfig ist a6Config plus die beiden Schluessel dieser Welle. dfConfig
// laesst sie auf dem Go-Nullwert, und 0 ist die dokumentierte No-op — eine
// Sonde auf 0 koennte den fehlenden Konsumenten nicht von der abgeschalteten
// Retention unterscheiden.
func c5bConfig(t *testing.T, runDays, seenDays int) *config.Config {
	t.Helper()
	c := a6Config(a6DumpRoot(t), 4000, 200)
	c.Distill.RetentionDays = runDays
	c.Distill.SeenRetentionDays = seenDays
	return c
}

// c5bCount zaehlt die Zeilen einer der beiden Buchhaltungstabellen fuer eine
// Quelle.
func c5bCount(t *testing.T, pool *pgxpool.Pool, table, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table+` WHERE source_key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// c5bAge liefert das Alter der aeltesten Ledger-Zeile einer Quelle in Tagen.
func c5bAge(t *testing.T, pool *pgxpool.Pool, key string) float64 {
	t.Helper()
	var days float64
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(max(extract(epoch FROM now() - last_seen) / 86400), -1)
		  FROM distill_seen WHERE source_key = $1`, key).Scan(&days); err != nil {
		t.Fatalf("Ledger-Alter: %v", err)
	}
	return days
}

// c5bAgeAll altert BEIDE Buchhaltungstabellen einer Quelle ueber beide Fenster
// hinaus (400 Tage gegen 90 und 30).
func c5bAgeAll(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	bg := context.Background()
	if _, err := pool.Exec(bg,
		`UPDATE distill_run SET started_at = now() - interval '400 days' WHERE source_key = $1`, key); err != nil {
		t.Fatalf("Journal altern: %v", err)
	}
	if _, err := pool.Exec(bg,
		`UPDATE distill_seen SET last_seen = now() - interval '400 days' WHERE source_key = $1`, key); err != nil {
		t.Fatalf("Ledger altern: %v", err)
	}
}

// c5bTicks faehrt n Laeufe ueber je EIN neues Manifest mit je eigenem Inhalt
// durch den Produktions-Schreibpfad und liefert die Wasserzeichen-Basis. Die
// Inhalte muessen unterscheidbar sein: der Dedup-Schluessel ist der
// normalisierte Chunk-Text, also waeren zwei Absaetze derselben Fuellung EINE
// Ledger-Zeile (a6Para, nicht a6Text).
func c5bTicks(t *testing.T, pool *pgxpool.Pool, cfg *config.Config, root string, base time.Time, tags []string) {
	t.Helper()
	ctx := context.Background()
	for i, tag := range tags {
		at := base.Add(time.Duration(i) * time.Minute)
		part := a6SeedPart(t, pool, root, "### Message 1 — user\n"+a6Para(tag, 800), at)
		a6SeedManifest(t, pool, root, at, []string{part})
		dfScheduler(pool, cfg, nil).distillOnce(ctx, dfNoDemand)
	}
}

func TestDistillRetentionN13(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	// SONDE 1 — das Fenster wirkt, und zwar durch das, was der Ticker feuert.
	// Alt verschwindet, jung bleibt, in BEIDEN Tabellen.
	t.Run("AltesVerschwindetJungesBleibt", func(t *testing.T) {
		a8Truncate(t, pool)
		const root = "20260830_130000_c5bwindow"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		cfg := c5bConfig(t, 90, 30)
		base := time.Now().Add(-6 * time.Hour)
		c5bTicks(t, pool, cfg, root, base, []string{"alpha", "beta", "gamma"})

		oldRuns, oldSeen := c5bCount(t, pool, "distill_run", key), c5bCount(t, pool, "distill_seen", key)
		if oldRuns < 3 || oldSeen < 3 {
			t.Fatalf("Fixture verfehlt: %d Journalzeilen / %d Ledger-Zeilen nach drei Laeufen, "+
				"erwartet je >= 3", oldRuns, oldSeen)
		}
		c5bAgeAll(t, pool, key)

		// Ein vierter, JUNGER Lauf ueber neues Material. Er traegt danach das
		// groesste Wasserzeichen und die juengste Zeile, womit die drei alten
		// Journalzeilen ihren Schutz verlieren.
		c5bTicks(t, pool, cfg, root, base.Add(10*time.Minute), []string{"delta"})
		beforeRuns, beforeSeen := c5bCount(t, pool, "distill_run", key), c5bCount(t, pool, "distill_seen", key)

		dfScheduler(pool, cfg, nil).runSixHourJanitor(ctx)

		afterRuns, afterSeen := c5bCount(t, pool, "distill_run", key), c5bCount(t, pool, "distill_seen", key)
		t.Logf("distill_run %d -> %d, distill_seen %d -> %d (alt: %d/%d, Fenster 90/30)",
			beforeRuns, afterRuns, beforeSeen, afterSeen, oldRuns, oldSeen)
		if afterRuns != beforeRuns-oldRuns {
			t.Fatalf("Journal: %d Zeilen nach dem Purge, erwartet %d (%d insgesamt minus %d alte)",
				afterRuns, beforeRuns-oldRuns, beforeRuns, oldRuns)
		}
		if afterSeen != beforeSeen-oldSeen {
			t.Fatalf("Ledger: %d Zeilen nach dem Purge, erwartet %d (%d insgesamt minus %d alte)",
				afterSeen, beforeSeen-oldSeen, beforeSeen, oldSeen)
		}
		if afterRuns == 0 || afterSeen == 0 {
			t.Fatalf("der Purge hat die jungen Zeilen mitgenommen: run=%d seen=%d", afterRuns, afterSeen)
		}
	})

	// SONDE 2 — 0 ist die dokumentierte No-op, in BEIDEN Fenstern. Der
	// gefaehrliche Default waere "alles loeschen": ein unkonfiguriertes Fenster
	// darf nie zum schaerfsten werden.
	t.Run("NullFensterLoeschtNichts", func(t *testing.T) {
		a8Truncate(t, pool)
		const root = "20260830_140000_c5bzero"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		base := time.Now().Add(-6 * time.Hour)
		c5bTicks(t, pool, c5bConfig(t, 90, 30), root, base, []string{"alpha", "beta", "gamma"})
		c5bAgeAll(t, pool, key)
		beforeRuns, beforeSeen := c5bCount(t, pool, "distill_run", key), c5bCount(t, pool, "distill_seen", key)
		if beforeRuns == 0 || beforeSeen == 0 {
			t.Fatal("Fixture leer — die Sonde misst nichts")
		}

		// Beide Fenster auf 0: der Zustand eines Baums, in dem der Operator die
		// Retention nie eingeschaltet hat.
		dfScheduler(pool, c5bConfig(t, 0, 0), nil).runSixHourJanitor(ctx)

		afterRuns, afterSeen := c5bCount(t, pool, "distill_run", key), c5bCount(t, pool, "distill_seen", key)
		if afterRuns != beforeRuns || afterSeen != beforeSeen {
			t.Fatalf("Fenster 0 hat geloescht: distill_run %d -> %d, distill_seen %d -> %d — "+
				"0 ist die dokumentierte No-op (config.go:2148-2153)",
				beforeRuns, afterRuns, beforeSeen, afterSeen)
		}
		t.Logf("Fenster 0/0: distill_run %d unveraendert, distill_seen %d unveraendert (Alter 400 Tage)",
			afterRuns, afterSeen)
	})

	// SONDE 3 — DIE KERNSONDE. Das Wasserzeichen ist abgeleitet
	// (COALESCE(max(watermark_to), 0) ueber die nicht-laufenden Zeilen), also
	// ist ein Journal-Purge ein Eingriff in die Position des Arms. Er darf sie
	// nicht bewegen, und der naechste Lauf muss auf ihr aufsetzen statt bei 0.
	t.Run("DasWasserzeichenUeberlebtDenPurge", func(t *testing.T) {
		a8Truncate(t, pool)
		const root = "20260830_150000_c5bwm"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		cfg := c5bConfig(t, 90, 30)
		base := time.Now().Add(-6 * time.Hour)
		c5bTicks(t, pool, cfg, root, base, []string{"alpha", "beta", "gamma"})

		s := dfScheduler(pool, cfg, nil)
		before, err := s.distillWatermark(ctx, key)
		if err != nil {
			t.Fatalf("Wasserzeichen vor dem Purge: %v", err)
		}
		if before == 0 {
			t.Fatal("Fixture verfehlt: das Wasserzeichen steht auf 0")
		}
		rowsBefore := c5bCount(t, pool, "distill_run", key)

		// Das GESAMTE Journal dieser Quelle jenseits des Fensters — der Zustand
		// einer Quelle, die laenger als retention_days ruht. Genau hier hat der
		// naive Purge das Wasserzeichen auf 0 gesetzt.
		c5bAgeAll(t, pool, key)
		s.runSixHourJanitor(ctx)

		after, err := s.distillWatermark(ctx, key)
		if err != nil {
			t.Fatalf("Wasserzeichen nach dem Purge: %v", err)
		}
		rowsAfter := c5bCount(t, pool, "distill_run", key)
		t.Logf("Journal %d -> %d Zeilen, Wasserzeichen %d -> %d", rowsBefore, rowsAfter, before, after)
		if after != before {
			t.Fatalf("der Purge hat das Wasserzeichen von %d auf %d bewegt — der Arm liest die "+
				"Quelle von vorn und kauft jeden bereits destillierten Chunk erneut", before, after)
		}
		if rowsAfter >= rowsBefore {
			t.Fatalf("der Purge hat nichts geloescht (%d -> %d) — die Sonde belegt den Schutz "+
				"nur, wenn der Purge die Quelle ueberhaupt angefasst hat", rowsBefore, rowsAfter)
		}

		// Und die Probe aufs Exempel ueber den Produktions-Lesepfad: der
		// naechste Lauf setzt auf dem erhaltenen Wasserzeichen auf.
		c5bTicks(t, pool, cfg, root, base.Add(20*time.Minute), []string{"delta"})
		var from int64
		if err := pool.QueryRow(ctx, `
			SELECT watermark_from FROM distill_run
			 WHERE source_key = $1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&from); err != nil {
			t.Fatalf("watermark_from des Folgelaufs: %v", err)
		}
		if from != before {
			t.Fatalf("der Folgelauf startete bei watermark_from=%d statt bei %d — das Journal "+
				"hat seine Position verloren", from, before)
		}
	})

	// SONDE 4 — die Wiedervorlage-Invariante, der eigentliche Auftrag: der Arm
	// darf durch die Loeschung KEINEN bereits destillierten Chunk erneut
	// kaufen. Der Chunk kehrt wieder (der dominante Kostenposten nach
	// 135:222-227), sein Fenster gleitet mit, der Purge laesst ihn stehen, und
	// der Folgelauf verwirft ihn weiterhin als Duplikat.
	t.Run("DerWiederkehrendeChunkWirdNichtErneutGekauft", func(t *testing.T) {
		a8Truncate(t, pool)
		const root = "20260830_160000_c5bcycle"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)

		cfg := c5bConfig(t, 90, 30)
		at := time.Now().Add(-6 * time.Hour)
		part := a6SeedPart(t, pool, root, "### Message 1 — user\n"+a6Para("zyklisch", 800), at)
		a6SeedManifest(t, pool, root, at, []string{part})
		dfScheduler(pool, cfg, nil).distillOnce(ctx, dfNoDemand)
		if n := c5bCount(t, pool, "distill_seen", key); n != 1 {
			t.Fatalf("Fixture verfehlt: %d Ledger-Zeilen nach dem ersten Lauf, erwartet 1", n)
		}

		// Die Ledger-Zeile ist alt — ein Hash, der seit langem im Ledger steht.
		if _, err := pool.Exec(ctx,
			`UPDATE distill_seen SET last_seen = now() - interval '400 days' WHERE source_key = $1`, key); err != nil {
			t.Fatalf("Ledger altern: %v", err)
		}

		// Dasselbe Material, von einem spaeteren Manifest erneut gelistet: der
		// zyklische Output, ueber dem Wasserzeichen und damit wieder im
		// Lesefenster.
		a6SeedManifest(t, pool, root, at.Add(time.Minute), []string{part})
		dfScheduler(pool, cfg, nil).distillOnce(ctx, dfNoDemand)
		if r := a6Ledger(t, pool, key); r.dup < 1 || r.selected != 0 {
			t.Fatalf("Fixture verfehlt: zweiter Lauf selected=%d dup=%d, erwartet 0/>=1", r.selected, r.dup)
		}
		if age := c5bAge(t, pool, key); age > 1 {
			t.Fatalf("das Wiedersehen hat last_seen nicht nachgezogen — Alter %.1f Tage", age)
		}

		dfScheduler(pool, cfg, nil).runSixHourJanitor(ctx)
		if n := c5bCount(t, pool, "distill_seen", key); n != 1 {
			t.Fatalf("der Purge hat den wiederkehrenden Hash geloescht (%d Ledger-Zeilen) — "+
				"seen_retention_days=30 gegen ein Wiedersehen vor Sekunden", n)
		}

		// Der Beweis am Verhalten und nicht am Zaehlerstand: ein dritter Lauf
		// ueber dasselbe Material darf es nicht erneut auswaehlen.
		dir := a6DumpRoot(t)
		third := c5bConfig(t, 90, 30)
		third.Distill.DryRunDir = dir
		a6SeedManifest(t, pool, root, at.Add(2*time.Minute), []string{part})
		dfScheduler(pool, third, nil).distillOnce(ctx, dfNoDemand)
		r := a6Ledger(t, pool, key)
		if r.selected != 0 || r.dup < 1 {
			t.Fatalf("nach dem Purge hat der Arm das Material ERNEUT gekauft: selected=%d dup=%d", r.selected, r.dup)
		}
		if recs := a6Dump(t, dir); len(recs) != 0 {
			t.Fatalf("nach dem Purge hat der Arm %d Chunk(s) erneut gedumpt", len(recs))
		}
	})

	// SONDE 5 — die beiden Schutzregeln sind UNABHAENGIG. Im Betrieb faellt der
	// Wasserzeichen-Traeger fast immer mit der juengsten Zeile zusammen (das
	// Wasserzeichen waechst monoton), also kann nur eine gestellte Journal-Lage
	// zeigen, dass zwei Regeln und nicht eine wirken. Bewusst SQL-nah gesetzt:
	// geprueft wird hier die Loeschregel, nicht das Verhalten des Arms.
	t.Run("BeideSchutzregelnGreifenGetrennt", func(t *testing.T) {
		a8Truncate(t, pool)
		const key = "c5b-split:20260830_170000"

		seed := func(wm int64, ageDays int, outcome string) {
			t.Helper()
			if _, err := pool.Exec(ctx, `
				INSERT INTO distill_run
				    (source_key, outcome, watermark_from, watermark_to, started_at, finished_at)
				VALUES ($1, $2, $3, $3, now() - make_interval(days => $4), now() - make_interval(days => $4))`,
				key, outcome, wm, ageDays); err != nil {
				t.Fatalf("Journalzeile setzen: %v", err)
			}
		}
		// Traeger des groessten Wasserzeichens, aber die AELTESTE Zeile.
		seed(900, 400, "ok")
		// Reine Fuellzeile: weder Traeger noch juengste — die einzige, die
		// gehen darf.
		seed(500, 300, "ok")
		// Juengste Zeile, aber mit KLEINEREM Wasserzeichen.
		seed(100, 200, "skipped")

		dfScheduler(pool, c5bConfig(t, 90, 30), nil).runSixHourJanitor(ctx)

		rows, err := pool.Query(ctx, `
			SELECT watermark_to FROM distill_run WHERE source_key = $1 ORDER BY watermark_to`, key)
		if err != nil {
			t.Fatalf("Journal lesen: %v", err)
		}
		defer rows.Close()
		var kept []int64
		for rows.Next() {
			var wm int64
			if err := rows.Scan(&wm); err != nil {
				t.Fatalf("scan: %v", err)
			}
			kept = append(kept, wm)
		}
		if len(kept) != 2 || kept[0] != 100 || kept[1] != 900 {
			t.Fatalf("nach dem Purge stehen %v, erwartet [100 900] — 900 ist der "+
				"Wasserzeichen-Traeger, 100 die juengste Zeile, 500 die einzige entbehrliche", kept)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM distill_run WHERE source_key = $1`, key); err != nil {
			t.Fatalf("aufraeumen: %v", err)
		}
	})
}
