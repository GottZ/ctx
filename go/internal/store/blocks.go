// blocks.go — block CRUD operations against the context_blocks table.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/clustersql"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/redact"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

// Block represents a full context_blocks row.
type Block struct {
	ID          string            `json:"id"`
	Category    string            `json:"category"`
	Tags        []string          `json:"tags"`
	Title       string            `json:"title"`
	Content     string            `json:"content"`
	Metadata    map[string]any    `json:"metadata"`
	Scope       string            `json:"scope"`
	GuardStatus string            `json:"guard_status,omitempty"`
	// Sensitivity + SensitivitySource (M055, F3-P3): trust-gate classification.
	// Only the paths that RETURN the columns fill them (upsert/update/get);
	// older list shapes leave them empty (omitempty).
	Sensitivity       string    `json:"sensitivity,omitempty"`
	SensitivitySource string    `json:"sensitivity_source,omitempty"`
	// TypeName/LifecycleState/TypeSource (WF T10, design/01 §7-T10): the two
	// block-type axes + provenance go wire-visible. Wire name for type_name
	// is `type` — the registry vocabulary the UI badges consume; type_source
	// carries the auto/manual provenance (M071). Filled by the paths whose
	// SELECT returns them (upsert/update/get); older shapes leave "" (omitempty).
	TypeName       string `json:"type,omitempty"`
	LifecycleState string `json:"lifecycle_state,omitempty"`
	TypeSource     string `json:"type_source,omitempty"`
	// WorkflowStatus (M077, Achse 02): the per-block workflow state VALUE (the
	// SET of valid states is type-config policy). Filled only by the issue paths
	// (InsertIssueBlock/GetIssue/UpdateIssueBlock/ListWorkflowBlocks); every
	// other SELECT leaves it "" (omitempty), so no existing wire shape changes.
	WorkflowStatus string `json:"workflow_status,omitempty"`
	// Untrusted is retrieval.untrusted of the block's TYPE (V-11, design/02
	// §5.1 BA7 layer 3): the block carries FOREIGN TEXT — captured tool output,
	// a session transcript, material a third party authored — and a reader must
	// treat it as observation data, never as instruction or as a first-party
	// fact. Filled from the registry snapshot the CALLER holds, never from a
	// type-name list here (the framing belongs to the type, so a second
	// foreign-text type must not require a change in this package).
	//
	// A nil set means "this caller has no registry" and leaves the field false,
	// which omitempty turns into an ABSENT key: no statement, rather than the
	// positive claim "trusted". Write paths pass nil for exactly that reason —
	// their answer keeps its pre-V-11 bytes.
	Untrusted      bool   `json:"untrusted,omitempty"`
	// UntrustedOrigin is the first type marked retrieval.untrusted that a
	// re-type took this block out of (issue #280, migration 174): the text came
	// from an agent session — tool output, a transcript, a distillate — and the
	// block now sits in a type its readers no longer frame. Written only by the
	// trigger trg_block_retype, never by a request, and kept through every later
	// re-type and edit. "" (omitempty: an absent key) for a block that never
	// left such a type. Filled by the reads that carry the block to a reader
	// (GetBlock, GetArchivedBlock) and by UpdateBlock's answer, so a by-id
	// re-type answers with the record it caused. UpsertBlock's answer leaves it
	// out on purpose: its statement is the write every arm and the migration
	// tests on an older schema run (Präambel 6), and the store surfaces answer
	// a write, not a read.
	UntrustedOrigin string `json:"untrusted_origin,omitempty"`
	// Archived marks a block answered by GetArchivedBlock, the one read that
	// returns an archived row (issue #201: the opt-in `include_archived` of MCP
	// get and manage get). Every other path leaves it false — the read paths
	// return live rows only, and DeleteBlock's RETURNING does not fill it —
	// which omitempty turns into an ABSENT key: no existing response changes
	// its bytes.
	Archived    bool              `json:"archived,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// SensitivityWrite carries the sensitivity intent of one block write.
// Value "" = leave the column to the DDL default (internal pipeline callers:
// digest index, dream report, ingest — G41 audits them out of the default).
// Manual=true = user-explicit classification: sensitivity_source='manual',
// and on upsert-conflict the value applies only UPGRADING (a write-path
// downgrade would bypass the confirm-gated update path, F3 §3.5).
// Detector=true = the G40 credentials pattern scanner fired:
// sensitivity_source='pattern' (veto-protected against the G41 audit, which
// only re-touches source='default'), value forced to credentials. On conflict
// it re-stamps only on a STRICT elevation (>), so an already-credentials block
// — manual or not — is left untouched (manual stays untantastbar).
// Derived=true = the write comes from the derived layer (design/01 §4.8.1):
// sensitivity_source='derived', value = the folded source maximum. See the
// field's own comment — it is the SERVER-path badge as well.
type SensitivityWrite struct {
	Value    backends.Sensitivity
	Manual   bool
	Detector bool

	// Derived marks a write of the derived layer (Wissens-Ebenen W01-5). It
	// does TWO things, and they are one decision rather than two:
	//
	//  1. sensitivity_source='derived' with the STRICT '>' on conflict
	//     (§4.8.2). Not '>=' like Manual: value and source sit in the SAME
	//     CASE expression below, so at an equal level '>=' would re-stamp
	//     source='derived' over an existing 'manual' — a human classification
	//     lost silently, without the value moving at all. With '>' a
	//     regeneration that loses its credentials source lowers nothing (B6),
	//     and manual survives.
	//
	//  2. It is the ARM's badge at the I7/S3 provenance guard further down
	//     (§4.3.1). That guard is unconditional today, which is why W01-2a
	//     wrote "the arm has to declare itself here when it lands" into it.
	//     This is that declaration. Its scope is exactly the arm's own need:
	//     rewrite a block that already carries a provenance, and re-create one
	//     whose identity is held by an ARCHIVED provenance block (§4.7.5 makes
	//     revival a fresh upsert, never is_archived=false, so without the
	//     second half an archived derivative would be a permanent tombstone on
	//     its own title).
	//
	// It is NOT client-reachable and must not become so. Three properties carry
	// that, and the first version of this comment named the wrong one: it said
	// "it carries no json tag", which is the OPPOSITE of a protection —
	// encoding/json decodes an exported field WITHOUT a tag under its own name,
	// case-insensitively, so `{"derived":true}` set it (W01-5 review #3a).
	// The properties that actually hold:
	//
	//  1. `json:"-"` — the only spelling that makes the field invisible to both
	//     Unmarshal and Marshal.
	//  2. Nothing decodes into a SensitivityWrite anywhere in the tree. The
	//     staged, HASH-BOUND payload is store.CanonicalWrite, which has no
	//     counterpart field, and the handlers build Value/Manual/Detector by
	//     hand (storeSensitivity, confirm_core).
	//  3. No production package outside internal/store sets it, in any form —
	//     composite key or assignment.
	//
	// resolve_sources_pin_test.go pins all three; (3) is checked over the parsed
	// syntax tree, not by grepping for a literal.
	//
	// Detector wins over Derived for the SOURCE value: a scanner hit is a
	// statement about this block's content, the fold is a statement about its
	// sources, and the content is the sharper evidence. The badge in (2) is
	// unaffected — who writes does not change because what was written scanned
	// positive.
	Derived bool `json:"-"`
}

// untrustedOf resolves retrieval.untrusted of one type name against the
// registry snapshot the CALLER holds (V-11, design/02 §5.1 BA7 layer 3).
//
// The set is a PARAMETER of the read functions rather than something resolved
// in here, and that is the point of the wave: BA7's finding was that the
// untrusted framing had exactly one consumption site in the whole tree, so
// every other reader lost it silently. Making the snapshot part of the
// signature means a new reader cannot get a BlockPreview or a Block without
// deciding about the framing — the compiler asks the question.
//
// nil answers false: "no registry, no statement". Combined with omitempty on
// both fields that yields an ABSENT key, never the positive claim "trusted".
//
// The framing's own fail-open direction — an UNKNOWN type name resolves to
// false inside blocktype.Set.IsUntrusted, with the empty name as its real
// caller — is a property of the registry lookup and is deliberately left
// untouched here: it is a separately tracked axis (BA7), not something this
// seam may quietly re-decide.
func untrustedOf(set *blocktype.Set, typeName string) bool {
	if set == nil {
		return false
	}
	return set.IsUntrusted(typeName)
}

// untrustedOriginCol is the select expression of Block.UntrustedOrigin and
// BlockPreview.UntrustedOrigin (issue #280): the nullable column as text, ""
// for a block without a record, so the scan target stays a plain string and
// omitempty keeps such a block's bytes.
const untrustedOriginCol = `COALESCE(untrusted_origin, '')`

// ErrRetypeBound is the refusal of a write that would change the type of a
// block of the issue domain — an issue or a comment (issue #280):
// trg_block_retype (migration 174) raises check_violation on
// retypeBoundConstraint inside the statement, so the row the decision reads is
// the row the statement locked. A sentinel for the reason
// ErrProvenanceProtected is one: the write surfaces answer it as a refusal,
// not as the 500 of an unexpected store error.
var ErrRetypeBound = errors.New("store: block is an issue or a comment — its type stays")

// retypeBoundConstraint is the constraint name trg_block_retype raises with.
const retypeBoundConstraint = "ck_context_blocks_retype_bound"

// retypeBound reports the trigger's refusal and no other check violation.
func retypeBound(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == retypeBoundConstraint
}

// UntrustedOriginNote is the one wording of Block.UntrustedOrigin on a TEXT
// surface (issue #280): the MCP query tool's "Sources:" list, MCP recent and
// the CLI's query output put it on a line of its own under the row it belongs
// to, so a row without a record keeps its exact text and a parser that reads
// a source line up to its id still reads this one. "" for "" — the caller
// writes no line. ASCII, like the other markers of those surfaces, so it
// survives a transport that drops non-ASCII runes. The JSON surfaces carry
// the same value as the `untrusted_origin` field.
func UntrustedOriginNote(origin string) string {
	if origin == "" {
		return ""
	}
	return "untrusted_origin:" + origin + " (agent-produced text: re-typed out of a type marked untrusted)"
}

// UntrustedOrigins reads the untrusted_origin record (issue #280) of the
// given blocks: id → origin, for the ids that carry one. It serves the query
// sources, whose retrieval rows come out of ctx_rrf, graph expansion, the
// cluster injection and the aggregate fold — four shapes, one of which
// (ctx_rrf) is a SQL function whose result type a new column would have to
// re-declare. One primary-key read over the final source ids (the user limit,
// not the candidate set) instead. The ids come out of the caller's
// scope-filtered retrieval; the statement returns nothing else about them.
func UntrustedOrigins(ctx context.Context, q pgxdb.Querier, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx,
		`SELECT id::text, untrusted_origin FROM context_blocks
		  WHERE id = ANY($1::uuid[]) AND untrusted_origin IS NOT NULL`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: untrusted origins: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, origin string
		if err := rows.Scan(&id, &origin); err != nil {
			return nil, fmt.Errorf("store: untrusted origins scan: %w", err)
		}
		out[id] = origin
	}
	return out, rows.Err()
}

// sensRankSQL renders the sensitivity rank of a SQL expression — must mirror
// backends.sensRank (credentials=3 … public=0).
func sensRankSQL(expr string) string {
	return fmt.Sprintf(
		`(CASE %s WHEN 'credentials' THEN 3 WHEN 'personal' THEN 2 WHEN 'internal' THEN 1 ELSE 0 END)`,
		expr)
}

// BlockPreview is a compact search result (no full content).
type BlockPreview struct {
	ID             string    `json:"id"`
	Category       string    `json:"category"`
	Tags           []string  `json:"tags"`
	Title          string    `json:"title"`
	ContentPreview string    `json:"content_preview,omitempty"`
	ContentLength  int       `json:"content_length,omitempty"`
	Content        string    `json:"content,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Scope          string    `json:"scope"`
	// Sensitivity is the trust-gate level (M055) for the W6 list badge.
	// SearchBlocks selects the column (both the compact and full SELECT) and
	// scans it here; older callers that don't select it leave it "" (omitempty).
	Sensitivity    string    `json:"sensitivity,omitempty"`
	// Type axes (WF T10): filled by SearchBlocks + RecentBlocks; callers
	// whose SELECT doesn't return them leave "" (omitempty).
	TypeName       string    `json:"type,omitempty"`
	LifecycleState string    `json:"lifecycle_state,omitempty"`
	TypeSource     string    `json:"type_source,omitempty"`
	// Untrusted is retrieval.untrusted of TypeName — see Block.Untrusted for
	// what it means and why a nil registry set leaves the key absent.
	Untrusted      bool      `json:"untrusted,omitempty"`
	// UntrustedOrigin — see Block.UntrustedOrigin. Filled by SearchBlocks
	// (compact and full) and RecentBlocks; "" stays absent.
	UntrustedOrigin string   `json:"untrusted_origin,omitempty"`
	Score          float64   `json:"score,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	// CreatedAt is selected by every statement that fills this struct
	// (SearchBlocks compact and full, RecentBlocks). `omitzero`, not
	// `omitempty`: omitempty never drops a struct value, so a statement that
	// left the column out would answer year 1 as if it were a date (#205).
	CreatedAt      time.Time `json:"created_at,omitzero"`
}

// BlockMeta is a lightweight listing entry (no content, no metadata).
type BlockMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Category  string    `json:"category"`
	Tags      []string  `json:"tags"`
	Scope     string    `json:"scope"`
	// Type axes (WF T10): filled by ListMeta; the ResolveBlockID matches
	// shape doesn't select them ("" + omitempty keeps its wire unchanged).
	TypeName       string `json:"type,omitempty"`
	LifecycleState string `json:"lifecycle_state,omitempty"`
	TypeSource     string `json:"type_source,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CategoryCount holds a category name and its block count.
type CategoryCount struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// Stats holds aggregate context store statistics.
type Stats struct {
	TotalBlocks      int    `json:"total_blocks"`
	TotalCategories  int    `json:"total_categories"`
	OldestEntry      *time.Time `json:"oldest_entry"`
	NewestEntry      *time.Time `json:"newest_entry"`
	DBSize           string `json:"db_size"`
}

// GuardListItem represents a flagged block in the guard list.
type GuardListItem struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Category     string  `json:"category"`
	Scope        string  `json:"scope"`
	Type         string  `json:"type_name"`
	GuardStatus  string  `json:"guard_status"`
	Similarity   *string `json:"similarity"`
	MatchedID    *string `json:"matched_id"`
	MatchedTitle *string `json:"matched_title"`
	CheckedAt    *string `json:"checked_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// GuardSkip reports why a requested id was not resolved by GuardResolveBatch.
// Cross-scope and nonexistent ids collapse into the same reason on purpose
// (no existence oracle — the structural-links doctrine).
type GuardSkip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"` // invalid_id | not_found | already_archived | not_flagged
}

// GuardStats holds guard state statistics.
type GuardStats struct {
	TotalBlocks     int        `json:"total_blocks"`
	Active          int        `json:"active"`
	Clean           int        `json:"clean"`
	NeedsReview     int        `json:"needs_review"`
	NearDuplicate   int        `json:"near_duplicate"`
	Unchecked       int        `json:"unchecked"`
	ArchivedDups    int        `json:"archived_dups"`
	WriteLogEntries int        `json:"write_log_entries"`
	LastGuardAt     *time.Time `json:"last_guard_at"`
}

// ClampLimit constrains a limit value between defaultVal (when <= 0) and maxVal.
func ClampLimit(limit, defaultVal, maxVal int) int {
	if limit <= 0 {
		return defaultVal
	}
	if limit > maxVal {
		return maxVal
	}
	return limit
}

// identicalContentWhere is the identity predicate of IdenticalContentMatch:
// the live row of ($2 scope, $3 category, $4 title) — the upsert's conflict
// identity — whose content hashes like $1. Not archived, because the ON
// CONFLICT target is partial on NOT is_archived. The integration gates in
// blocks_identical_match_integration_test.go pin this predicate through
// IdenticalContentMatch.
const identicalContentWhere = `content_hash = encode(digest($1, 'sha256'), 'hex')
		   AND NOT is_archived
		   AND scope = $2
		   AND category = $3
		   AND title = $4`

