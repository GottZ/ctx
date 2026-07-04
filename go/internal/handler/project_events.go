// GET /api/project/events — the workflow SSE domain-event stream (workflow W9,
// design/03-workflow-api-cli.md §4.5/§6.2). MEMBER-gated (any valid key), scope-
// FILTERED at the fan-out: a key receives frames only for the project scopes in
// its read set, never another tenant's. This is the deliberate counterpart to the
// server-admin-only telemetry hub GET /api/events (§4.5): different publikum
// (member vs admin), different payload (ids-only domain events vs status/backends/
// llmcall telemetry), so a SEPARATE endpoint over a SEPARATE hub — but the K8
// reuse vehicles (sseWriter frame+deadline, ": ping" keepalive, cap→429, in-
// stream re-auth) are shared with /api/events.
//
// Re-auth (verschärft gegenüber /api/events, §4.5): the telemetry hub ends a
// stream on revocation OR tenant_id/role change (identity compare). That is NOT
// enough here — ReadScopes carries CROSS-TENANT scope grants (auth.go), and a
// grant-revoke mid-stream changes NEITHER tenant_id NOR role. So every re-auth
// tick recomputes the sub's scope tags from the FRESH AuthResult and replaces the
// subscribe-time snapshot: a revoked grant drops that scope from the fan-out ≤ the
// re-auth interval, while the stream itself lives on (the key is still valid).
// Revocation / tenant change still ends the stream outright.
//
// Frames are IDS-ONLY (K16): {kind, project_id, op, block_ids} or the coalesced
// {kind:'issues-bulk', project_id, count} — never title/content/body. The client
// refetches details over the read API, so the stream is not a content-leak path.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// projectSSEReauthMult sets the re-auth cadence at N flush intervals (§4.5 "alle
// 12 Ticks"). A "tick" here is the hub flush interval (project.events.flush_
// interval). Package var so tests shrink it.
var projectSSEReauthMult = 12

// projectSSEReauthOverride, when > 0, forces the re-auth interval directly
// (tests). Production leaves it 0 and derives interval = flush × mult.
var projectSSEReauthOverride time.Duration

// ProjectEventsHandler serves GET /api/project/events over the shared projectHub.
// authenticate is the in-stream re-auth seam (defaults to auth.Authenticate on
// the pool; tests inject a fake to drive revocation / grant-revoke).
type ProjectEventsHandler struct {
	hub          *events.ProjectHub
	pool         *pgxpool.Pool
	cfg          ConfigStore
	authenticate func(ctx context.Context, key string) (*auth.AuthResult, error)
}

// NewProjectEventsHandler wires the hub + pool + config. cfg supplies the per-
// tenant connection cap and the ping / re-auth cadence.
func NewProjectEventsHandler(hub *events.ProjectHub, pool *pgxpool.Pool, cfg ConfigStore) *ProjectEventsHandler {
	return &ProjectEventsHandler{
		hub:  hub,
		pool: pool,
		cfg:  cfg,
		authenticate: func(ctx context.Context, key string) (*auth.AuthResult, error) {
			return auth.Authenticate(ctx, pool, key)
		},
	}
}

// MountProjectEvents mounts GET /api/project/events behind RequireMember (the
// gate lives in the mount — a missing gate is a missing route, §5.1). The handler
// then scope-filters every frame at the hub fan-out (RequireMember admits, the
// fan-out scopes — the K-T1 pairing).
func MountProjectEvents(r chi.Router, h *ProjectEventsHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireMember)
		r.Get("/api/project/events", h.HandleProjectEvents)
	})
}

// subscribeIdentity resolves the (global, scopeTags) fan-out identity for an
// AuthResult under an optional ?project_id= filter. A server-admin with NO filter
// is global (all project scopes, §4.6). With a filter, the sub is pinned to that
// ONE project's scope — and a non-admin must be able to read it (else 404 uniform,
// no existence oracle). Returns ok=false after writing the 404/500.
func (h *ProjectEventsHandler) subscribeIdentity(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) (global bool, tags []string, ok bool) {
	pid := r.URL.Query().Get("project_id")
	if pid == "" {
		// Unfiltered: all readable project scopes. Server-admin sees all (global);
		// the fan-out only ever emits PROJECT-scope frames, so tagging a member with
		// its full ReadScopes is exactly "its readable project scopes".
		return ar.IsServerAdmin(), ar.ReadScopes, true
	}
	// Filtered to one project: resolve + scope-read gate (uniform 404).
	row, err := store.GetProjectByID(r.Context(), h.pool, pid)
	if err != nil {
		writeInternal(w)
		return false, nil, false
	}
	if row == nil || (!ar.IsServerAdmin() && !slices.Contains(ar.ReadScopes, row.Scope)) {
		writeIssueNotFound(w)
		return false, nil, false
	}
	// A filtered sub is pinned to the one scope (NOT global-match-all even for an
	// admin — the caller asked to filter).
	return false, []string{row.Scope}, true
}

