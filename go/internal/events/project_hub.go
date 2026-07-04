package events

// projectHub — the LISTEN-consumer + per-scope SSE fan-out core for the workflow
// domain-event stream (GET /api/project/events, workflow W9; design/03-workflow-
// api-cli.md §4.5/§6.2). It is deliberately HTTP-free: it delivers ProjectFrame
// structs onto per-connection channels, and the handler layer (handler/project_
// events.go) frames them onto the SSE connection with the shared sseWriter. That
// split keeps the events package free of a handler import (no cycle) and lets the
// fan-out / cap / coalescing / cache logic be unit-tested with a fake sink.
//
// Topology (one process, monolith):
//
//	081 trigger ──NOTIFY ctx_project_write──▶ pgxlisten (one dedicated conn)
//	                                              │ HandleNotification
//	                                              ▼
//	                                       hub.Dispatch(payload)
//	                                              │ scope→project cache, accumulate
//	                                              ▼
//	                              flush loop (project.events.flush_interval)
//	                                              │ coalesce per project, build frames
//	                                              ▼
//	                              fan-out ──▶ subs whose scope-tag matches (global = all)
//
// The single pgxlisten goroutine calls Dispatch serially; the flush loop and the
// subscribe/unsubscribe/fan-out paths guard shared state with mu. Frames are
// IDS-ONLY (never content, K16): the client refetches over the read API.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/util"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// channelProjectWrite is the PG NOTIFY channel the 081 triggers fire
// (trg_project_write row-level INSERT/UPDATE, trg_project_delete statement-level
// DELETE) for issue/comment blocks ONLY. It is DISTINCT from ctx_block_write
// (guard/digest, byte-for-byte untouched by 081) — an old binary that does not
// LISTEN this channel discards every notify (Postgres no-op), so 081 is
// backward-safe (§4.5 listener-discard).
const channelProjectWrite = "ctx_project_write"

// projectCacheTTL is the defensive re-resolve window for the scope→project map
// (§4.5): create/patch/delete invalidate synchronously (monolith, one process),
// the TTL only guards a hypothetical future out-of-band context_projects write.
const projectCacheTTL = 60 * time.Second

// projectSubMailbox is the per-connection send buffer. A sub whose mailbox
// overflows cannot keep up; the hub drops it (close done) so a slow consumer
// cannot stall the fan-out — the client reconnects / falls back to polling.
const projectSubMailbox = 32

// ProjectFrame is one fanned-out SSE domain event. IDS-ONLY (K16): it carries
// block ids, NEVER title/content/body — there is no content-leak path through the
// stream, the client refetches over the read API. Kind is the discriminator:
// 'issue'/'comment' for an id-list frame, 'issues-bulk' for a coalesced burst
// (Count, no ids — the client refetches the affected list), 'resync' after a
// listener reconnect (refetch everything visible).
type ProjectFrame struct {
	Kind      string   `json:"kind"`
	ProjectID string   `json:"project_id,omitempty"`
	Op        string   `json:"op,omitempty"`
	BlockIDs  []string `json:"block_ids,omitempty"`
	Count     int      `json:"count,omitempty"`
}

// projectNotifyPayload mirrors the 081 NOTIFY payload. bulk marks the statement-
// level DELETE (prune) coalesce signal; id is empty then.
type projectNotifyPayload struct {
	ID    string `json:"id"`
	Op    string `json:"op"`
	Scope string `json:"scope"`
	Type  string `json:"type"`
	Bulk  bool   `json:"bulk"`
}

// projectSub is one subscriber's mailbox + fan-out identity. tags is the set of
// scopes this sub receives frames for; it is REPLACED on the handler's re-auth
// tick (grant-revoke nachgeführt, §4.5) under mu. global (server-admin, no
// project filter) matches every frame regardless of tags.
type projectSub struct {
	ch     chan ProjectFrame
	done   chan struct{}
	tenant string
	global bool

	mu   sync.Mutex
	tags map[string]struct{}
}

