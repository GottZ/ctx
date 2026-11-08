package handler

// The write actions of the model consumer administration (design/01 §4.6,
// wave W01-5): model-consumer-create, -update, -delete, model-key-rotate and
// -revoke. The admin tier of each action sits in the dispatch table
// (manage_table.go: tierSelfService for create and rotate, tierTenantAdmin
// for the rest); the handlers add what the table cannot see:
//
//   - the field tiers — "revoking is always allowed, granting needs a
//     release" (consumerGrant): a tenant-admin may only narrow a consumer
//     unless gateway.enabled and gateway.tenant_self_service are both on, and
//     kind=ctx-peer, a lower prompt_sensitivity, allow_plaintext=true, a higher
//     fair_weight and admission_lane=urgent stay server-admin even then (E3,
//     V-01-a, design/03 §3.1);
//   - the peer row tier (V-01-b): every granting change of a ctx-peer
//     consumer is server-admin only;
//   - the tenant binding (N12 helpers resolveKeyMintTenant /
//     rejectScopeOutsideTenant): tenant_id is server-admin only, tenant_scope
//     must be a scope of the consumer's tenant;
//   - the expiry per kind (keyExpiry): ≤ now + gateway.key_max_lifetime_<kind>,
//     the default when the request names none; no update path (V-01-d);
//   - the model grants (models, migration 164): a target set of alias ids;
//     every id it adds must be one the CONSUMER is offered (modelalias.Offers
//     over the registry snapshot with the scope the consumer resolves in: its
//     tenant_scope, _global only for a ctx-peer — V-02-c; never the caller's
//     home scope, V-0102-h), else 400 at models; adding is granting, removing
//     is revoking;
//   - reveal-once: the plaintext of a key is in the create and the rotate
//     answer, nowhere else.
//
// Every refusal of a tier is the shared 403 "admin key required" — no tier,
// switch or field oracle. A consumer or key outside the caller's reach is the
// same 404 as an unknown or malformed id (V-01-g; the store reaches keys only
// through their consumer).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/store"
)

const (
	// consumerLabelMaxRunes is the label column's own bound
	// (context_model_consumers.label VARCHAR(100), migration 161) — checked
	// here so an overlong label is the caller's 400, not a 22001 500.
	consumerLabelMaxRunes = 100
	// consumerScopeMaxRunes is tenant_scope's column bound (VARCHAR(50)).
	consumerScopeMaxRunes = 50
	// consumerDescriptionMaxRunes bounds the description. Every update copies
	// the whole consumer state into old_value AND new_value of its audit row,
	// so an unbounded description would let a tenant-admin decide the size of
	// audit rows (W01-1 review F-6). A storage bound of the audit trail, not
	// a policy (A-E2: named constant).
	consumerDescriptionMaxRunes = 1000
	// consumerFairWeightMin/Max are the fair_weight CHECK of migration 161.
	consumerFairWeightMin = 1
	consumerFairWeightMax = 100
)

// Error texts of the consumer administration. errConsumerNotFound and
// errModelKeyNotFound are the no-oracle bodies: a foreign, unknown and
// malformed id all get them byte-identically.
const (
	errConsumerNotFound = "model consumer not found"
	errModelKeyNotFound = "model key not found"
)

// consumerDefaults is the registry default snapshot, read once: the fallback
// when the handler has no config store (tests) — fail-closed, since the
// registry defaults keep gateway.enabled and gateway.tenant_self_service off.
var consumerDefaults = sync.OnceValue(config.Defaults)

// consumerPolicy is the consumer administration's slice of ONE _global
// snapshot, taken once per request.
type consumerPolicy struct {
	enabled       bool // gateway.enabled
	selfService   bool // gateway.tenant_self_service
	lifetimeApp   time.Duration
	lifetimePeer  time.Duration
	rotationGrace time.Duration
	expiryWarn    time.Duration
	listMax       int
	chainMax      int
	auditMax      int
}

// consumerPolicy reads the policy from the _global snapshot.
func (h *ManageHandler) consumerPolicy() consumerPolicy {
	var c *config.Config
	if h.cfg != nil {
		c = h.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: gateway.enabled and gateway.* consumer administration are global-only switches over a host-global good (design/01 §4.6), never tenant-scoped.
	}
	if c == nil {
		c = consumerDefaults()
	}
	g := c.Gateway.Consumers
	return consumerPolicy{
		enabled: c.Gateway.Enabled, selfService: g.TenantSelfService,
		lifetimeApp: g.KeyMaxLifetimeApp, lifetimePeer: g.KeyMaxLifetimePeer,
		rotationGrace: g.KeyRotationGrace, expiryWarn: g.KeyExpiryWarn,
		listMax: g.ListMaxLimit, chainMax: g.ChainMax, auditMax: g.AuditTrailMax,
	}
}

// selfServiceOpen is the switch of tierSelfService: gateway.enabled AND
// gateway.tenant_self_service (V-01-c dark launch — with the gateway off, no
// tenant-admin grants anything, whatever the self-service switch says).
func (h *ManageHandler) selfServiceOpen() bool {
	p := h.consumerPolicy()
	return p.enabled && p.selfService
}

