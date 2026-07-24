package schemacontract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/migrations"
)

// Check runs Introspect + Diff + migration-integrity + GUC probes in one
// call (design/03 §4.1 — VERBINDLICH signature; "eine Quelle" meant to be
// shared by boot, the re-check ticker, /api/contract and the CLI — none of
// which exist yet in W03-2, but the contract holds regardless of caller).
//
// A non-nil error means the check itself could not run at all (fail-closed
// per design/03 §4.4's last row: a prüfer, der nicht laufen kann, darf
// nicht "ok" implizieren) — Status is set to unchecked in that case too, so
// callers never need to inspect the error to know the report isn't
// trustworthy. Expected, self-describing schema conditions (a pre-108
// database missing the checksum column, a version gap, a failed GUC probe)
// are never Go errors — they become Drift entries with Status=drift.
func Check(ctx context.Context, pool *pgxpool.Pool) (Report, error) {
	m := Embedded()

	report := Report{
		CheckedAt:   time.Now().UTC(),
		ManifestMax: m.GeneratedAgainst.MigrationMax,
		Mode:        DefaultMode,
		ModeSource:  DefaultModeSource,
	}

	live, err := Introspect(ctx, pool)
	if err != nil {
		report.Status = StatusUnchecked
		return report, fmt.Errorf("introspect: %w", err)
	}

	// Image-parity guard (design/03 §4.9): pg_get_functiondef/pg_get_expr
	// deparse can differ across Postgres majors — a mismatch here would
	// otherwise manufacture Schein-Drifts on every function/expression.
	if m.GeneratedAgainst.PGMajor != 0 && live.PGMajor != m.GeneratedAgainst.PGMajor {
		report.Status = StatusUnchecked
		report.Drifts = []Drift{{
			Class: ClassMigrationIntegrity, Severity: SeverityBreaking,
			Object: "pg_major",
			Detail: fmt.Sprintf("manifest generated against PG%d, live is PG%d — deparse may differ, refusing to report schema drifts",
				m.GeneratedAgainst.PGMajor, live.PGMajor),
		}}
		return report, nil
	}

	drifts, excluded := diffDetailed(m, live)
	report.ExcludedSnapshotTables = excluded

	miDrifts, liveMax, err := checkMigrationIntegrity(ctx, pool)
	if err != nil {
		report.Status = StatusUnchecked
		return report, fmt.Errorf("migration integrity: %w", err)
	}
	report.LiveMax = liveMax
	drifts = append(drifts, miDrifts...)

	gucDrifts, err := checkGUCProbes(ctx, pool, m.GucProbes)
	if err != nil {
		report.Status = StatusUnchecked
		return report, fmt.Errorf("guc probes: %w", err)
	}
	drifts = append(drifts, gucDrifts...)

	sort.Slice(drifts, func(i, j int) bool {
		if drifts[i].Class != drifts[j].Class {
			return drifts[i].Class < drifts[j].Class
		}
		return drifts[i].Object < drifts[j].Object
	})
	report.Drifts = drifts

	if len(drifts) == 0 {
		report.Status = StatusOK
	} else {
		report.Status = StatusDrift
	}
	return report, nil
}

// migFile is one embedded migration file's identity + content checksum.
type migFile struct {
	version  int
	filename string
	checksum string
}