// guardVerdictSQL is the duplicate guard's part of the metadata document doc:
// every key with the guard_ prefix (the decision applyDecision writes, the
// person's trace GuardResolve writes, the repair marks of M121/M156/M157), as a
// jsonb object — '{}' when there is none, and for a document that is NULL or
// not an object.
//
// The guard decides on the embedding (ctx_guard_check reads embedding and
// scope, never tags or metadata), so these keys are a verdict about the
// CONTENT. identicalMetadataSQL lets them win over a request's guard_* keys
// for exactly the reason UpsertBlock keeps the embedding on identical content
// (W04-8): nothing they were derived from has changed.
func guardVerdictSQL(doc string) string {
	return `CASE WHEN jsonb_typeof(` + doc + `) = 'object' THEN COALESCE((
		  SELECT jsonb_object_agg(g.key, g.value) FROM jsonb_each(` + doc + `) AS g
		   WHERE starts_with(g.key, 'guard_')), '{}'::jsonb)
		ELSE '{}'::jsonb END`
}

// identicalMetadataSQL is the metadata an upsert writes when the stored
// content is IDENTICAL to the incoming one (issue #64): the incoming document,
// minus guard_checked_at, MERGED into the stored one, with the stored guard
// verdict on top.
//
//   - Merged, not replaced: keys the request does not name stay. Those are the
//     keys other writers put there — the guard's verdict, the sensitivity
//     audit trail of a confirmed downgrade (UpdateBlock), the M154
//     supersedes_repair record, the forge/issue sync keys, a key set through
//     manage update. Every one of them is a statement about a content that has
//     not changed, and a replace would delete them on a byte-identical
//     re-save of a client that never sent them. A key the request does name
//     takes the request's value: the second write is the truth for what it
//     says (session_id, project, agent_type of `ctx persist --hook`).
//   - The stored guard verdict on top: a request cannot forge or reset it.
//     Wiping it would put the block back into the guard queue for an
//     unchanged vector, and a block a person KEPT through guard-resolve could
//     then be archived by the re-check (operations.md, "A guard-resolve
//     trace").
//
// stored is a column reference and incoming an already-typed jsonb
// expression; both are code-owned SQL text, never input. A stored document
// that is NULL or not an object counts as '{}'. A content CHANGE keeps the
// replace semantics (UpsertBlock): the annotations then describe a content
// that is gone.
func identicalMetadataSQL(stored, incoming string) string {
	return `((CASE WHEN jsonb_typeof(` + stored + `) = 'object' THEN ` + stored + ` ELSE '{}'::jsonb END)
		|| (` + incoming + ` - 'guard_checked_at')
		|| ` + guardVerdictSQL(stored) + `)`
}

// sensConflict is how UpsertBlock's ON CONFLICT clause treats a sensitivity
// intent: whether it touches the two sensitivity columns at all, the rank
// comparison that decides an upgrade, and the source it stamps. ONE function
// for UpsertBlock and IdenticalContentMatch, so the noop decision cannot drift
// from the write it predicts.
func sensConflict(sens SensitivityWrite) (applies bool, op, source string) {
	source = "default"
	switch {
	case sens.Detector:
		source = "pattern"
	case sens.Manual:
		source = "manual"
	case sens.Derived:
		source = "derived"
	}
	if sens.Value == "" || (!sens.Manual && !sens.Detector && !sens.Derived) {
		return false, "", source
	}
	// Upgrade-only on conflict: a write-path downgrade would bypass the
	// confirm-gated update path (F3 §3.5). Manual uses >= (re-asserting the
	// same level re-stamps source='manual'); the pattern detector and the
	// derived fold use strict > so they re-stamp only on a real elevation and
	// never flip an already-credentials block's source (manual stays intact —
	// design/01 §4.8.2).
	op = ">="
	if sens.Detector || sens.Derived {
		op = ">"
	}
	return true, op, source
}

// IdenticalContent is IdenticalContentMatch's answer for one write.
type IdenticalContent struct {
	// ID is the live block of the write's (category, title, scope) whose
	// content is byte-identical to the write's — "" when there is none.
	ID string
	// The four report whether the upsert of the write would leave that part
	// of the row as it is. Meaningless when ID is "".
	TagsEqual        bool
	MetadataEqual    bool
	SensitivityEqual bool
	TypeEqual        bool
}

// Unchanged reports whether the write is a true no-op: identical content AND
// nothing the upsert would rewrite differs.
func (m IdenticalContent) Unchanged() bool {
	return m.ID != "" && m.TagsEqual && m.MetadataEqual && m.SensitivityEqual && m.TypeEqual
}

// Changed names what an identical-content upsert of the write would rewrite,
// in a fixed order. nil when there is no identical-content block — the write
// is then an ordinary create or content update.
func (m IdenticalContent) Changed() []string {
	if m.ID == "" {
		return nil
	}
	changed := make([]string, 0, 4)
	if !m.TagsEqual {
		changed = append(changed, "tags")
	}
	if !m.MetadataEqual {
		changed = append(changed, "metadata")
	}
	if !m.SensitivityEqual {
		changed = append(changed, "sensitivity")
	}
	if !m.TypeEqual {
		changed = append(changed, "type")
	}
	return changed
}

// IdenticalContentMatch reports everything the upsert of the same write would
// rewrite (issue #64). A content-hash-only check that answers "noop" drops the
// request's tags and metadata behind a success answer — `ctx persist --hook`
// from a second project leaves the block tagged with the first one and counts
// it as saved — and with them an explicit sensitivity upgrade or type.
//
// The arguments are UpsertBlock's, and each part is judged against what
// UpsertBlock would WRITE for them on this row, not against the raw request:
//
//   - tags as given (nil = empty; the array order is part of the stored value);
//   - metadata after the credentials detector, through identicalMetadataSQL —
//     the very expression UpsertBlock assigns on identical content;
//   - sensitivity through sensConflict: equal unless the conflict clause would
//     fire AND produce another value or source (a downgrade is not applied, so
//     it changes nothing);
//   - type: equal unless typeName is set and the row is not already that type
//     with type_source='manual'.
//
// NULL in the stored row compares like the empty value the upsert writes.
// metadata is not modified; the detector runs on a copy.
func IdenticalContentMatch(ctx context.Context, pool *pgxpool.Pool, content, scope, category, title string,
	tags []string, metadata map[string]any, sens SensitivityWrite, typeName string,
) (IdenticalContent, error) {
	if tags == nil {
		tags = []string{}
	}
	sens, md, _ := ApplyWriteDetector(content, sens, maps.Clone(metadata))
	if md == nil {
		md = map[string]any{}
	}
	args := []any{content, scope, category, title, tags, md}

	sensEqual := "true"
	if applies, op, source := sensConflict(sens); applies {
		args = append(args, string(sens.Value), source)
		v, s := len(args)-1, len(args)
		sensEqual = fmt.Sprintf(
			`NOT (%s %s %s AND (sensitivity IS DISTINCT FROM $%d::text OR sensitivity_source IS DISTINCT FROM $%d::text))`,
			sensRankSQL(fmt.Sprintf("$%d::text", v)), op, sensRankSQL("sensitivity"), v, s)
	}
	typeEqual := "true"
	if typeName != "" {
		args = append(args, typeName)
		typeEqual = fmt.Sprintf(
			`(type_name IS NOT DISTINCT FROM $%d::text AND type_source IS NOT DISTINCT FROM 'manual')`, len(args))
	}

	var m IdenticalContent
	err := pool.QueryRow(ctx,
		`SELECT id,
		        COALESCE(tags, '{}') = $5::text[],
		        COALESCE(metadata, '{}'::jsonb) = `+identicalMetadataSQL("metadata", "$6::jsonb")+`,
		        `+sensEqual+`,
		        `+typeEqual+`
		   FROM context_blocks
		  WHERE `+identicalContentWhere+`
		  LIMIT 1`,
		args...,
	).Scan(&m.ID, &m.TagsEqual, &m.MetadataEqual, &m.SensitivityEqual, &m.TypeEqual)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdenticalContent{}, nil
	}
	if err != nil {
		return IdenticalContent{}, fmt.Errorf("store: identical content match: %w", err)
	}
	return m, nil
}

// ErrProvenanceProtected is the S3 refusal (design D-01 §4.3.1, invariant I7):
// the write would replace a block that carries a derived provenance object. It
// is a SENTINEL and not a message because the write surfaces have to answer it
// with 403 rather than with the generic 500 of an unexpected store error —
// errors.Is is what keeps that mapping out of string matching.
var ErrProvenanceProtected = errors.New("store: block carries derived provenance — not client-writable")

// notDerivedSQL is the S3 exclusion the by-id client writes carry in their
// WHERE (UpdateBlock, DeleteBlock, GuardResolve archive): the row holds no
// derived provenance. metadata is nullable (DEFAULT '{}'), and a NULL holds no
// provenance key — so the test is COALESCEd to false. A plain
// `NOT (metadata ? …)` is NULL for such a row, the statement matched nothing,
// and a block that is no derivative answered "not found" to a key that may
// write it (and a staged write was carded and then failed at confirm).
const notDerivedSQL = `NOT COALESCE(metadata ? '` + derived.MetadataKey + `', false)`

// ProvenanceClaimed reports whether the upsert identity (category, title, scope)
// is claimed by a derivative — INCLUDING an archived one.
//
// The archived half is the point (W01-2a Nachbesserung, review finding #5): the
// unique index behind the ON CONFLICT target is partial on `NOT is_archived`, so
// archiving a derivative FREES its identity. Two client calls — manage delete,
// then an upsert — would otherwise walk around S3 entirely. The archive verbs
// now refuse a provenance row as well (DeleteBlock, GuardResolve 'archive'), so
// this probe is the second line: it also covers a row archived before this wave
// or by a path outside the Go code.
//
// Scale: the predicate leads with `metadata ? 'provenance'`, which the GIN index
// on metadata (idx_context_metadata, 113_baseline.sql:236) serves directly, so
// the scan is bounded by the number of DERIVATIVES rather than by the corpus.
// The upsert calls it only on the INSERT path (no live conflicting row), never
// on the hot conflict path.
//
// q is a one-row handle — satisfied by both *pgxpool.Pool (the ingest
// pre-flight) and pgx.Tx (inside the upsert's own transaction), so the
// statement exists once.
func ProvenanceClaimed(ctx context.Context, q pgxdb.Rower, category, title, scope string) (bool, error) {
	var claimed bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM context_blocks
		    WHERE metadata ? '`+derived.MetadataKey+`'
		      AND category = $1 AND title = $2 AND scope = $3
		 )`, category, title, scope).Scan(&claimed); err != nil {
		return false, fmt.Errorf("store: provenance claim probe: %w", err)
	}
	return claimed, nil
}

// provenanceIdentity is WHO a derivative belongs to: the arm that produced it
// and the level it was produced at (§3.2 `arm` and `stratum`).
//
// It exists because the badge alone said nothing about ownership. W01-5's first
// version let any badged write take over any provenance row — a second arm
// could claim the first one's (category, title, scope), and "whoever writes
// last wins" would have been the semantics the moment D-02 and D-03 both ran
// (review finding #2). Both fields are compared as TEXT: the stratum is read
// out of JSON, where a non-numeric value is representable, and a ::int cast on
// that path would answer 22P02 instead of a contract refusal.
type provenanceIdentity struct {
	arm     string
	stratum string
}

// named reports whether the identity is usable as one. An empty arm is NOT: two
// unnamed derivatives would compare equal, and the whole point of the pair is
// that they do not. §3.2 makes `arm` a field of every provenance, so this asks
// the writer for nothing new — it only stops treating "absent" as "matches".
func (p provenanceIdentity) named() bool { return p.arm != "" }

// writeProvenanceIdentity extracts the identity from the metadata the caller is
// about to write. It goes through derived.Provenance rather than reading the
// map by hand, which makes the contract version a precondition of the badge:
// UnmarshalJSON refuses anything that is not ContractVersion (§3.2 "Decode
// lehnt Unbekanntes ab"), so a struct from another build cannot pass here.
//
// ok=false is the fail-closed answer for every shape that is not a v=1
// provenance — absent key, wrong version, unparseable — and the guard turns it
// into ErrProvenanceProtected.
func writeProvenanceIdentity(metadata map[string]any) (provenanceIdentity, bool) {
	raw, present := metadata[derived.MetadataKey]
	if !present {
		return provenanceIdentity{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return provenanceIdentity{}, false
	}
	var p derived.Provenance
	if err := json.Unmarshal(encoded, &p); err != nil {
		return provenanceIdentity{}, false
	}
	return provenanceIdentity{arm: p.Arm, stratum: strconv.Itoa(int(p.Stratum))}, true
}

// foreignProvenanceClaim reports whether the upsert identity is claimed — live
// or ARCHIVED — by a derivative that does NOT belong to the given writer.
//
// It is the revival half of the ownership binding. §4.7.5 archives a derivative
// whose topic died and forbids reviving it with is_archived=false (B11: that
// collides on the partial unique index), so revival is a fresh upsert; without
// this probe the fresh upsert would let ANY arm take a title that another arm's
// archived block still holds. The predicate is ProvenanceClaimed's, plus the
// identity mismatch — same GIN-served leading term, same scale properties.
func foreignProvenanceClaim(ctx context.Context, q pgxdb.Rower, category, title, scope string, id provenanceIdentity) (bool, error) {
	var claimed bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM context_blocks
		    WHERE metadata ? '`+derived.MetadataKey+`'
		      AND category = $1 AND title = $2 AND scope = $3
		      AND NOT (COALESCE(metadata->'`+derived.MetadataKey+`'->>'arm', '') = $4
		           AND COALESCE(metadata->'`+derived.MetadataKey+`'->>'stratum', '') = $5)
		 )`, category, title, scope, id.arm, id.stratum).Scan(&claimed); err != nil {
		return false, fmt.Errorf("store: foreign provenance claim probe: %w", err)
	}
	return claimed, nil
}

// guardProvenanceClaim is I7 / S3 (design D-01 §4.3.1, §5.2 B14) for one
// upsert, both halves, and the ONE place the derived layer's server-path badge
// opens it.
//
// It is refused HERE rather than in the handlers because the caller's pre-read
// holds the conflicting row under FOR UPDATE: the check and the write are one
// transaction, so there is no window between them. A guard at the client call
// sites would have that window and would have to be repeated at the next
// surface — there are seven (W01-2a review finding #1).
//
// derivedWriter is SensitivityWrite.Derived, and this is the declaration W01-2a
// asked for when it wrote "the arm has to declare itself here when it lands"
// into the unconditional guard. The seam stayed a seam: it is still
// unconditional for every caller without the badge, and the badge is a Go
// struct field no request body can reach (see SensitivityWrite.Derived for the
// properties that keep it server-side, all pinned in
// resolve_sources_pin_test.go). Why the arm needs it: a derivative is
// regenerated by upserting on its own (category, title, scope) identity (§4.7.2
// — regeneration is an upsert, not a supersede), so without the badge the
// second run of every arm would die on its own first run's provenance.
//
// THE BADGE IS NOT A BYPASS, and W01-5's first version made it one in two ways
// the review found:
//
//   - #1: the badge did not require the write to carry a provenance of its own.
//     The ON CONFLICT clause replaces metadata WHOLESALE whenever the content
//     changes (it merges only on identical content, issue #64), so a badged
//     write with empty metadata DELETED the target's provenance — and
//     from that moment the title was client-writable again, because S3 hangs on
//     `metadata ? 'provenance'`. One aborted arm run reopened B14; no attacker
//     needed. A badged write must therefore name itself.
//   - #2: the badge opened EVERY provenance row, not the writer's own. D-02 and
//     D-03 are two arms. The identity pair (arm, stratum) is the minimal binding
//     that makes "a derivative it owns" a statement about the code rather than
//     about the documentation.
//
// Both halves of S3 are opened, and both are bound:
//
//   - the CONFLICT path, answered by the locked row itself;
//   - the INSERT path, which has to see ARCHIVED rows: the unique index behind
//     the ON CONFLICT target is partial on NOT is_archived, so archiving a
//     derivative frees its identity and an unguarded write would create a fresh
//     block wearing it (W01-2a review finding #5). §4.7.5 archives a derivative
//     whose topic died or whose source set fell below MinSourceCount and
//     forbids reviving it with is_archived=false (B11), so revival is a new
//     upsert and an arm locked out here would be locked out of its own title
//     forever, by its own archived block.
//
// What it deliberately does NOT do: bind the CONTENT of the provenance beyond
// the identity pair, or check that the writer is entitled to the arm name it
// claims. The arm name is trusted the way this layer trusts its caller for
// sensitivity levels and type names — it is a server-path value, and the badge
// that carries it is unreachable from any client.
//
// The client form of the very same write stays refused:
// handler/derived_write_lock_integration_test.go j_S3_Upsert/k_S3_Update answer
// 403 provenance_protected across the surfaces, and W01-5 adds the direct pair
// — same category, title and scope, once with the badge (accepted) and once
// without (refused) — in handler/derived_server_path_integration_test.go.
func guardProvenanceClaim(ctx context.Context, tx pgx.Tx, sens SensitivityWrite, metadata map[string]any,
	hadRow, hadProvenance bool, existing provenanceIdentity, category, title, scope string,
) error {
	if !sens.Derived {
		return guardUnbadged(ctx, tx, hadRow, hadProvenance, category, title, scope)
	}
	writer, ok := writeProvenanceIdentity(metadata)
	if !ok || !writer.named() {
		// #1: a badged write that names no v=1 provenance is treated exactly
		// like any other caller. It cannot strip what it does not carry.
		return guardUnbadged(ctx, tx, hadRow, hadProvenance, category, title, scope)
	}
	if hadRow {
		if hadProvenance && existing != writer {
			return ErrProvenanceProtected
		}
		return nil
	}
	claimed, err := foreignProvenanceClaim(ctx, tx, category, title, scope, writer)
	if err != nil {
		return err
	}
	if claimed {
		return ErrProvenanceProtected
	}
	return nil
}