// consumerGrant ranks one change of a consumer by the release it needs.
type consumerGrant int

const (
	// grantNone: neutral (label, description) or revoking — a tenant-admin
	// of the consumer's tenant may do it.
	grantNone consumerGrant = iota
	// grantSelfService: granting — a tenant-admin only with self-service on,
	// and never on a ctx-peer consumer (V-01-b).
	grantSelfService
	// grantServerAdmin: server-admin only, self-service or not.
	grantServerAdmin
)

// allowed reports whether ar may make a change of rank g on a consumer (peer:
// kind = ctx-peer). The table tier has already admitted ar as a tenant-admin
// of its own tenant or as a server-admin.
func (p consumerPolicy) allowed(ar *auth.AuthResult, g consumerGrant, peer bool) bool {
	switch {
	case ar.IsServerAdmin(), g == grantNone:
		return true
	case g == grantSelfService && !peer:
		return p.enabled && p.selfService
	}
	return false
}

// accessOf binds the store primitives to the caller: a server-admin reaches
// every tenant, everyone else its own (an empty tenant reaches nothing).
func accessOf(ar *auth.AuthResult) store.ModelAccess {
	if ar.IsServerAdmin() {
		return store.ModelAccess{ServerAdmin: true}
	}
	return store.ModelAccess{TenantID: ar.TenantID}
}

func writeConsumerError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": msg})
}

// writeAdminRequired is the shared tier refusal (the requireAdminAction text).
func writeAdminRequired(w http.ResponseWriter) {
	writeConsumerError(w, http.StatusForbidden, "admin key required")
}

// errFieldTier is the refusal of the field-tier check under the consumer
// lock (store.UpdateConsumerChecked); it surfaces as writeAdminRequired.
var errFieldTier = errors.New("consumer field tier refused")

// modelsError is a refusal of the models field: 400 with param "models".
type modelsError struct{ msg string }

func (e *modelsError) Error() string { return e.msg }

// errModelsNotOffered is the one text for every id the consumer's scope is
// not offered — unknown, deleted, disabled, of an invalid policy, of another
// scope, shadowed: no reason tells an alias of another tenant apart.
func errModelsNotOffered(id string) *modelsError {
	return &modelsError{msg: "models: " + id + " is not a model alias this consumer can be granted " +
		"(unknown, disabled, invalid, outside its tenant_scope, or shadowed by a tenant alias of the same name)"}
}

func writeModelsError(w http.ResponseWriter, e *modelsError) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": e.msg, "param": "models"})
}

// parseModels turns a models list into the canonical target set.
func parseModels(in []string) ([]string, error) {
	// A handler-owned message naming the offending value — never the store's
	// internal error text (Copilot review on #118).
	for _, s := range in {
		if _, err := uuid.Parse(s); err != nil {
			return nil, &modelsError{msg: fmt.Sprintf("models: %q is not a model alias id — every entry must be the id of a model alias", s)}
		}
	}
	ids, err := store.CanonicalAliasIDs(in)
	if err != nil {
		return nil, &modelsError{msg: "models: every entry must be a model alias id"}
	}
	return ids, nil
}

// errModelsNotWired: a grant is to be added, but the handler has no alias
// registry — no id can be judged offered (fail-closed, 503). Removing grants
// needs no registry.
var errModelsNotWired = errors.New("model aliases not wired")

func writeModelsNotWired(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": errModelsNotWired.Error()})
}

// modelSnapshot returns the alias registry's snapshot, nil without one.
func (h *ManageHandler) modelSnapshot() *modelalias.Snapshot {
	if h.modelAliases == nil {
		return nil
	}
	return h.modelAliases.Snapshot()
}

// offerScope is the scope a consumer is offered aliases in — the scope /v1
// resolves its model names in: its tenant_scope (nil = _global only), and
// _global only for a ctx-peer, which never resolves against a tenant alias
// (design/05 V-02-c; W02-05 Resolve: Lookup(…, "")). Validating a peer
// against its tenant_scope would accept a grant /v1 never honours (and that
// still holds a delete of the alias back, V-02-b) and refuse the _global
// alias it does resolve to.
func offerScope(kind string, tenantScope *string) *string {
	if kind == "ctx-peer" {
		return nil
	}
	return tenantScope
}

