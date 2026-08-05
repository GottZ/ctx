//go:build integration

// Achse 04 / Welle S6+S7 — Gates S67-G1 … S67-G4 (design/04 §6.7).
//
// Die Welle legt einen SCHALTER, sie legt ihn nicht um. Deshalb ist ihr
// wichtigstes Gate ein NO-OP-Gate: mit dem Default (engine=gonum) muss sich
// nichts aendern — byte-genau nichts.
package overview

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestS67_G1_GonumDefaultIsAByteIdenticalNoOp ist S67-G1.
//
// Das Deploy dieser Welle darf am laufenden System NICHTS aendern. Geprueft
// wird die volle Partition, nicht die Clusterzahl — und zusaetzlich der
// partition_hash, weil er die Groesse ist, an der ein spaeterer Drift auffliegt.
func TestS67_G1_GonumDefaultIsAByteIdenticalNoOp(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 300)

	nodes, _, err := loadNodes(ctx, pool, csrTypes, nil)
	if err != nil {
		t.Fatalf("loadNodes: %v", err)
	}
	edges, err := loadEdges(ctx, pool, nil)
	if err != nil {
		t.Fatalf("loadEdges: %v", err)
	}
	legacy := computeClustering(nodes, edges, 1.0)

	// Der Weg, den die Welle einbaut — mit LEEREM Engine-Feld, also so, wie
	// ein Deploy ohne gesetzten Key ihn faehrt.
	engine, err := NormalizeEngine("")
	if err != nil {
		t.Fatalf("NormalizeEngine(\"\"): %v", err)
	}
	if engine != EngineGonum {
		t.Fatalf("leerer Engine-Wert ergibt %q statt %q", engine, EngineGonum)
	}
	if got := EffectiveMaxNodes(engine, 200000, 5000000); got != 200000 {
		t.Fatalf("gonum bekommt Cap %d statt max_nodes 200000", got)
	}

	_, _, csr, err := loadCSR(ctx, pool, csrTypes, nil)
	if err != nil {
		t.Fatalf("loadCSR: %v", err)
	}
	viaCSR := computeClusteringCSR(nodes, csr, 1.0)
	if !reflect.DeepEqual(legacy.blockToCluster, viaCSR.blockToCluster) {
		t.Fatal("gonum-Pfad weicht ab — das Deploy waere kein No-op")
	}
	if !reflect.DeepEqual(partitionHash(legacy.blockToCluster), partitionHash(viaCSR.blockToCluster)) {
		t.Fatal("partition_hash weicht ab")
	}
}

// TestS67_G2_UnknownEngineFailsLoudly ist S67-G2 mit seiner Rot-Probe.
//
// Ein stiller Fallback waere der verbotene Ausgang: wer engine=leiden schreibt,
// bekaeme eine gonum-Partition, die aussieht, als sei sie nach Wunsch
// gerechnet worden.
func TestS67_G2_UnknownEngineFailsLoudly(t *testing.T) {
	for _, bad := range []string{"leiden", "GONUM", "ctx ", "louvain"} {
		if _, err := NormalizeEngine(bad); err == nil {
			t.Errorf("engine=%q wurde akzeptiert — stiller Fallback ist der verbotene Ausgang", bad)
		}
	}
	for _, good := range []string{"", "gonum", "ctx"} {
		if _, err := NormalizeEngine(good); err != nil {
			t.Errorf("engine=%q abgelehnt: %v", good, err)
		}
	}
	// UD-07-04: die Engine waehlt den KEY, nicht den Wert.
	if got := EffectiveMaxNodes(EngineCtx, 200000, 5000000); got != 5000000 {
		t.Fatalf("engine=ctx bekommt Cap %d statt max_nodes_ctx 5000000", got)
	}
}

