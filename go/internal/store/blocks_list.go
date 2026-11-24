package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The listings that read context_blocks newest first — browse, the
// empty-query path of the compact and the full search, and recent — read
// their page from two indexes of migration 195 (issue #237):
//
//	idx_blocks_scope_updated          (scope, updated_at DESC, id DESC) WHERE NOT is_archived
//	idx_blocks_scope_category_updated (scope, category, updated_at DESC, id DESC) WHERE NOT is_archived
//
// one ordered range per read scope (and category). The statements before 195
// (searchBlocksSQL, recentBlocksSQL) carry the visibility clause
// `scope = ANY($1) OR id = ANY($grants)`, which no index delivers in the page
// order: the planner reads every visible row of the scopes (a bitmap or a
// sequential scan) and sorts them for the LIMIT, so a page cost what the
// reader's scopes hold (docs/operations.md, "Migration 195").
//
// listPageSQL states the same rows as one LATERAL arm per distinct read
// scope, each the scope's range of the index from the cursor on with the
// statement's filters and the statement's own LIMIT, plus one arm for the
// granted blocks outside the read scopes, and sorts the arms' rows for the
// LIMIT above them. A row of the page is among the first LIMIT rows of its
// scope (or of the granted blocks) after the cursor, so the union holds the
// page. Each arm stops after LIMIT rows that pass its filters, so a page reads
// its scopes' ranges up to the page, whatever the scopes hold beside it and
// whatever other tenants hold. A reader of K scopes reads up to K × LIMIT rows
// and K descents for a page.
//
// An arm's filter that keeps few rows of its range makes it walk the range to
// find them, where the statements before 195 read an index of the filter
// (idx_blocks_type_name, idx_context_tags, the cluster membership). So the
// walk serves only a listing whose filters cannot make it read far past its
// page (listWalks): no filter but a category (an equality the second index
// carries as part of its range) and the type exclusion. Every other listing,
// and every listing while the index its arms read does not stand
// (listIndexReady), sends the statement before 195 unchanged.

// listPage is the input of listPageSQL: one page of browse or of recent.
type listPage struct {
	ReadScopes      []string
	GrantedBlockIDs []string
	Category        string
	TypesExclude    []string
	After           *SearchCursor
	// Limit is the number of rows the statement answers (the page, plus the
	// probe row where the caller asks for one).
	Limit int
	// PageCols are the columns the arms hand up; OuterCols the columns of the
	// answer, computed over the page only. OuterCols may name
	// context_blocks.<col>: the page is aliased so.
	PageCols, OuterCols string
}

// listPageSQL composes the walk.
//
//   - The read scopes are deduplicated: a scope named twice is one arm, so no
//     row comes twice.
//   - The grant arm takes the granted blocks whose scope is not a read scope
//     (`IS NOT TRUE`: a NULL among the read scopes turns `= ANY` NULL for a
//     scope that is none of the others, and such a block is not read through
//     the scope arms either); a granted block in a read scope comes from its
//     scope's arm, once. The grant set is bounded (maxGrantedBlockIDs), and
//     the arm reads it by primary key.
//   - NOT is_archived in every arm: it is the predicate of both indexes, so
//     the planner can read them for the scope arms, and it keeps an archived
//     granted block out, as the statements before 195 did.
//   - The category, an equality, is the second key column of
//     idx_blocks_scope_category_updated: with it the arm is that index's
//     range of (scope, category).
//   - The cursor is the row comparison (updated_at, id) < ($at, $id) of the
//     keyset (SearchCursor), in every arm: within a range it is its start.
func listPageSQL(p listPage) (string, []any) {
	grants := p.GrantedBlockIDs
	if grants == nil {
		grants = []string{} // deterministic '{}'::uuid[], never NULL
	}
	args := []any{p.ReadScopes, grants}
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	var filter strings.Builder
	if p.Category != "" {
		filter.WriteString(" AND category = " + bind(p.Category))
	}
	if len(p.TypesExclude) > 0 {
		filter.WriteString(" AND NOT (type_name = ANY(" + bind(p.TypesExclude) + "::text[]))")
	}
	if p.After != nil {
		at := bind(p.After.UpdatedAt)
		filter.WriteString(" AND (updated_at, id) < (" + at + "::timestamptz, " + bind(p.After.ID) + "::uuid)")
	}
	limit := bind(p.Limit)
	return fmt.Sprintf(`SELECT %[1]s FROM (
	SELECT arm.* FROM (SELECT DISTINCT s.scope FROM unnest($1::text[]) AS s(scope)) AS rs
	CROSS JOIN LATERAL (
		SELECT %[2]s FROM context_blocks
		 WHERE scope = rs.scope AND NOT is_archived%[3]s
		 ORDER BY updated_at DESC, id DESC
		 LIMIT %[4]s) AS arm
	UNION ALL
	(SELECT %[2]s FROM context_blocks
	  WHERE id = ANY($2::uuid[]) AND (scope = ANY($1::text[])) IS NOT TRUE AND NOT is_archived%[3]s
	  ORDER BY updated_at DESC, id DESC
	  LIMIT %[4]s)
	ORDER BY updated_at DESC, id DESC
	LIMIT %[4]s) AS context_blocks
ORDER BY updated_at DESC, id DESC`, p.OuterCols, p.PageCols, filter.String(), limit), args
}

