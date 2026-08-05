package overview

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Delta-Persist des Member-Schreibwegs (Achse 04 / S9b, design/04 §4.8 Punkt 2).
//
// ══ WAS SICH ÄNDERT ══
//
// Statt DELETE-alles + INSERT-alles wird ov_new gegen den Ist-Stand gejoint.
// Der Ergebniszustand ist IDENTISCH zum Vollersatz — und das ist das Gate
// (S9b-G1), nicht die Annahme.
//
// ══ WARUM ES ÜBERHAUPT LOHNT — und wogegen ══
//
// §6.5: der Vollersatz schreibt bei JEDEM Lauf alle Member-Zeilen neu und
// markiert alle alten als tot. Bei 9,8M Membern und vier Läufen am Tag sind
// das 39,2M tote Tupel pro Tag auf einer Tabelle mit 9,8M lebenden Zeilen —
// Dauer-Autovacuum plus Index-Bloat auf drei Indizes. Das ist ein
// Betriebsrisiko, das mit der reinen Rechenzeit nichts zu tun hat.
//
// ══ DER MINUUID-VERSTÄRKER (K13) — warum ZWEI Kennzahlen ══
//
// cluster_id ist die lexikographisch kleinste Member-UUID der Community.
// Kommt einer Community ein Block mit KLEINERER UUID hinzu — bei UUIDv4 mit
// Wahrscheinlichkeit ~1/(k+1) je Neuzugang in eine k-elementige Community —
// ändert sich cluster_id für ALLE Mitglieder, und Delta-Persist schreibt sie
// alle. Die geschriebene Zeilenmenge ist damit NICHT proportional zur
// Mitgliedschaftsänderung, sondern zur Grösse der von einer Umbenennung
// betroffenen Cluster.
//
// Deshalb gehen zwei Kennzahlen ins Journal statt einer:
//
//	members_changed    — geschriebene Zeilen
//	members_reassigned — davon: echte Cluster-ZUGEHÖRIGKEITS-Wechsel
//
// Die Differenz IST der Umbenennungsanteil. Beide werden seit W3 gemessen;
// S9b ist die Welle, die sie konsumiert.

// deltaDeleteTemplate entfernt jede Member-Zeile, die es so nicht mehr gibt.
//
// Das Prädikat ist bewusst "NICHT EXISTIERT eine identische ov_new-Zeile" und
// nicht "block_id fehlt": eine Zeile, deren cluster_id oder scope sich geändert
// hat, MUSS weg, damit der folgende INSERT sie neu schreiben kann. Nach diesem
// DELETE gilt: jede überlebende Zeile ist byte-gleich zu ihrer ov_new-Zeile.
const deltaDeleteTemplate = `
DELETE FROM graph_cluster_member m
 WHERE %s NOT EXISTS (
   SELECT 1 FROM ov_new n
    WHERE n.block_id = m.block_id
      AND n.cluster_id = m.cluster_id
      AND n.scope = m.scope)`

// deltaInsertSQL schreibt genau die Zeilen, die nach dem DELETE fehlen.
//
// block_id ist alleiniger PK (087-Invariante), deshalb genügt der
// Existenz-Test darauf — eine überlebende Zeile ist per Konstruktion bereits
// die richtige.
const deltaInsertSQL = `
INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
SELECT n.block_id, n.cluster_id, n.scope FROM ov_new n
 WHERE NOT EXISTS (SELECT 1 FROM graph_cluster_member m WHERE m.block_id = n.block_id)`

