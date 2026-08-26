package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/topiclabel"
)

// TestOhnePerTopicByteIdentisch ist Gate 6 der Welle: OHNE -per-topic ist der
// M-W7-Report byte-identisch. Die beiden Golden-Dateien wurden mit dem
// UNVERÄNDERTEN M-W7-Code erzeugt (Commit d03ac785) und liegen unter testdata/;
// diese Prüfung vergleicht Byte für Byte, nicht „enthält".
func TestOhnePerTopicByteIdentisch(t *testing.T) {
	rep := sampleReport()
	if rep.PerTopic != nil {
		t.Fatal("sampleReport darf keine Per-Topic-Sektion tragen")
	}
	var buf bytes.Buffer
	if err := renderTable(&buf, rep); err != nil {
		t.Fatal(err)
	}
	wantTxt, err := os.ReadFile(filepath.Join("testdata", "mw7-report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), wantTxt) {
		t.Fatalf("Tabelle nicht byte-identisch zum M-W7-Golden\nist:\n%s\nsoll:\n%s", buf.String(), wantTxt)
	}
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	wantJSON, err := os.ReadFile(filepath.Join("testdata", "mw7-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, wantJSON) {
		t.Fatalf("JSON nicht byte-identisch zum M-W7-Golden\nist:\n%s\nsoll:\n%s", body, wantJSON)
	}
	if strings.Contains(string(body), "per_topic") {
		t.Fatal("per_topic darf ohne die Sektion nicht im JSON stehen (omitempty)")
	}

	// Negativ-Probe der Prüfung selbst: mit gesetzter Sektion MUSS sie
	// fehlschlagen — sonst wäre die Byte-Identität keine Aussage.
	variant := rep
	variant.PerTopic = &PerTopicReport{Arm: armClusterLabel, JoinRule: joinRuleText}
	var vbuf bytes.Buffer
	if err := renderTable(&vbuf, variant); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(vbuf.Bytes(), wantTxt) {
		t.Fatal("Negativ-Probe wirkungslos: Report MIT Per-Topic-Sektion ist byte-gleich zum Golden")
	}
	vbody, err := json.MarshalIndent(variant, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vbody), `"per_topic"`) {
		t.Fatalf("gesetzte Sektion fehlt im JSON: %s", vbody)
	}
}

// TestCheckArmExitKontrakt fährt den Exit-2-Kontrakt des Flag-Paars.
func TestCheckArmExitKontrakt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		arm      string
		perTopic bool
		want     int
		needle   string
	}{
		{"keins von beiden", "", false, 0, ""},
		{"per-topic ohne arm", "", true, 2, "verlangt -arm=cluster-label"},
		{"arm ohne per-topic", armClusterLabel, false, 2, "verlangt -per-topic"},
		{"fremder arm", "dream-eval", true, 2, "nur -arm=cluster-label wird unterstützt"},
		{"leerer fremder arm", "  ", true, 2, "nur -arm=cluster-label wird unterstützt"},
		{"beide, cluster-label", armClusterLabel, true, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := checkArm(tc.arm, tc.perTopic)
			if code != tc.want {
				t.Fatalf("code=%d, erwartet %d (err=%v)", code, tc.want, err)
			}
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("kein Fehler erwartet: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.needle) {
				t.Fatalf("Meldung ohne %q: %v", tc.needle, err)
			}
		})
	}
}

