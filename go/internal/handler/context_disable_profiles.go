package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
)

var (
	errDataRequired    = errors.New("data payload required")
	errUnparseableData = errors.New("unparseable data payload")
)

// errResponse is a deferred JSON error a helper can build and the caller writes
// (so validation helpers stay pure and testable).
type errResponse struct {
	code int
	msg  string
}

func (e *errResponse) write(w http.ResponseWriter) {
	writeJSON(w, e.code, map[string]any{"success": false, "error": e.msg})
}

// Disable-profile manage actions (092, Web-UX U01-W3, design/01 §4.3 + AM-5).
//
// TIER + SCOPE choice (documented): all five actions are tierTenantAdmin, NOT
// server-admin. This follows the backend-* pattern EXACTLY (T37/04-W5): the tier
// admits a tenant-admin, and the handler + store carry the tenant-isolation
// (the A8 precondition "open only what is already isolated"). AM-5 VOLL:
//   - list: shows _global profiles (read) + the caller's own-scope profiles.
//     backendVisibleToCaller is the exact visibility predicate (server-admin =
//     all; tenant-admin = _global ∪ own).
//   - create: scope is FORCED to ar.HomeScope for a tenant-admin (server-admin
//     picks freely, defaults _global) — mirrors backendCreateScope. A
//     tenant-admin's profile members must be its OWN backends; a _global (or
//     foreign) member ⇒ 422 (AM-5: "ein tenant-Profil, das ein _global-Backend
//     enthalten soll ⇒ 422").
//   - update/delete/toggle: the store scope predicate (profileWriteScopes) is
//     the fail-closed backstop — a tenant-admin touching a _global/foreign
//     profile matches zero rows → 404 (uniform with the backend pattern, gate g).
//
// Blackout impact (§4.4): roles_blacked_out is computed against the REFERENCE
// backend set of the profile's scope — _global for a _global profile (the
// fail-open correction: a role served only tenant-privately still counts as
// system-globally dark, gate e), _global ∪ tenant for a tenant profile (AM-5).
// Cooldown counts as available (transient, never removed from the chain); trust
// is request-dependent and excluded (documented approximation).

// profileToggleNote is the generalization of gamingModeNote (§5.4): a toggle
// affects NEW chains only — running requests finish normally.
const profileToggleNote = "laufende Requests beenden normal; Failover übernimmt ab nächster Chain"

// disableProfileSpec is the create/update/toggle payload. Members are backend
// NAMES (the API surface, §4.3) resolved to ids against the pool snapshot.
type disableProfileSpec struct {
	Name                *string  `json:"name"`
	Scope               *string  `json:"scope"`
	Label               *string  `json:"label"`
	Description         *string  `json:"description"`
	Active              *bool    `json:"active"`
	Members             []string `json:"members"`
	ConfirmRoleBlackout bool     `json:"confirm_role_blackout"`
	DryRun              bool     `json:"dry_run"`
}

// profileWriteScopes mirrors backendWriteScopes: nil for a server-admin (no
// scope filter), []string{ar.HomeScope} for a tenant-admin (own scope only).
func profileWriteScopes(ar *auth.AuthResult) []string {
	return backendWriteScopes(ar)
}

// dispatchDisableProfileAction fans the disable-profile-* actions out (split
// from HandleManage's switch for the cyclomatic budget).
func (h *ManageHandler) dispatchDisableProfileAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "gaming-mode", "eject-mode":
		// AM-7: kanonische eject-Fläche + gaming-Alias (Read = Legacy-Shape,
		// Mutation = Ein-Tx-Doppel-Write) — hier mit-dispatcht, damit HandleManage
		// im cyclop-Budget bleibt.
		h.handleGamingMode(w, r, req)
	case "disable-profile-list":
		h.handleDisableProfileList(w, r, ar)
	case "disable-profile-create":
		h.handleDisableProfileCreate(w, r, ar, req)
	case "disable-profile-update":
		h.handleDisableProfileUpdate(w, r, ar, req)
	case "disable-profile-delete":
		h.handleDisableProfileDelete(w, r, ar, req)
	case "disable-profile-toggle":
		h.handleDisableProfileToggle(w, r, ar, req)
	}
}

func (h *ManageHandler) handleDisableProfileList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	all, err := store.ListDisableProfiles(ctx, h.pool)
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-list: query failed", err)
		return
	}
	out := make([]map[string]any, 0, len(all))
	for i := range all {
		p := all[i]
		if !backendVisibleToCaller(ar, p.Scope) {
			continue
		}
		out = append(out, h.disableProfileView(ar, p, p.Active, all))
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "profiles": out})
}

