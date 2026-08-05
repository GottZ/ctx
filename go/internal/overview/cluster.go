// Package overview builds the F5-W6 graph "landkarte": a precomputed Louvain
// cluster supergraph over context_dream_links. The heavy compute (gonum,
// in-memory, single-threaded) runs offline in a scheduler goroutine and writes
// scope-PARTITIONED aggregate tables (migration 057); the read path
// (store.GraphOverview, F5-W6-W2) sums only the rows of the caller's readScopes.
// See design 07-graph-overview.md.
package overview

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"

	"github.com/GottZ/ctx/internal/visibility"
)

const (
	// louvainSeed1/2 are HARD-WIRED so Modularize is reproducible across runs
	// (math/rand/v2 PCG source). Determinism hinges on TWO axes: this fixed
	// seed AND a reproducible node/edge insertion order (loadNodes/loadEdges
	// use ORDER BY). Drift on either silently changes the partition — guarded
	// by the modularity Q-score smoke (graph_overview_meta) + the unit test.
	louvainSeed1 uint64 = 0x637478_6f766572  // "ctx over"
	louvainSeed2 uint64 = 0x6c6f757661696e21 // "louvain!"

	// overviewLockKey gates the rebuild tx with pg_try_advisory_xact_lock so two
	// ctxd instances (multi-tenant line) never overwrite the tables against each
	// other. First advisory lock in the repo. Since B-W4 this is the BASE key:
	// a global run (nil ScopeFilter) locks exactly this value, a partition run
	// XORs a process-stable hash of its scope set on top (lockKeyForScopes).
	overviewLockKey int64 = 0x6f76727677 // "ovrvw"

	// memberBatch caps the per-INSERT unnest array size (1M+ members → batched,
	// not one giant bind parameter).
	memberBatch = 5000
)

// persistTempBuffers dimensions the temp-table working set of the W3 identity
// phase. The PostgreSQL default is 8 MB; ov_prev (one row per member plus a PK
// index) and ov_overlap (up to one row per member) exceed that at the node cap,
// and the overflow lands in pgsql_tmp — on DISK, INSIDE the advisory-locked
// transaction. That is the difference between single- and double-digit seconds
// of lock hold time.
//
// Code-owned, not a config key — and it MUST NOT become one. PostgreSQL
// rejects a CHANGE of temp_buffers once the session has touched a temp table,
// with SQLSTATE 22023 (the same code as a range violation, so the error reads
// like a bad value). Pool connections are reused across rebuilds, so a
// hot-reloadable knob would work exactly once per connection and then poison
// every persist that landed on an already-used one. The value stays a constant
// of the code, set as the FIRST statement of the transaction — the only point
// at which it can be set at all.
//
// A var rather than a const solely so the W3-G11 gate can measure the spill it
// prevents; production never writes to it.
var persistTempBuffers = "64MB"

// Stats is the result of one rebuild for logging/telemetry.
//
// W-A note: this struct crosses the worker IPC boundary and BOTH decoders are
// strict (worker.go DisallowUnknownFields) — a field addition is a protocol
// change and must land on both sides in the same commit.
type Stats struct {
	Skipped      bool    // no-op run — SkipReason says why
	SkipReason   string  // "advisory-lock" | "node-cap" (empty when not skipped)
	NodeCount    int     // clustered blocks (members)
	ClusterCount int     // distinct communities
	EdgeRows     int     // rows written to graph_cluster_edge (scope-pair partitioned)
	Modularity   float64 // gonum Q-score of the partition

	// CandidateCount is the node-candidate count PER SCOPE of this attempt
	// (visible ∩ overview.include, tallied over loadNodes' nodeScopes BEFORE
	// the MaxNodes cap) — the denominator of the root map's coverage figure
	// and the distance to max_nodes (W-A, migration 123).
	//
	// A MAP, never a scalar: the meta rows are per-scope (GROUP BY n.scope),
	// so a passed-through run total would write "1.204.331 candidates" into
	// the persisted, backup- and export-travelling block of a tenant that
	// only sees 3.000 — wordwise the difference channel on foreign corpus
	// size that BP-1 forbids.
	CandidateCount map[string]int

	// ── W3: the identity lifecycle of this run ──────────────────────────────
	//
	// Without these the topic layer is a black box from outside the worker
	// child: a run that renames every topic and a run that changes nothing
	// look identical in the log. They are counters, never inputs — nothing in
	// the rebuild reads them back.
	//
	// PROTOCOL NOTE (same one the struct header carries): these seven fields
	// cross the strict worker IPC decoders, so W3 must ship in the same deploy
	// window as the other Stats-extending waves (K12: W-A, S2).
	TopicsCarried    int // continued from the living predecessor generation
	TopicsReattached int // revived from a tombstone (E2-01 / A01-2)
	TopicsBorn       int // fresh identity, origin_kind='birth'
	TopicsSplit      int // fresh identity, origin_kind='split'
	TopicsRetired    int // died this run (plain death or merged_into)

	// MembersChanged/MembersReassigned are the K13 measurement (masterplan
	// §2): a newcomer with a smaller uuid renames the WHOLE community, so a
	// future delta-persist would rewrite every member row of a community that
	// did not actually move. MembersReassigned is exactly that subset — the
	// member changed cluster_id while its TOPIC stayed the same. Pure
	// measurement in W3; the decision whether the topic identity replaces
	// cluster_id as the delta key belongs to Achse 04 (S9b).
	MembersChanged    int
	MembersReassigned int

	// ── W-F: the meta-cluster level of this run ─────────────────────────────
	//
	// SuperN is the group count over ALL scopes and exists for the log line
	// only — the per-scope truth lives in graph_overview_meta.super_n, because
	// a run total in a tenant's own row would be the BP-1 difference channel.
	// SuperCapped says at least one scope's supergraph was above
	// root_map.super_max_nodes and degraded to the flat map; the main rebuild
	// committed regardless, which is the whole reason the two are separate.
	SuperN      int
	SuperCapped bool

	// ── S2: die Messfläche des Lauf-Journals (Achse 04, Migration 130) ──────
	//
	// PROTOKOLL-HINWEIS (K12, derselbe wie im Struct-Kopf): diese Felder kreuzen
	// den strikten Kind→Eltern-Decoder. Sie landen in EINEM Commit auf beiden
	// Seiten — was hier trivial ist, weil Eltern und Kind dasselbe Binary sind
	// und ein Container-Deploy beide atomar tauscht. Das dokumentierte
	// Mischversions-Fenster (BP-8) bleibt: ein noch laufendes altes Kind unter
	// einem neuen Elternprozess scheitert LAUT statt still.
	//
	// Ohne diese Felder ist jede Skalen-Aussage der Achse eine Behauptung: die
	// Phasendauern, die Lock-Haltezeit und der Speicher-Hochwassermarker sind
	// heute NIRGENDS sichtbar — weder in Stats noch im Log (scheduler.go:
	// 1258-1261). S1 hat sie extern gemessen; ab hier misst sie der Lauf selbst.

	// LoadMs/ClusterMs/PersistMs zerlegen die Wall-Clock in ihre drei Phasen.
	// Die Trennung ist die Voraussetzung dafür, dass S3 (CSR-Loader) und S4
	// (eigener Kern) getrennt bewertbar sind — ohne sie misst jede Welle die
	// Summe der anderen mit.
	LoadMs    int
	ClusterMs int
	PersistMs int
	// LockHeldMs ist die Haltezeit des Advisory-Locks: die Größe, an der ein
	// Skalen-Pfad im BETRIEB scheitert, weil sie alle Rebuilds derselben
	// Partition serialisiert. Sie wird von der ERSTEN Anweisung der persist-Tx
	// bis zum Commit gemessen.
	LockHeldMs int
	// CopyMs trennt den CopyFrom-Anteil ab S9a heraus. Bis dahin 0/NULL — die
	// Spalte existiert, damit S9a nicht behaupten kann, lock_held_ms gesenkt zu
	// haben, indem es Arbeit in eine unbeobachtete Phase verschiebt (S9a-G2).
	CopyMs int
	// PeakRSSKb ist VmHWM des KINDprozesses. Momentanwerte taugen nicht: der
	// Rebuild ist eine Sägezahn-Last, und die cgroup schlägt an der Spitze zu.
	PeakRSSKb int64
	// PartitionHash ist der Determinismus-Ersatzanker A1 (§4.6): SHA-256 über
	// die kanonisierte Partition. Er ersetzt ab engine=ctx den 50-Lauf-Test,
	// der ohne PRNG trivial grün wäre.
	PartitionHash []byte
	// EdgeCount/DanglingN/SelfLoopN beschreiben den KANTEN-Schnitt. dangling und
	// Self-Loops werden heute STILL verworfen (cluster.go:462-466) — live sind
	// das 264 von 3.519 Kanten, also 7,5 %, die niemand sieht.
	EdgeCount int
	DanglingN int
	SelfLoopN int
	// ComponentN/LevelN/SweepN/SigmaDrift füllen S4/S8. gonum meldet weder
	// Sweeps noch Ebenen nach außen (localMovingHeuristic führt keinen Zähler),
	// deshalb bleiben sie auf dem gonum-Pfad 0 ⇒ NULL im Journal.
	ComponentN int
	LevelN     int
	SweepN     int
	SigmaDrift float64

	// S6+S7: Engine und effektiver Cap dieses Laufs. Beide gehoeren ins
	// Journal, weil sie ohne sie nicht rekonstruierbar sind — die Engine ist
	// hot-reloadbar, und welcher der beiden Cap-Keys galt, entscheidet die
	// Engine (UD-07-04).
	Engine      string
	MaxNodesEff int
}

// EngineGonum/EngineCtx benennen die beiden Rechenkerne. Der Schalter selbst
// kommt erst mit S6+S7; die Konstante existiert ab S2, weil das Lauf-Journal
// die Engine JEDER Zeile führt — ein Journal, das erst ab dem Schalter weiß,
// womit gerechnet wurde, könnte den Wechsel nicht belegen.
const (
	EngineGonum = "gonum"
	EngineCtx   = "ctx"
)

