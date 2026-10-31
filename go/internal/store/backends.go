package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
		if pgxdb.UniqueViolation(err) {
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
		if pgxdb.UniqueViolation(err) {
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

// BackendSecretScopes lists the secret scopes a pool row of rowScope and
// base_url host resolves its api_key_ref ref in, in resolver order: a _global
// row reads _global only; a tenant row reads its own scope first and — only
// behind the tenant's allow_shared_secrets opt-in (TenantAllowsSharedSecrets,
// fail-closed) AND only when a _global row carrying the same api_key_ref points
// at the same origin (GlobalBackendSharesOrigin) — _global.
// settings.BackendSecretResolver walks this list, and the
// backend-create/-update gate rejects a ref that exists in none of these
// scopes, so write-time acceptance and reload-time resolution can never
// disagree (GHSA 2026-10-31 §1: a tenant row naming an operator secret).
//
// The host binding (§B): the opt-in shares the operator's key with the
// operator's provider, not with a host the tenant names. A tenant-admin picks
// its row's base_url freely (any publicly routable host), so a name-only
// fallback would let it aim the operator's provider key at a host of its
// choosing. Bound to the origin of an operator row that already sends that key,
// the opt-in means what design 03 §9.3 meant by it — a tenant row with its own
// model_map, priority, limits and trust on the operator's provider, under the
// operator's key. An operator moving its row to another host, or deleting it,
// leaves the tenant row keyless at the next reload instead of sending the key
// along to the old host.
func BackendSecretScopes(ctx context.Context, pool *pgxpool.Pool, rowScope, ref, host string) ([]string, error) {
	if rowScope == "" || rowScope == GlobalScope {
		return []string{GlobalScope}, nil
	}
	allowShared, err := TenantAllowsSharedSecrets(ctx, pool, rowScope)
	if err != nil {
		return nil, err
	}
	if !allowShared {
		return []string{rowScope}, nil
	}
	bound, err := GlobalBackendSharesOrigin(ctx, pool, ref, host)
	if err != nil {
		return nil, err
	}
	if bound {
		return []string{rowScope, GlobalScope}, nil
	}
	return []string{rowScope}, nil
}

// GlobalBackendSharesOrigin reports whether a _global pool row whose
// api_key_ref is ref points at the same origin as host — scheme and host
// lowercased, the default port made explicit, path dropped
// (dispatch.NormalizeOrigin, the admission layer's notion of one physical
// target). Both sides go through the same normalization; a host that does not
// parse matches nothing (fail-closed). The row's enabled flag does not matter:
// the binding is to where the operator sends its key, not to whether that row
// serves right now.
func GlobalBackendSharesOrigin(ctx context.Context, pool *pgxpool.Pool, ref, host string) (bool, error) {
	if ref == "" {
		return false, nil
	}
	want, err := dispatch.NormalizeOrigin(host)
	if err != nil {
		return false, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT base_url FROM context_backends WHERE scope = $1 AND api_key_ref = $2`,
		GlobalScope, ref)
	if err != nil {
		return false, fmt.Errorf("store: shared-secret origin lookup: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var baseURL string
		if err := rows.Scan(&baseURL); err != nil {
			return false, fmt.Errorf("store: shared-secret origin lookup: %w", err)
		}
		if got, err := dispatch.NormalizeOrigin(baseURL); err == nil && got == want {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: shared-secret origin lookup: %w", err)
	}
	return false, nil
}

// BackendSecretRefsInScopes lists every pool row in one of scopes whose
// api_key_ref is set — the pool half of the sealbox delete guard's reference
// union (04-W5 §3.3). Since F3 made the pool the living home of provider
// credentials, a secret referenced ONLY by a pool row was deletable: the guard
// scanned context_settings alone, so DELETE answered 200, deleteSealed removed
// the plaintext and the backend went keyless at the next resolver pass — the
// fail-open class the guard exists to prevent (§5.7).
//
// The scan follows the resolver (BackendSecretScopes, inverted): a tenant
// secret is bound by that tenant's own rows, so a tenant-admin's DELETE scans
// its own scope; a _global secret is bound by _global rows and by the rows of
// opt-in tenants (their fallback), so the operator's DELETE scans _global plus
// OptInTenantScopes. A non-opt-in tenant row naming a _global secret no longer
// binds it (it stays keyless by the resolver rule) and is deliberately not
// counted — counting it would 409 the operator on a reference that resolves
// nothing. The scoped scan is also what keeps foreign row names out of a
// tenant's referenced_by (§5.5).
//
// Fail-closed: an empty scope list returns no rows WITHOUT touching the DB —
// `scope = ANY('{}')` would match nothing silently, which is the same answer,
// but the early return makes the contract explicit for the caller.
func BackendSecretRefsInScopes(ctx context.Context, pool *pgxpool.Pool, scopes []string) ([]BackendSecretRef, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT name, api_key_ref FROM context_backends
		  WHERE api_key_ref IS NOT NULL AND btrim(api_key_ref) <> ''
		    AND scope = ANY($1::text[])
		  ORDER BY name`, scopes)
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