// addedModels returns the ids of next that cur does not hold (both sorted
// sets) — the grants a target set adds.
func addedModels(cur, next []string) []string {
	have := make(map[string]bool, len(cur))
	for _, id := range cur {
		have[id] = true
	}
	var out []string
	for _, id := range next {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out
}

// checkModelsOffered refuses the first id of add that Offers does not list
// for tenantScope (nil = _global aliases only; see offerScope) — V-0102-h: the
// scope is the consumer's, as /v1 resolves it. A grant the consumer already
// holds is not judged again: keeping it grants nothing, and the request time
// filters a grant that became invisible (design/01 §3.8). Without a snapshot
// an addition is errModelsNotWired.
func checkModelsOffered(snap *modelalias.Snapshot, tenantScope *string, add []string) error {
	if len(add) == 0 {
		return nil
	}
	if snap == nil {
		return errModelsNotWired
	}
	scope := ""
	if tenantScope != nil {
		scope = *tenantScope
	}
	offered := map[string]bool{}
	for _, a := range modelalias.Offers(snap, scope) {
		offered[a.ID] = true
	}
	for _, id := range add {
		if !offered[id] {
			return errModelsNotOffered(id)
		}
	}
	return nil
}

// writeConsumerStoreError maps a store error of the consumer primitives.
func writeConsumerStoreError(w http.ResponseWriter, r *http.Request, err error, op string) {
	var (
		pgErr *pgconn.PgError
		mErr  *modelsError
	)
	switch {
	case errors.Is(err, errFieldTier):
		writeAdminRequired(w)
	case errors.As(err, &mErr):
		writeModelsError(w, mErr)
	case errors.Is(err, errModelsNotWired):
		writeModelsNotWired(w)
	case errors.Is(err, store.ErrModelGrantAliasGone):
		// Deleted between the snapshot check and the insert.
		writeModelsError(w, &modelsError{msg: "models: a model alias of the list was deleted meanwhile — reload the aliases and retry"})
	case errors.Is(err, store.ErrModelConsumerNotFound):
		writeConsumerError(w, http.StatusNotFound, errConsumerNotFound)
	case errors.Is(err, store.ErrModelKeyNotFound):
		writeConsumerError(w, http.StatusNotFound, errModelKeyNotFound)
	case errors.Is(err, store.ErrTenantNotFound):
		writeConsumerError(w, http.StatusNotFound, "tenant not found")
	case errors.Is(err, store.ErrModelConsumerCapExceeded):
		writeConsumerError(w, http.StatusTooManyRequests, "tenant model consumer quota exceeded")
	case errors.Is(err, store.ErrModelConsumerLabelTaken):
		writeConsumerError(w, http.StatusConflict, "a model consumer with this label already exists in the tenant")
	case errors.Is(err, store.ErrModelConsumerInactive):
		writeConsumerError(w, http.StatusConflict, "model consumer is not active")
	case errors.Is(err, store.ErrRotationGraceOutOfRange):
		writeConsumerError(w, http.StatusBadRequest, fmt.Sprintf("grace must lie between 0 and %d seconds", int64(store.MaxRotationGrace/time.Second)))
	case errors.Is(err, store.ErrModelKeyExpiryInPast):
		writeConsumerError(w, http.StatusBadRequest, "expires_at must lie in the future")
	case errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "ck_model_consumers_plaintext_private":
		writeConsumerError(w, http.StatusBadRequest, "allow_plaintext needs allowed_cidrs inside private networks (RFC 1918, ULA, loopback)")
	default:
		slog.Error("manage: "+op+" failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeConsumerError(w, http.StatusInternalServerError, "internal error")
	}
}

// decodeConsumerPayload decodes a manage payload, refusing unknown fields: a
// field the action does not know (expires_at on an update — V-01-d) must be an
// error, never silently dropped.
func decodeConsumerPayload(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data after the payload")
	}
	return nil
}

// consumerSensitivities is the prompt_sensitivity vocabulary (161 CHECK).
var consumerSensitivities = map[string]bool{"credentials": true, "personal": true, "internal": true, "public": true}

// sensitivityLowered reports whether to is less strict than from.
func sensitivityLowered(from, to string) bool {
	return from != to && backends.MaxSensitivity(backends.Sensitivity(from), backends.Sensitivity(to)) == backends.Sensitivity(from)
}

// validateLabel checks a consumer label (required, column bound).
func validateLabel(label string) error {
	if strings.TrimSpace(label) == "" {
		return errors.New("label is required")
	}
	if strings.ContainsRune(label, 0) {
		return errors.New("label must not contain NUL bytes")
	}
	if utf8.RuneCountInString(label) > consumerLabelMaxRunes {
		return fmt.Errorf("label must be at most %d characters", consumerLabelMaxRunes)
	}
	return nil
}

func validateDescription(d string) error {
	if strings.ContainsRune(d, 0) {
		return errors.New("description must not contain NUL bytes")
	}
	if utf8.RuneCountInString(d) > consumerDescriptionMaxRunes {
		return fmt.Errorf("description must be at most %d characters", consumerDescriptionMaxRunes)
	}
	return nil
}

// validateTenantScope checks the shape of a tenant_scope; whether the tenant
// owns it is rejectScopeOutsideTenant's question.
func validateTenantScope(scope string) error {
	switch {
	case scope == "":
		return errors.New("tenant_scope must not be empty; null means _global backends only")
	case strings.HasPrefix(scope, "_"):
		return errors.New("scope names starting with '_' are reserved: " + scope)
	case utf8.RuneCountInString(scope) > consumerScopeMaxRunes:
		return fmt.Errorf("tenant_scope must be at most %d characters", consumerScopeMaxRunes)
	}
	return nil
}

// validatePolicyValues checks the enumerated and ranged policy fields.
func validatePolicyValues(sens, lane *string, weight *int16) error {
	if sens != nil && !consumerSensitivities[*sens] {
		return errors.New("prompt_sensitivity must be one of credentials, personal, internal, public")
	}
	if lane != nil && *lane != "urgent" && *lane != "bulk" {
		return errors.New("admission_lane must be urgent or bulk")
	}
	if weight != nil && (*weight < consumerFairWeightMin || *weight > consumerFairWeightMax) {
		return fmt.Errorf("fair_weight must lie between %d and %d", consumerFairWeightMin, consumerFairWeightMax)
	}
	return nil
}

// parseCIDRs checks an allowed_cidrs list: at least one entry (an empty list
// would refuse every source, also a request without a client address — W01-1
// review F-7; null is "any source"), each a canonical prefix without host bits
// (the cidr column type refuses those).
func parseCIDRs(in []string) ([]netip.Prefix, error) {
	if len(in) == 0 {
		return nil, errors.New("allowed_cidrs must not be empty; null means any source")
	}
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("allowed_cidrs: %q is not a CIDR prefix", s)
		}
		if p != p.Masked() {
			return nil, fmt.Errorf("allowed_cidrs: %q has host bits set (use %s)", s, p.Masked())
		}
		out = append(out, p)
	}
	return out, nil
}

