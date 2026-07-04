// project-provision (workflow I-I, design/02 §4.6; masterplan K12/E4/E7): the
// ONE server-admin manage-action that turns a bare repo identity into a fully
// usable, isolated tenant-per-repo. It is deliberately a MANAGE-ACTION, not a
// REST /api/project verb, because it CREATES A TENANT — and tenant creation is
// server-admin (E4, tenant-create is already a manage-action, actionTier =
// tierServerAdmin). /api/project (W4) stays the REST surface for binding a repo
// into an EXISTING tenant (a different, member/tenant-admin operation); provision
// is the superset that bootstraps the whole tenant. Keeping it on /api/manage
// alongside tenant-create is the Ist-code convention (tenant lifecycle lives in
// manage; the S9 enumeration gate pins its tier RED-then-GREEN).
//
// The compound (store.ProvisionProject, ONE tx): tenant + limits + '<slug>:main'
// scope + owner key + project register row + repo-agent key (K12 template) +
// quota seed (E7). Idempotent on the project identity: a re-run returns the
// existing project with provisioned=false and mints NOTHING (the two keys are
// reveal-once at first provision). An optional forge PAT is sealed POST-commit
// via the sync engine (SetToken owns its own tx; the token is a retriable
// post-provision concern — the project is usable, push stays disabled until
// enabled anyway, §5.6).
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
)

// defaultProvisionDailyCalls is the E7 quota seed: a conservative foreground
// wire-call ceiling so a freshly-imported 10k-issue repo can not monopolise the
// shared inference budget (§6.4). A starting point, not a limit — the operator
// tunes it per tenant via tenant-quota-set, or overrides it in the provision
// payload's `quota` object.
const defaultProvisionDailyCalls = 5000

// provisionRequest is the project-provision data payload. identity is required;
// slug/display_name/scope_name are optional server-side overrides (derived from
// the identity otherwise); forge/token seed the sync binding; quota overrides the
// E7 default (an explicit `"quota": null`-less absence keeps the default).
type provisionRequest struct {
	Identity    string          `json:"identity"`
	Slug        string          `json:"slug"`         // optional override; else derived from identity
	DisplayName string          `json:"display_name"` // optional; defaults to the identity
	Forge       json.RawMessage `json:"forge"`        // optional {kind,owner,repo,api_base?}
	Token       string          `json:"token"`        // optional forge PAT (sealed post-commit, never echoed)
	MaxScopes   *int            `json:"max_scopes"`   // optional tenant limit seed
	MaxKeys     *int            `json:"max_keys"`     // optional tenant limit seed
	Quota       *quotaSpec      `json:"quota"`        // optional E7 quota override
	SeedQuota   *bool           `json:"seed_quota"`   // false ⇒ skip the E7 seed entirely (default: seed)
}

// handleProjectProvision runs the compound. Server-admin only (actionTier =
// tierServerAdmin, the tenant-creation authority E4). A member / tenant-admin
// never reaches this handler — the tier gate 403s first (the negative probe).
func (h *ManageHandler) handleProjectProvision(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	if len(req.Data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required (identity)"})
		return
	}
	var pr provisionRequest
	if err := json.Unmarshal(req.Data, &pr); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "unparseable data payload"})
		return
	}

	identity := strings.TrimSpace(pr.Identity)
	if !validIdentity(identity) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "identity must start with github: | git-root: | manual:"})
		return
	}

	// Slug: an explicit override (gated) or derived from the identity (§4.6 step 2,
	// truncation ⇒ hash-suffix so two long identities can not collide).
	slug := strings.TrimSpace(pr.Slug)
	if slug == "" {
		slug = deriveTenantSlug(identity)
	}
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "could not derive a tenant slug from the identity; pass an explicit slug"})
		return
	}
	if reservedSlug(slug) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "slug names starting with '_' are reserved (system namespace)"})
		return
	}
	if !slugPattern.MatchString(slug) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "slug must be 1-24 chars of a-z, 0-9, '-' (no leading/trailing '-')"})
		return
	}

	// Scope = '<slug>:main' (the bootstrap scope, built from the GATED slug — never
	// the payload). slug ≤ 24 + ":main" ≤ 29 < 50, but keep the defensive check.
	scope := slug + ":" + bootstrapScopeName
	if len(scope) > 50 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "scope name too long (max 50 chars including the tenant prefix)"})
		return
	}

	displayName := strings.TrimSpace(pr.DisplayName)
	if displayName == "" {
		displayName = identity
	}

	// forge.api_base SSRF deny-list at provision time too (a provision can seed the
	// same dangerous api_base a PATCH would reject; §5.7).
	if msg := validateForge(pr.Forge); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
		return
	}

	// E7 quota seed (default unless seed_quota=false). An explicit `quota` object
	// overrides the default; on_exceed is validated for a clean 422.
	quota, msg := h.resolveProvisionQuota(pr)
	if msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": msg})
		return
	}

	res, err := store.ProvisionProject(ctx, h.pool, store.ProvisionParams{
		Slug:        slug,
		DisplayName: displayName,
		Scope:       scope,
		Identity:    identity,
		Forge:       pr.Forge,
		SeedScopes:  pr.MaxScopes,
		SeedKeys:    pr.MaxKeys,
		Quota:       quota,
		CreatedBy:   ar.ApiKeyID,
	})
	if err != nil {
		writeProvisionError(w, err)
		return
	}

	// Idempotent re-run: the identity was already provisioned. Return the existing
	// project, provisioned=false, NO keys (reveal-once happened at first provision).
	if !res.Provisioned {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":     true,
			"provisioned": false,
			"scope":       res.Scope,
			"repo_id":     res.Project.ID,
			"project":     res.Project,
		})
		return
	}

	// Fresh provision: seal the forge PAT POST-commit if one was supplied. A seal
	// failure does NOT undo the provision (the token is set later via
	// forge-token-set) — surface token_set honestly so the caller knows.
	tokenSet := false
	if pr.Token != "" {
		if h.forge == nil {
			slog.Warn("provision: token supplied but sync engine not enabled", "project", res.Project.ID)
		} else if terr := h.forge.SetToken(ctx, *res.Project, pr.Token); terr != nil {
			slog.Error("provision: token seal failed (project provisioned, set the token later)", "project", res.Project.ID, "error", terr, "request_id", RequestIDFromContext(ctx))
		} else {
			tokenSet = true
		}
	}

	// FLAT reveal-once compound result (mirrors the tenant-create shape,
	// tenant_manage.go:297-305): both key plaintexts are shown EXACTLY ONCE here
	// and never persisted in clear (only their SHA-256 hashes, api_keys.go).
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"provisioned":     true,
		"tenant":          res.Tenant,
		"scope":           res.Scope,
		"repo_id":         res.Project.ID,
		"project":         res.Project,
		"owner_key_id":    res.OwnerKey.ID,
		"owner_key":       res.OwnerPlaintext,
		"agent_key_id":    res.AgentKey.ID,
		"agent_key":       res.AgentPlaintext,
		"agent_home_scope": res.AgentKey.HomeScope,
		"token_set":       tokenSet,
	})
}