// TestRunFremderArmExit2 belegt den Kontrakt am CLI-Rahmen: Exit 2, KEINE
// Datei, und der Abbruch passiert vor jeder DB-Berührung (der Lauf hat keine
// erreichbare Datenbank — er kommt gar nicht so weit).
func TestRunFremderArmExit2(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(cwd, ".arm-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "x.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-out", out, "-arm", "dream-eval", "-per-topic"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cluster-label") {
		t.Fatalf("stderr muss den unterstützten Arm nennen: %s", stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("auf dem Aufruffehler darf keine Datei entstehen (stat err=%v)", err)
	}
}

// TestArmKonstantenGegenDenArm ist die Drift-Sicherung gegen den Label-Arm:
// der Pipeline-Name ist die Konstante des Arms selbst, und die
// Erschöpfungs-Schwelle wird gegen das Literal in topiclabel.go geprüft
// (maxAttempts ist unexportiert und kann nicht importiert werden).
func TestArmKonstantenGegenDenArm(t *testing.T) {
	if armClusterLabel != topiclabel.Pipeline {
		t.Fatalf("armClusterLabel=%q, topiclabel.Pipeline=%q", armClusterLabel, topiclabel.Pipeline)
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "topiclabel", "topiclabel.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?maxAttempts\s*=\s*(\d+)`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatal("maxAttempts nicht in topiclabel.go gefunden — die Schwelle dieses Reports hat keinen Anker mehr")
	}
	if got := string(m[1]); got != "3" || exhaustedAttempts != 3 {
		t.Fatalf("topiclabel maxAttempts=%s, exhaustedAttempts=%d — die Erschöpfungs-Schwelle ist gedriftet",
			got, exhaustedAttempts)
	}
}

// TestPerTopicSQLForm hält die beiden Nähte fest, an denen die Negativ-Proben
// ansetzen: der LEFT JOIN steht genau einmal im fertigen Statement, und die
// Join-Regel steht in BEIDEN Statements — die Bilanz zählt nach derselben
// Regel wie die Topic-Zeilen.
func TestPerTopicSQLForm(t *testing.T) {
	if n := strings.Count(perTopicSQL, perTopicJoin); n != 1 {
		t.Fatalf("perTopicJoin steht %dx in perTopicSQL, erwartet 1x", n)
	}
	if perTopicJoin != "LEFT JOIN" {
		t.Fatalf("perTopicJoin=%q — ein Topic ohne Call fiele aus dem Report", perTopicJoin)
	}
	if n := strings.Count(perTopicSQL, topicMatchExpr); n != 1 {
		t.Fatalf("topicMatchExpr steht %dx in perTopicSQL, erwartet 1x", n)
	}
	if n := strings.Count(perTopicStatsSQL, topicMatchExpr); n != 2 {
		t.Fatalf("topicMatchExpr steht %dx in perTopicStatsSQL, erwartet 2x", n)
	}
	for _, want := range []string{"&&", "c.created_at >= t.created_at"} {
		if !strings.Contains(topicMatchExpr, want) {
			t.Fatalf("Join-Regel ohne %q: %s", want, topicMatchExpr)
		}
	}
	// Die Belegungs-Währung ist dieselbe wie im Rest des Reports.
	if !strings.Contains(perTopicCallsCTE, occupancyExpr) {
		t.Fatalf("Per-Topic-Sicht rechnet nicht mit occupancyExpr: %s", perTopicCallsCTE)
	}
	// Der Arm ist gebunden, nicht interpoliert.
	if !strings.Contains(perTopicCallsCTE, "l.pipeline = $1") {
		t.Fatalf("Arm nicht als Parameter gebunden: %s", perTopicCallsCTE)
	}
}

// TestDeriveTopicSchwelle fährt die Erschöpfungs-Ableitung an ihrer Grenze und
// die abgeleiteten Zeitgrößen.
func TestDeriveTopicSchwelle(t *testing.T) {
	until := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	built := until.Add(-50 * time.Hour)
	base := TopicCalls{CreatedAt: until.Add(-100 * time.Hour), Calls: 20, LabelBuiltAt: &built}

	for _, tc := range []struct {
		name     string
		attempts int32
		stale    bool
		want     bool
	}{
		{"stale, 2 Versuche", 2, true, false},
		{"stale, 3 Versuche", 3, true, true},
		{"stale, 4 Versuche", 4, true, true},
		{"nicht stale, 3 Versuche", 3, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.LabelAttempts, in.LabelStale = tc.attempts, tc.stale
			if got := deriveTopic(in, until).Exhausted; got != tc.want {
				t.Fatalf("Exhausted=%t, erwartet %t", got, tc.want)
			}
		})
	}

	got := deriveTopic(base, until)
	switch {
	case got.LifetimeHours != 100:
		t.Fatalf("LifetimeHours=%v, erwartet 100", got.LifetimeHours)
	case got.CallsPerHour != 0.2:
		t.Fatalf("CallsPerHour=%v, erwartet 0,2", got.CallsPerHour)
	case got.LabelAgeHours == nil || *got.LabelAgeHours != 50:
		t.Fatalf("LabelAgeHours=%v, erwartet 50", got.LabelAgeHours)
	}
	// Ein Topic ohne label_built_at bekommt kein erfundenes Alter.
	noLabel := base
	noLabel.LabelBuiltAt = nil
	if a := deriveTopic(noLabel, until).LabelAgeHours; a != nil {
		t.Fatalf("Alter ohne label_built_at = %v, erwartet nil", *a)
	}
}

