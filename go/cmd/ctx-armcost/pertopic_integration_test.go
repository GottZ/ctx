//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// ─────────────────────────────────────────────────────────────────────────────
// V-W5 — Golden-Test der Per-Topic-Sicht.
//
// Die Fixture trägt GENAU die Formen, auf die die Gates zeigen: sechs Topics
// (fünf lebend, eines pensioniert), davon zwei lebende erschöpfte, eines ohne
// einen einzigen Call, eines mit gedriftetem Kern — plus eine Log-Zeile, die
// zu zwei Topics passt, eine Zeile VOR der Geburt ihres Topics, eine K9-Zeile
// ohne block_ids, eine Zeile vor dem Fenster und eine Zeile eines FREMDEN Arms.
//
// Die Erwartungswerte werden nicht hartkodiert, sondern in Go aus derselben
// Fixture-Beschreibung gezogen (fixtureExpect) — zwei Wege, ein Ergebnis.

// fixTopic ist eine Fixture-Topic-Zeile.
type fixTopic struct {
	id          string
	ageH        float64 // Geburt: dbNow − ageH
	retiredAgeH *float64
	label       string
	source      string
	attempts    int32
	stale       bool
	builtAgeH   *float64
	core        []string
}

// fixCall ist eine Fixture-Log-Zeile.
type fixCall struct {
	pipeline string
	ageH     float64
	blocks   []string
	duration *int64
	queue    *int64
	abort    *string
}

func blockID(n int) string        { return fmt.Sprintf("00000000-0000-0000-0000-0000000000%02x", n) }
func topicID(n int) string        { return fmt.Sprintf("00000000-0000-0000-0000-0000000001%02x", n) }
func hoursPtr(h float64) *float64 { return &h }

// perTopicFixture beschreibt die Fixture einmal — für den Seed UND für die
// unabhängigen Erwartungswerte.
func perTopicFixture() ([]fixTopic, []fixCall) {
	topics := []fixTopic{
		{id: topicID(1), ageH: 400, label: "Retrieval", source: "llm", attempts: 0, stale: false,
			builtAgeH: hoursPtr(200), core: []string{blockID(1), blockID(2)}},
		{id: topicID(2), ageH: 300, label: "Erschöpft alt", source: "fallback", attempts: 3, stale: true,
			builtAgeH: hoursPtr(250), core: []string{blockID(3), blockID(4)}},
		{id: topicID(3), ageH: 200, label: "Erschöpft jung", source: "llm", attempts: 3, stale: true,
			builtAgeH: hoursPtr(100), core: []string{blockID(5)}},
		{id: topicID(4), ageH: 50, label: "", source: "none", attempts: 0, stale: true,
			core: []string{blockID(6)}},
		{id: topicID(5), ageH: 150, label: "Kern-Drift", source: "llm", attempts: 1, stale: true,
			builtAgeH: hoursPtr(140), core: []string{blockID(7), blockID(8)}},
		{id: topicID(6), ageH: 350, retiredAgeH: hoursPtr(10), label: "Pensioniert erschöpft", source: "llm",
			attempts: 3, stale: true, builtAgeH: hoursPtr(300), core: []string{blockID(10)}},
	}
	calls := []fixCall{
		// T1: zwei exakte Treffer und ein Treffer auf einem Teil des Kerns.
		{pipeline: armClusterLabel, ageH: 350, blocks: []string{blockID(1), blockID(2)}, duration: ptr(int64(900)), queue: ptr(int64(100))},
		{pipeline: armClusterLabel, ageH: 300, blocks: []string{blockID(1), blockID(2)}, duration: ptr(int64(800)), queue: ptr(int64(0))},
		{pipeline: armClusterLabel, ageH: 200, blocks: []string{blockID(1)}, duration: ptr(int64(700))},
		// T2: zwei exakte Treffer.
		{pipeline: armClusterLabel, ageH: 250, blocks: []string{blockID(3), blockID(4)}, duration: ptr(int64(600)), queue: ptr(int64(50))},
		{pipeline: armClusterLabel, ageH: 100, blocks: []string{blockID(3), blockID(4)}, duration: ptr(int64(500)), queue: ptr(int64(0))},
		// T3: ein exakter Treffer.
		{pipeline: armClusterLabel, ageH: 150, blocks: []string{blockID(5)}, duration: ptr(int64(400)), queue: ptr(int64(0))},
		// MEHRDEUTIG: die beiden Blöcke sitzen heute in zwei verschiedenen Kernen.
		{pipeline: armClusterLabel, ageH: 120, blocks: []string{blockID(1), blockID(3)}, duration: ptr(int64(300)), queue: ptr(int64(0))},
		// VOR der Geburt von T4 — die Zeitschranke muss sie abweisen.
		{pipeline: armClusterLabel, ageH: 60, blocks: []string{blockID(6)}, duration: ptr(int64(200)), queue: ptr(int64(0))},
		// Trifft NUR das pensionierte Topic.
		{pipeline: armClusterLabel, ageH: 300, blocks: []string{blockID(10)}, duration: ptr(int64(250)), queue: ptr(int64(0))},
		// K9-Ablehnung: kein Wire-Call, keine block_ids.
		{pipeline: armClusterLabel, ageH: 80, abort: ptr("queue_full"), queue: ptr(int64(0))},
		// T5: gedrifteter Kern — Überlappung, keine Gleichheit.
		{pipeline: armClusterLabel, ageH: 140, blocks: []string{blockID(7), blockID(9)}, duration: ptr(int64(150)), queue: ptr(int64(0))},
		// VOR dem Fenster (älter als das älteste lebende Topic).
		{pipeline: armClusterLabel, ageH: 450, blocks: []string{blockID(1)}, duration: ptr(int64(999)), queue: ptr(int64(0))},
		// FREMDER Arm — darf nirgends auftauchen.
		{pipeline: "dream-eval", ageH: 90, blocks: []string{blockID(1), blockID(2)}, duration: ptr(int64(5000)), queue: ptr(int64(0))},
	}
	return topics, calls
}