// cidrsNarrow reports whether every prefix of next lies inside some prefix of
// cur — the list is narrowed (or unchanged), not widened. cur is the stored
// list (non-nil; NULL is handled by the caller). An unparsable stored entry
// contains nothing (fail-closed: the change then counts as widening).
func cidrsNarrow(cur []string, next []netip.Prefix) bool {
	have := make([]netip.Prefix, 0, len(cur))
	for _, s := range cur {
		if p, err := netip.ParsePrefix(s); err == nil {
			have = append(have, p.Masked())
		}
	}
	for _, n := range next {
		inside := false
		for _, o := range have {
			if o.Addr().Is4() == n.Addr().Is4() && o.Bits() <= n.Bits() && o.Contains(n.Addr()) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

func prefixStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

// keyExpiry resolves the expiry of a new key of a consumer of kind at now
// (E-01-4): the request's value if it lies in the future and within the kind's
// ceiling, the ceiling itself when the request names none, and none at all
// when the kind's lifetime setting is 0 (no ceiling). The setting is read
// once per request; a key keeps its expiry when the setting changes later.
func (p consumerPolicy) keyExpiry(kind string, requested *time.Time, now time.Time) (*time.Time, error) {
	key, max := "gateway.key_max_lifetime_app", p.lifetimeApp
	if kind == "ctx-peer" {
		key, max = "gateway.key_max_lifetime_peer", p.lifetimePeer
	}
	if requested == nil {
		if max <= 0 {
			return nil, nil
		}
		at := now.Add(max)
		return &at, nil
	}
	if !requested.After(now) {
		return nil, errors.New("expires_at must lie in the future")
	}
	if max > 0 && requested.After(now.Add(max)) {
		return nil, fmt.Errorf("expires_at lies beyond %s (%d s from now)", key, int64(max/time.Second))
	}
	return requested, nil
}

// consumerCreateRequest is the payload of model-consumer-create. TenantID is
// server-admin only (resolveKeyMintTenant). Models are the alias ids to grant
// (null or absent: none); an unknown field is a 400.
type consumerCreateRequest struct {
	Label             string     `json:"label"`
	Description       string     `json:"description"`
	TenantID          string     `json:"tenant_id"`
	TenantScope       *string    `json:"tenant_scope"`
	Kind              string     `json:"kind"`
	AllModels         bool       `json:"all_models"`
	Models            []string   `json:"models"`
	PromptSensitivity *string    `json:"prompt_sensitivity"`
	FairWeight        *int16     `json:"fair_weight"`
	AdmissionLane     *string    `json:"admission_lane"`
	AllowedCIDRs      []string   `json:"allowed_cidrs"`
	AllowPlaintext    bool       `json:"allow_plaintext"`
	ExpiresAt         *time.Time `json:"expires_at"`
	// Limits are the consumer's starting limits per op (FOLGE-Z Z3): each
	// op's dimensions are written as the consumer's row of the CALLER's tier
	// in the create's transaction — validated like gateway-limits-set.
	Limits *consumerCreateLimits `json:"limits"`
}

// consumerCreateLimits is the limits object of model-consumer-create: one
// optional value set per op (an unknown op is an unknown field, 400).
type consumerCreateLimits struct {
	Chat   *gatewaylimits.Values `json:"chat"`
	Embed  *gatewaylimits.Values `json:"embed"`
	Rerank *gatewaylimits.Values `json:"rerank"`
}

// validate checks the payload's shape (no tier, no database).
func (d *consumerCreateRequest) validate() ([]netip.Prefix, error) {
	if err := validateLabel(d.Label); err != nil {
		return nil, err
	}
	if err := validateDescription(d.Description); err != nil {
		return nil, err
	}
	if d.Kind == "" {
		d.Kind = "app"
	}
	if d.Kind != "app" && d.Kind != "ctx-peer" {
		return nil, errors.New("kind must be app or ctx-peer")
	}
	if d.TenantScope != nil {
		if err := validateTenantScope(*d.TenantScope); err != nil {
			return nil, err
		}
	}
	if err := validatePolicyValues(d.PromptSensitivity, d.AdmissionLane, d.FairWeight); err != nil {
		return nil, err
	}
	if d.Models != nil {
		ids, err := parseModels(d.Models)
		if err != nil {
			return nil, err
		}
		d.Models = ids
	}
	if d.AllowedCIDRs == nil {
		return nil, nil
	}
	return parseCIDRs(d.AllowedCIDRs)
}

// grant ranks a create against the fail-closed column defaults of migration
// 161 (app, credentials, fair_weight 1, bulk, no plaintext): creating is
// itself granting; a field above those defaults may be server-admin only.
func (d *consumerCreateRequest) grant() consumerGrant {
	switch {
	case d.Kind == "ctx-peer",
		d.PromptSensitivity != nil && sensitivityLowered("credentials", *d.PromptSensitivity),
		d.AllowPlaintext,
		d.FairWeight != nil && *d.FairWeight > consumerFairWeightMin,
		d.AdmissionLane != nil && *d.AdmissionLane == "urgent":
		return grantServerAdmin
	}
	return grantSelfService
}

func (h *ManageHandler) handleModelConsumerCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d consumerCreateRequest
	if err := decodeConsumerPayload(req.Data, &d); err != nil {
		writeConsumerError(w, http.StatusBadRequest, "invalid data: "+err.Error())
		return
	}
	cidrs, err := d.validate()
	var mErr *modelsError
	switch {
	case errors.As(err, &mErr):
		writeModelsError(w, mErr)
		return
	case err != nil:
		writeConsumerError(w, http.StatusBadRequest, err.Error())
		return
	}
	pol := h.consumerPolicy()
	if !pol.allowed(ar, d.grant(), false) {
		writeAdminRequired(w)
		return
	}
	if len(d.Models) > 0 && h.modelAliases == nil {
		writeModelsNotWired(w)
		return
	}
	var scopes []string
	if d.TenantScope != nil {
		scopes = []string{*d.TenantScope}
	}
	// The tenant first (tenant_id is server-admin only), then its scope.
	tenant, handled := h.resolveKeyMintTenant(w, r, d.TenantID, nil, ar)
	if handled {
		return
	}
	if _, err := uuid.Parse(tenant); err != nil {
		writeConsumerError(w, http.StatusNotFound, "tenant not found") // as the store answers an unknown one
		return
	}
	// The consumer's tenant must own its scope for EVERY caller (V-0102-h):
	// unlike an api key's server-admin self-mint, a server-admin creating in
	// its own tenant cannot hand the consumer another tenant's scope either —
	// gate 4 of ctx_model_auth would refuse it at every request anyway.
	if h.rejectScopeOutsideTenant(w, r, scopes, tenant) {
		return
	}
	// Every model is a new grant, judged against the new consumer's scope.
	if err := checkModelsOffered(h.modelSnapshot(), offerScope(d.Kind, d.TenantScope), d.Models); err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-create")
		return
	}
	// Starting limits (FOLGE-Z Z3): validated before anything is written.
	limits, ok := h.consumerStartLimits(w, ar, tenant, d.Limits)
	if !ok {
		return
	}
	now := time.Now()
	expiry, err := pol.keyExpiry(d.Kind, d.ExpiresAt, now)
	if err != nil {
		writeConsumerError(w, http.StatusBadRequest, err.Error())
		return
	}
	spec := store.ModelConsumerSpec{Limits: limits,
		TenantID: tenant, Label: d.Label, Description: d.Description, Kind: d.Kind,
		TenantScope: d.TenantScope, AllModels: d.AllModels, Models: d.Models, AllowPlaintext: d.AllowPlaintext,
	}
	if d.PromptSensitivity != nil {
		spec.PromptSensitivity = *d.PromptSensitivity
	}
	if d.FairWeight != nil {
		spec.FairWeight = *d.FairWeight
	}
	if d.AdmissionLane != nil {
		spec.AdmissionLane = *d.AdmissionLane
	}
	if cidrs != nil {
		spec.AllowedCIDRs = prefixStrings(cidrs)
	}
	c, k, key, err := store.CreateConsumerWithKey(r.Context(), h.pool, spec, expiry, auditStamp(r, ar))
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-create")
		return
	}
	if len(limits) > 0 {
		h.reloadGL(r) // the next request meters under the new rows
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"consumer": h.freshConsumerView(r, ar, pol, c, []store.ModelKeyInfo{k}),
		"api_key":  key.Plaintext(), // shown once (reveal-once, K-05-3)
		// What the meter applies, per op, each dimension with its source —
		// no hidden default (FOLGE-Z Z3).
		"effective_limits": h.consumerEffectiveLimits(c.ID, c.TenantID, startLimitRows(c.ID, limits), ar.IsServerAdmin()),
	})
}

