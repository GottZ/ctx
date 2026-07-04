// Webhook inbox processing + retention (workflow W13, design/03-workflow-api-cli.md
// §3.4/§4.4/§5.3). Two scheduler arms over context_webhook_events:
//
//   - runWebhookInbox (own ticker): drains pending deliveries with FOR UPDATE SKIP
//     LOCKED, DEBOUNCES them to one forge sync per project, and fires the sync
//     TRIGGER — never a payload upsert (§5.3 "Events sind Sync-TRIGGER, nie
//     Autorität"; the translator applies the 3-way hash / cursor discard). The
//     HTTP annahme is decoupled from processing so the GitHub 10-s timeout never
//     waits on LLM/embed work.
//   - runWebhookRetention (shares the embed-cache janitor tick): evicts processed
//     rows past webhook.retention index-gestützt (idx_webhook_done) — the queue
//     is a through-buffer, not an archive.
//
// The sync trigger is a CALLBACK (WebhookSyncTrigger), not a direct forge
// dependency, so this package stays free of an import cycle and the debounce gate
// tests it with a counting fake (no forge, no live engine).
package events

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// webhookInboxInterval is the inbox drain cadence. The window between ticks IS
	// the debounce window: N deliveries of one project accumulated between drains
	// collapse to ONE sync trigger (§4.4). Short enough for prompt sync, long
	// enough that a 10k-import webhook storm coalesces instead of firing a sync per
	// delivery. Empty drains are one cheap partial-index query (idx_webhook_pending).
	webhookInboxInterval = 3 * time.Second
	// webhookDrainBatch bounds one drain's row count (FOR UPDATE SKIP LOCKED page).
	webhookDrainBatch = 500
)

// WebhookSyncTrigger fires a forge sync for one project (bound to
// forge.SyncManager.StartSync in server.go, status discarded). A benign refusal
// (already running / concurrency-saturated) is returned as an error and logged,
// NOT fatal — the delivery is a non-authoritative trigger already marked done;
// the next delivery or the periodic sync re-drives it, and syncs are idempotent.
type WebhookSyncTrigger func(ctx context.Context, project store.ProjectRow) error

// SetWebhookSyncTrigger installs the inbox→sync callback. Unlike SetProjectHub/
// SetBlocktypeRegistry it is MUTEX-guarded (not boot happens-before): the forge
// SyncManager it binds to is built inside NewRouter, which runs AFTER go
// scheduler.Run (main.go), so this is set with the listener already live. The
// inbox arm is inert (nil trigger → no drain) until it is wired — and no webhook
// deliveries exist at boot — so the brief window is a no-op.
func (s *Scheduler) SetWebhookSyncTrigger(fn WebhookSyncTrigger) {
	s.mu.Lock()
	s.webhookSync = fn
	s.mu.Unlock()
}

// runWebhookInbox drains one page of pending deliveries and fires one sync per
// distinct project. Inert (no drain) until a trigger is wired.
func (s *Scheduler) runWebhookInbox(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in webhook inbox", "error", r, "stack", string(debug.Stack()))
		}
	}()
	s.mu.Lock()
	fn := s.webhookSync
	s.mu.Unlock()
	if fn == nil {
		return // no consumer wired → leave deliveries pending (do not lose the trigger)
	}
	triggered, err := DrainWebhookInbox(ctx, s.pool, webhookDrainBatch, fn)
	if err != nil {
		slog.Warn("scheduler: webhook inbox drain failed", "error", err)
		return
	}
	if triggered > 0 {
		slog.Info("scheduler: webhook inbox drained", "projects_synced", triggered)
	}
}

// DrainWebhookInbox drains up to batch pending deliveries (debounced to one sync
// per project by store.DrainPendingWebhookEvents) and fires trigger per DISTINCT
// project. Returns the number of projects a sync was triggered for. Standalone
// (not a method) so the debounce gate drives it with a counting fake + a real
// test DB, no Scheduler/forge wiring. A project that vanished between drain and
// trigger is skipped; a trigger error is logged, not returned (benign refusal,
// §5.3) — so one project's saturation never blocks another's sync.
func DrainWebhookInbox(ctx context.Context, pool *pgxpool.Pool, batch int, trigger WebhookSyncTrigger) (int, error) {
	projects, err := store.DrainPendingWebhookEvents(ctx, pool, batch)
	if err != nil {
		return 0, err
	}
	triggered := 0
	for _, pid := range projects {
		row, err := store.GetProjectByID(ctx, pool, pid)
		if err != nil {
			slog.Warn("scheduler: webhook inbox project load", "project", pid, "error", err)
			continue
		}
		if row == nil {
			continue // project deleted between drain and trigger (events already cascaded)
		}
		if err := trigger(ctx, *row); err != nil {
			slog.Debug("scheduler: webhook sync trigger refused", "project", pid, "error", err)
		}
		triggered++
	}
	return triggered, nil
}

// runWebhookRetention evicts processed webhook deliveries older than
// webhook.retention (the Janitor arm; shares the embed-cache tick). retention=0
// is a no-op (kept forever — operator opt-out). The DELETE rides idx_webhook_done
// (design/03 §3.4). global-only, like the llmlog body-NULLing janitor.
func (s *Scheduler) runWebhookRetention(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in webhook retention", "error", r, "stack", string(debug.Stack()))
		}
	}()
	hours := float64(s.cfg.Snapshot().Project.Webhook.Retention) //nolint:forbidigo // MT 06 background: webhook retention is a server-global janitor policy over a process-wide queue table, not tenant-scoped.
	ttl := time.Duration(hours * float64(time.Hour))
	evicted, err := store.EvictWebhookEvents(ctx, s.pool, ttl)
	if err != nil {
		slog.Warn("scheduler: webhook retention failed", "error", err)
		return
	}
	if evicted > 0 {
		slog.Info("scheduler: webhook events evicted", "rows", evicted, "retention_hours", hours)
	}
}