func (h *ManageHandler) handleDisableProfileCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	spec, err := parseDisableProfileSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if spec.Name == nil || *spec.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "name required"})
		return
	}
	// Scope: forced to own tenant for a tenant-admin, free (default _global) for
	// a server-admin — reuse the backend rule verbatim.
	scope := backendCreateScope(ar, spec.Scope)
	memberIDs, errResp := h.resolveMembers(ar, scope, spec.Members)
	if errResp != nil {
		errResp.write(w)
		return
	}
	p := &store.DisableProfile{Scope: scope, Name: *spec.Name}
	if spec.Label != nil {
		p.Label = *spec.Label
	}
	if spec.Description != nil {
		p.Description = *spec.Description
	}
	if spec.Active != nil {
		p.Active = *spec.Active
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-create: begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-create: set request id failed", err)
		return
	}
	id, err := store.CreateDisableProfile(ctx, tx, p, memberIDs, actorID(r))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-create: commit failed", err)
		return
	}
	p.ID = id
	p.MemberIDs = memberIDs
	h.reloadAfterMutation(ctx, "disable-profile-create")
	all, _ := store.ListDisableProfiles(ctx, h.pool)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"profile": h.disableProfileView(ar, *p, p.Active, all),
		"as_of":   serverNow(),
	})
}

func (h *ManageHandler) handleDisableProfileUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	spec, err := parseDisableProfileSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	scope, name, ok := h.resolveProfileTarget(w, ar, spec)
	if !ok {
		return
	}
	// Member replacement is optional; when present it is subject to the same
	// own-scope rule as create (a tenant-admin can only attach its own backends).
	var memberIDsPtr *[]string
	if spec.Members != nil {
		memberIDs, errResp := h.resolveMembers(ar, scope, spec.Members)
		if errResp != nil {
			errResp.write(w)
			return
		}
		memberIDsPtr = &memberIDs
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-update: begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-update: set request id failed", err)
		return
	}
	found, err := store.UpdateDisableProfile(ctx, tx, scope, name, spec.Label, spec.Description, memberIDsPtr, actorID(r), profileWriteScopes(ar))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if !found {
		writeProfileNotFound(w)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-update: commit failed", err)
		return
	}
	h.reloadAfterMutation(ctx, "disable-profile-update")
	updated, err := store.GetDisableProfile(ctx, h.pool, scope, name)
	if err != nil || updated == nil {
		h.gamingInternalError(w, ctx, "disable-profile-update: reread failed", err)
		return
	}
	all, _ := store.ListDisableProfiles(ctx, h.pool)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"profile": h.disableProfileView(ar, *updated, updated.Active, all),
		"as_of":   serverNow(),
	})
}

func (h *ManageHandler) handleDisableProfileDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	spec, err := parseDisableProfileSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	scope, name, ok := h.resolveProfileTarget(w, ar, spec)
	if !ok {
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-delete: begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-delete: set request id failed", err)
		return
	}
	reserved, found, err := store.DeleteDisableProfile(ctx, tx, scope, name, actorID(r), profileWriteScopes(ar))
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-delete: delete failed", err)
		return
	}
	if !found {
		writeProfileNotFound(w)
		return
	}
	if reserved {
		// Break-glass guard (§4.3): the eject alias (ctx eject / ctx gaming)
		// hangs off this profile — deletion would strand the alias.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false,
			"error":   "Reserviertes Profil — Alias ctx eject/ctx gaming hängt daran; nicht löschbar",
		})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-delete: commit failed", err)
		return
	}
	h.reloadAfterMutation(ctx, "disable-profile-delete")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": name, "as_of": serverNow()})
}

