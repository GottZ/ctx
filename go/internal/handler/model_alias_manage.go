// model-alias-* — the admin surface of the /v1 model namespace (W02-04;
// design/02 §3.1, §3.2, §4.4.2; design/05 V-02-a, V-02-c, K-05-2; store and
// registry from W02-03, migration 162).
//
// The four rows are tierTenantAdmin; the field tiers live here:
//
//   - scope: a server-admin writes every scope (a tenant alias names its
//     tenant_id, and its scope must be one of that tenant's scopes); a
//     tenant-admin writes only its own home scope — a _global or foreign alias
//     is "model alias not found", byte-identical to an id that does not exist
//     (the backendWritableByCaller pattern, no existence oracle).
//   - sensitivity: raising is always allowed; lowering (on create: below
//     credentials, on update: below the current value) is server-admin only
//     (403 for a tenant-admin, DECISIONS E3) and needs
//     "confirm_sensitivity_downgrade": true (422 otherwise, the settings
//     guard's wording); the store records the flag in the audit row.
//   - shadowing floor (V-02-c): a tenant alias never sits below the
//     sensitivity of the _global alias of the same name — checked on both
//     write paths (a tenant alias written low, a _global alias created or
//     raised above an existing shadow) under one advisory lock on the name.
//   - policy: parsed fail-closed from the raw request bytes
//     (modelalias.ParsePolicy), then the setting-bound ceilings
//     (modelalias.CheckCeilings) — the tenant ceiling for every alias outside
//     _global, a server-admin's included.
//   - identity: name, kind and scope (and the tenant binding) never change
//     through update — 400 immutable_field with param (V-02-a).
//   - enabled must be sent explicitly on create (K-05-2): a new alias is never
//     live by default.
//
// Removing access is always allowed: a tenant-admin disables or deletes its
// own alias without any gate beyond the scope. Every committed write ends with
// Registry.Trigger (W02-03 R-5: a second chance next to the NOTIFY path).
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/store"
)

// SetModelAliases wires the model-alias registry (Trigger after a committed
// write) and the getter of the setting-bound policy ceilings (cmd/ctxd builds
// it over gateway.*, the same getter the registry clamps with). Boot
// happens-before: called in NewRouter before the server serves. Without the
// getter the writing actions answer 503 — validating a policy against no
// ceiling would be fail-open.
func (h *ManageHandler) SetModelAliases(reg *modelalias.Registry, ceilings func() modelalias.Ceilings) {
	h.modelAliases = reg
	h.aliasCeilings = ceilings
}

const (
	modelAliasNotFound = "model alias not found"
	// modelAliasDescriptionMax mirrors ck_model_aliases_description
	// (migration 162): a longer text is a 422 here, not a 23514 there.
	modelAliasDescriptionMax = 500
)