// TestQuantileLinear prüft die Verteilungs-Statistik an handgerechneten Werten
// — inklusive der Nullen, die zur Grundgesamtheit gehören.
func TestQuantileLinear(t *testing.T) {
	if got := quantileLinear(nil, 0.5); got != 0 {
		t.Fatalf("leere Liste = %v, erwartet 0", got)
	}
	one := []float64{7}
	if got := quantileLinear(one, 0.95); got != 7 {
		t.Fatalf("Einzelwert = %v, erwartet 7", got)
	}
	// 0,0,0,10 → rn = 0,5·3 = 1,5 ⇒ zwischen 0 und 0 ⇒ 0; p95: rn = 2,85 ⇒
	// 0 + (10−0)·0,85 = 8,5.
	four := []float64{0, 0, 0, 10}
	if got := quantileLinear(four, 0.5); got != 0 {
		t.Fatalf("p50 = %v, erwartet 0", got)
	}
	if got := quantileLinear(four, 0.95); math.Abs(got-8.5) > 1e-9 {
		t.Fatalf("p95 = %v, erwartet 8,5", got)
	}
}

// checkPerTopicNotes ist die geteilte Prüfung der Pflicht-Notizen — als Fehler,
// damit dieselbe Prüfung gegen eine Variante ohne die Zeile gefahren werden kann.
func checkPerTopicNotes(rendered string) error {
	for _, want := range []struct{ name, needle string }{
		{"Join-Regel", "block_ids && graph_cluster_topic.core_blocks AND log.created_at >= topic.created_at"},
		{"Mehrdeutigkeit", "Sie zählen bei JEDEM Treffer"},
		{"nicht zugeordnet", "ohne block_ids — K9-Ablehnungen ohne Wire-Call"},
		{"Erschöpfungs-Schwelle", "topiclabel.go:61 maxAttempts"},
		{"kein Fix", "ist E05-5"},
	} {
		if !strings.Contains(rendered, want.needle) {
			return fmt.Errorf("Notiz %q fehlt (gesucht: %q)", want.name, want.needle)
		}
	}
	return nil
}