func seedPerTopic(t *testing.T, pool *pgxpool.Pool, topics []fixTopic, calls []fixCall) {
	t.Helper()
	ctx := context.Background()
	secs := func(h *float64) *float64 {
		if h == nil {
			return nil
		}
		s := *h * 3600
		return &s
	}
	for _, tp := range topics {
		var label *string
		if tp.label != "" {
			l := tp.label
			label = &l
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph_cluster_topic
			  (topic_id, scope, created_at, last_seen_at, retired_at, core_blocks,
			   label, label_source, label_built_at, label_attempts, label_stale)
			VALUES ($1::uuid, 'private',
			        now() - make_interval(secs => $2),
			        now() - make_interval(secs => $2),
			        CASE WHEN $3::float8 IS NULL THEN NULL ELSE now() - make_interval(secs => $3) END,
			        $4::uuid[], $5, $6,
			        CASE WHEN $7::float8 IS NULL THEN NULL ELSE now() - make_interval(secs => $7) END,
			        $8, $9)`,
			tp.id, tp.ageH*3600, secs(tp.retiredAgeH), tp.core, label, tp.source,
			secs(tp.builtAgeH), tp.attempts, tp.stale); err != nil {
			t.Fatalf("seed topic %s: %v", tp.id, err)
		}
	}
	for i, c := range calls {
		var blocks any
		if len(c.blocks) > 0 {
			blocks = c.blocks
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_llm_log
			  (created_at, pipeline, model, host, duration_ms, queue_wait_ms, block_ids,
			   dispatch_class, dispatch_abort)
			VALUES (now() - make_interval(secs => $1), $2, 'qwen', 'h', $3, $4, $5::uuid[], 'background', $6)`,
			c.ageH*3600, c.pipeline, c.duration, c.queue, blocks, c.abort); err != nil {
			t.Fatalf("seed call %d: %v", i, err)
		}
	}
}

// expectTopic ist der unabhängig in Go gezogene Erwartungswert einer Zeile.
type expectTopic struct {
	calls, exact, ambiguous int64
	occupancy, wire         float64
	exhausted               bool
}