// freshConsumerView renders a consumer just written: re-read (tenant slug,
// creator, live keys), or — when that read fails after the commit — the
// written row itself, so a committed create never loses its one answer.
func (h *ManageHandler) freshConsumerView(r *http.Request, ar *auth.AuthResult, pol consumerPolicy, written store.ModelConsumer, keys []store.ModelKeyInfo) consumerView {
	now := time.Now()
	c, err := store.GetConsumer(r.Context(), h.pool, written.ID, accessOf(ar), now)
	if err != nil {
		slog.Warn("manage: consumer re-read after write failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		c = written
		if keys != nil {
			c.Keys, c.KeysTotal = keys, len(keys)
		}
	}
	return newConsumerView(c, c.Keys, ar, now, pol.expiryWarn)
}

// consumerUpdateFields is the closed field set of model-consumer-update.
// expires_at is deliberately absent (V-01-d: a new expiry needs a new key —
// model-key-rotate), so are kind, tenant_id and the peer binding. models is
// the target set of granted alias ids.
var consumerUpdateFields = map[string]bool{
	"id": true, "label": true, "description": true, "tenant_scope": true, "all_models": true,
	"models":             true,
	"prompt_sensitivity": true, "fair_weight": true, "admission_lane": true,
	"allowed_cidrs": true, "allow_plaintext": true, "active": true,
}

// consumerUpdate is a decoded update: the store patch plus what the field
// tiers compare (the parsed CIDR list).
type consumerUpdate struct {
	id    string
	patch store.ModelConsumerPatch
	cidrs []netip.Prefix
}

// decodeConsumerUpdate decodes model-consumer-update's payload. A field present
// as null is distinct from an absent one where the column is nullable
// (tenant_scope, allowed_cidrs: null writes NULL); elsewhere null is refused.
func decodeConsumerUpdate(raw json.RawMessage) (consumerUpdate, error) {
	var u consumerUpdate
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return u, errors.New("invalid data: expected an object with id and the fields to change")
	}
	for k := range fields {
		if !consumerUpdateFields[k] {
			return u, fmt.Errorf("unknown field %q (expiry changes need model-key-rotate)", k)
		}
	}
	if err := json.Unmarshal(fields["id"], &u.id); err != nil || u.id == "" {
		return u, errors.New("id is required")
	}
	p := &u.patch
	for _, f := range []struct {
		name string
		dst  any
	}{
		{"label", &p.Label}, {"description", &p.Description}, {"all_models", &p.AllModels},
		{"prompt_sensitivity", &p.PromptSensitivity}, {"fair_weight", &p.FairWeight},
		{"admission_lane", &p.AdmissionLane}, {"allow_plaintext", &p.AllowPlaintext}, {"active", &p.Active},
	} {
		raw, ok := fields[f.name]
		if !ok {
			continue
		}
		if string(bytes.TrimSpace(raw)) == "null" {
			return u, fmt.Errorf("%s must not be null", f.name)
		}
		if err := json.Unmarshal(raw, f.dst); err != nil {
			return u, fmt.Errorf("%s has the wrong type", f.name)
		}
	}
	if err := decodeNullableScope(fields, p); err != nil {
		return u, err
	}
	if err := decodeModels(fields, p); err != nil {
		return u, err
	}
	var err error
	u.cidrs, err = decodeNullableCIDRs(fields, p)
	return u, err
}

