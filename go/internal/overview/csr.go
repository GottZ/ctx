package overview

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/visibility"
)

// Das CSR-Substrat (Achse 04 / S3, design/04 §4.5).
//
// WAS ES ERSETZT — und warum das der Hebel ist. Der Ist-Pfad materialisiert
// den Graphen dreimal, bevor gonum ihn überhaupt sieht:
//
//	M1  []rawEdge          — zwei UUID-STRINGS je Kante (cluster.go:405-437)
//	M2  map[pair]float64   — ein Eintrag je ungerichtetem Paar (:487-509)
//	idx map[string]int64   — UUID → Ladeposition (:478-481)
//
// S1 hat gemessen, was das kostet: 423 MB Spitze bei 200k Knoten / 450k Paaren,
// gegen ein §6.3(a)-Modell von ~254 MB — das Modell UNTERSCHÄTZT den Ist-Pfad
// um 66 %, und der Container hat 512 MiB. Der CSR-Pfad lässt alle drei weg:
// Kanten leben als zwei uint32-Slices plus float64-Gewichte, und die
// Endpunkt-Auflösung ist eine BINÄRSUCHE über die ohnehin sortierten Roh-Bytes
// statt einer Hash-Map über Strings.
//
// WARUM DIE BINÄRSUCHE ZULÄSSIG IST: PostgreSQL vergleicht `uuid` per memcmp,
// `ORDER BY cb.id` liefert damit exakt die Go-Bytes-Ordnung von [16]byte.
// Dasselbe Argument trägt bereits graphcache.Snapshot.UUIDs (snapshot.go:21).
// Der Mechanismus ist damit festgehalten, nicht implizit — er trägt
// Determinismus-Achse 2 mit.
//
// WARUM EINE TRANSAKTION: der Zwei-Pass-Build MUSS beide Passen auf demselben
// Snapshot fahren. Dream schreibt Links im Hintergrund per Replace-Sweep
// (DELETE + INSERT, dream/writelinks.go:49/56) — genau während eines Rebuilds.
// Kommt zwischen den Passen eine Kante hinzu, schriebe Passe 2 über die
// Off-Grenze des Nachbarknotens; fällt eine weg, blieben Nullziele stehen, die
// auf Knoten 0 zeigen. Beides erzeugt eine falsche Partition OHNE
// Fehlermeldung. REPEATABLE READ schließt das; die Kehrseite (offene
// Lese-Transaktion hält xmin und blockiert Vacuum) ist real und steht in
// load_ms des Journals.

// txRepeatableReadReadOnly ist die Isolationsstufe des Zwei-Pass-Builds — als
// Funktion und nicht als Literal an der Aufrufstelle, damit das Gate S3-G6 die
// EXAKT gleiche Stufe faehrt wie die Produktion. Ein Gate, das seine eigene
// Transaktion anders oeffnet als der Code, den es prueft, belegt nichts.
func txRepeatableReadReadOnly() pgx.TxOptions {
	return pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
}

// csrGraph ist eine symmetrische, gewichtete CSR über den Knotenschnitt.
// Jede ungerichtete Kante {u,v} erscheint zweimal — in Adj[Off[u]:Off[u+1]]
// und in Adj[Off[v]:Off[v+1]].
type csrGraph struct {
	Off []uint32 // len == N+1
	Adj []uint32 // len == 2E
	// W ist in S3 bewusst float64 und noch nicht schmaler (§4.3, zweistufige
	// Gewichtsbreite). Hier gilt das Byte-Identitaets-Gate gegen gonum: der
	// Ist-Pfad aggregiert in map[pair]float64 und uebergibt float64 an
	// SetWeightedEdge, und gonums Zugvergleich ist ein blankes
	// `dQ <= deltaQtol` (louvain_undirected.go:487) — jede Rundung kann einen
	// knappen Fall kippen und ein Gate rot faerben, dessen Rot dann NICHTS
	// ueber die Korrektheit der CSR sagt. Die Verschmaelerung (float32 oder
	// uint16-Fixpunkt wie graphcache.CSR.RawConf) gehoert zu engine=ctx in
	// S4/S6, wo kein Byte-Vergleich gegen gonum mehr gilt.
	W []float64 // len == 2E, parallel zu Adj

	// Pairs ist die Zahl der UNGERICHTETEN Paare nach Deduplizierung — nicht
	// die geladene Zeilenmenge. Dangling und SelfLoops sind die beiden Mengen,
	// die der Ist-Pfad still verwirft (live 264 von 3.519 Kanten).
	Pairs     int
	Dangling  int
	SelfLoops int
}

