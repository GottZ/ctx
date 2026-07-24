//go:build genmanifest

package schemacontract

// TestGenerateManifest is the generator (design/03 §4.9), not a regression
// test: `go test -tags=genmanifest ./internal/schemacontract
// -run TestGenerateManifest` starts a fresh, prod-parity Postgres, runs the
// full embedded migration chain, introspects it, and overwrites
// manifest.json in this directory with the result. It carries its own
// build tag (not `integration`) so it never runs as part of the normal
// integration suite and is never accidentally invoked by `go test
// -tags=integration ./...` — regenerating the checked-in contract is a
// deliberate, separate act tied to a migration commit (§4.9 "Commit-
// Disziplin"), not a thing that happens as a side effect of running tests.
//
// It deliberately does NOT import internal/testdb (which carries the
// `integration` tag) — see the package doc above for why the tag stays
// singular — so the ~35 lines of testcontainers bootstrap below duplicate
// testdb.SetupTestDB's shape rather than reuse it.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/GottZ/ctx/internal/store"
)

// prodImage is the locally built, prod-identical image (docker-compose.yml
// db service: `build: ./db-image` / `image: pgvector-timescaledb:pg18`).
// Design/03 §4.9's "harte Regel: Image-Parität zum Prod-Major" — the
// generator prefers this over testdb's publicly-pullable
// timescale/timescaledb-ha:pg18 specifically so the checked-in manifest
// reflects the exact extension build prod runs, not just the same PG
// major. Override via CTX_GENMANIFEST_PG_IMAGE.
const prodImage = "pgvector-timescaledb:pg18"

// fallbackImage mirrors internal/testdb's default — used only if prodImage
// cannot be started locally (not built yet), so the generator still runs
// somewhere instead of hard-failing. The deviation is recorded LOUD: in the
// manifest's own generated_against.image field AND via t.Log, per the W03-2
// brief's "vermerke die Abweichung LAUT in Manifest und Rückgabe".
const fallbackImage = "timescale/timescaledb-ha:pg18"

// hardcodedGucProbes is the Manifest's declared set of runtime GUC
// expectations (design/03 §4.3/§4.1 GucProbes field). Unlike every other
// Manifest section, this is NOT introspectable from a fresh catalog — it is
// a runtime dependency the contract FUNCTIONS declare in their own bodies
// (073_rrf_policy_params.sql:100, 074_guard_check_type_policy.sql:125-127,
// 112_rrf_gen15_dual_arm.sql's `SET LOCAL hnsw.iterative_scan =
// 'relaxed_order'`), so the generator pins it as policy-as-data here
// rather than deriving it from the catalog like every other section.
var hardcodedGucProbes = []GucProbe{
	{Name: "hnsw.iterative_scan", ProbeValue: "relaxed_order"},
}

func TestGenerateManifest(t *testing.T) {
	image := prodImage
	if v := os.Getenv("CTX_GENMANIFEST_PG_IMAGE"); v != "" {
		image = v
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, usedImage, err := startGeneratorDB(ctx, t, image)
	if err != nil {
		if image == fallbackImage {
			t.Fatalf("generator: could not start any Postgres container: %v", err)
		}
		t.Logf("generator: prod image %q unavailable (%v) — falling back to %q; "+
			"manifest.generated_against.image will record the deviation LOUD, per W03-2 brief", image, err, fallbackImage)
		pool, usedImage, err = startGeneratorDB(ctx, t, fallbackImage)
		if err != nil {
			t.Fatalf("generator: fallback image %q also failed: %v", fallbackImage, err)
		}
	}

	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("generator: run migrations: %v", err)
	}
	if err := store.BackfillChecksums(ctx, pool); err != nil {
		t.Fatalf("generator: backfill checksums: %v", err)
	}

	live, err := Introspect(ctx, pool)
	if err != nil {
		t.Fatalf("generator: introspect: %v", err)
	}

	embedded, err := embeddedMigrationFiles()
	if err != nil {
		t.Fatalf("generator: enumerate embedded migrations: %v", err)
	}
	migMax := 0
	for _, f := range embedded {
		if f.version > migMax {
			migMax = f.version
		}
	}

	extensions := make(map[string]ExtSpec, len(live.Extensions))
	for name, ext := range live.Extensions {
		extensions[name] = ExtSpec{MinVersion: ext.Version}
	}

	probes := append([]GucProbe(nil), hardcodedGucProbes...)
	sort.Slice(probes, func(i, j int) bool { return probes[i].Name < probes[j].Name })

	m := Manifest{
		ManifestVersion: 1,
		GeneratedAgainst: GeneratedAgainst{
			MigrationMax:   migMax,
			MigrationCount: len(embedded),
			PGMajor:        live.PGMajor,
			Image:          usedImage,
		},
		Extensions:  extensions,
		Tables:      live.Tables,
		Indexes:     live.Indexes,
		Functions:   live.Functions,
		Triggers:    live.Triggers,
		Rules:       live.Rules,
		Hypertables: live.Hypertables,
		GucProbes:   probes,
	}

	data, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("generator: marshal manifest: %v", err)
	}
	if err := os.WriteFile("manifest.json", data, 0o644); err != nil {
		t.Fatalf("generator: write manifest.json: %v", err)
	}

	t.Logf("generator: wrote manifest.json — migration_max=%d migration_count=%d pg_major=%d image=%s "+
		"tables=%d indexes=%d functions=%d triggers=%d rules=%d hypertables=%d extensions=%d guc_probes=%d",
		m.GeneratedAgainst.MigrationMax, m.GeneratedAgainst.MigrationCount, m.GeneratedAgainst.PGMajor, m.GeneratedAgainst.Image,
		len(m.Tables), len(m.Indexes), len(m.Functions), len(m.Triggers), len(m.Rules), len(m.Hypertables), len(m.Extensions), len(m.GucProbes))

	if len(m.Tables) == 0 || len(m.Indexes) == 0 || len(m.Functions) == 0 {
		t.Fatalf("generator: suspiciously empty manifest (tables=%d indexes=%d functions=%d) — introspection likely broken",
			len(m.Tables), len(m.Indexes), len(m.Functions))
	}

	// Self-check: the manifest we just wrote must diff clean against the DB
	// we just generated it from (Gate 1's live cousin — catches an
	// introspect/marshal asymmetry the pure nullbeweis test can't see,
	// since that test never round-trips through JSON or a real DB).
	reloaded, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("generator: reparse just-written manifest.json: %v", err)
	}
	if drifts := Diff(reloaded, live); len(drifts) != 0 {
		t.Fatalf("generator: freshly generated manifest does not diff clean against its own source DB: %+v", drifts)
	}
}

func startGeneratorDB(ctx context.Context, t *testing.T, image string) (*pgxpool.Pool, string, error) {
	t.Helper()

	container, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("ctxgen"),
		tcpostgres.WithUsername("ctxgen"),
		tcpostgres.WithPassword("ctxgen"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3*time.Minute),
		),
	)
	if err != nil {
		return nil, "", fmt.Errorf("start postgres container %q: %w", image, err)
	}
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer termCancel()
		if err := container.Terminate(termCtx); err != nil {
			t.Logf("generator: terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, "", fmt.Errorf("connection string: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, "", fmt.Errorf("connect pool: %w", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		return nil, "", fmt.Errorf("enable timescaledb: %w", err)
	}

	return pool, image, nil
}