// TestS67_G3_TimeBudgetFreezesWithoutTouchingIdentity ist S67-G3 — das
// tragende Verhaltens-Gate der Welle.
//
// Drei Zusicherungen in einer Probe, weil sie nur zusammen etwas belegen:
//  1. ein gerissenes Zeitbudget meldet einen SKIP, keinen Fehler;
//  2. der Grund ist der BESTEHENDE Wert 'timeout' (Mig 123 traegt dafuer einen
//     CHECK — ein neuer Wert waere eine Migration fuer nichts);
//  3. graph_cluster_member ist danach BYTE-UNVERAENDERT und die Topic-Zeilen
//     sind unangetastet (W3-G12 / A01-5: ein Freeze fasst keine Identitaet an).
func TestS67_G3_TimeBudgetFreezesWithoutTouchingIdentity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 400)

	base := Options{
		Resolution: 1.0, VisibleTypes: csrTypes, OverviewTypes: csrTypes,
		MaxNodes: 200000, MaxNodesCtx: 5000000,
	}

	// Erst ein erfolgreicher Lauf, damit ueberhaupt etwas da ist, das
	// eingefroren werden kann.
	warm := base
	warm.Engine = EngineCtx
	warm.Refine = true
	if _, err := Rebuild(ctx, pool, warm); err != nil {
		t.Fatalf("warm-up rebuild: %v", err)
	}
	before := dumpMemberRows(t, pool)
	if len(before) == 0 {
		t.Fatal("der Aufwaermlauf hat keine Member geschrieben — die Probe belegt nichts")
	}
	topicsBefore := countRows(t, pool, `SELECT count(*) FROM graph_cluster_topic`)
	computedBefore := computedAtOf(t, pool)

	// Jetzt mit einem Budget, das nicht zu halten ist.
	tight := base
	tight.Engine = EngineCtx
	tight.Refine = true
	tight.TimeBudget = time.Nanosecond
	st, err := Rebuild(ctx, pool, tight)
	if err != nil {
		t.Fatalf("Zeitbudget-Abbruch ergab einen FEHLER statt eines Skips: %v", err)
	}
	if !st.Skipped || st.SkipReason != "timeout" {
		t.Fatalf("erwartet Skip mit Grund 'timeout', erhalten Skipped=%v Reason=%q", st.Skipped, st.SkipReason)
	}

	after := dumpMemberRows(t, pool)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("graph_cluster_member wurde vom eingefrorenen Lauf veraendert: %d → %d Zeilen", len(before), len(after))
	}
	if got := countRows(t, pool, `SELECT count(*) FROM graph_cluster_topic`); got != topicsBefore {
		t.Fatalf("Topic-Zeilen veraendert: %d → %d — ein Freeze darf keine Identitaet anfassen", topicsBefore, got)
	}
	if got := computedAtOf(t, pool); got != computedBefore {
		t.Fatalf("computed_at rueckte vor (%v → %v) — die Karte behauptet Frische, die sie nicht hat", computedBefore, got)
	}
	// Und der Grund muss in graph_overview_meta schreibbar sein: der CHECK aus
	// Mig 123/126 kennt genau die erlaubten Werte, und ein 23514 hier waere
	// der Beweis, dass die Welle doch eine Migration braucht.
	if err := StampAttempt(ctx, pool, nil, st.SkipReason, st.CandidateCount, time.Now()); err != nil {
		t.Fatalf("skip_reason %q ist nicht nach graph_overview_meta schreibbar: %v", st.SkipReason, err)
	}
}

// TestS67_G4_OptionsIPCIsStrict ist S67-G4: das Options-Fenster.
func TestS67_G4_OptionsIPCIsStrict(t *testing.T) {
	var buf bytes.Buffer
	{
		if err := EncodeWorkerOptions(&buf, Options{
			Resolution: 1.0, Engine: EngineCtx, TimeBudget: 600 * time.Second,
			MaxNodesCtx: 5000000, Refine: true, CSRLoader: true,
		}); err != nil {
			t.Fatalf("EncodeWorkerOptions: %v", err)
		}
	}
	// Der AKTUELLE Decoder liest verlustfrei.
	got, err := DecodeWorkerOptions(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("aktueller Decoder scheitert am eigenen Dokument: %v", err)
	}
	if got.Engine != EngineCtx || got.TimeBudget != 600*time.Second || got.MaxNodesCtx != 5000000 || !got.Refine {
		t.Fatalf("Roundtrip verlor S6+S7-Felder: %+v", got)
	}
	// Ein ALTES Kind (Options ohne die neuen Felder) MUSS laut scheitern.
	type legacyOptions struct {
		Resolution         float64
		VisibleTypes       []string
		OverviewTypes      []string
		MaxNodes           int
		ScopeFilter        []string
		TombstoneRetention time.Duration
		SuperEnabled       bool
		SuperTargetRows    int
		SuperMinResolution float64
		SuperMaxNodes      int
	}
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.DisallowUnknownFields() // exakt die Einstellung, die worker.go faehrt
	var old legacyOptions
	if err := dec.Decode(&old); err == nil {
		t.Fatal("altes Kind akzeptierte die neuen Options — ein stilles Verwerfen von Engine/Budget ist der verbotene Ausgang")
	}
}

// dumpMemberRows liest graph_cluster_member in stabiler Ordnung — die Form, in
// der "byte-unveraendert" ueberhaupt vergleichbar ist.
func dumpMemberRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT block_id::text || '|' || cluster_id::text || '|' || scope FROM graph_cluster_member`)
	if err != nil {
		t.Fatalf("dumping members: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning member: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dumping members: %v", err)
	}
	sort.Strings(out)
	return out
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("counting (%s): %v", q, err)
	}
	return n
}

// computedAtOf liest den Erfolgs-Zeitstempel der Karte. Er ist die Groesse, an
// der ein Konsument "frisch" von "eingefroren" unterscheidet — ruecken darf er
// nur nach einem ERFOLGREICHEN Lauf.
func computedAtOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s *string
	if err := pool.QueryRow(context.Background(),
		`SELECT max(computed_at)::text FROM graph_overview_meta`).Scan(&s); err != nil {
		t.Fatalf("reading computed_at: %v", err)
	}
	if s == nil {
		return ""
	}
	return *s
}