// loadCSR baut Knotenschnitt und CSR in EINER REPEATABLE-READ-Transaktion.
//
// Rückgabe formgleich zum Ist-Pfad (loadNodes + loadEdges + Symmetrisierung),
// damit S3 gegen die Vorwelle byte-vergleichbar bleibt: dieselbe
// UUID-Reihenfolge, dieselben Scopes, dieselben Kantengewichte.
func loadCSR(ctx context.Context, pool *pgxpool.Pool, nodeTypes []string, scopeFilter []string) ([]string, map[string]string, *csrGraph, error) {
	tx, err := pool.BeginTx(ctx, txRepeatableReadReadOnly())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("csr: begin repeatable read: %w", err)
	}
	// Read-only + Rollback: es gibt nichts zu committen, und ein Rollback
	// beendet die Snapshot-Haltung sofort — das ist der Punkt, an dem der
	// Vacuum-Block endet.
	defer func() { _ = tx.Rollback(ctx) }()

	raw, scopes, err := csrLoadNodes(ctx, tx, nodeTypes, scopeFilter)
	if err != nil {
		return nil, nil, nil, err
	}
	g, err := csrLoadEdges(ctx, tx, raw, scopeFilter)
	if err != nil {
		return nil, nil, nil, err
	}

	// Die String-Form entsteht ERST HIER und genau einmal. Sie ist unvermeidbar:
	// persist bindet block_id als Text, und blockToCluster/intraDegree sind
	// string-geschlüsselt. Der Gewinn dieser Welle liegt auf der KANTEN-Seite —
	// die Knoten-Seite bleibt bis S9 wie sie ist.
	uuids := make([]string, len(raw))
	scopeMap := make(map[string]string, len(raw))
	for i, b := range raw {
		u := formatUUID(b)
		uuids[i] = u
		scopeMap[u] = scopes[i]
	}
	return uuids, scopeMap, g, nil
}

// csrLoadNodes liest den Knotenschnitt als Roh-Bytes.
//
// Gegenüber loadNodes ist das eine QUERY-ÄNDERUNG und keine byte-identische
// Kopie: cb.id wird nativ als uuid gescannt statt per ::text. Das Prädikat und
// das ORDER BY bleiben Wort für Wort. Der Gate-Text von S3 sagt das
// ausdrücklich, statt "byte-identisch" zu behaupten.
func csrLoadNodes(ctx context.Context, tx pgx.Tx, nodeTypes []string, scopeFilter []string) ([][16]byte, []string, error) {
	query := fmt.Sprintf(`SELECT cb.id, cb.scope FROM context_blocks cb
		 WHERE %s
		 ORDER BY cb.id`, visibility.TypeVisible("cb", "$1"))
	args := []any{nodeTypes}
	if len(scopeFilter) > 0 {
		query = fmt.Sprintf(`SELECT cb.id, cb.scope FROM context_blocks cb
		 WHERE %s
		   AND cb.scope = ANY($2)
		 ORDER BY cb.id`, visibility.TypeVisible("cb", "$1"))
		args = append(args, scopeFilter)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("csr: loading nodes: %w", err)
	}
	defer rows.Close()

	var ids [][16]byte
	var scopes []string
	for rows.Next() {
		var id [16]byte
		var scope string
		if err := rows.Scan(&id, &scope); err != nil {
			return nil, nil, fmt.Errorf("csr: scanning node: %w", err)
		}
		ids = append(ids, id)
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("csr: loading nodes: %w", err)
	}
	return ids, scopes, nil
}