// setTags atomically replaces the sub's scope-tag set (re-auth nachführung).
func (s *projectSub) setTags(scopes []string) {
	next := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		next[sc] = struct{}{}
	}
	s.mu.Lock()
	s.tags = next
	s.mu.Unlock()
}

// wants reports whether this sub should receive a frame for scope.
func (s *projectSub) wants(scope string) bool {
	if s.global {
		return true
	}
	s.mu.Lock()
	_, ok := s.tags[scope]
	s.mu.Unlock()
	return ok
}

// cachedProject is one scope→project resolution. id=="" is a NEGATIVE entry (a
// non-project scope — the whole knowledge corpus resolves here once and is
// discarded on every later notify without a DB hit).
type cachedProject struct {
	id string
	at time.Time
}

// pendingScope accumulates one flush window's changes for a single project scope.
// groups is keyed "kind|op" → set of block ids; bulk marks a DELETE/prune or a
// threshold overflow → the flush emits a content-free issues-bulk frame instead.
type pendingScope struct {
	projectID string
	groups    map[string]map[string]struct{}
	bulk      bool
}

// ProjectHub is the process-wide domain-event fan-out. One per daemon.
type ProjectHub struct {
	pool *pgxpool.Pool
	cfg  *config.Store
	life context.Context

	mu      sync.Mutex
	subs    map[*projectSub]struct{}
	perTn   map[string]int           // tenant_id → live sub count (per-tenant cap)
	cache   map[string]cachedProject // scope → project resolution (+ negatives)
	pending map[string]*pendingScope // scope → this window's accumulation
}

// NewProjectHub builds the hub and starts its flush loop bound to life (server
// lifecycle ctx; cancel stops the loop). cfg drives the flush cadence + coalesce
// threshold (hot-reloaded per flush).
func NewProjectHub(life context.Context, pool *pgxpool.Pool, cfg *config.Store) *ProjectHub {
	h := &ProjectHub{
		pool:    pool,
		cfg:     cfg,
		life:    life,
		subs:    map[*projectSub]struct{}{},
		perTn:   map[string]int{},
		cache:   map[string]cachedProject{},
		pending: map[string]*pendingScope{},
	}
	go h.flushLoop()
	return h
}

// Subscribe registers a connection. cap is the PER-TENANT connection ceiling the
// caller loaded from the tenant's config snapshot (project.events.max_connections,
// tenant-overridable, §4.4) — NOT the server-global events cap. Returns ok=false
// when THIS tenant already holds cap streams (the caller answers 429); a foreign
// tenant saturating its own cap never blocks another (§6.2 per-tenant probe).
func (h *ProjectHub) Subscribe(tenant string, global bool, tags []string, maxConns int) (*projectSub, bool) {
	if maxConns <= 0 {
		maxConns = 16
	}
	s := &projectSub{
		ch:     make(chan ProjectFrame, projectSubMailbox),
		done:   make(chan struct{}),
		tenant: tenant,
		global: global,
	}
	s.setTags(tags)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.perTn[tenant] >= maxConns {
		return nil, false
	}
	h.subs[s] = struct{}{}
	h.perTn[tenant]++
	return s, true
}

// Unsubscribe removes a connection and decrements its tenant's live count.
func (h *ProjectHub) Unsubscribe(s *projectSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	if h.perTn[s.tenant] > 0 {
		h.perTn[s.tenant]--
	}
	if h.perTn[s.tenant] == 0 {
		delete(h.perTn, s.tenant)
	}
}

// Frames exposes a sub's read channel (the handler ranges it).
func (s *projectSub) Frames() <-chan ProjectFrame { return s.ch }

// Done is closed when the hub drops the sub (mailbox overflow).
func (s *projectSub) Done() <-chan struct{} { return s.done }

