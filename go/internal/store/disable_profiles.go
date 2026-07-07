package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Disable-profile registry CRUD (092, Web-UX U01-W3, design/01 §3/§4.3). A
// profile is a named, scope-filtered set of backends that a toggle takes out of
// every chain. Write paths run inside a caller transaction with setTxActor (the
// 092 audit trigger reads ctx.api_key_id, so audit attribution can never desync
// from the mutation) — mirroring the settings/backend stores.
//
// The scope predicate on update/delete/toggle is the fail-closed backstop (MT
// T37 doctrine, AM-5): scopes is the caller's permitted tenant set — nil for a
// server-admin (authority over every scope, no filter) and []string{HomeScope}
// for a tenant-admin (only its own scope's profiles). A foreign/_global profile
// then matches zero rows → found=false → the handler answers 404 (no oracle, no
// TOCTOU), exactly like UpdateBackend/DeleteBackend.

// DisableProfile is one context_disable_profiles row plus its member backend
// ids (from the join). MemberIDs is the authoritative membership — the handler
// resolves ids to backend metadata against the pool snapshot.
type DisableProfile struct {
	ID          string   `json:"-"`
	Scope       string   `json:"scope"`
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Active      bool     `json:"active"`
	Reserved    bool     `json:"reserved"`
	MemberIDs   []string `json:"-"`
}

const selectDisableProfileCols = `
	p.id, p.scope, p.name, p.label, p.description, p.active, p.reserved,
	COALESCE(array_agg(m.backend_id::text) FILTER (WHERE m.backend_id IS NOT NULL), '{}')`

func scanDisableProfile(row pgx.Row) (*DisableProfile, error) {
	var p DisableProfile
	if err := row.Scan(&p.ID, &p.Scope, &p.Name, &p.Label, &p.Description,
		&p.Active, &p.Reserved, &p.MemberIDs); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListDisableProfiles returns every profile + its member ids, ORDER BY
// scope,name (stable — the read is scope-filtered in the handler, not here: the
// list surface shows _global ∪ the caller's own scope, uniform with backend-list).
func ListDisableProfiles(ctx context.Context, pool *pgxpool.Pool) ([]DisableProfile, error) {
	rows, err := pool.Query(ctx, `
		SELECT`+selectDisableProfileCols+`
		  FROM context_disable_profiles p
		  LEFT JOIN context_disable_profile_backends m ON m.profile_id = p.id
		 GROUP BY p.id
		 ORDER BY p.scope, p.name`)
	if err != nil {
		return nil, fmt.Errorf("store: list disable profiles: %w", err)
	}
	defer rows.Close()
	var out []DisableProfile
	for rows.Next() {
		p, err := scanDisableProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan disable profile: %w", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list disable profiles: %w", err)
	}
	return out, nil
}

// GetDisableProfile reads one profile by (scope, name) + its member ids, or nil
// if none exists. No scope gate here — the caller (handler) applies visibility;
// the write functions carry the fail-closed scope predicate.
func GetDisableProfile(ctx context.Context, pool *pgxpool.Pool, scope, name string) (*DisableProfile, error) {
	p, err := scanDisableProfile(pool.QueryRow(ctx, `
		SELECT`+selectDisableProfileCols+`
		  FROM context_disable_profiles p
		  LEFT JOIN context_disable_profile_backends m ON m.profile_id = p.id
		 WHERE p.scope = $1 AND p.name = $2
		 GROUP BY p.id`, scope, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get disable profile: %w", err)
	}
	return p, nil
}

// CreateDisableProfile inserts one registry row (fail-closed active=false unless
// specified) and its memberships. A duplicate (scope, name) surfaces as a
// caller-ready error; a member id with no backend row hits the FK and returns
// an error (the handler already resolved names → ids against the pool snapshot,
// so this is the last-line integrity guard).
func CreateDisableProfile(ctx context.Context, tx pgx.Tx, p *DisableProfile, memberIDs []string, by *string) (string, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return "", err
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO context_disable_profiles (scope, name, label, description, active)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		p.Scope, p.Name, p.Label, p.Description, p.Active).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("disable profile %q already exists in scope %q", p.Name, p.Scope)
		}
		return "", fmt.Errorf("store: create disable profile: %w", err)
	}
	if err := insertProfileMembers(ctx, tx, id, memberIDs); err != nil {
		return "", err
	}
	return id, nil
}