// csrEdgeQuery ist die Kanten-Selektion beider Passen — WORTGLEICH zu
// loadEdges, bis auf den Verzicht auf ::text.
//
// ABWEICHUNG VOM ENTWURF, mit Grund. design/04 §4.5 Schritt 5 verlangt, die
// Ladeordnung um `relationship` zu erweitern, damit Gleichstände zwischen
// PARALLELEN Links auf demselben Paar deterministisch aufgelöst werden. Diese
// Erweiterung bleibt hier AUS: context_dream_links trägt
// PRIMARY KEY (source_block_id, target_block_id) (016_dream.sql:25) — zwei
// Zeilen auf demselben GERICHTETEN Paar kann es nicht geben. Die Sorge des
// Entwurfs adressiert einen Fall, den das Schema ausschließt; (source, target)
// ist bereits eine totale Ordnung. Eine dritte Sortierspalte würde dafür den
// PK-Index als Ordnungsquelle entwerten und bei 98M Zeilen einen externen Sort
// erzwingen — Kosten ohne Gegenwert.
//
// Was BLEIBT, ist die Summationsordnung für die zwei RICHTUNGEN eines Paares:
// die Beiträge aus (a→b) und (b→a) treffen an verschiedenen Cursor-Positionen
// ein. Beide Pfade — Ist und CSR — summieren in Cursor-Reihenfolge, deshalb ist
// die Gleitkomma-Summe bitgleich (Gate S3-G1).
const csrEdgeQuery = `SELECT source_block_id, target_block_id, raw_confidence
	 FROM context_dream_links
	 WHERE relationship <> 'supersedes'
	 ORDER BY source_block_id, target_block_id`

const csrEdgeQueryScoped = `SELECT l.source_block_id, l.target_block_id, l.raw_confidence
	 FROM context_dream_links l
	 JOIN context_blocks bs ON bs.id = l.source_block_id AND bs.scope = ANY($1)
	 JOIN context_blocks bt ON bt.id = l.target_block_id AND bt.scope = ANY($1)
	 WHERE l.relationship <> 'supersedes'
	 ORDER BY l.source_block_id, l.target_block_id`

// csrLoadEdges ist der zweistufige Build.
func csrLoadEdges(ctx context.Context, tx pgx.Tx, nodes [][16]byte, scopeFilter []string) (*csrGraph, error) {
	n := len(nodes)
	g := &csrGraph{Off: make([]uint32, n+1)}
	if n == 0 {
		return g, nil
	}

	// ── Passe 1: Grade zählen ──────────────────────────────────────────────
	//
	// Gezählt werden ZEILEN, nicht Paare: ein Paar, das in beiden Richtungen
	// existiert, belegt hier zwei Slots. Die Überbelegung (live 1,21× — 3.255
	// gerichtete Zeilen auf 2.679 Paare) wird in der Verdichtung unten wieder
	// abgebaut. Der umgekehrte Weg — erst deduplizieren, dann zählen — bräuchte
	// genau die Map, die diese Welle abschafft.
	deg := make([]uint32, n)
	err := csrScanEdges(ctx, tx, scopeFilter, func(src, dst [16]byte, _ float64) {
		u, okU := csrLookup(nodes, src)
		v, okV := csrLookup(nodes, dst)
		if !okU || !okV {
			g.Dangling++
			return
		}
		if u == v {
			g.SelfLoops++
			return
		}
		deg[u]++
		deg[v]++
	})
	if err != nil {
		return nil, err
	}
	var acc uint32
	for i, d := range deg {
		g.Off[i] = acc
		acc += d
	}
	g.Off[n] = acc

	// ── Passe 2: füllen ────────────────────────────────────────────────────
	g.Adj = make([]uint32, acc)
	g.W = make([]float64, acc)
	fill := make([]uint32, n)
	// Die Zähler werden NEU erhoben statt aus Passe 1 übernommen: stimmten sie
	// nicht überein, wäre der Snapshot gebrochen — und genau das prüft die
	// Grenzwacht unten. Ein stillschweigend übernommener Zähler könnte den
	// Bruch nicht sehen.
	p2Dangling, p2SelfLoops := 0, 0
	err = csrScanEdges(ctx, tx, scopeFilter, func(src, dst [16]byte, w float64) {
		u, okU := csrLookup(nodes, src)
		v, okV := csrLookup(nodes, dst)
		if !okU || !okV {
			p2Dangling++
			return
		}
		if u == v {
			p2SelfLoops++
			return
		}
		// DEFENSIVE GRENZE (§4.5 Schritt 4). Ohne sie schriebe eine zwischen den
		// Passen eingefügte Kante in die Adjazenz des NACHBARKNOTENS — eine
		// stille Korruption, die als falsche Partition ohne Fehlermeldung
		// endet. Mit REPEATABLE READ darf das nicht passieren; die Prüfung ist
		// der Beleg dafür, nicht ihr Ersatz.
		if g.Off[u]+fill[u] >= g.Off[u+1] || g.Off[v]+fill[v] >= g.Off[v+1] {
			panic("csr: edge overflow between passes — snapshot isolation broken")
		}
		g.Adj[g.Off[u]+fill[u]] = uint32(v) //nolint:gosec // v < n < 2^32
		g.W[g.Off[u]+fill[u]] = w
		fill[u]++
		g.Adj[g.Off[v]+fill[v]] = uint32(u) //nolint:gosec // u < n < 2^32
		g.W[g.Off[v]+fill[v]] = w
		fill[v]++
	})
	if err != nil {
		return nil, err
	}
	if p2Dangling != g.Dangling || p2SelfLoops != g.SelfLoops {
		return nil, fmt.Errorf(
			"csr: the two passes disagree (dangling %d vs %d, self-loops %d vs %d) — snapshot isolation is not holding",
			g.Dangling, p2Dangling, g.SelfLoops, p2SelfLoops)
	}

	csrCompact(g, n)
	return g, nil
}

