// /api/settings — the F2-W5 runtime-settings API (design 02-§3.4).
//
// GET    /api/settings        effective configuration, UI-ready (registry
//                             metadata + value + source per key)
// GET    /api/settings/{key}  single key + recent audit rows
// PUT    /api/settings/{key}  set a DB override (validated BEFORE persist)
// DELETE /api/settings/{key}  remove the override (revert to env/default)
//
// All routes are admin-gated (Auth → RequireAdmin, mounted in server.go; the
// negative probe in settings_test.go was red against the ungated chain).
//
// Masking rule (§3.4, generic over ALL response surfaces): every spot that
// carries the effective value of a sensitive key renders "(set via env)" when
// the source is env — GET value, PUT previous.value and DELETE value (the
// post-revert effective value). A DB-sourced sensitive value renders as the
// secret_ref NAME from the override row, never from the snapshot (the
// snapshot holds the RESOLVED plaintext). Pinned by the leak-scan tests.

package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maskedEnvValue is the §3.4 placeholder for env-sourced sensitive values.
// The .env plaintext (e.g. CTX_CHAT_API_KEY) must never travel through a
// settings response — the standard migration flow (PUT secret_ref over an
// env-set key) would otherwise leak it via previous.value.
const maskedEnvValue = "(set via env)"

// auditLimit caps the history rows on GET /api/settings/{key}.
const auditLimit = 10

// SettingsHandler implements the /api/settings routes.
type SettingsHandler struct {
	pool *pgxpool.Pool
	cfg  *config.Store
	// reload is settings.Reload bound to (pool, cfg) — a function field so
	// unit tests can observe/stub the post-commit reload without a DB.
	reload func(r *http.Request) error
}

// NewSettingsHandler creates a new SettingsHandler.
func NewSettingsHandler(pool *pgxpool.Pool, cfg *config.Store) *SettingsHandler {
	h := &SettingsHandler{pool: pool, cfg: cfg}
	h.reload = func(r *http.Request) error {
		return settings.Reload(r.Context(), pool, cfg)
	}
	return h
}

// MountSettings mounts the /api/settings routes behind RequireAdminOrTenantAdmin
// (03-W5): a tenant-admin manages its OWN tenant scope, an operator manages
// _global; a member or unauthenticated caller still gets 403. The gate only
// ADMITS — the handler scopes every read/write to writeScope/readScopes, so the
// looser gate never leaks a foreign tenant's config (the §4.1a fail-closed
// order). ONE function used by server.go and the gate tests, so the 403 probe
// exercises exactly the chain production mounts.
func MountSettings(r chi.Router, h *SettingsHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireAdminOrTenantAdmin)
		r.Get("/api/settings", h.HandleList)
		r.Get("/api/settings/{key}", h.HandleGet)
		r.Put("/api/settings/{key}", h.HandlePut)
		r.Delete("/api/settings/{key}", h.HandleDelete)
	})
}

// settingView is one key in the GET /api/settings rendering.
type settingView struct {
	Key        string `json:"key"`
	EnvVar     string `json:"env_var,omitempty"`
	Type       string `json:"type"`
	Mutability string `json:"mutability"`
	Value      any    `json:"value"`
	Source     string `json:"source"`
	Default    any    `json:"default"`
	Sensitive  bool   `json:"sensitive,omitempty"`
}

// apiSource maps the snapshot source vocabulary to the API one: the override
// layer is called "db" on the wire (§3.4), "settings" internally.
func apiSource(s string) string {
	if s == "settings" {
		return "db"
	}
	if s == "" {
		return "default"
	}
	return s
}

// renderEffective renders one key's effective value under the §3.4 masking
// rule. overrides maps key → raw override row value (only consulted for
// DB-sourced sensitive keys, where the row carries the harmless secret_ref
// name while the snapshot carries the resolved plaintext).
func renderEffective(c *config.Config, info config.KeyInfo, overrides map[string]json.RawMessage) any {
	if !info.Sensitive {
		v, _ := config.RenderValue(c, info.Key)
		return v
	}
	switch c.Source(info.Key) {
	case "settings", config.SourceTenant:
		if raw, ok := overrides[info.Key]; ok {
			if name, err := settings.ScalarValue(raw); err == nil {
				return name
			}
		}
		return "set" // row vanished between load and render — presence floor
	case "env":
		return maskedEnvValue
	default:
		return "" // secret-class defaults are empty by registry construction
	}
}

