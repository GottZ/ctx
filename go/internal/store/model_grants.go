package store

// Model grants — the single model releases of a model consumer (design/01
// §3.8, masterplan K11; migration 164): one row per (consumer, model alias
// id). all_models stays the wildcard beside them.
//
// The rules these primitives keep:
//
//   - A grant binds the alias ID. Deleting the alias removes it (ON DELETE
//     CASCADE); an alias created later under the same name is not granted
//     (B28). Which ids a consumer may be granted — the aliases its tenant
//     scope is offered — is the admin action's check (modelalias.Offers);
//     the foreign key is the backstop for an alias deleted since.
//   - Writes are a target set: SetConsumerGrants turns it into the diff, so
//     the audit trail (migration 164: one grant / ungrant row per changed
//     grant) records changes, never a rewrite of the unchanged ones.
//   - Reads serve the request path of /v1 (W02-05): ConsumerGrantsAlias is
//     one primary-key probe per inference request, ConsumerGrantIDs one index
//     range for the model list; the consumer list carries the ids inside its
//     one statement (model_consumers_read.go).

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ErrModelGrantAliasGone: a grant names a model alias that no longer exists —
// deleted between the caller's check and the insert (SQLSTATE 23503 of
// fk_model_grants_alias). Nothing of the write is kept.
var ErrModelGrantAliasGone = errors.New("store: model grant: the model alias no longer exists")

// modelGrantAliasFK is the alias foreign key of migration 164; its 23503 —
// not the code alone, which the consumer key raises too — is
// ErrModelGrantAliasGone.
const modelGrantAliasFK = "fk_model_grants_alias"

// canonicalUUID parses s; ok=false names no row.
func canonicalUUID(s string) (string, bool) {
	u, err := uuid.Parse(s)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

// CanonicalAliasIDs returns ids as a set: each parsed as a UUID and written
// canonically, duplicates removed, sorted (the order ConsumerGrantIDs reads).
// A malformed id is an error naming it. The result is never nil.
func CanonicalAliasIDs(ids []string) ([]string, error) {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, s := range ids {
		id, ok := canonicalUUID(s)
		if !ok {
			return nil, fmt.Errorf("store: model grant: %q is not a model alias id", s)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// SetConsumerGrants makes the grants of consumerID exactly aliasIDs (a target
// set; nil and empty both mean none) inside tx — the transaction of the
// consumer write, which holds the consumer's row lock: grants absent from the
// set are deleted, new ones inserted, unchanged ones left alone (no audit
// row). stamp is applied first (stampAudit never clears what tx already
// carries). A malformed id is an error before anything is written; an alias
// that no longer exists is ErrModelGrantAliasGone — the caller then rolls tx
// back. The caller has checked that the consumer's scope is offered every
// added id.
func SetConsumerGrants(ctx context.Context, tx pgx.Tx, consumerID string, aliasIDs []string, stamp AuditStamp) error {
	ids, err := CanonicalAliasIDs(aliasIDs)
	if err != nil {
		return err
	}
	if err := stampAudit(ctx, tx, stamp); err != nil {
		return err
	}
	// ids is never nil: <> ALL('{}') holds for every row, <> ALL(NULL) for none.
	if _, err := tx.Exec(ctx,
		`DELETE FROM context_model_consumer_grants
		  WHERE consumer_id = $1::uuid AND alias_id <> ALL($2::uuid[])`, consumerID, ids); err != nil {
		return fmt.Errorf("model_grants: remove: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO context_model_consumer_grants (consumer_id, alias_id, created_by_key, created_by_principal)
		 SELECT $1::uuid, a, `+sqlActorKey+`, `+sqlActorPrincipal+` FROM unnest($2::uuid[]) AS a
		 ON CONFLICT (consumer_id, alias_id) DO NOTHING`, consumerID, ids); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == modelGrantAliasFK {
			return ErrModelGrantAliasGone
		}
		return fmt.Errorf("model_grants: add: %w", err)
	}
	return nil
}

// ConsumerGrantIDs lists the alias ids granted to consumerID, sorted — one
// range of the primary key. An unknown or malformed consumer has none. The
// result is never nil.
func ConsumerGrantIDs(ctx context.Context, q pgxdb.Querier, consumerID string) ([]string, error) {
	id, ok := canonicalUUID(consumerID)
	if !ok {
		return []string{}, nil
	}
	rows, err := q.Query(ctx,
		`SELECT alias_id::text FROM context_model_consumer_grants WHERE consumer_id = $1::uuid ORDER BY alias_id`, id)
	if err != nil {
		return nil, fmt.Errorf("model_grants: list: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("model_grants: list: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// ConsumerGrantsAlias reports whether aliasID is granted to consumerID: ONE
// statement, a probe of the primary key — the per-request check of /v1 next
// to all_models (design/01 §3.8). Malformed ids are not granted.
func ConsumerGrantsAlias(ctx context.Context, q pgxdb.Rower, consumerID, aliasID string) (bool, error) {
	c, okC := canonicalUUID(consumerID)
	a, okA := canonicalUUID(aliasID)
	if !okC || !okA {
		return false, nil
	}
	var ok bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM context_model_consumer_grants WHERE consumer_id = $1::uuid AND alias_id = $2::uuid)`,
		c, a).Scan(&ok); err != nil {
		return false, fmt.Errorf("model_grants: probe: %w", err)
	}
	return ok, nil
}

// CountActiveExplicitGrants counts the ACTIVE consumers holding an explicit
// grant on aliasID (idx_model_grants_alias) — the guard of model-alias-delete
// (design/05 V-02-b). all_models is not a grant: a wildcard consumer without
// a grant row is not counted, one with a grant row is. A malformed id counts 0.
func CountActiveExplicitGrants(ctx context.Context, q pgxdb.Rower, aliasID string) (int, error) {
	id, ok := canonicalUUID(aliasID)
	if !ok {
		return 0, nil
	}
	var n int
	if err := q.QueryRow(ctx,
		`SELECT count(*)::int FROM context_model_consumer_grants g
		   JOIN context_model_consumers c ON c.id = g.consumer_id
		  WHERE g.alias_id = $1::uuid AND c.active`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("model_grants: count holders: %w", err)
	}
	return n, nil
}