// decodeModels reads models: a list of alias ids (the target set; [] removes
// every grant), never null.
func decodeModels(fields map[string]json.RawMessage, p *store.ModelConsumerPatch) error {
	raw, ok := fields["models"]
	if !ok {
		return nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return &modelsError{msg: "models must not be null — [] removes every grant"}
	}
	var in []string
	if err := json.Unmarshal(raw, &in); err != nil {
		return &modelsError{msg: "models must be a list of model alias ids"}
	}
	ids, err := parseModels(in)
	if err != nil {
		return err
	}
	p.Models = &ids
	return nil
}

func decodeNullableScope(fields map[string]json.RawMessage, p *store.ModelConsumerPatch) error {
	raw, ok := fields["tenant_scope"]
	if !ok {
		return nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		p.ClearTenantScope = true
		return nil
	}
	if err := json.Unmarshal(raw, &p.TenantScope); err != nil {
		return errors.New("tenant_scope must be a string or null")
	}
	return validateTenantScope(*p.TenantScope)
}

func decodeNullableCIDRs(fields map[string]json.RawMessage, p *store.ModelConsumerPatch) ([]netip.Prefix, error) {
	raw, ok := fields["allowed_cidrs"]
	if !ok {
		return nil, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		p.ClearAllowedCIDRs = true
		return nil, nil
	}
	var in []string
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errors.New("allowed_cidrs must be a list of CIDR prefixes or null")
	}
	ps, err := parseCIDRs(in)
	if err != nil {
		return nil, err
	}
	p.AllowedCIDRs = prefixStrings(ps)
	return ps, nil
}

// grant ranks the update against the consumer as it stands (design/01 §4.6:
// "revoking is always allowed, granting needs a release"). Unchanged values
// rank as nothing.
func (u consumerUpdate) grant(cur store.ModelConsumer) consumerGrant {
	if u.serverAdminOnly(cur) {
		return grantServerAdmin
	}
	if u.widens(cur) {
		return grantSelfService
	}
	return grantNone
}

