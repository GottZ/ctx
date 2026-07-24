package schemacontract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Querier is the minimal read surface Introspect needs — satisfied by
// *pgxpool.Pool and pgx.Tx alike, so the same introspection code runs
// unwrapped (Check's normal path) or pinned to one rolled-back transaction
// (not currently exercised by Introspect itself, but kept generic per
// design/03 §4.1's exact Introspect(ctx, q Querier) signature).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var wsRe = regexp.MustCompile(`\s+`)

// normalizeDef collapses whitespace runs to single spaces and trims, so an
// indexdef/functiondef/triggerdef hash is stable across cosmetic
// pretty-printing differences between Postgres versions.
func normalizeDef(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// Introspect reads the live public-schema catalog into the same canonical
// shape the Manifest uses, so Diff can compare them directly (design/03
// §4.1/§4.3). Catalog-only: no table data is ever read (the sole exception,
// the GUC probe's vector-cast, lives in Check, not here — design/03 §6).
func Introspect(ctx context.Context, q Querier) (LiveSnapshot, error) {
	live := LiveSnapshot{
		Extensions: map[string]LiveExtension{},
		Tables:     map[string]TableSpec{},
		Indexes:    map[string]IndexSpec{},
		Functions:  map[string]FuncSpec{},
		Triggers:   map[string]TriggerSpec{},
		Rules:      map[string]RuleSpec{},
	}

	if err := introspectExtensions(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect extensions: %w", err)
	}
	if err := introspectTables(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect tables: %w", err)
	}
	if err := introspectIndexes(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect indexes: %w", err)
	}
	if err := introspectFunctions(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect functions: %w", err)
	}
	if err := introspectTriggers(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect triggers: %w", err)
	}
	if err := introspectRules(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect rules: %w", err)
	}
	if err := introspectPolicies(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect policies: %w", err)
	}
	if err := introspectHypertables(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect hypertables: %w", err)
	}
	if err := introspectPGMajor(ctx, q, &live); err != nil {
		return live, fmt.Errorf("introspect pg_major: %w", err)
	}

	return live, nil
}

func introspectExtensions(ctx context.Context, q Querier, live *LiveSnapshot) error {
	rows, err := q.Query(ctx, `SELECT extname, extversion FROM pg_extension`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, version string
		if err := rows.Scan(&name, &version); err != nil {
			return err
		}
		live.Extensions[name] = LiveExtension{Version: version}
	}
	return rows.Err()
}

// introspectTables reads public.relkind='r' tables, their live (non-dropped)
// columns, GENERATED expressions, and row-security flag. Extension-owned
// tables (pg_depend deptype='e' — none exist in public today, but the
// filter is the documented invariant, design/03 §4.3) are excluded.
func introspectTables(ctx context.Context, q Querier, live *LiveSnapshot) error {
	const sqlTables = `
SELECT c.relname, c.relrowsecurity
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind = 'r'
   AND NOT EXISTS (
       SELECT 1 FROM pg_depend dep
        WHERE dep.objid = c.oid AND dep.deptype = 'e'
   )
 ORDER BY c.relname`
	rows, err := q.Query(ctx, sqlTables)
	if err != nil {
		return err
	}
	names := []string{}
	rowSec := map[string]bool{}
	for rows.Next() {
		var name string
		var rls bool
		if err := rows.Scan(&name, &rls); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
		rowSec[name] = rls
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	const sqlColumns = `
SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod) AS coltype,
       a.attnotnull, a.attgenerated,
       CASE WHEN a.attgenerated <> '' THEN pg_get_expr(ad.adbin, ad.adrelid) ELSE NULL END AS gen_expr,
       a.attstorage
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
  LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
 WHERE n.nspname = 'public' AND c.relkind = 'r'
   AND NOT EXISTS (
       SELECT 1 FROM pg_depend dep
        WHERE dep.objid = c.oid AND dep.deptype = 'e'
   )
 ORDER BY c.relname, a.attnum`
	crows, err := q.Query(ctx, sqlColumns)
	if err != nil {
		return err
	}
	defer crows.Close()

	cols := map[string][]ColumnSpec{}
	for crows.Next() {
		var table, colName, colType, generated, storage string
		var notNull bool
		var genExpr *string
		if err := crows.Scan(&table, &colName, &colType, &notNull, &generated, &genExpr, &storage); err != nil {
			return err
		}
		cs := ColumnSpec{Name: colName, Type: colType, NotNull: notNull, Storage: storage}
		if generated != "" && genExpr != nil {
			cs.GeneratedExprHash = sha256Hex(normalizeDef(*genExpr))
		}
		cols[table] = append(cols[table], cs)
	}
	if err := crows.Err(); err != nil {
		return err
	}

	for _, name := range names {
		live.Tables[name] = TableSpec{
			Columns:     cols[name],
			RowSecurity: rowSec[name],
		}
	}
	return nil
}

// introspectIndexes reads public indexes plus their reloptions. TimescaleDB
// chunk indexes live in _timescaledb_internal and never appear here (schema
// filter alone excludes them — design/03 §2 live re-verification).
func introspectIndexes(ctx context.Context, q Querier, live *LiveSnapshot) error {
	const sqlIdx = `
SELECT c.relname, pgi.indexdef, c.reloptions
  FROM pg_indexes pgi
  JOIN pg_namespace n ON n.nspname = pgi.schemaname
  JOIN pg_class c ON c.relname = pgi.indexname AND c.relnamespace = n.oid
 WHERE pgi.schemaname = 'public'
   AND NOT EXISTS (
       SELECT 1 FROM pg_depend dep
        WHERE dep.objid = c.oid AND dep.deptype = 'e'
   )
 ORDER BY c.relname`
	rows, err := q.Query(ctx, sqlIdx)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, def string
		var relopts []string
		if err := rows.Scan(&name, &def, &relopts); err != nil {
			return err
		}
		live.Indexes[name] = IndexSpec{
			DefHash:    sha256Hex(normalizeDef(def)),
			RelOptions: parseRelOptions(relopts),
		}
	}
	return rows.Err()
}

func parseRelOptions(opts []string) map[string]string {
	if len(opts) == 0 {
		return nil
	}
	m := make(map[string]string, len(opts))
	for _, o := range opts {
		parts := strings.SplitN(o, "=", 2)
		if len(parts) != 2 {
			continue
		}
		m[parts[0]] = parts[1]
	}
	return m
}

// introspectFunctions excludes extension-member functions (pg_depend
// deptype='e') so pgcrypto/vector/pg_trgm helper functions never masquerade
// as ctx-owned contract functions (design/03 §4.3).
func introspectFunctions(ctx context.Context, q Querier, live *LiveSnapshot) error {
	const sqlFn = `
SELECT p.proname, pg_get_function_identity_arguments(p.oid) AS args, pg_get_functiondef(p.oid) AS def
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'public'
   AND NOT EXISTS (
       SELECT 1 FROM pg_depend dep
        WHERE dep.objid = p.oid AND dep.deptype = 'e'
   )
 ORDER BY p.proname, args`
	rows, err := q.Query(ctx, sqlFn)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, args, def string
		if err := rows.Scan(&name, &args, &def); err != nil {
			return err
		}
		key := fmt.Sprintf("%s(%s)", name, args)
		live.Functions[key] = FuncSpec{SrcHash: sha256Hex(normalizeDef(def))}
	}
	return rows.Err()
}