// SetTags is the handler's re-auth nachführung: it replaces the sub's scope tags
// with the freshly authenticated read set (grant-revoke drops a scope ≤ the
// re-auth interval; §4.5). global stays as set at subscribe (identity, not scope).
func (h *ProjectHub) SetTags(s *projectSub, scopes []string) { s.setTags(scopes) }

// InvalidateProjects clears the scope→project cache. Called by the MountProject
// create/patch/delete write path (§4.5): a new project's scope resolves on its
// next notify, a deleted project's scope stops resolving. Monolith = one process,
// so this synchronous wipe is a complete invalidation (no distributed cache).
func (h *ProjectHub) InvalidateProjects() {
	h.mu.Lock()
	h.cache = map[string]cachedProject{}
	h.mu.Unlock()
}

// resolveProject maps a scope to its project id, cached (+ negative cached). A
// cache miss (or expired TTL entry) does ONE indexed lookup on context_projects
// (uq_projects_scope). Returns ok=false for a non-project scope (the corpus).
func (h *ProjectHub) resolveProject(scope string) (string, bool) {
	now := time.Now()
	h.mu.Lock()
	if c, ok := h.cache[scope]; ok && now.Sub(c.at) < projectCacheTTL {
		h.mu.Unlock()
		return c.id, c.id != ""
	}
	h.mu.Unlock()

	var id string
	ctx, cancel := context.WithTimeout(h.life, 3*time.Second)
	err := h.pool.QueryRow(ctx, `SELECT id::text FROM context_projects WHERE scope = $1`, scope).Scan(&id)
	cancel()
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// Transient DB error: do NOT cache, do NOT resolve — the notify is dropped
		// this round; a later notify re-tries. Logged, not fatal.
		slog.Warn("projecthub: scope→project resolve failed", "scope", scope, "error", err)
		return "", false
	}
	h.mu.Lock()
	h.cache[scope] = cachedProject{id: id, at: now} // id=="" on ErrNoRows → negative
	h.mu.Unlock()
	return id, id != ""
}

// Dispatch is called by the pgxlisten handler for each ctx_project_write notify
// (single listener goroutine → serialized). It resolves the scope to a project,
// drops non-project scopes, and accumulates the change into the current flush
// window. It performs NO fan-out itself — the flush loop coalesces and fans out.
func (h *ProjectHub) Dispatch(payload string) {
	var p projectNotifyPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		slog.Warn("projecthub: bad notify payload",
			"payload", util.TruncateRunesWithSuffix(payload, "...", 200), "error", err)
		return
	}
	if p.Scope == "" {
		return
	}
	projectID, ok := h.resolveProject(p.Scope)
	if !ok {
		return // non-project scope (the corpus) — discarded, negative-cached
	}

	h.mu.Lock()
	ps := h.pending[p.Scope]
	if ps == nil {
		ps = &pendingScope{projectID: projectID, groups: map[string]map[string]struct{}{}}
		h.pending[p.Scope] = ps
	}
	ps.projectID = projectID
	if p.Bulk || p.Op == "DELETE" {
		ps.bulk = true // prune / physical delete → refetch signal, no ids
	} else if p.ID != "" {
		kind := p.Type
		if kind == "" {
			kind = "issue"
		}
		key := kind + "|" + p.Op
		g := ps.groups[key]
		if g == nil {
			g = map[string]struct{}{}
			ps.groups[key] = g
		}
		g[p.ID] = struct{}{}
	}
	h.mu.Unlock()
}

// flushLoop coalesces + fans out once per flush interval until life is cancelled.
func (h *ProjectHub) flushLoop() {
	interval := h.flushInterval()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-h.life.Done():
			return
		case <-t.C:
			// Hot-reload the cadence: a settings flip retunes the next window.
			if ni := h.flushInterval(); ni != interval {
				interval = ni
				t.Reset(interval)
			}
			h.flush()
		}
	}
}