// applyMemberDelta ersetzt teardown+INSERT für den Member-Pfad.
//
// Sie liefert die Zahl der GESCHRIEBENEN Zeilen (gelöscht + eingefügt zählt
// als eine geschriebene Zeile je betroffenem Block) — die Grösse, die §6.5 dem
// WAL zuschreibt.
func applyMemberDelta(ctx context.Context, tx pgx.Tx, scoped bool, scopeFilter []string) (int64, error) {
	delSQL := fmt.Sprintf(deltaDeleteTemplate, "")
	var args []any
	if scoped {
		// Der Scope-Filter gehört an die GELÖSCHTE Seite: ohne ihn würde ein
		// gescopter Lauf die Zeilen FREMDER Partitionen löschen, weil sie in
		// seinem ov_new nicht vorkommen. Das ist der B1-C1-Bruch in seiner
		// teuersten Form — und der Grund, aus dem der Filter hier steht und
		// nicht im INSERT (ov_new ist bereits partitionsrein, S9a prüft das).
		delSQL = fmt.Sprintf(deltaDeleteTemplate, "m.scope = ANY($1) AND")
		args = []any{scopeFilter}
	}
	delTag, err := tx.Exec(ctx, delSQL, args...)
	if err != nil {
		return 0, fmt.Errorf("delta member delete: %w", err)
	}
	insTag, err := tx.Exec(ctx, deltaInsertSQL)
	if err != nil {
		return 0, fmt.Errorf("delta member insert: %w", err)
	}
	// Gelöscht-und-neu-geschrieben ist EIN geänderter Block, nicht zwei. Die
	// Einfügezahl ist deshalb das Mass der geschriebenen Zeilen; die
	// Löschzahl kann darüber liegen (ein Block, der ganz aus dem Schnitt
	// fällt, wird gelöscht und nie neu geschrieben).
	written := insTag.RowsAffected()
	if d := delTag.RowsAffected(); d > written {
		written = d
	}
	return written, nil
}

// writeMembers raeumt und schreibt den Member-Pfad — als eigene Funktion, weil
// persist sonst den cyclop-Deckel des Projekts reisst und weil die beiden
// Varianten (Vollersatz / Delta) genau eine abgeschlossene Entscheidung sind.
//
// Rueckgabe: die Cluster-Menge fuer die Aggregations-Filter und die Zahl der
// GESCHRIEBENEN Zeilen (nur im Delta-Pfad von Null verschieden — beim
// Vollersatz ist sie per Definition "alle" und wird von W3 gemessen).
func writeMembers(ctx context.Context, tx pgx.Tx, cl clustering, opts Options, scoped bool) (map[string]struct{}, int64, error) {
	if opts.DeltaPersist {
		// S9b: graph_cluster_member bleibt stehen und wird gejoint; alles
		// andere wird weiterhin voll geraeumt, weil die Aggregationen voll
		// laufen (die inkrementelle Variante ist S9c, bedingt UD-11-04).
		if err := teardownAggregatesOnly(ctx, tx, scoped, opts.ScopeFilter); err != nil {
			return nil, 0, err
		}
		written, err := applyMemberDelta(ctx, tx, scoped, opts.ScopeFilter)
		if err != nil {
			return nil, 0, err
		}
		return clusterSetOf(cl), written, nil
	}
	if err := teardown(ctx, tx, scoped, opts.ScopeFilter); err != nil {
		return nil, 0, err
	}
	set, err := insertMembersFromOvNew(ctx, tx, cl)
	return set, 0, err
}

// teardownAggregatesOnly räumt alles AUSSER graph_cluster_member.
//
// Der Member-Pfad wird bei aktivem Delta-Persist nicht mehr leergeräumt;
// alles andere schon, weil die Aggregationen weiterhin voll laufen (die
// inkrementelle Variante ist S9c und hängt an UD-11-04).
//
// B1-C1 bleibt gewahrt: DELETE-Filter und Aggregations-Filter sind
// unverändert derselbe scope-Ausdruck wie im Vollersatz — Delta betrifft
// AUSSCHLIESSLICH die Member-Tabelle.
func teardownAggregatesOnly(ctx context.Context, tx pgx.Tx, scoped bool, scopeFilter []string) error {
	if !scoped {
		if _, err := tx.Exec(ctx, `TRUNCATE graph_cluster_node, graph_cluster_edge,
			graph_cluster_topic_edge, graph_cluster_super, graph_cluster_super_member`); err != nil {
			return fmt.Errorf("truncate aggregates: %w", err)
		}
		return nil
	}
	for _, del := range []struct{ label, sql string }{
		{"node", `DELETE FROM graph_cluster_node WHERE scope = ANY($1)`},
		{"edge", `DELETE FROM graph_cluster_edge WHERE scope_s = ANY($1) OR scope_t = ANY($1)`},
		{"topic edge", `DELETE FROM graph_cluster_topic_edge WHERE scope_a = ANY($1) OR scope_b = ANY($1)`},
		{"super member", `DELETE FROM graph_cluster_super_member WHERE scope = ANY($1)`},
		{"super", `DELETE FROM graph_cluster_super WHERE scope = ANY($1)`},
	} {
		if _, err := tx.Exec(ctx, del.sql, scopeFilter); err != nil {
			return fmt.Errorf("scoped %s teardown: %w", del.label, err)
		}
	}
	return nil
}