// guardUnbadged is S3 as every caller without the badge sees it — unchanged
// since W01-2a, and the answer a badged write falls back to when it names no
// provenance of its own.
func guardUnbadged(ctx context.Context, tx pgx.Tx, hadRow, hadProvenance bool, category, title, scope string) error {
	if hadRow {
		if hadProvenance {
			return ErrProvenanceProtected
		}
		return nil
	}
	claimed, err := ProvenanceClaimed(ctx, tx, category, title, scope)
	if err != nil {
		return err
	}
	if claimed {
		return ErrProvenanceProtected
	}
	return nil
}

// UpsertBlock inserts or updates a block by (category, title).
// content_hash is a GENERATED COLUMN and must not be set manually.
// If scopeExplicit is true, scope is included in the ON CONFLICT UPDATE.
// sens carries the sensitivity intent (F3-P3): zero value = DDL default on
// insert, untouched on update; Manual applies upgrade-only on conflict.
// typeName (WF T10, design/01 §4.5/T4 semantics): "" = leave the type axes
// to the DDL default + auto-classifier; non-empty = user-explicit type, sets
// type_source='manual' so the auto-classifier never re-touches the block
// (the classify hook updates only type_source='auto' rows). The HANDLER
// validates the name against the registry snapshot (422 on unknown) — this
// layer trusts its caller like it does for sensitivity levels.
func UpsertBlock(ctx context.Context, pool *pgxpool.Pool, category, title, content string, tags []string, metadata map[string]any, scope string, scopeExplicit bool, sens SensitivityWrite, typeName string) (*Block, error) {
	u := prepareUpsert(category, title, content, tags, metadata, scope, scopeExplicit, sens, typeName)
	var b *Block
	if err := pgxdb.Write(ctx, pool, pgxdb.At("store: upsert block"), func(tx pgx.Tx) error {
		var err error
		b, err = u.exec(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	return b, nil
}

// upsertStatement is one prepared UpsertBlock write: the statement and its
// arguments plus what the in-transaction half needs. prepareUpsert builds it
// without touching the database; exec runs it inside a caller-owned
// transaction — UpsertBlock's own, or UpsertBlockSupersedes', which composes
// the supersedes declaration into the same transaction (issue #197).
type upsertStatement struct {
	query                  string
	args                   []any
	sens                   SensitivityWrite
	metadata               map[string]any
	category, title, scope string
}

// prepareUpsert is the pure half of UpsertBlock (see there for the rules).
func prepareUpsert(category, title, content string, tags []string, metadata map[string]any, scope string, scopeExplicit bool, sens SensitivityWrite, typeName string) upsertStatement {
	if tags == nil {
		tags = []string{}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}

	// G40 credentials detector on EVERY upsert path (Wissens-Ebenen V-W8,
	// design/05 §7 row V-W8 and §5 B3). Before this it ran in the handler only
	// (context_store.go:107, stage_gates.go:81), so digest, dream, rootmap and
	// the ingest block path wrote credentials-bearing content with the DDL
	// defaults and no verdict. The handler keeps its own call — there the
	// verdict must exist BEFORE the write, because the staged path pins it
	// into the hash-bound canonical payload (confirm_payload.go:47-51) — and
	// the two applications are idempotent: upgrade-only, same scan, same
	// single metadata key.
	sens, metadata, _ = ApplyWriteDetector(content, sens, metadata)

	// Reserved metadata keys are stripped from caller-supplied metadata on
	// BOTH upsert paths (Wissens-Ebenen V-W3, design/05 §2.2 row S3): today
	// exactly `guard_checked_at`, in the same expression form UpdateBlock
	// already uses (:715-732). guardPendingWhere (guard/guard.go:65-70) takes
	// a block out of the guard batch as soon as that key is non-NULL — an
	// unfiltered write-through would let any client, and every future derived
	// writer (they all go through UpsertBlock), silently remove its own block
	// from the duplicate check. Fail-open.
	//
	// The INSERT value carries the strip because a fresh block takes the
	// INSERT branch and never sees the ON CONFLICT clause. EXCLUDED.metadata
	// is the already-evaluated insert value, so the conflict clause's strip is
	// belt-and-braces: it keeps the invariant local to the assignment that
	// actually writes the conflicting row.
	//
	// On a conflict whose content is IDENTICAL the metadata is merged, not
	// replaced, and the stored guard verdict wins (identicalMetadataSQL, issue
	// #64): keys other writers put there describe a content that has not
	// changed, and since the REST identical-content shortcut answers noop only
	// when nothing would change, a tags- or metadata-only rewrite reaches this
	// clause routinely. `context_blocks.content` in a SET expression is the
	// stored row's value, whatever the content assignment next to it says. A
	// content change keeps the replace below: the annotations then describe a
	// content that is gone, and the next guard batch re-checks the new vector.
	insertCols := "category, tags, title, content, metadata, scope"
	insertVals := "$1, $2, $3, $4, $5::jsonb - 'guard_checked_at', $6"
	args := []any{category, tags, title, content, metadata, scope}

	setClauses := []string{
		"content = EXCLUDED.content",
		"tags = EXCLUDED.tags",
		"metadata = CASE WHEN context_blocks.content = EXCLUDED.content THEN " +
			identicalMetadataSQL("context_blocks.metadata", "EXCLUDED.metadata") +
			" ELSE EXCLUDED.metadata - 'guard_checked_at' END",
		"updated_at = now()",
	}
	if scopeExplicit {
		setClauses = append(setClauses, "scope = EXCLUDED.scope")
	}
	if sens.Value != "" {
		applies, op, source := sensConflict(sens)
		insertCols += ", sensitivity, sensitivity_source"
		insertVals += ", $7, $8"
		args = append(args, string(sens.Value), source)
		if applies {
			upgrades := fmt.Sprintf("%s %s %s",
				sensRankSQL("EXCLUDED.sensitivity"), op, sensRankSQL("context_blocks.sensitivity"))
			setClauses = append(setClauses,
				fmt.Sprintf("sensitivity = CASE WHEN %s THEN EXCLUDED.sensitivity ELSE context_blocks.sensitivity END", upgrades),
				fmt.Sprintf("sensitivity_source = CASE WHEN %s THEN '%s' ELSE context_blocks.sensitivity_source END", upgrades, source),
			)
		}
	}

	if typeName != "" {
		// Explicit type = manual provenance (T4 semantics: manual overrides
		// the auto-classifier permanently, exactly the sensitivity_source
		// pattern). On conflict the explicit intent overwrites — the caller
		// asserted the type in THIS write.
		insertCols += ", type_name, type_source"
		insertVals += fmt.Sprintf(", $%d, 'manual'", len(args)+1)
		args = append(args, typeName)
		setClauses = append(setClauses,
			"type_name = EXCLUDED.type_name",
			"type_source = 'manual'")
	}

	// The last RETURNING column is the statement's own answer to "did this
	// write replace the content of an existing row" (issue #281): old.* is the
	// conflicting row before the update and NULL on an insert (PostgreSQL 18),
	// so it is true only on the conflict branch with a different content —
	// whatever the pre-read in exec saw.
	query := fmt.Sprintf(`INSERT INTO context_blocks (%s)
		VALUES (%s)
		ON CONFLICT (category, title, scope) WHERE NOT is_archived DO UPDATE SET
			%s
		RETURNING id, category, tags, title, content, metadata, scope, sensitivity, sensitivity_source, type_name, lifecycle_state, type_source, created_at, updated_at,
		          (old.id IS NOT NULL AND old.content IS DISTINCT FROM new.content)`,
		insertCols, insertVals, strings.Join(setClauses, ",\n\t\t\t"))
	return upsertStatement{
		query: query, args: args, sens: sens, metadata: metadata,
		category: category, title: title, scope: scope,
	}
}

// exec is the in-transaction half of UpsertBlock: pre-read, the S3 guard, the
// upsert and the stale-vector clear, in the caller's transaction.
func (u upsertStatement) exec(ctx context.Context, tx pgx.Tx) (*Block, error) {
	category, title, scope := u.category, u.title, u.scope

	// Achse 04 W04-8 (design/04-reembed-migration.md §3.4/§7, Inventur §3.4
	// point 8): a conflicting upsert that CHANGES content must invalidate the
	// stale vector the same way manage-update/MCP-update always did — before
	// this fix, ON CONFLICT only ever set content = EXCLUDED.content, never
	// touched embedding/embed_model, so `ctx save` on an existing title with
	// new content left the OLD vector attributed to the NEW content, unmarked
	// and out of the backfill's reach (context_store.go:184-236 → blocks.go).
	// Since issue #198 part B the vector stays until the backfill replaces it,
	// marked stale (InvalidateEmbeddingTx). W04-4's re-embed migration
	// convergence invariant (design §4.3) has this as a hard precondition:
	// without it, an in-flight migration's embedding_next could go stale the
	// same way.
	//
	// Everything below runs in ONE tx: the pre-read, the upsert, and (on a
	// real content change) the invalidation are atomic together — "der Upsert und
	// das Clear gehören in EINE Tx" (design §7).
	//
	// Whether the content changed is the upsert statement's own answer (its
	// last RETURNING column, see prepareUpsert), not the pre-read's (issue
	// #281). The pre-read locks the conflicting row (if any) with FOR UPDATE
	// before the upsert runs, using the exact (category, title, scope) WHERE
	// NOT is_archived predicate of the ON CONFLICT target, and serves the S3
	// guard below. A row it does not see takes no lock: another writer's
	// first create of the identity that commits after this SELECT — and whose
	// block may be embedded before the upsert resumes — sends the upsert down
	// the conflict branch on a row this read never saw. Deciding the
	// invalidation from the read let that write keep the other writer's
	// vector next to its own content; the statement sees the row it updates.
	var hadProvenance bool
	var existing provenanceIdentity
	hadRow := true
	// The identity columns ride along on the row this SELECT already locks — no
	// extra query, and they are read as TEXT (see provenanceIdentity for why a
	// ::int cast on the stratum would be an availability bug, not a check).
	if err := tx.QueryRow(ctx,
		`SELECT metadata ? '`+derived.MetadataKey+`',
		        COALESCE(metadata->'`+derived.MetadataKey+`'->>'arm', ''),
		        COALESCE(metadata->'`+derived.MetadataKey+`'->>'stratum', '')
		   FROM context_blocks
		 WHERE category = $1 AND title = $2 AND scope = $3 AND NOT is_archived
		 FOR UPDATE`,
		category, title, scope,
	).Scan(&hadProvenance, &existing.arm, &existing.stratum); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: upsert block: pre-read: %w", err)
		}
		hadRow = false
	}

	if err := guardProvenanceClaim(ctx, tx, u.sens, u.metadata, hadRow, hadProvenance, existing, category, title, scope); err != nil {
		return nil, err
	}

	b := &Block{}
	var contentReplaced bool
	if err := tx.QueryRow(ctx, u.query, u.args...).Scan(
		&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope,
		&b.Sensitivity, &b.SensitivitySource, &b.TypeName, &b.LifecycleState, &b.TypeSource, &b.CreatedAt, &b.UpdatedAt,
		&contentReplaced,
	); err != nil {
		// An explicit type on the conflict row of a comment or an issue
		// (trg_block_retype, issue #280).
		if retypeBound(err) {
			return nil, ErrRetypeBound
		}
		return nil, fmt.Errorf("store: upsert block: %w", err)
	}

	// Only a REAL content change on an existing row invalidates the vector.
	// An idempotent upsert with identical content must NOT touch the
	// embedding — that would make the backfill re-embed on every no-op
	// `ctx save` at the 1M-10M target scale (organic growth means repeat
	// saves of unchanged titles are routine, not rare). The title is part of
	// the conflict target, so it never changes here.
	//
	// Reuses InvalidateEmbeddingTx — the same primitive UpdateBlock runs in
	// its transaction — rather than duplicating its SQL, so any future
	// extension of that primitive (it marks the serving vector stale, nulls
	// the _next pair and deletes the backfill memo) is inherited here
	// automatically.
	if contentReplaced {
		if err := InvalidateEmbeddingTx(ctx, tx, b.ID); err != nil {
			return nil, fmt.Errorf("store: upsert block: invalidate embedding: %w", err)
		}
	}
	return b, nil
}

// storeEmbeddingSet is the SET list of a backfill's vector write: the vector,
// the model that produced it, and the end of a stale window (issue #198,
// part B: the vector now stands for the block's current text).
const storeEmbeddingSet = `embedding = $2, embed_model = $3, embedding_stale = false`

// StoreEmbedding updates the embedding vector for a block, together with the
// model that produced it (Achse 04 W04-1: embed_model is the provenance
// trail the re-embed statemachine builds on — "NULL = no vector" only holds
// if every write is attributed), and clears the stale marker. It accepts any
// pgxdb.Execer, which BOTH *pgxpool.Pool and pgx.Tx satisfy.
//
// The write is unconditional, so the caller must hold the block's row lock
// from the read of the embedded text to this write: the scheduler backfill
// does (its FOR UPDATE SKIP LOCKED pick and this write share one
// transaction), so a title or content change waits for the commit and then
// marks this vector stale. A caller without the lock writes through
// StoreEmbeddingIfUnchanged.
//
// Fail-closed on model=="": a vector without provenance is worse than no
// vector at all (it would silently re-inherit the DDL default or a stale
// value) — rejected before the DB round-trip, no partial write.
func StoreEmbedding(ctx context.Context, q pgxdb.Execer, blockID, model string, vec []float32) error {
	if model == "" {
		return fmt.Errorf("store: store embedding: model is required (block %s)", blockID)
	}
	_, err := q.Exec(ctx,
		`UPDATE context_blocks SET `+storeEmbeddingSet+` WHERE id = $1`,
		blockID, pgvec.NewVector(vec), model,
	)
	if err != nil {
		return fmt.Errorf("store: store embedding: %w", err)
	}
	return nil
}

// StoreEmbeddingIfUnchanged is StoreEmbedding for a caller that read the
// embedded title and content without holding the row lock (the query path's
// inline backfill, which embeds over the wire inside the request): the
// vector lands only while the block still holds that title and content, and
// stored reports whether it did. A change that committed during the embed
// call leaves the block as the change left it — pending, unembedded or
// stale — so the backfill embeds the current text next (issue #198, part B).
// Without the condition the vector of the earlier text landed, cleared the
// marker, and the block left the queue holding a vector of text it no longer
// has.
//
// The condition is evaluated on the row version the UPDATE locks: a change
// that holds the row makes this write wait and then re-check the condition on
// the changed row (READ COMMITTED re-evaluation), so it cannot overwrite it.
// The comparison reads the block's content once, the cost of one row.
func StoreEmbeddingIfUnchanged(ctx context.Context, q pgxdb.Execer, blockID, title, content, model string, vec []float32) (stored bool, err error) {
	if model == "" {
		return false, fmt.Errorf("store: store embedding: model is required (block %s)", blockID)
	}
	tag, err := q.Exec(ctx,
		`UPDATE context_blocks SET `+storeEmbeddingSet+`
		 WHERE id = $1 AND title = $4 AND content = $5`,
		blockID, pgvec.NewVector(vec), model, title, content,
	)
	if err != nil {
		return false, fmt.Errorf("store: store embedding: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// StoreEmbeddingNext is StoreEmbedding's dual-column sister (Achse 04 W04-4,
// design/04 §4.3 Store row): the migration worker writes the NEW space into
// embedding_next/embed_model_next while the serving pair stays byte-identical
// untouched until the cutover swap (§4.5 option c — zero retrieval
// degradation during the whole backfill). Same pgxdb.Execer signature so the
// write composes into the worker's pick-holding tx (doctrine c80869c), same
// fail-closed model=="" rule: an unattributed _next vector would be invisible
// to the verify gate's embed_model_next check and could cutover silently.
func StoreEmbeddingNext(ctx context.Context, q pgxdb.Execer, blockID, model string, vec []float32) error {
	if model == "" {
		return fmt.Errorf("store: store embedding next: model is required (block %s)", blockID)
	}
	_, err := q.Exec(ctx,
		`UPDATE context_blocks SET embedding_next = $2, embed_model_next = $3 WHERE id = $1`,
		blockID, pgvec.NewVector(vec), model,
	)
	if err != nil {
		return fmt.Errorf("store: store embedding next: %w", err)
	}
	return nil
}

// InvalidateEmbeddingTx is what a real title or content change does to a
// block's vectors: it marks the serving vector stale (embedding_stale, only
// when there is one), sets the _next pair (embedding_next/embed_model_next,
// Achse 04 W04-3 / migration 114) to NULL, and deletes the block's
// regular-backfill memo row (context_embed_failures, migration_id IS NULL),
// so a content change converges cleanly on every axis that content
// invalidates:
//
//   - Serving pair: kept, and marked stale (issue #198, part B; migration
//     173). Until the backfill writes the vector of the new text
//     (StoreEmbedding clears the marker), the semantic arms keep ranking the
//     block by its previous text instead of losing it; the backfill pick
//     reads stale blocks next to unembedded ones (PendingPickSQL), and the
//     duplicate guard neither checks nor matches a stale vector. A block
//     without a vector stays unmarked: it is pending as unembedded. Before
//     part B this pair was nulled, which took the block out of the semantic
//     arms for the length of the queue.
//   - _next pair: design/04 §3.2b/§4.3 "Konvergenz-Invariante" — this IS the
//     content-change path a re-embed migration's dual-column cutover
//     depends on. Without nulling embedding_next here, a content update
//     during an active migration would leave a stale migrated vector next
//     to fresh content: the migration's pending predicate
//     (embedding_next IS NULL) would never re-pick the block, and Verify
//     (which checks model/norm, never content) would count it complete —
//     the new space would permanently serve a content-foreign vector for
//     that block.
//   - Backfill memo delete: W04-2/W04-3 coupling resolution (Lead-Entscheid,
//     wave briefing). The oversize infinity-memo's semantics are "parked
//     UNTIL content changes" (design §4.4); this IS the content-change
//     path. Without the delete, a block shrunk below the token limit
//     stayed parked forever — no code path ever revisited it.
//     Migration-scoped memos (migration_id NOT NULL) are untouched: those
//     belong to a specific migration's bookkeeping, not the regular
//     backfill, and are the migration worker's (W04-4+) concern.
//
// This is the pgxdb.Execer-parameterized primitive (same pattern as
// StoreEmbedding): it does NOT own a transaction, so a caller that needs the
// invalidation to be atomic with the write of the new text passes its own
// pgx.Tx, which already holds the block's row: UpsertBlock's content-change
// path (W04-8), UpdateBlock's (issue #198), the issue update and the two forge
// pull updates. The block row comes before the memo row, the order the
// backfill takes them in as well. Kept as ONE implementation so future
// extensions of the invalidation (e.g. a migration-scoped memo delete) land in
// every writer automatically, never just one.
//
// The serving pair is kept only while both stale-pick indexes of migration 173
// are ready (StaleIndexesReady, read once per call): the pick reads stale
// blocks on them alone, so on a corpus that has not built them yet (the
// out-of-band runbook of a large corpus) a kept vector would never be
// replaced. Without them the change nulls the pair as before 173, and the
// block is pending as unembedded.
//
// A block whose type is never embedded (ctx_block_type_never_embedded, the
// pick's rule, issue #213) that still holds a vector — embedded before its
// type became excluded, or before migration 176 for a tenant's own type —
// loses the serving pair instead and carries the embed_excluded marker: no
// reader ranks its vector, the backfill would never replace it, and as a
// stale block it would sit in the stale-pick indexes of migration 173 (which
// know no marker) for good, filtered row by row by every pick of its class.
// The statement flips the marker column, which makes trg_block_embed_excluded
// decide the block again under its locks: when the answer is true it sets the
// marker and drops the stale pair; when a lift of the type is in flight it
// waits for its commit and keeps the vector stale. No other reader needs such
// a vector: the duplicate guard neither checks nor matches a stale one, and
// the dream candidates, the cluster centroids and the /status channel probe
// read visible types only.
func InvalidateEmbeddingTx(ctx context.Context, q pgxdb.Execer, blockID string) error {
	if _, err := q.Exec(ctx,
		`WITH r AS (SELECT `+StaleIndexesReady+` AS keep)
		 UPDATE context_blocks
		 SET embedding_stale = (embedding IS NOT NULL AND r.keep),
		     embedding = CASE WHEN r.keep THEN embedding END,
		     embed_model = CASE WHEN r.keep THEN embed_model END,
		     embedding_next = NULL, embed_model_next = NULL,
		     embed_excluded = NOT embed_excluded
		 FROM r
		 WHERE id = $1`,
		blockID,
	); err != nil {
		return fmt.Errorf("store: invalidate embedding: %w", err)
	}
	if _, err := q.Exec(ctx,
		`DELETE FROM context_embed_failures WHERE block_id = $1 AND migration_id IS NULL`,
		blockID,
	); err != nil {
		return fmt.Errorf("store: invalidate embedding: delete backfill memo: %w", err)
	}
	return nil
}

// CheckRateLimit returns the number of write operations in the last 60 seconds
// for a given API key.
func CheckRateLimit(ctx context.Context, pool *pgxpool.Pool, apiKeyID string) (int, error) {
	return CheckRateLimitByAction(ctx, pool, apiKeyID, "write")
}

// CheckRateLimitByAction returns the number of operations with the given action
// in the last 60 seconds for a given API key.
func CheckRateLimitByAction(ctx context.Context, pool *pgxpool.Pool, apiKeyID, action string) (int, error) {
	var count int
	err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM context_access_log
		 WHERE api_key_id = $1::uuid
		   AND action = $2
		   AND created_at > now() - INTERVAL '60 seconds'`,
		apiKeyID, action,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: check rate limit (%s): %w", action, err)
	}
	return count, nil
}

// MinIDPrefixLen is the minimum length for prefix-based ID resolution.
// Set to 8 to prevent accidental wide matches; UUIDv7 timestamp prefixes
// give birthday-collision space well below 8 chars even at 10⁶ blocks.
const MinIDPrefixLen = 8

// ErrAmbiguousID is returned by ResolveBlockID when a prefix matches more than
// one block. The accompanying []BlockPreview contains the candidates.
var ErrAmbiguousID = errors.New("store: ambiguous block id prefix")

// IsFullUUID returns true if s has the canonical 36-char UUID shape (8-4-4-4-12
// with dashes at the expected positions). No charset validation — Postgres will
// reject malformed hex on actual lookup. Cheap pre-check to skip prefix-mode
// when the caller already has a full ID.
func IsFullUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	return s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}

// ResolveBlockID maps an ID prefix (≥MinIDPrefixLen chars) to exactly one block
// ID. Full UUIDs (36 chars) skip the prefix path. Returns:
//   - (id, single-element matches, nil) on unique match
//   - ("", nil, nil) on zero matches (caller treats as "not found")
//   - ("", matches, ErrAmbiguousID) on multiple matches — caller surfaces the
//     candidate list so the user can re-specify
//
// Scope-filtered identically to GetBlock; archived blocks excluded.
// grantedBlockIDs (T40a, design/07 §4) is the resolved block-grant set; nil ⇒
// '{}'::uuid[] ⇒ no-op OR-arm. The full-UUID bypass is UNCHANGED: a full id is
// returned verbatim and GetBlock re-gates it with the SAME scope/grant OR — the
// existence oracle is mitigated by caller re-gating (design/07 §5.5), not by a
// global ResolveBlockID change.
func ResolveBlockID(ctx context.Context, pool *pgxpool.Pool, idOrPrefix string, readScopes []string, grantedBlockIDs []string) (string, []BlockMeta, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return "", nil, err
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL
	}
	if IsFullUUID(idOrPrefix) {
		return idOrPrefix, nil, nil
	}
	if len(idOrPrefix) < MinIDPrefixLen {
		return "", nil, fmt.Errorf("store: id prefix must be at least %d characters (got %d)", MinIDPrefixLen, len(idOrPrefix))
	}

	// Cap candidates at 11: anything ≥10 is too ambiguous to surface usefully,
	// and the extra row tells the caller "≥10, narrow further" without paging.
	// $1=prefix, $2=readScopes, $3=maxCandidates(LIMIT), $4=grantedBlockIDs.
	const maxCandidates = 11
	rows, err := pool.Query(ctx,
		`SELECT id, title, category, tags, scope, updated_at
		 FROM context_blocks
		 WHERE id::text LIKE $1 || '%' AND NOT is_archived AND (scope = ANY($2::text[]) OR id = ANY($4::uuid[]))
		 ORDER BY updated_at DESC
		 LIMIT $3`,
		idOrPrefix, readScopes, maxCandidates, grantedBlockIDs,
	)
	if err != nil {
		return "", nil, fmt.Errorf("store: resolve id prefix: %w", err)
	}
	defer rows.Close()

	matches := make([]BlockMeta, 0, 2)
	for rows.Next() {
		var m BlockMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.Category, &m.Tags, &m.Scope, &m.UpdatedAt); err != nil {
			return "", nil, fmt.Errorf("store: resolve id prefix scan: %w", err)
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("store: resolve id prefix rows: %w", err)
	}

	switch len(matches) {
	case 0:
		return "", nil, nil
	case 1:
		return matches[0].ID, matches, nil
	default:
		return "", matches, ErrAmbiguousID
	}
}

// guardRedactedMetadataSQL is the `metadata` select expression of the raw read
// paths (GetBlock, non-compact SearchBlocks, the UpdateBlock RETURNING and the
// issue projection issueScanColsRedacted): the guard's guard_matched_id is
// dropped when the matched block does not lie in the caller's scopes —
// the rule GuardList applies to matched_id/matched_title, now on every path
// that returns metadata verbatim (GHSA 2026-10-31 §2: the key named a block
// the caller could not read, a UUIDv7 with its creation time). scopesParam is
// a text[] expression over the caller's read scopes, or a subset of them
// (the write paths pass their write-scope bind) — code-owned at every call
// site, never user input. The CASE guards the uuid cast so a malformed
// value in metadata degrades to "not readable" instead of a 22P02. Blocks
// whose metadata carries no guard_matched_id pass through untouched, and the
// matched-block lookup is a primary-key probe (one row per returned block).
func guardRedactedMetadataSQL(scopesParam string) string {
	return `CASE WHEN context_blocks.metadata ? 'guard_matched_id' AND NOT EXISTS (
			SELECT 1 FROM context_blocks mb
			WHERE mb.id = CASE WHEN pg_input_is_valid(context_blocks.metadata->>'guard_matched_id', 'uuid')
			                   THEN (context_blocks.metadata->>'guard_matched_id')::uuid END
			  AND mb.scope = ANY(` + scopesParam + `::text[]))
		THEN context_blocks.metadata - 'guard_matched_id' ELSE context_blocks.metadata END AS metadata`
}

// GetBlock retrieves a single block by ID, filtered by allowed scopes plus the
// resolved block-grant set (T40a, design/07 §4). grantedBlockIDs is the set of
// block IDs row-level-granted to the caller's tenant; nil ⇒ '{}'::uuid[] ⇒ the
// additive OR-arm is a deterministic no-op (byte-identical to scope-only). The
// mandatory parentheses keep the NOT is_archived guard OUTSIDE the scope/grant
// OR — a granted archived block must NOT surface.
// set is the caller's block-type registry snapshot; it fills Block.Untrusted
// (V-11). nil ⇒ the field stays absent from the response (write/confirm paths
// pass nil — see untrustedOf).
func GetBlock(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, id string, readScopes []string, grantedBlockIDs []string) (*Block, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL
	}
	return readBlockByID(ctx, pool, set, "store: get block",
		`WHERE id = $1 AND NOT is_archived AND (scope = ANY($2::text[]) OR id = ANY($3::uuid[]))`,
		id, readScopes, grantedBlockIDs)
}

// GetArchivedBlock reads one ARCHIVED block by its full id (issue #201: the
// opt-in `include_archived` of MCP get and manage get — a citing block names
// the id, and a reader must be able to check what the cited block said).
//
// Deliberately narrower than GetBlock, so that "visible" keeps its one meaning
// everywhere else: the scopes the caller passes only — the handler passes the
// key's OWN scopes, without cross-tenant scope grants — NO block-grant arm (a
// grant shares a live block; it never kept an archived one readable, see the
// GetBlock comment), and only rows that ARE archived — a live id answers nil
// here, the caller reads it through GetBlock. The answer carries
// Archived = true. A miss is (nil, nil) for an unknown id, a foreign scope and
// a live block alike.
func GetArchivedBlock(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, id string, readScopes []string) (*Block, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	b, err := readBlockByID(ctx, pool, set, "store: get archived block",
		`WHERE id = $1 AND is_archived AND scope = ANY($2::text[])`,
		id, readScopes)
	if b != nil {
		b.Archived = true
	}
	return b, err
}

// readBlockByID is the row read GetBlock and GetArchivedBlock share: one
// column list and one scan, so the two answer the same block shape and differ
// only in their WHERE. where binds the id as $1 and the caller's read scopes as
// $2 — guardRedactedMetadataSQL reads the scopes from $2. Code-owned SQL at both
// call sites, never user input.
func readBlockByID(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, op, where string, args ...any) (*Block, error) {
	b := &Block{}
	err := pool.QueryRow(ctx,
		`SELECT id, category, tags, title, content, `+guardRedactedMetadataSQL("$2")+`, scope, sensitivity, sensitivity_source, type_name, lifecycle_state, type_source, created_at, updated_at,
		        `+untrustedOriginCol+`
		 FROM context_blocks
		 `+where+`
		 LIMIT 1`,
		args...,
	).Scan(&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope,
		&b.Sensitivity, &b.SensitivitySource, &b.TypeName, &b.LifecycleState, &b.TypeSource, &b.CreatedAt, &b.UpdatedAt,
		&b.UntrustedOrigin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	b.Untrusted = untrustedOf(set, b.TypeName) // V-11
	return b, nil
}

// DeleteBlock archives a block (sets is_archived = true). The block must live in
// one of the caller's write-eligible scopes (home_scope ∪ shared-if-allowed).
//
// I7 / S3, third door (W01-2a Nachbesserung, review finding #5): a derivative is
// not client-ARCHIVABLE either. Archiving it is damage in its own right — the
// block falls out of every read path (bruchpfad B1's damage class) — and it is
// also step one of a two-call bypass, because the partial unique index frees the
// (category, title, scope) identity for a fresh upsert. The exclusion rides in
// the WHERE, atomic with the archive; the 0-row case is diagnosed afterwards to
// tell "protected" apart from "not found".
//
// An archived block supersedes nothing (the reconcile reads only live sources),
// so the targets of its supersedes links are reconciled in the same
// transaction: a target no other live block supersedes returns to knowledge at
// once instead of staying a snapshot that points at an archived block until
// the daily link sweep (issue #197).
func DeleteBlock(ctx context.Context, pool *pgxpool.Pool, id string, writeScopes []string) (*Block, error) {
	b := &Block{}
	err := writeSupersedesSource(ctx, pool, pgxdb.At("store: delete block"), func(tx pgx.Tx) error {
		targets, err := lockSupersedesSource(ctx, tx, id, writeScopes)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx,
			`UPDATE context_blocks SET is_archived = true, updated_at = now()
			 WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived
			   AND `+notDerivedSQL+`
			 RETURNING id, category, title, scope, created_at, updated_at`,
			id, writeScopes,
		).Scan(&b.ID, &b.Category, &b.Title, &b.Scope, &b.CreatedAt, &b.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return pgxdb.ErrRollback
		}
		if err != nil {
			return fmt.Errorf("store: delete block: %w", err)
		}
		if _, err := ReconcileSupersedesTargets(ctx, tx, targets, ""); err != nil {
			return fmt.Errorf("store: delete block: %w", err)
		}
		return nil
	})
	if errors.Is(err, pgxdb.ErrRollback) {
		return nil, updateBlockMissReason(ctx, pool, id, writeScopes)
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// UpdateBlockData holds the fields that can be updated on a block.
type UpdateBlockData struct {
	Category *string         `json:"category,omitempty"`
	Title    *string         `json:"title,omitempty"`
	Content  *string         `json:"content,omitempty"`
	Tags     []string        `json:"tags,omitempty"`
	Metadata map[string]any  `json:"metadata,omitempty"`
	Scope    *string         `json:"scope,omitempty"`
	// Sensitivity reclassifies the block (sensitivity_source becomes
	// 'manual'). A DOWNGRADE below the current level requires
	// ConfirmSensitivityDowngrade — same friction as a backend trust
	// elevation, it opens the same flow (F3 §3.5). The handler enforces the
	// confirm; the store applies.
	Sensitivity                 *string `json:"sensitivity,omitempty"`
	ConfirmSensitivityDowngrade bool    `json:"confirm_sensitivity_downgrade,omitempty"`
	// Type re-types the block (WF T10): type_source becomes 'manual', which
	// permanently overrides the auto-classifier (T4 semantics — the classify
	// hook only touches type_source='auto' rows). The handler validates the
	// name against the registry snapshot (422 on unknown); the store applies.
	Type *string `json:"type,omitempty"`
	// SensitivityAudit is set by the handler on a confirmed downgrade
	// (who/when/from→to) and merged into metadata.sensitivity_audit. Never
	// client-supplied (json:"-").
	SensitivityAudit map[string]any `json:"-"`
}

// blockIdentityIndex is the partial unique index that makes (category, title,
// scope) the identity of an ACTIVE block (113_baseline.sql, the fold of
// 005_scope_unique.sql: WHERE NOT is_archived).
const blockIdentityIndex = "uq_context_category_title_scope"

// BlockIdentityTakenError is the refusal of a by-id update that would give a
// block the (category, title, scope) of another active block (issue #227). It
// is a typed error and not a message for the reason ErrProvenanceProtected is
// a sentinel: the write surfaces answer it as a 409 the caller resolves by
// choosing another category or title, not as the generic 500 of an unexpected
// store error.
//
// ConflictID names the active block that holds the identity, "" when the
// follow-up read found none (the holder was renamed or archived between the
// refused statement and the read), found a block of another identity (the
// source row was changed in between), or failed. That read runs within the
// caller's write scopes (blockIdentityHolder), so the id never names a block
// outside them.
type BlockIdentityTakenError struct {
	ConflictID string
	// refusedKey is the DETAIL of the 23505, PostgreSQL's own statement of the
	// key it refused ("Key (category, title, scope)=(…) already exists.").
	refusedKey string
}

func (e *BlockIdentityTakenError) Error() string {
	if e.ConflictID == "" {
		return "store: update block: another active block in this scope already has this category and title"
	}
	return "store: update block: block " + e.ConflictID + " already has this category and title in this scope"
}

// blockIdentityViolation reports the 23505 of blockIdentityIndex and of no
// other constraint, with the error's DETAIL (the refused key): a unique
// violation elsewhere is not a collision this layer can explain, and it stays
// the caller's server fault.
func blockIdentityViolation(err error) (refusedKey string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == blockIdentityIndex {
		return pgErr.Detail, true
	}
	return "", false
}

// blockIdentityHolder reads the id of the active block that holds the identity
// a refused update of id asked for: the value the update sent for each of
// category, title and scope, the row's own value for each it did not send. It
// runs on the refusal path only, after the update's transaction rolled back.
// The row's own values come from scalar subqueries on the primary key, which
// run at most once each and only for a field the update did not send, so the
// holder scan gets all three values as parameters and is an index equality
// probe whatever the planner's statistics say. (The equivalent self-join was
// planned with the title as a join filter over the category's active blocks
// when the planner chose not to parameterize the inner scan.) The holder is read within
// writeScopes, so the answer stays inside what the caller may read
// (writableBlockScopes honours a write scope only where the key also reads it).
//
// The row's own values are read after the rollback, so a writer that changed
// the row in between (a move from category alpha to beta while the refused
// update sent only a title) would make the read probe an identity the update
// never asked for. The found block is therefore named only when its identity
// is the key PostgreSQL refused, as the 23505 stated it in refusedKey; any
// other answer, an empty refusedKey, or a DETAIL in another wording leaves the
// id out. Capturing the identity inside the transaction instead would cost
// every update a read before its statement.
//
// Like updateBlockMissReason it decides the message, never the outcome: a miss,
// a mismatch or a read fault answers "".
func blockIdentityHolder(ctx context.Context, pool *pgxpool.Pool, id string, data UpdateBlockData, writeScopes []string, refusedKey string) string {
	if refusedKey == "" {
		return ""
	}
	var holder, category, title, scope string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, category, title, scope FROM context_blocks
		  WHERE category = COALESCE($2::text, (SELECT category FROM context_blocks WHERE id = $1))
		    AND title    = COALESCE($3::text, (SELECT title    FROM context_blocks WHERE id = $1))
		    AND scope    = COALESCE($4::text, (SELECT scope    FROM context_blocks WHERE id = $1))
		    AND NOT is_archived
		    AND id <> $1
		    AND scope = ANY($5::text[])
		  LIMIT 1`,
		id, data.Category, data.Title, data.Scope, writeScopes,
	).Scan(&holder, &category, &title, &scope); err != nil {
		return ""
	}
	if !strings.Contains(refusedKey, "=("+category+", "+title+", "+scope+")") {
		return ""
	}
	return holder
}

// UpdateBlock updates specific fields of a block. The block must live in one of
// the caller's write-eligible scopes (home_scope ∪ shared-if-allowed).
// Returns the updated block and whether the title or the content now differs
// from the stored row. When it does, the block's vector has been invalidated
// in the same transaction (InvalidateEmbeddingTx: the serving vector stays
// searchable and is marked stale until the backfill writes the new one). A
// title or content sent with the value the row already holds does not count:
// the embed input is unchanged, and invalidating the vector would put the
// block back into the backfill for nothing (issue #198; UpsertBlock reads the
// same difference off its own RETURNING, issue #281).
//
// An update that would give the block the (category, title, scope) of another
// active block — a title rename, a category change or a scope move onto an
// identity in use — writes nothing and returns *BlockIdentityTakenError
// (issue #227). A re-type of an issue or a comment writes nothing and returns
// ErrRetypeBound; a re-type out of a type
// marked untrusted records that type in untrusted_origin, which the answer
// carries (both trg_block_retype, issue #280).
func UpdateBlock(ctx context.Context, pool *pgxpool.Pool, id string, data UpdateBlockData, writeScopes []string) (*Block, bool, error) {
	// Build SET clause dynamically.
	setClauses := []string{}
	args := []any{}
	argIdx := 1

	if data.Category != nil {
		setClauses = append(setClauses, fmt.Sprintf("category = $%d", argIdx))
		args = append(args, *data.Category)
		argIdx++
	}
	if data.Title != nil {
		setClauses = append(setClauses, fmt.Sprintf("title = $%d", argIdx))
		args = append(args, *data.Title)
		argIdx++
	}
	// contentSent is presence, not difference: it keys the guard_checked_at
	// strip below, which this statement has to decide before it has seen the
	// old row. Whether the embed input changed is read off the row instead
	// (see the RETURNING clause).
	contentSent := data.Content != nil
	if contentSent {
		setClauses = append(setClauses, fmt.Sprintf("content = $%d", argIdx))
		args = append(args, *data.Content)
		argIdx++
	}
	if data.Tags != nil {
		setClauses = append(setClauses, fmt.Sprintf("tags = $%d", argIdx))
		args = append(args, data.Tags)
		argIdx++
	}
	// metadata expression: exactly ONE assignment per column is legal in SQL,
	// so the guard_checked_at strip and the sensitivity_audit merge compose
	// into a single expression.
	metaExpr := ""
	if data.Metadata != nil {
		if contentSent {
			// Merge provided metadata AND strip guard_checked_at in one expression.
			metaExpr = fmt.Sprintf("$%d::jsonb - 'guard_checked_at'", argIdx)
		} else {
			metaExpr = fmt.Sprintf("$%d::jsonb", argIdx)
		}
		args = append(args, data.Metadata)
		argIdx++
	} else if contentSent {
		// No explicit metadata update — strip guard_checked_at from existing metadata.
		metaExpr = "metadata - 'guard_checked_at'"
	}
	if data.SensitivityAudit != nil {
		if metaExpr == "" {
			metaExpr = "COALESCE(metadata, '{}'::jsonb)"
		}
		metaExpr = fmt.Sprintf("(%s) || jsonb_build_object('sensitivity_audit', $%d::jsonb)", metaExpr, argIdx)
		args = append(args, data.SensitivityAudit)
		argIdx++
	}
	if metaExpr != "" {
		setClauses = append(setClauses, "metadata = "+metaExpr)
	}
	if data.Scope != nil {
		setClauses = append(setClauses, fmt.Sprintf("scope = $%d", argIdx))
		args = append(args, *data.Scope)
		argIdx++
	}
	if data.Sensitivity != nil {
		// Handler has validated the level and enforced the downgrade confirm.
		setClauses = append(setClauses, fmt.Sprintf("sensitivity = $%d", argIdx))
		args = append(args, *data.Sensitivity)
		argIdx++
		setClauses = append(setClauses, "sensitivity_source = 'manual'")
	}
	if data.Type != nil {
		// Handler has validated the name against the registry (WF T10).
		setClauses = append(setClauses, fmt.Sprintf("type_name = $%d", argIdx))
		args = append(args, *data.Type)
		argIdx++
		setClauses = append(setClauses, "type_source = 'manual'")
	}

	if len(setClauses) == 0 {
		return nil, false, fmt.Errorf("store: update block: no fields to update")
	}

	setClauses = append(setClauses, "updated_at = now()")

	// Add WHERE params
	args = append(args, id)
	idIdx := argIdx
	argIdx++
	args = append(args, writeScopes)
	scopeIdx := argIdx

	// I7 / S3, second door (design D-01 §4.3.1): update-by-id reaches the same
	// derivative WITHOUT naming its (category, title, scope) identity, so the
	// upsert-side guard alone would leave the invariant to the choice of verb.
	// The exclusion rides in the WHERE — atomic with the update itself, no
	// separate read to race against — and the 0-row case is then diagnosed
	// below to tell "protected" apart from "not found".
	//
	// UpdateBlock has exactly three callers, all of them client surfaces
	// (handler/context_manage.go, handler/mcp_update.go, handler/confirm_core.go);
	// no arm or scheduler writes through it. The guard is therefore
	// client-only by construction rather than by a flag.
	//
	// The RETURNING metadata goes through the guard redaction GetBlock applies
	// (guardRedactedMetadataSQL): the REST update answers with the whole block,
	// so without it a guard_matched_id naming a block outside the caller's view
	// came back verbatim on any field update (§F-§2). The redaction reads the
	// write-scope bind this statement already carries, not the caller's read
	// scopes. writeScopes is a subset of the read scopes (writableBlockScopes
	// honours a write scope only where the key also reads it), so a match the
	// caller can read but not write is dropped here too — the fail-closed
	// direction; `get` still returns it. Binding the read scopes instead would
	// widen the signature of all three callers for parity on one key of a
	// write response. Plain updates and scope moves run this one query string.
	//
	// The last RETURNING column compares the row before and after the update
	// (old/new in RETURNING, PostgreSQL 18): true only when the title or the
	// content was actually replaced by a different value. No extra read to
	// race against, and the content is not sent back twice.
	query := fmt.Sprintf(
		`UPDATE context_blocks SET %s
		 WHERE id = $%d AND scope = ANY($%d::text[]) AND NOT is_archived
		   AND `+notDerivedSQL+`
		 RETURNING id, category, tags, title, content, %s, scope, sensitivity, sensitivity_source, type_name, lifecycle_state, type_source, created_at, updated_at,
		           `+untrustedOriginCol+`,
		           (old.title IS DISTINCT FROM new.title OR old.content IS DISTINCT FROM new.content)`,
		strings.Join(setClauses, ", "), idIdx, scopeIdx, guardRedactedMetadataSQL(fmt.Sprintf("$%d", scopeIdx)),
	)

	// The statement, the invalidation of the vector and (on a scope move) the
	// link sweep commit together. The invalidation in particular must not be
	// a second step after the commit: needsReEmbed is a difference, so an
	// invalidation that got lost between the two (request cancelled, pool
	// fault) would not be repaired by sending the same update again, and the
	// block would keep a vector for content it no longer holds, unmarked and
	// invisible to the backfill.
	b := &Block{}
	var moveScopes []string
	if data.Scope != nil {
		moveScopes = writeScopes
	}
	found, needsReEmbed, err := runBlockUpdate(ctx, pool, b, id, query, args, moveScopes)
	var taken *BlockIdentityTakenError
	if errors.As(err, &taken) {
		taken.ConflictID = blockIdentityHolder(ctx, pool, id, data, writeScopes, taken.refusedKey)
		return nil, false, taken
	}
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, updateBlockMissReason(ctx, pool, id, writeScopes)
	}
	return b, needsReEmbed, nil
}

// updateBlockMissReason turns the 0-row outcome of a by-id block write into the
// reason for it — shared by UpdateBlock and DeleteBlock, whose WHERE clauses
// carry the same three conditions a caller can miss: the id, the write-scope
// filter and, since W01-2a, the S3 provenance exclusion. Only the last one is a
// REFUSAL rather than a miss: it must reach the caller as 403 and not as "Block
// not found", which would be an oracle-free but also information-free answer
// about a block the key can see.
//
// It runs only on the 0-row path, so the ordinary update pays no extra read. Returning nil (= "not found", the pre-wave contract) is the
// fail-open direction here on purpose: this function decides the MESSAGE, never
// the outcome — the write itself was already refused by the WHERE.
func updateBlockMissReason(ctx context.Context, pool *pgxpool.Pool, id string, writeScopes []string) error {
	var protected bool
	if err := pool.QueryRow(ctx,
		`SELECT metadata ? '`+derived.MetadataKey+`' FROM context_blocks
		 WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived`,
		id, writeScopes,
	).Scan(&protected); err != nil {
		// No row (the ordinary miss) or a read fault: both leave the pre-wave
		// answer in place. Not an error return — the outcome was already
		// decided by the UPDATE that wrote nothing.
		protected = false
	}
	if protected {
		return ErrProvenanceProtected
	}
	return nil
}

// runBlockUpdate runs a by-id block update as ONE transaction: the
// UPDATE…RETURNING scan into b, on a scope move the GD5 link sweep (the link
// tables' row-scope invariant, both endpoints + row in the same scope, would
// otherwise break post-hoc; GD5/K8), and the vector invalidation when the
// statement reports that the title or the content changed
// (InvalidateEmbeddingTx, the primitive UpsertBlock uses on its
// content-change path). Returns
// found=false (no error) when the block was not found in the caller's write
// scopes.
//
// moveScopes is nil unless the update names a scope (a possible scope move);
// then it carries the caller's write scopes, and the block and the targets of
// its supersedes links are locked before the update (lockSupersedesSource), so
// the supersedes state the sweep ends can be reconciled in this transaction.
func runBlockUpdate(ctx context.Context, pool *pgxpool.Pool, b *Block, id, query string, args []any, moveScopes []string) (found, needsReEmbed bool, err error) {
	var swept int
	scopeMove := moveScopes != nil
	write := pgxdb.Write
	if scopeMove {
		write = writeSupersedesSource // lockSupersedesSource may ask for a re-run
	}
	if err := write(ctx, pool, pgxdb.At("store: update block"), func(tx pgx.Tx) error {
		var supersedesTargets []string
		if scopeMove {
			var err error
			if supersedesTargets, err = lockSupersedesSource(ctx, tx, id, moveScopes); err != nil {
				return err
			}
		}
		err := tx.QueryRow(ctx, query, args...).Scan(
			&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope,
			&b.Sensitivity, &b.SensitivitySource, &b.TypeName, &b.LifecycleState, &b.TypeSource, &b.CreatedAt, &b.UpdatedAt,
			&b.UntrustedOrigin, &needsReEmbed,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			// A miss writes nothing: pgxdb.ErrRollback keeps it a rollback
			// instead of a commit.
			return pgxdb.ErrRollback
		}
		if refusedKey, ok := blockIdentityViolation(err); ok {
			return &BlockIdentityTakenError{refusedKey: refusedKey}
		}
		if retypeBound(err) {
			return ErrRetypeBound
		}
		if err != nil {
			return fmt.Errorf("store: update block: %w", err)
		}

		if scopeMove {
			var ended []string
			if swept, ended, err = sweepScopeMoveLinks(ctx, tx, id, b.Scope); err != nil {
				return err
			}
			// Only targets locked above (plus the moved block itself, which the
			// update locked) are reconciled: a supersedes link that ended here
			// either left one of the block's own targets behind, or pointed at
			// the block from a source that stayed behind.
			var reconcile []string
			for _, t := range ended {
				if t == b.ID || slices.Contains(supersedesTargets, t) {
					reconcile = append(reconcile, t)
				}
			}
			restored, err := ReconcileSupersedesTargets(ctx, tx, reconcile, "")
			if err != nil {
				return fmt.Errorf("store: update block: %w", err)
			}
			// The moved block may itself be a target the move took away from
			// its last superseder: the reconcile changed its lifecycle after
			// the update returned it, so the answer reads it again.
			if slices.Contains(restored, b.ID) {
				if err := tx.QueryRow(ctx,
					`SELECT lifecycle_state FROM context_blocks WHERE id = $1::uuid`, b.ID).Scan(&b.LifecycleState); err != nil {
					return fmt.Errorf("store: update block: re-read lifecycle: %w", err)
				}
			}
		}
		if needsReEmbed {
			if err := InvalidateEmbeddingTx(ctx, tx, b.ID); err != nil {
				return fmt.Errorf("store: update block: invalidate embedding: %w", err)
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, pgxdb.ErrRollback) {
			return false, false, nil
		}
		return false, false, err
	}
	if swept > 0 {
		slog.Info("store: scope move swept divergent links", "block_id", id, "new_scope", b.Scope, "links_deleted", swept)
	}
	return true, needsReEmbed, nil
}

// sweepScopeMoveLinks restores the link tables' row-scope invariant after a
// block scope move (GD5/K8; design at build time — sweep over update-gate,
// which would block practically every move): per link table, links whose far
// endpoint already lives in the target scope FOLLOW (row.scope update — also
// heals raw-injected legacy rows, the K8 audit case); links whose far
// endpoint stays behind are DELETED. Dream links regenerate through the dream
// cycle; a structural link with scope-divergent endpoints could never be
// re-written under the write invariant, so keeping the row would be dead,
// wrongly-scoped ballast the read paths must filter forever. Returns the
// number of deleted rows and, for every deleted dream supersedes link, the
// block whose supersedes state it ended (its target) — pinned links included,
// because supersedes holds only within one scope.
func sweepScopeMoveLinks(ctx context.Context, tx pgx.Tx, id, newScope string) (int, []string, error) {
	swept := 0
	var ended []string
	for _, table := range []string{"context_dream_links", "context_structural_links"} {
		if _, err := tx.Exec(ctx, `
			UPDATE `+table+` l SET scope = $2
			WHERE (l.source_block_id = $1 OR l.target_block_id = $1)
			  AND (SELECT cb.scope FROM context_blocks cb
			       WHERE cb.id = CASE WHEN l.source_block_id = $1 THEN l.target_block_id ELSE l.source_block_id END) = $2`,
			id, newScope); err != nil {
			return 0, nil, fmt.Errorf("store: update block: move %s: %w", table, err)
		}
		// The relationship column exists on both tables under different names;
		// only the dream table carries supersedes, so the structural sweep
		// reports a constant.
		rel := "l.relationship"
		if table == "context_structural_links" {
			rel = "''"
		}
		rows, err := tx.Query(ctx, `
			DELETE FROM `+table+` l
			WHERE (l.source_block_id = $1 OR l.target_block_id = $1)
			  AND (SELECT cb.scope FROM context_blocks cb
			       WHERE cb.id = CASE WHEN l.source_block_id = $1 THEN l.target_block_id ELSE l.source_block_id END) IS DISTINCT FROM $2
			RETURNING l.target_block_id::text, `+rel, id, newScope)
		if err != nil {
			return 0, nil, fmt.Errorf("store: update block: sweep %s: %w", table, err)
		}
		for rows.Next() {
			var target, relationship string
			if err := rows.Scan(&target, &relationship); err != nil {
				rows.Close()
				return 0, nil, fmt.Errorf("store: update block: sweep %s: %w", table, err)
			}
			swept++
			if relationship == "supersedes" {
				ended = append(ended, target)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, nil, fmt.Errorf("store: update block: sweep %s: %w", table, err)
		}
	}
	return swept, ended, nil
}

// SearchCursor is the keyset-pagination position for the empty-query
// (updated_at DESC) browse path (block-workbench W7). It captures the LAST row
// of the previous page so the next page resumes strictly after it.
//
// The browse ORDER BY is `updated_at DESC, id DESC`; updated_at is NOT unique
// (ties are possible), so id is the mandatory tiebreak. The keyset WHERE is the
// row-value comparison `(updated_at, id) < ($afterUpdated, $afterId)` — strictly
// "older than, or same timestamp with a smaller id". A nil *SearchCursor means
// page 1 (no cursor, unchanged behavior).
//
// The cursor is INERT on the FTS (ranked) path: ts_rank_cd is a float
// expression with ties that would have to be recomputed in the WHERE for a
// keyset, which is fragile and slow — that path stays LIMIT-only ("top matches;
// refine the query for more"). SearchBlocks ignores a non-nil cursor when the
// query is non-empty.
//
// The two fields are omitzero/omitempty for the MCP `search` tool (#206): the
// SDK derives the tool's input schema from this type, and a field without the
// option would be REQUIRED there, so the documented page-1 spelling `{}` (the
// zero position, which every caller treats as "no cursor") could not be sent.
// A cursor the server hands out is never zero, so its JSON is unchanged.
type SearchCursor struct {
	// UpdatedAt is the updated_at of the last row returned on the previous page.
	UpdatedAt time.Time `json:"after_updated,omitzero"`
	// ID is that row's id — the tiebreak that makes (updated_at, id) unique.
	ID string `json:"after_id,omitempty"`
}

// SearchMaxLimit is the most rows one search statement answers, for every
// caller (REST /api/search, the MCP `search` tool, the chat ctx_search tool).
// Every answer is bounded; a caller that needs more pages with the keyset
// cursor (SearchBlocksPage). Exported so a surface can tell its caller that a
// requested limit was lowered without restating the number.
const SearchMaxLimit = 50

// SearchDefaultLimit is the page size of a search without a positive limit.
const SearchDefaultLimit = 10

// SearchBlocks performs a lightweight search with optional FTS, category, and tag filters.
// Uses precomputed ts_de tsvector for FTS ranking.
//
// after is the keyset-pagination cursor (block-workbench W7): nil = page 1
// (unchanged). On the empty-query browse path it resumes strictly after the
// cursor's (updated_at, id); on the FTS path it is ignored (that path is
// LIMIT-only). Callers that do not paginate pass nil.
//
// types / typesExclude (WF T10, design/01 §7-T10 R1): OPT-IN server-side
// type filters — `type_name = ANY($n)` / `NOT type_name = ANY($n)` as bind
// parameters, empty/nil = no filter. Deliberately NOT a hard exclude: the
// browse surfaces keep the D5 asymmetry (excluded-policy types stay
// browseable; only retrieval ranks them out). A client-side filter over
// paginated lists would be functionally wrong at the 1M+/10k-issues target
// scale (knowledge browsing would page through issue pages).
//
// cluster (C6, design/03 §4.8) is the optional `cluster:<handle>` facet: nil or
// empty = no restriction. The caller is responsible for the FORM check (a
// malformed handle is a 400 before this call, never a 22P02 from the cast) and
// for the cluster.facet_enabled gate — this layer only binds the resolution.
//
// set is the caller's block-type registry snapshot; it fills
// BlockPreview.Untrusted per row (V-11). nil ⇒ the field stays absent.
func SearchBlocks(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, query string, readScopes []string, category string, tags []string, limit int, compact bool, after *SearchCursor, grantedBlockIDs []string, types []string, typesExclude []string, cluster *string) ([]BlockPreview, error) {
	rows, _, err := searchBlocks(ctx, pool, set, searchParams{
		Query: query, ReadScopes: readScopes, Category: category, Tags: tags,
		Limit: limit, Compact: compact, After: after, GrantedBlockIDs: grantedBlockIDs,
		Types: types, TypesExclude: typesExclude, Cluster: cluster,
	})
	return rows, err
}

// SearchBlocksPage is SearchBlocks for a caller that hands out a next-page
// cursor (#206): same arguments, same rows, plus whether the browse listing
// goes on after the last row. It reads ONE row more than the page on the
// empty-query path and drops it, so `more` is an observed fact — the rule it
// replaced ("a full page may have more") answered a cursor to an empty page
// whenever the last page happened to be full. The full-text path is "top
// matches" and never pages; there `more` is always false and no extra row is
// read.
//
// tagsAll (#219) is the containment filter beside the overlap of tags: a row
// carries every one of them. nil/empty = no filter. SearchBlocks has no such
// argument; its one caller (the chat ctx_search tool) offers no such field.
func SearchBlocksPage(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, query string, readScopes []string, category string, tags, tagsAll []string, limit int, compact bool, after *SearchCursor, grantedBlockIDs []string, types []string, typesExclude []string, cluster *string) (rows []BlockPreview, more bool, err error) {
	return searchBlocks(ctx, pool, set, searchParams{
		Query: query, ReadScopes: readScopes, Category: category, Tags: tags, TagsAll: tagsAll,
		Limit: limit, Compact: compact, After: after, GrantedBlockIDs: grantedBlockIDs,
		Types: types, TypesExclude: typesExclude, Cluster: cluster, ProbeNext: true,
	})
}

// searchBlocks runs one search statement; see SearchBlocksPage for `more`.
func searchBlocks(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, p searchParams) ([]BlockPreview, bool, error) {
	if err := RequireScopes(p.ReadScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, false, err
	}
	sql, args := searchBlocksSQL(p)
	rows, err := runSearchBlocks(ctx, pool, set, sql, args, p.Compact)
	if err != nil {
		return nil, false, err
	}
	if page := ClampLimit(p.Limit, SearchDefaultLimit, SearchMaxLimit); len(rows) > page {
		return rows[:page], true, nil
	}
	return rows, false, nil
}

// searchParams is the input of the statement builder. It exists so the §6.6
// plan gate can EXPLAIN the very statement production runs instead of a
// hand-copied approximation of it — a copy would drift and the gate would then
// measure the copy.
type searchParams struct {
	Query           string
	ReadScopes      []string
	Category        string
	Tags            []string
	TagsAll         []string // #219: rows that carry every one of these tags; empty = no filter
	Limit           int
	Compact         bool
	After           *SearchCursor
	GrantedBlockIDs []string
	Types           []string
	TypesExclude    []string
	Cluster         *string
	// ProbeNext asks the browse path for one row past the page
	// (SearchBlocksPage). Inert on the full-text path.
	ProbeNext bool
}

// searchBlocksSQL composes the browse/FTS statement and its arguments. Pure —
// no database, no context — which is what makes it explainable and testable.
//
//nolint:cyclop // one filter = one branch; the dynamic argIdx bookkeeping is the point and splitting it would hide the $-index discipline
func searchBlocksSQL(p searchParams) (string, []any) {
	query, readScopes, category, tags := p.Query, p.ReadScopes, p.Category, p.Tags
	after, types, typesExclude := p.After, p.Types, p.TypesExclude
	compact := p.Compact
	grantedBlockIDs := p.GrantedBlockIDs
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL
	}
	limit := ClampLimit(p.Limit, SearchDefaultLimit, SearchMaxLimit)
	if p.ProbeNext && p.Query == "" {
		limit++ // the probe row; searchBlocks drops it again
	}

	// $1 stays readScopes and $2 stays the FTS query arg (the FTS orderBy
	// hardcodes $2 — never shift it). The scope/grant OR-clause is NOT the first
	// fixed element anymore: it is appended AFTER the dynamic args with its own
	// allocated index so the grant uuid[] never collides with $2. (AND is
	// commutative — is_archived up front, the scope-OR-clause at the back is
	// semantically identical and keeps the mandatory parentheses, design/07 §4.)
	whereClauses := []string{"NOT is_archived"}
	args := []any{readScopes}
	argIdx := 2

	hasQuery := query != ""
	if hasQuery {
		whereClauses = append(whereClauses, fmt.Sprintf(
			`(ts_de @@ plainto_tsquery('german', $%d) OR ts_en @@ plainto_tsquery('english', $%d))`,
			argIdx, argIdx,
		))
		args = append(args, query)
		argIdx++
		// C2-2 / OPS-W1 review A2: the two FTS GIN indexes are PARTIAL since
		// migration 145, and a partial index is only used when the planner can
		// PROVE its predicate. The type filters below are bind parameters, which
		// prove nothing under a generic plan — and pgx makes that plan
		// production-reachable. Measured at 100 000 rows, this seam
		// (/api/context/search, `ctx search`) under force_generic_plan: with
		// types=[knowledge,reference] 37 713 (idx_blocks_type_name, FTS demoted
		// to a filter) against 1 634 over both GIN indexes once the predicate is
		// declared; with types_exclude covering the deny-list 40 203 (full Seq
		// Scan) against 1 432.
		//
		// So when the caller's own opt-in already implies the index predicate,
		// the statement DECLARES it as constant text. Redundant by construction
		// — it never removes a row the type filter would have kept
		// (impliesHardFTSDeny, partial_fts_optin.go) — and it consumes no
		// $-index, so the FTS ORDER BY at $2 and the grant array stay in place.
		// Without a type opt-in nothing is declared and the branch keeps the Seq
		// Scan the review measured (721 → 8 304): deny-listed blocks stay
		// browseable (the D5 asymmetry above), and whether that trade holds at
		// corpus scale is a D-01/D-05 decision, not a statement detail.
		if impliesHardFTSDeny(types, typesExclude) {
			whereClauses = append(whereClauses, hardFTSDenyConjunct)
		}
	}

	if category != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("category = $%d", argIdx))
		args = append(args, category)
		argIdx++
	}

	if len(tags) > 0 {
		whereClauses = append(whereClauses, fmt.Sprintf("tags && $%d", argIdx))
		args = append(args, tags)
		argIdx++
	}
	// tags_all (#219): the block carries EVERY given tag (containment), next
	// to the overlap above, so the two combine to "all of these, and any of
	// those". idx_context_tags (GIN, array_ops) serves @> as it serves &&.
	if len(p.TagsAll) > 0 {
		whereClauses = append(whereClauses, fmt.Sprintf("tags @> $%d", argIdx))
		args = append(args, p.TagsAll)
		argIdx++
	}

	// Server-side type filters (WF T10): bind parameters, opt-in only. The
	// 035-line partial index (WHERE type_name != 'knowledge') covers
	// non-knowledge filter values by predicate implication; the unfiltered
	// default view stays the knowledge-dominated browse path.
	if len(types) > 0 {
		whereClauses = append(whereClauses, fmt.Sprintf("type_name = ANY($%d::text[])", argIdx))
		args = append(args, types)
		argIdx++
	}
	if len(typesExclude) > 0 {
		whereClauses = append(whereClauses, fmt.Sprintf("NOT (type_name = ANY($%d::text[]))", argIdx))
		args = append(args, typesExclude)
		argIdx++
	}

	// Cluster facet (C6, design/03 §4.8): a SEMI-JOIN on the topic handle, never
	// a materialised id list — the planner has to stay free to choose between a
	// hash semi-join (mega cluster) and an index nested loop (selective cluster),
	// §6.6. The subquery binds $1, the SAME read scopes the outer visibility
	// clause binds, so the restriction is doubly fail-closed: membership is
	// scope-filtered AND the outer visibility applies unchanged. An unresolvable
	// handle yields zero rows here — indistinguishable from a resolvable one
	// whose members are all invisible, which is the anti-oracle property (§5.7).
	if p.Cluster != nil && *p.Cluster != "" {
		whereClauses = append(whereClauses,
			fmt.Sprintf("id IN (%s)", clustersql.TopicMemberSubquery(fmt.Sprintf("$%d", argIdx), "$1")))
		args = append(args, *p.Cluster)
		argIdx++
	}

	// Keyset cursor (block-workbench W7). Only the empty-query browse path
	// (ORDER BY updated_at DESC, id DESC) paginates by keyset: resume strictly
	// after the cursor's (updated_at, id) via the row-value comparison
	// (updated_at, id) < ($afterUpdated, $afterId) — "older, or same timestamp
	// with a smaller id". The args are appended AFTER the existing ones (the FTS
	// orderBy hardcodes the query arg at $2 — never shift it). On the FTS path
	// the cursor is INERT (LIMIT-only "top matches; refine for more").
	if after != nil && !hasQuery {
		whereClauses = append(whereClauses, fmt.Sprintf("(updated_at, id) < ($%d, $%d)", argIdx, argIdx+1))
		args = append(args, after.UpdatedAt, after.ID)
		argIdx += 2
	}

	// Visibility OR-clause (T40a): scope = ANY($1) OR id = ANY($grantIdx). The
	// grant uuid[] is allocated AFTER every dynamic arg so it never lands on $2.
	// Mandatory parentheses: NOT is_archived already AND-ed up front, the OR group
	// stays self-contained (a granted archived block must not leak, design/07 §4).
	grantIdx := argIdx
	args = append(args, grantedBlockIDs)
	argIdx++
	whereClauses = append(whereClauses, fmt.Sprintf("(scope = ANY($1::text[]) OR id = ANY($%d::uuid[]))", grantIdx))

	whereClause := strings.Join(whereClauses, " AND ")

	var selectFields string
	var orderBy string
	if compact {
		// The page subquery carries content unread; the preview is computed
		// above it (compactPreviewCols).
		selectFields = compactPageCols
		if hasQuery {
			selectFields += fmt.Sprintf(
				`, GREATEST(ts_rank_cd(ts_de, plainto_tsquery('german', $%d)), ts_rank_cd(ts_en, plainto_tsquery('english', $%d))) AS search_rank`,
				// reuse the query arg index
				2, 2,
			)
			// id DESC is the tie-break among rows of one rank (#368), as in
			// the browse order: without it they came in scan order and the
			// cut at the limit among them was arbitrary.
			orderBy = `search_rank DESC, id DESC`
		} else {
			// Browse path: id DESC is the MANDATORY tiebreak (updated_at is not
			// unique) so the keyset cursor (updated_at, id) is a total order —
			// no row is skipped or repeated across a page boundary (W7).
			orderBy = `updated_at DESC, id DESC`
		}
	} else {
		selectFields = `id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, content, ` + guardRedactedMetadataSQL("$1") + `, created_at, updated_at, ` + untrustedOriginCol
		if hasQuery {
			// The same tie-break as the compact form (#368).
			orderBy = fmt.Sprintf(
				`GREATEST(ts_rank_cd(ts_de, plainto_tsquery('german', $%d)), ts_rank_cd(ts_en, plainto_tsquery('english', $%d))) DESC, id DESC`,
				2, 2,
			)
		} else {
			// Browse path: id DESC is the MANDATORY tiebreak (updated_at is not
			// unique) so the keyset cursor (updated_at, id) is a total order —
			// no row is skipped or repeated across a page boundary (W7).
			orderBy = `updated_at DESC, id DESC`
		}
	}

	args = append(args, limit)
	limitIdx := argIdx

	stmt := fmt.Sprintf(
		`SELECT %s FROM context_blocks WHERE %s ORDER BY %s LIMIT $%d`,
		selectFields, whereClause, orderBy, limitIdx,
	)
	if compact {
		stmt = fmt.Sprintf(`SELECT %s FROM (%s) page ORDER BY %s`, compactPreviewCols, stmt, orderBy)
	}
	return stmt, args
}

// The compact statements (the compact search, both paths, and RecentBlocks)
// are a page subquery and a projection above it (issue #342). The subquery
// filters, sorts and limits with content as it is stored — for a wide block a
// TOAST pointer, which the sort carries without reading it; the projection
// then computes the preview, LEFT(content, 200), and the character count only
// for the rows of the page. In one statement the two expressions sat below
// the sort, so every visible row had its content read out of TOAST and
// decompressed (a character count needs all of it) for a preview the LIMIT
// then dropped: the planner postpones only expressions it costs as expensive
// past a sort, and both are costed as one operator each. Measured on a store
// with 57 % never-ranked rows of 35 000 characters, browse and recent read
// 87 027 buffers at 10 000 rows; as a page subquery they read the heap and the
// page, 937 (TestSearchRecentNeverRankedRowsMeasure). The rows, their order
// and every output column are unchanged — the projection reads the same row
// versions in the same snapshot. The full
// (non-compact) statement needs no subquery: it outputs content unread, and
// its one expensive expression, the redacted metadata, is postponed past the
// sort by the planner itself (it holds a subquery).
const (
	// compactPageCols are the columns the page subquery hands up.
	compactPageCols = `id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, content, created_at, updated_at, untrusted_origin`
	// compactPreviewCols are the columns of a compact answer, in the order
	// runSearchBlocks scans them, computed over the page.
	compactPreviewCols = `id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, LEFT(content, 200) AS content_preview, length(content) AS content_length, created_at, updated_at, ` + untrustedOriginCol
)

// runSearchBlocks executes a statement built by searchBlocksSQL and scans it
// into the preview rows. Split from the builder purely so the builder stays
// pure; the scan shape is unchanged.
func runSearchBlocks(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, sql string, args []any, compact bool) ([]BlockPreview, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search blocks: %w", err)
	}
	defer rows.Close()

	results := make([]BlockPreview, 0)
	for rows.Next() {
		bp := BlockPreview{}
		if compact {
			if err := rows.Scan(&bp.ID, &bp.Category, &bp.Tags, &bp.Title, &bp.Scope, &bp.Sensitivity, &bp.TypeName, &bp.LifecycleState, &bp.TypeSource, &bp.ContentPreview, &bp.ContentLength, &bp.CreatedAt, &bp.UpdatedAt, &bp.UntrustedOrigin); err != nil {
				return nil, fmt.Errorf("store: search blocks scan: %w", err)
			}
		} else {
			if err := rows.Scan(&bp.ID, &bp.Category, &bp.Tags, &bp.Title, &bp.Scope, &bp.Sensitivity, &bp.TypeName, &bp.LifecycleState, &bp.TypeSource, &bp.Content, &bp.Metadata, &bp.CreatedAt, &bp.UpdatedAt, &bp.UntrustedOrigin); err != nil {
				return nil, fmt.Errorf("store: search blocks scan: %w", err)
			}
		}
		bp.Untrusted = untrustedOf(set, bp.TypeName) // V-11
		results = append(results, bp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search blocks rows: %w", err)
	}

	return results, nil
}

// RecentBlocks lists the most recently updated non-archived blocks visible to
// readScopes, newest first, with a 200-char preview — the store backing for the
// ctx_recent tool (F6) and the MCP recent tool. An empty category means no
// category filter; limit <= 0 falls back to 10, capped at 50.
// types / typesExclude (WF T10): opt-in server-side type filters as bind
// parameters, nil/empty = no filter (see SearchBlocks).
//
// grantedBlockIDs (T40a) is the caller's resolved block-grant set — nil/empty
// makes the OR-arm a no-op, so a caller without grants reads exactly as before.
//
// set is the caller's block-type registry snapshot; it fills
// BlockPreview.Untrusted per row (V-11). nil ⇒ the field stays absent.
func RecentBlocks(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, readScopes []string, category string, limit int, types []string, typesExclude []string, grantedBlockIDs []string) ([]BlockPreview, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	limit = ClampLimit(limit, 10, 50)
	q, args := recentBlocksSQL(readScopes, category, limit, types, typesExclude, grantedBlockIDs)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: recent blocks: %w", err)
	}
	defer rows.Close()

	results := make([]BlockPreview, 0, limit)
	for rows.Next() {
		bp := BlockPreview{}
		if err := rows.Scan(&bp.ID, &bp.Category, &bp.Tags, &bp.Title, &bp.Scope, &bp.TypeName, &bp.LifecycleState, &bp.TypeSource, &bp.ContentPreview, &bp.ContentLength, &bp.CreatedAt, &bp.UpdatedAt, &bp.UntrustedOrigin); err != nil {
			return nil, fmt.Errorf("store: recent blocks scan: %w", err)
		}
		bp.Untrusted = untrustedOf(set, bp.TypeName) // V-11
		results = append(results, bp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent blocks rows: %w", err)
	}
	return results, nil
}

// recentBlocksSQL composes RecentBlocks' statement for an already clamped
// limit. Pure, like searchBlocksSQL, so the cost gate EXPLAINs the statement
// production runs.
func recentBlocksSQL(readScopes []string, category string, limit int, types, typesExclude, grantedBlockIDs []string) (string, []any) {
	// $1=scopes, $2=grants (block-grant OR-arm, T40a). The mandatory
	// parentheses keep NOT is_archived OUTSIDE the scope/grant OR (a granted
	// archived block must not leak). category, if present, shifts to $3.
	// A page subquery and the preview above it, like the compact search
	// (compactPageCols).
	q := `SELECT id, category, tags, title, scope, type_name, lifecycle_state, type_source, content, created_at, updated_at, untrusted_origin
	      FROM context_blocks
	      WHERE NOT is_archived AND ( scope = ANY($1::text[]) OR id = ANY($2::uuid[]) )`
	args := []any{readScopes, grantedBlockIDs}
	if category != "" {
		args = append(args, category)
		q += fmt.Sprintf(` AND category = $%d`, len(args))
	}
	if len(types) > 0 {
		args = append(args, types)
		q += fmt.Sprintf(` AND type_name = ANY($%d::text[])`, len(args))
	}
	if len(typesExclude) > 0 {
		args = append(args, typesExclude)
		q += fmt.Sprintf(` AND NOT (type_name = ANY($%d::text[]))`, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY updated_at DESC LIMIT $%d`, len(args))
	return `SELECT id, category, tags, title, scope, type_name, lifecycle_state, type_source, LEFT(content, 200), char_length(content), created_at, updated_at, ` + untrustedOriginCol + `
	      FROM (` + q + `) page ORDER BY updated_at DESC`, args
}

// listCategoriesSQL is ListCategories' statement.
const listCategoriesSQL = `SELECT category, sum(n)::int AS count
		 FROM ` + blockCountRowsSQL + ` k
		 WHERE scope = ANY($1::text[]) AND NOT is_archived
		 GROUP BY category
		 HAVING sum(n) > 0
		 ORDER BY count DESC`

// ListCategories returns distinct categories with block counts. The counts
// are sums over the block counters (migration 170, issue #266), not a count
// over context_blocks: the cost is the number of counter rows of the read
// scopes, whatever the corpus size.
func ListCategories(ctx context.Context, pool *pgxpool.Pool, readScopes []string) ([]CategoryCount, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	rows, err := pool.Query(ctx, listCategoriesSQL, readScopes)
	if err != nil {
		return nil, fmt.Errorf("store: list categories: %w", err)
	}
	defer rows.Close()

	results := make([]CategoryCount, 0)
	for rows.Next() {
		var cc CategoryCount
		if err := rows.Scan(&cc.Category, &cc.Count); err != nil {
			return nil, fmt.Errorf("store: list categories scan: %w", err)
		}
		results = append(results, cc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list categories rows: %w", err)
	}
	return results, nil
}

// activeBlocksSQL is the number of live blocks in the read scopes $1 — the
// total_blocks of GetStats and the coverage denominator ActiveBlockCount —
// from the counter table.
const activeBlocksSQL = `SELECT COALESCE(sum(n), 0)::int FROM ` + blockCountRowsSQL + ` k
		  WHERE scope = ANY($1::text[]) AND NOT is_archived`

// activeCategoriesSQL selects one row per category that has live blocks in
// the read scopes $1 — the count(DISTINCT category) of GetStats and of the
// digest envelope (ActiveCategoryCount), from the counter table.
const activeCategoriesSQL = `SELECT category FROM ` + blockCountRowsSQL + ` k
		  WHERE scope = ANY($1::text[]) AND NOT is_archived
		  GROUP BY category HAVING sum(n) > 0`

// statsSQL is GetStats' statement. The block and category numbers are sums
// over the block counters (blockCountRowsSQL, migration 170). oldest/newest stay queries on
// context_blocks, one per read scope: unnest + LATERAL turns each into a
// min/max over idx_context_scope_active (scope, created_at DESC) WHERE NOT
// is_archived, i.e. one index probe per scope. The plain
// `min(created_at) WHERE scope = ANY($1)` form walks idx_context_created in
// global created_at order and filters by scope: cheap while the read scopes
// hold the newest and the oldest rows of the corpus, linear for a scope whose
// rows all lie behind the rest (issue #266: a 10-block scope older than the
// other 100 000 blocks read 16 951 buffers that way, 8 with LATERAL).
const statsSQL = `SELECT
			(` + activeBlocksSQL + `) AS total_blocks,
			(SELECT count(*) FROM (` + activeCategoriesSQL + `) c)::int AS total_categories,
			(SELECT min(e.m) FROM unnest($1::text[]) AS s(scope)
			   CROSS JOIN LATERAL (SELECT min(b.created_at) AS m FROM context_blocks b
			                        WHERE b.scope = s.scope AND NOT b.is_archived) e) AS oldest_entry,
			(SELECT max(e.m) FROM unnest($1::text[]) AS s(scope)
			   CROSS JOIN LATERAL (SELECT max(b.created_at) AS m FROM context_blocks b
			                        WHERE b.scope = s.scope AND NOT b.is_archived) e) AS newest_entry,
			(SELECT pg_size_pretty(pg_total_relation_size('context_blocks'))) AS db_size`

// GetStats returns aggregate statistics for the context store.
func GetStats(ctx context.Context, pool *pgxpool.Pool, readScopes []string) (*Stats, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	s := &Stats{}
	err := pool.QueryRow(ctx, statsSQL, readScopes).
		Scan(&s.TotalBlocks, &s.TotalCategories, &s.OldestEntry, &s.NewestEntry, &s.DBSize)
	if err != nil {
		return nil, fmt.Errorf("store: get stats: %w", err)
	}
	return s, nil
}

// ListMetaMaxRows is the most rows one list-meta answer holds (issue #212).
// It is the cap the listing always had; since #212 an answer that stops at it
// says so and hands out the position to go on from (ListMetaCursor).
const ListMetaMaxRows = 10000

// ListMetaCursor is a position in the list-meta order (category, title,
// scope): the last row of the previous page. (category, title, scope) is
// unique among the non-archived rows (uq_context_category_title_scope), so
// the order is total and a page boundary skips or repeats no row. The cursor
// is a position only — any three strings are one — and carries no filter or
// scope: each page applies the read scopes and type filters of its own
// request. A nil or empty cursor is the first page.
type ListMetaCursor struct {
	Category string `json:"after_category,omitempty"`
	Title    string `json:"after_title,omitempty"`
	Scope    string `json:"after_scope,omitempty"`
}

// ListMeta returns the blocks without content (metadata listing), at most
// ListMetaMaxRows of them, in (category, title, scope) order, starting after
// the cursor. more reports whether rows follow the last one returned: the
// statement reads one row past the page, so it is an observed fact.
// types / typesExclude (WF T10): opt-in server-side type filters as bind
// parameters, nil/empty = no filter (see SearchBlocks).
func ListMeta(ctx context.Context, pool *pgxpool.Pool, readScopes []string, types []string, typesExclude []string, after *ListMetaCursor) (rows []BlockMeta, more bool, err error) {
	return listMeta(ctx, pool, readScopes, types, typesExclude, after, ListMetaMaxRows)
}

// listMeta is ListMeta with the page size as a parameter, so a test can page
// a small store; production always passes ListMetaMaxRows.
func listMeta(ctx context.Context, pool *pgxpool.Pool, readScopes []string, types []string, typesExclude []string, after *ListMetaCursor, page int) ([]BlockMeta, bool, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, false, err
	}
	q, args := listMetaSQL(readScopes, types, typesExclude, after, page)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: list meta: %w", err)
	}
	defer rows.Close()

	results := make([]BlockMeta, 0)
	for rows.Next() {
		var bm BlockMeta
		if err := rows.Scan(&bm.ID, &bm.Title, &bm.Category, &bm.Tags, &bm.Scope, &bm.TypeName, &bm.LifecycleState, &bm.TypeSource, &bm.UpdatedAt); err != nil {
			return nil, false, fmt.Errorf("store: list meta scan: %w", err)
		}
		results = append(results, bm)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: list meta rows: %w", err)
	}
	if len(results) > page {
		return results[:page], true, nil
	}
	return results, false, nil
}

// listMetaSQL composes ListMeta's statement: the page plus one probe row.
// Pure, like searchBlocksSQL, so a cost gate can EXPLAIN the statement
// production runs. scope is the tie-break that makes the order total; before
// #212 rows of one (category, title) in two scopes came in no defined order.
func listMetaSQL(readScopes []string, types []string, typesExclude []string, after *ListMetaCursor, page int) (string, []any) {
	q := `SELECT id, title, category, tags, scope, type_name, lifecycle_state, type_source, updated_at
		 FROM context_blocks
		 WHERE scope = ANY($1::text[]) AND NOT is_archived`
	args := []any{readScopes}
	if len(types) > 0 {
		args = append(args, types)
		q += fmt.Sprintf(` AND type_name = ANY($%d::text[])`, len(args))
	}
	if len(typesExclude) > 0 {
		args = append(args, typesExclude)
		q += fmt.Sprintf(` AND NOT (type_name = ANY($%d::text[]))`, len(args))
	}
	if after != nil && (after.Category != "" || after.Title != "" || after.Scope != "") {
		args = append(args, after.Category, after.Title, after.Scope)
		q += fmt.Sprintf(` AND (category, title, scope) > ($%d, $%d, $%d)`, len(args)-2, len(args)-1, len(args))
	}
	// The limit stays a literal, as it was: a bind parameter would leave a
	// generic plan to guess the row count. page is the caller's int, never
	// request input.
	q += fmt.Sprintf(` ORDER BY category, title, scope LIMIT %d`, page+1)
	return q, args
}

// LogAccess inserts an access log entry (read or write). principal_id is
// derived from the acting key in SQL (096, F3): key→principal is 1:1 NOT NULL
// since 094, so the person anchor can never drift from the key anchor (INV-A).
//
// An EMPTY blockID books the row with block_id NULL — the column is nullable
// by design (001_initial.sql). That is the write-INTENT shape (Gap-C6-a): a
// staged write has no block yet and may never get one, but the intent must
// still count against the write budget. Every reader of this table is either
// block_id-keyed (temporal gravity, grant probes — a NULL row simply never
// joins) or a (api_key_id, action) aggregate like CheckRateLimitByAction, which
// is precisely the consumer this shape exists for.
func LogAccess(ctx context.Context, pool *pgxpool.Pool, apiKeyID, blockID, action string) error {
	var block *string
	if blockID != "" {
		block = &blockID
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO context_access_log (api_key_id, block_id, action, metadata, principal_id)
		 VALUES ($1::uuid, $2::uuid, $3, '{}'::jsonb,
		         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $1::uuid))`,
		apiKeyID, block, action,
	)
	if err != nil {
		return fmt.Errorf("store: log access: %w", err)
	}
	return nil
}

// ActionBlobWrite is the context_access_log action of a /api/blob/store write.
// It is SEPARATE from the block-write action ("write") by decision (B2/E1-A):
// the two budgets count disjoint populations, so a key at its block-write limit
// can still store blobs and a burst of blob writes cannot starve block writes.
// Metering (LogAccessRef) and gate (CheckRateLimitByAction) read this one
// constant — they cannot drift into a budget that is fed under one name and
// counted under another, which is exactly how the blob budget was dead before.
const ActionBlobWrite = "blob-write"

// LogAccessRef inserts an access log entry that references a BLOB instead of a
// block: block_id stays NULL (it is FK-bound to context_blocks, so a blob id in
// that column raises 23503 — the Gap-C0-b defect) and blob_id carries the
// reference, unconstrained by migration 122 so the row outlives its blob.
//
// blobID may be empty. A blob write books its row BEFORE the upsert runs, as
// an INTENT: the expensive work must cost budget even when it ends in a
// constraint violation, and the id it would carry is not known yet. The caller
// fills it in afterwards via AttributeAccessBlob, addressing the returned id.
//
// principal_id is derived from the acting key in SQL exactly as in LogAccess.
func LogAccessRef(ctx context.Context, pool *pgxpool.Pool, apiKeyID, blobID, action string) (string, error) {
	var blob *string
	if blobID != "" {
		blob = &blobID
	}
	var logID string
	err := pool.QueryRow(ctx,
		`INSERT INTO context_access_log (api_key_id, blob_id, action, metadata, principal_id)
		 VALUES ($1::uuid, $2::uuid, $3, '{}'::jsonb,
		         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $1::uuid))
		 RETURNING id::text`,
		apiKeyID, blob, action,
	).Scan(&logID)
	if err != nil {
		return "", fmt.Errorf("store: log access ref: %w", err)
	}
	return logID, nil
}

// AttributeAccessBlob attaches a blob id to an access log row booked earlier by
// LogAccessRef. By primary key, so it stays a single-row update no matter how
// large the audit trail grows.
func AttributeAccessBlob(ctx context.Context, pool *pgxpool.Pool, logID, blobID string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_access_log SET blob_id = $2::uuid WHERE id = $1::uuid`,
		logID, blobID,
	)
	if err != nil {
		return fmt.Errorf("store: attribute access blob: %w", err)
	}
	return nil
}

// GuardList returns flagged blocks for guard review. types is an optional
// server-side type_name filter (bind parameter, WF-T10 line — never a client
// filter over paginated lists at target scale).
func GuardList(ctx context.Context, pool *pgxpool.Pool, readScopes []string, category, status string, types []string, limit int) ([]GuardListItem, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	limit = ClampLimit(limit, 50, 200)

	whereClauses := []string{
		"NOT b.is_archived",
		"b.scope = ANY($1::text[])",
	}
	args := []any{readScopes}
	argIdx := 2

	if status != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("b.guard_status = $%d", argIdx))
		args = append(args, status)
		argIdx++
	} else {
		whereClauses = append(whereClauses, "b.guard_status NOT IN ('active', 'clean')")
	}

	if category != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("b.category = $%d", argIdx))
		args = append(args, category)
		argIdx++
	}

	if len(types) > 0 {
		whereClauses = append(whereClauses, fmt.Sprintf("b.type_name = ANY($%d::text[])", argIdx))
		args = append(args, types)
		argIdx++
	}

	args = append(args, limit)
	limitIdx := argIdx
	argIdx++

	// We also need readScopes for the cross-scope redaction check
	args = append(args, readScopes)
	scopeCheckIdx := argIdx

	whereClause := strings.Join(whereClauses, " AND ")

	query := fmt.Sprintf(`SELECT b.id, b.title, b.category, b.scope, b.type_name, b.guard_status,
		b.metadata->>'guard_similarity' AS similarity,
		CASE
			WHEN mb.id IS NULL THEN NULL
			WHEN mb.scope = ANY($%d) THEN b.metadata->>'guard_matched_id'
			ELSE NULL
		END AS matched_id,
		CASE
			WHEN mb.id IS NULL THEN NULL
			WHEN mb.scope = ANY($%d) THEN mb.title
			ELSE '`+redact.Redacted+`'
		END AS matched_title,
		b.metadata->>'guard_checked_at' AS checked_at,
		b.updated_at
	FROM context_blocks b
	LEFT JOIN context_blocks mb ON mb.id::text = b.metadata->>'guard_matched_id'
	WHERE %s
	ORDER BY (b.metadata->>'guard_similarity')::numeric DESC NULLS LAST
	LIMIT $%d`,
		scopeCheckIdx, scopeCheckIdx, whereClause, limitIdx,
	)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: guard list: %w", err)
	}
	defer rows.Close()

	results := make([]GuardListItem, 0)
	for rows.Next() {
		var item GuardListItem
		if err := rows.Scan(
			&item.ID, &item.Title, &item.Category, &item.Scope, &item.Type, &item.GuardStatus,
			&item.Similarity, &item.MatchedID, &item.MatchedTitle,
			&item.CheckedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: guard list scan: %w", err)
		}
		results = append(results, item)
	}
	return results, nil
}

// guardStatsSQL is GetGuardStats' statement. The block numbers are sums over
// the block counters (blockCountRowsSQL, migration 170) with the predicates the per-call counts
// had: every live number is NOT is_archived, archived_dups counts
// guard_status = 'archived_dup' archived or not, unchecked is guard_status IS
// NULL. The blocks the guard still has to check (pending_count, dirty_since
// of manage guard-stats) are counted by guard.PendingStats since migration
// 181 (issue #341); context_guard_state is read for last_guard_at only.
const guardStatsSQL = `SELECT
			COALESCE(sum(c.n) FILTER (WHERE NOT c.is_archived), 0)::int,
			COALESCE(sum(c.n) FILTER (WHERE NOT c.is_archived AND c.guard_status = 'active'), 0)::int,
			COALESCE(sum(c.n) FILTER (WHERE NOT c.is_archived AND c.guard_status = 'clean'), 0)::int,
			COALESCE(sum(c.n) FILTER (WHERE NOT c.is_archived AND c.guard_status = 'needs_review'), 0)::int,
			COALESCE(sum(c.n) FILTER (WHERE NOT c.is_archived AND c.guard_status = 'near_duplicate'), 0)::int,
			COALESCE(sum(c.n) FILTER (WHERE NOT c.is_archived AND c.guard_status IS NULL), 0)::int,
			COALESCE(sum(c.n) FILTER (WHERE c.guard_status = 'archived_dup'), 0)::int,
			COALESCE((SELECT count(*)::int FROM context_write_log), 0),
			(SELECT last_guard_at FROM context_guard_state WHERE id = true)
		 FROM ` + blockCountRowsSQL + ` c
		 WHERE c.scope = ANY($1::text[])`

// GetGuardStats returns guard state statistics.
func GetGuardStats(ctx context.Context, pool *pgxpool.Pool, readScopes []string) (*GuardStats, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	gs := &GuardStats{}
	err := pool.QueryRow(ctx, guardStatsSQL, readScopes).Scan(
		&gs.TotalBlocks, &gs.Active, &gs.Clean, &gs.NeedsReview, &gs.NearDuplicate,
		&gs.Unchecked, &gs.ArchivedDups, &gs.WriteLogEntries,
		&gs.LastGuardAt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: guard stats: %w", err)
	}
	return gs, nil
}

// GuardResolve resolves a flagged block with either "archive" or "keep". The
// block must live in one of the caller's write-eligible scopes.
func GuardResolve(ctx context.Context, pool *pgxpool.Pool, id, resolution string, writeScopes []string) (*Block, error) {
	var query string
	switch resolution {
	case "archive":
		// I7 / S3 (W01-2a Nachbesserung, review finding #5): guard-resolve is
		// the SECOND archive verb a client can reach, and the single-id path
		// deliberately does not require a flagged status (see the batch
		// comment below) — so without this conjunct it is manage-delete's twin
		// for freeing a derivative's identity. Derived types carry
		// guard.candidate=false and can never be flagged legitimately, which
		// makes the exclusion free in practice and load-bearing on paper.
		query = `UPDATE context_blocks SET
			is_archived = true,
			guard_status = 'archived_dup',
			metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
				'guard_resolved_at', now()::text,
				'guard_resolution', 'archive'
			),
			updated_at = now()
		WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived
		  AND ` + notDerivedSQL + `
		RETURNING id, title, category, scope, guard_status, created_at, updated_at`
	case "keep":
		query = `UPDATE context_blocks SET
			guard_status = 'active',
			metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
				'guard_resolved_at', now()::text,
				'guard_resolution', 'keep'
			),
			updated_at = now()
		WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived
		RETURNING id, title, category, scope, guard_status, created_at, updated_at`
	default:
		return nil, fmt.Errorf("store: guard resolve: invalid resolution %q (must be 'archive' or 'keep')", resolution)
	}

	b := &Block{}
	err := pool.QueryRow(ctx, query, id, writeScopes).Scan(
		&b.ID, &b.Title, &b.Category, &b.Scope, &b.GuardStatus, &b.CreatedAt, &b.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// 'keep' cannot be refused by the S3 conjunct (it does not carry one),
		// so this diagnosis only ever fires for 'archive' — the same shape
		// UpdateBlock and DeleteBlock use.
		return nil, updateBlockMissReason(ctx, pool, id, writeScopes)
	}
	if err != nil {
		return nil, fmt.Errorf("store: guard resolve: %w", err)
	}
	return b, nil
}

// guardResolveBatchMax caps one batch call; larger queues page over the cap.
const guardResolveBatchMax = 500

// guardBatchResolvable is the set of guard states a BATCH resolve may touch.
// Deliberately narrower than the single-id GuardResolve (which resolves any
// non-archived block in a writable scope): a batch amplifies mistakes, so an
// id that is not actually flagged is reported as skipped instead of silently
// mass-archived (minimal blast radius on the destructive path).
var guardBatchResolvable = []string{"needs_review", "near_duplicate", "possible_duplicate"}

// dedupeGuardBatchIDs validates ids in Go so one malformed entry degrades to
// a skip instead of failing the whole statement; dedup keeps the accounting
// one-row-per-id.
func dedupeGuardBatchIDs(ids []string) (valid []string, skipped []GuardSkip) {
	skipped = make([]GuardSkip, 0)
	valid = make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := uuid.Parse(id); err != nil {
			skipped = append(skipped, GuardSkip{ID: id, Reason: "invalid_id"})
			continue
		}
		valid = append(valid, id)
	}
	return valid, skipped
}

// classifyGuardBatch locks the candidate rows (so a concurrent guard run
// cannot re-stamp guard_status between classification and update) and splits
// them into resolvable ids and skips. A row invisible in writeScopes yields
// not_found — indistinguishable from nonexistent (no existence oracle).
func classifyGuardBatch(ctx context.Context, tx pgx.Tx, valid, writeScopes []string, skipped []GuardSkip) (resolvable []string, _ []GuardSkip, err error) {
	rows, err := tx.Query(ctx,
		`SELECT id, is_archived, guard_status
		 FROM context_blocks
		 WHERE id = ANY($1::uuid[]) AND scope = ANY($2::text[])
		 FOR UPDATE`,
		valid, writeScopes,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: guard resolve batch: select: %w", err)
	}
	defer rows.Close()
	type rowState struct {
		archived bool
		status   string
	}
	states := make(map[string]rowState, len(valid))
	for rows.Next() {
		var id, status string
		var archived bool
		if err := rows.Scan(&id, &archived, &status); err != nil {
			return nil, nil, fmt.Errorf("store: guard resolve batch: scan: %w", err)
		}
		states[id] = rowState{archived: archived, status: status}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: guard resolve batch: rows: %w", err)
	}

	resolvable = make([]string, 0, len(valid))
	for _, id := range valid {
		st, ok := states[id]
		switch {
		case !ok:
			skipped = append(skipped, GuardSkip{ID: id, Reason: "not_found"})
		case st.archived:
			skipped = append(skipped, GuardSkip{ID: id, Reason: "already_archived"})
		case !slices.Contains(guardBatchResolvable, st.status):
			skipped = append(skipped, GuardSkip{ID: id, Reason: "not_flagged"})
		default:
			resolvable = append(resolvable, id)
		}
	}
	return resolvable, skipped, nil
}

// execGuardBatchUpdate applies one resolution to the pre-classified ids and
// returns the updated blocks (same RETURNING shape as the single-id path).
func execGuardBatchUpdate(ctx context.Context, tx pgx.Tx, resolvable, writeScopes []string, resolution string) ([]Block, error) {
	setClause := `guard_status = 'active'`
	if resolution == "archive" {
		setClause = `is_archived = true, guard_status = 'archived_dup'`
	}
	updated, err := tx.Query(ctx,
		`UPDATE context_blocks SET `+setClause+`,
			metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
				'guard_resolved_at', now()::text,
				'guard_resolution', $3::text
			),
			updated_at = now()
		WHERE id = ANY($1::uuid[]) AND scope = ANY($2::text[])
		RETURNING id, title, category, scope, guard_status, created_at, updated_at`,
		resolvable, writeScopes, resolution,
	)
	if err != nil {
		return nil, fmt.Errorf("store: guard resolve batch: update: %w", err)
	}
	defer updated.Close()
	resolved := make([]Block, 0, len(resolvable))
	for updated.Next() {
		var b Block
		if err := updated.Scan(&b.ID, &b.Title, &b.Category, &b.Scope, &b.GuardStatus, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: guard resolve batch: update scan: %w", err)
		}
		resolved = append(resolved, b)
	}
	if err := updated.Err(); err != nil {
		return nil, fmt.Errorf("store: guard resolve batch: update rows: %w", err)
	}
	return resolved, nil
}

// GuardResolveBatch resolves many flagged blocks with one resolution inside a
// single transaction. Every requested id is accounted for: either in the
// returned resolved blocks or in skipped with a reason. Cross-scope ids
// surface as not_found (no existence oracle).
func GuardResolveBatch(ctx context.Context, pool *pgxpool.Pool, ids []string, resolution string, writeScopes []string) ([]Block, []GuardSkip, error) {
	if resolution != "archive" && resolution != "keep" {
		return nil, nil, fmt.Errorf("store: guard resolve batch: invalid resolution %q (must be 'archive' or 'keep')", resolution)
	}
	if err := RequireScopes(writeScopes); err != nil { // T07 fail-closed
		return nil, nil, err
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("store: guard resolve batch: empty id list")
	}
	if len(ids) > guardResolveBatchMax {
		return nil, nil, fmt.Errorf("store: guard resolve batch: %d ids exceed the cap of %d", len(ids), guardResolveBatchMax)
	}

	valid, skipped := dedupeGuardBatchIDs(ids)
	resolved := make([]Block, 0, len(valid))
	if len(valid) == 0 {
		return resolved, skipped, nil
	}

	if err := pgxdb.Write(ctx, pool, pgxdb.At("store: guard resolve batch"), func(tx pgx.Tx) error {
		resolvable, sk, err := classifyGuardBatch(ctx, tx, valid, writeScopes, skipped)
		skipped = sk
		if err != nil {
			return err
		}
		if len(resolvable) > 0 {
			if resolved, err = execGuardBatchUpdate(ctx, tx, resolvable, writeScopes, resolution); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return resolved, skipped, nil
}
