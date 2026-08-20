// Package events implements PG LISTEN/NOTIFY for event-driven guard and digest.
// Uses pgxlisten for auto-reconnect and backlog handling.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/util"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgxlisten"
)

const (
	// channelBlockWrite is the PG NOTIFY channel fired by the trg_block_write trigger.
	channelBlockWrite = "ctx_block_write"

	// channelLinkWrite is the graph-cache dirty signal (design/05 §4.3). Fed since
	// Migration 116 (W05.3) by the row-level triggers on both link tables, their
	// statement-level TRUNCATE triggers, and the column-filtered
	// is_archived/scope flip trigger on context_blocks — every cache-relevant
	// mutation, and nothing else (a dream_checked_at stamp stays off the wire).
	channelLinkWrite = "ctx_link_write"

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

// LinkWriteHandler marks the graph cache dirty on ctx_link_write notifications
// (design/05 §4.3). Payload is irrelevant — the consumer is a debounce that only
// needs "dirty", not row identity (a constant per-table payload, §3.1 Nr. 1).
type LinkWriteHandler struct {
	scheduler *Scheduler
}

// HandleNotification marks dirty on every ctx_link_write NOTIFY.
func (h *LinkWriteHandler) HandleNotification(ctx context.Context, notification *pgconn.Notification, conn *pgx.Conn) error {
	h.scheduler.NotifyLinkWrite()
	return nil
}

// HandleBacklog marks dirty once after a reconnect: a link write during the
// disconnect window would otherwise stay invisible until the hard interval
// (HandleBacklog pattern, design/05 §4.3 backlog semantics).
func (h *LinkWriteHandler) HandleBacklog(ctx context.Context, channel string, conn *pgx.Conn) error {
	h.scheduler.NotifyLinkWrite()
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
	// dispatcher receives the re-mapped dispatch.Settings after a settings
	// reload and the re-derived admission Policy after a backend-pool reload
	// (MW2 carrying the W1 construction leftover, design/01 §3.1 + N9). nil
	// (tests, pre-wire) leaves both refreshes inert.
	dispatcher *dispatch.Dispatcher
	// coupledPrev is the embed-cache-coupled set of the pool as of the last
	// SUCCESSFUL flush — or, until the first one, of the snapshot this handler
	// was constructed on (A04-W3, design/04 §4.2). It is only ever read and
	// written from the single pgxlisten handler goroutine.
	coupledPrev coupledSet
	// flush is embedcache.Flush behind a seam: the failure posture of §4.2(b)
	// (log-and-continue, but coupledPrev stays behind so the NEXT notification
	// retries) is only pinnable with an injectable error. nil = the real flush.
	flush func(ctx context.Context, pool *pgxpool.Pool) (int64, error)
}

// NewSettingsWriteHandler creates the hot-reload notification handler.
// blocktypes may be nil (tests, pre-T3 wiring): the block-type entity branch
// is then inert and block_type writes fall through to the settings reload —
// harmless, just registry-invisible.
//
// The coupled baseline is taken here, from the pool as it stands at
// construction. Boot publishes the first pool snapshot BEFORE the listener is
// built (cmd/ctxd/main.go), so the baseline is the live serving topology and a
// boot without an intervening edit diffs empty — W3 never flushes at boot. The
// remaining boot case (an edit made while ctxd was down) is covered since W4 by
// the persisted fingerprint (ReconcileCoupledFingerprint, coupled_fingerprint.go),
// which main.go runs against the same loaded pool before this handler exists.
func NewSettingsWriteHandler(pool *pgxpool.Pool, cfg *config.Store, backendPool *backends.Pool, blocktypes *blocktype.Registry) *SettingsWriteHandler {
	return &SettingsWriteHandler{
		pool: pool, cfg: cfg, backendPool: backendPool, blocktypes: blocktypes,
		coupledPrev: coupledSetOf(backendPool),
	}
}

// coupledPair is one connection identity of an embed-writing backend. Model is
// deliberately NOT part of it: context_embed_cache keys on (text_hash, model),
// so a model change addresses different rows anyway — the same reasoning the
// config-side EmbedCacheCoupledChanged has carried since G16
// (settings/reload.go). Host/protocol is what silently changes the vector SPACE
// under an unchanged model name.
type coupledPair struct {
	host     string
	protocol string
}

// coupledSet is the embed-cache-coupled fingerprint of a pool snapshot: the set
// of (host, protocol) pairs over all serving-eligible backends carrying an
// embed role (design/04 §3.2a).
type coupledSet map[coupledPair]struct{}

// coupledSetOf derives the coupled set from the pool's current snapshot.
//
// Serving-eligible means enabled AND not disabled by an ACTIVE profile — the
// same qualification the chain applies (backends.Pool.Chain), because that is
// what decides who WRITES the cache. Defining the set over the enabled column
// alone would miss the profile path entirely: profiles never touch that column
// (membership lives in disabledBy), yet disabling one backend hands serving to
// a failover that may answer from a different host under the same model name —
// exactly the cross-space case this diff exists for.
//
// Snapshot and DisabledBy are two atomic loads, so a pool reload racing between
// them can yield a mixed set. That is benign and self-correcting: every pool
// row and profile write rides the notify funnel, so the racing reload brings
// its own notification and this diff runs again on a settled snapshot. A phantom
// set can therefore only cause one extra flush (fail-closed direction), never a
// missed one.
func coupledSetOf(p *backends.Pool) coupledSet {
	if p == nil {
		return coupledSet{}
	}
	rows := p.Snapshot()
	disabledBy := p.DisabledBy()
	out := make(coupledSet, len(rows))
	for i := range rows {
		b := &rows[i]
		if !b.Enabled || disabledBy[b.ID] != "" {
			continue
		}
		if !b.HasRole(backends.RoleEmbed) && !b.HasRole(backends.RoleDreamEmbed) {
			continue
		}
		out[coupledPair{host: b.Host, protocol: string(b.Protocol)}] = struct{}{}
	}
	return out
}

// flushIfCoupledChanged flushes context_embed_cache when the coupled set of the
// now-active pool snapshot differs from the last flushed one (A04-W3,
// design/04 §4.2). Called after every pool reload in this handler — the funnel
// that carries API writes AND psql edits (053/092 triggers, same channel).
//
// A FAILED pool reload keeps the previous snapshot active, so the set is
// identical and nothing is flushed — no flush on a read that did not happen.
//
// The failure posture differs from settings.Reload on purpose: that path
// returns the error, here it is logged and the handler continues (a returned
// error is connection-level for pgxlisten). What makes the retry real instead
// of a phrase is that coupledPrev is advanced ONLY after a successful flush:
// the next notification re-diffs against the un-flushed stand and tries again.
func (h *SettingsWriteHandler) flushIfCoupledChanged(ctx context.Context) {
	if h.backendPool == nil || h.pool == nil {
		return
	}
	cur := coupledSetOf(h.backendPool)
	if maps.Equal(h.coupledPrev, cur) {
		return
	}
	flush := h.flush
	if flush == nil {
		flush = embedcache.Flush
	}
	n, err := flush(ctx, h.pool)
	if err != nil {
		slog.Error("listener: embed-cache flush after coupled pool change failed — stale vectors may serve", "error", err)
		return
	}
	h.coupledPrev = cur
	// A04-W4: the in-memory stand alone dies with the process. Stamping the
	// same set on record is what keeps the NEXT boot from re-diffing against a
	// pre-flush fingerprint and flushing a cache this one already emptied — and
	// it is what makes the boot check see only the edits it is there for.
	// A failed stamp is logged, not returned: the flush already happened, so the
	// worst case is one redundant flush at the next boot (fail-closed).
	if err := storeCoupledFingerprint(ctx, h.pool, cur); err != nil {
		slog.Error("listener: persisting embed-cache coupled fingerprint failed — the next boot re-diffs against the stale stamp and may flush again", "error", err)
	}
	slog.Info("listener: embed-cache-coupled pool topology changed — flushed context_embed_cache",
		"rows", n, "pairs", len(cur))
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
		// Re-derive the admission policy from whatever snapshot is now
		// active (design/01 N9): after a failed reload this maps the
		// unchanged previous snapshot — idempotent, never a policy from a
		// failed read.
		h.refreshDispatchPolicy()
		// The pool is the serving truth for the embed cache too: a base_url or
		// protocol edit on an embed row moves the vector space under an
		// unchanged model name (A04-W3, design/04 §4.2).
		h.flushIfCoupledChanged(ctx)
		return nil
	}
	// Disable-profile entity branch (Web-UX U01-W1, design/01 §4.1/N9): the 092
	// notify triggers ride the same channel. Profiles/memberships live in the
	// backend-pool snapshot, so a write here reloads the pool — but does NOT
	// touch refreshDispatchPolicy: profiles change no `limits`, the admission
	// policy derivation would be identical.
	if p.Entity == "context_disable_profiles" || p.Entity == "context_disable_profile_backends" {
		if h.backendPool == nil {
			return nil
		}
		if err := h.backendPool.Reload(ctx); err != nil {
			slog.Warn("listener: backend pool reload failed — previous snapshot stays active", "error", err)
		}
		// Same coupled diff as the row branch: a profile that ejects an embed
		// backend hands serving to a failover, and that failover may embed on a
		// different host under the same model name (design/04 §3.2a).
		h.flushIfCoupledChanged(ctx)
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
	// Re-map the dispatch.* keys from the now-active snapshot (design/01
	// §3.1). Only on this _global branch: the keys are global-only, so a
	// tenant-scope write (early return above) can never change them.
	h.refreshDispatchSettings()
	return nil
}

// refreshDispatchSettings pushes the current snapshot's dispatch.* keys into
// the dispatcher (design/01 §3.1: the reload owner maps config.DispatchConfig
// → dispatch.Settings; dispatch never imports config). Idempotent — callers
// invoke it after every reload attempt, successful or kept-previous.
func (h *SettingsWriteHandler) refreshDispatchSettings() {
	if h.dispatcher == nil {
		return
	}
	h.dispatcher.UpdateSettings(DispatchSettings(h.cfg.Snapshot().Dispatch)) //nolint:forbidigo // MT 06 OWNER: dispatch.* keys are global-only (design/01 §3.1) — the reload owner reads the base generation, no tenant dimension exists for them.
}

// refreshDispatchPolicy re-derives the per-origin admission policy from the
// current backend-pool snapshot and hot-swaps it (design/01 N9: NOTIFY →
// DerivePolicy → UpdatePolicy; a cleared limits row drains its wait queue as
// pass-through within NOTIFY latency — the W7 return path).
func (h *SettingsWriteHandler) refreshDispatchPolicy() {
	if h.dispatcher == nil || h.backendPool == nil {
		return
	}
	h.dispatcher.UpdatePolicy(dispatch.DerivePolicy(DispatchBackendRows(h.backendPool.Snapshot()), nil))
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
	h.refreshDispatchSettings()
	if h.backendPool != nil {
		if err := h.backendPool.Reload(ctx); err != nil {
			slog.Warn("listener: backend pool backlog reload failed — previous snapshot stays active", "error", err)
		}
		h.refreshDispatchPolicy()
		// A backend or profile write during the disconnect window carries the
		// same coupled semantics as one on the wire; its NOTIFY is gone, so the
		// diff here is the only thing that sees it. Unchanged topology diffs
		// empty, so a plain reconnect never flushes.
		h.flushIfCoupledChanged(ctx)
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
	// The graph-cache dirty signal: registered in W05.2, fed by the Migration 116
	// triggers since W05.3.
	listener.Handle(channelLinkWrite, &LinkWriteHandler{scheduler: scheduler})
	// dispatcher comes from the owning scheduler (SetDispatcher, boot
	// happens-before Run): the reload owner pushes re-mapped settings/policy
	// into it (MW2; nil = inert, exactly like blocktypes). Built through the
	// constructor since A04-W3 so the coupled baseline is taken from the
	// already-loaded boot pool — a struct literal would start at the empty set
	// and flush the embed cache on the first backend write after every boot.
	settingsHandler := NewSettingsWriteHandler(pool, cfg, backendPool, blocktypes)
	settingsHandler.dispatcher = scheduler.dispatcher
	listener.Handle(channelSettingsWrite, settingsHandler)
	// W9: forward ctx_project_write (081) to the SSE domain-event hub. Registered
	// ONLY when the hub is wired — a nil hub leaves the channel un-LISTENed, so
	// the 081 notifies are Postgres no-ops (the listener-discard invariant also
	// holds intra-process, not just for an old binary).
	if projectHub != nil {
		listener.Handle(channelProjectWrite, &ProjectNotifyHandler{hub: projectHub})
	}

	return listener
}
