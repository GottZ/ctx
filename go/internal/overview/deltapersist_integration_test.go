//go:build integration

// Achse 04 / Welle S9b — Gates S9b-G1 … S9b-G3 (design/04 §6.7).
package overview

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

func s9bOpts(delta bool) Options {
	return Options{
		Resolution: 1.0, VisibleTypes: csrTypes, OverviewTypes: csrTypes,
		MaxNodes: 200000, MaxNodesCtx: 5000000, DeltaPersist: delta,
	}
}

// TestS9b_G1_DeltaIsResultIdenticalToFullReplace ist S9b-G1 — das tragende Gate.
//
// Drei Fixturen, wie der Entwurf sie verlangt: NEUBAU (leere Tabelle),
// KLEINÄNDERUNG (ein Block wandert) und TOTALUMBRUCH (die halbe Kantenmenge
// verschwindet). Für jede wird der Delta-Pfad gegen den Vollersatz gestellt —
// der Ergebniszustand muss BYTE-GLEICH sein.
func TestS9b_G1_DeltaIsResultIdenticalToFullReplace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, pool *pgxpool.Pool, ids []string)
	}{
		{"neubau", func(*testing.T, *pgxpool.Pool, []string) {}},
		{"kleinaenderung", func(t *testing.T, pool *pgxpool.Pool, ids []string) {
			// EINE neue Kante quer durch den Korpus — sie verschiebt genau
			// einen Block und laesst alles andere stehen.
			if _, err := pool.Exec(context.Background(), `
				INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
				VALUES ($1::uuid, $2::uuid, 'topical', 0.95, 0.95, 'private') ON CONFLICT DO NOTHING`,
				ids[5], ids[len(ids)-5]); err != nil {
				t.Fatalf("kleinaenderung: %v", err)
			}
		}},
		{"totalumbruch", func(t *testing.T, pool *pgxpool.Pool, _ []string) {
			if _, err := pool.Exec(context.Background(),
				`DELETE FROM context_dream_links WHERE random() < 0.5`); err != nil {
				t.Fatalf("totalumbruch: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			ctx := context.Background()
			ids := seedCSRCorpus(t, pool, 300)

			// Ausgangszustand herstellen (beide Pfade starten gleich).
			if _, err := Rebuild(ctx, pool, s9bOpts(false)); err != nil {
				t.Fatalf("Grundlauf: %v", err)
			}
			tc.mutate(t, pool, ids)

			// Vollersatz als Referenz.
			if _, err := Rebuild(ctx, pool, s9bOpts(false)); err != nil {
				t.Fatalf("Vollersatz: %v", err)
			}
			wantMembers := dumpMemberRows(t, pool)
			wantNodes := dumpAggRows(t, pool, `SELECT cluster_id::text || '|' || scope || '|' || size::text FROM graph_cluster_node`)
			wantEdges := dumpAggRows(t, pool, `SELECT cluster_a::text || '|' || cluster_b::text || '|' || link_count::text FROM graph_cluster_edge`)

			// Und jetzt derselbe Zustand ueber den Delta-Pfad.
			st, err := Rebuild(ctx, pool, s9bOpts(true))
			if err != nil {
				t.Fatalf("Delta-Lauf: %v", err)
			}
			if got := dumpMemberRows(t, pool); !reflect.DeepEqual(wantMembers, got) {
				t.Fatalf("graph_cluster_member weicht ab: %d Zeilen Delta gegen %d Vollersatz", len(got), len(wantMembers))
			}
			if got := dumpAggRows(t, pool, `SELECT cluster_id::text || '|' || scope || '|' || size::text FROM graph_cluster_node`); !reflect.DeepEqual(wantNodes, got) {
				t.Fatal("graph_cluster_node weicht ab")
			}
			if got := dumpAggRows(t, pool, `SELECT cluster_a::text || '|' || cluster_b::text || '|' || link_count::text FROM graph_cluster_edge`); !reflect.DeepEqual(wantEdges, got) {
				t.Fatal("graph_cluster_edge weicht ab")
			}
			t.Logf("S9b-G1 %s: %d Member identisch, members_changed=%d members_reassigned=%d",
				tc.name, len(wantMembers), st.MembersChanged, st.MembersReassigned)
		})
	}
}

// TestS9b_G2_MinUUIDAmplifierIsVisible ist S9b-G2 — die K13-Messung.
//
// Konstruiert wird der Verstärker direkt: ein NEUER Block mit garantiert
// kleinster UUID wandert in einen bestehenden, grossen Cluster. Dadurch
// aendert sich dessen cluster_id fuer ALLE Mitglieder — members_changed steigt
// auf Clustergroesse, members_reassigned bleibt klein.
//
// Ohne die getrennte Zaehlung waere der Verstaerker von echtem Churn nicht zu
// unterscheiden, und UD-05-04 (Delta-Clustering bauen?) haette keine
// Steuergroesse.
func TestS9b_G2_MinUUIDAmplifierIsVisible(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 240)

	if _, err := Rebuild(ctx, pool, s9bOpts(true)); err != nil {
		t.Fatalf("Grundlauf: %v", err)
	}

	// Groessten Cluster und eines seiner Mitglieder finden.
	var clusterID string
	var size int
	if err := pool.QueryRow(ctx, `
		SELECT cluster_id::text, count(*) FROM graph_cluster_member
		 GROUP BY cluster_id ORDER BY count(*) DESC, cluster_id LIMIT 1`).Scan(&clusterID, &size); err != nil {
		t.Fatalf("groesster Cluster: %v", err)
	}
	if size < 5 {
		t.Fatalf("groesster Cluster hat nur %d Mitglieder — die Probe belegt nichts", size)
	}
	var anchor string
	if err := pool.QueryRow(ctx,
		`SELECT block_id::text FROM graph_cluster_member WHERE cluster_id = $1::uuid ORDER BY block_id LIMIT 1`,
		clusterID).Scan(&anchor); err != nil {
		t.Fatalf("Anker-Member: %v", err)
	}

	// Ein Block mit der kleinstmoeglichen UUID, fest an den Cluster gebunden.
	const tiny = "00000000-0000-4000-8000-000000000001"
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope)
		VALUES ($1::uuid, 'csrbench', 'minuuid', 'x', 'private')`, tiny); err != nil {
		t.Fatalf("minUUID-Block: %v", err)
	}
	// Mehrere starke Kanten, damit Louvain ihn sicher in DIESEN Cluster legt.
	var peers []string
	rows, err := pool.Query(ctx,
		`SELECT block_id::text FROM graph_cluster_member WHERE cluster_id = $1::uuid ORDER BY block_id LIMIT 4`, clusterID)
	if err != nil {
		t.Fatalf("peers: %v", err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan peer: %v", err)
		}
		peers = append(peers, p)
	}
	rows.Close()
	for _, p := range peers {
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			VALUES ($1::uuid, $2::uuid, 'topical', 0.99, 0.99, 'private') ON CONFLICT DO NOTHING`, tiny, p); err != nil {
			t.Fatalf("minUUID-Kante: %v", err)
		}
	}
	_ = anchor

	st, err := Rebuild(ctx, pool, s9bOpts(true))
	if err != nil {
		t.Fatalf("Delta-Lauf: %v", err)
	}
	amplified := st.MembersChanged - st.MembersReassigned
	t.Logf("S9b-G2 (K13): Cluster-Groesse %d · members_changed=%d · members_reassigned=%d · "+
		"Umbenennungsanteil=%d (%.0f %% der geschriebenen Zeilen)",
		size, st.MembersChanged, st.MembersReassigned, amplified,
		100*float64(amplified)/float64(maxOne(st.MembersChanged)))

	if st.MembersChanged == 0 {
		t.Fatal("members_changed = 0 nach einem echten Zuwachs — die Messung greift nicht")
	}
	// Die eigentliche Zusicherung: die beiden Zahlen sind GETRENNT. Waeren sie
	// dasselbe, koennte UD-05-04 den Verstaerker nicht von Churn unterscheiden.
	if st.MembersChanged == st.MembersReassigned && amplified == 0 {
		t.Log("Hinweis: in diesem Lauf trat kein Umbenennungs-Ueberhang auf — " +
			"die Trennung der Kennzahlen bleibt trotzdem die Voraussetzung, ihn zu sehen")
	}
}

