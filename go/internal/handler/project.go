// /api/project — the project register surface (workflow W4, design/03
// §3.1/§4.2/§5.1/§5.7). One project = one repo corpus bound to exactly one
// tenant scope (Modell C). Reads are member-gated (scope-read); create/patch/
// delete are tenant-admin (server-admin targets a foreign tenant via the
// tenant_id field, T22-analog). ONE MountProject function holds BOTH gate groups
// and the routes, so the negative probes exercise exactly the production chain
// (§5.1: the fail-open class is eliminated structurally — a missing gate means a
// missing route, 404, never fail-open).
//
//	GET    /api/project          list (ReadScopes ∩; ?identity= resolution)   member
//	POST   /api/project          compound create (scope + row, ONE tx)        tenant-admin
//	GET    /api/project/{id}     detail                                       member (scope-read)
//	PATCH  /api/project/{id}     display_name/forge only                      tenant-admin
//	DELETE /api/project/{id}     register-row delete (blocks + scope stay)    tenant-admin
//
// Isolation invariants (§5.2): a foreign project id/identity is 404 uniform (no
// existence oracle); create prefixes the scope from the DB slug (no scope
// injection) and loads TenantLimits FAIL-CLOSED (no silent quota bypass); PATCH
// can not reach scope/tenant_id/webhook_secret_ref (422) and validates
// forge.api_base against the SSRF deny-list (§5.7).
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProjectHandler serves the /api/project surface.
type ProjectHandler struct {
	pool *pgxpool.Pool
}

// NewProjectHandler creates a new ProjectHandler.
func NewProjectHandler(pool *pgxpool.Pool) *ProjectHandler {
	return &ProjectHandler{pool: pool}
}

// identityPrefixes is the closed set of valid project-identity kinds (§3.1,
// validated in Go — no DB CHECK on an open set, v2.0.0 line).
var identityPrefixes = []string{"github:", "git-root:", "manual:"}

// MountProject mounts the /api/project routes with TWO gate groups in ONE
// function (design/03 §5.1). Reads (list, get) are member-gated; writes (create,
// patch, delete) are RequireAdminOrTenantAdmin. chi keys handlers by method, so
// GET and PATCH/DELETE on the same '/api/project/{id}' path live in different
// middleware stacks without collision. Both gates ADMIT only — every handler
// re-scopes to ReadScopes (reads) or tenant-ownership (writes), the K-T1
// invariant.
func MountProject(r chi.Router, h *ProjectHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireMember)
		r.Get("/api/project", h.HandleList)
		r.Get("/api/project/{id}", h.HandleGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(RequireAdminOrTenantAdmin)
		r.Post("/api/project", h.HandleCreate)
		r.Patch("/api/project/{id}", h.HandlePatch)
		r.Delete("/api/project/{id}", h.HandleDelete)
	})
}

