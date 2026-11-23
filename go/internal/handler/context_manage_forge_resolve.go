// Manage family forge-conflict-* / forge-stamp-reset (issue #509): the way out
// of a conflict flag and of a forge stamp that went back, on top of
// forge.Resolver:
//
//	forge-conflict-list     the project's flagged mappings with both sides, keyset by block id
//	forge-conflict-resolve  resolve one flag: keep the block's version (local) or take the forge's (remote)
//	forge-stamp-reset       forge stamp NULL + fetch leg held to a full listing, for one mapping or all of the project
//
// All three are tierTenantAdmin like the forge-* sync family: they decide
// which side of a mirrored issue wins and steer the next sync run. Ownership
// is re-checked per project (loadOwnedProject: a foreign or absent project is
// the uniform 404, no oracle), and block_id and after are checked before
// the first database access, so a malformed id is a 400 and never reaches a
// uuid cast (project_id goes through loadOwnedProject like the other forge
// actions).
// The payloads decode strictly: a misspelt expect or all would otherwise drop
// the guard it names. A resolution and a reset record who did what in
// context_settings_audit (entity_type forge_sync, store.WriteForgeSyncAudit),
// which GET /api/audit?entity=forge_sync serves to the project tenant's admins.
package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/forge"
	"github.com/GottZ/ctx/internal/store"
)

// forgePushInFlightRetryAfterS is the retry hint (seconds) of a resolution
// refused because a push of the entity is on the wire. A push section holds
// the lock for one PATCH and one short transaction (the forge client's
// timeout bounds the PATCH), so a few seconds is the honest order of
// magnitude; the process cannot know how long the other session needs.
const forgePushInFlightRetryAfterS = 5

// forgeNoMappingError is the 404 text of a block without a mapping in the
// named project — the same answer for an unknown block, a block of another
// project, and one that left the project's scope or was archived.
const forgeNoMappingError = "no mapping of this project for that block"

// forgeBlockIDError is the 400 text of a block_id that is not a UUID.
const forgeBlockIDError = "block_id must be the block id (UUID) of a mapped issue or comment"

// ForgeResolver is the resolve surface the three actions consume
// (ForgeController pattern). *forge.Resolver implements it.
type ForgeResolver interface {
	ListConflicts(ctx context.Context, project store.ProjectRow, after string, limit int) ([]forge.Conflict, string, error)
	ResolveConflict(ctx context.Context, project store.ProjectRow, blockID, direction, expect string, stamp store.AuditStamp) (forge.ResolveResult, error)
	ResetStamps(ctx context.Context, project store.ProjectRow, blockID string, stamp store.AuditStamp) (forge.ResetResult, error)
}

// SetForgeResolver wires the resolve surface after construction, like
// SetForgeController; server.go calls it once. nil ⇒ the actions answer 503.
func (h *ManageHandler) SetForgeResolver(fr ForgeResolver) { h.forgeResolver = fr }

// forgeBadRequest writes the 400 of a payload the three actions refuse.
func forgeBadRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": msg})
}

type forgeConflictListData struct {
	ProjectID string `json:"project_id"`
	After     string `json:"after"`
	Limit     int    `json:"limit"`
}

// handleForgeConflictList answers a page of the project's flagged mappings:
// block and base with their projection, what the database knows of the forge
// side, the matrix cell and the flag token a resolution may pass as expect.
// after is the next_cursor of the previous page (a block id); limit 0 is the
// resolver's default, above its maximum the maximum.
func (h *ManageHandler) handleForgeConflictList(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	var d forgeConflictListData
	if msg := decodeStrict(req.Data, &d); msg != "" {
		forgeBadRequest(w, msg)
		return
	}
	switch {
	case d.ProjectID == "":
		forgeBadRequest(w, "project_id is required")
		return
	case d.After != "" && !uuidRe.MatchString(d.After):
		forgeBadRequest(w, "after must be the next_cursor of a previous page (a block id)")
		return
	case d.Limit < 0:
		forgeBadRequest(w, "limit must not be negative (0 = the default page size)")
		return
	}
	row, ok := h.loadOwnedProject(w, r, d.ProjectID)
	if !ok {
		return
	}
	conflicts, next, err := h.forgeResolver.ListConflicts(r.Context(), *row, d.After, d.Limit)
	if err != nil {
		logRequestError(r.Context(), "forge: conflict list failed", "project", row.ID, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeInternal(w)
		return
	}
	if conflicts == nil {
		conflicts = []forge.Conflict{}
	}
	var cursor any
	if next != "" {
		cursor = next
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "project_id": row.ID, "conflicts": conflicts, "next_cursor": cursor})
}