// loadOverrideMap loads the override rows for the effective-view scopes keyed by
// settings key, keeping the HIGHEST-precedence scope's value per key (03-W5
// §4.5): for a tenant readScopes={_global, tenant} a tenant override row wins, so
// the rendered secret_ref name is the tenant's, not _global's. Precedence is
// materialized in Go (the scope's position in scopes), not trusted to the SQL
// ORDER BY.
func (h *SettingsHandler) loadOverrideMap(r *http.Request, scopes []string) (map[string]json.RawMessage, error) {
	rows, err := store.LoadSettingOverridesMulti(r.Context(), h.pool, scopes)
	if err != nil {
		return nil, err
	}
	return overrideMapByPrecedence(rows, scopes), nil
}

// overrideMapByPrecedence keeps, per key, the value of the HIGHEST-precedence
// scope present (the scope's position in scopes is its priority). Materialized in
// Go, not trusted to the SQL ORDER BY — the effective secret_ref name a tenant
// renders must be its own, never _global's.
func overrideMapByPrecedence(rows []store.SettingOverride, scopes []string) map[string]json.RawMessage {
	prio := func(scope string) int { return slices.Index(scopes, scope) }
	best := make(map[string]int, len(rows))
	m := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		if p, seen := best[row.Key]; seen && prio(row.Scope) <= p {
			continue
		}
		best[row.Key] = prio(row.Scope)
		m[row.Key] = row.Value
	}
	return m
}

// HandleList implements GET /api/settings.
func (h *SettingsHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	overrides, err := h.loadOverrideMap(r, readScopes(ar))
	if err != nil {
		h.internalError(w, r, "settings: list overrides failed", err)
		return
	}
	c := h.cfg.SnapshotForRequest(r.Context()) // effective tenant>global view (03-W5 §4.5)

	infos := config.Keys()
	views := make([]settingView, 0, len(infos))
	for _, info := range infos {
		views = append(views, settingView{
			Key:        info.Key,
			EnvVar:     info.EnvVar,
			Type:       info.Type,
			Mutability: info.Mutability,
			Value:      renderEffective(c, info, overrides),
			Source:     apiSource(c.Source(info.Key)),
			Default:    info.Default,
			Sensitive:  info.Sensitive,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "settings": views})
}

// HandleGet implements GET /api/settings/{key}.
func (h *SettingsHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	info, ok := config.KeyByName(key)
	if !ok {
		h.unknownKey(w, key)
		return
	}
	ar := AuthResultFromContext(r.Context())
	overrides, err := h.loadOverrideMap(r, readScopes(ar))
	if err != nil {
		h.internalError(w, r, "settings: load overrides failed", err)
		return
	}
	// Audit reads the history of the LEVEL one writes (writeScope): a tenant-admin
	// its own scope, an operator _global (§4.5). The trigger spiegelt the scope.
	audit, err := store.ListSettingAudit(r.Context(), h.pool, key, writeScope(ar), auditLimit)
	if err != nil {
		h.internalError(w, r, "settings: load audit failed", err)
		return
	}
	c := h.cfg.SnapshotForRequest(r.Context()) // effective tenant>global view (03-W5 §4.5)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"setting": settingView{
			Key:        info.Key,
			EnvVar:     info.EnvVar,
			Type:       info.Type,
			Mutability: info.Mutability,
			Value:      renderEffective(c, info, overrides),
			Source:     apiSource(c.Source(info.Key)),
			Default:    info.Default,
			Sensitive:  info.Sensitive,
		},
		"audit": audit,
	})
}

