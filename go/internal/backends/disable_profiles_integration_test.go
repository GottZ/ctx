//go:build integration

// Integrationstest für Web-UX Achse U01-W1 (Migration 092 + Pool.Reload).
// Pinnt: (1) die zwei neuen Tabellen laden über Pool.Reload in den EINEN
// atomaren Snapshot (Profiles + disabledBy), (2) Idempotenz der 092
// (zweiter RunMigrations = no-op, ein _migrations-Eintrag), (3) der Backfill
// legt aus einer gaming.active=true-Settings-Row + zwei passenden Backends das
// Profil 'eject' aktiv mit 2 Membern, reserved=true, scope='_global' an,
// (4) diffKey-Stabilität: zwei Reloads ohne Änderung ⇒ byte-identische
// Profil-Slice-Ordnung.
//
// Externes Test-Paket (backends_test): internal/testdb importiert
// internal/store → internal/backends, ein internes Test würde zyklen.
// NewPool/Reload/Profiles/DisabledBy sind exportiert.
//
// Lauf-Kommando (der Lead fährt es — braucht Docker + PG-Image):
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/backends/ \
//	  -run 'TestDisableProfiles' -count=1 -v
package backends_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// readEmbedded092 liest das reale eingebettete Migrations-File (Fixture-
// Kollusions-Guard: keine test-lokale SQL-Kopie).
func readEmbedded092(t *testing.T) (string, error) {
	t.Helper()
	b, err := migrations.FS.ReadFile("092_disable_profiles.sql")
	return string(b), err
}

// insertGlobalBackend legt ein '_global'-Backend an und gibt seine id zurück.
func insertGlobalBackend(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_backends (name, base_url, scope) VALUES ($1, $2, '_global') RETURNING id`,
		name, "http://"+name).Scan(&id); err != nil {
		t.Fatalf("insert backend %s: %v", name, err)
	}
	return id
}

// TestDisableProfilesSchemaAndReload: die Tabellen existieren nach 092, ein
// aktives Profil mit Membern landet über Reload im Snapshot (Profiles +
// disabledBy), und 092 ist idempotent.
func TestDisableProfilesSchemaAndReload(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Tabellen präsent.
	for _, tbl := range []string{"context_disable_profiles", "context_disable_profile_backends"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)`, tbl).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if !exists {
			t.Fatalf("migration 092 legte Tabelle %s nicht an", tbl)
		}
	}

	// UNIQUE ist (scope, name) — AM-5.
	var hasUnique bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='uq_disable_profiles_scope_name')`).Scan(&hasUnique); err != nil {
		t.Fatalf("check unique: %v", err)
	}
	if !hasUnique {
		t.Error("uq_disable_profiles_scope_name fehlt (AM-5 (scope,name))")
	}

	// Ein aktives Profil + zwei Backends als Member.
	b1 := insertGlobalBackend(t, pool, "gpu-a")
	b2 := insertGlobalBackend(t, pool, "gpu-b")
	var pid string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_disable_profiles (scope, name, label, active) VALUES ('_global','wartung','GPU-Wartung',true) RETURNING id`).Scan(&pid); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	for _, bid := range []string{b1, b2} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_disable_profile_backends (profile_id, backend_id) VALUES ($1,$2)`, pid, bid); err != nil {
			t.Fatalf("insert membership: %v", err)
		}
	}

	p := backends.NewPool(pool, nil)
	if err := p.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Der Snapshot trägt das Profil.
	var found *backends.Profile
	for i := range p.Profiles() {
		if p.Profiles()[i].Name == "wartung" {
			pr := p.Profiles()[i]
			found = &pr
		}
	}
	if found == nil {
		t.Fatalf("Profil 'wartung' fehlt im Snapshot: %+v", p.Profiles())
	}
	if !found.Active || found.Label != "GPU-Wartung" || found.Scope != "_global" {
		t.Fatalf("Profil-Felder falsch: %+v", *found)
	}

	// disabledBy: beide Backends tragen den aktiven Profil-Namen.
	db := p.DisabledBy()
	if db[b1] != "wartung" || db[b2] != "wartung" {
		t.Fatalf("disabledBy = %v, want beide auf 'wartung'", db)
	}

	// Idempotenz: zweiter RunMigrations = no-op, genau ein _migrations-Eintrag.
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("zweiter RunMigrations: %v", err)
	}
	var cnt int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version=92`).Scan(&cnt); err != nil {
		t.Fatalf("count 92: %v", err)
	}
	if cnt != 1 {
		t.Errorf("_migrations version 92 = %d Zeilen, want 1 (idempotent)", cnt)
	}
}

