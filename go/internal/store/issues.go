// Package store — issue/comment write + read primitives (Achse 02, Welle I-D,
// K2 "Store+Tier" form). Issues and comments are ordinary context_blocks
// (type_name issue/comment, migration 084 seeds) — they inherit scope
// isolation, RRF, guard and dream for free (vision 019e83df-9666). This file is
// the STORE logic; both the manage operator transport (context_manage_issues.go)
// and the later REST issue surface (W6/W7) call THESE functions — one logic,
// two transports (the type-* / §4.1 house pattern, docs/api.md).
//
// Identity (design/02 §3.5): issue writes NEVER go through UpsertBlock (its
// (category,title,scope) conflict target is wrong for issues — titles change and
// two issues may share a title). Instead: insert-once + update-by-id, with a
// per-scope local sequence stamped as a "#L<seq>" title prefix so the partial
// unique index uq_context_category_title_scope (005) can never 23505 two issues
// of the same human title. The local sequence lives in metadata.local_seq
// (allocated under a per-scope pg_advisory_xact_lock) — NOT in a counter column:
// the context_forge_repos.local_seq counter of design §3.5 belongs to the forge
// wave I-F/M080, which does not ship here, and this wave adds no table/column.
//
// Scope isolation (fail-closed, §5.2):
//   - InsertIssueBlock: scope ∈ writableScopes (else ErrIssueScope).
//   - InsertCommentBlock: scope is ALWAYS the PARENT's scope (never the request),
//     and it composes store.PutBlockParent, which re-asserts child.scope ==
//     parent.scope ∈ writableScopes in the same Tx (comment-scope invariant).
//   - GetIssue/ListIssues: filtered through readScopes (+ block grants) — a
//     foreign-key holder sees an empty result, never a foreign row.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Issue/comment type + category names (registry vocabulary, migration 084).
const (
	IssueTypeName   = "issue"
	CommentTypeName = "comment"
)

// Body cap: mirror the /api/store 50 KB cap (context_store.go, design/02 §5.5) —
// issue/comment bodies are attacker-controlled markdown; over-length is the
// caller's error (the handler truncates + flags for the forge path in I-F, but
// the store rejects a raw over-cap write fail-closed).
const maxIssueBodyBytes = 50 * 1024

// Sentinel errors. The handler maps ErrIssueNotFound/ErrIssueScope to a uniform
// 404 (no existence oracle for foreign block ids, §5.2); ErrIssueBody to 422.
// Transition errors bubble up from blocktype (ErrInvalidTransition &c → 422).
var (
	// ErrIssueNotFound — the target issue does not exist, is archived, or is
	// invisible/foreign to the caller (uniform, no oracle).
	ErrIssueNotFound = errors.New("store: issue not found")
	// ErrIssueScope — the requested write scope is not writable by the caller.
	ErrIssueScope = errors.New("store: issue scope not writable")
	// ErrIssueBody — the body exceeds the persistence cap.
	ErrIssueBody = errors.New("store: issue body exceeds cap")
	// ErrCommentParentRequired — a comment write carried no parent (a comment is
	// a required-parent block; creating one orphaned is the §9.1a orphan case).
	ErrCommentParentRequired = errors.New("store: comment requires a parent issue")
	// ErrReservedMetadata — the write carries the derived layer's provenance key
	// in client-supplied metadata (I7/S3b, design D-01 §4.3.1).
	//
	// The gate lives HERE and not only at the seven issue entry points
	// (handler/context_manage_issues.go ×3, handler/project_issues_write.go ×3,
	// handler/mcp_issues.go ×2) because this domain has already cost two review
	// rounds exactly that way: a gate on N of N+1 doors is a convention. Every
	// issue write in the tree passes InsertIssueBlock, InsertCommentBlock or
	// UpdateIssueBlock, so three checks cover eight entries and the ninth that
	// has not been written yet.
	//
	// Why it matters on THIS domain in particular: issue metadata is the only
	// client-supplied map in the tree that is MERGED into an existing row
	// (`metadata = metadata || $n::jsonb`), and since the archive verbs learned
	// the provenance exclusion, a planted key made the block unremovable by any
	// client verb — the issue domain has no delete, and manage-delete and
	// guard-resolve both answer 403. Client-creatable and client-unremovable is
	// a class this store must not have.
	ErrReservedMetadata = errors.New("store: 'provenance' is the derived layer's metadata key — not client-writable")
)