// HandlePut implements PUT /api/settings/{key}. Validation runs BEFORE
// persist by building the candidate config from the would-be row set through
// the same path the reload uses (settings.BuildFromRows) — a key whose
// override would be ignored or dropped is a 422, never a silent no-op row.
func (h *SettingsHandler) HandlePut(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	info, ok := config.KeyByName(key)
	if !ok {
		h.unknownKey(w, key)
		return
	}
	if msg, blocked := mutabilityBlock(info); blocked {
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": msg})
		return
	}

	var body struct {
		Value json.RawMessage `json:"value"`
		// ConfirmSensitivityDowngrade unlocks LOWERING a guarded
		// pool.default_*_sensitivity key (F3 §3.5): one wrong write on
		// 'public' would mark ALL new unclassified blocks external-eligible.
		ConfirmSensitivityDowngrade bool `json:"confirm_sensitivity_downgrade"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid JSON body"})
		return
	}
	raw, err := settings.ScalarValue(body.Value)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false, "error": fmt.Sprintf("validation: %s: %v", key, err)})
		return
	}
	if info.Type == "bool" && raw != "true" && raw != "false" {
		// The legacy-exact bool parser would silently turn anything but
		// "true" into false — API input gets the strict check instead.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false, "error": fmt.Sprintf("validation: %s must be true or false", key)})
		return
	}
	// Two-write-worlds scope resolution (§4.1a/§4.5): the write targets writeScope
	// (operator → _global, tenant-admin → own scope), the candidate validates the
	// effective readScopes view, and secret_refs resolve at the write scope with
	// the tenant's gated _global fallback. allowShared is read at the tenant scope
	// (operator never needs it — _global resolution ignores it).
	ar := AuthResultFromContext(r.Context())
	ws := writeScope(ar)
	rs := readScopes(ar)
	allowShared := false
	if ws != store.GlobalScope {
		var serr error
		allowShared, serr = store.TenantAllowsSharedSecrets(r.Context(), h.pool, ws)
		if serr != nil {
			h.internalError(w, r, "settings: shared-secrets opt-in check failed", serr)
			return
		}
	}

	if info.Sensitive {
		if code, msg := h.checkSecretRef(r, key, raw, ar, allowShared); msg != "" {
			writeJSON(w, code, map[string]any{"success": false, "error": msg})
			return
		}
	}
	normalized := normalizedJSON(info, raw)

	// Candidate build: current rows with this write mixed in, through the exact
	// reload path. Applied == the override took effect from the DB layer — the
	// operator's _global ("settings") OR a tenant's own scope ("tenant"); anything
	// else means it would be ignored — reject with the build's reason. A
	// tenant-scope row on a global-only key (a tenant self-granting
	// allow_shared_secrets) is dropped by the toOverrides gate here → not applied
	// → 422: the fail-closed self-grant block.
	baseRows, err := store.LoadSettingOverridesMulti(r.Context(), h.pool, rs)
	if err != nil {
		h.internalError(w, r, "settings: load overrides failed", err)
		return
	}
	candRows := upsertRow(baseRows, key, normalized, ws)
	candidate, candIssues := settings.BuildFromRowsScoped(r.Context(), h.pool, candRows, rs, ws, allowShared)
	if !appliedFromDB(candidate.Source(key)) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false,
			"error":   fmt.Sprintf("validation: %s: %s", key, issueFor(candIssues, key)),
		})
		return
	}
	_, baseIssues := settings.BuildFromRowsScoped(r.Context(), h.pool, baseRows, rs, ws, allowShared)
	warnings := newWarnings(candIssues, baseIssues)
	warnings = append(warnings, pairingWarnings(key, candidate)...)

	// Previous effective value + source, masked, BEFORE the swap. The override
	// map is precedence-aware so a tenant's own ref name shows, not _global's.
	prevOverrides := overrideMapByPrecedence(baseRows, rs)
	prev := h.cfg.SnapshotForRequest(r.Context()) // pre-swap effective tenant>global view (03-W5 §4.5)
	previous := map[string]any{
		"value":  renderEffective(prev, info, prevOverrides),
		"source": apiSource(prev.Source(key)),
	}

	// Guarded sensitivity defaults (F3 §3.5): LOWERING needs an explicit
	// confirm — the value is already type-valid (candidate build passed), so
	// the rank comparison is sound. Raising stays free.
	if info.Guard == "sensitivity-downgrade" {
		curVal, _ := config.RenderValue(prev, key)
		curSens := backends.Sensitivity(fmt.Sprint(curVal))
		newSens := backends.Sensitivity(raw)
		if newSens.Rank() < curSens.Rank() {
			if !body.ConfirmSensitivityDowngrade {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"success": false,
					"error": fmt.Sprintf(
						"validation: %s: lowering %s → %s marks new unclassified content as eligible for lower-trust backends — repeat with \"confirm_sensitivity_downgrade\": true",
						key, curSens, newSens),
				})
				return
			}
			slog.Warn("settings: confirmed sensitivity default downgrade",
				"key", key, "from", string(curSens), "to", string(newSens),
				"request_id", RequestIDFromContext(r.Context()))
			warnings = append(warnings, fmt.Sprintf(
				"sensitivity default lowered %s → %s (confirmed)", curSens, newSens))
		}
	}

	if err := h.writeSetting(r, key, normalized, ws); err != nil {
		h.internalError(w, r, "settings: persist failed", err)
		return
	}
	if err := h.reload(r); err != nil {
		// Persisted but not live — never report that as plain success.
		h.internalError(w, r, "settings: persisted but reload failed", err)
		return
	}

	// reload Replaced the _global base and wiped the tenant cache, so this
	// SnapshotForRequest rebuilds the caller's generation with the fresh row.
	now := h.cfg.SnapshotForRequest(r.Context())
	nowOverrides := prevOverrides
	nowOverrides[key] = normalized
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"key":      key,
		"value":    renderEffective(now, info, nowOverrides),
		"source":   apiSource(now.Source(key)),
		"previous": previous,
		"warnings": warnings,
	})
}

// HandleDelete implements DELETE /api/settings/{key} — remove the override,
// revert to env/default. The response carries the post-revert effective
// value (masked for sensitive keys, §3.4).
func (h *SettingsHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	info, ok := config.KeyByName(key)
	if !ok {
		h.unknownKey(w, key)
		return
	}

	found, err := h.deleteSetting(r, key, writeScope(AuthResultFromContext(r.Context())))
	if err != nil {
		h.internalError(w, r, "settings: delete failed", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": fmt.Sprintf("no override set for %s", key)})
		return
	}
	if err := h.reload(r); err != nil {
		h.internalError(w, r, "settings: deleted but reload failed", err)
		return
	}

	c := h.cfg.SnapshotForRequest(r.Context()) // post-revert effective view (03-W5 §4.5)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"key":     key,
		"value":   renderEffective(c, info, nil), // post-revert: source is env/default
		"source":  apiSource(c.Source(key)),
	})
}

// checkSecretRef gates sensitive-key values (§2.3): format first, then
// existence in context_secrets — a plaintext provider key set as the value
// would otherwise land verbatim in context_settings AND the append-only
// audit. Returns (status, message); empty message = pass.
func (h *SettingsHandler) checkSecretRef(r *http.Request, key, raw string, ar *auth.AuthResult, allowShared bool) (int, string) {
	if !store.ValidSecretName(raw) {
		return http.StatusUnprocessableEntity, fmt.Sprintf(
			"validation: %s takes a secret NAME (lowercase [a-z0-9._-], max 128 chars), never the secret value — create the secret first", key)
	}
	// The gate follows the RESOLVER (§4.5/D2): existence in the scopes
	// tenantSecretResolver will search — the tenant scope plus, with the
	// allow_shared_secrets opt-in, _global. Tenant-only would 422 exactly the
	// _global-only ref the resolver could find via the gated fallback; an
	// ungated _global would 200 a ref the resolver would NOT resolve (strict
	// isolation). Exists in ANY resolver scope ⇒ pass.
	for _, scope := range resolveScopes(ar, allowShared) {
		exists, err := store.SecretExists(r.Context(), h.pool, raw, scope)
		if err != nil {
			slog.Error("settings: secret existence check failed", "error", err,
				"request_id", RequestIDFromContext(r.Context()))
			return http.StatusInternalServerError, "internal error"
		}
		if exists {
			return 0, ""
		}
	}
	return http.StatusUnprocessableEntity, fmt.Sprintf(
		"validation: %s references an unknown secret — create the secret first", key)
}

// writeSetting persists one override in an attributed transaction (the 051
// triggers emit audit + NOTIFY atomically with the row). scope is writeScope(ar)
// — operator _global or the tenant's own scope, NEVER the request body.
func (h *SettingsHandler) writeSetting(r *http.Request, key string, value json.RawMessage, scope string) error {
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // rollback after commit is a no-op
	if err := store.SetTxRequestID(r.Context(), tx, RequestIDFromContext(r.Context())); err != nil {
		return err
	}
	if err := store.UpsertSetting(r.Context(), tx, key, scope, value, actorID(r)); err != nil {
		return err
	}
	return tx.Commit(r.Context())
}

// deleteSetting removes one override in an attributed transaction. scope is
// writeScope(ar) — a tenant-admin deletes only its own row, never _global.
func (h *SettingsHandler) deleteSetting(r *http.Request, key, scope string) (bool, error) {
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		return false, err
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // rollback after commit is a no-op
	if err := store.SetTxRequestID(r.Context(), tx, RequestIDFromContext(r.Context())); err != nil {
		return false, err
	}
	found, err := store.DeleteSetting(r.Context(), tx, key, scope, actorID(r))
	if err != nil {
		return false, err
	}
	return found, tx.Commit(r.Context())
}

// actorID extracts the acting key id for audit attribution.
func actorID(r *http.Request) *string {
	if ar := AuthResultFromContext(r.Context()); ar != nil && ar.ApiKeyID != "" {
		return &ar.ApiKeyID
	}
	return nil
}

// mutabilityBlock rejects writes on keys the override layer cannot serve
// (§7.3: no pending-restart state in F2; coupled = vector space).
func mutabilityBlock(info config.KeyInfo) (string, bool) {
	switch info.Mutability {
	case "hot", "coupled:embed-cache":
		return "", false
	case "coupled":
		return fmt.Sprintf("%s is coupled to the embedding vector space; changing it requires a re-embed migration — set %s and restart", info.Key, envHint(info)), true
	default: // restart
		return fmt.Sprintf("%s is restart-only; set %s and restart", info.Key, envHint(info)), true
	}
}

func envHint(info config.KeyInfo) string {
	if info.EnvVar == "" {
		return "it via env (no env var exists for this key)"
	}
	return info.EnvVar
}

// normalizedJSON converts the raw scalar into its canonical JSONB form so the
// DB never carries type drift ("0.7" string vs 0.7 number). Unparseable
// numerics fall back to the string form — the candidate build then rejects
// them with the real parser's message instead of a duplicated check here.
func normalizedJSON(info config.KeyInfo, raw string) json.RawMessage {
	quote := func() json.RawMessage {
		b, _ := json.Marshal(raw)
		return b
	}
	switch info.Type {
	case "int", "seconds":
		if n, err := strconv.Atoi(raw); err == nil {
			return json.RawMessage(strconv.Itoa(n))
		}
		return quote()
	case "float", "hours":
		if _, err := strconv.ParseFloat(raw, 64); err == nil {
			return json.RawMessage(raw)
		}
		return quote() // hours keeps suffix forms ("45d") as strings
	case "bool":
		return json.RawMessage(raw) // pre-validated to true|false
	default:
		return quote()
	}
}

// upsertRow returns rows with the (key, writeScope) row's value replaced or a new
// row at writeScope appended. It matches on BOTH key and scope: a tenant write
// must add/replace the TENANT-scope row and leave any _global row of the same key
// in place (so the candidate build sees both and the tenant precedence wins),
// never silently overwrite the _global row.
func upsertRow(rows []store.SettingOverride, key string, value json.RawMessage, scope string) []store.SettingOverride {
	out := make([]store.SettingOverride, 0, len(rows)+1)
	replaced := false
	for _, row := range rows {
		if row.Key == key && row.Scope == scope {
			row.Value = value
			replaced = true
		}
		out = append(out, row)
	}
	if !replaced {
		out = append(out, store.SettingOverride{Key: key, Scope: scope, Value: value})
	}
	return out
}

// issueFor returns the build issue attributed to key, or a generic reason.
func issueFor(issues []config.Issue, key string) string {
	for _, is := range issues {
		if is.Field == key {
			return is.Msg
		}
	}
	return "override would not take effect"
}

// newWarnings returns the warning messages present in cand but not in base —
// the §2.3 cross-field advisories (dual-runner num_ctx, blend 1.0 + graph)
// surface on exactly the write that introduces them.
func newWarnings(cand, base []config.Issue) []string {
	seen := make(map[config.Issue]bool, len(base))
	for _, is := range base {
		seen[is] = true
	}
	var out []string
	for _, is := range cand {
		if is.Severity == config.SeverityWarn && !seen[is] {
			out = append(out, fmt.Sprintf("%s: %s", is.Field, is.Msg))
		}
	}
	return out
}

// pairingWarnings implements the X3 advisory (risk 8): a .host override whose
// sibling .protocol still comes from env/default is the classic wire-format
// mismatch (ollama path against an openai server = 404). Data stays data —
// this warns, it never blocks.
func pairingWarnings(key string, candidate *config.Config) []string {
	group, field, _ := strings.Cut(key, ".")
	if field != "host" {
		return nil
	}
	protoKey := group + ".protocol"
	if _, ok := config.KeyByName(protoKey); !ok {
		return nil
	}
	if candidate.Source(protoKey) == "settings" {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s changed without %s — pair host+protocol+api_key in one migration step (wire-format mismatch otherwise)", key, protoKey)}
}

func (h *SettingsHandler) unknownKey(w http.ResponseWriter, key string) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"success": false, "error": fmt.Sprintf("unknown settings key %q — GET /api/settings lists the registry", key)})
}

func (h *SettingsHandler) internalError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	slog.Error(msg, "error", err, "request_id", RequestIDFromContext(r.Context()))
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
}
