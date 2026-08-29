//go:build integration

// Welle C4-5 — N-15: die Retype-Einbahnstraße des Typ-Guards.
//
// BEFUND (reports/bau/c3-3-re-pilot.md §4.2 Nr. 3, §9 „Neue Auflage"): die
// X-W-Messreihen retypisieren die Insight-Blöcke des Arms auf einer Mess-Kopie
// per SQL auf einen Schatten-Typ (`session-insight-shadow`, Registry-Zeile
// `_global`/`builtin=false`, `excluded` + `shadow_measurable=true`). Der Arm
// findet seine Identität über (category, title, scope) OHNE Typ
// (distill_block.go:896-908) und verweigert danach JEDEN Lauf über diese
// Wurzel — für den Betreiber ohne Rückweg außer Archivieren oder Löschen.
//
// Diese Datei stellt genau diesen Zustand über den PRODUKTIONS-Schreibpfad her
// (der Block entsteht durch einen echten Arm-Lauf, nicht durch ein Hand-Insert)
// und misst drei Dinge: dass der Guard den Schatten-Typ abweist und es LAUT
// sagt (have/want), dass das Mess-Werkzeug die Lauffähigkeit wiederherstellt,
// und dass der Guard einen FREMDEN Typ weiterhin abweist, den das Werkzeug
// seinerseits nicht anfasst.
//
//	go test -tags=integration ./internal/events/ -run TestDistillShadowRetype -count=1 -v
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/distillreset"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	// n15ShadowType ist der Typ der belegten Mess-Praxis (X-W4-Report §11
	// Zeile f/h), n15PlainType derselbe Sichtbarkeits-Klasse OHNE die
	// Messbarkeits-Flagge — das Paar, das „Schatten-Typ" von „irgendein
	// exkludierter Typ" trennt.
	n15ShadowType = "session-insight-shadow"
	n15PlainType  = "n15-plain"
)

// n15ShadowConfig ist die Registry-Zeile der Mess-Kopie, ausgeschrieben wie in
// handler/shadow_mw2_integration_test.go: jedes Feld eine Festlegung.
const n15ShadowConfig = `{"v":1,
  "retrieval":{"policy":"excluded","shadow_measurable":true,"untrusted":true},
  "guard":{"check":false,"candidate":false},
  "dream":{"linkable":false},
  "digest":{"include":false},
  "overview":{"include":false}}`

// n15PlainConfig ist dieselbe Sichtbarkeits-Klasse OHNE shadow_measurable.
const n15PlainConfig = `{"v":1,"retrieval":{"policy":"excluded"}}`

// n15SeedTypes legt beide Registry-Zeilen über den Produktpfad an.
func n15SeedTypes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	mk := func(name, cfg string) {
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: name, Scope: store.GlobalScope, DisplayName: name,
			Config: json.RawMessage(cfg),
		}, nil, ""); err != nil {
			t.Fatalf("Registry-Zeile %s: %v", name, err)
		}
	}
	mk(n15ShadowType, n15ShadowConfig)
	mk(n15PlainType, n15PlainConfig)
}

// n15Retype ist der dokumentierte Kopie-Eingriff, wörtlich in der Form der
// Mess-Praxis: type_name wird gesetzt, Content und Metadata bleiben unberührt.
func n15Retype(t *testing.T, pool *pgxpool.Pool, from, to string) int64 {
	t.Helper()
	tag, err := pool.Exec(context.Background(), `
		UPDATE context_blocks SET type_name = $1
		 WHERE category = 'session-insights' AND scope = $2
		   AND type_name = $3 AND NOT is_archived`, to, dfScope, from)
	if err != nil {
		t.Fatalf("Retype %s → %s: %v", from, to, err)
	}
	return tag.RowsAffected()
}

// n15Types liest die Typen der Arm-Blöcke.
func n15Types(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	out := []string{}
	for _, b := range a9Blocks(t, pool) {
		out = append(out, b.typeName)
	}
	return out
}