// widens reports a granting change: one that opens the consumer further.
func (u consumerUpdate) widens(cur store.ModelConsumer) bool {
	p := u.patch
	switch {
	case p.TenantScope != nil && (cur.TenantScope == nil || *cur.TenantScope != *p.TenantScope):
		return true // opens tenant-private backends (B11)
	case p.AllModels != nil && *p.AllModels && !cur.AllModels:
		return true
	case p.Models != nil && len(addedModels(cur.Models, *p.Models)) > 0:
		return true // a new release of a model alias (K11); removing one revokes
	case p.Active != nil && *p.Active && !cur.Active:
		return true // reactivation; revives no key (I-K8)
	case p.ClearAllowedCIDRs && cur.AllowedCIDRs != nil:
		return true // NULL = any source
	case u.cidrs != nil && cur.AllowedCIDRs != nil && !cidrsNarrow(cur.AllowedCIDRs, u.cidrs):
		return true
	}
	return false
}

// serverAdminOnly reports a change that stays server-admin even with
// self-service (E3, V-01-a, design/03 §3.1).
func (u consumerUpdate) serverAdminOnly(cur store.ModelConsumer) bool {
	p := u.patch
	switch {
	case p.PromptSensitivity != nil && sensitivityLowered(cur.PromptSensitivity, *p.PromptSensitivity):
		return true // egress to less trusted backends (I-K9)
	case p.FairWeight != nil && *p.FairWeight > cur.FairWeight:
		return true // the GPU share is host-global
	case p.AdmissionLane != nil && *p.AdmissionLane == "urgent" && cur.AdmissionLane != "urgent":
		return true // above ctx's own background work (design/03 §3.1)
	case p.AllowPlaintext != nil && *p.AllowPlaintext && !cur.AllowPlaintext:
		return true // credential transport without TLS (B24)
	}
	return false
}

// scopeAfter is the consumer's tenant_scope once the update has run: the
// patch's value, NULL when it clears the scope, else the current one.
func (u consumerUpdate) scopeAfter(cur store.ModelConsumer) *string {
	switch {
	case u.patch.ClearTenantScope:
		return nil
	case u.patch.TenantScope != nil:
		return u.patch.TenantScope
	}
	return cur.TenantScope
}

// checkModels judges the grants the update adds against what the consumer is
// offered after the update (kind is immutable; the scope may change with it).
func (u consumerUpdate) checkModels(snap *modelalias.Snapshot, cur store.ModelConsumer) error {
	if u.patch.Models == nil {
		return nil
	}
	return checkModelsOffered(snap, offerScope(cur.Kind, u.scopeAfter(cur)), addedModels(cur.Models, *u.patch.Models))
}

func (h *ManageHandler) handleModelConsumerUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	u, err := decodeConsumerUpdate(req.Data)
	var mErr *modelsError
	switch {
	case errors.As(err, &mErr):
		writeModelsError(w, mErr)
		return
	case err != nil:
		writeConsumerError(w, http.StatusBadRequest, err.Error())
		return
	}
	if p := u.patch; p.Label != nil || p.Description != nil {
		if err := validateUpdateTexts(p); err != nil {
			writeConsumerError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := validatePolicyValues(u.patch.PromptSensitivity, u.patch.AdmissionLane, u.patch.FairWeight); err != nil {
		writeConsumerError(w, http.StatusBadRequest, err.Error())
		return
	}
	pol := h.consumerPolicy()
	access := accessOf(ar)
	cur, err := store.GetConsumer(r.Context(), h.pool, u.id, access, time.Now())
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-update")
		return
	}
	// First, unlocked: refuse early with the tier text before any scope
	// lookup can answer differently. The authoritative check repeats under
	// the consumer lock below.
	if !pol.allowed(ar, u.grant(cur), cur.Kind == "ctx-peer") {
		writeAdminRequired(w)
		return
	}
	if u.patch.TenantScope != nil && h.rejectScopeOutsideTenant(w, r, []string{*u.patch.TenantScope}, cur.TenantID) {
		return
	}
	// One registry snapshot for both judgements of the grants: unlocked here,
	// then against the consumer as it stands under its lock. Only an added id
	// needs it — removing grants works without the registry.
	snap := h.modelSnapshot()
	if err := u.checkModels(snap, cur); err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-update")
		return
	}
	check := func(locked store.ModelConsumer) error {
		if !pol.allowed(ar, u.grant(locked), locked.Kind == "ctx-peer") {
			return errFieldTier
		}
		return u.checkModels(snap, locked)
	}
	c, err := store.UpdateConsumerChecked(r.Context(), h.pool, u.id, access, u.patch, check, auditStamp(r, ar))
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-update")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "consumer": h.freshConsumerView(r, ar, pol, c, nil)})
}

func validateUpdateTexts(p store.ModelConsumerPatch) error {
	if p.Label != nil {
		if err := validateLabel(*p.Label); err != nil {
			return err
		}
	}
	if p.Description != nil {
		return validateDescription(*p.Description)
	}
	return nil
}

