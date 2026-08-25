// Package hermesstate is a read-only reader over the SQLite state store of a
// hermes agent (NousResearch/hermes-agent, file `state.db`). It answers three
// questions about a session — what is archived, is there anything new, how
// long has it been idle — and hands back decoded tool rows. Nothing else.
//
// Five invariants hold for the whole package, each one a property of the code
// rather than of a caller's discipline:
//
//  1. No write path. The DSN is pinned to mode=ro (immutable=1 only in
//     snapshot mode) and the package exposes no statement-running surface at
//     all. The ABSENCE of such a method is the guarantee, not a runtime check.
//  2. Schema hardening before the first payload query: trusted_schema off,
//     cell_size_check on, query_only on, SQLITE_DBCONFIG_DEFENSIVE on — plus a
//     type check that `messages` and `sessions` really are TABLEs. A prepared
//     file that swaps a table for a VIEW (or hangs a generated column with a
//     function call off it) is refused, not read.
//  3. The query plan is CHOSEN and VERIFIED, never assumed. hermes' schema does
//     not guarantee an index on (session_id, id): older stores carry only
//     idx_messages_session(session_id, timestamp), against which the natural
//     phrasing degrades to a full session scan plus a sort. Open picks a
//     strategy, then asserts the plan with EXPLAIN QUERY PLAN and refuses to
//     open if the assertion fails.
//  4. Every query is its own autocommit read. Never a snapshot, never an open
//     transaction spanning batches: a WAL reader holding a read mark keeps the
//     foreign writer's -wal from being checkpointed back. Batch consistency
//     comes from the monotone id range, not from a snapshot.
//  5. Content is decoded before it leaves the package. hermes stores multimodal
//     parts as "\x00json:" + JSON; only the text parts survive, and no NUL byte
//     ever reaches a caller.
//
// Source: https://github.com/GottZ/ctx
package hermesstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// driverName is the name modernc.org/sqlite registers itself under. The driver
// is CGO-free (the ctx image is distroless/static with CGO_ENABLED=0) and it
// parses URI DSNs — the latter is what makes "?mode=ro" take effect at all. A
// driver without URI parsing would treat the whole string as a literal file
// name and silently create an empty database next to it.
const driverName = "sqlite"

// openTimeout bounds the whole Open sequence (first file touch, schema type
// check, index probe, two plan assertions). A source that cannot answer these
// within the budget is treated as unavailable rather than blocking a caller.
const openTimeout = 15 * time.Second

// defaultBusyTimeout is how long a read waits for a lock before giving up. A
// WAL reader is rarely blocked at all; the budget exists for the seconds during
// which a foreign writer restarts the wal.
const defaultBusyTimeout = 2 * time.Second

// Error classes. They are the strings the caller's journal records — the
// underlying driver text is wrapped for local diagnosis but must never be
// carried into a persisted field.
var (
	// ErrSourceUnavailable means the file could not be read right now: absent,
	// not permitted, or a cleanly closed WAL database opened read-only (SQLite
	// would have to create the wal-index and cannot, SQLITE_READONLY (8)). It
	// is never a hard failure — while hermes is stopped, no new material is
	// produced either.
	ErrSourceUnavailable = errors.New("source_unavailable")

	// ErrSchemaUntrusted means the file's shape is not the shape this package
	// agreed to read — `messages` or `sessions` is missing or is not a TABLE.
	ErrSchemaUntrusted = errors.New("schema_untrusted")

	// ErrQueryFailed covers everything else, including a failed plan assertion.
	ErrQueryFailed = errors.New("query_failed")

	// ErrNoActiveRows is returned by QuietFor for a session whose rows are all
	// archived. There is no newest active message to measure against; the
	// caller decides what that means for its gate.
	ErrNoActiveRows = errors.New("no_active_rows")
)

// Strategy names the access path chosen for the session-scoped reads.
type Strategy string

const (
	// StrategyIndexSeek is used when the store carries an index whose leading
	// columns are (session_id, id): the planner finds an equality-plus-range
	// seek and returns rows already in id order.
	StrategyIndexSeek Strategy = "index-seek"

	// StrategyRowidRange is used otherwise. A unary plus on the session_id
	// comparison disqualifies every index on that column, which leaves the
	// planner the INTEGER PRIMARY KEY range on id — ordered by construction,
	// so no sort is materialised either.
	StrategyRowidRange Strategy = "rowid-range"
)

// forbiddenIndexFragment is the plan substring that marks the degraded path:
// idx_messages_session(session_id, timestamp) drives the scan and the id order
// has to be built in a temp b-tree. The trailing space is load-bearing — it is
// what separates this index from idx_messages_session_id and
// idx_messages_session_active, both of which are fine.
const forbiddenIndexFragment = "idx_messages_session "

// forbiddenSortFragment marks a materialised sort of the batch.
const forbiddenSortFragment = "USE TEMP B-TREE"