func (h *ManageHandler) handleDisableProfileToggle(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	spec, err := parseDisableProfileSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if spec.Active == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "active (bool) required"})
		return
	}
	scope, name, ok := h.resolveProfileTarget(w, ar, spec)
	if !ok {
		return
	}
	target, err := store.GetDisableProfile(ctx, h.pool, scope, name)
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-toggle: query failed", err)
		return
	}
	// A missing profile OR one the caller may not mutate is uniformly "not found"
	// (no existence oracle — the same 404 body the store gate would produce).
	if target == nil || !backendWritableByCaller(ar, target.Scope) {
		writeProfileNotFound(w)
		return
	}

	all, err := store.ListDisableProfiles(ctx, h.pool)
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-toggle: list failed", err)
		return
	}
	impact := h.profileImpact(ar, *target, *spec.Active, all)

	// Fail-closed Role-Blackout (§5.1): activating a profile that would take a
	// role fully dark needs explicit confirm — 422 with the role list otherwise.
	if *spec.Active && !spec.ConfirmRoleBlackout && !spec.DryRun && len(impact.blackedOut) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success":           false,
			"error":             "activating this profile takes these roles fully dark — resend with confirm_role_blackout:true",
			"roles_blacked_out": impact.blackedOut,
			"embed_degraded":    impact.embedDegraded,
		})
		return
	}

	if spec.DryRun {
		target.Active = *spec.Active
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"profile": h.profileMeta(*target),
			"impact":  impact.render(),
			"as_of":   serverNow(),
			"note":    profileToggleNote,
			"dry_run": true,
		})
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-toggle: begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-toggle: set request id failed", err)
		return
	}
	found, err := store.SetDisableProfileActive(ctx, tx, scope, name, *spec.Active, actorID(r), profileWriteScopes(ar))
	if err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-toggle: write failed", err)
		return
	}
	if !found {
		writeProfileNotFound(w)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.gamingInternalError(w, ctx, "disable-profile-toggle: commit failed", err)
		return
	}
	// Synchronous reload so the flip hits the next chain in THIS process; as_of
	// is server time AFTER the reload (the merge floor, §4.5).
	h.reloadAfterMutation(ctx, "disable-profile-toggle")
	target.Active = *spec.Active
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"profile": h.profileMeta(*target),
		"impact":  impact.render(),
		"as_of":   serverNow(),
		"note":    profileToggleNote,
	})
}

// resolveProfileTarget resolves (scope, name) for update/delete/toggle. Scope is
// caller-chosen when present, else defaults per tier (server-admin → _global,
// tenant-admin → own scope). Returns ok=false having written the error.
func (h *ManageHandler) resolveProfileTarget(w http.ResponseWriter, ar *auth.AuthResult, spec *disableProfileSpec) (string, string, bool) {
	if spec.Name == nil || *spec.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "name required"})
		return "", "", false
	}
	scope := backends.GlobalScope
	if !ar.IsServerAdmin() {
		scope = ar.HomeScope
	}
	if spec.Scope != nil && *spec.Scope != "" {
		scope = *spec.Scope
	}
	return scope, *spec.Name, true
}

// resolveMembers turns member backend NAMES into ids against the pool snapshot,
// enforcing the AM-5 scope rule: a member must be a backend in the profile's
// scope (a _global profile takes _global backends; a tenant profile takes its
// own tenant's backends). A name that resolves to no backend in that scope —
// including a tenant-admin naming a _global backend — is a 422.
func (h *ManageHandler) resolveMembers(ar *auth.AuthResult, profileScope string, names []string) ([]string, *errResponse) {
	if len(names) == 0 {
		return nil, nil
	}
	byName := make(map[string]*backends.Backend)
	snap := h.backendPool.Snapshot()
	for i := range snap {
		b := &snap[i]
		if backendScopeMatches(profileScope, b.Scope) {
			byName[b.Name] = b
		}
	}
	ids := make([]string, 0, len(names))
	for _, n := range names {
		b, ok := byName[n]
		if !ok {
			return nil, &errResponse{code: http.StatusUnprocessableEntity,
				msg: "backend " + n + " is not a member candidate for scope " + profileScope +
					" — a profile may only disable backends in its own scope"}
		}
		ids = append(ids, b.ID)
	}
	return ids, nil
}

// backendScopeMatches reports whether a backend in bScope belongs to a profile
// in profileScope. A _global profile matches _global (or the "" test-seeded
// equivalent); a tenant profile matches exactly its own scope.
func backendScopeMatches(profileScope, bScope string) bool {
	if profileScope == backends.GlobalScope {
		return bScope == "" || bScope == backends.GlobalScope
	}
	return bScope == profileScope
}

// profileImpactResult is the internal impact tuple (blackedOut is also consumed
// by the toggle gate before it is rendered).
type profileImpactResult struct {
	backends      []map[string]any
	rolesAffected []string
	blackedOut    []string
	embedDegraded bool
}

func (r profileImpactResult) render() map[string]any {
	out := map[string]any{
		"backends":          r.backends,
		"roles_affected":    r.rolesAffected,
		"roles_blacked_out": r.blackedOut,
	}
	if r.embedDegraded {
		out["embed_degraded"] = true
	}
	return out
}