// n15Run ist a9Run mit GEBOOTETER Typ-Registry: der Daemon fährt sie, und die
// Diagnose-Zeile des Typ-Guards liest die Messbarkeits-Flagge daraus. Ohne
// Reload bliebe die Sonde am generischen Zweig hängen und würde die
// Registry-Anbindung gar nicht prüfen.
func n15Run(t *testing.T, pool *pgxpool.Pool, src distillsource.Source) {
	t.Helper()
	stub := a8NewStub(t, a9Stub)
	s := a8Scheduler(pool, a8Config(), src, a8Pool(stub.srv.URL))
	if err := s.blocktypes.Reload(context.Background(), pool); err != nil {
		t.Fatalf("Typ-Registry laden: %v", err)
	}
	s.distillOnce(context.Background(), dfNoDemand)
}

// n15Identity ist die Schreib-Identität des Arms, wie cmd/ctx-distillreset sie
// aus derselben Konfiguration auflöst, plus das Provenienz-Etikett einer
// Mess-Kopie. Sie kommt aus dfConfig und nicht aus Konstanten: was der Arm in
// diesem Test schreibt und was das Werkzeug sucht, hat damit EINE Quelle.
func n15Identity() distillreset.Identity {
	c := dfConfig()
	scope := c.Distill.Scope
	if scope == "" {
		scope = c.Scheduler.HomeScope
	}
	return distillreset.Identity{
		InstanceKind: distillreset.InstanceKindMeasureCopy,
		Category:     c.Distill.Category,
		Scope:        scope,
		ToType:       c.Distill.BlockType,
	}
}

