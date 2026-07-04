// Package events implements PG LISTEN/NOTIFY for event-driven guard and digest.
// Uses pgxlisten for auto-reconnect and backlog handling.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/util"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgxlisten"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// channelBlockWrite is the PG NOTIFY channel fired by the trg_block_write trigger.
	channelBlockWrite = "ctx_block_write"

	// channelSettingsWrite is fired by the 051 notify triggers on
	// context_settings AND context_secrets (payload: entity/key/op — never
	// values). It is what makes psql direct edits and break-glass resets hot:
	// API writes reload twice (handler + NOTIFY), which is idempotent and
	// cheap by design (§2.1).
	channelSettingsWrite = "ctx_settings_write"

	// defaultReconnectDelay is the delay before reconnecting after a connection loss.
	defaultReconnectDelay = 5 * time.Second
)

// WriteHandler processes ctx_block_write notifications and signals the scheduler.
type WriteHandler struct {
	scheduler *Scheduler
}

// HandleNotification is called by pgxlisten for each NOTIFY on ctx_block_write.
func (h *WriteHandler) HandleNotification(ctx context.Context, notification *pgconn.Notification, conn *pgx.Conn) error {
	// Rune-aware payload truncation for the debug log (Issue #4 defensive —
	// invalid UTF-8 in slog output isn't a crash today but is a latent issue
	// if the log sink ever parses strict UTF-8).
	payload := util.TruncateRunesWithSuffix(notification.Payload, "...", 200)
	slog.Debug("listener: received notification",
		"channel", notification.Channel,
		"payload", payload,
	)
	h.scheduler.NotifyWrite()
	return nil
}

// HandleBacklog processes any writes that occurred while the listener was disconnected.
// On reconnect, we unconditionally signal guard+digest to pick up missed events.
func (h *WriteHandler) HandleBacklog(ctx context.Context, channel string, conn *pgx.Conn) error {
	slog.Info("listener: processing backlog, signaling guard+digest")
	h.scheduler.NotifyWrite()
	return nil
}

// SettingsWriteHandler reloads the config snapshot on ctx_settings_write
// notifications (G16 hot-reload path for psql/break-glass edits; redundant
// but idempotent for API writes, whose handlers reload after commit). Since
// F3-P1 the 053 trigger fires the SAME channel for context_backends rows —
// the payload's entity field dispatches to the backend-pool reload instead.
type SettingsWriteHandler struct {
	pool        *pgxpool.Pool
	cfg         *config.Store
	backendPool *backends.Pool
	blocktypes  *blocktype.Registry
}

// NewSettingsWriteHandler creates the hot-reload notification handler.
// blocktypes may be nil (tests, pre-T3 wiring): the block-type entity branch
// is then inert and block_type writes fall through to the settings reload —
// harmless, just registry-invisible.
func NewSettingsWriteHandler(pool *pgxpool.Pool, cfg *config.Store, backendPool *backends.Pool, blocktypes *blocktype.Registry) *SettingsWriteHandler {
	return &SettingsWriteHandler{pool: pool, cfg: cfg, backendPool: backendPool, blocktypes: blocktypes}
}

// settingsNotifyPayload mirrors the notify_settings_write() trigger payload
// (identity + op + scope, never values). Scope was added in migration 065
// (MT T32, 03-W6): it lets the listener invalidate the per-tenant config cache
// SELECTIVELY instead of rebuilding every tenant generation on every write. An
// absent scope (a pre-065 payload — backward-compat) unmarshals to "" and
// routes to the full _global reload, the safe over-invalidating fallback.
type settingsNotifyPayload struct {
	Entity string `json:"entity"`
	Scope  string `json:"scope"`
}

