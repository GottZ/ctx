package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isUniqueViolation reports a 23505 unique-constraint error (duplicate name).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Backend pool CRUD (F3-P1). Write paths run inside a caller transaction
// with setTxActor, mirroring the settings store: the 053 audit trigger reads
// ctx.api_key_id, so audit attribution can never desync from the mutation.
// Plaintext keys NEVER travel through here — api_key_ref names an F2 secret,
// the resolved value exists only in the pool's in-memory snapshot.

// backendWriteCols materializes the Backend fields into SQL parameters.
// nil maps become empty objects (the columns are NOT NULL DEFAULT '{}' and
// an explicit NULL would bypass the default).
func backendWriteArgs(b *backends.Backend) []any {
	roles := b.Roles
	if roles == nil {
		roles = []string{}
	}
	modelMap := b.ModelMap
	if modelMap == nil {
		modelMap = map[string]backends.ModelSpec{}
	}
	timeouts := b.Timeouts
	if timeouts == nil {
		timeouts = map[string]int{}
	}
	extraHeaders := b.ExtraHeaders
	if extraHeaders == nil {
		extraHeaders = map[string]string{}
	}
	extraBody := b.ExtraBody
	if extraBody == nil {
		extraBody = map[string]any{}
	}
	limits := b.Limits
	if limits == nil {
		limits = map[string]any{}
	}
	metadata := b.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	var apiKeyRef *string
	if b.APIKeyRef != "" {
		apiKeyRef = &b.APIKeyRef
	}
	var numCtx *int
	if b.NumCtx != 0 {
		numCtx = &b.NumCtx
	}
	// scope is the tenant dimension (062, Modell C). An empty Scope (a
	// pre-062 / test-seeded zero value) normalizes to the shared sentinel so a
	// write never persists "" — the DB column is NOT NULL DEFAULT '_global',
	// and visibleTo treats "" as shared, but the row should carry the explicit
	// sentinel, not a blank that depends on the read-side equivalence.
	scope := b.Scope
	if scope == "" {
		scope = backends.GlobalScope
	}
	return []any{
		b.Name, b.Host, string(b.Protocol), b.ProviderClass, apiKeyRef,
		string(b.Trust), b.Locality, roles, modelMap, timeouts, numCtx,
		b.Priority, b.Enabled, extraHeaders, extraBody, limits, metadata, scope,
	}
}

// CreateBackend inserts one pool row and returns its id. The unique name
// constraint surfaces as a caller-ready error.
func CreateBackend(ctx context.Context, tx pgx.Tx, b *backends.Backend, by *string) (string, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return "", err
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO context_backends
		    (name, base_url, protocol, provider_class, api_key_ref, trust,
		     locality, roles, model_map, timeouts, num_ctx, priority, enabled,
		     extra_headers, extra_body, limits, metadata, scope)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		RETURNING id`, backendWriteArgs(b)...).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("backend name %q already exists", b.Name)
		}
		return "", fmt.Errorf("store: create backend: %w", err)
	}
	return id, nil
}

// UpdateBackend writes the full row state addressed by b.ID. Returns false
// when the row vanished between snapshot read and write OR when scopes gates it
// out (MT T37, 04-W5 §4.6/§5.5). scopes is the caller's permitted tenant set:
// a tenant-admin passes []string{ar.HomeScope}, a server-admin passes nil
// (no scope filter — authority over every tenant). The scope predicate fires
// fail-closed IN THE STATEMENT, not as a fetch-then-write: a foreign/_global
// row simply matches zero rows → found=false → the handler answers 404 (no
// existence oracle, no TOCTOU race). This is the last line before the row is
// touched — a second call path (CLI, migration tool) that skips the handler
// check still cannot cross the tenant boundary (doctrine: api_keys.go:44).
func UpdateBackend(ctx context.Context, tx pgx.Tx, b *backends.Backend, by *string, scopes []string) (bool, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return false, err
	}
	args := append(backendWriteArgs(b), b.ID, scopes)
	tag, err := tx.Exec(ctx, `
		UPDATE context_backends SET
		    name=$1, base_url=$2, protocol=$3, provider_class=$4,
		    api_key_ref=$5, trust=$6, locality=$7, roles=$8, model_map=$9,
		    timeouts=$10, num_ctx=$11, priority=$12, enabled=$13,
		    extra_headers=$14, extra_body=$15, limits=$16, metadata=$17,
		    scope=$18, updated_at=now()
		WHERE id=$19 AND ($20::text[] IS NULL OR scope = ANY($20))`, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return false, fmt.Errorf("backend name %q already exists", b.Name)
		}
		return false, fmt.Errorf("store: update backend: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// BackendPriority is the reorder working set: the minimal projection the
// backend-reorder handler needs to verify the client's snapshot and rewrite
// the priority ladder (never the full row — reorder must not round-trip
// unrelated fields through a possibly stale client copy).
type BackendPriority struct {
	ID       string
	Name     string
	Priority int
}

// LockBackendPriorities locks (FOR UPDATE) EVERY backend row the caller may
// write — scopes as in UpdateBackend (nil = server-admin, no filter;
// []string{ar.HomeScope} = tenant-admin, MT T37) — and returns id/name/priority
// in Chain order (priority DESC, name ASC). The lock deliberately spans the
// WHOLE writable set, not just the payload ids: backend-reorder rewrites the
// complete ladder, so a concurrent reorder/update must serialize behind it and
// the staleness check (409) reads a stable snapshot, not a moving one.
func LockBackendPriorities(ctx context.Context, tx pgx.Tx, by *string, scopes []string) ([]BackendPriority, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id, name, priority FROM context_backends
		 WHERE ($1::text[] IS NULL OR scope = ANY($1))
		 ORDER BY priority DESC, name ASC
		 FOR UPDATE`, scopes)
	if err != nil {
		return nil, fmt.Errorf("store: lock backend priorities: %w", err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[BackendPriority])
	if err != nil {
		return nil, fmt.Errorf("store: lock backend priorities: %w", err)
	}
	return out, nil
}