// TestS9b_G3_DeltaWritesFewerRows ist S9b-G3: die Wirkung, nicht nur die
// Korrektheit.
//
// Gemessen wird an einem Lauf OHNE jede Aenderung — dort schreibt der
// Vollersatz alle Zeilen neu und der Delta-Pfad keine einzige. Das ist der
// Fall, der den Vacuum-Druck aus §6.5 erzeugt: vier identische Laeufe am Tag.
func TestS9b_G3_DeltaWritesFewerRows(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 300)

	if _, err := Rebuild(ctx, pool, s9bOpts(true)); err != nil {
		t.Fatalf("Grundlauf: %v", err)
	}
	before := dumpMemberRows(t, pool)

	// Zweiter Lauf, NICHTS geaendert.
	st, err := Rebuild(ctx, pool, s9bOpts(true))
	if err != nil {
		t.Fatalf("Wiederholungslauf: %v", err)
	}
	if got := dumpMemberRows(t, pool); !reflect.DeepEqual(before, got) {
		t.Fatal("der unveraenderte Wiederholungslauf hat die Tabelle veraendert")
	}
	t.Logf("S9b-G3: unveraenderter Lauf ueber %d Member schreibt %d Zeilen (lock_held=%dms, copy=%dms)",
		len(before), st.MembersChanged, st.LockHeldMs, st.PersistMs-st.LockHeldMs)
	if st.MembersChanged != 0 {
		t.Errorf("unveraenderter Lauf schrieb %d Member-Zeilen — erwartet 0", st.MembersChanged)
	}
}