func (h *ProjectHub) flushInterval() time.Duration {
	d := h.cfg.Snapshot().Project.Events.FlushInterval //nolint:forbidigo // hub flush cadence is a process-global fan-out timer, not tenant-scoped.
	if d <= 0 {
		d = time.Second
	}
	return d
}

func (h *ProjectHub) coalesceThreshold() int {
	n := h.cfg.Snapshot().Project.Events.CoalesceThreshold //nolint:forbidigo // process-global coalescing knob, shared across all connections.
	if n <= 0 {
		n = 20
	}
	return n
}

// flush drains the accumulated window and fans out the resulting frames. It
// snapshots + clears pending under the lock, then builds + fans out lock-free
// per scope (fan-out re-takes the lock briefly per frame).
func (h *ProjectHub) flush() {
	h.mu.Lock()
	if len(h.pending) == 0 {
		h.mu.Unlock()
		return
	}
	pending := h.pending
	h.pending = map[string]*pendingScope{}
	h.mu.Unlock()

	threshold := h.coalesceThreshold()
	for scope, ps := range pending {
		total := 0
		for _, g := range ps.groups {
			total += len(g)
		}
		// Burst OR prune → ONE content-free issues-bulk frame (the client refetches
		// the affected list). O(subs) per tick, not O(writes) (§6.2 coalescing).
		if ps.bulk || total > threshold {
			h.fanout(scope, ProjectFrame{Kind: "issues-bulk", ProjectID: ps.projectID, Count: total})
			continue
		}
		// Normal: one id-list frame per (kind, op) group.
		for key, g := range ps.groups {
			kind, op, _ := strings.Cut(key, "|")
			ids := make([]string, 0, len(g))
			for id := range g {
				ids = append(ids, id)
			}
			h.fanout(scope, ProjectFrame{Kind: kind, ProjectID: ps.projectID, Op: op, BlockIDs: ids})
		}
	}
}

// fanout delivers one frame to every sub that wants scope, non-blocking. A full
// mailbox drops the sub (close done + remove) so a slow consumer cannot stall the
// rest. Deleting during range is safe in Go.
func (h *ProjectHub) fanout(scope string, f ProjectFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if !s.wants(scope) {
			continue
		}
		select {
		case s.ch <- f:
		default:
			close(s.done)
			delete(h.subs, s)
			if h.perTn[s.tenant] > 0 {
				h.perTn[s.tenant]--
			}
		}
	}
}

// broadcastResync tells EVERY live sub to refetch its visible board. Used after a
// listener reconnect (HandleBacklog): notifies fired during the disconnect window
// are lost, so a blanket resync is the only correct recovery (no per-project
// granularity survives a gap). Content-free (K16).
func (h *ProjectHub) broadcastResync() {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := ProjectFrame{Kind: "resync"}
	for s := range h.subs {
		select {
		case s.ch <- f:
		default:
			close(s.done)
			delete(h.subs, s)
			if h.perTn[s.tenant] > 0 {
				h.perTn[s.tenant]--
			}
		}
	}
}

// ── pgxlisten glue ──────────────────────────────────────────────────────────.

// ProjectNotifyHandler adapts the hub onto pgxlisten's per-channel handler for
// ctx_project_write. It forwards the payload to the hub and, on reconnect
// backlog, triggers a blanket resync.
type ProjectNotifyHandler struct{ hub *ProjectHub }

// HandleNotification forwards one ctx_project_write payload to the hub.
func (h *ProjectNotifyHandler) HandleNotification(_ context.Context, n *pgconn.Notification, _ *pgx.Conn) error {
	h.hub.Dispatch(n.Payload)
	return nil
}

// HandleBacklog runs after a reconnect: the disconnect window's notifies are
// gone, so signal every sub to resync. Never returns an error (pgxlisten treats
// handler errors as connection-level).
func (h *ProjectNotifyHandler) HandleBacklog(_ context.Context, _ string, _ *pgx.Conn) error {
	slog.Info("projecthub: listener backlog — broadcasting resync")
	h.hub.broadcastResync()
	return nil
}