// rejectReservedMetadata is the one guard behind ErrReservedMetadata, sharing
// its predicate with the handler gate through derived.HasProvenance.
func rejectReservedMetadata(metadata map[string]any) error {
	if derived.HasProvenance(metadata) {
		return ErrReservedMetadata
	}
	return nil
}

// issuePrefixRe matches the leading ctx number prefix of an issue title:
// "#L<seq>" (local, this wave) or "#<nr>" (forge, I-F). Group 1 is the prefix
// without the trailing space. Used to preserve the prefix across a title edit
// and to build the comment title (design/02 §3.1/§3.6).
var issuePrefixRe = regexp.MustCompile(`^(#L?\d+)(?:\s|$)`)

// IssueFields carries the fields of an issue create. Title is the HUMAN title
// WITHOUT the ctx prefix — InsertIssueBlock derives "#L<seq>". Status is the
// workflow status to stamp ("" ⇒ NULL); the handler resolves it from the type
// config (WorkflowInitial) and validates a caller-supplied status.
type IssueFields struct {
	Scope    string
	Title    string
	Content  string
	Tags     []string
	Metadata map[string]any
	Status   string
}

// IssueUpdate is the by-id PATCH shape (nil = leave unchanged). Title is the new
// HUMAN title; the "#L<seq>" prefix is preserved. Metadata is JSONB-merged onto
// the existing metadata (so local_seq/forge fields survive a partial update).
type IssueUpdate struct {
	Title    *string
	Content  *string
	Tags     []string
	Metadata map[string]any
	Status   *string
}

// CommentFields carries the fields of a comment create. There is deliberately
// NO scope field: the comment ALWAYS inherits the parent issue's scope (§5.2).
type CommentFields struct {
	Author   string
	Content  string
	Metadata map[string]any
}

// scopeIn reports whether scope is in the allowed set.
func scopeIn(scope string, allowed []string) bool {
	for _, s := range allowed {
		if s == scope {
			return true
		}
	}
	return false
}

// nextLocalSeq allocates the next per-scope (issues) or per-parent (comments)
// local sequence under a pg_advisory_xact_lock, serialising concurrent creates
// so two writers never pick the same number. The lock is transaction-scoped
// (released at commit/rollback). It reads MAX(metadata.local_seq) over the
// relevant rows INCLUDING archived ones — numbers are monotone, never reused.
func nextLocalSeq(ctx context.Context, tx pgx.Tx, lockKey, whereClause string, whereArg any) (int, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return 0, fmt.Errorf("issue seq: advisory lock: %w", err)
	}
	var seq int
	q := fmt.Sprintf(`SELECT COALESCE(MAX((metadata->>'local_seq')::int), 0) + 1
	     FROM context_blocks
	     WHERE %s AND jsonb_typeof(metadata->'local_seq') = 'number'`, whereClause)
	if err := tx.QueryRow(ctx, q, whereArg).Scan(&seq); err != nil {
		return 0, fmt.Errorf("issue seq: max: %w", err)
	}
	return seq, nil
}

// issueScanCols is the shared RETURNING/SELECT projection for issue rows: the
// GetBlock columns plus workflow_status (M077). One string so insert/get/update
// never drift.
const issueScanCols = `id, category, tags, title, content, metadata, scope,
	sensitivity, sensitivity_source, type_name, lifecycle_state, type_source,
	COALESCE(workflow_status, ''), created_at, updated_at`

