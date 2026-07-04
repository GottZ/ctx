package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoScopes is the fail-closed sentinel: a read path resolved an EMPTY scope
// set. It is an error, never silently "all scopes". Callers gate on it via
// RequireScopes (design/01 §5.4).
var ErrNoScopes = errors.New("store: no scopes resolved (fail-closed)")

// RequireScopes enforces fail-closed: an empty scope slice is an error, NOT
// "all scopes". It mirrors the guard that today lives only in rrf.Search
// (search.go:65-67) so the later fail-closed-hardening wave (T07) can spread it
// to SearchBlocks + the manage reads as a per-path second line of defence behind
// the auth choke point (design/01 §5.0/§5.4). T04 only DEFINES the helper; the
// wiring into the read call-sites is T07.
func RequireScopes(scopes []string) error {
	if len(scopes) == 0 {
		return ErrNoScopes
	}
	return nil
}

// TenantScopes returns every scope OWNED by the given tenant (Modell C: a lookup
// on context_tenant_scopes WHERE tenant_id = $1). This is the per-TENANT scope
// set — distinct from a single KEY's read_scopes (built in ctx_auth, migration
// 060): it is the data foundation ("which scopes belong to whom") the visibility
// axis consumes, plus the cross-tenant isolation guarantee that a tenant id NEVER
// resolves to a foreign scope (the PK on context_tenant_scopes.scope ⇒ one scope
// = one tenant, no multi-mapping). It does NOT change how visibility works today
// (design/01 §4.2): SearchBlocks/ctx_rrf already gate on `scope = ANY(...)`; this
// supplies the per-tenant list those paths consume.
//
// An unknown or empty tenant yields an empty slice (NOT an error) — callers MUST
// pass the result through RequireScopes (or rely on the auth choke point) so that
// empty fails CLOSED rather than collapsing to "all scopes" via the PG
// `scope = ANY('{}')` artefact (design/01 §5.0/§5.2). An empty tenantID short-
// circuits before the query (an empty string is not a valid UUID and would raise
// 22P02; semantically it is "no tenant" → no scopes). The result is ORDER BY
// scope for a deterministic, testable order (TenantScopes is a set; the wire-
// ordered read_scopes[0]=home invariant belongs to ctx_auth, not here).
func TenantScopes(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]string, error) {
	if tenantID == "" {
		return nil, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT scope FROM context_tenant_scopes WHERE tenant_id = $1::uuid ORDER BY scope`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: tenant scopes query: %w", err)
	}
	defer rows.Close()

	var scopes []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: tenant scopes scan: %w", err)
		}
		scopes = append(scopes, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tenant scopes rows: %w", err)
	}
	return scopes, nil
}

// TenantStatusForScope resolves the OWNING tenant's lifecycle status for a scope
// via context_tenant_scopes → context_tenants (Modell C: one scope ⇒ one tenant,
// PK on context_tenant_scopes.scope). found=false when the scope maps to no
// tenant — the single-tenant transition, where an unmapped scope has no tenant to
// suspend — so callers proceed rather than fail closed on it (059 seeds the
// existing {private,work,shared} → default tenant, so real sessions DO resolve).
// The sole consumer is the E6 session-suspend-cut (T05c, chat/engine.go): it cuts
// a running turn only when found && status != "active". This is a status lookup,
// NOT a re-auth — it never touches read_scopes or last_used_at (design/01 §5.2).
func TenantStatusForScope(ctx context.Context, pool *pgxpool.Pool, scope string) (status string, found bool, err error) {
	if scope == "" {
		return "", false, nil
	}
	err = pool.QueryRow(ctx,
		`SELECT t.status
		   FROM context_tenant_scopes ts
		   JOIN context_tenants t ON t.id = ts.tenant_id
		  WHERE ts.scope = $1`, scope).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: tenant status for scope: %w", err)
	}
	return status, true, nil
}

// Tenant is a row in context_tenants — the owner/management register (059).
//
// MaxScopes / MaxKeys are the structural per-tenant caps carried by the typed
// INTEGER columns added in migration 069 (max_scopes/max_keys, CHECK >= 0). A
// nil pointer = SQL NULL = unlimited (the 063 NULL-unlimited contract). 069's
// column DEFAULT is a concrete fail-closed cap (25/50); only the system/default
// tenant is seeded NULL. GetTenant/ListTenants echo these automatically because
// they share tenantCols + scanTenant.
type Tenant struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"` // active | suspended | offboarding (059 CHECK)
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	MaxScopes   *int      `json:"max_scopes"` // nil = unlimited (069 max_scopes)
	MaxKeys     *int      `json:"max_keys"`   // nil = unlimited (069 max_keys)
}

// ErrTenantNotFound is returned by GetTenant/UpdateTenant when no tenant matches
// the id — the 404 path (no existence oracle beyond the admin-gated action).
var ErrTenantNotFound = errors.New("store: tenant not found")

// ErrTenantSlugExists is returned by CreateTenant on a UNIQUE(slug) violation
// (incl. a second 'default') — the 409 path. Rejecting '_'-prefixed slugs (the
// 400 path, reservedSlug) is the caller's job; this enforces only schema gates.
var ErrTenantSlugExists = errors.New("store: tenant slug already exists")

// ErrScopeExists is returned by AssignTenantScope on a 23505 PK violation on
// context_tenant_scopes.scope — the scope string is GLOBALLY unique (one scope =
// one tenant), so a duplicate is the 409 path. The store inserts the scope AS-IS;
// the auto-prefix policy (<slug>:<name>) lives in the handler, not here.
var ErrScopeExists = errors.New("store: scope already assigned")

// ErrScopeQuotaExceeded is returned by AssignTenantScope when the tenant already
// owns >= max_scopes scopes — the 429 path. The count and the insert run inside
// one transaction under a context_tenants-row lock, so the cap is race-tight (no
// TOCTOU); a nil or negative maxScopes means unlimited (the cap is skipped).
var ErrScopeQuotaExceeded = errors.New("store: tenant scope quota exceeded")

// tenantCols is the shared SELECT/RETURNING column list. max_scopes/max_keys
// (069) are read AS-IS — typed INTEGER columns, so no defensive cast is needed
// (a NULL scans into a nil *int = unlimited; a value is always a CHECK-validated
// non-negative int). This is the typed-column path the JSONB design (02 §2) was
// superseded by — the §2 poison-the-shared-aggregate cast risk does not exist
// for a typed column.
const tenantCols = `id::text, slug, display_name, status, created_at, updated_at, max_scopes, max_keys`

func scanTenant(row pgx.Row) (*Tenant, error) {
	var t Tenant
	if err := row.Scan(&t.ID, &t.Slug, &t.DisplayName, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.MaxScopes, &t.MaxKeys); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTenantTx is the tx-composable core of CreateTenant: it runs the INSERT
// against a caller-owned transaction (pgx.Tx) instead of beginning its own. This
// is what lets the compound tenant-create bootstrap (tenant row + initial scope +
// owner key) commit ATOMICALLY — either all three rows land or none do, so a
// partial failure never leaves a half-created, inert tenant (K10). The
// 23505 → ErrTenantSlugExists mapping (→ 409) lives HERE, so both the pool wrapper
// and the bootstrap path surface the identical typed error. Slug-namespace
// validation (reservedSlug, → 400) is the caller's job; this enforces only the
// schema gates.
func CreateTenantTx(ctx context.Context, tx pgx.Tx, slug, displayName string) (*Tenant, error) {
	t, err := scanTenant(tx.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name)
		 VALUES ($1, $2)
		 RETURNING `+tenantCols, slug, displayName))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrTenantSlugExists
		}
		return nil, fmt.Errorf("store: create tenant: %w", err)
	}
	return t, nil
}

// CreateTenant inserts a new tenant (status defaults to 'active' per 059). A
// duplicate slug — including a second 'default' — raises 23505 and is returned
// as the typed ErrTenantSlugExists (→ 409). It is a thin pool wrapper over
// CreateTenantTx (begin → insert → commit), so the standalone path and the
// bootstrap-composed path share one INSERT body (no logic drift).
func CreateTenant(ctx context.Context, pool *pgxpool.Pool, slug, displayName string) (*Tenant, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: create tenant begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := CreateTenantTx(ctx, tx, slug, displayName)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: create tenant commit: %w", err)
	}
	return t, nil
}

// AssignTenantScope registers scope → tenantID in context_tenant_scopes, race-
// gated and quota-capped, in ONE transaction. It is the store mechanic behind the
// BE5 self-service scope-create action. The handler owns the auto-prefix policy
// (<slug>:<name>, built from the DB-resolved slug, never the payload) — this
// function inserts `scope` AS-IS and NEVER prefixes (mechanic ⟂ policy, S1).
//
// Race gate: `SELECT id FROM context_tenants WHERE id=$1 FOR UPDATE` locks the
// OWNING tenant's row, serialising ALL concurrent scope-assigns of THIS tenant.
// The count→insert then runs under that lock, so two parallel assigns can NOT both
// observe count < max_scopes and both commit (no TOCTOU). The lock target is the
// shared context_tenants row deliberately — a per-scope lock would not serialise
// two DIFFERENT scope names. The lock doubles as the existence check: no row (or a
// malformed id → 22P02) collapses to ErrTenantNotFound, mirroring GetTenant's
// no-oracle 404 contract.
//
// Quota: maxScopes == nil OR *maxScopes < 0 means UNLIMITED (cap skipped) — the
// bootstrap owner-scope is registered with -1 so it never wedges on max_scopes=0.
// Otherwise count(*) of the tenant's scopes is read under the lock; cnt >=
// *maxScopes → ErrScopeQuotaExceeded. The count covers ALL of the tenant's scopes
// (context_tenant_scopes has no soft-delete column and there is no scope-reclaim).
//
// INSERT carries NO ON CONFLICT (no silent re-assign): 23505 → ErrScopeExists (the
// global scope PK), 23503 → ErrTenantNotFound (FK backstop — unreachable while the
// row lock is held, kept for defence in depth). created is true on a committed
// insert (always true on the nil-error path, since there is no ON CONFLICT).
func AssignTenantScope(ctx context.Context, pool *pgxpool.Pool, tenantID, scope string, maxScopes *int) (created bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: assign scope begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err = AssignTenantScopeTx(ctx, tx, tenantID, scope, maxScopes)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: assign scope commit: %w", err)
	}
	return created, nil
}

// AssignTenantScopeTx is the tx-composable core of AssignTenantScope: the same
// lock → quota-count → insert mechanic, but against a caller-owned transaction
// (pgx.Tx) instead of beginning its own. The compound tenant-create bootstrap
// composes it INTO the CreateTenantTx/MintOwnerKey transaction, registering the
// initial scope with maxScopes=-1 (cap-free) so the FIRST scope never wedges on a
// max_scopes=0 seed, and BEFORE the owner key so the key's home_scope is already
// registered (T22). The race + quota + error-mapping contract is identical to the
// pool wrapper (see AssignTenantScope's doc) — only the transaction ownership
// differs.
func AssignTenantScopeTx(ctx context.Context, tx pgx.Tx, tenantID, scope string, maxScopes *int) (created bool, err error) {
	// Lock the owning tenant row: per-tenant serialisation + existence check in one
	// step. No row (or a malformed id → 22P02) → ErrTenantNotFound (404, no oracle).
	var lockedID string
	if err = tx.QueryRow(ctx,
		`SELECT id FROM context_tenants WHERE id = $1::uuid FOR UPDATE`, tenantID).Scan(&lockedID); err != nil {
		if tenantNotFound(err) {
			return false, ErrTenantNotFound
		}
		return false, fmt.Errorf("store: assign scope lock: %w", err)
	}

	// Quota cap (nil / negative = unlimited). Counted under the lock so it observes
	// any competing assign's committed insert — race-tight, no TOCTOU.
	if maxScopes != nil && *maxScopes >= 0 {
		var cnt int
		if err = tx.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_scopes WHERE tenant_id = $1::uuid`, tenantID).Scan(&cnt); err != nil {
			return false, fmt.Errorf("store: assign scope count: %w", err)
		}
		if cnt >= *maxScopes {
			return false, ErrScopeQuotaExceeded
		}
	}

	// Insert AS-IS, no ON CONFLICT (no silent re-assign).
	if _, err = tx.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, scope, tenantID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505":
				return false, ErrScopeExists
			case "23503":
				return false, ErrTenantNotFound
			}
		}
		return false, fmt.Errorf("store: assign scope insert: %w", err)
	}

	return true, nil
}