// TestDisableProfilesBackfill: eine gaming.active=true-Settings-Row + zwei
// passende '_global'-Backends ⇒ Profil 'eject' aktiv, reserved=true,
// scope='_global', 2 Member.
func TestDisableProfilesBackfill(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Fixture: die zwei Default-Backends + die gaming.active-Settings-Row.
	insertGlobalBackend(t, pool, "herbert-chat")
	insertGlobalBackend(t, pool, "herbert-rerank")
	trueJSON, _ := json.Marshal(true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_settings (key, scope, value) VALUES ('gaming.active','_global',$1)
		   ON CONFLICT (key, scope) DO UPDATE SET value=EXCLUDED.value`, trueJSON); err != nil {
		t.Fatalf("insert gaming.active: %v", err)
	}

	// Backfill NACH der Fixture erneut fahren (092 ist nach RunMigrations schon
	// gelaufen, damals ohne Backends → 0 Member; das WHERE-NOT-EXISTS-Gate
	// überspränge den Zweitlauf). Für die Probe: das evtl. leere eject-Profil
	// entfernen, dann das reale eingebettete 092 erneut anwenden (Fixture-
	// Kollusions-Guard: kein test-lokales SQL, das echte File).
	if _, err := pool.Exec(ctx, `DELETE FROM context_disable_profiles WHERE name='eject'`); err != nil {
		t.Fatalf("cleanup eject: %v", err)
	}
	sqlBytes, err := readEmbedded092(t)
	if err != nil {
		t.Fatalf("read 092: %v", err)
	}
	if _, err := pool.Exec(ctx, sqlBytes); err != nil {
		t.Fatalf("re-apply 092 backfill: %v", err)
	}

	var (
		active, reserved bool
		scope, label     string
		members          int
	)
	if err := pool.QueryRow(ctx,
		`SELECT p.active, p.reserved, p.scope, p.label,
		        (SELECT count(*) FROM context_disable_profile_backends m WHERE m.profile_id=p.id)
		   FROM context_disable_profiles p WHERE p.name='eject'`).Scan(
		&active, &reserved, &scope, &label, &members); err != nil {
		t.Fatalf("read eject profile: %v", err)
	}
	if !active {
		t.Error("eject.active = false, want true (gaming.active=true kopiert)")
	}
	if !reserved {
		t.Error("eject.reserved = false, want true (Break-Glass-Schutz)")
	}
	if scope != "_global" {
		t.Errorf("eject.scope = %q, want _global", scope)
	}
	if label != "Eject-Modus" {
		t.Errorf("eject.label = %q, want Eject-Modus (AM-7)", label)
	}
	if members != 2 {
		t.Errorf("eject Member = %d, want 2 (herbert-chat, herbert-rerank)", members)
	}

	// Zweiter Backfill-Lauf ist idempotent (WHERE NOT EXISTS): kein Fehler,
	// weiterhin genau ein eject-Profil.
	if _, err := pool.Exec(ctx, sqlBytes); err != nil {
		t.Fatalf("zweiter 092-Lauf: %v", err)
	}
	var ejectCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_disable_profiles WHERE name='eject'`).Scan(&ejectCount); err != nil {
		t.Fatalf("count eject: %v", err)
	}
	if ejectCount != 1 {
		t.Errorf("eject-Profile = %d, want 1 (Backfill idempotent)", ejectCount)
	}
}

// TestDisableProfilesReloadOrderStable: zwei aufeinanderfolgende Reloads ohne
// Änderung ⇒ byte-identische Profil-Slice-Ordnung (ORDER BY name, nicht
// Map-Iteration).
func TestDisableProfilesReloadOrderStable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Namen absichtlich nicht-alphabetisch eingefügt.
	for _, n := range []string{"zulu", "alpha", "mike", "bravo"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_disable_profiles (scope, name) VALUES ('_global',$1)`, n); err != nil {
			t.Fatalf("insert profile %s: %v", n, err)
		}
	}

	p := backends.NewPool(pool, nil)
	if err := p.Reload(ctx); err != nil {
		t.Fatalf("reload 1: %v", err)
	}
	first := profileNames(p.Profiles())
	if err := p.Reload(ctx); err != nil {
		t.Fatalf("reload 2: %v", err)
	}
	second := profileNames(p.Profiles())

	if first != second {
		t.Fatalf("Profil-Ordnung instabil zwischen Reloads: %q vs %q", first, second)
	}
	// ORDER BY name: alphabetisch (eject aus dem RunMigrations-Backfill ist auch
	// dabei — der Test prüft die relative Sortierung der eingefügten Namen).
	want := "alpha,bravo,eject,mike,zulu"
	if first != want {
		t.Fatalf("Profil-Ordnung = %q, want %q (ORDER BY name)", first, want)
	}
}

func profileNames(ps []backends.Profile) string {
	out := ""
	for i, p := range ps {
		if i > 0 {
			out += ","
		}
		out += p.Name
	}
	return out
}