// The CHECKs of migration 162 (ck_model_aliases_name, ck_model_aliases_role),
// mirrored so a bad value is a 422 at its field instead of a 23514.
var (
	modelAliasNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	modelAliasRoleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9:_-]{0,63}$`)
)

// modelAliasSpec is the create/update payload. Presence is read from the raw
// key set (a JSON null is present, too); the pointers carry the values.
// Policy stays raw: ParsePolicy reads exactly the bytes that were sent
// (a jsonb round trip would normalize duplicates and number spellings away,
// W02-03 F-7).
type modelAliasSpec struct {
	Name                        *string         `json:"name"`
	Kind                        *string         `json:"kind"`
	Scope                       *string         `json:"scope"`
	TenantID                    *string         `json:"tenant_id"`
	Role                        *string         `json:"role"`
	Sensitivity                 *string         `json:"sensitivity"`
	Policy                      json.RawMessage `json:"policy"`
	Description                 *string         `json:"description"`
	Enabled                     *bool           `json:"enabled"`
	ConfirmSensitivityDowngrade bool            `json:"confirm_sensitivity_downgrade"`
}

// modelAliasIdentityKeys are the fields update refuses, in the order the
// refusal names them.
var modelAliasIdentityKeys = []string{"name", "kind", "scope", "tenant_id"}

// modelAliasKeys is the exact key set of the payload. encoding/json matches
// struct fields case-insensitively, while presence (identity refusal,
// "nothing to update", the null check) is read from the exact keys — so a
// "Name" or "Policy" would be decoded but judged absent. Every key outside
// this set is refused before decoding (Fremd-Review F-1).
var modelAliasKeys = map[string]bool{
	"name": true, "kind": true, "scope": true, "tenant_id": true, "role": true, "sensitivity": true,
	"policy": true, "description": true, "enabled": true, "confirm_sensitivity_downgrade": true,
}

// modelAliasNullable: keys whose JSON null is a refusal of its own (a null
// policy reaches ParsePolicy and is refused there, at "policy").
var modelAliasNullable = []string{"role", "sensitivity", "description", "enabled"}

func parseModelAliasSpec(data json.RawMessage) (*modelAliasSpec, map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 || string(bytes.TrimSpace(data)) == "null" {
		return nil, nil, errors.New("data required")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, nil, fmt.Errorf("invalid data: %w", err)
	}
	unknown := make([]string, 0, len(keys))
	for k := range keys {
		if !modelAliasKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, nil, fmt.Errorf("invalid data: unknown field %q", unknown[0])
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s modelAliasSpec
	if err := dec.Decode(&s); err != nil {
		return nil, nil, fmt.Errorf("invalid data: %w", err)
	}
	return &s, keys, nil
}

func writeModelAliasNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": modelAliasNotFound})
}

// writeModelAliasImmutable is V-02-a's refusal: 400 immutable_field, the
// field in param.
func writeModelAliasImmutable(w http.ResponseWriter, field string) {
	writeJSONCode(w, http.StatusBadRequest, "immutable_field", map[string]any{
		"success": false,
		"error":   field + " is immutable — renaming or re-scoping an alias is a new alias: create it, then disable or delete the old one",
		"param":   field,
	})
}

// validTenantAliasScope: a scope a tenant alias may live in — never _global,
// never "" (the pool reads it as _global), never another reserved scope
// (ck_model_aliases_scope, modelalias.validScope).
func validTenantAliasScope(s string) bool {
	return s != "" && !strings.HasPrefix(s, "_")
}

// modelAliasWriteScopes is the scope set of the store's predicate (W02-03
// R-3): nil = every scope, server-admin only; a tenant-admin's own home scope,
// built explicitly; the empty set (never nil) for a caller without a tenant
// home scope — such a caller owns no alias.
func modelAliasWriteScopes(ar *auth.AuthResult) []string {
	if ar.IsServerAdmin() {
		return nil
	}
	if validTenantAliasScope(ar.HomeScope) {
		return []string{ar.HomeScope}
	}
	return []string{}
}

// modelAliasReadScopes: a tenant-admin reads _global (what it may shadow,
// and the floors) and its own scope; a server-admin reads every scope.
func modelAliasReadScopes(ar *auth.AuthResult) []string {
	if ar.IsServerAdmin() {
		return nil
	}
	return append(modelAliasWriteScopes(ar), backends.GlobalScope)
}

// modelAliasWritable mirrors modelAliasWriteScopes for one row (the list's
// writable flag).
func modelAliasWritable(ar *auth.AuthResult, scope string) bool {
	return ar.IsServerAdmin() || (validTenantAliasScope(scope) && scope == ar.HomeScope)
}

func modelAliasView(a store.ModelAlias) map[string]any {
	policy := a.Policy
	if len(policy) == 0 {
		policy = json.RawMessage(`{}`)
	}
	return map[string]any{
		"id": a.ID, "scope": a.Scope, "name": a.Name, "kind": a.Kind, "role": a.Role,
		"sensitivity": a.Sensitivity, "policy": policy, "description": a.Description,
		"enabled": a.Enabled, "created_at": a.CreatedAt, "updated_at": a.UpdatedAt,
	}
}

// modelAliasCeilingsWired returns the current ceilings, or answers 503.
func (h *ManageHandler) modelAliasCeilingsWired(w http.ResponseWriter) (modelalias.Ceilings, bool) {
	if h.aliasCeilings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "model aliases not wired"})
		return modelalias.Ceilings{}, false
	}
	return h.aliasCeilings(), true
}

// afterModelAliasWrite asks the registry for a reload once the write has
// committed. The NOTIFY of the write reaches the listener as well; this is
// the second chance after a failed listener reload (W02-03 R-5). Trigger is
// single-flight and returns at once; its reload must outlive the request, so
// it runs on a context without the request's cancellation.
func (h *ManageHandler) afterModelAliasWrite(ctx context.Context) {
	if h.modelAliases != nil {
		h.modelAliases.Trigger(context.WithoutCancel(ctx))
	}
}

// Field validation.

func fieldErr(field, msg string) backends.FieldError {
	return backends.FieldError{Field: field, Message: msg}
}

func modelAliasNameErr(v *string) *backends.FieldError {
	switch {
	case v == nil:
		e := fieldErr("name", "required")
		return &e
	case !modelAliasNameRE.MatchString(*v):
		e := fieldErr("name", `want 1-128 characters of [A-Za-z0-9._:/-], starting with a letter or digit`)
		return &e
	}
	return nil
}

func modelAliasKindErr(v *string) *backends.FieldError {
	if v == nil {
		e := fieldErr("kind", "required")
		return &e
	}
	switch modelalias.Kind(*v) {
	case modelalias.KindChat, modelalias.KindEmbedding, modelalias.KindRerank:
		return nil
	}
	e := fieldErr("kind", `want "chat", "embedding" or "rerank"`)
	return &e
}

func modelAliasRoleErr(v *string) *backends.FieldError {
	switch {
	case v == nil:
		e := fieldErr("role", "required")
		return &e
	case !modelAliasRoleRE.MatchString(*v):
		e := fieldErr("role", "want a pool role: 1-64 characters of [a-z0-9:_-], starting with a letter or digit")
		return &e
	}
	return nil
}

func modelAliasSensitivityErr(v string) *backends.FieldError {
	switch backends.Sensitivity(v) {
	case backends.SensPublic, backends.SensInternal, backends.SensPersonal, backends.SensCredentials:
		return nil
	}
	e := fieldErr("sensitivity", `want "public", "internal", "personal" or "credentials"`)
	return &e
}

// modelAliasDescriptionErr: valid UTF-8 without NUL (a Postgres text value
// cannot hold one — it would be a 22021 and a 500), at most
// modelAliasDescriptionMax characters.
func modelAliasDescriptionErr(v string) *backends.FieldError {
	if !utf8.ValidString(v) || strings.ContainsRune(v, 0) || utf8.RuneCountInString(v) > modelAliasDescriptionMax {
		e := fieldErr("description", fmt.Sprintf("want valid UTF-8 without NUL, at most %d characters", modelAliasDescriptionMax))
		return &e
	}
	return nil
}

// policyField names the 422 field of a policy refusal: "policy" for the
// document, "policy.<key>" for one key (keys carry dots themselves, e.g.
// policy.tools.max_count — strip the prefix to address the form field).
func policyField(key string) string {
	if key == "" {
		return "policy"
	}
	return "policy." + key
}

// modelAliasPolicyErrs parses raw for an alias of kind and checks the
// setting-bound ceilings (tenant: the alias lies outside _global).
func modelAliasPolicyErrs(kind string, raw json.RawMessage, c modelalias.Ceilings, tenant bool) []backends.FieldError {
	p, err := modelalias.ParsePolicy(modelalias.Kind(kind), raw)
	if err != nil {
		var pe *modelalias.PolicyError
		if errors.As(err, &pe) {
			return []backends.FieldError{fieldErr(policyField(pe.Key), pe.Reason)}
		}
		return []backends.FieldError{fieldErr("policy", err.Error())}
	}
	var out []backends.FieldError
	for _, pe := range modelalias.CheckCeilings(p, c, tenant) {
		out = append(out, fieldErr(policyField(pe.Key), pe.Reason))
	}
	return out
}

// nullFieldErrs refuses a JSON null on a field that has no null form: on
// create omitting means the default, on update keeping the value.
func nullFieldErrs(keys map[string]json.RawMessage) []backends.FieldError {
	var out []backends.FieldError
	for _, k := range modelAliasNullable {
		if raw, ok := keys[k]; ok && string(bytes.TrimSpace(raw)) == "null" {
			out = append(out, fieldErr(k, "null is no value — omit the key instead"))
		}
	}
	return out
}

func appendFieldErr(errs []backends.FieldError, e *backends.FieldError) []backends.FieldError {
	if e != nil {
		return append(errs, *e)
	}
	return errs
}

// buildModelAliasCreate validates a create payload for scope and returns the
// row to insert.
func buildModelAliasCreate(spec *modelAliasSpec, keys map[string]json.RawMessage, scope string, c modelalias.Ceilings) (store.ModelAlias, []backends.FieldError) {
	errs := nullFieldErrs(keys)
	if len(errs) > 0 {
		return store.ModelAlias{}, errs
	}
	errs = appendFieldErr(errs, modelAliasNameErr(spec.Name))
	kindErr := modelAliasKindErr(spec.Kind)
	errs = appendFieldErr(errs, kindErr)
	errs = appendFieldErr(errs, modelAliasRoleErr(spec.Role))
	a := store.ModelAlias{Scope: scope, Sensitivity: string(backends.SensCredentials), Policy: json.RawMessage(`{}`)}
	if spec.Sensitivity != nil {
		errs = appendFieldErr(errs, modelAliasSensitivityErr(*spec.Sensitivity))
		a.Sensitivity = *spec.Sensitivity
	}
	if spec.Description != nil {
		errs = appendFieldErr(errs, modelAliasDescriptionErr(*spec.Description))
		a.Description = *spec.Description
	}
	if spec.Enabled == nil {
		errs = append(errs, fieldErr("enabled", "required — send true or false; a new alias is never live by default"))
	} else {
		a.Enabled = *spec.Enabled
	}
	if kindErr == nil {
		policy := spec.Policy
		if policy == nil {
			policy = json.RawMessage(`{}`)
		}
		errs = append(errs, modelAliasPolicyErrs(*spec.Kind, policy, c, scope != backends.GlobalScope)...)
		a.Policy = policy
	}
	if len(errs) > 0 {
		return store.ModelAlias{}, errs
	}
	a.Name, a.Kind, a.Role = *spec.Name, *spec.Kind, *spec.Role
	return a, nil
}

// applyModelAliasPatch overlays an update payload onto cur. policy replaces
// the whole object; an absent key keeps its value. The stored policy is not
// re-judged when the payload leaves it alone — a ceiling lowered after the
// write is the reload clamp's business, and must not block a disable.
func applyModelAliasPatch(cur store.ModelAlias, spec *modelAliasSpec, keys map[string]json.RawMessage, c modelalias.Ceilings) (store.ModelAlias, []backends.FieldError) {
	errs := nullFieldErrs(keys)
	if len(errs) > 0 {
		return cur, errs
	}
	next := cur
	if spec.Role != nil {
		errs = appendFieldErr(errs, modelAliasRoleErr(spec.Role))
		next.Role = *spec.Role
	}
	if spec.Sensitivity != nil {
		errs = appendFieldErr(errs, modelAliasSensitivityErr(*spec.Sensitivity))
		next.Sensitivity = *spec.Sensitivity
	}
	if spec.Description != nil {
		errs = appendFieldErr(errs, modelAliasDescriptionErr(*spec.Description))
		next.Description = *spec.Description
	}
	if spec.Enabled != nil {
		next.Enabled = *spec.Enabled
	}
	if _, ok := keys["policy"]; ok {
		errs = append(errs, modelAliasPolicyErrs(cur.Kind, spec.Policy, c, cur.Scope != backends.GlobalScope)...)
		next.Policy = spec.Policy
	}
	return next, errs
}

// modelAliasRoleCheck is the kind↔role write check (design/02 §3.1): an
// embedding alias needs every backend row visible to its scope that carries
// the role to speak openai or ollama (422 at "role" otherwise — the
// protocols are named, never the rows); a role no visible row carries is
// legal but answers with a warning. Rows count whether enabled or not — the
// configuration is the contract, enabled is runtime state.
func modelAliasRoleCheck(pool *backends.Pool, kind, role, scope string) ([]string, *backends.FieldError) {
	var rows []backends.Backend
	if pool != nil {
		rows = pool.Snapshot()
	}
	carried := 0
	bad := map[string]struct{}{}
	for i := range rows {
		b := &rows[i]
		if !b.HasRole(role) || !backends.VisibleTo(b.Scope, scope) {
			continue
		}
		carried++
		if modelalias.Kind(kind) == modelalias.KindEmbedding &&
			b.Protocol != backends.ProtocolOpenAI && b.Protocol != backends.ProtocolOllama {
			bad[string(b.Protocol)] = struct{}{}
		}
	}
	if len(bad) > 0 {
		protocols := make([]string, 0, len(bad))
		for p := range bad {
			protocols = append(protocols, p)
		}
		sort.Strings(protocols)
		e := fieldErr("role", fmt.Sprintf("role %q is carried by backend rows of protocol %s, which cannot serve embeddings (openai or ollama only)",
			role, strings.Join(protocols, ", ")))
		return nil, &e
	}
	if carried == 0 {
		return []string{fmt.Sprintf("no backend row visible to scope %q carries role %q — the alias serves nothing until one does", scope, role)}, nil
	}
	return nil, nil
}

// Scope and sensitivity tiers.

// modelAliasCreateScope resolves the scope of a new alias, or answers.
// A tenant-admin creates in its own home scope only (payload scope absent or
// equal to it); _global, a foreign or an unknown scope answer "model alias
// not found" — one body for all of them. A server-admin creates _global by
// default, a tenant alias with tenant_id + one of that tenant's scopes.
func (h *ManageHandler) modelAliasCreateScope(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, spec *modelAliasSpec) (string, bool) {
	if !ar.IsServerAdmin() {
		if spec.TenantID != nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "tenant_id is server-admin only"})
			return "", false
		}
		home := ar.HomeScope
		if !validTenantAliasScope(home) || (spec.Scope != nil && *spec.Scope != home) {
			writeModelAliasNotFound(w)
			return "", false
		}
		return home, true
	}
	scope := backends.GlobalScope
	if spec.Scope != nil {
		scope = *spec.Scope
	}
	if spec.TenantID == nil {
		if scope != backends.GlobalScope {
			writeBackendValidation(w, []backends.FieldError{fieldErr("tenant_id",
				"required for a tenant alias — the scope must be one of that tenant's scopes")})
			return "", false
		}
		return scope, true
	}
	return h.modelAliasTenantScope(w, r, *spec.TenantID, scope)
}

// modelAliasTenantScope checks a server-admin's tenant alias: scope is a
// tenant scope of tenantID (context_tenant_scopes).
func (h *ManageHandler) modelAliasTenantScope(w http.ResponseWriter, r *http.Request, tenantID, scope string) (string, bool) {
	if !validTenantAliasScope(scope) {
		writeBackendValidation(w, []backends.FieldError{fieldErr("scope", "a tenant alias needs one of the tenant's scopes")})
		return "", false
	}
	u, err := uuid.Parse(tenantID)
	if err != nil {
		writeBackendValidation(w, []backends.FieldError{fieldErr("scope",
			fmt.Sprintf("scope %q is not a scope of tenant %q", scope, tenantID))})
		return "", false
	}
	// uuid.Parse also takes the URN and brace spellings; the lookup gets the
	// canonical form, which $1::uuid always accepts (Fremd-Review F-2).
	owned, err := store.TenantScopes(r.Context(), h.pool, u.String())
	if err != nil {
		internalError(w, r.Context(), "model-alias-create: tenant scopes lookup failed", err)
		return "", false
	}
	for _, s := range owned {
		if s == scope {
			return scope, true
		}
	}
	writeBackendValidation(w, []backends.FieldError{fieldErr("scope",
		fmt.Sprintf("scope %q is not a scope of tenant %q", scope, tenantID))})
	return "", false
}

// authorizeModelAliasLowering is the sensitivity tier: lowering from → to is
// server-admin + confirm. It returns the confirm flag the write records (true
// only for a confirmed lowering) or false after answering.
func authorizeModelAliasLowering(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, confirm bool, from, to backends.Sensitivity) (bool, bool) {
	if to.Rank() >= from.Rank() {
		return false, true
	}
	if !ar.IsServerAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "admin key required"})
		return false, false
	}
	if !confirm {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false,
			"error": fmt.Sprintf(
				"validation: sensitivity: lowering %s → %s marks prompts sent to this model alias as eligible for lower-trust backends — repeat with \"confirm_sensitivity_downgrade\": true",
				from, to),
		})
		return false, false
	}
	slog.Warn("model-alias: confirmed sensitivity downgrade",
		"from", string(from), "to", string(to), "request_id", RequestIDFromContext(r.Context()))
	return true, true
}

// checkModelAliasFloor holds V-02-c inside tx, under the name lock: a tenant
// alias may not sit below the _global alias of its name, and a _global alias
// may not rise above a tenant alias that shadows it. Returns false after
// answering (422 at "sensitivity", or 500).
func checkModelAliasFloor(ctx context.Context, w http.ResponseWriter, tx pgx.Tx, scope, name string, sens backends.Sensitivity) bool {
	if scope != backends.GlobalScope {
		floor, found, err := store.GlobalModelAliasSensitivity(ctx, tx, name)
		if err != nil {
			internalError(w, ctx, "model-alias: floor read failed", err)
			return false
		}
		if found && sens.Rank() < backends.Sensitivity(floor).Rank() {
			writeBackendValidation(w, []backends.FieldError{fieldErr("sensitivity", fmt.Sprintf(
				"%s is below %s, the sensitivity of the _global alias %q this alias shadows — a shadow may redirect, never lower the floor",
				sens, floor, name))})
			return false
		}
		return true
	}
	shadows, err := store.ModelAliasShadows(ctx, tx, name)
	if err != nil {
		internalError(w, ctx, "model-alias: shadow read failed", err)
		return false
	}
	var below []string
	for _, s := range shadows {
		if backends.Sensitivity(s.Sensitivity).Rank() < sens.Rank() {
			below = append(below, s.Scope+"="+s.Sensitivity)
		}
	}
	if len(below) > 0 {
		writeBackendValidation(w, []backends.FieldError{fieldErr("sensitivity", fmt.Sprintf(
			"%d tenant alias(es) named %q would sit below %s (%s) — raise or delete them first",
			len(below), name, sens, strings.Join(below, ", ")))})
		return false
	}
	return true
}

// lockedModelAlias reads the alias id under the caller's write scopes, takes
// the name lock and reads it again — the second read is the state every other
// alias write of that name has committed (the lock serializes them), so the
// sensitivity comparison never runs on a stale row. Not found (missing,
// malformed, foreign, _global for a tenant-admin) answers 404.
func lockedModelAlias(ctx context.Context, w http.ResponseWriter, tx pgx.Tx, id string, scopes []string) (store.ModelAlias, bool) {
	cur, found, err := store.GetModelAlias(ctx, tx, id, scopes)
	if err == nil && found {
		if err = store.LockModelAliasName(ctx, tx, cur.Name); err == nil {
			cur, found, err = store.GetModelAlias(ctx, tx, id, scopes)
		}
	}
	switch {
	case err != nil:
		internalError(w, ctx, "model-alias: read failed", err)
		return store.ModelAlias{}, false
	case !found:
		writeModelAliasNotFound(w)
		return store.ModelAlias{}, false
	}
	return cur, true
}

// writeModelAliasStoreErr answers a failed store write.
func writeModelAliasStoreErr(ctx context.Context, w http.ResponseWriter, op string, err error) {
	var imm *store.ModelAliasImmutableError
	switch {
	case errors.As(err, &imm):
		writeModelAliasImmutable(w, imm.Field)
	case errors.Is(err, store.ErrModelAliasScope):
		writeModelAliasNotFound(w)
	case errors.Is(err, store.ErrModelAliasExists):
		writeJSONReject(w, classConstraint.rejectStatus(http.StatusConflict, "a model alias of that name already exists in the scope"))
	default:
		internalError(w, ctx, "model-alias-"+op+": store write failed", err)
	}
}

func writeModelAliasOK(w http.ResponseWriter, a store.ModelAlias, warnings []string) {
	resp := map[string]any{"success": true, "model_alias": modelAliasView(a)}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}

func loweredWarning(from, to backends.Sensitivity) string {
	return fmt.Sprintf("sensitivity lowered %s → %s (confirmed)", from, to)
}

// The four actions.

func (h *ManageHandler) handleModelAliasCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	ceilings, ok := h.modelAliasCeilingsWired(w)
	if !ok {
		return
	}
	spec, keys, err := parseModelAliasSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	scope, ok := h.modelAliasCreateScope(w, r, ar, spec)
	if !ok {
		return
	}
	a, errs := buildModelAliasCreate(spec, keys, scope, ceilings)
	if len(errs) > 0 {
		writeBackendValidation(w, errs)
		return
	}
	warnings, fe := modelAliasRoleCheck(h.backendPool, a.Kind, a.Role, a.Scope)
	if fe != nil {
		writeBackendValidation(w, []backends.FieldError{*fe})
		return
	}
	from, to := backends.SensCredentials, backends.Sensitivity(a.Sensitivity)
	var out store.ModelAlias
	if !answeredTx(ctx, h.pool, func(stage string, _ error) { writeBackendTxFailure(w, stage) }, func(tx pgx.Tx) bool {
		if err := store.LockModelAliasName(ctx, tx, a.Name); err != nil {
			internalError(w, ctx, "model-alias-create: name lock failed", err)
			return false
		}
		if !checkModelAliasFloor(ctx, w, tx, a.Scope, a.Name, to) {
			return false
		}
		confirmed, ok := authorizeModelAliasLowering(w, r, ar, spec.ConfirmSensitivityDowngrade, from, to)
		if !ok {
			return false
		}
		if confirmed {
			warnings = append(warnings, loweredWarning(from, to))
		}
		out, err = store.CreateModelAlias(ctx, tx, a, modelAliasWriteScopes(ar),
			store.ModelAliasWrite{Stamp: auditStamp(r, ar), ConfirmSensitivityDowngrade: confirmed})
		if err != nil {
			writeModelAliasStoreErr(ctx, w, "create", err)
			return false
		}
		return true
	}) {
		return
	}
	h.afterModelAliasWrite(ctx)
	writeModelAliasOK(w, out, warnings)
}

func (h *ManageHandler) handleModelAliasUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	ceilings, ok := h.modelAliasCeilingsWired(w)
	if !ok {
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	spec, keys, err := parseModelAliasSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	for _, k := range modelAliasIdentityKeys {
		if _, present := keys[k]; present {
			writeModelAliasImmutable(w, k)
			return
		}
	}
	if !modelAliasPatchHasField(keys) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "nothing to update"})
		return
	}
	var (
		out      store.ModelAlias
		warnings []string
	)
	if !answeredTx(ctx, h.pool, func(stage string, _ error) { writeBackendTxFailure(w, stage) }, func(tx pgx.Tx) bool {
		var ok bool
		out, warnings, ok = h.updateModelAliasTx(ctx, w, r, tx, ar, req.ID, spec, keys, ceilings)
		return ok
	}) {
		return
	}
	h.afterModelAliasWrite(ctx)
	writeModelAliasOK(w, out, warnings)
}

// modelAliasPatchHasField: the update names at least one mutable field.
func modelAliasPatchHasField(keys map[string]json.RawMessage) bool {
	for _, k := range []string{"role", "sensitivity", "policy", "description", "enabled"} {
		if _, ok := keys[k]; ok {
			return true
		}
	}
	return false
}

// updateModelAliasTx is the transactional body of model-alias-update.
func (h *ManageHandler) updateModelAliasTx(ctx context.Context, w http.ResponseWriter, r *http.Request, tx pgx.Tx, ar *auth.AuthResult,
	id string, spec *modelAliasSpec, keys map[string]json.RawMessage, c modelalias.Ceilings,
) (store.ModelAlias, []string, bool) {
	scopes := modelAliasWriteScopes(ar)
	cur, ok := lockedModelAlias(ctx, w, tx, id, scopes)
	if !ok {
		return store.ModelAlias{}, nil, false
	}
	next, errs := applyModelAliasPatch(cur, spec, keys, c)
	if len(errs) > 0 {
		writeBackendValidation(w, errs)
		return store.ModelAlias{}, nil, false
	}
	var warnings []string
	if next.Role != cur.Role {
		var fe *backends.FieldError
		if warnings, fe = modelAliasRoleCheck(h.backendPool, next.Kind, next.Role, next.Scope); fe != nil {
			writeBackendValidation(w, []backends.FieldError{*fe})
			return store.ModelAlias{}, nil, false
		}
	}
	from, to := backends.Sensitivity(cur.Sensitivity), backends.Sensitivity(next.Sensitivity)
	tenantLowers := next.Scope != backends.GlobalScope && to.Rank() < from.Rank()
	globalRises := next.Scope == backends.GlobalScope && to.Rank() > from.Rank()
	if (tenantLowers || globalRises) && !checkModelAliasFloor(ctx, w, tx, next.Scope, next.Name, to) {
		return store.ModelAlias{}, nil, false
	}
	confirmed, ok := authorizeModelAliasLowering(w, r, ar, spec.ConfirmSensitivityDowngrade, from, to)
	if !ok {
		return store.ModelAlias{}, nil, false
	}
	if confirmed {
		warnings = append(warnings, loweredWarning(from, to))
	}
	out, found, err := store.UpdateModelAlias(ctx, tx, next, scopes,
		store.ModelAliasWrite{Stamp: auditStamp(r, ar), ConfirmSensitivityDowngrade: confirmed})
	switch {
	case err != nil:
		writeModelAliasStoreErr(ctx, w, "update", err)
		return store.ModelAlias{}, nil, false
	case !found:
		writeModelAliasNotFound(w)
		return store.ModelAlias{}, nil, false
	}
	return out, warnings, true
}

func (h *ManageHandler) handleModelAliasDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	var out store.ModelAlias
	if !answeredTx(ctx, h.pool, func(stage string, _ error) { writeBackendTxFailure(w, stage) }, func(tx pgx.Tx) bool {
		var (
			found bool
			err   error
		)
		out, found, err = store.DeleteModelAlias(ctx, tx, req.ID, modelAliasWriteScopes(ar), store.ModelAliasWrite{Stamp: auditStamp(r, ar)})
		switch {
		case err != nil:
			writeModelAliasStoreErr(ctx, w, "delete", err)
			return false
		case !found:
			writeModelAliasNotFound(w)
			return false
		}
		return true
	}) {
		return
	}
	h.afterModelAliasWrite(ctx)
	writeModelAliasOK(w, out, nil)
}

func (h *ManageHandler) handleModelAliasList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	rows, err := store.ListModelAliases(r.Context(), h.pool, modelAliasReadScopes(ar))
	if err != nil {
		internalError(w, r.Context(), "model-alias-list: read failed", err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		v := modelAliasView(a)
		v["writable"] = modelAliasWritable(ar, a.Scope)
		items = append(items, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "model_aliases": items, "count": len(items)})
}