// HandleNotification is called by pgxlisten for each NOTIFY on ctx_settings_write.
// The reload only READS context_settings/context_secrets/context_backends —
// it can never write to them, so no notify loop is possible (§6.5 review
// anchor; the backend-pool reload holds the same contract).
func (h *SettingsWriteHandler) HandleNotification(ctx context.Context, notification *pgconn.Notification, conn *pgx.Conn) error {
	slog.Info("listener: settings write — reloading snapshot",
		"payload", util.TruncateRunesWithSuffix(notification.Payload, "...", 200))

	var p settingsNotifyPayload
	_ = json.Unmarshal([]byte(notification.Payload), &p)
	if p.Entity == "context_backends" {
		if h.backendPool == nil {
			return nil
		}
		if err := h.backendPool.Reload(ctx); err != nil {
			slog.Warn("listener: backend pool reload failed — previous snapshot stays active", "error", err)
		}
		return nil
	}
	// Block-type registry entity branch (WF T3, design 01 §4.3): the 072
	// notify trigger rides the same channel; without this branch the write
	// would fall through to settings.Reload — wirkungslos for the registry
	// (the T3 negative probe pins exactly that). A tenant-scope row (tier 2+)
	// drops only that tenant's generation; a _global / reserved / absent
	// scope swaps the base generation (+ tenant-cache wipe, once tier 2
	// exists). A successful reload also clears a boot-degraded state — the
	// psql row-fix heal path without restart.
	if p.Entity == "context_block_types" {
		if h.blocktypes == nil {
			return nil
		}
		if scope := p.Scope; scope != "" && !strings.HasPrefix(scope, "_") {
			h.blocktypes.InvalidateTenant(scope)
			return nil
		}
		if err := h.blocktypes.Reload(ctx, h.pool); err != nil {
			slog.Warn("listener: blocktype registry reload failed — previous snapshot stays active", "error", err)
		}
		return nil
	}
	// Scope-carried lazy invalidation (MT T32, 03-W6). A tenant-scope settings
	// or secrets write does NOT change the _global base generation —
	// settings.Reload reads scope='_global' exclusively (reload.go) — so a full
	// Reload would needlessly rebuild the base AND Replace-wipe EVERY tenant
	// generation. Instead drop only that tenant's cached generation; it rebuilds
	// lazily on the tenant's next request (0 synchronous builds on this single-
	// conn listener thread, design 03 §6.3). A _global / reserved / absent scope
	// falls through to the full Reload, which rebuilds the base and Replace-wipes
	// all tenant generations (also O(1) — they all derive from the base). The
	// guard mirrors the Store's own fail-safe (config/store.go: a scope earns a
	// tenant generation iff non-empty and not "_"-prefixed), so a pre-065 payload
	// (no scope field → "") routes to the safe full reload. The 063 quota trigger
	// rides this same channel with a tenant scope; dropping that tenant's config
	// generation is harmless (quota is cached separately, not in the snapshot).
	if scope := p.Scope; scope != "" && !strings.HasPrefix(scope, "_") {
		h.cfg.InvalidateTenant(scope)
		return nil
	}
	if err := settings.Reload(ctx, h.pool, h.cfg); err != nil {
		// Reload already logged the cause; the previous snapshot stays active.
		// Never propagate: pgxlisten treats handler errors as connection-level.
		slog.Warn("listener: settings reload failed — previous snapshot stays active", "error", err)
	}
	return nil
}

// HandleBacklog reloads unconditionally after a reconnect — a settings,
// backend or block-type write during the disconnect window would otherwise
// stay invisible until the next write or restart. Entity is unknown here:
// reload all three (listener.go:139-150 pattern, extended per design 01 §4.3).
func (h *SettingsWriteHandler) HandleBacklog(ctx context.Context, channel string, conn *pgx.Conn) error {
	slog.Info("listener: processing settings backlog, reloading snapshots")
	if err := settings.Reload(ctx, h.pool, h.cfg); err != nil {
		slog.Warn("listener: settings backlog reload failed — previous snapshot stays active", "error", err)
	}
	if h.backendPool != nil {
		if err := h.backendPool.Reload(ctx); err != nil {
			slog.Warn("listener: backend pool backlog reload failed — previous snapshot stays active", "error", err)
		}
	}
	if h.blocktypes != nil {
		if err := h.blocktypes.Reload(ctx, h.pool); err != nil {
			slog.Warn("listener: blocktype backlog reload failed — previous snapshot stays active", "error", err)
		}
	}
	return nil
}

// NewPgxlistenListener creates a pgxlisten.Listener configured for the context store.
// Uses a dedicated pgx.Conn (NOT from pool) as required by pgxlisten.
// pool/cfg feed the settings hot-reload handler; both come from the scheduler
// that owns this listener.
func NewPgxlistenListener(dsn string, reconnectDelay time.Duration, scheduler *Scheduler, pool *pgxpool.Pool, cfg *config.Store, backendPool *backends.Pool, blocktypes *blocktype.Registry, projectHub *ProjectHub) *pgxlisten.Listener {
	if reconnectDelay == 0 {
		reconnectDelay = defaultReconnectDelay
	}

	listener := &pgxlisten.Listener{
		Connect: func(ctx context.Context) (*pgx.Conn, error) {
			return pgx.Connect(ctx, dsn)
		},
		LogError: func(ctx context.Context, err error) {
			slog.Error("listener: pgxlisten error", "error", err)
		},
		ReconnectDelay: reconnectDelay,
	}

	handler := &WriteHandler{scheduler: scheduler}
	listener.Handle(channelBlockWrite, handler)
	listener.Handle(channelSettingsWrite, &SettingsWriteHandler{pool: pool, cfg: cfg, backendPool: backendPool, blocktypes: blocktypes})
	// W9: forward ctx_project_write (081) to the SSE domain-event hub. Registered
	// ONLY when the hub is wired — a nil hub leaves the channel un-LISTENed, so
	// the 081 notifies are Postgres no-ops (the listener-discard invariant also
	// holds intra-process, not just for an old binary).
	if projectHub != nil {
		listener.Handle(channelProjectWrite, &ProjectNotifyHandler{hub: projectHub})
	}

	return listener
}