// tallyScopes counts node candidates per scope over the loadNodes output.
// O(candidates) and free next to the load itself; runs BEFORE the MaxNodes
// check so the skipped run can still report how far over the cap it was.
func tallyScopes(nodeScopes map[string]string) map[string]int {
	out := make(map[string]int, len(nodeScopes))
	for _, scope := range nodeScopes {
		out[scope]++
	}
	return out
}

// candidateArrays flattens a CandidateCount map into the two parallel arrays
// the meta SQL binds (unnest($n::text[], $m::int[])). Sorted for a stable
// bind order — the same reproducibility discipline as the edge insertion.
func candidateArrays(candidates map[string]int) ([]string, []int32) {
	scopes := make([]string, 0, len(candidates))
	for s := range candidates {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	ns := make([]int32, len(scopes))
	for i, s := range scopes {
		ns[i] = int32(candidates[s]) //nolint:gosec // candidate counts are corpus-bounded; int32 covers 2.1e9 blocks per scope
	}
	return scopes, ns
}

// Options bundles the rebuild inputs (B-W1). The per-tenant scope filter
// (B-W6) lands here so call sites change once.
type Options struct {
	// Resolution is the Louvain resolution parameter (<=0 → 1.0).
	Resolution float64
	// VisibleTypes is the resolved retrieval allowlist (T6); feeds the node cut
	// and the aggregation re-checks. Empty = wiring bug, fails loudly.
	VisibleTypes []string
	// OverviewTypes is the overview.include allowlist (T6); node cut only.
	// Empty = wiring bug, fails loudly.
	OverviewTypes []string
	// MaxNodes is the load-bearing liveness guard (B2-C1): Louvain is one
	// opaque gonum call that cannot be interrupted, and past ~200k nodes it
	// hits a convergence wall (bench 019ec56f: 400k=465s, 1M never converges).
	// A node set larger than this cap skips the rebuild fail-safe (logged,
	// Stats.SkipReason="node-cap"). 0 = uncapped.
	MaxNodes int
	// ScopeFilter is the per-tenant partition filter (B-W3, decision B-E3).
	// Non-nil: teardown AND aggregation are scoped to exactly these scopes,
	// ATOMICALLY — never one without the other (B1-C1: a scoped DELETE with an
	// unscoped re-aggregation resurrects foreign-partition rows and hits the
	// (cluster_id, scope) PK ⇒ 23505 from tenant #2 on). nil: global run,
	// behaviour-identical to the pre-B full replace (TRUNCATE + unscoped
	// aggregation). Since B-W6 the filter also cuts the INPUT (loadNodes/
	// loadEdges, both edge endpoints); the persist input-purity guard stays
	// as the fail-loud backstop against an out-of-filter input.
	ScopeFilter []string
	// TombstoneRetention is the window in which a RETIRED topic can still be
	// re-attached by a re-appearing community (W3, decision E2-01 / amendment
	// A01-2). It has to cross the worker boundary because the re-attach probe
	// runs INSIDE the persist tx, in the child process. 0 disables the path —
	// the run then mints fresh identities, which is the fail-closed direction.
	//
	// This is the ONLY option the axis adds, and therefore the axis' single
	// IPC protocol change: the retention PURGE (W8) deliberately runs in the
	// parent process and reads cfg directly, so it never crosses this boundary
	// a second time.
	TombstoneRetention time.Duration

	// ── W-F: the meta-cluster level (design/02 §4.7, root_map.super_*) ──────
	//
	// These four cross the worker boundary for the same reason TombstoneRetention
	// does: the resolution search runs in the compute window of the CHILD, right
	// next to the main Louvain and outside every transaction (K5). Both decoders
	// are strict, so they ship in one commit — and parent and child are the same
	// binary, so there is no mixed window except a running child across a binary
	// swap (BP-8: fails loudly, never silently).
	//
	// SuperEnabled off is the shipped state: the level is computed only when the
	// operator asks for it, and until then graph_overview_meta.super_n stays NULL
	// — "not attempted", which the map renders differently from "attempted and
	// capped".
	SuperEnabled bool
	// SuperTargetRows is the map's row capacity (rootmap.NodeLimitFor). The
	// search looks for the FINEST level that still fits it.
	SuperTargetRows int
	// SuperMinResolution is the lower γ bound of the search.
	SuperMinResolution float64
	// SuperMaxNodes caps the supergraph node count PER SCOPE. Exceeding it
	// degrades that scope to the flat map — it never skips the main rebuild:
	// the meta level is the quality layer, not the safety layer.
	SuperMaxNodes int

	// ── S3: das CSR-Substrat (Achse 04, design/04 §4.5) ────────────────────
	//
	// CSRLoader schaltet den zweistufigen REPEATABLE-READ-Cursor-Build an die
	// Stelle von loadNodes + loadEdges + Symmetrisierung. Default AUS: auf einem
	// live produktiven System, dessen Deploys aus Tag-Worktrees gebaut werden,
	// ist ein Binary-Rollback die teuerste Rueckabwicklungsform und ein
	// Config-Flip die billigste. Der alte []rawEdge-Pfad bleibt daneben stehen,
	// bis das Identitaets-Gate ueber mehrere Deploys grün war.
	//
	// PROTOKOLL: das ist das ZWEITE IPC-Fenster dieser Achse (nach S2s
	// Stats-Erweiterung) und damit eines mehr, als §3.5 vorgesehen hat. Der
	// Grund ist, dass der Rechenpfad im KIND liegt: ein Schalter, der ihn
	// steuert, muss ueber die Grenze. Die Alternative — das Kind liest die
	// Config selbst — scheitert daran, dass cmd/ctxd die Options dekodiert,
	// BEVOR es Config oder Pool baut (worker.go, "no mutation"-Gate). Beide
	// Seiten sind dasselbe Binary und werden vom Container-Deploy atomar
	// getauscht; das Mischversions-Fenster bleibt dokumentiert (BP-8) und
	// scheitert laut.
	CSRLoader bool

	// ── S6+S7: Engine-Umschalter + Zeitbudget (design/04 §4.9) ─────────────
	//
	// Diese drei Felder bilden ZUSAMMEN ein IPC-Fenster — deshalb liegen S6
	// und S7 in EINEM Commit statt in zweien (§3.5). Beide Decoder sind
	// strikt; zwei Wellen waeren zwei Neustartfenster fuer denselben Nutzen.
	//
	// Engine leer = gonum. Der Default ist bewusst gonum und bleibt es: die
	// Umschaltung ist ein EINMALIGER, globaler Partitionsbruch und gehoert
	// gebuendelt an einen angekuendigten Release-Schnitt mit erhoehtem
	// tombstone_retention (UD-03-04) — nicht in das Deploy dieser Welle.
	Engine string
	// TimeBudget ist der Primaer-Guard bei engine=ctx: der Mover prueft
	// zwischen Warteschlangen-Bloecken und bricht SAUBER ab. Auf dem
	// gonum-Pfad wirkungslos — Modularize kann ctx nicht beobachten, dort
	// bleibt der rebuild_timeout-SIGKILL der Guard. 0 = aus.
	TimeBudget time.Duration
	// MaxNodesCtx ist das Not-Aus des eigenen Kerns. Die ENGINE waehlt den
	// Key (UD-07-04), nicht der Wert: max_nodes gilt fuer gonum,
	// max_nodes_ctx fuer ctx.
	MaxNodesCtx int
	// Refine schaltet die Leiden-Refinement-Phase (S5). Nur bei engine=ctx
	// wirksam — gonum hat keine.
	Refine bool
}

// lockKeyForScopes returns the advisory lock key for one persist run (B-W4).
//
// Empty/nil filter (the global run) returns overviewLockKey BYTE-IDENTICALLY:
// during a mixed-version deploy or rollback window an old binary (which only
// knows the base key) and the new one keep serializing against each other.
//
// A partition run hashes its scope set with FNV-64a — seedless and therefore
// process-stable — and XORs it onto the base key. hash/maphash is NOT usable
// here (B1-M1): its seed is per-process, so two ctxd instances on the same DB
// would compute DIFFERENT keys for the same tenant and stop excluding each
// other. The input is sorted and deduplicated (a tenant's owned set is a SET —
// order or duplicates must never change the key), scopes joined with a 0x00
// separator (scope values are VARCHAR/TEXT and cannot contain NUL, so the
// framing is unambiguous).
//
// Collision note (B2-m2(i), deliberately unhandled): two distinct scope sets
// colliding in the 64-bit space merely serialize their rebuilds — the losing
// run skips and retries next tick; a delayed rebuild, never a correctness
// problem.
func lockKeyForScopes(scopeFilter []string) int64 {
	if len(scopeFilter) == 0 {
		return overviewLockKey
	}
	set := make([]string, 0, len(scopeFilter))
	seen := make(map[string]struct{}, len(scopeFilter))
	for _, s := range scopeFilter {
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			set = append(set, s)
		}
	}
	sort.Strings(set)
	h := fnv.New64a()
	for _, s := range set {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return overviewLockKey ^ int64(h.Sum64()) //nolint:gosec // deliberate wrap-around: the lock key is an opaque 64-bit identity, not a quantity
}

// rawEdge is a directed dream-link reduced to the fields the clustering needs.
type rawEdge struct {
	src, dst string
	weight   float64
}

// clustering is the pure (DB-free) output of the Louvain run.
type clustering struct {
	blockToCluster map[string]string // block uuid → cluster_id (= min member uuid)
	// intraDegree is the WITHIN-cluster weighted degree per block: the summed
	// link confidence of a block's edges that stay inside its own community.
	// It is the graph-native centrality the cluster core is picked by — the
	// successor of the degenerate quality_score representative choice, where
	// two thirds of the clusters are decided by the uuid tiebreak. Same shape
	// as blockToCluster so persist reads it without knowing node indices.
	intraDegree  map[string]float64
	modularity   float64
	clusterCount int

	// S2: die drei Kanten-Zähler des Journals. edgePairs ist die Zahl der
	// UNGERICHTETEN Paare nach Symmetrisierung und Deduplizierung — nicht die
	// geladene Zeilenmenge; dangling und selfLoops sind die beiden Mengen, die
	// der Symmetrisierungs-Schritt bisher STILL verwirft (:462-466). Live sind
	// das 264 dangling von 3.519 Kanten. S3 baut auf denselben Zählern sein
	// Gate S3-G4 gegen die psql-Gegenprobe auf.
	edgePairs int
	dangling  int
	selfLoops int

	// S6+S7: nur der eigene Kern kennt diese drei. gonum meldet weder Ebenen
	// noch Sweeps nach aussen (localMovingHeuristic fuehrt keinen Zaehler),
	// deshalb bleiben sie auf dem gonum-Pfad 0 ⇒ NULL im Journal.
	levels     int
	sweeps     int
	sigmaDrift float64
}

// Rebuild recomputes the cluster supergraph and replaces the 057 tables in one
// advisory-locked transaction. Never call from a request path — gonum loads the
// whole graph into RAM; this belongs in the scheduler (runOverviewRebuild).
//
// Type policy (WF T6, design/01 §6.8/§7-T6): visibleTypes is the resolved
// retrieval allowlist (blocktype.Set.VisibleTypes — the registry successor of
// the former `type_name <> 'system-meta'` literal); overviewTypes is the
// overview.include allowlist (Set.OverviewTypes — the digest.include pendant
// against issue flooding). The NODE set is cut by the INTERSECTION of both
// (a type must be retrieval-visible AND overview-included to become a Louvain
// node); the aggregation re-checks use visibleTypes alone (pure visibility
// semantics — members are already the cut set). Both lists come from the BASE
// snapshot: the per-tenant rebuild loop (B-W6) varies only the scope filter,
// never the type policy (§9.6 caveat, T12 boundary). Empty lists are a wiring
// bug and fail loudly (rrf.Search pattern).
//
// Input scoping (B-W6): a non-nil opts.ScopeFilter cuts the LOAD — loadNodes
// filters blocks, loadEdges keeps only edges with BOTH endpoints inside the
// filter (partition semantics; a cross-partition edge belongs to no
// partition). nil keeps both queries byte-identical to the pre-B global load.
//
// Liveness (B-W1): opts.MaxNodes is the load-bearing guard (fail-safe skip);
// ctx cancellation/timeout is the secondary guard — it aborts BETWEEN the
// load/compute/persist phases and via clusterWithCtx during compute (with the
// documented goroutine leak: a running Modularize cannot be interrupted).
func Rebuild(ctx context.Context, pool *pgxpool.Pool, opts Options) (Stats, error) {
	if opts.Resolution <= 0 {
		opts.Resolution = 1.0
	}
	if len(opts.VisibleTypes) == 0 || len(opts.OverviewTypes) == 0 {
		return Stats{}, fmt.Errorf("overview: empty type allowlist (visible=%d, overview=%d) — block-type registry not wired?",
			len(opts.VisibleTypes), len(opts.OverviewTypes))
	}
	// S6+S7: die Engine steht VOR allem anderen fest — sie waehlt den
	// Guard-Key, den Ladepfad und den Rechenkern. Ein unbekannter Wert ist ein
	// Fehler und kein stiller Fallback (SP-2).
	engine, err := NormalizeEngine(opts.Engine)
	if err != nil {
		return Stats{}, err
	}
	// engine=ctx IMPLIZIERT das CSR-Substrat: der eigene Kern konsumiert eine
	// CSR und kann mit []rawEdge nichts anfangen. Das ist keine stille
	// Umdeutung eines Nutzerwunsches, sondern eine Typ-Notwendigkeit — und sie
	// steht im Log, damit niemand csr_loader=false liest und glaubt, der alte
	// Ladepfad laufe noch.
	useCSR := opts.CSRLoader
	if engine == EngineCtx && !useCSR {
		useCSR = true
		slog.Info("overview: engine=ctx implies the CSR loader — the own kernel consumes a CSR, not an edge list")
	}
	loadStart := time.Now()
	nodeTypes := intersect(opts.VisibleTypes, opts.OverviewTypes)
	// S3: der CSR-Pfad laedt Knoten UND Kanten in EINER Transaktion, weil der
	// Zwei-Pass-Build beide Passen auf demselben Snapshot braucht. Deshalb faellt
	// hier die Entscheidung, nicht erst an der Kantenladung.
	var csr *csrGraph
	var nodeUUIDs []string
	var nodeScopes map[string]string
	if useCSR {
		nodeUUIDs, nodeScopes, csr, err = loadCSR(ctx, pool, nodeTypes, opts.ScopeFilter)
	} else {
		nodeUUIDs, nodeScopes, err = loadNodes(ctx, pool, nodeTypes, opts.ScopeFilter)
	}
	if err != nil {
		return Stats{}, fmt.Errorf("loading nodes: %w", err)
	}
	// W-A: tally BEFORE the cap check — the skipped run is exactly the case
	// where the map matters (how far over max_nodes is this partition?).
	candidates := tallyScopes(nodeScopes)
	// EMPTY CUT WITH LIVING IDENTITIES (review K2-3). loadNodes returning zero
	// rows is reachable purely through configuration: the node set is
	// visible ∩ overview.include, and an empty intersection compiles to
	// `type_name = ANY('{}')` — a deterministic FALSE (visibility.go). Without
	// this guard persist would run on an empty input and the identity phase's
	// death statement would retire EVERY topic of the partition, starting the
	// retention clock on all of them; one mistyped allowlist would erase the
	// corpus' identity, and the map would be empty rather than stale.
	//
	// The guard is conditional on purpose. A partition that has no identities
	// yet (fresh deploy, or genuinely never built) must still persist — that
	// writes the honest empty map and its meta row. Only an empty cut IN FRONT
	// OF living topics is the anomaly, and it is treated exactly like the node
	// cap: return before persist, keep the last good map, stamp the reason.
	//
	// Named trade-off: a partition whose last block is legitimately archived
	// now FREEZES instead of emptying itself, and its topics have to be retired
	// by an explicit action. That is the deliberate direction — a visible
	// freeze (skip_reason in graph_overview_meta) is recoverable, a silently
	// erased identity layer is not.
	if len(nodeUUIDs) == 0 {
		live, err := partitionHasLiveTopics(ctx, pool, opts.ScopeFilter)
		if err != nil {
			return Stats{}, err
		}
		if live {
			slog.Warn("overview: rebuild skipped — the node cut is empty while the partition still has living topics (check overview.include ∩ visible types)",
				"scope_filter", opts.ScopeFilter)
			return Stats{Skipped: true, SkipReason: "empty-node-cut", CandidateCount: candidates}, nil
		}
	}

	// A FROZEN MAP KEEPS ITS IDENTITIES (W3-G12, amendment A01-5). The skip
	// returns BEFORE persist, so no topic is touched: no last_seen_at moves,
	// nothing retires, graph_cluster_node stays as it was and the route keeps
	// serving the last good map. That is load-bearing — a freeze that retired
	// topics would turn a liveness guard into an identity shredder, and the
	// label pipeline (W6) selects on graph_cluster_node, so a frozen map
	// produces no new labels either.
	//
	// TODO(Achse 04, S6+S7): after the engine switch there are THREE freeze
	// reasons, not one — "node-cap" here, "time-budget" (S6/S7) and
	// "mem-budget" (S7b, the child memory ceiling). Each of them must return
	// on this same path, i.e. before persist opens a transaction; W3-G12 is
	// then extended to all three instead of being duplicated per reason.
	// UD-07-04: die ENGINE waehlt den Key. Bei engine=ctx ist der Cap nur noch
	// das Not-Aus gegen eine absurde Eingabe (Default 5M) — der tragende Guard
	// ist dann das Zeitbudget. Er bleibt trotzdem stehen: ein Guard ohne Zahl
	// ist kein Guard, und dieser zieht VOR dem Kantenladen und kostet nichts.
	effMaxNodes := EffectiveMaxNodes(engine, opts.MaxNodes, opts.MaxNodesCtx)
	if effMaxNodes > 0 && len(nodeUUIDs) > effMaxNodes {
		slog.Warn("overview: rebuild skipped — node count exceeds cap",
			"nodes", len(nodeUUIDs), "max_nodes_eff", effMaxNodes, "engine", engine)
		return Stats{Skipped: true, SkipReason: "node-cap", CandidateCount: candidates, MaxNodesEff: effMaxNodes}, nil
	}
	var edges []rawEdge
	if !useCSR {
		edges, err = loadEdges(ctx, pool, opts.ScopeFilter)
		if err != nil {
			return Stats{}, fmt.Errorf("loading edges: %w", err)
		}
	}
	// S2: die Ladephase endet HIER, nicht nach loadNodes. Beide Loads gehören
	// in load_ms — sie sind derselbe Kostenpfad (zwei sortierte Cursor über
	// dieselbe Transaktion), und S3 wird genau diese Zahl gegen den CSR-Build
	// stellen.
	loadMS := int(time.Since(loadStart).Milliseconds())

	clusterStart := time.Now()
	var cl clustering
	switch {
	case engine == EngineCtx:
		cl, err = clusterCtxEngine(ctx, nodeUUIDs, csr, opts.Resolution, opts.TimeBudget, opts.Refine)
		// ZEITBUDGET-ABBRUCH: das Ergebnis wird VOLLSTAENDIG verworfen, die
		// Karte friert ein — exakt wie am Knoten-Cap (SP-5). Gemeldet wird der
		// BESTEHENDE Grund 'timeout' und kein neuer: graph_overview_meta traegt
		// dafuer seit Mig 123 einen CHECK, und ein neuer Wert waere eine
		// Migration fuer eine Unterscheidung, die das Lauf-Journal ohnehin
		// praeziser fuehrt (skip_reason dort ohne CHECK).
		//
		// Unterschieden wird am EIGENEN Budget, nicht am Kontext des Aufrufers:
		// laeuft der aeussere ctx ab, ist das der rebuild_timeout und gehoert
		// dem Elternprozess.
		if err != nil && ctx.Err() == nil && opts.TimeBudget > 0 {
			slog.Warn("overview: rebuild skipped — the compute time budget was exhausted",
				"time_budget", opts.TimeBudget, "nodes", len(nodeUUIDs))
			return Stats{Skipped: true, SkipReason: "timeout", CandidateCount: candidates, MaxNodesEff: effMaxNodes}, nil
		}
	case useCSR:
		cl, err = clusterCSRWithCtx(ctx, nodeUUIDs, csr, opts.Resolution)
	default:
		cl, err = clusterWithCtx(ctx, nodeUUIDs, edges, opts.Resolution)
	}
	if err != nil {
		return Stats{}, err
	}
	// W-F: the meta level is computed HERE — in the compute window, before
	// persist opens its transaction (K5, design/02 §9.1 "Was NICHT in die Tx
	// gehört"). Eight Louvain probes under the advisory lock and on top of the
	// teardown locks of three tables would multiply the lock hold time, and a
	// rebuild_timeout landing in that window would discard the whole rebuild
	// instead of just the meta level. Into the transaction goes an INSERT.
	super := computeSuperLevel(ctx, cl, nodeScopes, edges, superParams{
		Enabled:       opts.SuperEnabled,
		TargetRows:    opts.SuperTargetRows,
		MinResolution: opts.SuperMinResolution,
		MaxNodes:      opts.SuperMaxNodes,
		Resolution:    opts.Resolution,
	})
	// Die Meta-Ebene liegt bewusst INNERHALB von cluster_ms: sie ist bis zu
	// acht weitere Louvain-Proben (super.go) und damit Rechenzeit derselben
	// Phase. Sie hier auszuklammern hieße, cluster_ms kleinzurechnen.
	clusterMS := int(time.Since(clusterStart).Milliseconds())

	stats, err := persist(ctx, pool, cl, opts, nodeScopes, candidates, super)
	if err != nil {
		return stats, err
	}
	stats.LoadMs = loadMS
	stats.ClusterMs = clusterMS
	stats.EdgeCount = cl.edgePairs
	stats.DanglingN = cl.dangling
	stats.SelfLoopN = cl.selfLoops
	stats.PartitionHash = partitionHash(cl.blockToCluster)
	stats.Engine = engine
	stats.MaxNodesEff = effMaxNodes
	stats.LevelN = cl.levels
	stats.SweepN = cl.sweeps
	stats.SigmaDrift = cl.sigmaDrift
	// PeakRSSKb wird als LETZTES gelesen — VmHWM ist ein Hochwassermarker, der
	// erst am Ende die Spitze des ganzen Laufs kennt (Laden, Symmetrisieren,
	// gonum-Graph, persist). Genau diese Reihenfolge war der Messfehler, den
	// der S1-Bench im ersten Anlauf hatte.
	stats.PeakRSSKb = ReadVmHWMkB()
	return stats, nil
}

// clusterWithCtx runs computeClustering in its own goroutine so a ctx
// timeout/cancel lets the CALLER move on. Modularize is one opaque gonum call
// and cannot observe ctx — on abort the goroutine keeps computing until
// Louvain converges and is then discarded (documented leak; bounded by
// Options.MaxNodes, which keeps the input below the convergence wall). The
// buffered channel lets the orphaned goroutine exit instead of blocking
// forever.
//
// Since wave E-B this leak lives ONLY in the in-process FALLBACK path (no
// worker argv wired, or the child not startable — events.executeOverviewRebuild):
// the REGULAR path runs this code inside the worker child process, where the
// parent's rebuild_timeout is a SIGKILL and the leak dies with the process.
// Deliberately NOT rebuilt away here — the process boundary already retires
// it structurally on the path that matters (design/05 §4.7).
func clusterWithCtx(ctx context.Context, nodeUUIDs []string, edges []rawEdge, resolution float64) (clustering, error) {
	done := make(chan clustering, 1)
	go func() {
		done <- computeClustering(nodeUUIDs, edges, resolution)
	}()
	select {
	case <-ctx.Done():
		return clustering{}, fmt.Errorf("clustering aborted (goroutine keeps running until convergence): %w", ctx.Err())
	case cl := <-done:
		return cl, nil
	}
}

// intersect returns the sorted intersection of two string slices. Used for
// the node-set cut (visible ∩ overview.include); n is registry-sized (tens),
// never corpus-sized.
func intersect(a, b []string) []string {
	in := make(map[string]struct{}, len(b))
	for _, s := range b {
		in[s] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if _, ok := in[s]; ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// computeClustering is the pure core: build an undirected weighted graph from
// the visible node set + symmetrized links, run Louvain with the fixed seed,
// derive a content-stable cluster_id (min member uuid). DB-free → unit-testable.
func computeClustering(nodeUUIDs []string, edges []rawEdge, resolution float64) clustering {
	n := len(nodeUUIDs)
	if n == 0 {
		return clustering{blockToCluster: map[string]string{}, intraDegree: map[string]float64{}}
	}

	idx := make(map[string]int64, n)
	for i, u := range nodeUUIDs {
		idx[u] = int64(i)
	}

	// Symmetrize: aggregate directed links into undirected edge weights. Skip
	// dangling endpoints (link to an archived/meta block not in the node set)
	// and self-loops (gonum simple graph forbids them).
	type pair struct{ a, b int64 }
	agg := make(map[pair]float64, len(edges))
	// S2: die beiden Verwerfungs-Gründe werden GETRENNT gezählt statt gemeinsam
	// verschwiegen. Getrennt, weil sie verschiedene Befunde sind: dangling heißt
	// "der Knotenschnitt ist enger als der Kantensatz" (Archivierung, Typ-Politik,
	// Scope-Filter), selfLoop heißt "ein Block verweist auf sich selbst". Ein
	// gemeinsamer Zähler könnte einen Anstieg des einen nicht vom anderen trennen.
	var dangling, selfLoops int
	for _, e := range edges {
		ai, okA := idx[e.src]
		bi, okB := idx[e.dst]
		if !okA || !okB {
			dangling++
			continue
		}
		if ai == bi {
			selfLoops++
			continue
		}
		if ai > bi {
			ai, bi = bi, ai
		}
		agg[pair{ai, bi}] += e.weight
	}

	g := simple.NewWeightedUndirectedGraph(0, 0)
	for i := range nodeUUIDs {
		g.AddNode(simple.Node(int64(i))) // isolated nodes too → singleton clusters
	}
	// Deterministic edge insertion order (sorted keys) — map iteration is random
	// in Go; the sort makes the build reproducible independent of map layout.
	keys := make([]pair, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].a != keys[j].a {
			return keys[i].a < keys[j].a
		}
		return keys[i].b < keys[j].b
	})
	for _, k := range keys {
		w := agg[k]
		if w <= 0 {
			w = 1e-9 // Modularize panics on negative weight; 0 is meaningless
		}
		g.SetWeightedEdge(simple.WeightedEdge{F: simple.Node(k.a), T: simple.Node(k.b), W: w})
	}

	reduced := community.Modularize(g, resolution, rand.NewPCG(louvainSeed1, louvainSeed2))
	comms := reduced.Communities()

	b2c := make(map[string]string, n)
	for _, members := range comms {
		var minUUID string
		for _, node := range members {
			if u := nodeUUIDs[node.ID()]; minUUID == "" || u < minUUID {
				minUUID = u
			}
		}
		for _, node := range members {
			b2c[nodeUUIDs[node.ID()]] = minUUID
		}
	}

	// Intra-cluster weighted degree — one pass over the already symmetrized,
	// dangling- and self-loop-free edge map, so no extra SQL and no extra
	// materialization. Edges CROSSING community boundaries are skipped: the
	// core must describe what holds this cluster together, not what pulls at
	// it. Every node gets an entry, isolated ones at 0 — the core selection
	// reads this map by block id and must not have to tell a missing key from
	// a zero. Cost: O(|agg|) time, 8 B × n memory (1.6 MB at MaxNodes=200000).
	//
	// The pass walks the SORTED keys, not the map: float addition is not
	// associative, so summing in Go's randomized map order would drift the
	// last ULP between runs — the same effect the Q score is documented to
	// have. Here it would be load-bearing rather than cosmetic: the core is
	// picked by degree order and hashed, so a wobbling last bit could reorder
	// two neighbours, change core_hash and trigger a re-label of a topic that
	// never moved.
	deg := make(map[string]float64, n)
	for _, u := range nodeUUIDs {
		deg[u] = 0
	}
	for _, k := range keys {
		ua, ub := nodeUUIDs[k.a], nodeUUIDs[k.b]
		if b2c[ua] != b2c[ub] {
			continue
		}
		deg[ua] += agg[k]
		deg[ub] += agg[k]
	}

	q := community.Q(g, comms, resolution)
	if math.IsNaN(q) || math.IsInf(q, 0) {
		q = 0 // 0-edge graph → undefined Q; report 0 rather than NaN
	}
	return clustering{
		blockToCluster: b2c, intraDegree: deg, modularity: q, clusterCount: len(comms),
		edgePairs: len(agg), dangling: dangling, selfLoops: selfLoops,
	}
}

// partitionHash ist der Determinismus-Ersatzanker A1 (design/04 §4.6):
// SHA-256 über die KANONISIERTE Partition — Member nach block_id sortiert, je
// Zeile block_id || cluster_id.
//
// Kanonisiert heißt: die Map-Iterationsordnung darf nicht eingehen. Ohne die
// Sortierung wäre der Hash bei jedem Lauf ein anderer und der Anker wertlos —
// derselbe Grund, aus dem die Kanteneinfügung sortiert läuft (:275-291).
//
// Warum der Hash den 50-Lauf-Determinismus-Test ERGÄNZT und nicht ersetzt: der
// Test prüft WIEDERHOLBARKEIT innerhalb eines Binaries, der Hash prüft KONSTANZ
// über die Zeit. Ab engine=ctx entfällt der PRNG, der 50-Lauf-Test wird damit
// trivial grün — und der Hash ist dann der einzige verbliebene Anker.
func partitionHash(b2c map[string]string) []byte {
	blocks := make([]string, 0, len(b2c))
	for b := range b2c {
		blocks = append(blocks, b)
	}
	sort.Strings(blocks)
	// Die 0x00-Rahmung ist NICHT kosmetisch. Ohne sie ist der Hash nicht
	// injektiv: ("ab" → "c") und ("a" → "bc") ergaeben denselben Digest, weil
	// nur die Konkatenation eingeht. Bei UUIDs fester Breite kann der Fall heute
	// nicht auftreten — aber ein Anker, dessen Korrektheit an einer Annahme
	// ueber die Eingabebreite haengt, ist ein Anker auf Widerruf, und der naechste
	// Schluesseltyp (Topic-Handle, K13) haette ihn still gebrochen.
	// UUID-Textform enthaelt kein NUL, die Rahmung ist damit eindeutig — dasselbe
	// Argument, auf dem lockKeyForScopes seine Trennung baut (:230-236).
	//
	// Gefunden von der eigenen Praefix-Kollisionsprobe in S2-G4, nicht im Review.
	h := sha256.New()
	var sep = [1]byte{0}
	for _, b := range blocks {
		_, _ = h.Write([]byte(b))
		_, _ = h.Write(sep[:])
		_, _ = h.Write([]byte(b2c[b]))
		_, _ = h.Write(sep[:])
	}
	return h.Sum(nil)
}

func loadNodes(ctx context.Context, pool *pgxpool.Pool, nodeTypes []string, scopeFilter []string) ([]string, map[string]string, error) {
	// ORDER BY id = determinism axis 2: the int64 surrogate id is the load
	// position, so a stable order yields a stable partition under a fixed seed.
	// nodeTypes = visible ∩ overview.include (T6): the shared type-visibility
	// fragment plus the overview sieve, folded into ONE bind parameter.
	// The scope rides along (B-W2): persist denormalizes it onto the member
	// row, so the written partition is the one the clustering RAN in — never
	// a re-read that a concurrent scope-move could skew.
	// scopeFilter (B-W6): non-nil cuts the node input to the tenant partition
	// (owned scopes); nil is the byte-identical pre-B global load.
	query := fmt.Sprintf(`SELECT cb.id::text, cb.scope FROM context_blocks cb
		 WHERE %s
		 ORDER BY cb.id`, visibility.TypeVisible("cb", "$1"))
	args := []any{nodeTypes}
	if len(scopeFilter) > 0 {
		query = fmt.Sprintf(`SELECT cb.id::text, cb.scope FROM context_blocks cb
		 WHERE %s
		   AND cb.scope = ANY($2)
		 ORDER BY cb.id`, visibility.TypeVisible("cb", "$1"))
		args = append(args, scopeFilter)
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []string
	scopes := make(map[string]string)
	for rows.Next() {
		var s, scope string
		if err := rows.Scan(&s, &scope); err != nil {
			return nil, nil, err
		}
		out = append(out, s)
		scopes[s] = scope
	}
	return out, scopes, rows.Err()
}

func loadEdges(ctx context.Context, pool *pgxpool.Pool, scopeFilter []string) ([]rawEdge, error) {
	// supersedes is a replacement relation, not a topical bond — excluded from
	// clustering (matches the ego traversal, which never walks supersedes).
	//
	// scopeFilter (B-W6, review B2-M4): the scoped load restricts BOTH
	// endpoints via context_blocks joins — NOT via graph_cluster_member (the
	// member table is the PREVIOUS run's state, semantically wrong as an input
	// filter). Without this cut every tenant rebuild would pull the full edge
	// set (~3.24M rows at the live corpus target) into RAM N times per cycle —
	// no isolation leak (computeClustering skips dangling endpoints), but
	// exactly the N-fold scan/RAM load the per-tenant line exists to avoid.
	// nil keeps the query byte-identical to the pre-B global load.
	query := `SELECT source_block_id::text, target_block_id::text, raw_confidence
		 FROM context_dream_links
		 WHERE relationship <> 'supersedes'
		 ORDER BY source_block_id, target_block_id`
	var args []any
	if len(scopeFilter) > 0 {
		query = `SELECT l.source_block_id::text, l.target_block_id::text, l.raw_confidence
		 FROM context_dream_links l
		 JOIN context_blocks bs ON bs.id = l.source_block_id AND bs.scope = ANY($1)
		 JOIN context_blocks bt ON bt.id = l.target_block_id AND bt.scope = ANY($1)
		 WHERE l.relationship <> 'supersedes'
		 ORDER BY l.source_block_id, l.target_block_id`
		args = append(args, scopeFilter)
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rawEdge
	for rows.Next() {
		var e rawEdge
		if err := rows.Scan(&e.src, &e.dst, &e.weight); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// nodeAggSQL builds graph_cluster_node: two-level aggregation — inner per
// (cluster, scope, category) counts + best representative, outer rolls up to
// size + category_counts(jsonb) + the representative of the highest-quality
// category. All visibility filtering is the canonical type-visibility
// fragment (visibility.TypeVisible, $1 = registry allowlist — T6); scope
// partitioning is the GROUP BY, so each row belongs to exactly one scope
// (design §3.3).
// nodeAggTemplate has two slots: the type-visibility fragment ($1) and the
// B-W3 scope filter ("" = global run, or a WHERE on the DENORMALIZED
// m.scope ($2) — the member column exists exactly so the scoped aggregation
// never joins context_blocks for partition membership, 087 header).
//
// W3 wraps the two historical levels in a THIRD one. The reason is mechanical:
// the identity and the core have to be attached to the FINISHED (cluster,
// scope) rollup, and neither category_counts nor the representative exist as
// values inside the level that produces them — they cannot be referenced from
// the same projection list that builds them.
//
// Both W3 joins are INNER, and that is the fail-loud choice. A cluster without
// an ov_match row is an assignment bug, one without an ov_core row is a core
// bug; either shows up here as a MISSING node row, which persist's row-count
// check turns into a loud rollback. The alternative — a LEFT JOIN writing
// topic_id IS NULL — would be silently swallowed by the read path, and a node
// row without identity is exactly the state this axis exists to abolish.
const nodeAggTemplate = `
INSERT INTO graph_cluster_node (cluster_id, scope, size, category_counts, repr_block_id, repr_title,
                                repr_quality, topic_id, core_hash, core_blocks)
SELECT r.cluster_id, r.scope, r.size, r.category_counts, r.repr_block_id, r.repr_title,
       r.repr_quality, mt.topic_id, oc.core_hash, oc.core_blocks
FROM (
    SELECT cluster_id, scope,
           sum(cat_cnt)::int                                           AS size,
           jsonb_object_agg(category, cat_cnt)                         AS category_counts,
           (array_agg(repr_id    ORDER BY cat_max_q DESC, repr_id))[1] AS repr_block_id,
           (array_agg(repr_title ORDER BY cat_max_q DESC, repr_id))[1] AS repr_title,
           max(cat_max_q)                                              AS repr_quality
    FROM (
        SELECT m.cluster_id, b.scope, b.category,
               count(*)::int        AS cat_cnt,
               max(b.quality_score) AS cat_max_q,
               (array_agg(b.id              ORDER BY b.quality_score DESC, b.id))[1] AS repr_id,
               (array_agg(left(b.title,120) ORDER BY b.quality_score DESC, b.id))[1] AS repr_title
        FROM graph_cluster_member m
        JOIN context_blocks b ON b.id = m.block_id
           AND %s
        %s
        GROUP BY m.cluster_id, b.scope, b.category
    ) per_cat
    GROUP BY cluster_id, scope
) r
JOIN ov_match mt ON mt.cluster_id = r.cluster_id AND mt.scope = r.scope
JOIN ov_core  oc ON oc.cluster_id = r.cluster_id AND oc.scope = r.scope`

var nodeAggSQL = fmt.Sprintf(nodeAggTemplate, visibility.TypeVisible("b", "$1"), "")

// nodeAggScopedSQL is the B-W3 partition-scoped variant ($2 = scope filter).
// It MUST only ever run after the equally scoped teardown DELETE (persist
// keeps the two in one branch — B1-C1).
var nodeAggScopedSQL = fmt.Sprintf(nodeAggTemplate, visibility.TypeVisible("b", "$1"),
	"WHERE m.scope = ANY($2)")

// edgeAggSQL builds graph_cluster_edge: inter-cluster links aggregated per
// (cluster-pair, scope-pair). cluster_a < cluster_b normalizes the undirected
// meta-edge; scope_s/scope_t are the source/target BLOCK scopes (a meta-edge is
// visible iff both are in readScopes — design §2). raw_confidence (CHECK >= 0)
// is the weight, never the unconstrained confidence column. Both endpoint
// re-checks use the shared type-visibility fragment ($1 = registry allowlist).
// edgeAggTemplate: slots = two type-visibility fragments ($1) and the B-W3
// scope filter. The scoped variant restricts BOTH endpoint member scopes
// (AND, not OR): a partition's meta-edges are exactly those the tenant's
// owned-disjoint Louvain input can produce (both endpoints owned — matches
// the B-W6 loadEdges contract and the read path's "both scopes readable"
// rule, 057 header). Mixed cross-partition rows left over from a pre-B
// GLOBAL run are swept by the OR edge teardown (B-W6, see teardown) and —
// with this AND — never re-created.
// K1-2: the scope pair is swapped TOGETHER with the cluster pair.
//
// cluster_a/cluster_b are normalised by LEAST/GREATEST (the 057 CHECK demands
// cluster_a < cluster_b), but scope_s/scope_t used to stay bound to the LINK's
// source and target. Whenever the source block's cluster carried the larger
// uuid, the row therefore paired cluster_a with the scope of the OTHER endpoint.
//
// That was invisible for as long as nothing read the two together — the scope
// filters of the read path, the teardown and the meta count are all symmetric.
// Since W7 the identity read path resolves an endpoint as (cluster, scope),
// because a topic is a (cluster, scope) partition; a swapped pair then either
// resolves to no node at all (the edge silently disappears) or, in a
// scope-crossing cluster, to the WRONG half. The whole reason the identity path
// keeps the scope pair is to make that lookup exact, so the pair has to be
// oriented like the cluster pair it belongs to.
//
// `<=` rather than `<` only for symmetry with LEAST; the equal case is excluded
// by the WHERE (ms.cluster_id <> mt.cluster_id) anyway.
const edgeAggTemplate = `
INSERT INTO graph_cluster_edge (cluster_a, cluster_b, scope_s, scope_t, link_count, weight_sum)
SELECT LEAST(ms.cluster_id, mt.cluster_id),
       GREATEST(ms.cluster_id, mt.cluster_id),
       CASE WHEN ms.cluster_id <= mt.cluster_id THEN bs.scope ELSE bt.scope END,
       CASE WHEN ms.cluster_id <= mt.cluster_id THEN bt.scope ELSE bs.scope END,
       count(*)::int, sum(l.raw_confidence)::real
FROM context_dream_links l
JOIN graph_cluster_member ms ON ms.block_id = l.source_block_id
JOIN graph_cluster_member mt ON mt.block_id = l.target_block_id
JOIN context_blocks bs ON bs.id = l.source_block_id AND %s
JOIN context_blocks bt ON bt.id = l.target_block_id AND %s
WHERE l.relationship <> 'supersedes' AND ms.cluster_id <> mt.cluster_id%s
GROUP BY 1, 2, 3, 4`

var edgeAggSQL = fmt.Sprintf(edgeAggTemplate,
	visibility.TypeVisible("bs", "$1"), visibility.TypeVisible("bt", "$1"), "")

// edgeAggScopedSQL is the B-W3 partition-scoped variant ($2 = scope filter,
// both endpoints). Only ever runs after the scoped teardown (B1-C1).
var edgeAggScopedSQL = fmt.Sprintf(edgeAggTemplate,
	visibility.TypeVisible("bs", "$1"), visibility.TypeVisible("bt", "$1"),
	"\n  AND ms.scope = ANY($2) AND mt.scope = ANY($2)")

// Meta writes (B-W5, migration 088): one row per scope, replaced in the SAME
// advisory-locked tx as the tables it describes (no sentinel scope, no
// computed_at:null window — B3-M1). The global run derives its scope set from
// the freshly aggregated graph_cluster_node (DISTINCT scope); the scoped run
// writes exactly its filter scopes via unnest + LEFT JOIN, so a partition
// that became empty still records a fresh computed_at (its rebuild DID run).
// Stats are per-scope: cluster_n/node_n from the node rows, edge_n counts
// INTRA-partition edge rows (scope_s = scope_t — a cross-scope row belongs to
// no single partition, see 088 header); modularity/resolution are the run's.
//
// W-A (migration 123): the SUCCESS row also carries the attempt stamp —
// last_attempt_at = now() (same tx timestamp as computed_at, so gate 9 reads
// them equal) and skip_reason = NULL, which is how a successful run CLEARS a
// previously recorded cap. candidate_n rides along as a per-scope value from
// the parallel arrays ($n::text[], $m::int[]), never as one run total.
const metaWriteGlobalSQL = `
INSERT INTO graph_overview_meta (scope, computed_at, modularity, cluster_n, node_n, edge_n, resolution,
                                 last_attempt_at, skip_reason, candidate_n)
SELECT n.scope, now(), $1, count(*)::int, COALESCE(sum(n.size), 0)::int,
       (SELECT count(*) FROM graph_cluster_edge e
         WHERE e.scope_s = n.scope AND e.scope_t = n.scope)::int,
       $2,
       now(), NULL,
       COALESCE((SELECT c.cn FROM unnest($3::text[], $4::int[]) AS c(cscope, cn) WHERE c.cscope = n.scope), 0)
  FROM graph_cluster_node n
 GROUP BY n.scope`

const metaWriteScopedSQL = `
INSERT INTO graph_overview_meta (scope, computed_at, modularity, cluster_n, node_n, edge_n, resolution,
                                 last_attempt_at, skip_reason, candidate_n)
SELECT s.scope, now(), $1, count(n.cluster_id)::int, COALESCE(sum(n.size), 0)::int,
       (SELECT count(*) FROM graph_cluster_edge e
         WHERE e.scope_s = s.scope AND e.scope_t = s.scope)::int,
       $2,
       now(), NULL,
       COALESCE((SELECT c.cn FROM unnest($4::text[], $5::int[]) AS c(cscope, cn) WHERE c.cscope = s.scope), 0)
  FROM unnest($3::text[]) AS s(scope)
  LEFT JOIN graph_cluster_node n ON n.scope = s.scope
 GROUP BY s.scope`

// stampAttemptScopedSQL records ONE rebuild attempt per named scope — the
// four exits of rebuildOverviewOnce that write nothing today (disabled,
// registry-unwired, error/timeout, skip). Success goes through the metaWrite
// SQL above instead.
//
// Three details, each load-bearing (design/02 §3.1 point 2):
//
//   - computed_at is named EXPLICITLY as NULL. Without it the 057 DEFAULT
//     now() would make a never-built partition claim freshness — the reason
//     migration 123 drops the default. In the DO UPDATE branch the column is
//     NOT touched: a skip does not invalidate the old partition, it makes it
//     old. modularity/cluster_n/node_n/edge_n/resolution likewise.
//   - candidate_n is carried forward DEFENSIVELY (COALESCE(NULLIF(…,0), old)):
//     the advisory-lock skip is born inside persist with no candidate map, and
//     an unconditional SET would overwrite a correct value with 0 — while
//     candidate_n is the denominator of the coverage figure.
//   - The WHERE clause settles the CONTENTION race: advisory-lock means
//     ANOTHER instance is building this partition successfully right now
//     (cluster.go "keeps serializing"). The winner commits computed_at =
//     now(), skip_reason = NULL; an unconditional stamp by the loser would
//     mark a seconds-old partition as frozen and the map would print a cap
//     that does not exist. computed_at < attempt start discards the loser.
const stampAttemptScopedSQL = `
INSERT INTO graph_overview_meta (scope, computed_at, last_attempt_at, skip_reason, candidate_n)
SELECT s.scope, NULL, now(), $2, COALESCE(c.n, 0)
  FROM unnest($1::text[]) AS s(scope)
  LEFT JOIN unnest($3::text[], $4::int[]) AS c(scope, n) ON c.scope = s.scope
ON CONFLICT (scope) DO UPDATE
   SET last_attempt_at = EXCLUDED.last_attempt_at,
       skip_reason     = EXCLUDED.skip_reason,
       candidate_n     = COALESCE(NULLIF(EXCLUDED.candidate_n, 0), graph_overview_meta.candidate_n)
 WHERE graph_overview_meta.computed_at IS NULL
    OR graph_overview_meta.computed_at < $5`

// stampAttemptGlobalSQL is the nil-ScopeFilter variant: a global run has no
// known scope list, so the attempt stamps every EXISTING row and creates
// none — the same computed_at condition guards the contention race.
const stampAttemptGlobalSQL = `
UPDATE graph_overview_meta m
   SET last_attempt_at = now(),
       skip_reason     = $1,
       candidate_n     = COALESCE(NULLIF((SELECT c.n FROM unnest($2::text[], $3::int[]) AS c(scope, n)
                                           WHERE c.scope = m.scope), 0), m.candidate_n)
 WHERE m.computed_at IS NULL
    OR m.computed_at < $4`

// StampAttempt records one FAILED, SKIPPED or DISABLED rebuild attempt so no
// exit of rebuildOverviewOnce stays invisible in the database (W-A). scopes
// is the run's partition filter — empty/nil is the global run (stamps the
// existing rows, creates none). attemptStart is this attempt's start time and
// discards a stamp that a concurrent, SUCCESSFUL run has already overtaken.
//
// Deliberately exported and pool-based (not tx-bound): the caller is the
// scheduler, and on the timeout path it must stamp with the OUTER context —
// the rebuild's own context is expired exactly then.
func StampAttempt(ctx context.Context, pool *pgxpool.Pool, scopes []string, reason string, candidates map[string]int, attemptStart time.Time) error {
	candScopes, candNs := candidateArrays(candidates)
	if len(scopes) == 0 {
		if _, err := pool.Exec(ctx, stampAttemptGlobalSQL, reason, candScopes, candNs, attemptStart); err != nil {
			return fmt.Errorf("overview: stamping global rebuild attempt (%s): %w", reason, err)
		}
		return nil
	}
	if _, err := pool.Exec(ctx, stampAttemptScopedSQL, scopes, reason, candScopes, candNs, attemptStart); err != nil {
		return fmt.Errorf("overview: stamping rebuild attempt (%s) for %v: %w", reason, scopes, err)
	}
	return nil
}

// teardown clears the 057 tables for one persist run: global = TRUNCATE (the
// pre-B path), scoped = per-partition DELETEs (B-W3) — the atomic partner of
// the equally scoped aggregation in persist; never ships without it (B1-C1).
// Members and nodes belong to exactly one scope; edges use AND on both
// endpoint scopes (partition semantics, see edgeAggTemplate).
func teardown(ctx context.Context, tx pgx.Tx, scoped bool, scopeFilter []string) error {
	if !scoped {
		// W-F joins the TRUNCATE list rather than getting its own statement:
		// one statement is one lock acquisition over all five tables, and the
		// derived tables (topic edges, meta level) must never survive the
		// partition they describe — a super row pointing at a topic whose node
		// row was just removed is exactly the dangling state the atomic pairing
		// exists to prevent.
		if _, err := tx.Exec(ctx, `TRUNCATE graph_cluster_member, graph_cluster_node, graph_cluster_edge,
			graph_cluster_topic_edge, graph_cluster_super, graph_cluster_super_member`); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
		return nil
	}
	// Edge teardown is OR on purpose (B-W6 transition lifecycle): a mixed
	// cross-partition row — possible only from a pre-B GLOBAL run, since the
	// scoped aggregation emits AND-rows and owned-disjointness means no OTHER
	// tenant can ever aggregate an edge touching one of THESE scopes — is
	// swept by the first partition run that owns either endpoint and is never
	// re-created. Member/node rows are single-scope; ANY() is exact there.
	// W-F: the topic edge follows the graph_cluster_edge lifecycle verbatim (OR
	// on both endpoint scopes — a mixed cross-partition row is swept by the first
	// partition run that owns either endpoint and never re-created, because the
	// scoped projection emits AND-rows). Super rows and their membership are
	// single-scope by construction (the meta level is computed per scope), so
	// ANY() is exact there.
	for _, del := range []struct{ label, sql string }{
		{"member", `DELETE FROM graph_cluster_member WHERE scope = ANY($1)`},
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

// insertMembers writes this run's block→cluster assignment in deterministic
// order (unnest text[]→uuid[] cast, batched at memberBatch) and returns the set
// of distinct cluster ids.
//
// scope is denormalized from the Louvain input (B-W2, 087) — the member row
// records the partition the clustering RAN in, never a re-read a concurrent
// scope-move could skew. block_id stays the SOLE PK: the overview input is
// strictly owned-disjoint (no grants), so no block is a member under two
// scopes.
//
// Split out of persist in W3 purely to keep that function inside the cyclop
// budget once the identity phase joined it; the body is unchanged.
func insertMembers(ctx context.Context, tx pgx.Tx, cl clustering, opts Options,
	nodeScopes map[string]string, scoped bool, filterSet map[string]struct{},
) (map[string]struct{}, error) {
	blocks := make([]string, 0, len(cl.blockToCluster))
	for b := range cl.blockToCluster {
		blocks = append(blocks, b)
	}
	sort.Strings(blocks)
	clusters := make([]string, len(blocks))
	scopes := make([]string, len(blocks))
	clusterSet := make(map[string]struct{})
	for i, b := range blocks {
		clusters[i] = cl.blockToCluster[b]
		clusterSet[cl.blockToCluster[b]] = struct{}{}
		scope, ok := nodeScopes[b]
		if !ok || scope == "" {
			// Fail loud: a member without an input scope would either violate
			// NOT NULL or silently land in the wrong partition (B-W3 poison).
			return nil, fmt.Errorf("insert members: block %s has no Louvain-input scope", b)
		}
		if scoped {
			// Input-purity guard (B-W3): a scoped run whose input carries a
			// block outside the filter would collide with the surviving
			// foreign partition (block_id PK) or poison it. Input scoping
			// itself (loadNodes/loadEdges) is B-W6 — until then callers must
			// hand persist an already-cut input; fail loud, never trim.
			if _, in := filterSet[scope]; !in {
				return nil, fmt.Errorf("insert members: block %s scope %q outside ScopeFilter %v — input not partition-cut (B-W6 wires scoped loading)", b, scope, opts.ScopeFilter)
			}
		}
		scopes[i] = scope
	}
	for i := 0; i < len(blocks); i += memberBatch {
		end := min(i+memberBatch, len(blocks))
		if _, err := tx.Exec(ctx,
			`INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
			 SELECT b::uuid, c::uuid, s FROM unnest($1::text[], $2::text[], $3::text[]) AS t(b, c, s)`,
			blocks[i:end], clusters[i:end], scopes[i:end]); err != nil {
			return nil, fmt.Errorf("insert members: %w", err)
		}
	}
	return clusterSet, nil
}

// partitionHasLiveTopics reports whether this run's partition still carries at
// least one non-retired topic. It is the discriminator of the empty-cut guard
// (review K2-3) and deliberately an EXISTS, not a count: the answer is binary
// and the index-free path over a small table should stop at the first row.
func partitionHasLiveTopics(ctx context.Context, pool *pgxpool.Pool, scopeFilter []string) (bool, error) {
	const q = `SELECT EXISTS (SELECT 1 FROM graph_cluster_topic WHERE retired_at IS NULL`
	sql, args := q+`)`, []any(nil)
	if len(scopeFilter) > 0 {
		sql, args = q+` AND scope = ANY($1))`, []any{scopeFilter}
	}
	var live bool
	if err := pool.QueryRow(ctx, sql, args...).Scan(&live); err != nil {
		return false, fmt.Errorf("overview: probing live topics of %v: %w", scopeFilter, err)
	}
	return live, nil
}

// reachableNodesTemplate counts the (cluster, scope) groups the node
// aggregation CAN produce a row for: an ov_match row plus at least one member
// that is still visible at aggregation time. It mirrors nodeAggTemplate's join
// shape exactly — grouping on the BLOCK's scope, joining ov_match on it — so
// the two numbers are comparable.
//
// Slots: the type-visibility fragment ($1) and the scope filter ($2).
const reachableNodesTemplate = `
SELECT count(*) FROM (
    SELECT m.cluster_id, b.scope
      FROM graph_cluster_member m
      JOIN context_blocks b ON b.id = m.block_id AND %s
      JOIN ov_match mt ON mt.cluster_id = m.cluster_id AND mt.scope = b.scope
      %s
     GROUP BY 1, 2) reachable`

// missingNodesTemplate names the assigned clusters that produced no row, for
// the log. Capped — the point is a diagnosis, not a dump.
const missingNodesTemplate = `
SELECT mt.cluster_id::text || '/' || mt.scope
  FROM ov_match mt
 WHERE NOT EXISTS (
    SELECT 1 FROM graph_cluster_member m
      JOIN context_blocks b ON b.id = m.block_id AND %s
     WHERE m.cluster_id = mt.cluster_id AND b.scope = mt.scope %s)
 ORDER BY 1 LIMIT 20`

var (
	reachableNodesSQL       = fmt.Sprintf(reachableNodesTemplate, visibility.TypeVisible("b", "$1"), "")
	reachableNodesScopedSQL = fmt.Sprintf(reachableNodesTemplate, visibility.TypeVisible("b", "$1"), "WHERE m.scope = ANY($2)")
	missingNodesSQL         = fmt.Sprintf(missingNodesTemplate, visibility.TypeVisible("b", "$1"), "")
	missingNodesScopedSQL   = fmt.Sprintf(missingNodesTemplate, visibility.TypeVisible("b", "$1"), "AND m.scope = ANY($2)")
)

// checkNodeCoverage compares the node aggregation's output against the
// assignment and separates the two reasons it can fall short (W3, review K2-2).
//
// Every (cluster, scope) of this run has an ov_match row by construction —
// carried, re-attached or freshly minted — so in the normal case the node
// aggregation writes exactly that many rows. A shortfall has two possible
// causes with opposite handling:
//
//   - BENIGN, and reachable from outside: the aggregation joins context_blocks
//     through the visibility fragment, the assignment does not. A block that
//     was archived, moved scope or changed type BETWEEN loadNodes and this
//     transaction therefore drops out of the aggregation while its member row
//     stays. If it was the last visible member of its cluster, that cluster
//     writes no node row. The window is the ENTIRE Louvain runtime — minutes at
//     the target scale — so this is a race a live system will lose regularly,
//     and failing the whole rebuild for it would trade a missing topic against
//     a missing map. It is logged with the affected clusters and the run
//     continues; the topic simply has no node row this generation.
//   - A DEFECT: the identity join dropped a cluster that still HAS visible
//     members. That is the state this axis exists to abolish, and it stays a
//     loud rollback — the previous map remains readable and the next tick
//     reruns.
//
// The discriminator is one extra query, and it only runs when the counts
// already disagree — the happy path pays nothing.
func checkNodeCoverage(ctx context.Context, tx pgx.Tx, opts Options, scoped bool, written, assigned int) error {
	if written == assigned {
		return nil
	}
	reachSQL, missSQL := reachableNodesSQL, missingNodesSQL
	args := []any{opts.VisibleTypes}
	if scoped {
		reachSQL, missSQL = reachableNodesScopedSQL, missingNodesScopedSQL
		args = append(args, opts.ScopeFilter)
	}
	var reachable int
	if err := tx.QueryRow(ctx, reachSQL, args...).Scan(&reachable); err != nil {
		return fmt.Errorf("node coverage check (%d rows for %d assigned clusters): %w", written, assigned, err)
	}
	var missing []string
	rows, err := tx.Query(ctx, missSQL, args...)
	if err != nil {
		return fmt.Errorf("node coverage diagnosis (%d rows for %d assigned clusters): %w", written, assigned, err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return fmt.Errorf("node coverage diagnosis: %w", err)
		}
		missing = append(missing, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("node coverage diagnosis: %w", err)
	}

	if written == reachable {
		slog.Warn("overview: assigned clusters lost every visible member between the Louvain load and persist — archived, scope-moved or type-changed mid-run; those topics get no node row this generation",
			"written", written, "assigned", assigned, "clusters", missing, "scope_filter", opts.ScopeFilter)
		return nil
	}
	return fmt.Errorf("node aggregation wrote %d rows for %d assigned clusters, %d of which still have visible members — the identity join dropped a cluster (persist rolls back, previous map stays readable); affected: %v",
		written, assigned, reachable, missing)
}

// writeMeta replaces the liveness rows this run owns (B-W5, migration 088) and
// stamps the meta level's two columns behind them (W-F, migration 127).
//
// Split out of persist purely to keep that function inside the cyclop budget
// once the W-F statements joined it; the body is the unchanged W-A logic plus
// the super stamp, in that order — and the ORDER is load-bearing, see
// writeSuperMeta.
func writeMeta(ctx context.Context, tx pgx.Tx, scoped bool, opts Options, stats Stats,
	candidates map[string]int, super superLevel,
) error {
	candScopes, candNs := candidateArrays(candidates)
	// The global run replaces ALL rows (its scope set = every scope it just
	// aggregated), the scoped run only its filter scopes — a foreign
	// partition's computed_at is never touched (leak B1-m1).
	if !scoped {
		// W-A: the teardown is restricted to the scopes this run re-writes.
		// The unconditional DELETE would erase a SKIP-ONLY row (the "fresh
		// deploy above the cap" case: computed_at IS NULL, no cluster rows) —
		// metaWriteGlobalSQL only re-creates scopes present in
		// graph_cluster_node, so the skip memory would vanish on the next
		// successful global run. The node table already carries THIS run's
		// rows here (teardown + aggregation ran before), so the filter is
		// exactly the set the INSERT below covers.
		if _, err := tx.Exec(ctx,
			`DELETE FROM graph_overview_meta WHERE scope IN (SELECT DISTINCT scope FROM graph_cluster_node)`); err != nil {
			return fmt.Errorf("meta teardown: %w", err)
		}
		if _, err := tx.Exec(ctx, metaWriteGlobalSQL, stats.Modularity, opts.Resolution, candScopes, candNs); err != nil {
			return fmt.Errorf("meta write: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM graph_overview_meta WHERE scope = ANY($1)`, opts.ScopeFilter); err != nil {
			return fmt.Errorf("scoped meta teardown: %w", err)
		}
		if _, err := tx.Exec(ctx, metaWriteScopedSQL, stats.Modularity, opts.Resolution, opts.ScopeFilter, candScopes, candNs); err != nil {
			return fmt.Errorf("scoped meta write: %w", err)
		}
	}
	// W-F: the meta level's two columns come LAST — the rows they belong to
	// were only just re-created above.
	return writeSuperMeta(ctx, tx, super)
}

// persist replaces the 057 tables in one advisory-locked transaction —
// globally (nil ScopeFilter: TRUNCATE + unscoped aggregation, the pre-B
// behaviour) or for ONE partition (B-W3: scoped DELETE + scoped aggregation,
// ATOMICALLY paired — a scoped DELETE with the unscoped aggregation
// resurrects foreign-partition rows into the (cluster_id, scope) PK ⇒ 23505,
// the verified B1-C1 breakage). opts.VisibleTypes feeds the aggregation
// re-checks (T6, $1). nodeScopes is the Louvain-input scope per block
// (loadNodes) — denormalized onto the member row (B-W2, migration 087).
//
// candidates is the per-scope node-candidate tally of THIS attempt (W-A). It
// is handed down rather than recomputed because the advisory-lock skip is
// born here (see below) and must be able to report it — persist otherwise
// only knows the blocks that survived into the clustering.
//
// super is the ALREADY COMPUTED meta level (W-F). It arrives as a finished
// grouping precisely so that this function never runs a Louvain probe: the
// order inside the transaction is teardown → member → identity → aggregation →
// topic edges → super INSERT → meta, and every one of those is a write.
func persist(ctx context.Context, pool *pgxpool.Pool, cl clustering, opts Options, nodeScopes map[string]string, candidates map[string]int, super superLevel) (Stats, error) {
	persistStart := time.Now()
	scoped := len(opts.ScopeFilter) > 0
	filterSet := make(map[string]struct{}, len(opts.ScopeFilter))
	for _, s := range opts.ScopeFilter {
		filterSet[s] = struct{}{}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return Stats{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	// W3: FIRST statement of the tx — temp_buffers is only settable while the
	// session has not touched a temp table yet (see persistTempBuffers). The
	// value is a code-owned constant, never input.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL temp_buffers = '%s'`, persistTempBuffers)); err != nil {
		return Stats{}, fmt.Errorf("temp_buffers: %w", err)
	}

	// B-W4: the lock is per-partition — two different tenants persist in
	// parallel, the same tenant (and the global run against old binaries)
	// keeps serializing. See lockKeyForScopes.
	lockKey := lockKeyForScopes(opts.ScopeFilter)
	var locked bool
	// S2: die Lock-Uhr laeuft von der ERTEILUNG bis zum Commit — nicht ab
	// Begin. Der Unterschied ist die Wartezeit auf den Lock selbst, und die ist
	// keine Haltezeit: sie blockiert niemanden, sie wird blockiert. Wer beides
	// zusammenwirft, meldet unter Konkurrenz eine Haltezeit, die nie stattfand.
	lockAcquired := time.Now()
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey).Scan(&locked); err != nil {
		return Stats{}, fmt.Errorf("advisory lock: %w", err)
	}
	if !locked {
		slog.Info("overview: rebuild skipped — advisory lock held by another instance",
			"lock_key", lockKey, "scope_filter", opts.ScopeFilter)
		return Stats{Skipped: true, SkipReason: "advisory-lock", CandidateCount: candidates}, nil
	}

	// W3, K5 order: the predecessor snapshot has to be taken BEFORE the
	// teardown — graph_cluster_member is the only record of the old block→
	// topic assignment and the teardown deletes it.
	phase := topicPhase{
		scoped: scoped, scopeFilter: opts.ScopeFilter,
		tombstone: opts.TombstoneRetention, members: len(cl.blockToCluster),
	}
	if err := phase.snapshotPrevTopics(ctx, tx); err != nil {
		return Stats{}, err
	}

	if err := teardown(ctx, tx, scoped, opts.ScopeFilter); err != nil {
		return Stats{}, err
	}

	clusterSet, err := insertMembers(ctx, tx, cl, opts, nodeScopes, scoped, filterSet)
	if err != nil {
		return Stats{}, err
	}

	// W3 identity phase — between members and aggregation (K5). Everything in
	// here works on TEMP tables; no Louvain, no resolution search.
	topics, err := assignTopics(ctx, tx, phase, buildCores(cl, nodeScopes))
	if err != nil {
		return Stats{}, err
	}

	// Aggregation — ALWAYS the variant matching the teardown above (B1-C1:
	// scoped DELETE + unscoped aggregation = resurrected foreign rows ⇒ 23505).
	var edgeTag, nodeTag pgconn.CommandTag
	if !scoped {
		if nodeTag, err = tx.Exec(ctx, nodeAggSQL, opts.VisibleTypes); err != nil {
			return Stats{}, fmt.Errorf("node aggregation: %w", err)
		}
		edgeTag, err = tx.Exec(ctx, edgeAggSQL, opts.VisibleTypes)
		if err != nil {
			return Stats{}, fmt.Errorf("edge aggregation: %w", err)
		}
	} else {
		if nodeTag, err = tx.Exec(ctx, nodeAggScopedSQL, opts.VisibleTypes, opts.ScopeFilter); err != nil {
			return Stats{}, fmt.Errorf("scoped node aggregation: %w", err)
		}
		edgeTag, err = tx.Exec(ctx, edgeAggScopedSQL, opts.VisibleTypes, opts.ScopeFilter)
		if err != nil {
			return Stats{}, fmt.Errorf("scoped edge aggregation: %w", err)
		}
	}

	// W3 completeness check.
	assigned := topics.carried + topics.reattached + topics.born + topics.split
	if err := checkNodeCoverage(ctx, tx, opts, scoped, int(nodeTag.RowsAffected()), assigned); err != nil {
		return Stats{}, err
	}

	// W-F, in the §9.1 order: the topic edge projection reads the edge rows the
	// aggregation just wrote AND the identity the node rows just got, so it can
	// only run here; the meta level then writes the grouping that was computed
	// back in Rebuild's compute window.
	if err := writeTopicEdges(ctx, tx, scoped, opts.ScopeFilter); err != nil {
		return Stats{}, err
	}
	if err := writeSuperLevel(ctx, tx, super); err != nil {
		return Stats{}, err
	}

	// W5: the deterministic fallback label plus the materialized drift state.
	// It runs AFTER the node aggregation because it reads core_blocks,
	// category_counts and repr_title off the rows that aggregation just wrote,
	// and INSIDE this transaction because "the map is never unnamed" has to be
	// as atomic as the identity it names (design/01 §4.6).
	labelled, err := phase.writeFallbackLabels(ctx, tx)
	if err != nil {
		return Stats{}, err
	}

	stats := Stats{
		NodeCount:         len(cl.blockToCluster),
		ClusterCount:      len(clusterSet),
		EdgeRows:          int(edgeTag.RowsAffected()),
		Modularity:        cl.modularity,
		CandidateCount:    candidates,
		TopicsCarried:     topics.carried,
		TopicsReattached:  topics.reattached,
		TopicsBorn:        topics.born,
		TopicsSplit:       topics.split,
		TopicsRetired:     topics.retired,
		MembersChanged:    topics.membersChanged,
		MembersReassigned: topics.membersReassigned,
		SuperN:            len(super.Groups),
		SuperCapped:       len(super.Capped) > 0,
	}
	if err := writeMeta(ctx, tx, scoped, opts, stats, candidates, super); err != nil {
		return Stats{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Stats{}, fmt.Errorf("commit: %w", err)
	}
	// Nach dem Commit gelesen, nicht davor: der Commit selbst haelt den Lock
	// noch (pg_try_advisory_XACT_lock gibt ihn erst beim Transaktionsende frei),
	// und bei einem grossen Member-Schreibweg ist der Commit-Anteil genau die
	// Groesse, die S9a spaeter senken will.
	stats.LockHeldMs = int(time.Since(lockAcquired).Milliseconds())
	stats.PersistMs = int(time.Since(persistStart).Milliseconds())
	// W3 identity ledger. members_reassigned is deliberately reported NEXT TO
	// members_changed and not folded into it (K13): the two answer different
	// questions — how much of the corpus actually moved, versus how much of
	// the churn is nothing but a minUUID rename of communities that stayed put.
	slog.Info("overview: topic identity",
		"scope_filter", opts.ScopeFilter,
		"carried", stats.TopicsCarried, "reattached", stats.TopicsReattached,
		"born", stats.TopicsBorn, "split", stats.TopicsSplit, "retired", stats.TopicsRetired,
		"members_changed", stats.MembersChanged, "members_reassigned", stats.MembersReassigned,
		// W5: deliberately a LOG field and not a Stats field — Stats crosses the
		// worker IPC boundary with a strict decoder, and design/01 §3.6 pins the
		// axis to exactly ONE protocol change (W3's). The number is observability,
		// not a decision input, so it belongs where it costs nothing.
		"labels_touched", labelled)
	if super.Attempted {
		// W-F: separate log line, and a LOG line rather than more Stats fields.
		// The per-scope truth lives in graph_overview_meta; what an operator
		// needs here is whether the level was built at all and whether a scope
		// fell back to flat — both of which are invisible in the map when the
		// section is simply absent.
		slog.Info("overview: meta cluster level",
			"scope_filter", opts.ScopeFilter,
			"super_n", stats.SuperN, "capped_scopes", sortedKeys(super.Capped),
			"gamma", super.Gamma)
	}
	return stats, nil
}