// listIndexes is a reading of the two indexes of migration 195: whether each
// stands as the migration builds it.
type listIndexes struct {
	scope    bool // idx_blocks_scope_updated
	category bool // idx_blocks_scope_category_updated
}

// listWalks reports whether a listing with these filters reads the walk, given
// which indexes stand. The arms of a listing with a category read
// idx_blocks_scope_category_updated, the others idx_blocks_scope_updated. A
// type list, tags and the cluster facet keep the statement before 195: each
// can keep a handful of a scope's rows, the arm would read the scope's whole
// range to find them, and the statement before 195 reads the filter's own
// index (measured at 1 000 000 rows: a type that the reader's scope of 1 000
// rows does not hold, 81 buffers before and 933 on the walk;
// docs/operations.md, "Migration 195"). The type exclusion stays in the walk:
// it keeps the rows of every type but the named ones.
func listWalks(ix listIndexes, category string, tags, tagsAll, types []string, cluster *string) bool {
	if len(tags) > 0 || len(tagsAll) > 0 || len(types) > 0 || (cluster != nil && *cluster != "") {
		return false
	}
	if category != "" {
		return ix.category
	}
	return ix.scope
}

// listMayWalk reports whether a listing with these filters walks once both
// indexes stand: only then is the catalog read for it.
func listMayWalk(tags, tagsAll, types []string, cluster *string) bool {
	return listWalks(listIndexes{scope: true, category: true}, "", tags, tagsAll, types, cluster)
}

// browseIndexSQL is the walk for the browse path of searchBlocksSQL: the same
// columns in the same order, the same limit and probe row.
func browseIndexSQL(p searchParams) (string, []any) {
	limit := ClampLimit(p.Limit, SearchDefaultLimit, SearchMaxLimit)
	if p.ProbeNext {
		limit++ // the probe row; searchBlocks drops it again
	}
	lp := listPage{
		ReadScopes: p.ReadScopes, GrantedBlockIDs: p.GrantedBlockIDs, Category: p.Category,
		TypesExclude: p.TypesExclude, After: p.After, Limit: limit,
	}
	if p.Compact {
		lp.PageCols, lp.OuterCols = compactPageCols, compactPreviewCols
	} else {
		// The redacted metadata holds a subquery per row: computed over the
		// page, as the planner postponed it past the sort before.
		lp.PageCols = fullPageCols
		lp.OuterCols = `id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, content, ` + guardRedactedMetadataSQL("$1") + `, created_at, updated_at, ` + untrustedOriginCol
	}
	return listPageSQL(lp)
}

// fullPageCols are the columns the arms of the full browse hand up.
const fullPageCols = `id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, content, metadata, created_at, updated_at, untrusted_origin`

// recentPageCols are the columns of recent's page; recentPreviewCols the
// columns of its answer, computed over the page (compactPageCols).
const (
	recentPageCols    = `id, category, tags, title, scope, type_name, lifecycle_state, type_source, content, created_at, updated_at, untrusted_origin`
	recentPreviewCols = `id, category, tags, title, scope, type_name, lifecycle_state, type_source, LEFT(content, 200), char_length(content), created_at, updated_at, ` + untrustedOriginCol
)

// recentIndexSQL is the walk for RecentBlocks, for an already clamped limit.
func recentIndexSQL(readScopes []string, category string, limit int, typesExclude, grantedBlockIDs []string) (string, []any) {
	return listPageSQL(listPage{
		ReadScopes: readScopes, GrantedBlockIDs: grantedBlockIDs, Category: category,
		TypesExclude: typesExclude, Limit: limit,
		PageCols: recentPageCols, OuterCols: recentPreviewCols,
	})
}

