//go:build integration

// Die Gates, die eine Datenbank brauchen: die Typ-Registry (Gate 2 + 3) und die
// Auswahl des Identitätsraums.
//
//	go test -tags=integration ./internal/distillreset/ -count=1 -v
package distillreset

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	drShadow   = "dr-shadow"
	drPlain    = "dr-plain"
	drCategory = "session-insights"
	drScope    = "private"
)

func drIdentity() Identity {
	return Identity{
		InstanceKind: InstanceKindMeasureCopy,
		Category:     drCategory,
		Scope:        drScope,
		ToType:       "insight",
	}
}

func drSeedTypes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	mk := func(name, cfg string) {
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: name, Scope: store.GlobalScope, DisplayName: name, Config: json.RawMessage(cfg),
		}, nil, ""); err != nil {
			t.Fatalf("Registry-Zeile %s: %v", name, err)
		}
	}
	mk(drShadow, `{"v":1,"retrieval":{"policy":"excluded","shadow_measurable":true}}`)
	mk(drPlain, `{"v":1,"retrieval":{"policy":"excluded"}}`)
}

// drBlock legt einen Block im Identitätsraum an und gibt seine Id zurück.
func drBlock(t *testing.T, pool *pgxpool.Pool, title, typeName string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, drCategory, title, "inhalt",
		nil, map[string]any{}, drScope, true,
		store.SensitivityWrite{Value: backends.SensPublic, Manual: true}, typeName)
	if err != nil {
		t.Fatalf("Block %s: %v", title, err)
	}
	return b.ID
}

// drTypeOf liest den Typ einer Zeile.
func drTypeOf(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT type_name FROM context_blocks WHERE id = $1::uuid`, id).Scan(&got); err != nil {
		t.Fatalf("Typ lesen: %v", err)
	}
	return got
}

func TestRunSelectsOnlyTheArmsIdentitySpace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	drSeedTypes(t, pool)

	mine := drBlock(t, pool, "Destillat aus Compaction a", drShadow)
	otherCat := drBlock(t, pool, "Destillat aus Compaction b", drShadow)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET category = 'andere-kategorie' WHERE id = $1::uuid`, otherCat); err != nil {
		t.Fatalf("Nachbar-Kategorie: %v", err)
	}
	otherScope := drBlock(t, pool, "Destillat aus Compaction c", drShadow)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET scope = 'anderer-scope' WHERE id = $1::uuid`, otherScope); err != nil {
		t.Fatalf("Nachbar-Scope: %v", err)
	}
	archived := drBlock(t, pool, "Destillat aus Compaction d", drShadow)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, archived); err != nil {
		t.Fatalf("archivieren: %v", err)
	}
	// Ein ABGELEITETER Block im selben Raum. Die Provenienz wird hier per SQL
	// gesetzt: das Prädikat des Werkzeugs ist die ANWESENHEIT des Schlüssels,
	// nicht sein Inhalt, und der v=1-Vertrag eines echten Derivats würde die
	// Sonde nur verlängern, ohne sie schärfer zu machen.
	derivedID := drBlock(t, pool, "Destillat aus Compaction e", drShadow)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET metadata = metadata || '{"provenance":{"v":1,"arm":"fremd"}}'::jsonb
		  WHERE id = $1::uuid`, derivedID); err != nil {
		t.Fatalf("Provenienz setzen: %v", err)
	}

	res, err := Run(ctx, pool, drIdentity(), Options{FromType: drShadow, Apply: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].ID != mine {
		t.Fatalf("zurückgesetzt = %+v, want genau %s", res.Rows, mine)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].ID != derivedID {
		t.Fatalf("übersprungen = %+v, want genau den abgeleiteten Block %s", res.Skipped, derivedID)
	}
	if got := drTypeOf(t, pool, mine); got != "insight" {
		t.Errorf("eigener Block = %q, want insight", got)
	}
	for name, id := range map[string]string{
		"Nachbar-Kategorie": otherCat, "Nachbar-Scope": otherScope,
		"archiviert": archived, "abgeleitet": derivedID,
	} {
		if got := drTypeOf(t, pool, id); got != drShadow {
			t.Errorf("%s wurde angefasst: Typ = %q", name, got)
		}
	}
}

func TestRunRefusesTypesTheRegistryDoesNotBack(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	drSeedTypes(t, pool)
	id := drBlock(t, pool, "Destillat aus Compaction a", drPlain)

	// (a) ein registrierter Typ OHNE Messbarkeits-Flagge.
	if _, err := Run(ctx, pool, drIdentity(), Options{FromType: drPlain, Apply: true}); !errors.Is(err, ErrNotShadowType) {
		t.Fatalf("Run auf %s = %v, want ErrNotShadowType", drPlain, err)
	}
	// (b) ein Name, den die Registry gar nicht kennt — dieselbe Antwort, weil
	// IsShadowMeasurable fail-closed ist.
	if _, err := Run(ctx, pool, drIdentity(), Options{FromType: "gibt-es-nicht", Apply: true}); !errors.Is(err, ErrNotShadowType) {
		t.Fatalf("Run auf einen unbekannten Typ = %v, want ErrNotShadowType", err)
	}
	// (c) ein ZIELtyp, den die Registry nicht kennt: der Rücksetzer darf keinen
	// unregistrierten Typ in den Bestand schreiben.
	id2 := drIdentity()
	id2.ToType = "auch-nicht"
	if _, err := Run(ctx, pool, id2, Options{FromType: drShadow, Apply: true}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("Run mit unbekanntem Zieltyp = %v, want ErrIdentity", err)
	}
	if got := drTypeOf(t, pool, id); got != drPlain {
		t.Errorf("trotz Verweigerung geschrieben: %q", got)
	}
}
