package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// Tenant lifecycle manage-actions (MT T05a + T05b, Achse 01-T5; design/01 §4.3).
// All admin-gated via actionRequiresAdmin. T05a = create/list/get/update; T05b =
// delete = full-prune (this slice). The one remaining T05 concern is carved out
// along the design's own seam and deferred:
//   - E6 engine-turn suspend gate (per-turn status re-check, addendum §6.4,
//     chat/engine.go:208) = T05c. Note: status='suspended' set via tenant-update
//     ALREADY bites at the next auth through the 060 ctx_auth status gate; T05c
//     only adds the cut for already-running long-lived sessions (R-LEAK6).

// reservedSlug reports whether a tenant slug is in the system-reserved namespace
// ('_'-prefix, e.g. '_global' = the settings identity). design/01 §4.3 Finding
// 17: this is a SEPARATE gate from firstReservedScope (which checks home/allowed
// SCOPES, not a tenant slug — a new slug field does NOT inherit that gate). The
// 'default' slug is NOT '_'-prefixed and is protected only by UNIQUE(slug), so a
// second tenant-create slug='default' is a 409 (ErrTenantSlugExists), not 400.
func reservedSlug(slug string) bool {
	return strings.HasPrefix(slug, "_")
}

// slugPattern is the tenant-slug charset gate (BE5-1, Masterplan K5; design/04
// §D-A SEC-1). 1..24 chars of ASCII-lowercase alnum + internal hyphen, no
// leading/trailing hyphen. This deliberately excludes ':', whitespace, '_',
// uppercase and any non-ASCII/Unicode-homograph. It is Defense-in-Depth/hygiene,
// NOT the S1 collision-bearer (that is the scope name-gate + UNIQUE(slug)) — but
// it is load-bearing for clean prefix attribution: the D2a auto-prefix
// '<slug>:<name>' parses the slug off the LAST ':'; a ':'-or-whitespace-carrying
// slug would mis-attribute that prefix. The legacy 'default' slug satisfies it
// (grandfathered, no data migration).
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,22}[a-z0-9])?$`)

// scopeNamePattern is the S1-bearing name gate for self-service scope-create
// (BE5-3, Masterplan K1; design/04 §D-A name-gate). The caller chooses ONLY the
// name part; the server prepends the resolved tenant slug to build
// '<slug>:<name>'. DNS-label style: 1..24 chars of ASCII-lowercase alnum +
// internal hyphen, no edge hyphen. It excludes ':' (the prefix separator — the
// structural S1 collision-bearer together with UNIQUE(slug)), whitespace, '_',
// uppercase and any non-ASCII/Unicode-homograph BY CONSTRUCTION, not by
// blacklist. Parsing the scope off the LAST ':' is then unambiguous (name carries
// no ':'), so (tenant,name)→scope is injective: two distinct tenants can never
// construct the same scope string.
var scopeNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,22}[a-z0-9])?$`)

// scopeCreateSpec is the JSON shape under req.Data for scope-create. The caller
// supplies ONLY the bare name; the '<slug>:' prefix is derived server-side from
// the resolved tenant (never the payload) → cross-tenant prefix-injection is
// structurally impossible (S1). TenantID is a SERVER-ADMIN-ONLY override
// (provision for a foreign tenant); a non-server-admin's TenantID is ignored, so
// it can only ever address its OWN '<slug>:' namespace (S2). The frozen FE
// contract (web/src/lib/api/scopes.ts) sends this override as data.tenant_id.
type scopeCreateSpec struct {
	Name     string `json:"name"`
	TenantID string `json:"tenant_id,omitempty"`
}

type tenantSpec struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

// maxTenantLimit is the upper bound per structural dimension (BEQ-1b, design/02
// §1). It sits SAFELY below int4-MAX so a fat-finger ("10_000_000_000 = basically
// unlimited") hits the write validation (→ 400) instead of overflowing the typed
// 069 INTEGER column (22003 → 500). 0 is a VALID stored value ("frozen": creation
// blocked without suspending the tenant); negative is rejected (the n >= -1
// permanent-lockout footgun).
const maxTenantLimit = 1_000_000

