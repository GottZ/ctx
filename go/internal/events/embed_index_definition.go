package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Issue #312: a valid runtime index of the expected name is reused only when
// its definition is the one its DDL builds. The runtime indexes of the
// re-embed migration (the pending index, the _next HNSW twin and the _next
// sister indexes) are built by ensureConcurrentIndexValid from DDL constants
// of this package; a release can change such a constant while a migration
// is in flight, and the index built by the older release would otherwise be
// reused and, for the _next twins, renamed onto the serving name by the
// cutover.
//
// The expected definition is not a second copy of the DDL. It is what the
// connected server makes of the DDL itself: the statement is run against an
// empty copy of context_blocks (CREATE TABLE ... (LIKE context_blocks))
// inside a transaction that is always rolled back, and pg_get_indexdef of
// that index is compared with pg_get_indexdef of the live one, both without
// the index and table names. Both deparses come from the same server, the
// same session and the same search_path, so a Postgres or pgvector version
// whose deparse differs from the one a test ran on cannot make a correct
// index look different (a false difference drops and rebuilds the HNSW
// twin, hours at 10M vectors); and a DDL edited in a release cannot be
// compared against a stale expectation, because there is none to edit.
//
// The copy is an ordinary table, not a TEMP one: an HNSW build on a TEMP
// table initializes the session's local buffers, after which PostgreSQL
// refuses a change of temp_buffers on that pooled connection (SQLSTATE
// 22023), which the overview persist sets as the first statement of its
// transaction. The rolled-back table never becomes visible to another
// session; its files are removed at the rollback.

// Outcomes of ensureConcurrentIndexValid, as the verify report's index
// entries name them (verifyIndexState.Action).
const (
	indexActionReused  = "reused"
	indexActionCreated = "created"
	// indexActionRebuiltInvalid: an INVALID leftover of a crashed or canceled
	// CREATE INDEX CONCURRENTLY was dropped and built again.
	indexActionRebuiltInvalid = "rebuilt_invalid"
	// indexActionRebuiltDefinition: a valid index of another definition than
	// its DDL builds was dropped and built again.
	indexActionRebuiltDefinition = "rebuilt_definition_mismatch"
)

// managedIndexDDLPrefix is how every DDL constant ensureConcurrentIndexValid
// takes begins, followed by the index name and ON context_blocks;
// indexProbeStatement refuses a DDL of another shape.
const managedIndexDDLPrefix = "CREATE INDEX CONCURRENTLY IF NOT EXISTS "

// errIndexDefinitionUndetermined marks a comparison that could not be made:
// the index is neither reused nor rebuilt.
var errIndexDefinitionUndetermined = errors.New("index definition could not be compared with its DDL")

// runtimeIndexState is the catalog view of one runtime index on
// context_blocks: whether it exists, pg_index.indisvalid, its
// pg_get_indexdef, and the schema it lives in (the schema of
// context_blocks), which a DROP names instead of resolving the bare index
// name through the session's search_path.
type runtimeIndexState struct {
	exists bool
	valid  bool
	def    string
	schema string
}

// qualifiedName is the index as a quoted schema-qualified identifier.
func (st runtimeIndexState) qualifiedName(name string) string {
	return pgx.Identifier{st.schema, name}.Sanitize()
}

// runtimeIndexReadAttempts bounds readRuntimeIndex's re-reads after a
// concurrent drop. Fixed, not a setting: the first re-read already runs on a
// snapshot after the drop's commit, the bound only stops a genuine internal
// error from looping.
const runtimeIndexReadAttempts = 3

// sqlstateInternalError is internal_error, the SQLSTATE of PostgreSQL's
// "cache lookup failed" errors.
const sqlstateInternalError = "XX000"