type forgeConflictResolveData struct {
	ProjectID string `json:"project_id"`
	BlockID   string `json:"block_id"`
	Direction string `json:"direction"`
	Expect    string `json:"expect"`
}

// handleForgeConflictResolve resolves the flag of one mapping: local keeps
// the block's version (the next push sends it over the forge's), remote takes
// the forge's (the next pull writes it over the block). A mapping that is no
// longer flagged answers 200 with changed:false and writes nothing.
func (h *ManageHandler) handleForgeConflictResolve(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d forgeConflictResolveData
	if msg := decodeStrict(req.Data, &d); msg != "" {
		forgeBadRequest(w, msg)
		return
	}
	switch {
	case d.ProjectID == "":
		forgeBadRequest(w, "project_id is required")
		return
	case !uuidRe.MatchString(d.BlockID):
		forgeBadRequest(w, forgeBlockIDError)
		return
	case d.Direction != forge.ResolveLocal && d.Direction != forge.ResolveRemote:
		forgeBadRequest(w, "direction must be local or remote")
		return
	}
	row, ok := h.loadOwnedProject(w, r, d.ProjectID)
	if !ok {
		return
	}
	res, err := h.forgeResolver.ResolveConflict(r.Context(), *row, d.BlockID, d.Direction, d.Expect, auditStamp(r, ar))
	if err != nil {
		writeForgeResolveError(w, r, row.ID, res, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "project_id": row.ID, "result": res})
}

// writeForgeResolveError maps a refused resolution onto its status. Every
// refusal of the resolver leaves the mapping as it was; only the 500 logs,
// and its cause stays in the log.
func writeForgeResolveError(w http.ResponseWriter, r *http.Request, projectID string, res forge.ResolveResult, err error) {
	switch {
	case errors.Is(err, forge.ErrConflictNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": forgeNoMappingError})
	case errors.Is(err, forge.ErrResolveDirection):
		forgeBadRequest(w, "direction must be local or remote")
	case errors.Is(err, forge.ErrPushInFlight):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false,
			"error":         "a push of this issue or comment is on the wire — retry shortly",
			"retry_after_s": forgePushInFlightRetryAfterS})
	case errors.Is(err, forge.ErrFlagChanged):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false,
			"error": "the flag changed since it was listed — list the conflicts again and decide on the current flag",
			"flag":  res.Flag})
	case errors.Is(err, forge.ErrLocalTruncated):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false,
			"error": "local cannot be kept here: the issue's body is truncated and differs from the body the push would diff it against, and the push never sends a truncated body — resolve with direction remote"})
	default:
		logRequestError(r.Context(), "forge: conflict resolve failed", "project", projectID, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeInternal(w)
	}
}

type forgeStampResetData struct {
	ProjectID string `json:"project_id"`
	BlockID   string `json:"block_id"`
	All       bool   `json:"all"`
}

// handleForgeStampReset runs the way out of a forge stamp that went back:
// the forge stamp of one mapping (block_id), or of every mapping of the
// project (all:true), becomes NULL and the fetch leg of each reset kind is
// held to a full listing. A project-wide reset needs the explicit all:true —
// a forgotten block_id must not reset every mapping.
func (h *ManageHandler) handleForgeStampReset(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d forgeStampResetData
	if msg := decodeStrict(req.Data, &d); msg != "" {
		forgeBadRequest(w, msg)
		return
	}
	switch {
	case d.ProjectID == "":
		forgeBadRequest(w, "project_id is required")
		return
	case d.BlockID != "" && d.All:
		forgeBadRequest(w, "block_id and all exclude each other: name one mapping, or all of the project")
		return
	case d.BlockID == "" && !d.All:
		forgeBadRequest(w, "block_id is required; a reset of every mapping of the project needs all:true")
		return
	case d.BlockID != "" && !uuidRe.MatchString(d.BlockID):
		forgeBadRequest(w, forgeBlockIDError)
		return
	}
	row, ok := h.loadOwnedProject(w, r, d.ProjectID)
	if !ok {
		return
	}
	res, err := h.forgeResolver.ResetStamps(r.Context(), *row, d.BlockID, auditStamp(r, ar))
	switch {
	case errors.Is(err, forge.ErrConflictNotFound) && d.BlockID == "":
		projectNotFound(w) // the project row went between the load and the lock
		return
	case errors.Is(err, forge.ErrConflictNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": forgeNoMappingError})
		return
	case err != nil:
		logRequestError(r.Context(), "forge: stamp reset failed", "project", row.ID, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeInternal(w)
		return
	}
	legs := res.Legs
	if legs == nil {
		legs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "project_id": row.ID, "reset": res.Reset, "legs": legs})
}