var (
	// errLimitRange — a present limit is negative or above maxTenantLimit (→ 400).
	errLimitRange = errors.New("max_scopes/max_keys must each be an integer between 0 and 1000000")
	// errLimitsBothNeeded — tenant-limit-set with a field missing from the JSON
	// object (→ 400). A partial PATCH must NOT silently relax a dimension to
	// unlimited; the set is REPLACE-semantics, both keys mandatory.
	errLimitsBothNeeded = errors.New("both max_scopes and max_keys are required")
)

// limitsFromData decodes + range-validates the structural limit fields from the
// manage payload. ONE helper feeds both write paths (tenant-create seeding and
// tenant-limit-set) so the validation can never diverge. Three states per field:
//   - ABSENT: requirePresence (limit-set) → errLimitsBothNeeded; otherwise (create
//     seed) → nil pointer = "leave the 069 column DEFAULT" (the caller seeds only
//     supplied dimensions).
//   - explicit null: → nil pointer = unlimited for that dimension (set-to-unlimited;
//     replace, not patch).
//   - a number: must be 0 <= v <= maxTenantLimit, else errLimitRange. 0 is valid
//     (frozen); a non-integer / negative / over-range value is errLimitRange.
//
// Presence is detected via map[string]json.RawMessage (a *int alone cannot tell
// absent from explicit null). The returned pointers carry only the parsed values;
// the absent-vs-null distinction is consumed HERE (it only changes which error or
// nil a field yields), so callers receive the clean (*int, *int).
func limitsFromData(data json.RawMessage, requirePresence bool) (maxScopes, maxKeys *int, err error) {
	var fields map[string]json.RawMessage
	if len(data) > 0 {
		if uerr := json.Unmarshal(data, &fields); uerr != nil {
			return nil, nil, uerr
		}
	}
	if maxScopes, err = oneLimitField(fields, "max_scopes", requirePresence); err != nil {
		return nil, nil, err
	}
	if maxKeys, err = oneLimitField(fields, "max_keys", requirePresence); err != nil {
		return nil, nil, err
	}
	return maxScopes, maxKeys, nil
}

// oneLimitField parses a single optional limit key (see limitsFromData). A present
// null (or empty raw) → nil = unlimited; a present number → a range-checked *int.
func oneLimitField(fields map[string]json.RawMessage, key string, requirePresence bool) (*int, error) {
	raw, present := fields[key]
	if !present {
		if requirePresence {
			return nil, errLimitsBothNeeded
		}
		return nil, nil // absent: keep the 069 DEFAULT (create) / no-op
	}
	if t := strings.TrimSpace(string(raw)); t == "null" || t == "" {
		return nil, nil // explicit null = unlimited for this dimension
	}
	var v int
	if json.Unmarshal(raw, &v) != nil {
		return nil, errLimitRange // non-integer (string, float, bool, ...) → 400
	}
	if v < 0 || v > maxTenantLimit {
		return nil, errLimitRange
	}
	return &v, nil
}