// readRuntimeIndex reads the index name on context_blocks (the table every
// managed DDL names; an index of that name on another table is not the one
// the DDL would collide with). One catalog query, which the validity probe
// was before issue #312; pg_get_indexdef rides it.
//
// pg_get_indexdef reads the system caches, not the statement's snapshot:
// when another session's DROP INDEX (CONCURRENTLY) commits between the scan
// that found the index and the deparse, the deparse fails with "cache lookup
// failed" (SQLSTATE XX000, internal_error). That is the index vanishing, not
// a broken catalog, so the read is repeated on a fresh snapshot, which no
// longer finds it.
func readRuntimeIndex(ctx context.Context, q pgxdb.Rower, name string) (runtimeIndexState, error) {
	var err error
	for attempt := 1; ; attempt++ {
		var st runtimeIndexState
		var def *string
		err = q.QueryRow(ctx,
			`SELECT i.indisvalid, pg_get_indexdef(i.indexrelid), n.nspname FROM pg_index i
			 JOIN pg_class c ON c.oid = i.indexrelid
			 JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.relname = $1 AND i.indrelid = 'context_blocks'::regclass`, name).Scan(&st.valid, &def, &st.schema)
		if errors.Is(err, pgx.ErrNoRows) {
			return st, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateInternalError && attempt < runtimeIndexReadAttempts {
			continue
		}
		if err != nil {
			return st, err
		}
		st.exists = true
		if def != nil {
			st.def = *def
		}
		return st, nil
	}
}

// comparableIndexDefinition is a pg_get_indexdef output without the index
// and table names: "USING <method> (<keys>) [INCLUDE ...] [WITH (...)]
// [WHERE ...]", with a leading "UNIQUE " for a unique index. Method, key
// columns and expressions, operator classes, collations, orderings,
// reloptions (m, ef_construction) and the predicate all stay in it.
func comparableIndexDefinition(def string) (string, error) {
	unique := strings.HasPrefix(def, "CREATE UNIQUE INDEX ")
	if !unique && !strings.HasPrefix(def, "CREATE INDEX ") {
		return "", fmt.Errorf("%w: unexpected definition %q", errIndexDefinitionUndetermined, def)
	}
	_, tail, ok := strings.Cut(def, " USING ")
	if !ok {
		return "", fmt.Errorf("%w: no access method in %q", errIndexDefinitionUndetermined, def)
	}
	if unique {
		return "UNIQUE USING " + tail, nil
	}
	return "USING " + tail, nil
}

// indexProbeStatement turns the managed DDL of index name into the same
// index on the probe table: managedIndexDDLPrefix + name + ON context_blocks
// is replaced by a plain CREATE INDEX probeIndex ON probeTable, everything
// after the table name is kept byte for byte.
func indexProbeStatement(name, ddl, probeIndex, probeTable string) (string, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(ddl), managedIndexDDLPrefix+name)
	if ok {
		rest, ok = strings.CutPrefix(strings.TrimLeft(rest, " \t\r\n"), "ON context_blocks")
	}
	if !ok || rest == "" || !strings.ContainsAny(rest[:1], " \t\r\n(") {
		return "", fmt.Errorf("%w: the DDL of %s does not read %q", errIndexDefinitionUndetermined,
			name, managedIndexDDLPrefix+name+" ON context_blocks ...")
	}
	return "CREATE INDEX " + probeIndex + " ON " + probeTable + rest, nil
}

// expectedIndexDefinition returns the comparable definition the server gives
// the index that ddl builds, read off a copy of the index on an empty copy
// of context_blocks inside a transaction that is rolled back. The copy holds
// ACCESS SHARE on context_blocks for the few milliseconds of the
// transaction (CREATE TABLE ... LIKE reads its definition).
func expectedIndexDefinition(ctx context.Context, db pgxdb.Beginner, name, ddl string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("%w: probe name: %w", errIndexDefinitionUndetermined, err)
	}
	probeTable := "ctx_index_probe_" + hex.EncodeToString(suffix[:])
	probeIndex := probeTable + "_i"
	stmt, err := indexProbeStatement(name, ddl, probeIndex, probeTable)
	if err != nil {
		return "", err
	}

	// pgxdb.Probe never commits: the copy is always rolled back.
	var def string
	err = pgxdb.Probe(ctx, db, "begin probe", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TABLE `+probeTable+` (LIKE context_blocks)`); err != nil {
			return fmt.Errorf("probe table: %w", err)
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("probe index: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT pg_get_indexdef($1::regclass)`, probeIndex).Scan(&def); err != nil {
			return fmt.Errorf("probe definition: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("%w: %w", errIndexDefinitionUndetermined, err)
	}
	return comparableIndexDefinition(def)
}

// runtimeIndexMatches compares the live definition liveDef of index name
// with the one its DDL builds. It returns both comparable forms; an error
// wraps errIndexDefinitionUndetermined.
func runtimeIndexMatches(ctx context.Context, db pgxdb.Beginner, name, ddl, liveDef string) (live, expected string, same bool, err error) {
	live, err = comparableIndexDefinition(liveDef)
	if err != nil {
		return "", "", false, err
	}
	expected, err = expectedIndexDefinition(ctx, db, name, ddl)
	if err != nil {
		return live, "", false, err
	}
	return live, expected, live == expected, nil
}