// UpdateDisableProfile patches label/description (nil = keep) and, when
// memberIDs is non-nil, replaces the full membership set. The scope predicate
// gates it fail-closed (see file header). Returns found=false when the row
// vanished or the scope gate rejected it.
func UpdateDisableProfile(ctx context.Context, tx pgx.Tx, scope, name string, label, description *string, memberIDs *[]string, by *string, scopes []string) (bool, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return false, err
	}
	var id string
	err := tx.QueryRow(ctx, `
		UPDATE context_disable_profiles SET
		    label       = COALESCE($3, label),
		    description = COALESCE($4, description),
		    updated_at  = now()
		 WHERE scope = $1 AND name = $2 AND ($5::text[] IS NULL OR scope = ANY($5))
		 RETURNING id`, scope, name, label, description, scopes).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: update disable profile: %w", err)
	}
	if memberIDs != nil {
		if _, err := tx.Exec(ctx,
			`DELETE FROM context_disable_profile_backends WHERE profile_id = $1`, id); err != nil {
			return false, fmt.Errorf("store: clear profile members: %w", err)
		}
		if err := insertProfileMembers(ctx, tx, id, *memberIDs); err != nil {
			return false, err
		}
	}
	return true, nil
}

// SetDisableProfileActive flips the active flag (idempotent — an UPDATE to the
// same value is a no-op that still RETURNs the row so found stays true). The
// scope predicate gates it fail-closed. Used by the profile toggle AND, in the
// same transaction as the settings write, by the eject/gaming shim dual-write.
func SetDisableProfileActive(ctx context.Context, tx pgx.Tx, scope, name string, active bool, by *string, scopes []string) (bool, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE context_disable_profiles SET active = $3, updated_at = now()
		 WHERE scope = $1 AND name = $2 AND ($4::text[] IS NULL OR scope = ANY($4))`,
		scope, name, active, scopes)
	if err != nil {
		return false, fmt.Errorf("store: set disable profile active: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteDisableProfile removes one profile (its memberships cascade). It returns
// reserved=true WITHOUT deleting when the row is a break-glass profile (the
// eject alias hangs off it, §4.3) — the handler answers 422. found=false (scope
// gate reject or missing) → 404. The reserved check + delete run under one
// FOR UPDATE row lock so reserved cannot flip between read and delete.
func DeleteDisableProfile(ctx context.Context, tx pgx.Tx, scope, name string, by *string, scopes []string) (reserved bool, found bool, err error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return false, false, err
	}
	var isReserved bool
	err = tx.QueryRow(ctx, `
		SELECT reserved FROM context_disable_profiles
		 WHERE scope = $1 AND name = $2 AND ($3::text[] IS NULL OR scope = ANY($3))
		 FOR UPDATE`, scope, name, scopes).Scan(&isReserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("store: lock disable profile: %w", err)
	}
	if isReserved {
		return true, true, nil
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM context_disable_profiles
		 WHERE scope = $1 AND name = $2 AND ($3::text[] IS NULL OR scope = ANY($3))`,
		scope, name, scopes); err != nil {
		return false, true, fmt.Errorf("store: delete disable profile: %w", err)
	}
	return false, true, nil
}

// SyncBackendDisableProfiles reconciles the disable-profile memberships of ONE
// backend to exactly profileIDs (092, U01-W4): it PRUNEs the backend's join rows
// whose profile is not in the target set and INSERTs the missing ones (INSERT
// missing, DELETE surplus — per-backend atomic, minimal audit churn vs a full
// delete+reinsert). It runs inside the SAME caller Tx as the backend write, so
// the membership and the backend row commit or roll back together. An empty (or
// nil) profileIDs removes EVERY membership of the backend. The handler already
// resolved names→ids against the visible profile set, so the FK is the last-line
// integrity guard. by re-stamps the tx actor for the 092 join audit trigger
// (harmless when the preceding backend write already set it).
func SyncBackendDisableProfiles(ctx context.Context, tx pgx.Tx, backendID string, profileIDs []string, by *string) error {
	if err := setTxActor(ctx, tx, by); err != nil {
		return err
	}
	if len(profileIDs) == 0 {
		// nil/[] semantics: clear all memberships (a plain DELETE — a nil slice
		// would encode as SQL NULL and make `<> ALL(NULL)` prune nothing).
		if _, err := tx.Exec(ctx,
			`DELETE FROM context_disable_profile_backends WHERE backend_id = $1`, backendID); err != nil {
			return fmt.Errorf("store: clear backend profiles: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM context_disable_profile_backends
		 WHERE backend_id = $1 AND profile_id <> ALL($2::uuid[])`, backendID, profileIDs); err != nil {
		return fmt.Errorf("store: prune backend profiles: %w", err)
	}
	for _, pid := range profileIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO context_disable_profile_backends (profile_id, backend_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, pid, backendID); err != nil {
			return fmt.Errorf("store: add backend to profile %q: %w", pid, err)
		}
	}
	return nil
}

func insertProfileMembers(ctx context.Context, tx pgx.Tx, profileID string, memberIDs []string) error {
	for _, bid := range memberIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO context_disable_profile_backends (profile_id, backend_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, profileID, bid); err != nil {
			return fmt.Errorf("store: add profile member %q: %w", bid, err)
		}
	}
	return nil
}
