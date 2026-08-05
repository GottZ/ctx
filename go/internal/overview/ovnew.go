package overview

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// ov_new — der Member-Schreibweg über pgx.CopyFrom (Achse 04 / S9a,
// design/04 §4.8 Punkt 1).
//
// ══ WAS ERSETZT WIRD ══
//
// Bisher: drei vollständige []string über ALLE Member (blocks/clusters/scopes)
// plus ⌈N/5000⌉ unnest-INSERTs. Bei 9,8M Membern sind das drei Slices à
// ~160 MB und 1.960 Roundtrips, beide vollständig innerhalb der
// Advisory-Lock-Transaktion.
//
// Jetzt: ein CopyFrom-Strom in eine TEMP-Tabelle, VOR dem Lock-Erwerb, ohne
// dass eine der drei Slices materialisiert wird — pgx.CopyFromFunc zieht die
// Zeilen einzeln aus der bereits sortierten Blockliste.
//
// ══ DIE SORTIERUNG BLEIBT ══
//
// Die Blöcke werden weiterhin sortiert durchlaufen. Das ist kein Erbstück: die
// Einfügereihenfolge bestimmt die physische Zeilenordnung in
// graph_cluster_member, und die wiederum die Reihenfolge, in der spätere
// Aggregationen Gleitkomma-Summen bilden. Ein CopyFrom in Map-Ordnung wäre
// zwischen zwei Läufen verschieden.
//
// ══ WARUM TEMP UND NICHT UNLOGGED ══
//
// §4.8 nennt beide. TEMP gewinnt hier, weil die Tabelle nur innerhalb DIESER
// Transaktion gebraucht wird und ON COMMIT DROP ihre Lebensdauer ohne
// Aufräumpfad beendet. Eine UNLOGGED-Dauertabelle wäre für andere Sessions
// sichtbar — und damit für einen zweiten, parallel laufenden Rebuild einer
// ANDEREN Partition, der sie gerade nicht sehen darf.

// ovNewDDL legt die Zwischentabelle an. ON COMMIT DROP bindet ihre Lebensdauer
// an die Transaktion — ohne das würde sie auf der Pool-Verbindung überleben
// und den nächsten Rebuild auf derselben Verbindung mit Altdaten begrüßen.
const ovNewDDL = `CREATE TEMP TABLE ov_new (
	block_id   uuid NOT NULL,
	cluster_id uuid NOT NULL,
	scope      text NOT NULL
) ON COMMIT DROP`

// buildOvNew füllt ov_new per CopyFrom.
//
// Die Scope-Prüfungen sind Wort für Wort die aus dem unnest-Pfad: ein Member
// ohne Eingabe-Scope und ein Member ausserhalb des ScopeFilters sind beide
// laute Fehler, nie eine stille Korrektur (B-W3-Eingabereinheit).
func buildOvNew(ctx context.Context, tx pgx.Tx, cl clustering, opts Options,
	nodeScopes map[string]string, scoped bool, filterSet map[string]struct{},
) error {
	if _, err := tx.Exec(ctx, ovNewDDL); err != nil {
		return fmt.Errorf("ov_new: create: %w", err)
	}

	blocks := make([]string, 0, len(cl.blockToCluster))
	for b := range cl.blockToCluster {
		blocks = append(blocks, b)
	}
	sort.Strings(blocks)

	// Der Strom prüft je Zeile und bricht beim ersten Verstoss ab — CopyFrom
	// gibt den Fehler unverändert an den Aufrufer weiter, und die Transaktion
	// rollt zurück. Kein Teilzustand, kein "die meisten Zeilen sind drin".
	var rowErr error
	i := 0
	src := pgx.CopyFromFunc(func() ([]any, error) {
		if i >= len(blocks) || rowErr != nil {
			return nil, rowErr
		}
		b := blocks[i]
		i++
		scope, ok := nodeScopes[b]
		if !ok || scope == "" {
			// Fail loud: ein Member ohne Eingabe-Scope verletzt entweder
			// NOT NULL oder landet still in der falschen Partition.
			rowErr = fmt.Errorf("ov_new: block %s has no Louvain-input scope", b)
			return nil, rowErr
		}
		if scoped {
			if _, in := filterSet[scope]; !in {
				rowErr = fmt.Errorf("ov_new: block %s scope %q outside ScopeFilter %v — input not partition-cut", b, scope, opts.ScopeFilter)
				return nil, rowErr
			}
		}
		return []any{b, cl.blockToCluster[b], scope}, nil
	})

	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"ov_new"}, []string{"block_id", "cluster_id", "scope"}, src); err != nil {
		return fmt.Errorf("ov_new: copy: %w", err)
	}
	return nil
}

// insertMembersFromOvNew schreibt die Member aus ov_new.
//
// Der clusterSet-Rückgabewert wird weiterhin in Go gebildet und nicht aus der
// Datenbank gelesen: er speist die Aggregations-Filter, und ein zweiter
// Roundtrip für eine Menge, die bereits im Speicher liegt, wäre Arbeit
// innerhalb der Lock-Haltezeit — genau die Grösse, die diese Welle senkt.
func insertMembersFromOvNew(ctx context.Context, tx pgx.Tx, cl clustering) (map[string]struct{}, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
		SELECT block_id, cluster_id, scope FROM ov_new`); err != nil {
		return nil, fmt.Errorf("insert members: %w", err)
	}
	clusterSet := make(map[string]struct{}, len(cl.blockToCluster)/8+1)
	for _, c := range cl.blockToCluster {
		clusterSet[c] = struct{}{}
	}
	return clusterSet, nil
}