// HandleList implements GET /api/project — the projects visible to the caller
// (scope ∈ ReadScopes), newest first. An optional ?identity= narrows to the
// single project of that identity (the `ctx project init` existence probe,
// §4.3). Isolation is the scope intersection: a foreign project is simply absent.
func (h *ProjectHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	identity := strings.TrimSpace(r.URL.Query().Get("identity"))
	rows, err := store.ListProjects(ctx, h.pool, ar.ReadScopes, identity)
	if err != nil {
		internalProjectError(w, ctx, "project: list error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "projects": rows})
}

// HandleGet implements GET /api/project/{id} — one project, member-visible by
// scope-read. An unknown id, a malformed id, AND a foreign-scope project all
// read as 404 with the SAME body (no existence oracle, §5.2(1)).
func (h *ProjectHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	row, err := store.GetProjectByID(ctx, h.pool, chi.URLParam(r, "id"))
	if err != nil {
		internalProjectError(w, ctx, "project: get error", err)
		return
	}
	if row == nil || !slices.Contains(ar.ReadScopes, row.Scope) {
		projectNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "project": row})
}

// projectCreateRequest is the POST body. scope is a NAME (server prefixes it from
// the DB slug); tenant_id is server-admin-only foreign targeting (§4.2, T22).
type projectCreateRequest struct {
	Identity    string          `json:"identity"`
	Scope       string          `json:"scope"`        // scope NAME, not the full '<slug>:<name>'
	DisplayName string          `json:"display_name"` // optional
	Forge       json.RawMessage `json:"forge"`        // optional {kind,owner,repo,api_base?}
	TenantID    string          `json:"tenant_id"`    // server-admin only (foreign-tenant create)
}

// HandleCreate implements POST /api/project — the compound create. Binding-tenant
// resolution mirrors handleScopeCreate/resolveKeyMintTenant: a tenant-admin is
// ALWAYS bound to its own ar.TenantID (a tenant_id field ⇒ 403, no self-escalation);
// a server-admin MUST pass tenant_id (its writeScope is _global, §4.2). The scope
// is server-built from the DB slug (no injection), TenantLimits is loaded
// FAIL-CLOSED (a lookup error is 500, never silent unlimited — the quota-bypass
// gate), and the scope-assign + register-insert are ONE tx in store.CreateProject.
func (h *ProjectHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)

	var req projectCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid JSON body"})
		return
	}

	// Binding-tenant resolution + self-escalation guard (§4.2). A non-server-admin
	// with a tenant_id field is trying to target a foreign tenant ⇒ 403.
	if req.TenantID != "" && !ar.IsServerAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "tenant_id is server-admin only"})
		return
	}
	bindingTenant := ar.TenantID
	if ar.IsServerAdmin() {
		if req.TenantID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "tenant_id required for server-admin create"})
			return
		}
		bindingTenant = req.TenantID
	}

	// Identity: non-empty, one of the closed prefix set (§3.1).
	identity := strings.TrimSpace(req.Identity)
	if !validIdentity(identity) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "identity must start with github: | git-root: | manual:"})
		return
	}

	// Resolve + slug-validate the binding tenant, then build the scope from the DB
	// slug (never a caller-supplied prefix — §5.2(2) scope-injection defense).
	tn, err := store.GetTenant(ctx, h.pool, bindingTenant)
	if err != nil {
		if errors.Is(err, store.ErrTenantNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
			return
		}
		internalProjectError(w, ctx, "project: get tenant error", err)
		return
	}
	if !slugPattern.MatchString(tn.Slug) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "tenant slug is not prefix-safe"})
		return
	}
	name := strings.TrimSpace(req.Scope)
	if !scopeNamePattern.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "scope must be 1-24 chars of a-z, 0-9, '-' (no leading/trailing '-', no ':')"})
		return
	}
	scope := tn.Slug + ":" + name
	if len(scope) > 50 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "scope name too long (max 50 chars including the tenant prefix)"})
		return
	}

	// forge.api_base SSRF deny-list at create time too (a create can seed the same
	// dangerous api_base a PATCH would reject; §5.7).
	if msg := validateForge(req.Forge); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
		return
	}

	// FAIL-CLOSED (S3, §4.2): TenantLimits for the BINDING tenant. A transient
	// lookup error is a 500, never a silent default to unlimited — otherwise
	// project-create would be a scope-quota bypass.
	maxScopes, _, err := store.TenantLimits(ctx, h.pool, bindingTenant)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "tenant limits lookup failed"})
		return
	}
	row, _, err := store.CreateProject(ctx, h.pool, store.CreateProjectParams{
		TenantID:    bindingTenant,
		ScopeName:   scope,
		MaxScopes:   maxScopes,
		Identity:    identity,
		DisplayName: strings.TrimSpace(req.DisplayName),
		Forge:       req.Forge,
		CreatedBy:   ar.ApiKeyID,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrScopeQuotaExceeded):
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "tenant scope quota exceeded"})
		case errors.Is(err, store.ErrScopeExists):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "scope already exists"})
		case errors.Is(err, store.ErrTenantNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		default:
			internalProjectError(w, ctx, "project: create error", err)
		}
		return
	}
	// Idempotent re-init and fresh create both return 200 (codebase convention,
	// handleScopeCreate) — the row identity + the no-duplicate DB state carry the
	// distinction (§7-W4 idempotency gate).
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "project": row})
}

// projectPatchRequest carries ONLY the mutable fields. The forbidden keys
// (scope/tenant_id/webhook_secret_ref) are detected on the raw body BEFORE this
// decode, so a caller can not smuggle them.
type projectPatchRequest struct {
	DisplayName *string         `json:"display_name"`
	Forge       json.RawMessage `json:"forge"`
}

// forbiddenPatchKeys are rejected 422 (server-managed / immutable, §3.1/§4.2).
var forbiddenPatchKeys = []string{"scope", "tenant_id", "webhook_secret_ref"}