// handleTenantLimitSet writes the structural per-tenant caps (BEQ-1b, design/02
// §3b). Tier tierServerAdmin (actionTier): the cap is an OPERATOR ceiling, never
// tenant-self-raisable (the tenant-usage-get READ is the tenant-admin counterpart
// — K10 tier-asymmetry). REPLACE-semantics: limitsFromData(req.Data, true) forces
// BOTH max_scopes and max_keys to be present (a missing field → 400, no silent
// uncap); a present null = unlimited, a present number = the cap. Unknown/malformed
// id → 404 (no oracle). The 200 re-reads the row so the response echoes the stored
// limits (the frozen FE TenantResponse {success, tenant}); SetTenantLimits returns
// no row, so the re-get is how the echo is produced.
func (h *ManageHandler) handleTenantLimitSet(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	maxScopes, maxKeys, err := limitsFromData(req.Data, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if err := store.SetTenantLimits(r.Context(), h.pool, req.ID, maxScopes, maxKeys); err != nil {
		if errors.Is(err, store.ErrTenantNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "set tenant limits failed"})
		return
	}
	// Echo the canonical stored row. SetTenantLimits already committed, so a re-read
	// failure here is purely cosmetic — still report success.
	tn, err := store.GetTenant(r.Context(), h.pool, req.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tenant": tn})
}

// handleTenantUsageGet returns one tenant's structural usage (scope_count +
// active-key_count) alongside its limits (BEQ-1b, design/02 §3 / design/05
// Cross-Doc #3). Tier tierTenantAdmin. SECURITY (the load-bearing pin): the target
// is ALWAYS ar.TenantID for a non-server-admin — req.ID is read ONLY when
// IsServerAdmin (byte-analog handleTenantQuotaGet). A tenant-admin B sending id=A
// is therefore counted against B, NEVER A → no cross-tenant counts oracle. An empty
// ar.TenantID (never happens for a valid non-server-admin key — ctx_auth guarantees
// one, and the tier gate already rejected it) collapses to a 404 (fail-closed).
func (h *ManageHandler) handleTenantUsageGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	if ar == nil {
		// Defense in depth — the tier gate already rejected anon callers.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	target := ar.TenantID
	if ar.IsServerAdmin() && req.ID != "" {
		target = req.ID
	}
	maxScopes, maxKeys, err := store.TenantLimits(r.Context(), h.pool, target)
	if err != nil {
		if errors.Is(err, store.ErrTenantNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "tenant limits lookup failed"})
		return
	}
	scopes, err := store.TenantScopes(r.Context(), h.pool, target)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "tenant scopes lookup failed"})
		return
	}
	// key_count counts ONLY active keys (ListApiKeys activeOnly=true) — consistent
	// with the KeySlot cap semantics: a revoked (soft-deleted) key frees its slot.
	keys, err := store.ListApiKeys(r.Context(), h.pool, target, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "tenant keys lookup failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"usage": map[string]any{
			"tenant_id":   target,
			"max_scopes":  maxScopes,
			"max_keys":    maxKeys,
			"scope_count": len(scopes),
			"key_count":   len(keys),
		},
	})
}

// validTenantStatus is the lifecycle CHECK domain (059). Validated in the handler
// (→ 400) ahead of the DB CHECK backstop (23514).
func validTenantStatus(s string) bool {
	return s == "active" || s == "suspended" || s == "offboarding"
}