// n15CaptureLogs fängt slog ab, wie overview_worker_test.go es tut.
func n15CaptureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestDistillShadowRetypeReset(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key := distillSourceKey(dfLabel, dfScope, dfRoot)
	n15SeedTypes(t, pool)
	a9Truncate(t, pool)

	// 1. Der Arm schreibt seinen Block — Produktions-Schreibpfad, kein Fixture.
	n15Run(t, pool, a9Source(1, 2, 0))
	if _, outcome, errClass := a9Written(t, pool, key); outcome != distillOutcomeOk || errClass != "" {
		t.Fatalf("Lauf 1 = %q/%q, want ok — die Sonde wäre sonst gegenstandslos", outcome, errClass)
	}
	blocks := a9Blocks(t, pool)
	if len(blocks) != 1 || blocks[0].typeName != "insight" {
		t.Fatalf("nach Lauf 1: %d Blöcke, Typ %v", len(blocks), n15Types(t, pool))
	}
	carried := blocks[0].content

	// 2. Die Mess-Praxis: Retype auf den Schatten-Typ.
	if n := n15Retype(t, pool, "insight", n15ShadowType); n != 1 {
		t.Fatalf("Retype traf %d Zeilen, want 1", n)
	}

	// 2b. Der Journal-Reset der Messreihe (§4.2 Nr. 1 + 2): distill_run und
	// distill_seen zurück, die BLÖCKE bleiben stehen. Erst dadurch läuft der
	// Arm wieder über denselben Wasserzeichen-Bereich — und damit über
	// denselben Titel, also dieselbe Identität. Das ist die Lage, in der der
	// Re-Pilot stand: „für alle 16 Wurzeln" failed.
	a8Truncate(t, pool)

	// 3. N-15: der Arm ist blockiert — und muss es LAUT sagen.
	logs := n15CaptureLogs(t, func() { n15Run(t, pool, a9Source(1, 2, 0)) })
	_, outcome, errClass := a9Written(t, pool, key)
	if outcome != distillOutcomeFailed || errClass != distillErrBlockWriteFailed {
		t.Fatalf("Lauf 2 = %q/%q, want failed/block_write_failed (N-15-Zustand)", outcome, errClass)
	}
	for _, want := range []string{
		"have_type=" + n15ShadowType,
		"want_type=insight",
		"remedy=", "ctx-distillreset", "category=session-insights", "scope=" + dfScope,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("die Log-Zeile des Typ-Guards trägt %q nicht — der Fehlerfall ist still.\nGefangen: %s",
				want, logs)
		}
	}
	if rows := a8Rows(t, pool); len(rows) != 0 {
		t.Errorf("%d LLM-Calls für einen Lauf, der nie schreiben konnte", len(rows))
	}

	// 4. Das Werkzeug: erst zeigen, dann schreiben, dann nichts mehr zu tun.
	id := n15Identity()
	dry, err := distillreset.Run(ctx, pool, id, distillreset.Options{FromType: n15ShadowType})
	if err != nil {
		t.Fatalf("Trockenlauf: %v", err)
	}
	if len(dry.Rows) != 1 || dry.Applied {
		t.Fatalf("Trockenlauf = %d Zeilen, applied=%v; want 1/false", len(dry.Rows), dry.Applied)
	}
	if got := n15Types(t, pool); len(got) != 1 || got[0] != n15ShadowType {
		t.Fatalf("der Trockenlauf hat geschrieben: Typen = %v", got)
	}
	done, err := distillreset.Run(ctx, pool, id, distillreset.Options{FromType: n15ShadowType, Apply: true})
	if err != nil {
		t.Fatalf("Rücksetzen: %v", err)
	}
	if len(done.Rows) != 1 || done.Rows[0].Title != blocks[0].title || done.Rows[0].ID != blocks[0].id {
		t.Fatalf("zurückgesetzt = %+v, want genau %s/%s", done.Rows, blocks[0].id, blocks[0].title)
	}
	again, err := distillreset.Run(ctx, pool, id, distillreset.Options{FromType: n15ShadowType, Apply: true})
	if err != nil {
		t.Fatalf("zweiter Lauf: %v", err)
	}
	if len(again.Rows) != 0 {
		t.Errorf("der zweite Lauf fasste %d Zeilen an — nicht idempotent", len(again.Rows))
	}

	// 5. Der Arm läuft wieder, und der Bestand ist erhalten.
	a8ClearWindow(t, pool)
	n15Run(t, pool, a9Source(1, 2, 0))
	if _, outcome, errClass := a9Written(t, pool, key); outcome != distillOutcomeOk || errClass != "" {
		t.Fatalf("Lauf 3 = %q/%q, want ok — die Einbahnstraße ist offen geblieben", outcome, errClass)
	}
	after := a9Blocks(t, pool)
	if len(after) != 1 {
		t.Fatalf("nach dem Reset: %d Blöcke, want 1", len(after))
	}
	if after[0].typeName != "insight" {
		t.Errorf("Typ nach dem Reset = %q, want insight", after[0].typeName)
	}
	kept := 0
	for _, line := range strings.Split(carried, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		kept++
		if !strings.Contains(after[0].content, line) {
			t.Errorf("der Bestand ist verloren: %q fehlt im Block nach dem Reset", line)
		}
	}
	if kept == 0 {
		t.Fatalf("die Bestands-Sonde ist gegenstandslos — Lauf 1 hat keine Zeilen geschrieben:\n%s", carried)
	}
}