// ListTenants returns all tenants, newest first.
func ListTenants(ctx context.Context, pool *pgxpool.Pool) ([]Tenant, error) {
	rows, err := pool.Query(ctx, `SELECT `+tenantCols+` FROM context_tenants ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer rows.Close()

	var tenants []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan tenant: %w", err)
		}
		tenants = append(tenants, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tenants rows: %w", err)
	}
	return tenants, nil
}

// tenantNotFound maps BOTH "no such tenant" cases to ErrTenantNotFound: a
// well-formed-but-absent id (pgx.ErrNoRows) AND a MALFORMED id (22P02 from the
// ::uuid cast on a non-UUID string). Both must surface as the SAME 404 — a raw
// 22P02 → 500 would distinguish "malformed" from "well-formed-but-absent", a
// (weak) existence side-channel and an inconsistent contract.
func tenantNotFound(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// GetTenant returns one tenant by id, or ErrTenantNotFound. An empty, absent, OR
// malformed id all collapse to ErrTenantNotFound (404, no oracle). The empty
// short-circuit avoids a needless round-trip + the ”::uuid 22P02; a non-empty
// malformed id is caught by tenantNotFound after the cast.
func GetTenant(ctx context.Context, pool *pgxpool.Pool, id string) (*Tenant, error) {
	if id == "" {
		return nil, ErrTenantNotFound
	}
	t, err := scanTenant(pool.QueryRow(ctx,
		`SELECT `+tenantCols+` FROM context_tenants WHERE id = $1::uuid`, id))
	if err != nil {
		if tenantNotFound(err) {
			return nil, ErrTenantNotFound
		}
		return nil, fmt.Errorf("store: get tenant: %w", err)
	}
	return t, nil
}

// TenantLimits reads the structural per-tenant caps straight off the typed 069
// columns (max_scopes/max_keys). A NULL column scans into a nil *int = unlimited
// (the 063 NULL-unlimited contract); a non-NULL column is always a CHECK-bounded
// non-negative int, so the read needs no defensive cast. An empty, absent, OR
// malformed id all collapse to ErrTenantNotFound (404, no oracle), reusing
// GetTenant's tenantNotFound mapping (well-formed-but-absent pgx.ErrNoRows AND
// the 22P02 from the ::uuid cast on a non-UUID string). This is a pure read — it
// touches no other tenant column. Validation of the returned caps is the caller's
// job; this function is the storage accessor only.
func TenantLimits(ctx context.Context, pool *pgxpool.Pool, tenantID string) (maxScopes, maxKeys *int, err error) {
	if tenantID == "" {
		return nil, nil, ErrTenantNotFound
	}
	if err = pool.QueryRow(ctx,
		`SELECT max_scopes, max_keys FROM context_tenants WHERE id = $1::uuid`, tenantID).
		Scan(&maxScopes, &maxKeys); err != nil {
		if tenantNotFound(err) {
			return nil, nil, ErrTenantNotFound
		}
		return nil, nil, fmt.Errorf("store: tenant limits: %w", err)
	}
	return maxScopes, maxKeys, nil
}

// SetTenantLimits writes the structural per-tenant caps onto the typed 069
// columns. A nil argument sets its column to SQL NULL = unlimited; a non-nil
// value writes the int. NO validation lives here — the >= 0 / both-required
// rules are the handler's job (BEQ-1b); the 069 CHECK (max_* >= 0) is the
// DB-level backstop (a negative value surfaces as 23514, not a silent write).
// 0 rows affected → ErrTenantNotFound (a well-formed-but-absent id), and a
// malformed id raises 22P02 mapped to the same 404 — both via the no-oracle
// contract. This is a pure column write: it never touches scopes, keys, or
// status, only stamps updated_at.
func SetTenantLimits(ctx context.Context, pool *pgxpool.Pool, tenantID string, maxScopes, maxKeys *int) error {
	if tenantID == "" {
		return ErrTenantNotFound
	}
	tag, err := pool.Exec(ctx,
		`UPDATE context_tenants
		    SET max_scopes = $2, max_keys = $3, updated_at = now()
		  WHERE id = $1::uuid`, tenantID, maxScopes, maxKeys)
	if err != nil {
		if tenantNotFound(err) {
			return ErrTenantNotFound
		}
		return fmt.Errorf("store: set tenant limits: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTenantNotFound
	}
	return nil
}

// SetTenantLimitsTx is the tx-composable column write behind the compound
// tenant-create limit seeding: the same typed-column UPDATE as SetTenantLimits,
// but against a caller-owned transaction (pgx.Tx) so the caps commit IN THE SAME
// tx as the tenant row (no uncapped window between create and seed). nil = SQL NULL
// = unlimited; the >= 0 / both-required validation stays the handler's job. A
// well-formed-but-absent id (0 rows) or a malformed id (22P02) both collapse to
// ErrTenantNotFound (no-oracle) — unreachable on the bootstrap path, where the row
// was just inserted in the same tx, but kept for the contract.
func SetTenantLimitsTx(ctx context.Context, tx pgx.Tx, tenantID string, maxScopes, maxKeys *int) error {
	if tenantID == "" {
		return ErrTenantNotFound
	}
	tag, err := tx.Exec(ctx,
		`UPDATE context_tenants
		    SET max_scopes = $2, max_keys = $3, updated_at = now()
		  WHERE id = $1::uuid`, tenantID, maxScopes, maxKeys)
	if err != nil {
		if tenantNotFound(err) {
			return ErrTenantNotFound
		}
		return fmt.Errorf("store: set tenant limits: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTenantNotFound
	}
	return nil
}

// pruneBatchSize bounds each DELETE so a 1M+ offboarding does not take one giant
// lock / WAL burst (design/01 §4.3.1 / §6.3 N11). Each table is drained in a loop
// of DELETE ... LIMIT until a round affects 0 rows. The testcontainer data is tiny
// so the loop runs once.
//
// TENANT-DECISION(prune-batch-size): 2000 rows/batch — a balance of lock duration
// vs round-trips. Umentscheidbar: raise for fewer round-trips on huge tenants,
// lower if lock contention is observed. Not load-bearing for correctness.
const pruneBatchSize = 2000

// pruneScopeDeletes lists the scope-carried data tables to drain, IN FK ORDER.
// Each statement is fully static (no string interpolation → no gosec G201). $1 =
// the tenant's scopes (text[]), $2 = the batch limit. The ctid sub-select is the
// table-agnostic "DELETE a bounded slice" idiom (works for context_dream_links,
// whose PK is composite). ORDER IS LOAD-BEARING:
//
//	context_dream_links FIRST — {source,target}_block_id → context_blocks is
//	NO ACTION (016:18-19), so the links must go before the blocks they reference
//	(else 23503). Every other inbound FK to these tables is SET NULL (access_log /
//	blobs / write_log .block_id; blocks.source_id) or CASCADE (context_temporal,
//	graph_cluster_member → blocks; context_chat_messages → chat_sessions), so
//	blocks/blobs/sources/sessions have no hard inter-ordering among themselves.
//
// TENANT-DECISION(tenant-prune-order): dream_links→blocks→blobs→sources→
// chat_sessions, then keys (HARD), then tenant (CASCADE clears tenant_scopes).
// Umentscheidbar only if a future migration sets the dream_links FK to ON DELETE
// CASCADE (then step 1 would be redundant) — while 016:18-19 has no ON DELETE,
// links-first is forced (design/01 §4.3.1).
//
// SCALE-NAHT (N11, design/01 §6.3 — named, NOT fixed here): context_dream_links
// has NO index on `scope` (verified: all 9 indexes lead with source/
// target_block_id or confidence/version). At the ~2M-link target scale the
// ctid-LIMIT inner SELECT degenerates to a full seq scan per batch → O(n²) for
// this ONE table; context_blocks is fine (idx_context_scope greift). The fix is a
// `scope` btree on context_dream_links, but at 1M+ it MUST be built CONCURRENTLY
// out-of-band (the single-Tx migration runner forbids CONCURRENTLY, 057:8-12) — a
// deliberate index wave, NOT an ad-hoc add here (would ACCESS-EXCLUSIVE-lock a 2M
// table on deploy and collide with the planned 062-068 numbering). Today's corpus
// (~2k links) makes the prune trivial; this is a target-scale seam, not a
// correctness blocker. (Secondary N11: write_log.matched_block_id is an unindexed
// SET-NULL ref → a per-block seq scan only when actually set; rare.)
var pruneScopeDeletes = []struct{ table, sql string }{
	{"context_dream_links", `DELETE FROM context_dream_links WHERE ctid IN (SELECT ctid FROM context_dream_links WHERE scope = ANY($1) LIMIT $2)`},
	{"context_blocks", `DELETE FROM context_blocks WHERE ctid IN (SELECT ctid FROM context_blocks WHERE scope = ANY($1) LIMIT $2)`},
	{"context_blobs", `DELETE FROM context_blobs WHERE ctid IN (SELECT ctid FROM context_blobs WHERE scope = ANY($1) LIMIT $2)`},
	{"context_sources", `DELETE FROM context_sources WHERE ctid IN (SELECT ctid FROM context_sources WHERE scope = ANY($1) LIMIT $2)`},
	{"context_chat_sessions", `DELETE FROM context_chat_sessions WHERE ctid IN (SELECT ctid FROM context_chat_sessions WHERE scope = ANY($1) LIMIT $2)`},
}

// PruneTenant is the full-prune behind tenant-delete (T05b, design/01 §4.3.1): an
// FK-ordered, batched mass-DELETE of the tenant's scope-carried data, then its
// keys, then the tenant row. It is deliberately NOT a metadata-only step and NOT a
// single-Tx CASCADE: the data (blocks/links/blobs/sources/sessions) is tenant-LESS
// (scope-discriminated, Modell C), there is no implicit CASCADE onto
// context_blocks, and at 1M+ blocks a single mass-DELETE would be an over-large
// lock + WAL burst (§6.3 N11). An unknown/malformed tenant id is ErrTenantNotFound
// (404, no oracle) — the prune never pretends to delete a tenant that never was.
//
// AUDIT TABLES (context_access_log / context_llm_log / context_write_log) are NOT
// purged here. They carry the tenant only via api_key_id, and llm_log is a
// TimescaleDB hypertable with NO api_key_id FK at all (verified against the live
// schema; the design/01 §6.3 prose "ON DELETE SET NULL" is wrong for llm_log). Per
// §6.3 this is a Policy/Admin-axis decision (N11), NOT to be silently subsumed
// under the data prune:
//
// TENANT-DECISION(prune-audit-tables): named retention boundary (Option B) — audit
// rows are KEPT; access_log/write_log.api_key_id go NULL via their SET-NULL FK when
// the keys are hard-deleted; llm_log keeps the (FK-less) api_key_id column and the
// retention janitor (CTX_LLMLOG_RETENTION_DAYS) ages the bodies out. Umentscheidbar
// to Option A (DELETE ... WHERE api_key_id = ANY(keys) BEFORE the key delete) when
// a deployment needs a full compliance body-wipe on offboarding — but that is the
// Policy axis (§9 N11), not this mechanism wave.
func PruneTenant(ctx context.Context, pool *pgxpool.Pool, tenantID string) error {
	// Existence + the malformed/absent → ErrTenantNotFound contract, reusing the
	// same oracle as GetTenant (no 22P02 → 500 side-channel).
	if _, err := GetTenant(ctx, pool, tenantID); err != nil {
		return err
	}
	scopes, err := TenantScopes(ctx, pool, tenantID)
	if err != nil {
		return fmt.Errorf("store: prune tenant scopes: %w", err)
	}
	// 1-5: drain the scope-carried tables in FK order (skip if the tenant owns no
	// scope — nothing scope-discriminated can belong to it then).
	if len(scopes) > 0 {
		for _, d := range pruneScopeDeletes {
			for {
				tag, err := pool.Exec(ctx, d.sql, scopes, pruneBatchSize)
				if err != nil {
					return fmt.Errorf("store: prune %s: %w", d.table, err)
				}
				if tag.RowsAffected() == 0 {
					break
				}
			}
		}
	}
	// 6: PROJECT-SECRET-DRAIN (K14, design/02 §5.2): context_secrets rows carry the
	// scope ONLY as a plain column (no FK to context_blocks / context_tenant_scopes,
	// 051), so NOTHING cascades them — a naked prune would leave a live forge PAT
	// (I-F seals it under 'forge.token.<project_id>' in the PROJECT scope, sync.go)
	// AND any future webhook secret ('webhook.github.<id>', W13) alive in a scope
	// whose owning tenant is gone. That is a token surviving its tenant's death —
	// the exact leak §5.2 mandates draining here (the found=false sync gate is the
	// SECOND line; this delete is the FIRST). Drained by SCOPE (the secret's key), so
	// it MUST run before the scope teardown (step 8 CASCADE-clears the scope mapping)
	// while `scopes` still lists the tenant's scopes; ANY of an empty list is a no-op.
	// The I-I gate proves "PruneTenant -> 0 context_secrets rows of the tenant scope".
	if len(scopes) > 0 {
		if _, err := pool.Exec(ctx, `DELETE FROM context_secrets WHERE scope = ANY($1::text[])`, scopes); err != nil {
			return fmt.Errorf("store: prune tenant secrets: %w", err)
		}
	}
	// Drain the project register (K14, design/03 §3.1/§7-W4). context_projects is
	// TENANT-keyed (FK tenant_id -> context_tenants is NO ACTION) AND scope-keyed
	// (FK scope -> context_tenant_scopes is NO ACTION), so it MUST go before BOTH the
	// tenant row delete (step 8) and the scope teardown. context_project_sync_runs
	// (079) AND context_project_sync_map (080, I-F) both CASCADE off context_projects.
	if _, err := pool.Exec(ctx, `DELETE FROM context_projects WHERE tenant_id = $1::uuid`, tenantID); err != nil {
		return fmt.Errorf("store: prune tenant projects: %w", err)
	}
	// 7: HARD-delete the tenant's keys. DeleteApiKey is a SOFT delete (active=
	// false, api_keys.go:102) which would leave the RESTRICT FK and block step 8
	// with 23001.
	if _, err := pool.Exec(ctx, `DELETE FROM context_api_keys WHERE tenant_id = $1::uuid`, tenantID); err != nil {
		return fmt.Errorf("store: prune tenant keys: %w", err)
	}
	// 8: the tenant row last; ON DELETE CASCADE clears context_tenant_scopes.
	if _, err := pool.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, tenantID); err != nil {
		return fmt.Errorf("store: delete tenant: %w", err)
	}
	return nil
}

// UpdateTenant patches status and/or display_name (empty string = leave
// unchanged, COALESCE(NULLIF(...))) and returns the updated row, or
// ErrTenantNotFound. The 059 CHECK is the backstop for an invalid status (the
// handler validates the status domain first → 400). Status='suspended' takes
// effect at the next auth via the 060 ctx_auth status gate; the per-turn cut for
// already-running sessions is E6 (T05c), not this function.
func UpdateTenant(ctx context.Context, pool *pgxpool.Pool, id, status, displayName string) (*Tenant, error) {
	if id == "" {
		return nil, ErrTenantNotFound
	}
	t, err := scanTenant(pool.QueryRow(ctx,
		`UPDATE context_tenants
		    SET status       = COALESCE(NULLIF($2,''), status),
		        display_name = COALESCE(NULLIF($3,''), display_name),
		        updated_at   = now()
		  WHERE id = $1::uuid
		 RETURNING `+tenantCols, id, status, displayName))
	if err != nil {
		if tenantNotFound(err) {
			return nil, ErrTenantNotFound
		}
		return nil, fmt.Errorf("store: update tenant: %w", err)
	}
	return t, nil
}