// Revoking is always allowed (design/01 §4.6): both actions run for a
// tenant-admin of the consumer's tenant regardless of self-service. After
// the commit both end the running requests of what they revoked through the
// door's in-flight registry (W01-8) and answer cancelled_inflight: how many
// requests were running on it in THIS process — another ctx process on the
// same database ends its own through the NOTIFY of migration 165 and is not
// counted. null only without a registry (unknown — never a 0 that would
// claim none ran, V-01-e).

// cancelledInflight is the cancelled_inflight of an answer: end(id) through
// the registry, null without one.
func (h *ManageHandler) cancelledInflight(end func(*InflightRegistry) int) any {
	if h.inflight == nil {
		return nil
	}
	return end(h.inflight)
}

// revocationID is the one spelling both halves of a revocation use: the
// store takes any spelling of a uuid ($1::uuid), the in-flight registry
// only the canonical one its ids came in (store.CanonicalUUID, PostgreSQL's
// text form) — an upper-case id would revoke the key and end nothing
// (Fremd-Review 2 F1). An id CanonicalUUID does not take names no row: the
// store's not-found answer, before anything is revoked.
func revocationID(raw string) (string, bool) { return store.CanonicalUUID(raw) }

func (h *ManageHandler) handleModelConsumerDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d struct {
		ID string `json:"id"`
	}
	if err := decodeConsumerPayload(req.Data, &d); err != nil || d.ID == "" {
		writeConsumerError(w, http.StatusBadRequest, "id is required")
		return
	}
	id, ok := revocationID(d.ID)
	if !ok {
		writeConsumerStoreError(w, r, store.ErrModelConsumerNotFound, "model-consumer-delete")
		return
	}
	if err := store.DeleteConsumer(r.Context(), h.pool, id, accessOf(ar), auditStamp(r, ar)); err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-delete")
		return
	}
	n := h.cancelledInflight(func(reg *InflightRegistry) int { return reg.CancelConsumer(id) })
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "cancelled_inflight": n})
}

func (h *ManageHandler) handleModelKeyRevoke(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d struct {
		KeyID string `json:"key_id"`
	}
	if err := decodeConsumerPayload(req.Data, &d); err != nil || d.KeyID == "" {
		writeConsumerError(w, http.StatusBadRequest, "key_id is required")
		return
	}
	id, ok := revocationID(d.KeyID)
	if !ok {
		writeConsumerStoreError(w, r, store.ErrModelKeyNotFound, "model-key-revoke")
		return
	}
	if err := store.RevokeKey(r.Context(), h.pool, id, accessOf(ar), auditStamp(r, ar)); err != nil {
		writeConsumerStoreError(w, r, err, "model-key-revoke")
		return
	}
	n := h.cancelledInflight(func(reg *InflightRegistry) int { return reg.CancelKey(id) })
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "cancelled_inflight": n})
}

// consumerRotateRequest: grace in seconds (0 = revoke the predecessor at once,
// the leak case; absent = gateway.key_rotation_grace); expires_at follows the
// kind's rule like a mint.
type consumerRotateRequest struct {
	ConsumerID string     `json:"consumer_id"`
	Grace      *int64     `json:"grace"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

func (h *ManageHandler) handleModelKeyRotate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d consumerRotateRequest
	if err := decodeConsumerPayload(req.Data, &d); err != nil {
		writeConsumerError(w, http.StatusBadRequest, "invalid data: "+err.Error())
		return
	}
	if d.ConsumerID == "" {
		writeConsumerError(w, http.StatusBadRequest, "consumer_id is required")
		return
	}
	pol := h.consumerPolicy()
	grace := pol.rotationGrace
	if d.Grace != nil {
		if *d.Grace < 0 || *d.Grace > int64(store.MaxRotationGrace/time.Second) {
			writeConsumerError(w, http.StatusBadRequest, fmt.Sprintf("grace must lie between 0 and %d seconds", int64(store.MaxRotationGrace/time.Second)))
			return
		}
		grace = time.Duration(*d.Grace) * time.Second
	}
	access := accessOf(ar)
	now := time.Now()
	// kind is immutable (no patch reaches it), so the read before the
	// rotation's own lock decides the peer row tier for good.
	cur, err := store.GetConsumer(r.Context(), h.pool, d.ConsumerID, access, now)
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-key-rotate")
		return
	}
	if !pol.allowed(ar, grantSelfService, cur.Kind == "ctx-peer") {
		writeAdminRequired(w) // V-01-b: a peer's new key is server-admin only
		return
	}
	expiry, err := pol.keyExpiry(cur.Kind, d.ExpiresAt, now)
	if err != nil {
		writeConsumerError(w, http.StatusBadRequest, err.Error())
		return
	}
	k, key, err := store.RotateKey(r.Context(), h.pool, d.ConsumerID, access, grace, expiry, auditStamp(r, ar))
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-key-rotate")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"key":     newKeyView(k, k.ID, time.Now(), pol.expiryWarn),
		"api_key": key.Plaintext(), // shown once (reveal-once, K-05-3)
	})
}