func (h *ManageHandler) handleTenantCreate(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	var spec tenantSpec
	if len(req.Data) == 0 || json.Unmarshal(req.Data, &spec) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required (slug, display_name)"})
		return
	}
	// Normalize the slug BEFORE the reserved-namespace check: a raw HasPrefix
	// would let a leading-whitespace slug (" _global") slip past the '_'-gate and
	// plant a near-homograph of the system sentinel. Trim, then validate + store
	// the trimmed value.
	spec.Slug = strings.TrimSpace(spec.Slug)
	if spec.Slug == "" || spec.DisplayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "slug and display_name are required"})
		return
	}
	if reservedSlug(spec.Slug) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "slug names starting with '_' are reserved (system namespace)"})
		return
	}
	// Charset gate (BE5-1): runs AFTER the reserved-namespace check so a '_'-slug
	// keeps its specific "reserved" 400, while everything else off-charset
	// (uppercase, ':', whitespace, '_'-internal, Unicode) is rejected here. Hard
	// hygiene for the D2a '<slug>:<name>' auto-prefix attribution.
	if !slugPattern.MatchString(spec.Slug) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "slug must be 1-24 chars of a-z, 0-9, '-' (no leading/trailing '-')"})
		return
	}
	// BEQ-1b create-seeding (design/02 §3a): optional structural limits in the
	// create payload, validated by the SAME limitsFromData helper as tenant-limit-set
	// (symmetric range gate). Parse BEFORE the insert so a bad value 400s without
	// creating a tenant. requirePresence=false: an ABSENT dimension keeps the 069
	// column DEFAULT (25/50) — only supplied dimensions are seeded.
	seedScopes, seedKeys, err := limitsFromData(req.Data, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	// BE6-7 compound bootstrap (design/03 §6, design/01 Cross-Doc #3): the initial
	// scope is the auto-prefixed '<slug>:main', built from the gated slug (never the
	// payload — S1). The slug is already slugPattern-bounded (<= 24) and the name is
	// the fixed "main", so the result can never exceed the VARCHAR(50) budget; the
	// pre-check is defence-in-depth that yields a clean 400 ahead of the 22001 the
	// owner-key mint would otherwise raise (the ErrScopeTooLong path, design/03 §6).
	initialScope := spec.Slug + ":" + bootstrapScopeName
	if len(initialScope) > 50 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "scope name too long (max 50 chars including the tenant prefix)"})
		return
	}
	tn, ownerKey, ownerPlaintext, err := h.bootstrapTenant(r.Context(), spec.Slug, spec.DisplayName, initialScope, seedScopes, seedKeys)
	if err != nil {
		writeBootstrapError(w, err)
		return
	}
	// FLAT compound result (frozen FE contract TenantCreateResult, types.ts): the
	// owner-key plaintext is shown EXACTLY ONCE here (never persisted in clear — only
	// its SHA-256 hash, api_keys.go).
	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"tenant":       tn,
		"scope":        initialScope,
		"owner_key_id": ownerKey.ID,
		"owner_key":    ownerPlaintext,
	})
}

// bootstrapScopeName is the name part of the initial auto-prefixed scope minted by
// the compound tenant-create. The registered scope is '<slug>:main' — a sane
// default home for the owner key so the tenant is immediately usable (not inert,
// K10). The slug carries the cross-tenant uniqueness (S1); 'main' is just the
// conventional first scope.
const bootstrapScopeName = "main"

// bootstrapTenant runs the ATOMIC compound tenant-create (design/03 §6, closing
// K10): in ONE transaction it (a) creates the tenant, (b) seeds the supplied
// structural limits over the 069 DEFAULTs, (c) registers the initial auto-prefixed
// scope CAP-FREE (maxScopes=-1, so the first scope never wedges on a max_scopes=0
// seed), and (d) mints the owner key (role='owner', home + allowed = that scope).
// Any step's failure rolls the whole tx back — there is never a half-created, inert
// tenant. The insert ORDER is load-bearing: the scope (c) is registered BEFORE the
// owner key (d) so the key's home_scope already exists in context_tenant_scopes
// (T22). The owner-key mint skips the key quota by construction (MintOwnerKey, no
// cap) and skips T22 legitimately (server-admin tenant-create, freshly registered
// own scope). It returns the tenant (with seeded limits echoed), the owner key
// record, and its plaintext (shown once).
func (h *ManageHandler) bootstrapTenant(ctx context.Context, slug, displayName, initialScope string, seedScopes, seedKeys *int) (*store.Tenant, store.ApiKey, string, error) {
	// The two labels are the caller's own words, not At("bootstrap"): the texts
	// have always been "bootstrap begin"/"bootstrap commit" without the colon,
	// and %w keeps writeBootstrapError's errors.As(&pgErr) path intact.
	var (
		tn        *store.Tenant
		key       store.ApiKey
		plaintext string
	)
	err := pgxdb.Write(ctx, h.pool, pgxdb.Stages{Begin: "bootstrap begin", Commit: "bootstrap commit"}, func(tx pgx.Tx) error {
		// (a) Tenant row. A duplicate slug surfaces as ErrTenantSlugExists (→ 409).
		var err error
		tn, err = store.CreateTenantTx(ctx, tx, slug, displayName)
		if err != nil {
			return err
		}

		// (b) Seed only the dimensions the payload supplied, merged over tn's 069
		// DEFAULTs (25/50) so an absent dimension keeps its default — inside this tx, so
		// the caps commit with the tenant (no uncapped window, design/04 §D-B S3).
		if seedScopes != nil || seedKeys != nil {
			newScopes, newKeys := tn.MaxScopes, tn.MaxKeys
			if seedScopes != nil {
				newScopes = seedScopes
			}
			if seedKeys != nil {
				newKeys = seedKeys
			}
			if err := store.SetTenantLimitsTx(ctx, tx, tn.ID, newScopes, newKeys); err != nil {
				return err
			}
			tn.MaxScopes, tn.MaxKeys = newScopes, newKeys
		}

		// (c) Register the initial scope CAP-FREE (maxScopes=-1), BEFORE the owner key.
		capFree := -1
		if _, err := store.AssignTenantScopeTx(ctx, tx, tn.ID, initialScope, &capFree); err != nil {
			return err
		}

		// (d) Owner key: the single sanctioned path to role='owner' (MintOwnerKey), home
		// + allowed pinned to the just-registered scope. A 22001 here (over-long scope)
		// is mapped to a clean 400 by writeBootstrapError, not an anonymous 500.
		key, plaintext, err = store.MintOwnerKey(ctx, tx, slug+" owner", initialScope, []string{initialScope}, tn.ID)
		return err
	})
	if err != nil {
		return nil, store.ApiKey{}, "", err
	}
	return tn, key, plaintext, nil
}