// profileImpact computes the effect of `profile` AT the given active state
// (§4.4). Reference backend set = _global for a _global profile (fail-open
// correction, gate e), _global ∪ tenant for a tenant profile (AM-5). Cooldown
// counts available; trust is excluded (request-dependent).
func (h *ManageHandler) profileImpact(ar *auth.AuthResult, profile store.DisableProfile, targetActive bool, all []store.DisableProfile) profileImpactResult {
	snap := h.backendPool.Snapshot()
	byID := make(map[string]*backends.Backend, len(snap))
	for i := range snap {
		byID[snap[i].ID] = &snap[i]
	}
	statusByID := make(map[string]backends.BackendStatus)
	for _, s := range h.backendPool.Status() {
		statusByID[s.ID] = s
	}

	// disabledSet: backends taken out by ANY active profile, with THIS profile's
	// active state overridden to the target we are evaluating.
	disabledSet := make(map[string]bool)
	for i := range all {
		p := all[i]
		effActive := p.Active
		if p.Scope == profile.Scope && p.Name == profile.Name {
			effActive = targetActive
		}
		if effActive {
			for _, id := range p.MemberIDs {
				disabledSet[id] = true
			}
		}
	}

	// Reference scopes for the blackout computation.
	refScopes := map[string]bool{backends.GlobalScope: true, "": true}
	if profile.Scope != backends.GlobalScope {
		refScopes[profile.Scope] = true
	}

	// Member view (caller-visible only) + roles_affected (over ALL members —
	// impact truth, not filtered by visibility).
	memberView := make([]map[string]any, 0, len(profile.MemberIDs))
	roleSet := make(map[string]bool)
	for _, id := range profile.MemberIDs {
		b := byID[id]
		if b == nil {
			continue
		}
		for _, role := range b.Roles {
			roleSet[role] = true
		}
		if !backendVisibleToCaller(ar, b.Scope) {
			continue
		}
		state := "active"
		if s, ok := statusByID[id]; ok && s.EffectiveState != "" {
			state = s.EffectiveState
		}
		memberView = append(memberView, map[string]any{
			"id": b.ID, "name": b.Name, "scope": b.Scope,
			"roles": b.Roles, "enabled": b.Enabled, "effective_state": state,
		})
	}

	// roles_blacked_out: for each affected role, is there any reference-scope
	// backend still serving it (enabled, not in disabledSet)? Cooldown counts.
	var blackedOut []string
	for role := range roleSet {
		available := false
		for i := range snap {
			b := &snap[i]
			if !refScopes[b.Scope] || !b.Enabled || disabledSet[b.ID] {
				continue
			}
			if b.HasRole(role) {
				available = true
				break
			}
		}
		if !available {
			blackedOut = append(blackedOut, role)
		}
	}

	rolesAffected := make([]string, 0, len(roleSet))
	for role := range roleSet {
		rolesAffected = append(rolesAffected, role)
	}
	sort.Strings(rolesAffected)
	sort.Strings(blackedOut)

	embedDegraded := false
	for _, r := range blackedOut {
		if r == backends.RoleEmbed {
			embedDegraded = true
		}
	}
	return profileImpactResult{
		backends:      memberView,
		rolesAffected: rolesAffected,
		blackedOut:    blackedOut,
		embedDegraded: embedDegraded,
	}
}

// disableProfileView renders one profile + its impact (list/create/update). The
// impact is computed at atActive (the current state for list, the fresh state
// after a mutation).
func (h *ManageHandler) disableProfileView(ar *auth.AuthResult, p store.DisableProfile, atActive bool, all []store.DisableProfile) map[string]any {
	v := h.profileMeta(p)
	v["impact"] = h.profileImpact(ar, p, atActive, all).render()
	return v
}

func (h *ManageHandler) profileMeta(p store.DisableProfile) map[string]any {
	return map[string]any{
		"name": p.Name, "scope": p.Scope, "label": p.Label,
		"description": p.Description, "active": p.Active, "reserved": p.Reserved,
	}
}

func parseDisableProfileSpec(data json.RawMessage) (*disableProfileSpec, error) {
	if len(data) == 0 {
		return nil, errDataRequired
	}
	var spec disableProfileSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, errUnparseableData
	}
	return &spec, nil
}

func writeProfileNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "disable profile not found"})
}

// serverNow is the as_of floor value (§4.5): server time after a synchronous
// reload, RFC3339Nano so the client can order out-of-order status merges.
func serverNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
