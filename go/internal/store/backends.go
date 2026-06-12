package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	return []any{
		b.Name, b.Host, string(b.Protocol), b.ProviderClass, apiKeyRef,
		string(b.Trust), b.Locality, roles, modelMap, timeouts, numCtx,
		b.Priority, b.Enabled, extraHeaders, extraBody, limits, metadata,
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
		     extra_headers, extra_body, limits, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
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
// when the row vanished between snapshot read and write.
func UpdateBackend(ctx context.Context, tx pgx.Tx, b *backends.Backend, by *string) (bool, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return false, err
	}
	args := append(backendWriteArgs(b), b.ID)
	tag, err := tx.Exec(ctx, `
		UPDATE context_backends SET
		    name=$1, base_url=$2, protocol=$3, provider_class=$4,
		    api_key_ref=$5, trust=$6, locality=$7, roles=$8, model_map=$9,
		    timeouts=$10, num_ctx=$11, priority=$12, enabled=$13,
		    extra_headers=$14, extra_body=$15, limits=$16, metadata=$17,
		    updated_at=now()
		WHERE id=$18`, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return false, fmt.Errorf("backend name %q already exists", b.Name)
		}
		return false, fmt.Errorf("store: update backend: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteBackend removes one row by id (hard delete — llmlog references by
// backend_name TEXT, deliberately no FK, history stays readable). Returns
// the deleted name for the response.
func DeleteBackend(ctx context.Context, tx pgx.Tx, id string, by *string) (string, bool, error) {
	if err := setTxActor(ctx, tx, by); err != nil {
		return "", false, err
	}
	var name string
	err := tx.QueryRow(ctx,
		`DELETE FROM context_backends WHERE id=$1 RETURNING name`, id).Scan(&name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: delete backend: %w", err)
	}
	return name, true, nil
}