// listIndexReadySQL reads whether each index of migration 195 stands as the
// migration builds it (valid, ready, the deparsed definition the migration
// adopts). Migration 195 does not build them above 500 000 rows, nor over an
// INVALID or foreign index of the name. Without its index an arm reads its
// whole scope through idx_context_scope and sorts it, which costs more than
// the statement before 195 where a reader holds many scopes or filters a
// category (measured at 1 000 000 rows: 744 081 against 200 743-204 469
// buffers for 500 scopes, 18 881 against 8 377 for a category), so a listing
// sends the statement before 195 until the index its arms read stands.
const listIndexReadySQL = `SELECT
	EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass('public.idx_blocks_scope_updated') AND indisvalid AND indisready
		  AND pg_get_indexdef(indexrelid) = 'CREATE INDEX idx_blocks_scope_updated ON public.context_blocks USING btree (scope, updated_at DESC, id DESC) WHERE (NOT is_archived)'),
	EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass('public.idx_blocks_scope_category_updated') AND indisvalid AND indisready
		  AND pg_get_indexdef(indexrelid) = 'CREATE INDEX idx_blocks_scope_category_updated ON public.context_blocks USING btree (scope, category, updated_at DESC, id DESC) WHERE (NOT is_archived)')`

// listIndexProbeInterval is how long a process trusts its last reading of
// listIndexReadySQL for one database (the rule of
// derivedIdentityProbeInterval): browse and recent run on every listing, the
// reading is a catalog query of its own, and the indexes change only by a
// migration or an operator's out-of-band build or drop. In the minute after
// such a change a listing answers the same rows in either form, at the other
// form's cost.
const listIndexProbeInterval = time.Minute

// listIndexReading is one reading of listIndexReadySQL.
type listIndexReading struct {
	ix listIndexes
	at time.Time
}

// listIndexReadings holds the last reading per database (databaseKey).
var listIndexReadings = struct {
	sync.Mutex
	m map[string]listIndexReading
}{m: map[string]listIndexReading{}}

// listIndexReady reports which indexes of migration 195 stand, from the
// process's reading of the last listIndexProbeInterval for the database
// behind pool, else from a new reading.
func listIndexReady(ctx context.Context, pool *pgxpool.Pool) (listIndexes, error) {
	key, keep := databaseKey(pool)
	if keep {
		listIndexReadings.Lock()
		r, ok := listIndexReadings.m[key]
		listIndexReadings.Unlock()
		if ok && time.Since(r.at) < listIndexProbeInterval {
			return r.ix, nil
		}
	}
	var ix listIndexes
	if err := pool.QueryRow(ctx, listIndexReadySQL).Scan(&ix.scope, &ix.category); err != nil {
		return listIndexes{}, fmt.Errorf("store: list index probe: %w", err)
	}
	if keep {
		listIndexReadings.Lock()
		listIndexReadings.m[key] = listIndexReading{ix: ix, at: time.Now()}
		listIndexReadings.Unlock()
	}
	return ix, nil
}

// searchStatement is the statement searchBlocks sends for p: the walk for a
// browse whose filters listWalks admits while the index its arms read
// stands, else searchBlocksSQL's. The catalog is read only for a listing
// that may walk.
func searchStatement(ctx context.Context, pool *pgxpool.Pool, p searchParams) (string, []any, error) {
	var ix listIndexes
	if p.Query == "" && listMayWalk(p.Tags, p.TagsAll, p.Types, p.Cluster) {
		var err error
		if ix, err = listIndexReady(ctx, pool); err != nil {
			return "", nil, err
		}
	}
	sql, args := searchStatementFor(p, ix)
	return sql, args, nil
}

// searchStatementFor is searchStatement for a known reading of the indexes.
func searchStatementFor(p searchParams, ix listIndexes) (string, []any) {
	if p.Query == "" && listWalks(ix, p.Category, p.Tags, p.TagsAll, p.Types, p.Cluster) {
		return browseIndexSQL(p)
	}
	return searchBlocksSQL(p)
}

// recentStatement is the statement RecentBlocks sends, as searchStatement.
func recentStatement(ctx context.Context, pool *pgxpool.Pool, readScopes []string, category string, limit int, types, typesExclude, grantedBlockIDs []string) (string, []any, error) {
	var ix listIndexes
	if listMayWalk(nil, nil, types, nil) {
		var err error
		if ix, err = listIndexReady(ctx, pool); err != nil {
			return "", nil, err
		}
	}
	sql, args := recentStatementFor(readScopes, category, limit, types, typesExclude, grantedBlockIDs, ix)
	return sql, args, nil
}

// recentStatementFor is recentStatement for a known reading of the indexes.
func recentStatementFor(readScopes []string, category string, limit int, types, typesExclude, grantedBlockIDs []string, ix listIndexes) (string, []any) {
	if listWalks(ix, category, nil, nil, types, nil) {
		return recentIndexSQL(readScopes, category, limit, typesExclude, grantedBlockIDs)
	}
	return recentBlocksSQL(readScopes, category, limit, types, typesExclude, grantedBlockIDs)
}