// scanIssue scans one row in issueScanCols order into a Block.
func scanIssue(row pgx.Row) (*Block, error) {
	b := &Block{}
	err := row.Scan(&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope,
		&b.Sensitivity, &b.SensitivitySource, &b.TypeName, &b.LifecycleState, &b.TypeSource,
		&b.WorkflowStatus, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

// InsertIssueBlock creates a local issue block (type issue) with a "#L<seq>"
// title prefix, workflow_status = f.Status, type_source='manual' (so the
// auto-classifier never re-types it). Fail-closed on a non-writable scope and on
// an over-cap body. Tx-bound (design §4.2). embedding stays NULL ⇒ the scheduler
// backfill embeds it.
func InsertIssueBlock(ctx context.Context, tx pgx.Tx, f IssueFields, writableScopes []string) (*Block, error) {
	if !scopeIn(f.Scope, writableScopes) {
		return nil, ErrIssueScope
	}
	if err := rejectReservedMetadata(f.Metadata); err != nil { // I7/S3b
		return nil, err
	}
	if len(f.Content) > maxIssueBodyBytes {
		return nil, ErrIssueBody
	}
	seq, err := nextLocalSeq(ctx, tx, "ctx-issue-seq:"+f.Scope,
		"scope = $1 AND type_name = '"+IssueTypeName+"'", f.Scope)
	if err != nil {
		return nil, err
	}
	meta := f.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["local_seq"] = seq
	tags := f.Tags
	if tags == nil {
		tags = []string{}
	}
	title := fmt.Sprintf("#L%d %s", seq, f.Title)
	var status any
	if f.Status != "" {
		status = f.Status
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO context_blocks
		  (category, tags, title, content, metadata, scope, type_name, type_source, workflow_status)
		VALUES ($1, $2, $3, $4, $5, $6, '`+IssueTypeName+`', 'manual', $7)
		RETURNING `+issueScanCols,
		IssueTypeName, tags, title, f.Content, meta, f.Scope, status)
	b, err := scanIssue(row)
	if err != nil {
		return nil, fmt.Errorf("store: insert issue: %w", err)
	}
	return b, nil
}

// InsertCommentBlock creates a comment block (type comment) under parentID. The
// scope is ALWAYS the parent's (never the request) — the comment-scope invariant
// (§5.2). It composes store.PutBlockParent, which re-asserts the invariant and
// that the scope is writable in the same Tx (defence in depth). The comment
// title is "<parent-prefix>.cL<seq> <author>" (per-parent local sequence).
// A missing/foreign/invisible parent ⇒ ErrLinkScopeViolation (via PutBlockParent,
// uniform no-oracle); an empty parentID ⇒ ErrCommentParentRequired (orphan
// prevention for the required-parent comment type, §9.1a).
func InsertCommentBlock(ctx context.Context, tx pgx.Tx, parentID string, f CommentFields, writableScopes []string) (*Block, error) {
	if parentID == "" {
		return nil, ErrCommentParentRequired
	}
	if err := rejectReservedMetadata(f.Metadata); err != nil { // I7/S3b
		return nil, err
	}
	if len(f.Content) > maxIssueBodyBytes {
		return nil, ErrIssueBody
	}
	// Resolve the parent scope + title prefix + local_seq under FOR SHARE (the
	// scope must not change between here and the child insert/PutBlockParent).
	var parentScope, parentTitle string
	err := tx.QueryRow(ctx,
		`SELECT scope, title FROM context_blocks WHERE id = $1::uuid AND NOT is_archived FOR SHARE`,
		parentID).Scan(&parentScope, &parentTitle)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLinkScopeViolation // uniform: unknown parent = foreign parent
	}
	if err != nil {
		return nil, fmt.Errorf("store: comment parent lookup: %w", err)
	}
	if !scopeIn(parentScope, writableScopes) {
		return nil, ErrLinkScopeViolation
	}

	seq, err := nextLocalSeq(ctx, tx, "ctx-comment-seq:"+parentID,
		"parent_id = $1::uuid AND type_name = '"+CommentTypeName+"'", parentID)
	if err != nil {
		return nil, err
	}
	author := strings.TrimSpace(f.Author)
	if author == "" {
		author = "anon"
	}
	prefix := parentTitle
	if m := issuePrefixRe.FindStringSubmatch(parentTitle); m != nil {
		prefix = m[1]
	}
	meta := f.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["local_seq"] = seq
	if _, ok := meta["author"]; !ok {
		meta["author"] = author
	}
	title := fmt.Sprintf("%s.cL%d %s", prefix, seq, author)

	// Insert the comment in the PARENT's scope (never the request scope), then
	// set parent_id via PutBlockParent (re-validates the invariant, §5.2).
	row := tx.QueryRow(ctx, `
		INSERT INTO context_blocks
		  (category, tags, title, content, metadata, scope, type_name, type_source)
		VALUES ($1, '{}', $2, $3, $4, $5, '`+CommentTypeName+`', 'manual')
		RETURNING `+issueScanCols,
		CommentTypeName, title, f.Content, meta, parentScope)
	b, err := scanIssue(row)
	if err != nil {
		return nil, fmt.Errorf("store: insert comment: %w", err)
	}
	if err := PutBlockParent(ctx, tx, b.ID, parentID, writableScopes); err != nil {
		return nil, err // ErrLinkScopeViolation on any scope/existence breach
	}
	return b, nil
}

// GetIssue loads one issue/comment block by id with its workflow_status,
// filtered through readScopes (+ block grants) — a foreign/invisible id reads as
// ErrIssueNotFound (no oracle). It is type-restricted to issue/comment so it is
// an issue-domain surface, not a general block getter.
func GetIssue(ctx context.Context, pool *pgxpool.Pool, id string, readScopes, grantedBlockIDs []string) (*Block, error) {
	if err := RequireScopes(readScopes); err != nil {
		return nil, err
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{}
	}
	row := pool.QueryRow(ctx, `
		SELECT `+issueScanCols+`
		FROM context_blocks
		WHERE id = $1 AND NOT is_archived
		  AND type_name = ANY(ARRAY['`+IssueTypeName+`','`+CommentTypeName+`'])
		  AND (scope = ANY($2::text[]) OR id = ANY($3::uuid[]))
		LIMIT 1`,
		id, readScopes, grantedBlockIDs)
	b, err := scanIssue(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIssueNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get issue: %w", err)
	}
	return b, nil
}

// ListComments returns the comment children of parentID, visibility-filtered
// (readScopes + grants), oldest first (thread order). Used by issue-get to
// hydrate the thread. Fail-closed on empty readScopes.
func ListComments(ctx context.Context, pool *pgxpool.Pool, parentID string, readScopes, grantedBlockIDs []string) ([]*Block, error) {
	if err := RequireScopes(readScopes); err != nil {
		return nil, err
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{}
	}
	rows, err := pool.Query(ctx, `
		SELECT `+issueScanCols+`
		FROM context_blocks
		WHERE parent_id = $1::uuid AND NOT is_archived
		  AND type_name = '`+CommentTypeName+`'
		  AND (scope = ANY($2::text[]) OR id = ANY($3::uuid[]))
		ORDER BY created_at ASC, id ASC`,
		parentID, readScopes, grantedBlockIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list comments: %w", err)
	}
	defer rows.Close()
	var out []*Block
	for rows.Next() {
		b, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan comment: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// IssueListQuery is the board/list page request. It is a thin issue-typed façade
// over WorkflowListQuery (the I-B primitive ListWorkflowBlocks) — one listing
// mechanism, no duplicate keyset/merge logic (M072/M075 no-duplication line).
type IssueListQuery struct {
	Scopes   []string // caller ReadScopes intersection (fail-closed: empty ⇒ error)
	Status   string   // one board column; "" ⇒ per-status merge over Statuses
	Statuses []string // the issue type's config status set (required when Status "")
	Labels   []string // AND post-filter over metadata->'labels' (§3.3)
	Limit    int      // clamped ≤ 100
	Cursor   *WorkflowCursor
}

// ListIssues returns one keyset page of issue blocks + the next cursor. It fixes
// TypeName to "issue" and delegates to ListWorkflowBlocks: status-filtered = one
// index range scan; status-unfiltered = the per-status k-way merge (§3.3/§6.2).
func ListIssues(ctx context.Context, pool *pgxpool.Pool, q IssueListQuery) ([]WorkflowBlockRow, *WorkflowCursor, error) {
	return ListWorkflowBlocks(ctx, pool, WorkflowListQuery{
		Scopes:   q.Scopes,
		TypeName: IssueTypeName,
		Status:   q.Status,
		Statuses: q.Statuses,
		Labels:   q.Labels,
		Limit:    q.Limit,
		Cursor:   q.Cursor,
	})
}

// UpdateIssueBlock applies a by-id PATCH to an issue, validating a workflow
// status transition against POLICY DATA (set.ValidateTransition — the mechanism,
// the state set is type config) so an invalid transition yields a blocktype
// error the handler maps to 422. The block must be writable (scope ∈
// writableScopes) — a foreign/absent id ⇒ ErrIssueNotFound (no oracle). Title
// keeps its "#L<seq>" prefix; metadata is JSONB-merged (local_seq survives);
// a content/title change clears the embedding so the scheduler re-embeds.
func UpdateIssueBlock(ctx context.Context, tx pgx.Tx, id string, u IssueUpdate, set *blocktype.Set, writableScopes []string) (*Block, error) {
	// I7/S3b, ahead of the lookup: this is the one client-supplied metadata map
	// in the tree that is MERGED (`metadata = metadata || $n::jsonb`) rather
	// than replaced, so the key would survive on a row it was never written to.
	if err := rejectReservedMetadata(u.Metadata); err != nil {
		return nil, err
	}
	var typeName, curStatus, curTitle, scope string
	// The type restriction is part of the lookup, exactly as in GetIssue
	// (:278-285): this is an issue-domain surface, not a general block writer.
	//
	// W01-2a Nachbesserung (review finding #1, blocker): without it this was
	// the SIXTH write surface and the only one that walks past S3 completely —
	// it never touches store.UpdateBlock, so that function's provenance
	// exclusion cannot see it, and `typeName` was consulted only when a status
	// transition was requested. A PATCH carrying just content/metadata
	// therefore replaced a derivative's content AND its provenance
	// (`metadata = metadata || $n::jsonb`) and answered 200 — the Gate-4 vector
	// through a different verb. Restricting by TYPE closes the derived case and
	// the wider pre-existing class ("an issue verb writes arbitrary blocks") in
	// the same conjunct, and it keeps the no-oracle contract: a non-issue id is
	// ErrIssueNotFound, like a foreign one.
	err := tx.QueryRow(ctx,
		`SELECT type_name, COALESCE(workflow_status,''), title, scope
		 FROM context_blocks
		 WHERE id = $1 AND NOT is_archived
		   AND type_name = ANY(ARRAY['`+IssueTypeName+`','`+CommentTypeName+`'])
		 FOR UPDATE`,
		id).Scan(&typeName, &curStatus, &curTitle, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIssueNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: update issue lookup: %w", err)
	}
	if !scopeIn(scope, writableScopes) {
		return nil, ErrIssueNotFound // uniform no-oracle: not writable = not found
	}
	if u.Content != nil && len(*u.Content) > maxIssueBodyBytes {
		return nil, ErrIssueBody
	}

	set2 := setClauseBuilder{}
	if u.Status != nil && *u.Status != curStatus {
		if set == nil {
			return nil, fmt.Errorf("store: update issue: type registry not available for transition check")
		}
		if err := set.ValidateTransition(typeName, curStatus, *u.Status); err != nil {
			return nil, err // blocktype sentinel → handler 422
		}
		set2.bind("workflow_status = %s", *u.Status)
	}
	if u.Title != nil {
		prefix := ""
		if m := issuePrefixRe.FindStringSubmatch(curTitle); m != nil {
			prefix = m[1] + " "
		}
		set2.bind("title = %s", prefix+*u.Title)
	}
	if u.Content != nil {
		set2.bind("content = %s", *u.Content)
	}
	if u.Tags != nil {
		set2.bind("tags = %s", u.Tags)
	}
	if u.Metadata != nil {
		set2.bind("metadata = metadata || %s::jsonb", u.Metadata)
	}
	if len(set2.clauses) == 0 {
		return nil, fmt.Errorf("store: update issue: no fields to update")
	}
	if u.Content != nil || u.Title != nil {
		set2.addLiteral("embedding = NULL")
	}
	set2.addLiteral("updated_at = now()")

	query := fmt.Sprintf(`UPDATE context_blocks SET %s WHERE id = $%d RETURNING %s`,
		strings.Join(set2.clauses, ", "), len(set2.args)+1, issueScanCols)
	set2.args = append(set2.args, id)
	b, err := scanIssue(tx.QueryRow(ctx, query, set2.args...))
	if err != nil {
		return nil, fmt.Errorf("store: update issue: %w", err)
	}
	return b, nil
}

// setClauseBuilder assembles a parametrised SET list for the dynamic issue
// update: bind(tmpl, arg) appends arg and rewrites the single %s in tmpl to the
// next bind placeholder ($1, $2, …); addLiteral appends a no-argument clause.
type setClauseBuilder struct {
	clauses []string
	args    []any
}

func (b *setClauseBuilder) bind(tmpl string, arg any) {
	b.args = append(b.args, arg)
	b.clauses = append(b.clauses, fmt.Sprintf(tmpl, fmt.Sprintf("$%d", len(b.args))))
}

func (b *setClauseBuilder) addLiteral(clause string) {
	b.clauses = append(b.clauses, clause)
}