// writeBootstrapError maps a compound tenant-create failure to its HTTP status. A
// rolled-back tx means NO partial tenant escaped (the atomicity guarantee), so each
// of these is a clean terminal response. The 22001 (value too long) from the
// owner-key mint surfaces as a 400 'scope name too long' (design/03 §6
// ErrScopeTooLong), never an anonymous 500.
func writeBootstrapError(w http.ResponseWriter, err error) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, store.ErrTenantSlugExists):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "tenant slug already exists"})
	case errors.Is(err, store.ErrScopeExists):
		// The slug is UNIQUE and the scope is slug-derived, so a fresh tenant can
		// never collide here — defensive 409 rather than a misleading 500.
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "initial scope already exists"})
	case errors.As(err, &pgErr) && pgErr.Code == "22001":
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "scope name too long (max 50 chars including the tenant prefix)"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "create tenant failed"})
	}
}

func (h *ManageHandler) handleTenantList(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, _ manageRequest) {
	tenants, err := store.ListTenants(r.Context(), h.pool)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "list tenants failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tenants": tenants})
}

func (h *ManageHandler) handleTenantGet(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	tn, err := store.GetTenant(r.Context(), h.pool, req.ID)
	if errors.Is(err, store.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "get tenant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tenant": tn})
}

func (h *ManageHandler) handleTenantUpdate(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	// status comes from req.Status; an optional display_name patch from data.
	displayName := ""
	if len(req.Data) > 0 {
		var spec tenantSpec
		if err := json.Unmarshal(req.Data, &spec); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "unparseable data payload"})
			return
		}
		displayName = spec.DisplayName
	}
	if req.Status == "" && displayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "nothing to update (provide status and/or display_name)"})
		return
	}
	if req.Status != "" && !validTenantStatus(req.Status) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "status must be active, suspended, or offboarding"})
		return
	}
	tn, err := store.UpdateTenant(r.Context(), h.pool, req.ID, req.Status, displayName)
	if errors.Is(err, store.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "update tenant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tenant": tn})
}