// TestPerTopicNotizenPflichtUndNegativprobe: die Notizen stehen in der
// gerenderten Sektion, und eine Variante ohne sie macht die Prüfung rot.
func TestPerTopicNotizenPflichtUndNegativprobe(t *testing.T) {
	pt := samplePerTopic()
	rep := sampleReport()
	rep.PerTopic = &pt
	var buf bytes.Buffer
	if err := renderTable(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if err := checkPerTopicNotes(buf.String()); err != nil {
		t.Fatalf("Pflicht-Notiz fehlt in der echten Sektion: %v", err)
	}

	variant := pt
	variant.Notes = nil
	for _, n := range pt.Notes {
		if !strings.HasPrefix(n, "Mehrdeutigkeit:") {
			variant.Notes = append(variant.Notes, n)
		}
	}
	if len(variant.Notes) != len(pt.Notes)-1 {
		t.Fatalf("Variante hat %d Notizen, erwartet %d", len(variant.Notes), len(pt.Notes)-1)
	}
	vrep := sampleReport()
	vrep.PerTopic = &variant
	var vbuf bytes.Buffer
	if err := renderTable(&vbuf, vrep); err != nil {
		t.Fatal(err)
	}
	if err := checkPerTopicNotes(vbuf.String()); err == nil {
		t.Fatal("Negativ-Probe wirkungslos: Sektion ohne Mehrdeutigkeits-Notiz besteht die Prüfung")
	}
}

// TestNullCallTopicStehtInDerTabelle ist die Render-Seite der Pflicht-Probe
// (Gate 3): ein Topic ohne einen einzigen Call steht mit 0 in der Tabelle.
func TestNullCallTopicStehtInDerTabelle(t *testing.T) {
	pt := samplePerTopic()
	rep := sampleReport()
	rep.PerTopic = &pt
	var buf bytes.Buffer
	if err := renderTable(&buf, rep); err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, "00000000-0000-0000-0000-00000000000c") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("Topic ohne Call fehlt in der Tabelle:\n%s", buf.String())
	}
	f := strings.Fields(line)
	if len(f) < 4 || f[2] != "0" {
		t.Fatalf("Topic ohne Call steht nicht mit 0 in der calls-Spalte: %q", line)
	}
	if !strings.Contains(buf.String(), "letzter_call") || !strings.Contains(line, "—") {
		t.Fatalf("Topic ohne Call braucht einen leeren letzten Call, keine erfundene Zeit: %q", line)
	}
}

// samplePerTopic ist eine handgeschriebene Sektion mit genau den Formen, auf
// die die Gates zeigen: ein erschöpftes Topic, ein Topic ohne Call und eine
// Mehrdeutigkeit.
func samplePerTopic() PerTopicReport {
	until := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	built := until.Add(-200 * time.Hour)
	last := until.Add(-2 * time.Hour)
	pt := PerTopicReport{
		Arm:               armClusterLabel,
		Since:             until.Add(-500 * time.Hour),
		Until:             until,
		JoinRule:          joinRuleText,
		ExhaustedAttempts: exhaustedAttempts,
		Topics: []TopicCalls{
			deriveTopic(TopicCalls{
				TopicID: "00000000-0000-0000-0000-00000000000a", Scope: "private", Label: "Retrieval",
				LabelSource: "llm", LabelAttempts: 0, LabelStale: false, CreatedAt: until.Add(-400 * time.Hour),
				LastSeenAt: until, LabelBuiltAt: &built, CoreN: 4, Calls: 40, CallsExact: 12,
				CallsAmbiguous: 1, OccupancySeconds: 33.5, WireSeconds: 34.0, LastCall: &last,
			}, until),
			deriveTopic(TopicCalls{
				TopicID: "00000000-0000-0000-0000-00000000000b", Scope: "private", Label: "Erschöpft",
				LabelSource: "fallback", LabelAttempts: 3, LabelStale: true, CreatedAt: until.Add(-300 * time.Hour),
				LastSeenAt: until, LabelBuiltAt: &built, CoreN: 2, Calls: 3, CallsExact: 3,
				CallsAmbiguous: 1, OccupancySeconds: 2.5, WireSeconds: 2.6, LastCall: &last,
			}, until),
			deriveTopic(TopicCalls{
				TopicID: "00000000-0000-0000-0000-00000000000c", Scope: "private", Label: "Nie gerufen",
				LabelSource: "none", LabelAttempts: 0, LabelStale: true, CreatedAt: until.Add(-10 * time.Hour),
				LastSeenAt: until, CoreN: 1,
			}, until),
		},
		Assignment: AssignStats{
			ArmRows: 45, RowsWithoutBlockIDs: 1, RowsBeforeWindow: 2,
			AssignedRows: 42, AmbiguousRows: 1, MaxTopicsPerRow: 2, UnassignedRows: 3,
			UnassignedRetiredOnly: 2,
		},
	}
	summarizePerTopic(&pt)
	return pt
}