// fixtureExpect wendet die Join-Regel in Go auf die Fixture an — ohne eine
// Zeile SQL des Werkzeugs. Fenster: ab dem ältesten LEBENDEN Topic.
func fixtureExpect(topics []fixTopic, calls []fixCall) (map[string]expectTopic, AssignStats, float64) {
	windowAge := 0.0
	for _, tp := range topics {
		if tp.retiredAgeH == nil && tp.ageH > windowAge {
			windowAge = tp.ageH
		}
	}
	inWindow := func(c fixCall) bool { return c.ageH <= windowAge && c.pipeline == armClusterLabel }

	out := map[string]expectTopic{}
	for _, tp := range topics {
		if tp.retiredAgeH == nil {
			out[tp.id] = expectTopic{exhausted: tp.stale && tp.attempts >= exhaustedAttempts}
		}
	}
	var st AssignStats
	for _, c := range calls {
		if c.pipeline == armClusterLabel && c.ageH > windowAge {
			st.RowsBeforeWindow++
			continue
		}
		if !inWindow(c) {
			continue
		}
		st.ArmRows++
		if len(c.blocks) == 0 {
			st.RowsWithoutBlockIDs++
			continue
		}
		var hitsLiving, hitsRetired int64
		for _, tp := range topics {
			// Die Regel: Überlappung UND der Call liegt nach der Geburt.
			if !overlap(tp.core, c.blocks) || c.ageH > tp.ageH {
				continue
			}
			if tp.retiredAgeH != nil {
				hitsRetired++
				continue
			}
			hitsLiving++
		}
		if hitsLiving == 0 {
			if hitsRetired > 0 {
				st.UnassignedRetiredOnly++
			}
			continue
		}
		st.AssignedRows++
		if hitsLiving > 1 {
			st.AmbiguousRows++
		}
		if hitsLiving > st.MaxTopicsPerRow {
			st.MaxTopicsPerRow = hitsLiving
		}
		for _, tp := range topics {
			if tp.retiredAgeH != nil || !overlap(tp.core, c.blocks) || c.ageH > tp.ageH {
				continue
			}
			e := out[tp.id]
			e.calls++
			if sameSet(tp.core, c.blocks) {
				e.exact++
			}
			if hitsLiving > 1 {
				e.ambiguous++
			}
			if c.duration != nil {
				wait := int64(0)
				if c.queue != nil {
					wait = *c.queue
				}
				e.occupancy += float64(*c.duration-wait) / 1000
				e.wire += float64(*c.duration) / 1000
			}
			st.SumTopicCalls++
			out[tp.id] = e
		}
	}
	st.UnassignedRows = st.ArmRows - st.AssignedRows
	return out, st, windowAge
}

func overlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return overlapAll(a, b) && overlapAll(b, a)
}