// defaultTenantID is the fixed UUID of the single-tenant default tenant (059, E9
// slug 'default') that carries the ENTIRE legacy corpus (all of private/work/
// shared). It is referenced here only to GUARD it against tenant-delete. The
// canonical definition lives in the store layer (store.DefaultTenantID, T06);
// this is a package-local alias so the guard reads cleanly.
//
// TENANT-DECISION(prune-default-guard): tenant-delete REFUSES the default tenant
// (400) — a full-prune of it would destroy the whole single-tenant corpus (every
// block in every scope), irreversibly (W17 minimal blast-radius). The guard lives
// at the handler (policy) layer, NOT in store.PruneTenant (mechanism), so
// test-tenant teardown and a future deliberate operator path can still reach the
// raw prune. Umentscheidbar: an operator who genuinely offboards the default
// tenant must lift this guard explicitly.
const defaultTenantID = store.DefaultTenantID

// handleTenantDelete runs the T05b full-prune (store.PruneTenant): the FK-ordered,
// batched mass-DELETE of the tenant's scope-carried data, its keys, then the
// tenant row (design/01 §4.3.1). Admin-gated upstream (actionRequiresAdmin); the
// default tenant is additionally guarded (400). Unknown/malformed id → 404.
func (h *ManageHandler) handleTenantDelete(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	if req.ID == defaultTenantID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "the default tenant cannot be deleted"})
		return
	}
	err := store.PruneTenant(r.Context(), h.pool, req.ID)
	if errors.Is(err, store.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "delete tenant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": req.ID})
}

// Cross-tenant grant manage-actions (MT T17, Achse 02-V4; design/02 §V4). The
// friend-tenant read channel: an admin grants tenant B read access to a scope
// owned by tenant A. Read takes effect at the grantee's NEXT auth (ctx_auth, 060,
// re-resolves read_scopes per request) — these actions never mutate a running
// session, and never touch the WRITE side. Admin-gated via actionRequiresAdmin
// using the server-global is_admin; per-tenant-admin scoping is Achse 05 (T25,
// §V4 deps note) — explicitly named, no hidden coupling.

type grantSpec struct {
	GranteeTenant string `json:"grantee_tenant"`
	GrantedScope  string `json:"granted_scope"`
}

func (h *ManageHandler) handleTenantGrantCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var spec grantSpec
	if len(req.Data) == 0 || json.Unmarshal(req.Data, &spec) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required (grantee_tenant, granted_scope)"})
		return
	}
	spec.GrantedScope = strings.TrimSpace(spec.GrantedScope)
	if spec.GranteeTenant == "" || spec.GrantedScope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "grantee_tenant and granted_scope are required"})
		return
	}
	// '_'-system scopes are never grantable. The granted_scope FK to
	// context_tenant_scopes is the fail-closed backstop (a '_'-scope is not an
	// ownable, registered scope → store ErrGrantUnknownTarget), but reject early
	// with a clear 400 — same reserved-namespace rule as reservedSlug/api_keys.go:44.
	if strings.HasPrefix(spec.GrantedScope, "_") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "system scopes ('_'-prefixed) cannot be granted"})
		return
	}
	g, err := store.CreateTenantGrant(r.Context(), h.pool, spec.GranteeTenant, spec.GrantedScope, ar.ApiKeyID)
	switch {
	case errors.Is(err, store.ErrGrantExists):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "grant already exists"})
	case errors.Is(err, store.ErrGrantUnknownTarget):
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "unknown grantee tenant or unregistered scope"})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "create grant failed"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "grant": g})
	}
}

func (h *ManageHandler) handleTenantGrantList(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	// req.ID, if present, narrows to one grantee tenant's grants; empty = all.
	grants, err := store.ListTenantGrants(r.Context(), h.pool, req.ID)
	if errors.Is(err, store.ErrGrantUnknownTarget) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "malformed grantee_tenant filter"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "list grants failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "grants": grants})
}