// Options carries the few knobs that are not fixed by the invariants.
type Options struct {
	// Snapshot adds immutable=1 to the DSN. This is the ONLY way to read a
	// cleanly closed WAL database read-only, and it is admissible ONLY for a
	// private snapshot copy: on a file a foreign process still writes,
	// immutable=1 licenses SQLite to assume the bytes never change and the
	// results become undefined.
	Snapshot bool

	// BusyTimeout overrides defaultBusyTimeout when non-zero.
	BusyTimeout time.Duration
}

// Source is one read-only handle on a hermes state.db.
type Source struct {
	db        *sql.DB
	label     string
	path      string
	strategy  Strategy
	indexName string
	plan      string
}

// Open opens path read-only, applies the hardening pragmas, verifies the schema
// type of the tables it reads, chooses the query strategy and asserts its plan.
//
// WAL semantics (probed 2026-08-25 against SQLite 3.53.2 as an unprivileged
// non-owner): a WAL database is readable via mode=ro ONLY while its -shm file
// exists, i.e. while hermes holds the file open. A cleanly closed WAL database
// fails with SQLITE_READONLY (8) because the reader would have to create the
// wal-index. That maps to ErrSourceUnavailable, never to a hard failure.
//
// Open never creates a file: an absent path yields SQLITE_CANTOPEN, mapped to
// ErrSourceUnavailable, and leaves the directory untouched.
func Open(path, label string, opt Options) (*Source, error) {
	db, err := sql.Open(driverName, buildDSN(path, opt))
	if err != nil {
		return nil, classify(err)
	}
	// One connection. The reads are sequential by nature, a single handle keeps
	// exactly one reader registered against the foreign wal-index, and it makes
	// the autocommit discipline observable: there is no second connection on
	// which a stray transaction could survive.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Source{db: db, label: label, path: path}

	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	if err := s.verify(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the handle.
func (s *Source) Close() error { return s.db.Close() }

// Label is the caller-supplied name of this source, for logs and journals.
func (s *Source) Label() string { return s.label }

// Path is the file this source reads.
func (s *Source) Path() string { return s.path }

// Strategy reports which access path Open chose and verified.
func (s *Source) Strategy() Strategy { return s.strategy }

// Plan reports the asserted EXPLAIN QUERY PLAN of the batch read, joined into
// one line. It is diagnostic output for the operator, not a parsed value.
func (s *Source) Plan() string { return s.plan }

// buildDSN assembles the URI DSN. Everything security-relevant lives here
// rather than in a statement after the fact: the DSN is applied to EVERY
// connection the pool opens, a post-open statement would not be.
func buildDSN(path string, opt Options) string {
	busy := opt.BusyTimeout
	if busy <= 0 {
		busy = defaultBusyTimeout
	}
	params := []string{"mode=ro"}
	if opt.Snapshot {
		params = append(params, "immutable=1")
	}
	params = append(params,
		// SQLITE_DBCONFIG_DEFENSIVE: no writable_schema, no direct writes to
		// shadow tables of virtual tables (the FTS5 tables hermes keeps).
		"_defensive=1",
		// The connection refuses to write anything, belt to the DSN's braces.
		"_query_only=1",
		fmt.Sprintf("_busy_timeout=%d", busy.Milliseconds()),
		// Functions named in the schema are not run (B7: a generated column or
		// a partial-index predicate that calls an application function).
		"_pragma=trusted_schema(off)",
		// Corrupt b-tree cells are reported instead of being followed.
		"_pragma=cell_size_check(on)",
	)
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + strings.Join(params, "&")
}

// verify runs the whole Open contract: hardening readback, schema type check,
// strategy choice, plan assertion.
func (s *Source) verify(ctx context.Context) error {
	if err := s.checkHardening(ctx); err != nil {
		return err
	}
	if err := s.checkSchemaTypes(ctx); err != nil {
		return err
	}
	if err := s.chooseStrategy(ctx); err != nil {
		return err
	}
	return s.assertPlans(ctx)
}

// checkHardening reads the pragmas back. It is also the first real touch of the
// file, so it is where an absent, unreadable or cleanly-closed-WAL source
// surfaces as ErrSourceUnavailable.
func (s *Source) checkHardening(ctx context.Context) error {
	// The statements are literals, not assembled from the names: a pragma name
	// is not a bind parameter, so the only way to keep this free of string
	// building is to write the three queries out.
	for _, p := range []struct {
		name  string
		query string
		want  int64
	}{
		{"trusted_schema", "SELECT * FROM pragma_trusted_schema", 0},
		{"cell_size_check", "SELECT * FROM pragma_cell_size_check", 1},
		{"query_only", "SELECT * FROM pragma_query_only", 1},
	} {
		var got int64
		row := s.db.QueryRowContext(ctx, p.query)
		if err := row.Scan(&got); err != nil {
			return classify(err)
		}
		if got != p.want {
			return fmt.Errorf("%w: pragma %s is %d, want %d", ErrQueryFailed, p.name, got, p.want)
		}
	}
	return nil
}

// checkSchemaTypes is the answer to a hostile file (B7). A VIEW named
// `messages` would be read like a table and could carry arbitrary expressions;
// a size cap would not notice, this does.
func (s *Source) checkSchemaTypes(ctx context.Context) error {
	for _, name := range []string{"messages", "sessions"} {
		var typ string
		row := s.db.QueryRowContext(ctx,
			"SELECT type FROM sqlite_schema WHERE name = ? AND type IN ('table','view')", name)
		switch err := row.Scan(&typ); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s is absent", ErrSchemaUntrusted, name)
		case err != nil:
			return classify(err)
		}
		if typ != "table" {
			return fmt.Errorf("%w: %s is a %s, not a table", ErrSchemaUntrusted, name, typ)
		}
	}
	return nil
}

// chooseStrategy looks for a non-partial index on messages whose leading two
// columns are (session_id, id). Such an index exists in every store written by
// a recent hermes and in both live files probed for the design, but NOT in the
// schema of the version this package was written against — so its presence is
// established, never assumed.
func (s *Source) chooseStrategy(ctx context.Context) error {
	name, err := s.findSessionIDIndex(ctx)
	if err != nil {
		return err
	}
	if name != "" {
		s.strategy, s.indexName = StrategyIndexSeek, name
		return nil
	}
	s.strategy, s.indexName = StrategyRowidRange, ""
	return nil
}

func (s *Source) findSessionIDIndex(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name FROM pragma_index_list('messages') WHERE partial = 0 AND origin IN ('c','u')")
	if err != nil {
		return "", classify(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return "", classify(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", classify(err)
	}
	if err := rows.Close(); err != nil {
		return "", classify(err)
	}

	for _, n := range names {
		cols, err := s.indexColumns(ctx, n)
		if err != nil {
			return "", err
		}
		if len(cols) >= 2 && cols[0] == "session_id" && cols[1] == "id" {
			return n, nil
		}
	}
	return "", nil
}

func (s *Source) indexColumns(ctx context.Context, index string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT COALESCE(name,'') FROM pragma_index_info(?) ORDER BY seqno", index)
	if err != nil {
		return nil, classify(err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, classify(err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return cols, nil
}

// assertPlans proves both session-scoped hot paths against the real file. A
// source whose plan cannot be proven is not opened: an arm that reads with the
// wrong plan is worse than an arm that stays inert, because its cost only shows
// up once the store has grown.
func (s *Source) assertPlans(ctx context.Context) error {
	batch, err := s.explain(ctx, s.toolRowsSQL(), "s", int64(0), 1)
	if err != nil {
		return err
	}
	if err := s.assertPlan(batch); err != nil {
		return fmt.Errorf("%w: batch read: %w", ErrQueryFailed, err)
	}
	s.plan = batch

	probe, err := s.explain(ctx, s.hasNewSQL(), "s", int64(0))
	if err != nil {
		return err
	}
	if err := s.assertPlan(probe); err != nil {
		return fmt.Errorf("%w: existence probe: %w", ErrQueryFailed, err)
	}
	return nil
}

// assertPlan enforces the two prohibitions and the one requirement.
func (s *Source) assertPlan(plan string) error {
	if strings.Contains(strings.ToUpper(plan), forbiddenSortFragment) {
		return fmt.Errorf("plan materialises a sort: %s", plan)
	}
	if strings.Contains(plan, forbiddenIndexFragment) {
		return fmt.Errorf("plan uses the timestamp-ordered index: %s", plan)
	}
	want := "USING INTEGER PRIMARY KEY"
	if s.strategy == StrategyIndexSeek {
		want = s.indexName
	}
	if !strings.Contains(plan, want) {
		return fmt.Errorf("plan lacks %q (strategy %s): %s", want, s.strategy, plan)
	}
	return nil
}

// explain returns the EXPLAIN QUERY PLAN detail column of query, joined with
// " | ". The bind values are placeholders — the plan does not depend on them.
func (s *Source) explain(ctx context.Context, query string, args ...any) (string, error) {
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return "", classify(err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return "", classify(err)
	}
	var details []string
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			return "", classify(err)
		}
		// The detail text is the last column in every SQLite version that has
		// EXPLAIN QUERY PLAN as a four-column result.
		if last, ok := cells[len(cells)-1].(*sql.NullString); ok && last.Valid {
			details = append(details, last.String)
		}
	}
	if err := rows.Err(); err != nil {
		return "", classify(err)
	}
	return strings.Join(details, " | "), nil
}

// hint is the session_id predicate prefix of the chosen strategy: a unary plus
// under rowid-range (it disqualifies every index on session_id), nothing under
// index-seek.
func (s *Source) hint() string {
	if s.strategy == StrategyIndexSeek {
		return ""
	}
	return "+"
}

// classify maps a driver error onto the package's error taxonomy. Everything
// that means "the file cannot be read right now" becomes ErrSourceUnavailable;
// the rest is a query failure.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		// The low byte is the primary result code; the high bytes carry the
		// extended code, which is diagnosis, not classification.
		switch se.Code() & 0xff {
		case 3, // SQLITE_PERM
			8,  // SQLITE_READONLY — includes the cleanly-closed-WAL case
			10, // SQLITE_IOERR
			14, // SQLITE_CANTOPEN — includes the absent file
			23: // SQLITE_AUTH
			return fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
		}
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	return fmt.Errorf("%w: %w", ErrQueryFailed, err)
}