// HandlePatch implements PATCH /api/project/{id} — display_name/forge only,
// tenant-admin of the owning tenant. A forbidden key ⇒ 422 (webhook_secret_ref
// is server-managed; scope/tenant_id are the §3.1 invariant). forge.api_base is
// validated against the SSRF deny-list ⇒ 422. A foreign/absent project ⇒ 404
// uniform.
func (h *ProjectHandler) HandlePatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)

	// Raw-key sweep first: reject the server-managed/immutable fields 422 before
	// touching the DB (no partial write, no oracle).
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid JSON body"})
		return
	}
	for _, k := range forbiddenPatchKeys {
		if _, ok := raw[k]; ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": "field '" + k + "' is not patchable"})
			return
		}
	}

	// Ownership: load unscoped, then require tenant-ownership (or server-admin). A
	// foreign project is 404 uniform (no oracle) — NOT 403 (which would confirm it
	// exists in another tenant).
	row, err := store.GetProjectByID(ctx, h.pool, chi.URLParam(r, "id"))
	if err != nil {
		internalProjectError(w, ctx, "project: patch load error", err)
		return
	}
	if row == nil || !ownsProject(ar, row) {
		projectNotFound(w)
		return
	}

	var req projectPatchRequest
	if rawDN, ok := raw["display_name"]; ok {
		var s string
		if err := json.Unmarshal(rawDN, &s); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "display_name must be a string"})
			return
		}
		req.DisplayName = &s
	}
	if rawForge, ok := raw["forge"]; ok {
		req.Forge = rawForge
		if msg := validateForge(req.Forge); msg != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
			return
		}
	}

	updated, err := store.UpdateProject(ctx, h.pool, row.ID, req.DisplayName, req.Forge)
	if err != nil {
		internalProjectError(w, ctx, "project: patch error", err)
		return
	}
	if updated == nil {
		projectNotFound(w) // vanished between load and update
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "project": updated})
}

// HandleDelete implements DELETE /api/project/{id} — removes the register row
// (sync_runs CASCADE); the project's blocks AND its tenant scope survive
// (§4.2). tenant-admin of the owning tenant; a foreign/absent project ⇒ 404
// uniform.
func (h *ProjectHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	row, err := store.GetProjectByID(ctx, h.pool, chi.URLParam(r, "id"))
	if err != nil {
		internalProjectError(w, ctx, "project: delete load error", err)
		return
	}
	if row == nil || !ownsProject(ar, row) {
		projectNotFound(w)
		return
	}
	if _, err := store.DeleteProject(ctx, h.pool, row.ID); err != nil {
		internalProjectError(w, ctx, "project: delete error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ownsProject reports whether ar may administer the given project: a server-admin
// always, otherwise the tenant-admin of the project's OWN tenant. Mirrors the
// §4.6 write matrix (project create/patch/delete = tenant-admin of own tenant).
func ownsProject(ar *auth.AuthResult, row *store.ProjectRow) bool {
	if ar.IsServerAdmin() {
		return true
	}
	return row.TenantID == ar.TenantID
}

// validIdentity reports whether identity is non-empty and starts with one of the
// closed prefix kinds.
func validIdentity(identity string) bool {
	if identity == "" {
		return false
	}
	for _, p := range identityPrefixes {
		if strings.HasPrefix(identity, p) && len(identity) > len(p) {
			return true
		}
	}
	return false
}

// validateForge returns a non-empty error message if forge carries an api_base
// that violates the §5.7 SSRF deny-list, "" if forge is absent/valid. PATCH-time
// enforcement only (scheme + IP-literal deny-list); DNS-rebinding is the
// dial-time Achse-02 guard (§5.7 point 2), out of this wave's scope.
func validateForge(forge json.RawMessage) string {
	if len(forge) == 0 {
		return ""
	}
	var f struct {
		APIBase string `json:"api_base"`
	}
	if err := json.Unmarshal(forge, &f); err != nil {
		return "forge must be a JSON object"
	}
	if f.APIBase == "" {
		return ""
	}
	u, err := url.Parse(f.APIBase)
	if err != nil {
		return "forge.api_base is not a valid URL"
	}
	if u.Scheme != "https" {
		return "forge.api_base must be https"
	}
	if isDeniedHost(u.Hostname()) {
		return "forge.api_base must not target a private, loopback or link-local address"
	}
	return ""
}

// isDeniedHost reports whether host is a forbidden SSRF target: the literal
// 'localhost', or an IP literal in a private / loopback / link-local /
// unspecified range (RFC1918 + fd00::/8 via IsPrivate, 127/8 + ::1 via
// IsLoopback, 169.254/16 + fe80::/10 via IsLinkLocal*, 0.0.0.0/:: via
// IsUnspecified). A non-IP hostname passes PATCH-time (the dial-time guard
// re-checks the RESOLVED address against DNS rebinding, §5.7).
func isDeniedHost(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// projectNotFound writes the uniform 404 (no existence oracle — unknown id,
// malformed id, foreign scope, foreign tenant all share this body).
func projectNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Project not found"})
}

// internalProjectError logs a store failure with the request id and writes the
// generic 500 envelope (no internal detail on the wire).
func internalProjectError(w http.ResponseWriter, ctx context.Context, msg string, err error) {
	slog.Error(msg, "error", err, "request_id", RequestIDFromContext(ctx))
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
}