// reauthTags recomputes the fan-out tags for a re-auth tick from a fresh
// AuthResult, honoring the original filter. filterScope=="" is the unfiltered
// case (tags = fresh ReadScopes); a filter keeps the single scope only while it
// stays readable (grant-revoke drops it).
func reauthTags(fresh *auth.AuthResult, filterScope string) []string {
	if filterScope == "" {
		return fresh.ReadScopes
	}
	if fresh.IsServerAdmin() || slices.Contains(fresh.ReadScopes, filterScope) {
		return []string{filterScope}
	}
	return nil // grant to the filtered scope revoked → no frames (stream lives on)
}

// HandleProjectEvents serves the member SSE domain-event stream. Flow: identity +
// optional project filter → per-tenant cap-check → commit stream header → fan-out
// frames / pings / periodic re-auth until the client disconnects, the server
// shuts down, the hub drops a slow consumer, or the key is revoked / tenant-
// changed.
func (h *ProjectEventsHandler) HandleProjectEvents(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	global, tags, ok := h.subscribeIdentity(w, r, ar)
	if !ok {
		return
	}
	filterScope := ""
	if !global && len(tags) == 1 && r.URL.Query().Get("project_id") != "" {
		filterScope = tags[0]
	}

	// Per-TENANT cap (project.events.max_connections, tenant-overridable) — NOT the
	// server-global /api/events cap. Read from THIS caller's tenant snapshot so a
	// tenant override applies; count is per-tenant in the hub.
	cfg := h.cfg.SnapshotForRequest(r.Context())
	sub, admitted := h.hub.Subscribe(ar.TenantID, global, tags, cfg.Project.Events.MaxConnections)
	if !admitted {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"success": false, "error": "too many project event streams",
		})
		return
	}
	defer h.hub.Unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := newSSEWriter(w)
	// A ": ready" comment opens the stream (proves flushability, arms the proxy
	// read timeout) before any frame flows.
	if sw.comment("ready") != nil {
		return
	}

	flush := cfg.Project.Events.FlushInterval
	if flush <= 0 {
		flush = time.Second
	}
	ping := cfg.Project.Events.PingInterval
	if ping <= 0 {
		ping = 25 * time.Second
	}
	reauthEvery := projectSSEReauthOverride
	if reauthEvery <= 0 {
		reauthEvery = flush * time.Duration(projectSSEReauthMult)
	}

	key := apiKeyFromRequest(r)
	pingT := time.NewTicker(ping)
	defer pingT.Stop()
	reauthT := time.NewTicker(reauthEvery)
	defer reauthT.Stop()

	for {
		select {
		case <-r.Context().Done():
			return // client disconnected
		case <-sub.Done():
			return // hub dropped us (mailbox overflow)
		case f := <-sub.Frames():
			data, err := json.Marshal(f)
			if err != nil {
				continue
			}
			if sw.event("project", "", data) != nil {
				return
			}
		case <-pingT.C:
			if sw.ping() != nil {
				return
			}
		case <-reauthT.C:
			fresh, err := h.authenticate(r.Context(), key)
			// Revocation / invalid / TENANT change ends the stream outright (a
			// re-pointed key must not keep the old tenant's domain events).
			if err != nil || fresh == nil || !fresh.IsValid || fresh.TenantID != ar.TenantID {
				_ = sw.event("error", "", []byte(`{"error":"session revoked"}`))
				return
			}
			// Grant-revoke nachführung: recompute the scope tags from the FRESH read
			// set (a revoked cross-tenant grant drops its scope from the fan-out;
			// tenant_id/role are unchanged, so the identity compare above is blind to
			// it — this is the verschärfte W9 gate).
			h.hub.SetTags(sub, reauthTags(fresh, filterScope))
		}
	}
}