func (h *ManageHandler) handleTenantGrantDelete(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	err := store.DeleteTenantGrant(r.Context(), h.pool, req.ID)
	if errors.Is(err, store.ErrGrantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "grant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "delete grant failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": req.ID})
}

// handleScopeCreate registers ONE self-service scope for a tenant (BE5-3,
// Masterplan K1; design/04 §D-A, design/01 §3). Tier tierTenantAdmin (NOT the
// server-admin tier of its tenant-* case neighbours): the handler is
// tenant-isolated — the binding tenant is ar.TenantID for a tenant-admin (a
// server-admin MAY target another via data.tenant_id / req.ID), and the scope
// PREFIX is the DB slug of that tenant, never the payload. A non-server-admin
// therefore can only write in its OWN '<slug>:' namespace → no cross-tenant squat
// (S1+S2). The name gate (scopeNamePattern) excludes ':' (S1 collision-bearer).
func (h *ManageHandler) handleScopeCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	if ar == nil {
		// Defense in depth — the tier gate already rejected anon callers.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	var spec scopeCreateSpec
	if len(req.Data) == 0 || json.Unmarshal(req.Data, &spec) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required (name)"})
		return
	}
	// Target-tenant resolution (S2): a non-server-admin is ALWAYS bound to its own
	// ar.TenantID — the override (data.tenant_id, or the top-level req.ID) is read
	// ONLY for a server-admin. So an attacker can never choose the prefix authority
	// and never squat a foreign namespace. data.tenant_id is the frozen FE channel
	// (scopes.ts); req.ID is honoured as a fallback for the task's manage shape.
	override := spec.TenantID
	if override == "" {
		override = req.ID
	}
	bindingTenant := ar.TenantID
	if ar.IsServerAdmin() && override != "" {
		bindingTenant = override
	}
	tn, err := store.GetTenant(r.Context(), h.pool, bindingTenant)
	if errors.Is(err, store.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "get tenant failed"})
		return
	}
	// Defense-in-depth: re-validate the STORED slug before building the prefix, so a
	// hypothetical off-charset legacy slug (':'/whitespace) cannot mis-attribute the
	// '<slug>:<name>' prefix. Fail-closed (500) rather than trust the create-time gate alone.
	if !slugPattern.MatchString(tn.Slug) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "tenant slug is not prefix-safe"})
		return
	}
	name := strings.TrimSpace(spec.Name)
	if !scopeNamePattern.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "name must be 1-24 chars of a-z, 0-9, '-' (no leading/trailing '-', no ':')"})
		return
	}
	scope := tn.Slug + ":" + name
	if len(scope) > 50 {
		// context_tenant_scopes.scope is VARCHAR(50) — a hard backstop ahead of the
		// 22001 the INSERT would otherwise raise (→ 500); pre-check yields a clean 400.
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "scope name too long (max 50 chars including the tenant prefix)"})
		return
	}
	// Limits are ALWAYS fetched for the BINDING (target) tenant, NEVER ar.TenantID —
	// otherwise a server-admin targeting tenant B would enforce A's cap. The read is
	// FAIL-CLOSED (S3): a transient error is a 500, never a silent default to
	// unlimited (a pool-exhaustion fault would otherwise void the quota).
	maxScopes, _, err := store.TenantLimits(r.Context(), h.pool, bindingTenant)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "tenant limits lookup failed"})
		return
	}
	if _, err := store.AssignTenantScope(r.Context(), h.pool, bindingTenant, scope, maxScopes); err != nil {
		switch {
		case errors.Is(err, store.ErrScopeExists):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "scope already exists"})
		case errors.Is(err, store.ErrScopeQuotaExceeded):
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "tenant scope quota exceeded"})
		case errors.Is(err, store.ErrTenantNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "assign scope failed"})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "scope": scope, "tenant_id": bindingTenant})
}