func overlapAll(a, b []string) bool {
	for _, x := range a {
		found := false
		for _, y := range b {
			if x == y {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestPerTopicGoldenGegenHandzaehler ist das Grün-Gate der Welle: der Report
// reproduziert die unabhängig in Go gezogenen Zähler je Topic, führt die
// Topics ohne Call mit 0, schneidet die erschöpften lebenden Topics als eigene
// Sektion und weist die Mehrdeutigkeit aus.
func TestPerTopicGoldenGegenHandzaehler(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	topics, calls := perTopicFixture()
	seedPerTopic(t, pool, topics, calls)

	var dbNow time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	pt, err := buildPerTopic(ctx, pool, armClusterLabel, dbNow, dbNow.Add(-time.Hour))
	if err != nil {
		t.Fatalf("buildPerTopic: %v", err)
	}

	want, wantStats, windowAge := fixtureExpect(topics, calls)
	if pt.LivingTopics != len(want) {
		t.Fatalf("lebende Topics = %d, erwartet %d", pt.LivingTopics, len(want))
	}
	// Das Fenster beginnt an der Geburt des ältesten lebenden Topics.
	if gotH := dbNow.Sub(pt.Since).Hours(); gotH < windowAge-0.1 || gotH > windowAge+0.1 {
		t.Fatalf("Topic-Fenster beginnt %v h vor now, erwartet %v h", gotH, windowAge)
	}

	seen := map[string]bool{}
	for _, got := range pt.Topics {
		w, ok := want[got.TopicID]
		if !ok {
			t.Fatalf("unbekanntes Topic in der Sicht: %s", got.TopicID)
		}
		seen[got.TopicID] = true
		switch {
		case got.Calls != w.calls:
			t.Fatalf("%s calls=%d, erwartet %d", got.TopicID, got.Calls, w.calls)
		case got.CallsExact != w.exact:
			t.Fatalf("%s exakt=%d, erwartet %d", got.TopicID, got.CallsExact, w.exact)
		case got.CallsAmbiguous != w.ambiguous:
			t.Fatalf("%s mehrdeutig=%d, erwartet %d", got.TopicID, got.CallsAmbiguous, w.ambiguous)
		case !near(got.OccupancySeconds, w.occupancy):
			t.Fatalf("%s belegung_s=%v, erwartet %v", got.TopicID, got.OccupancySeconds, w.occupancy)
		case !near(got.WireSeconds, w.wire):
			t.Fatalf("%s wire_s=%v, erwartet %v", got.TopicID, got.WireSeconds, w.wire)
		case got.Exhausted != w.exhausted:
			t.Fatalf("%s erschöpft=%t, erwartet %t", got.TopicID, got.Exhausted, w.exhausted)
		case got.Calls > 0 && got.LastCall == nil:
			t.Fatalf("%s hat %d Calls, aber keinen letzten Call", got.TopicID, got.Calls)
		case got.Calls == 0 && got.LastCall != nil:
			t.Fatalf("%s hat 0 Calls, aber einen letzten Call %v", got.TopicID, got.LastCall)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("nur %d von %d lebenden Topics in der Sicht — fehlend: %v", len(seen), len(want), missing(want, seen))
	}

	// GATE (d): das Topic ohne einen einzigen Call steht MIT 0 in der Sicht.
	zero := findTopic(pt.Topics, topicID(4))
	if zero == nil {
		t.Fatal("das Topic ohne Label-Call fehlt in der Sicht — die Pflicht-Probe der Welle")
	}
	if zero.Calls != 0 || zero.CallsPerHour != 0 {
		t.Fatalf("Topic ohne Call: calls=%d calls/h=%v, erwartet 0/0", zero.Calls, zero.CallsPerHour)
	}

	// GATE (e): Mehrdeutigkeit gezählt, nicht still zugeschlagen.
	a := pt.Assignment
	switch {
	case a.ArmRows != wantStats.ArmRows:
		t.Fatalf("arm_rows=%d, erwartet %d", a.ArmRows, wantStats.ArmRows)
	case a.AssignedRows != wantStats.AssignedRows:
		t.Fatalf("zugeordnet=%d, erwartet %d", a.AssignedRows, wantStats.AssignedRows)
	case a.AmbiguousRows != wantStats.AmbiguousRows || a.AmbiguousRows != 1:
		t.Fatalf("mehrdeutig=%d, erwartet %d (Fixture: genau 1)", a.AmbiguousRows, wantStats.AmbiguousRows)
	case a.MaxTopicsPerRow != wantStats.MaxTopicsPerRow || a.MaxTopicsPerRow != 2:
		t.Fatalf("max Topics/Zeile=%d, erwartet %d (Fixture: 2)", a.MaxTopicsPerRow, wantStats.MaxTopicsPerRow)
	case a.UnassignedRows != wantStats.UnassignedRows:
		t.Fatalf("nicht zugeordnet=%d, erwartet %d", a.UnassignedRows, wantStats.UnassignedRows)
	case a.UnassignedRetiredOnly != wantStats.UnassignedRetiredOnly || a.UnassignedRetiredOnly != 1:
		t.Fatalf("nur pensioniert=%d, erwartet %d (Fixture: 1)", a.UnassignedRetiredOnly, wantStats.UnassignedRetiredOnly)
	case a.RowsWithoutBlockIDs != wantStats.RowsWithoutBlockIDs || a.RowsWithoutBlockIDs != 1:
		t.Fatalf("ohne block_ids=%d, erwartet %d (Fixture: die K9-Zeile)", a.RowsWithoutBlockIDs, wantStats.RowsWithoutBlockIDs)
	case a.RowsBeforeWindow != wantStats.RowsBeforeWindow || a.RowsBeforeWindow != 1:
		t.Fatalf("vor dem Fenster=%d, erwartet %d (Fixture: 1)", a.RowsBeforeWindow, wantStats.RowsBeforeWindow)
	case a.SumTopicCalls != wantStats.SumTopicCalls:
		t.Fatalf("Σ Topic-Calls=%d, erwartet %d", a.SumTopicCalls, wantStats.SumTopicCalls)
	case a.SumTopicCalls != a.AssignedRows+a.AmbiguousRows:
		t.Fatalf("Σ Topic-Calls %d ≠ zugeordnet %d + mehrdeutig %d — die Doppelzählung ist nicht ausgewiesen",
			a.SumTopicCalls, a.AssignedRows, a.AmbiguousRows)
	}

	// GATE (c): die zwei erschöpften LEBENDEN Topics, ältestes Label zuerst;
	// das pensionierte erschöpfte Topic steht NICHT darin.
	if len(pt.ExhaustedTopics) != 2 {
		t.Fatalf("erschöpfte Topics = %d, erwartet 2: %+v", len(pt.ExhaustedTopics), pt.ExhaustedTopics)
	}
	if pt.ExhaustedTopics[0].TopicID != topicID(2) || pt.ExhaustedTopics[1].TopicID != topicID(3) {
		t.Fatalf("Reihenfolge der erschöpften Topics: %s, %s — erwartet ältestes Label zuerst (%s, %s)",
			pt.ExhaustedTopics[0].TopicID, pt.ExhaustedTopics[1].TopicID, topicID(2), topicID(3))
	}
	for i, wantAge := range []float64{250, 100} {
		e := pt.ExhaustedTopics[i]
		if e.LabelAgeHours == nil {
			t.Fatalf("%s ohne Label-Alter", e.TopicID)
		}
		if *e.LabelAgeHours < wantAge-0.1 || *e.LabelAgeHours > wantAge+0.1 {
			t.Fatalf("%s Label-Alter %v h, erwartet %v h", e.TopicID, *e.LabelAgeHours, wantAge)
		}
		if e.LabelSource != map[int]string{0: "fallback", 1: "llm"}[i] {
			t.Fatalf("%s label_source=%s", e.TopicID, e.LabelSource)
		}
	}
	for _, e := range pt.ExhaustedTopics {
		if e.TopicID == topicID(6) {
			t.Fatal("das PENSIONIERTE erschöpfte Topic steht in der Sektion — der lebend-Filter greift nicht")
		}
	}

	// Der fremde Arm ist nirgends eingerechnet: dream-eval trägt 5 000 ms auf
	// den Kern von T1; die Belegung von T1 kennt sie nicht.
	t1 := findTopic(pt.Topics, topicID(1))
	if t1 == nil {
		t.Fatalf("%s fehlt in der Sicht", topicID(1))
	}
	if t1.WireSeconds > 3 {
		t.Fatalf("T1 wire_s=%v — die dream-eval-Zeile (5 000 ms auf denselben Kern) ist eingerechnet", t1.WireSeconds)
	}

	// Die Sektion rendert vollständig.
	var buf strings.Builder
	rep := Report{Since: dbNow.Add(-time.Hour), Until: dbNow, PerTopic: &pt, Footnotes: footnotes()}
	if err := renderTable(&buf, rep); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Label-Arm-Telemetrie je Topic — arm=cluster-label",
		"Erschöpfte Topics (lebend, label_stale AND label_attempts >= 3): 2 von 5",
		topicID(4), // das Topic ohne Call
		"mehrdeutig 1 (max 2 Topics/Zeile)",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("gerenderte Sektion ohne %q:\n%s", want, buf.String())
		}
	}
	if err := checkPerTopicNotes(buf.String()); err != nil {
		t.Fatalf("Pflicht-Notizen fehlen in der DB-gestützten Sektion: %v", err)
	}

	// ── NEGATIV-PROBE 1 (Pflicht, Gate 3): INNER JOIN statt LEFT JOIN.
	inner := strings.Replace(perTopicSQL, perTopicJoin, "JOIN", 1)
	if inner == perTopicSQL {
		t.Fatal("Negativ-Probe konnte den LEFT JOIN nicht ersetzen")
	}
	innerTopics, err := queryTopics(ctx, pool, inner, armClusterLabel, pt.Since, pt.Until)
	if err != nil {
		t.Fatalf("INNER-JOIN-Variante: %v", err)
	}
	if len(innerTopics) != len(pt.Topics)-1 {
		t.Fatalf("INNER-JOIN-Variante liefert %d Topics, erwartet %d (das Topic ohne Call muss verschwinden)",
			len(innerTopics), len(pt.Topics)-1)
	}
	if findTopic(innerTopics, topicID(4)) != nil {
		t.Fatal("Negativ-Probe wirkungslos: der INNER JOIN führt das Topic ohne Call weiterhin")
	}
	t.Logf("Negativ-Probe INNER JOIN: %d statt %d Topics — %s fehlt", len(innerTopics), len(pt.Topics), topicID(4))

	// ── NEGATIV-PROBE 2: die Regel OHNE Zeitschranke schlägt dem Topic ohne
	// Call einen Call zu, der vor seiner Geburt liegt.
	noTime := strings.Replace(perTopicSQL, topicMatchExpr, "t.core_blocks && c.block_ids", 1)
	if noTime == perTopicSQL {
		t.Fatal("Negativ-Probe konnte die Zeitschranke nicht entfernen")
	}
	loose, err := queryTopics(ctx, pool, noTime, armClusterLabel, pt.Since, pt.Until)
	if err != nil {
		t.Fatalf("Variante ohne Zeitschranke: %v", err)
	}
	l4 := findTopic(loose, topicID(4))
	if l4 == nil || l4.Calls != 1 {
		t.Fatalf("ohne Zeitschranke müsste %s genau 1 (vorgeburtlichen) Call tragen, hat %v", topicID(4), l4)
	}
	t.Logf("Negativ-Probe ohne Zeitschranke: %s bekommt %d Call vor seiner Geburt", topicID(4), l4.Calls)
}

func findTopic(list []TopicCalls, id string) *TopicCalls {
	for i := range list {
		if list[i].TopicID == id {
			return &list[i]
		}
	}
	return nil
}

func missing(want map[string]expectTopic, seen map[string]bool) []string {
	var out []string
	for id := range want {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
}

// TestPerTopicLeeresFensterUndFremderArm deckt die beiden Randfälle ab: ein
// Arm ohne eine einzige Zeile liefert eine leere, aber vollständige Bilanz,
// und ohne lebende Topics fällt das Fenster auf den Kosten-Report zurück.
func TestPerTopicLeeresFensterUndFremderArm(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var dbNow time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	// Ohne Topics: Fallback-Fenster, keine Zeilen, keine Panik.
	pt, err := buildPerTopic(ctx, pool, armClusterLabel, dbNow, dbNow.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("buildPerTopic ohne Topics: %v", err)
	}
	switch {
	case pt.LivingTopics != 0:
		t.Fatalf("lebende Topics=%d, erwartet 0", pt.LivingTopics)
	case pt.Assignment.ArmRows != 0 || pt.Assignment.AssignedRows != 0:
		t.Fatalf("Bilanz nicht leer: %+v", pt.Assignment)
	case !pt.Since.Equal(dbNow.Add(-24 * time.Hour)):
		t.Fatalf("Fallback-Fenster since=%v, erwartet %v", pt.Since, dbNow.Add(-24*time.Hour))
	case len(pt.Notes) == 0:
		t.Fatal("auch der leere Report trägt seine Notizen")
	}
}