func introspectTriggers(ctx context.Context, q Querier, live *LiveSnapshot) error {
	const sqlTrig = `
SELECT c.relname, t.tgname, pg_get_triggerdef(t.oid) AS def
  FROM pg_trigger t
  JOIN pg_class c ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND NOT t.tgisinternal
 ORDER BY c.relname, t.tgname`
	rows, err := q.Query(ctx, sqlTrig)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, def string
		if err := rows.Scan(&table, &name, &def); err != nil {
			return err
		}
		key := table + "." + name
		live.Triggers[key] = TriggerSpec{DefHash: sha256Hex(normalizeDef(def))}
	}
	return rows.Err()
}

// introspectRules reads pg_rules, which already excludes the implicit
// `_RETURN` view rules (design/03 §4.3).
func introspectRules(ctx context.Context, q Querier, live *LiveSnapshot) error {
	const sqlRules = `
SELECT tablename, rulename, definition
  FROM pg_rules
 WHERE schemaname = 'public'
 ORDER BY tablename, rulename`
	rows, err := q.Query(ctx, sqlRules)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, def string
		if err := rows.Scan(&table, &name, &def); err != nil {
			return err
		}
		key := table + "." + name
		live.Rules[key] = RuleSpec{DefHash: sha256Hex(normalizeDef(def))}
	}
	return rows.Err()
}

// introspectPolicies reads pg_policy on public tables. The Manifest never
// declares policies (design/03 §4.3: expected empty) — every row found here
// becomes an unconditional unknown_active_object drift in Diff.
func introspectPolicies(ctx context.Context, q Querier, live *LiveSnapshot) error {
	const sqlPol = `
SELECT c.relname, pol.polname
  FROM pg_policy pol
  JOIN pg_class c ON c.oid = pol.polrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
 ORDER BY c.relname, pol.polname`
	rows, err := q.Query(ctx, sqlPol)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			return err
		}
		live.Policies = append(live.Policies, table+"."+name)
	}
	return rows.Err()
}

func introspectHypertables(ctx context.Context, q Querier, live *LiveSnapshot) error {
	const sqlHt = `SELECT hypertable_name FROM timescaledb_information.hypertables ORDER BY hypertable_name`
	rows, err := q.Query(ctx, sqlHt)
	if err != nil {
		return err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Strings(names)
	live.Hypertables = names
	return nil
}

func introspectPGMajor(ctx context.Context, q Querier, live *LiveSnapshot) error {
	var versionNum string
	if err := q.QueryRow(ctx, `SHOW server_version_num`).Scan(&versionNum); err != nil {
		return err
	}
	n, err := strconv.Atoi(versionNum)
	if err != nil {
		return fmt.Errorf("parsing server_version_num %q: %w", versionNum, err)
	}
	live.PGMajor = n / 10000
	return nil
}
