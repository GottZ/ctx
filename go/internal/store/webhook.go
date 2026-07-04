// DB layer for context_webhook_events (migration 082, workflow W13). The table
// is a DURABLE debounce-able TRIGGER queue: the inbound HTTP handler INSERTs a
// signature-verified delivery (redelivery-idempotent), the scheduler inbox arm
// drains pending rows and fires ONE forge sync per project (§5.3 — events are
// triggers, never authority), and the Janitor arm evicts processed rows past the
// retention horizon index-gestützt (idx_webhook_done).
//
// The per-project WEBHOOK SECRET lives in context_secrets in the PROJECT scope
// under the SERVER-FIXED name 'webhook.github.<project_id>' (§5.4/§5.6). This
// file owns that name (WebhookSecretName) and the register-column setters so the
// handler and the prune-drain share one source of truth.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// webhookSecretPrefix names the per-project sealed HMAC secret in the PROJECT
// scope (§5.4 naming convention 'webhook.github.<project_id>'). Kept in lockstep
// with forge.tokenSecretPrefix ('forge.token.<id>') — both are project-scoped,
// project-id-keyed credentials drained on project-delete + tenant-prune.
const webhookSecretPrefix = "webhook.github." //nolint:gosec // G101 false positive: a secret NAME prefix, not a credential value (mirrors forge.tokenSecretPrefix)

// WebhookSecretName returns the server-fixed context_secrets name for a project's
// webhook HMAC secret. NOT caller-choosable (webhook_secret_ref is server-managed,
// §3.1/§4.2) — a free name would turn the public endpoint into an HMAC-verification
// oracle over foreign secrets (§5.3).
func WebhookSecretName(projectID string) string { return webhookSecretPrefix + projectID }

// WebhookEventRef is the id+project pair the inbox drain picks (payload not
// needed to fire a sync trigger — the translator refetches the forge IST-state).
type WebhookEventRef struct {
	ID        string
	ProjectID string
}

// InsertWebhookEvent appends one signature-verified delivery. Redelivery-
// idempotent: ON CONFLICT (project_id, delivery_id) DO NOTHING, so GitHub's own
// redeliveries (identical GUID) collapse to exactly one row. inserted=false means
// the delivery was already stored (a duplicate) — the caller still answers 202.
// This is NOT called until AFTER the HMAC verify + rate-limit gates pass (§5.3
// order Body-Cap → Lookup → HMAC → Rate-Limit → INSERT).
func InsertWebhookEvent(ctx context.Context, pool *pgxpool.Pool, projectID, deliveryID, event string, payload json.RawMessage) (bool, error) {
	if projectID == "" || deliveryID == "" {
		return false, fmt.Errorf("webhook: project_id and delivery_id are required")
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO context_webhook_events (project_id, delivery_id, event, payload)
		 VALUES ($1::uuid, $2, $3, $4::jsonb)
		 ON CONFLICT (project_id, delivery_id) DO NOTHING
		 RETURNING id`,
		projectID, deliveryID, event, string(payload)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // redelivery: row already present, no new insert
	}
	if err != nil {
		return false, fmt.Errorf("webhook: insert event: %w", err)
	}
	return true, nil
}

// CountWebhookEventsSince counts a project's inbound deliveries in the trailing
// window — the substrate for webhook.rate_limit (§4.4, "120/min pro Projekt").
// Rides idx_webhook_project_recent. Because the INSERT happens only AFTER a valid
// HMAC (§5.3 order), this counts ONLY signature-valid deliveries — an unsigned
// flood never reaches the INSERT and so can never fill the budget.
func CountWebhookEventsSince(ctx context.Context, pool *pgxpool.Pool, projectID string, window time.Duration) (int, error) {
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_webhook_events
		  WHERE project_id = $1::uuid
		    AND received_at > now() - make_interval(secs => $2::double precision)`,
		projectID, window.Seconds()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("webhook: count recent: %w", err)
	}
	return n, nil
}

// DrainPendingWebhookEvents picks up to limit pending deliveries with FOR UPDATE
// SKIP LOCKED (the embed-backfill concurrency pattern), marks them processed in
// the SAME transaction, and returns the DISTINCT project ids that had at least
// one pending event. That distinct set IS the debounce: N events of one project
// in a batch collapse to ONE returned project id ⇒ one sync trigger (§4.4). The
// events are marked done before the trigger fires (they are non-authoritative
// triggers, §5.3; a lost trigger is re-driven by the next delivery or the
// periodic sync, and syncs are idempotent), so the row locks are released
// promptly rather than held across StartSync's goroutine launch.
func DrainPendingWebhookEvents(ctx context.Context, pool *pgxpool.Pool, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("webhook: drain begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, project_id::text FROM context_webhook_events
		  WHERE processed_at IS NULL
		  ORDER BY received_at
		  LIMIT $1
		  FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("webhook: drain pick: %w", err)
	}
	var ids []string
	seen := map[string]struct{}{}
	var projects []string
	for rows.Next() {
		var ref WebhookEventRef
		if err := rows.Scan(&ref.ID, &ref.ProjectID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("webhook: drain scan: %w", err)
		}
		ids = append(ids, ref.ID)
		if _, ok := seen[ref.ProjectID]; !ok {
			seen[ref.ProjectID] = struct{}{}
			projects = append(projects, ref.ProjectID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhook: drain rows: %w", err)
	}
	if len(ids) == 0 {
		return nil, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE context_webhook_events
		    SET processed_at = now(), status = 'done'
		  WHERE id = ANY($1::uuid[])`, ids); err != nil {
		return nil, fmt.Errorf("webhook: drain mark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("webhook: drain commit: %w", err)
	}
	return projects, nil
}

// EvictWebhookEvents deletes processed deliveries older than retention (the
// Janitor arm; Config webhook.retention, default 14d). retention<=0 is a no-op
// (kept forever — operator opt-out, same convention as llmlog/webchat retention).
// The DELETE is driven by idx_webhook_done (received_at partial WHERE processed_at
// IS NOT NULL) — never a Seq-Scan. Returns the number of rows evicted.
func EvictWebhookEvents(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_webhook_events
		  WHERE processed_at IS NOT NULL
		    AND received_at < now() - make_interval(secs => $1::double precision)`, retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("webhook: evict: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// SetProjectWebhookSecretRef records the NAME of the sealed webhook secret on the
// register row (the plaintext never touches context_projects). Called by the
// webhook-secret lifecycle endpoint in the SAME tx as the PutSecret seal, so the
// column and the secret row are consistent by construction (mirrors
// SetProjectToken for the forge PAT).
func SetProjectWebhookSecretRef(ctx context.Context, tx pgx.Tx, projectID, secretName string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE context_projects SET webhook_secret_ref = $2 WHERE id = $1::uuid`,
		projectID, secretName)
	if err != nil {
		return fmt.Errorf("webhook: set secret ref: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("webhook: set secret ref: project %s vanished", projectID)
	}
	return nil
}

// ClearProjectWebhookSecretRef nulls the register column (webhook deactivated).
// Called in the SAME tx as the DeleteSecret so the column and the secret row drop
// together — no dangling ref, no orphan secret.
func ClearProjectWebhookSecretRef(ctx context.Context, tx pgx.Tx, projectID string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE context_projects SET webhook_secret_ref = NULL WHERE id = $1::uuid`,
		projectID); err != nil {
		return fmt.Errorf("webhook: clear secret ref: %w", err)
	}
	return nil
}