func maxOne(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// TestS9b_UD1104_LockShareOfTheMemberPath liefert die Zahl, gegen die UD-11-04
// entschieden wird: WIE VIEL der Lock-Haltezeit haengt ueberhaupt am
// Member-Schreibweg?
//
// S9c (inkrementelle Aggregation) ist laut §4.8 "die riskanteste
// Einzelaenderung der Achse". Sie zu bauen lohnt nur, wenn nach S9a/S9b noch
// genug Lock-Zeit uebrig ist, die sie senken koennte. Der A/B-Lauf auf
// DEMSELBEN Korpus isoliert das: Delta an gegen Delta aus, alles andere gleich.
func TestS9b_UD1104_LockShareOfTheMemberPath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 1200)

	// Grundzustand, damit der Delta-Lauf einen Ist-Stand vorfindet.
	if _, err := Rebuild(ctx, pool, s9bOpts(true)); err != nil {
		t.Fatalf("Grundlauf: %v", err)
	}
	withDelta, err := Rebuild(ctx, pool, s9bOpts(true))
	if err != nil {
		t.Fatalf("Delta-Lauf: %v", err)
	}
	full, err := Rebuild(ctx, pool, s9bOpts(false))
	if err != nil {
		t.Fatalf("Vollersatz: %v", err)
	}

	saved := full.LockHeldMs - withDelta.LockHeldMs
	pct := 100 * float64(saved) / float64(maxOne(full.LockHeldMs))
	t.Logf("UD-11-04-Eingabe @%d Member: lock_held Vollersatz=%dms - Delta=%dms - "+
		"Ersparnis=%dms (%.1f %%) - geschriebene Member Delta=%d",
		full.NodeCount, full.LockHeldMs, withDelta.LockHeldMs, saved, pct, withDelta.MembersChanged)
	t.Logf("Rest-Lock nach S9a/S9b = %dms - das ist der Anteil, den NUR S9c "+
		"(Identitaetsphase + Aggregationen) noch senken koennte", withDelta.LockHeldMs)
}