// UpdateBackendPriority writes ONLY the priority column of one row. No scope
// predicate here: the caller addresses exclusively ids it verified against
// LockBackendPriorities in the SAME tx (the scope gate + row lock already
// fired there); a vanished row is impossible under that lock, so zero affected
// rows is a hard invariant break, not a 404.
func UpdateBackendPriority(ctx context.Context, tx pgx.Tx, id string, priority int) error {
	tag, err := tx.Exec(ctx,
		`UPDATE context_backends SET priority=$2, updated_at=now() WHERE id=$1`,
		id, priority)
	if err != nil {
		return fmt.Errorf("store: update backend priority: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: backend %s vanished during reorder despite row lock", id)
	}
	return nil
}

// BackendSecretRef is one context_backends row's pointer at an F2 secret: the
// row NAME (what an operator edits) and the api_key_ref it carries. Never a
// resolved value — reference counting reads ROWS, not the pool snapshot (same
// doctrine as the settings scan: the snapshot holds resolved plaintext and is
// useless for counting references, and it may lag the DB).
type BackendSecretRef struct {
	Name      string
	APIKeyRef string
}

// BackendSecretRefsMulti lists the pool rows in scopes whose api_key_ref is
// set — the pool half of the sealbox delete guard's reference union (04-W5
// §3.3). Since F3 made the pool the living home of provider credentials, a
// secret referenced ONLY by a pool row was deletable: the guard scanned
// context_settings alone, so DELETE answered 200, deleteSealed removed the
// plaintext and the backend went keyless at the next resolver pass — the
// fail-open class the guard exists to prevent (§5.7).
//
// scopes is the caller's VISIBLE scope set, identical to the settings scan
// (writeScope plus opt-in tenant scopes): scanning wider would let a
// tenant-admin enumerate foreign provider topology through referenced_by
// (§5.5).
//
// Fail-closed exactly like LoadSettingOverridesMulti: an empty slice or an
// empty element is an error, never a scan that silently matches nothing.
func BackendSecretRefsMulti(ctx context.Context, pool *pgxpool.Pool, scopes []string) ([]BackendSecretRef, error) {
	if len(scopes) == 0 {
		return nil, fmt.Errorf("backends: at least one scope is required")
	}
	for _, s := range scopes {
		if s == "" {
			return nil, fmt.Errorf("backends: empty scope is not allowed")
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT name, api_key_ref FROM context_backends
		  WHERE scope = ANY($1::text[])
		    AND api_key_ref IS NOT NULL AND btrim(api_key_ref) <> ''
		  ORDER BY name`,
		scopes)
	if err != nil {
		return nil, fmt.Errorf("backends: load secret refs: %w", err)
	}
	defer rows.Close()

	var refs []BackendSecretRef
	for rows.Next() {
		var ref BackendSecretRef
		if err := rows.Scan(&ref.Name, &ref.APIKeyRef); err != nil {
			return nil, fmt.Errorf("backends: scan secret ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// DeleteBackend removes one row by id (hard delete — llmlog references by
// backend_name TEXT, deliberately no FK, history stays readable). Returns
// the deleted name for the response. scopes is the caller's permitted tenant
// set (nil = server-admin, no filter; []string{ar.HomeScope} = tenant-admin),
// gating the delete fail-closed in the statement exactly like UpdateBackend
// (MT T37, 04-W5 §4.6/§5.5): a foreign/_global id deletes zero rows →
// found=false → 404, no existence oracle, no TOCTOU.
func DeleteBackend(ctx context.Context, tx pgx.Tx, id string, by *string, scopes []string) (string, bool, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return "", false, err
	}
	var name string
	err := tx.QueryRow(ctx,
		`DELETE FROM context_backends
		 WHERE id=$1 AND ($2::text[] IS NULL OR scope = ANY($2))
		 RETURNING name`, id, scopes).Scan(&name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: delete backend: %w", err)
	}
	return name, true, nil
}