// embeddedMigrationFiles parses the embedded migrations FS the same way
// store.RunMigrations does (numeric prefix before the first underscore).
// Deliberately duplicated rather than imported from internal/store: this
// package has no other reason to depend on store, and the parse rule is
// three lines — re-deriving it here keeps schemacontract's only coupling to
// the migrations package itself (design/03 §3.2's "the vertrag muss
// außerhalb des Prüflings leben" principle extends to build-time deps too).
func embeddedMigrationFiles() ([]migFile, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}
	var out []migFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		content, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return nil, fmt.Errorf("reading migration %s: %w", name, err)
		}
		sum := sha256.Sum256(content)
		out = append(out, migFile{version: v, filename: name, checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

type migRow struct {
	filename string
	checksum *string
}

// checkMigrationIntegrity compares the embedded migration chain against
// _migrations (design/03 §4.2). It tolerates a pre-108 database (checksum
// column absent) by degrading to a filename-only read instead of erroring —
// the missing column becomes its own Drift, exactly the condition W03-2's
// Live-Gate exercises against the real DB (still on migration 107).
func checkMigrationIntegrity(ctx context.Context, pool *pgxpool.Pool) ([]Drift, int, error) {
	var drifts []Drift

	var checksumColExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='_migrations' AND column_name='checksum')`,
	).Scan(&checksumColExists); err != nil {
		return nil, 0, fmt.Errorf("probing _migrations.checksum column: %w", err)
	}

	dbByVersion := map[int]migRow{}
	liveMax := 0

	readInto := func(sql string, withChecksum bool) error {
		rows, err := pool.Query(ctx, sql)
		if err != nil {
			return fmt.Errorf("reading _migrations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v int
			var filename string
			var checksum *string
			if withChecksum {
				if err := rows.Scan(&v, &filename, &checksum); err != nil {
					return fmt.Errorf("scanning _migrations row: %w", err)
				}
			} else {
				if err := rows.Scan(&v, &filename); err != nil {
					return fmt.Errorf("scanning _migrations row (pre-108 shape): %w", err)
				}
			}
			dbByVersion[v] = migRow{filename: filename, checksum: checksum}
			if v > liveMax {
				liveMax = v
			}
		}
		return rows.Err()
	}

	if checksumColExists {
		if err := readInto(`SELECT version, filename, checksum FROM _migrations ORDER BY version`, true); err != nil {
			return nil, 0, err
		}
	} else {
		drifts = append(drifts, Drift{
			Class: ClassMigrationIntegrity, Severity: SeverityBreaking,
			Object: "_migrations.checksum",
			Detail: "column missing — DB predates migration 108 (W03-1)",
		})
		if err := readInto(`SELECT version, filename FROM _migrations ORDER BY version`, false); err != nil {
			return nil, 0, err
		}
	}

	embedded, err := embeddedMigrationFiles()
	if err != nil {
		return nil, 0, err
	}
	embeddedByVersion := make(map[int]migFile, len(embedded))
	for _, f := range embedded {
		embeddedByVersion[f.version] = f
	}

	for _, f := range embedded {
		row, ok := dbByVersion[f.version]
		if !ok {
			drifts = append(drifts, Drift{
				Class: ClassMigrationIntegrity, Severity: SeverityBreaking,
				Object: fmt.Sprintf("_migrations:%d", f.version),
				Detail: fmt.Sprintf("%s embedded but not applied (version gap)", f.filename),
			})
			continue
		}
		if !checksumColExists {
			continue // already flagged once above; no per-row checksum to compare
		}
		if row.checksum == nil {
			drifts = append(drifts, Drift{
				Class: ClassMigrationIntegrity, Severity: SeverityBreaking,
				Object: fmt.Sprintf("_migrations:%d", f.version),
				Detail: "checksum not backfilled",
			})
			continue
		}
		if *row.checksum != f.checksum {
			drifts = append(drifts, Drift{
				Class: ClassMigrationIntegrity, Severity: SeverityBreaking,
				Object: fmt.Sprintf("_migrations:%d", f.version),
				Detail: "checksum mismatch — applied file differs from embedded file",
			})
		}
	}

	for v, row := range dbByVersion {
		if _, ok := embeddedByVersion[v]; !ok {
			drifts = append(drifts, Drift{
				Class: ClassMigrationIntegrity, Severity: SeverityBreaking,
				Object: fmt.Sprintf("_migrations:%d", v),
				Detail: fmt.Sprintf("recorded as %s, no matching embedded file", row.filename),
			})
		}
	}

	return drifts, liveMax, nil
}

// checkGUCProbes runs the three-step library-load + set_config +
// pg_settings probe (design/03 §4.3) for every Manifest-declared GUC, each
// in its own rolled-back transaction (a probe failure must never leak
// transaction-local state, and each probe needs a single pinned connection
// — library-load and set_config must land on the SAME backend).
func checkGUCProbes(ctx context.Context, pool *pgxpool.Pool, probes []GucProbe) ([]Drift, error) {
	var drifts []Drift
	for _, p := range probes {
		d, err := probeOneGUC(ctx, pool, p)
		if err != nil {
			return nil, err
		}
		if d != nil {
			drifts = append(drifts, *d)
		}
	}
	return drifts, nil
}

func probeOneGUC(ctx context.Context, pool *pgxpool.Pool, p GucProbe) (*Drift, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin guc probe tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 1: load the pgvector library into THIS backend (a cheap cast,
	// cast to ::text so no pgvector Go-type registration is required on the
	// caller's pool — design/03 §4.3 "billig, katalog-nah", no table read).
	if _, err := tx.Exec(ctx, `SELECT ('[0]'::vector)::text`); err != nil {
		return &Drift{Class: ClassGucProbeFailed, Severity: SeverityBreaking,
			Object: "guc:" + p.Name, Detail: "library load failed: " + err.Error()}, nil
	}

	// Step 2: set the exact production value, transaction-local only.
	if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, p.Name, p.ProbeValue); err != nil {
		return &Drift{Class: ClassGucProbeFailed, Severity: SeverityBreaking,
			Object: "guc:" + p.Name, Detail: "set_config failed: " + err.Error()}, nil
	}

	// Step 3: assert the backend now recognizes the GUC.
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_settings WHERE name = $1`, p.Name).Scan(&count); err != nil {
		return &Drift{Class: ClassGucProbeFailed, Severity: SeverityBreaking,
			Object: "guc:" + p.Name, Detail: "pg_settings assert query failed: " + err.Error()}, nil
	}
	if count != 1 {
		return &Drift{Class: ClassGucProbeFailed, Severity: SeverityBreaking,
			Object: "guc:" + p.Name, Detail: fmt.Sprintf("pg_settings count=%d, want 1", count)}, nil
	}
	return nil, nil
}