// csrCompact sortiert jede Adjazenzliste nach Ziel-Index und fasst
// Mehrfacheinträge desselben Ziels zusammen.
//
// Das ist die neue Determinismus-Achse 3: sie ersetzt die globale sort.Slice
// über die aggregierten Paare (cluster.go:509-517). Die Sortierung ist STABIL,
// und das ist load-bearing — die beiden Richtungen eines Paares treffen an
// verschiedenen Cursor-Positionen ein, und float64-Addition ist nicht
// assoziativ. Eine instabile Sortierung würde die Summationsreihenfolge
// zwischen Läufen wackeln lassen und damit das letzte ULP des Gewichts, an dem
// gonums `dQ <= deltaQtol` einen knappen Zug entscheiden kann.
func csrCompact(g *csrGraph, n int) {
	newOff := make([]uint32, n+1)
	var out uint32
	for u := 0; u < n; u++ {
		lo, hi := g.Off[u], g.Off[u+1]
		adj := g.Adj[lo:hi]
		w := g.W[lo:hi]
		idx := make([]int, len(adj))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return adj[idx[a]] < adj[idx[b]] })

		newOff[u] = out
		var prev uint32
		first := true
		for _, i := range idx {
			t, weight := adj[i], w[i]
			if !first && t == prev {
				g.W[out-1] += weight // Cursor-Ordnung bleibt erhalten (stabile Sortierung)
				continue
			}
			g.Adj[out] = t
			g.W[out] = weight
			out++
			prev = t
			first = false
		}
	}
	newOff[n] = out
	g.Off = newOff
	g.Adj = g.Adj[:out]
	g.W = g.W[:out]
	g.Pairs = int(out) / 2 // jede ungerichtete Kante steht zweimal
}

// csrScanEdges fährt den Kanten-Cursor einmal.
func csrScanEdges(ctx context.Context, tx pgx.Tx, scopeFilter []string, fn func(src, dst [16]byte, w float64)) error {
	query, args := csrEdgeQuery, []any(nil)
	if len(scopeFilter) > 0 {
		query, args = csrEdgeQueryScoped, []any{scopeFilter}
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("csr: loading edges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, dst [16]byte
		var w float64
		if err := rows.Scan(&src, &dst, &w); err != nil {
			return fmt.Errorf("csr: scanning edge: %w", err)
		}
		fn(src, dst, w)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("csr: loading edges: %w", err)
	}
	return nil
}

// csrLookup löst einen Endpunkt per Binärsuche über die nach Roh-Bytes
// sortierten Knoten auf. O(log N) Zeit, NULL Zusatzspeicher — der Ersatz für
// idx map[string]int64 (§6.3: 882 MB @9,8M).
func csrLookup(nodes [][16]byte, id [16]byte) (int, bool) {
	i := sort.Search(len(nodes), func(i int) bool {
		return bytesGE(nodes[i], id)
	})
	if i < len(nodes) && nodes[i] == id {
		return i, true
	}
	return 0, false
}

// bytesGE ist der memcmp-Vergleich, auf dem die ganze Binärsuche ruht:
// PostgreSQL sortiert uuid per memcmp, also liefert ORDER BY cb.id exakt diese
// Ordnung.
func bytesGE(a, b [16]byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// formatUUID schreibt die kanonische Textform (kleingeschrieben, mit
// Bindestrichen) — die Form, die persist bindet und die loadNodes' ::text
// erzeugt hat.
func formatUUID(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
