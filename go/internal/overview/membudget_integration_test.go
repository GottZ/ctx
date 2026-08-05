//go:build integration

// Achse 04 / Welle S7b — Gates S7b-G1 und S7b-G2 (design/04 §6.7).
package overview

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestS7b_G1_MemBudgetSkipsInsteadOfOOM ist S7b-G1.
//
// Die Rot-Probe steckt im Aufbau: OHNE Budget laeuft derselbe Korpus sauber
// durch (das belegt, dass der Skip vom Budget kommt und nicht vom Korpus), MIT
// einem kuenstlich kleinen Budget endet er als Skip mit Grund 'mem-budget' —
// und die Karte bleibt unangetastet.
func TestS7b_G1_MemBudgetSkipsInsteadOfOOM(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 300)

	base := Options{
		Resolution: 1.0, VisibleTypes: csrTypes, OverviewTypes: csrTypes,
		MaxNodes: 200000, MaxNodesCtx: 5000000, Engine: EngineCtx, Refine: true,
	}

	// Ohne Budget: laeuft durch. Ohne diese Haelfte belegte der Skip unten
	// nichts — er koennte jede beliebige Ursache haben.
	ok, err := Rebuild(ctx, pool, base)
	if err != nil {
		t.Fatalf("rebuild ohne Budget: %v", err)
	}
	if ok.Skipped {
		t.Fatalf("rebuild ohne Budget wurde uebersprungen (%q) — die Probe belegt nichts", ok.SkipReason)
	}
	before := dumpMemberRows(t, pool)
	computedBefore := computedAtOf(t, pool)

	// Mit einem Budget, das die Abschaetzung nicht halten kann.
	tight := base
	tight.WorkerMemLimit = 1 << 20 // 1 MiB
	st, err := Rebuild(ctx, pool, tight)
	if err != nil {
		t.Fatalf("Budget-Abbruch ergab einen FEHLER statt eines Skips: %v", err)
	}
	if !st.Skipped || st.SkipReason != "mem-budget" {
		t.Fatalf("erwartet Skip 'mem-budget', erhalten Skipped=%v Reason=%q", st.Skipped, st.SkipReason)
	}
	if after := dumpMemberRows(t, pool); !reflect.DeepEqual(before, after) {
		t.Fatal("graph_cluster_member wurde vom Budget-Skip veraendert")
	}
	if got := computedAtOf(t, pool); got != computedBefore {
		t.Fatal("computed_at rueckte trotz Budget-Skip vor")
	}

	// Der Grund muss ins LAUF-JOURNAL schreibbar sein. graph_overview_run.
	// skip_reason traegt bewusst KEINEN CHECK — genau deshalb braucht
	// 'mem-budget' keine Migration. Das hier ist der Beleg, nicht die Annahme.
	runID, err := StartRun(ctx, pool, RunStart{ScopeSet: nil, Engine: EngineCtx, Resolution: 1.0})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := FinishRun(ctx, pool, runID, RunResult{Outcome: "skipped", SkipReason: st.SkipReason, Stats: st}); err != nil {
		t.Fatalf("skip_reason %q ist nicht ins Lauf-Journal schreibbar: %v", st.SkipReason, err)
	}
}

// TestS7b_G1_EstimateIsCalibratedNotGuessed prueft die Abschaetzung gegen die
// Groessen, die S3-G3 GEMESSEN hat.
//
// Eine Abschaetzung, die zu NIEDRIG liegt, laesst den Lauf in genau das OOM
// laufen, das sie verhindern soll — sie muss also oberhalb der Messung liegen,
// aber nicht so weit, dass sie brauchbare Laeufe verbietet.
func TestS7b_G1_EstimateIsCalibratedNotGuessed(t *testing.T) {
	cases := []struct {
		nodes, pairs int
		measuredMB   int64 // aus .bench-s0s1/s3-g3-substrate.log
	}{
		{50_000, 112_370, 23},
		{200_000, 449_575, 39},
		{400_000, 899_260, 62},
	}
	for _, c := range cases {
		est := EstimateRebuildBytes(c.nodes, c.pairs) / (1 << 20)
		if est < c.measuredMB {
			t.Errorf("@%dk Knoten: Abschaetzung %d MB liegt UNTER der Messung %d MB — sie wuerde ins OOM laufen lassen",
				c.nodes/1000, est, c.measuredMB)
		}
		if est > 8*c.measuredMB {
			t.Errorf("@%dk Knoten: Abschaetzung %d MB ist mehr als 8x die Messung %d MB — sie wuerde brauchbare Laeufe verbieten",
				c.nodes/1000, est, c.measuredMB)
		}
		t.Logf("@%-7d Knoten: Abschaetzung %3d MB gegen gemessene %2d MB (CSR-Substrat)", c.nodes, est, c.measuredMB)
	}
}

// TestS7b_G2_ChildBudgetComesFromTheInheritedEnv belegt den Weg, auf dem das
// Kind sein Budget erfaehrt — die Umgebung, die es vom Daemon erbt.
//
// Und die Fail-Richtung: ein VERTIPPTER Wert deckelt nicht, verhindert aber
// auch nichts. Ein Ops-Knopf mit Tippfehler darf den Rebuild nicht toeten.
func TestS7b_G2_ChildBudgetComesFromTheInheritedEnv(t *testing.T) {
	t.Setenv(WorkerMemLimitEnv, "")
	if v, err := WorkerMemLimitBytes(); v != 0 || err != nil {
		t.Fatalf("ungesetzt ⇒ (%d, %v), erwartet (0, nil)", v, err)
	}
	t.Setenv(WorkerMemLimitEnv, "314572800")
	if v, err := WorkerMemLimitBytes(); v != 314572800 || err != nil {
		t.Fatalf("300 MiB ⇒ (%d, %v)", v, err)
	}
	for _, bad := range []string{"300MiB", "-1", "viel"} {
		t.Setenv(WorkerMemLimitEnv, bad)
		v, err := WorkerMemLimitBytes()
		if err == nil {
			t.Errorf("%q wurde stillschweigend akzeptiert", bad)
		}
		if v != 0 {
			t.Errorf("%q ergab Limit %d — ein vertippter Wert darf nicht deckeln", bad, v)
		}
	}
	// ApplyWorkerMemLimit(0) MUSS ein no-op sein: ein Limit von 0 waere in Go
	// ein sofortiger Dauer-GC und damit ein Denial-of-Service gegen den
	// eigenen Rebuild.
	ApplyWorkerMemLimit(0)
	_ = os.Getenv(WorkerMemLimitEnv)
}