// resolveProvisionQuota builds the E7 quota seed: nil (skip) when seed_quota is
// false, the payload's explicit quota when supplied (on_exceed validated), else
// the conservative default (daily_calls cap, external_off). Returns (nil, msg)
// on a validation error (422).
func (h *ManageHandler) resolveProvisionQuota(pr provisionRequest) (*backends.TenantQuota, string) {
	if pr.SeedQuota != nil && !*pr.SeedQuota {
		return nil, ""
	}
	if pr.Quota != nil {
		onExceed := pr.Quota.OnExceed
		if onExceed == "" {
			onExceed = backends.QuotaExceedExternalOff
		}
		if onExceed != backends.QuotaExceedBlock && onExceed != backends.QuotaExceedExternalOff {
			return nil, `quota.on_exceed must be "block" or "external_off"`
		}
		enabled := true
		if pr.Quota.Enabled != nil {
			enabled = *pr.Quota.Enabled
		}
		return &backends.TenantQuota{
			DailyCostUSD:   pr.Quota.DailyCostUSD,
			MonthlyCostUSD: pr.Quota.MonthlyCostUSD,
			DailyCalls:     pr.Quota.DailyCalls,
			OnExceed:       onExceed,
			Enabled:        enabled,
		}, ""
	}
	dc := defaultProvisionDailyCalls
	return &backends.TenantQuota{
		DailyCalls: &dc,
		OnExceed:   backends.QuotaExceedExternalOff,
		Enabled:    true,
	}, ""
}

// writeProvisionError maps a compound failure to its HTTP status. A rolled-back
// tx means NO partial tenant escaped (the atomicity guarantee).
func writeProvisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrTenantSlugExists):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "tenant slug already exists (choose an explicit slug)"})
	case errors.Is(err, store.ErrScopeExists):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "initial scope already exists"})
	case errors.Is(err, store.ErrProjectExists):
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "project identity already registered"})
	default:
		slog.Error("provision: compound failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "provision failed"})
	}
}

// deriveTenantSlug computes a deterministic, collision-resistant tenant slug from
// a project identity (§4.6 step 2): 'gh-<owner>-<repo>' for github, 'repo-<sha12>'
// for git-root, the raw slug for manual. The base is sanitized to the slug
// charset (lowercase a-z0-9-, no leading/trailing '-'); on OVERLENGTH it is
// truncated to 19 chars and a 4-hex suffix derived from the FULL identity is
// appended (19 + '-' + 4 = 24) — so two long identities sharing a 24-char prefix
// still get distinct slugs (the truncation ⇒ hash-suffix gate). "" means the
// identity has no derivable slug (require an explicit one).
func deriveTenantSlug(identity string) string {
	var base string
	switch {
	case strings.HasPrefix(identity, "github:"):
		base = "gh-" + strings.TrimPrefix(identity, "github:")
	case strings.HasPrefix(identity, "git-root:"):
		sha := strings.TrimPrefix(identity, "git-root:")
		if len(sha) > 12 {
			sha = sha[:12]
		}
		base = "repo-" + sha
	case strings.HasPrefix(identity, "manual:"):
		base = strings.TrimPrefix(identity, "manual:")
	default:
		return ""
	}
	base = sanitizeSlugSegment(base)
	if base == "" {
		return ""
	}
	if len(base) <= 24 {
		return base
	}
	// Overlength: truncate + hash-suffix. Trim a trailing '-' left by the cut so
	// the '-'+hash join never doubles it.
	head := strings.TrimRight(base[:19], "-")
	sum := sha256.Sum256([]byte(identity))
	return head + "-" + hex.EncodeToString(sum[:])[:4]
}

// sanitizeSlugSegment lowercases, maps any run of non-[a-z0-9] to a single '-',
// and trims leading/trailing '-'. It does NOT cap length — deriveTenantSlug owns
// the truncation+hash policy.
func sanitizeSlugSegment(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