// handleScopeList lists the caller's OWN tenant scopes (BE5-3; design/01 §3, FE
// scopes.ts listScopes). Tier tierTenantAdmin, server-side filtered on
// ar.TenantID — a server-admin MAY target another tenant via req.ID, but a
// non-server-admin NEVER sees foreign-tenant scopes (no enumeration). Distinct
// from the server-admin scope-overview (deliberately unscoped, whole-store).
//
// The rows are ScopeOverview-shaped to match the frozen FE
// ScopeOverviewListResponse, but block_count/key_count are 0 BY DESIGN: a
// per-scope count would need either the server-admin-only global GROUP-BY of
// store.ScopeOverviews (an index-scan over ALL blocks at the 1M target — wrong
// for a per-tenant list) or a new tenant-scoped count store function (out of this
// wave's file scope). The tenant-scoped read returns the scope inventory; the
// counts are a separate, deferrable enrichment.
func (h *ManageHandler) handleScopeList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	if ar == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	target := ar.TenantID
	if ar.IsServerAdmin() && req.ID != "" {
		target = req.ID
	}
	scopes, err := store.TenantScopes(r.Context(), h.pool, target)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "list scopes failed"})
		return
	}
	tid := target
	rows := make([]store.ScopeOverview, 0, len(scopes))
	for _, s := range scopes {
		rows = append(rows, store.ScopeOverview{Scope: s, TenantID: &tid})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "scopes": rows})
}

// dispatchTenantAction fans the tenant-* lifecycle actions out (split from
// HandleManage's switch for cyclomatic budget, mirroring dispatchBackendAction).
// It also routes the tenant-grant-* cross-tenant read-grant actions (T17, §V4):
// same tenant family, same admin gate.
func (h *ManageHandler) dispatchTenantAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "tenant-create":
		h.handleTenantCreate(w, r, ar, req)
	case "project-provision":
		// I-I (design/02 §4.6): the server-admin compound that bootstraps a whole
		// tenant-per-repo. Routed through the tenant family (it CREATES a tenant);
		// tierServerAdmin is set in actionTier, not here (routing ⟂ tier).
		h.handleProjectProvision(w, r, ar, req)
	case "tenant-list":
		h.handleTenantList(w, r, ar, req)
	case "tenant-get":
		h.handleTenantGet(w, r, ar, req)
	case "tenant-update":
		h.handleTenantUpdate(w, r, ar, req)
	case "tenant-delete":
		h.handleTenantDelete(w, r, ar, req)
	case "tenant-limit-set":
		// BEQ-1b (design/02 §3b): set the structural caps. tierServerAdmin (set in
		// actionTier, NOT here) — the operator ceiling, never tenant-self-raisable.
		h.handleTenantLimitSet(w, r, ar, req)
	case "tenant-usage-get":
		// BEQ-1b (design/02 §3 / 05 Cross-Doc #3): read own usage+limits.
		// tierTenantAdmin; the handler pins a non-server-admin onto ar.TenantID.
		h.handleTenantUsageGet(w, r, ar, req)
	case "tenant-grant-create":
		h.handleTenantGrantCreate(w, r, ar, req)
	case "tenant-grant-list":
		h.handleTenantGrantList(w, r, ar, req)
	case "tenant-grant-delete":
		h.handleTenantGrantDelete(w, r, ar, req)
	case "scope-overview":
		// MT 04-W6/A0 (design/04 §3): server-admin scope landscape — per-scope
		// counts + scope→tenant mapping. No AuthResult: the read is a global
		// landscape (tierServerAdmin-gated), deliberately unscoped, counts-only.
		h.handleScopeOverview(w, r)
	case "scope-create":
		// BE5-3 (Masterplan K1): self-service scope-create. tierTenantAdmin (NOT
		// server-admin like its case neighbours) — tenant-isolated by the
		// server-side '<slug>:' auto-prefix; the tier is set in actionTier, not here.
		h.handleScopeCreate(w, r, ar, req)
	case "scope-list":
		// BE5-3: the tenant's OWN scope inventory (tierTenantAdmin, filtered on
		// ar.TenantID). Distinct from the server-admin scope-overview above.
		h.handleScopeList(w, r, ar, req)
	case "block-grant-create", "block-grant-list", "block-grant-revoke":
		// block-level (T43, 07-W6) — the row-level share, same grant family.
		h.dispatchBlockGrantAction(w, r, ar, req)
	}
}