// TestDistillShadowRetypeRefusals ist die Gegenprobe zur Welle: was das
// Werkzeug NICHT darf, und was der Guard weiterhin abweist.
func TestDistillShadowRetypeRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key := distillSourceKey(dfLabel, dfScope, dfRoot)
	n15SeedTypes(t, pool)

	// FREMDTYP — der Fall, den Festlegung 4(b) meint. Er bleibt in beide
	// Richtungen abgewiesen: der Guard lässt den Arm nicht laufen, und das
	// Werkzeug fasst die Zeile nicht an.
	t.Run("ein Fremdtyp bleibt abgewiesen, und das Werkzeug fasst ihn nicht an", func(t *testing.T) {
		a9Truncate(t, pool)
		n15Run(t, pool, a9Source(1, 2, 0))
		if n := n15Retype(t, pool, "insight", n15PlainType); n != 1 {
			t.Fatalf("Retype traf %d Zeilen, want 1", n)
		}
		a8Truncate(t, pool)

		logs := n15CaptureLogs(t, func() { n15Run(t, pool, a9Source(1, 2, 0)) })
		if _, outcome, errClass := a9Written(t, pool, key); outcome != distillOutcomeFailed ||
			errClass != distillErrBlockWriteFailed {
			t.Fatalf("Lauf = %q/%q, want failed/block_write_failed — der Guard hat den Fremdtyp durchgelassen",
				outcome, errClass)
		}
		if !strings.Contains(logs, "have_type="+n15PlainType) {
			t.Errorf("die Diagnose nennt den Fremdtyp nicht:\n%s", logs)
		}
		// Und die Empfehlung zeigt NICHT auf das Retype-Werkzeug: der Typ trägt
		// keine Messbarkeits-Flagge, der Block selbst ist zu bewegen.
		if strings.Contains(logs, "remedy=measure copy") {
			t.Errorf("der Fremdtyp bekommt die Mess-Kopie-Empfehlung:\n%s", logs)
		}

		_, err := distillreset.Run(ctx, pool, n15Identity(),
			distillreset.Options{FromType: n15PlainType, Apply: true})
		if !errors.Is(err, distillreset.ErrNotShadowType) {
			t.Fatalf("Werkzeug auf Fremdtyp = %v, want ErrNotShadowType", err)
		}
		if got := n15Types(t, pool); len(got) != 1 || got[0] != n15PlainType {
			t.Errorf("die Fremdtyp-Zeile wurde angefasst: %v", got)
		}
		// Der Guard bleibt danach rot — der Fall ist nicht heilbar, sondern
		// eine Aussage über den Bestand.
		a8ClearWindow(t, pool)
		n15Run(t, pool, a9Source(1, 2, 0))
		if _, outcome, errClass := a9Written(t, pool, key); outcome != distillOutcomeFailed ||
			errClass != distillErrBlockWriteFailed {
			t.Errorf("nach dem verweigerten Werkzeuglauf = %q/%q, want weiterhin failed/block_write_failed",
				outcome, errClass)
		}
	})

	// LIVE-INSTANZ — Gate 1, vor jedem Schreibzugriff und ohne Override.
	t.Run("eine Live-Instanz wird vor jedem Schreibzugriff abgewiesen", func(t *testing.T) {
		a9Truncate(t, pool)
		n15Run(t, pool, a9Source(1, 2, 0))
		if n := n15Retype(t, pool, "insight", n15ShadowType); n != 1 {
			t.Fatalf("Retype traf %d Zeilen, want 1", n)
		}
		live := n15Identity()
		live.InstanceKind = "live"
		_, err := distillreset.Run(ctx, pool, live,
			distillreset.Options{FromType: n15ShadowType, Apply: true})
		if !errors.Is(err, distillreset.ErrNotMeasureCopy) {
			t.Fatalf("Werkzeug gegen live = %v, want ErrNotMeasureCopy", err)
		}
		if got := n15Types(t, pool); len(got) != 1 || got[0] != n15ShadowType {
			t.Errorf("gegen eine Live-Instanz wurde geschrieben: %v", got)
		}
	})

	// AUFRUFFEHLER — Quelltyp = Zieltyp ist kein Nullvorgang, sondern ein Tippfehler.
	t.Run("Quelltyp gleich Zieltyp ist ein Aufruffehler", func(t *testing.T) {
		_, err := distillreset.Run(ctx, pool, n15Identity(),
			distillreset.Options{FromType: "insight", Apply: true})
		if !errors.Is(err, distillreset.ErrIdentity) {
			t.Fatalf("Werkzeug mit Quelltyp=Zieltyp = %v, want ErrIdentity", err)
		}
	})
}
