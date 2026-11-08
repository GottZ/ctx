package handler

// The manage actions gateway-limits-get|set|delete (wave W06-15, design/06
// §3.5, §4.3; docs/api.md "Gateway limits"): the meter's limits as data per
// consumer, tenant, instance and tenant envelope. All three are
// tierTenantAdmin at the table; the field tiers live here:
//
//   - The tier of a write is the caller's (server-admin ⇒ server row,
//     tenant-admin ⇒ tenant row), never a body field.
//   - A tenant-admin reaches its own consumers and its own tenant; another
//     tenant's subject, an unknown or a malformed id answer the same 404 as
//     the consumer actions (no oracle); subject_kind instance or
//     tenant_envelope is 403 for a tenant-admin.
//   - A tenant-admin write narrows: every value at or under its ceiling
//     (gatewaylimits.Snapshot.Ceiling; a missing server row is the setting's
//     value, never unlimited), allow_external only false,
//     allow_unmetered_external never — else 400 "limit widens: <field>" with
//     param = the field. A server-admin is free.
//   - Values: > 0 or null (no 0 sentinel), the envelope's dimensions only,
//     max_concurrency at most dispatch.external_queue_per_consumer — else 400
//     "invalid data: …" with param.
//   - After a committed write the registry reloads synchronously (the next
//     request meters under it), the audit trigger writes one row.

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/store"
)

// GatewayLimitsGetRequest is the data of gateway-limits-get: the subject and
// optionally one op (all three ops without).
type GatewayLimitsGetRequest struct {
	SubjectKind string `json:"subject_kind"`
	ConsumerID  string `json:"consumer_id,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	Op          string `json:"op,omitempty"`
}

// GatewayLimitsSetRequest is the data of gateway-limits-set: the subject, the
// op and the dimensions (a full replacement of the caller's tier row; an
// absent or null dimension inherits).
type GatewayLimitsSetRequest struct {
	SubjectKind string `json:"subject_kind"`
	ConsumerID  string `json:"consumer_id,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	Op          string `json:"op"`
	gatewaylimits.Values
}

// GatewayLimitsDeleteRequest is the data of gateway-limits-delete.
type GatewayLimitsDeleteRequest struct {
	SubjectKind string `json:"subject_kind"`
	ConsumerID  string `json:"consumer_id,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	Op          string `json:"op"`
}

// GatewayLimitsOp is one op of a gateway-limits-get answer.
type GatewayLimitsOp struct {
	// Server and Tenant are the stored rows of the subject for the op
	// (nil = no row).
	Server *gatewaylimits.Values `json:"server"`
	Tenant *gatewaylimits.Values `json:"tenant"`
	// Ceiling is the bound a tenant-tier write must stay at or under (a nil
	// dimension = unbounded); absent for the instance and the envelope.
	Ceiling *gatewaylimits.Values `json:"ceiling,omitempty"`
	// Effective are the resolved values (consumer subject: that consumer;
	// tenant subject: a consumer of the tenant without rows of its own).
	Effective *GatewayLimitsEffective `json:"effective,omitempty"`
	// Sources names the level that set each effective dimension.
	Sources map[string]gatewaylimits.Source `json:"sources,omitempty"`
}

// GatewayLimitsEffective is the wire form of gatewaylimits.Effective: 0 in a
// budget dimension is unbounded and serialised as null.
type GatewayLimitsEffective struct {
	RPM                    int64    `json:"rpm"`
	TPM                    int64    `json:"tpm"`
	MaxConcurrency         int64    `json:"max_concurrency"`
	DailyTokens            *int64   `json:"daily_tokens"`
	MonthlyTokens          *int64   `json:"monthly_tokens"`
	DailyRequests          *int64   `json:"daily_requests"`
	MonthlyCostUSD         *float64 `json:"monthly_cost_usd"`
	AllowExternal          bool     `json:"allow_external"`
	AllowUnmeteredExternal bool     `json:"allow_unmetered_external"`
	// TenantEnvelope and InstanceEnvelope: the aggregates (instance only for
	// a server-admin).
	TenantEnvelope   *GatewayLimitsBudget `json:"tenant_envelope,omitempty"`
	InstanceEnvelope *GatewayLimitsBudget `json:"instance_envelope,omitempty"`
}

// GatewayLimitsBudget is an envelope on the wire (null = unbounded).
type GatewayLimitsBudget struct {
	DailyTokens    *int64   `json:"daily_tokens"`
	MonthlyTokens  *int64   `json:"monthly_tokens"`
	DailyRequests  *int64   `json:"daily_requests"`
	MonthlyCostUSD *float64 `json:"monthly_cost_usd"`
}

// GatewayLimitsView is the "limits" object of a gateway-limits-get answer.
type GatewayLimitsView struct {
	SubjectKind string                     `json:"subject_kind"`
	ConsumerID  string                     `json:"consumer_id,omitempty"`
	TenantID    string                     `json:"tenant_id,omitempty"`
	Ops         map[string]GatewayLimitsOp `json:"ops"`
}

// SetGatewayLimits wires the limits registry (reloaded after each committed
// write) and the settings floor per op (the ceiling of a tenant-admin write
// and the effective values). Boot contract like SetModelAliases: called
// before the server serves; nil leaves the actions answering 503.
func (h *ManageHandler) SetGatewayLimits(reg *gatewaylimits.Registry, defaults func(op keyquota.Op) gatewaylimits.Defaults) {
	h.gatewayLimits = reg
	h.gatewayLimitDefaults = defaults
}

// gatewayLimitsOps is the op order of a get without op.
var gatewayLimitsOps = []keyquota.Op{keyquota.OpChat, keyquota.OpEmbed, keyquota.OpRerank}

// glSubject is a parsed request subject.
type glSubject struct {
	kind                 gatewaylimits.SubjectKind
	consumerID, tenantID string
}

// parseGLSubject parses subject_kind and the id it names; a refusal is
// written (400 with param) and ok is false.
func parseGLSubject(w http.ResponseWriter, kind, consumerID, tenantID string) (glSubject, bool) {
	k, ok := gatewaylimits.ParseSubjectKind(kind)
	if !ok {
		writeGLInvalid(w, "subject_kind", "subject_kind must be consumer, tenant, instance or tenant_envelope")
		return glSubject{}, false
	}
	switch {
	case k == gatewaylimits.SubjectConsumer && (consumerID == "" || tenantID != ""):
		writeGLInvalid(w, "consumer_id", "a consumer row names consumer_id and no tenant_id")
		return glSubject{}, false
	case (k == gatewaylimits.SubjectTenant || k == gatewaylimits.SubjectTenantEnvelope) && (tenantID == "" || consumerID != ""):
		writeGLInvalid(w, "tenant_id", "a tenant row names tenant_id and no consumer_id")
		return glSubject{}, false
	case k == gatewaylimits.SubjectInstance && (tenantID != "" || consumerID != ""):
		writeGLInvalid(w, "subject_kind", "the instance row names no consumer_id or tenant_id")
		return glSubject{}, false
	}
	return glSubject{kind: k, consumerID: consumerID, tenantID: tenantID}, true
}

// parseGLOp parses op; empty is allowed only where all ops are meant.
func parseGLOp(w http.ResponseWriter, op string, allowAll bool) ([]keyquota.Op, bool) {
	if op == "" && allowAll {
		return gatewayLimitsOps, true
	}
	o, ok := keyquota.ParseOp(op)
	if !ok {
		writeGLInvalid(w, "op", "op must be chat, embed or rerank")
		return nil, false
	}
	return []keyquota.Op{o}, true
}

func writeGLInvalid(w http.ResponseWriter, param, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid data: " + msg, "param": param})
}

func writeGLFieldError(w http.ResponseWriter, fe *gatewaylimits.FieldError) {
	if fe.Code == gatewaylimits.CodeWidens {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "limit widens: " + fe.Field + " — " + fe.Msg, "param": fe.Field})
		return
	}
	writeGLInvalid(w, fe.Field, fe.Field+": "+fe.Msg)
}

// glReady answers 503 while the registry or the settings floor is not
// wired.
func (h *ManageHandler) glReady(w http.ResponseWriter) bool {
	if h.gatewayLimits == nil || h.gatewayLimitDefaults == nil {
		writeConsumerError(w, http.StatusServiceUnavailable, "gateway limits are not available")
		return false
	}
	return true
}

// glTierOf is the tier a caller writes: the server-admin the server row, a
// tenant-admin the tenant row — never a body field. A tenant-admin on a
// server-tier-only kind is refused 403 (ok false).
func glTierOf(w http.ResponseWriter, ar *auth.AuthResult, kind gatewaylimits.SubjectKind) (gatewaylimits.Tier, bool) {
	if ar.IsServerAdmin() {
		return gatewaylimits.TierServer, true
	}
	if kind == gatewaylimits.SubjectInstance || kind == gatewaylimits.SubjectTenantEnvelope {
		writeAdminRequired(w)
		return "", false
	}
	return gatewaylimits.TierTenant, true
}

// glResolve resolves the subject within the caller's access: (consumer,
// tenant) or a written 404 (no oracle: foreign, unknown and malformed
// answer byte-identically, as the consumer actions do) or 500.
func (h *ManageHandler) glResolve(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, s glSubject) (consumer, tenant string, ok bool) {
	consumer, tenant, err := store.GatewayLimitSubjectTenant(r.Context(), h.pool, accessOf(ar), store.GatewayLimitKey{Kind: s.kind, ConsumerID: s.consumerID, TenantID: s.tenantID})
	if err != nil {
		h.writeGLStoreError(w, s.kind, err)
		return "", "", false
	}
	return consumer, tenant, true
}

func (h *ManageHandler) writeGLStoreError(w http.ResponseWriter, kind gatewaylimits.SubjectKind, err error) {
	if errors.Is(err, store.ErrGatewayLimitSubject) {
		if kind == gatewaylimits.SubjectConsumer {
			writeConsumerError(w, http.StatusNotFound, errConsumerNotFound)
		} else {
			writeConsumerError(w, http.StatusNotFound, "tenant not found")
		}
		return
	}
	slog.Error("gateway limits: store", "error", err)
	writeConsumerError(w, http.StatusInternalServerError, "internal error")
}

// reloadGL publishes a committed write before the answer: the next request
// meters under it. A failed reload is logged; the write's NOTIFY reloads
// the listener's registry, so the snapshot catches up.
func (h *ManageHandler) reloadGL(r *http.Request) {
	if err := h.gatewayLimits.Load(r.Context()); err != nil {
		slog.Warn("gateway limits: reload after a write failed — the NOTIFY reload catches up", "error", err)
	}
}

// handleGatewayLimitsGet answers {"success":true,"limits":GatewayLimitsView}.
func (h *ManageHandler) handleGatewayLimitsGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	if !h.glReady(w) {
		return
	}
	var in GatewayLimitsGetRequest
	if err := decodeConsumerPayload(req.Data, &in); err != nil {
		writeGLInvalid(w, "", err.Error())
		return
	}
	s, ok := parseGLSubject(w, in.SubjectKind, in.ConsumerID, in.TenantID)
	if !ok {
		return
	}
	ops, ok := parseGLOp(w, in.Op, true)
	if !ok {
		return
	}
	if _, ok := glTierOf(w, ar, s.kind); !ok {
		return
	}
	consumer, tenant, ok := h.glResolve(w, r, ar, s)
	if !ok {
		return
	}
	snap := h.gatewayLimits.Snapshot()
	view := GatewayLimitsView{SubjectKind: string(s.kind), ConsumerID: consumer, Ops: make(map[string]GatewayLimitsOp, len(ops))}
	if s.kind != gatewaylimits.SubjectConsumer {
		view.TenantID = tenant
	}
	tiers := snap.ConsumerTiers([]string{consumer})
	for _, op := range ops {
		d := h.gatewayLimitDefaults(op)
		var o GatewayLimitsOp
		switch s.kind {
		case gatewaylimits.SubjectConsumer:
			p := tiers[consumer][op]
			o.Server, o.Tenant = p.Server, p.Tenant
		default:
			o.Server, o.Tenant = glStored(snap, s.kind, tenant, op)
		}
		if c, err := snap.Ceiling(s.kind, consumer, tenant, op, d); err == nil {
			o.Ceiling = &c
		}
		if s.kind != gatewaylimits.SubjectTenantEnvelope {
			eff := snap.Resolve(consumer, tenant, op, d)
			o.Effective = glEffective(eff, ar.IsServerAdmin(), tenant != "")
			o.Sources = eff.Sources
		}
		view.Ops[string(op)] = o
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "limits": view})
}

// glStored are the stored server and tenant rows of a tenant, instance or
// envelope subject.
func glStored(snap *gatewaylimits.Snapshot, kind gatewaylimits.SubjectKind, tenant string, op keyquota.Op) (server, tenantRow *gatewaylimits.Values) {
	for _, row := range snap.Rows() {
		if row.Kind != kind || row.Op != op || row.TenantID != tenant {
			continue
		}
		v := row.Values
		if row.Tier == gatewaylimits.TierServer {
			server = &v
		} else {
			tenantRow = &v
		}
	}
	return server, tenantRow
}

// glEffective is the wire form of e: a 0 budget dimension is unbounded
// (null). The instance envelope only for a server-admin; the tenant envelope
// only where a tenant is in play.
func glEffective(e gatewaylimits.Effective, serverAdmin, withTenant bool) *GatewayLimitsEffective {
	out := &GatewayLimitsEffective{RPM: e.RPM, TPM: e.TPM, MaxConcurrency: e.Concurrency,
		DailyTokens: glBound(e.Consumer.DailyTokens), MonthlyTokens: glBound(e.Consumer.MonthlyTokens),
		DailyRequests: glBound(e.Consumer.DailyRequests), MonthlyCostUSD: glCost(e.Consumer.MonthlyCostUSD),
		AllowExternal: e.AllowExternal, AllowUnmeteredExternal: e.AllowUnmetered}
	if withTenant {
		out.TenantEnvelope = glBudget(e.Tenant)
	}
	if serverAdmin {
		out.InstanceEnvelope = glBudget(e.Instance)
	}
	return out
}

func glBudget(b keyquota.Budget) *GatewayLimitsBudget {
	return &GatewayLimitsBudget{DailyTokens: glBound(b.DailyTokens), MonthlyTokens: glBound(b.MonthlyTokens),
		DailyRequests: glBound(b.DailyRequests), MonthlyCostUSD: glCost(b.MonthlyCostUSD)}
}

func glBound(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

func glCost(usd float64) *float64 {
	if usd <= 0 {
		return nil
	}
	return &usd
}

// GatewayLimitsRow is the wire form of a stored row (the set answer).
type GatewayLimitsRow struct {
	SubjectKind string `json:"subject_kind"`
	ConsumerID  string `json:"consumer_id,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	Tier        string `json:"tier"`
	Op          string `json:"op"`
	gatewaylimits.Values
}

// handleGatewayLimitsSet answers {"success":true,"limits":GatewayLimitsRow}.
// Order: shape, tier, values, subject (404 before any check that reads the
// subject's rows), narrowing (tenant tier), write, reload.
func (h *ManageHandler) handleGatewayLimitsSet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	if !h.glReady(w) {
		return
	}
	var in GatewayLimitsSetRequest
	if err := decodeConsumerPayload(req.Data, &in); err != nil {
		writeGLInvalid(w, "", err.Error())
		return
	}
	s, ok := parseGLSubject(w, in.SubjectKind, in.ConsumerID, in.TenantID)
	if !ok {
		return
	}
	ops, ok := parseGLOp(w, in.Op, false)
	if !ok {
		return
	}
	op := ops[0]
	tier, ok := glTierOf(w, ar, s.kind)
	if !ok {
		return
	}
	d := h.gatewayLimitDefaults(op)
	if fe := gatewaylimits.ValidateValues(s.kind, tier, in.Values, d); fe != nil {
		writeGLFieldError(w, fe)
		return
	}
	consumer, tenant, ok := h.glResolve(w, r, ar, s)
	if !ok {
		return
	}
	if tier == gatewaylimits.TierTenant {
		ceiling, err := h.gatewayLimits.Snapshot().Ceiling(s.kind, consumer, tenant, op, d)
		if err != nil {
			writeAdminRequired(w)
			return
		}
		if fe := gatewaylimits.CheckNarrowing(in.Values, ceiling); fe != nil {
			writeGLFieldError(w, fe)
			return
		}
	}
	row, err := store.UpsertGatewayLimit(r.Context(), h.pool, accessOf(ar),
		store.GatewayLimitKey{Kind: s.kind, ConsumerID: consumer, TenantID: tenant, Tier: tier, Op: op}, in.Values, auditStamp(r, ar))
	if err != nil {
		h.writeGLStoreError(w, s.kind, err)
		return
	}
	h.reloadGL(r)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "limits": GatewayLimitsRow{
		SubjectKind: string(row.Kind), ConsumerID: row.ConsumerID, TenantID: row.TenantID, Tier: string(row.Tier), Op: string(row.Op), Values: row.Values}})
}

// handleGatewayLimitsDelete answers {"success":true,"deleted":bool}: the
// caller's tier row of the subject and op.
func (h *ManageHandler) handleGatewayLimitsDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	if !h.glReady(w) {
		return
	}
	var in GatewayLimitsDeleteRequest
	if err := decodeConsumerPayload(req.Data, &in); err != nil {
		writeGLInvalid(w, "", err.Error())
		return
	}
	s, ok := parseGLSubject(w, in.SubjectKind, in.ConsumerID, in.TenantID)
	if !ok {
		return
	}
	ops, ok := parseGLOp(w, in.Op, false)
	if !ok {
		return
	}
	tier, ok := glTierOf(w, ar, s.kind)
	if !ok {
		return
	}
	deleted, err := store.DeleteGatewayLimit(r.Context(), h.pool, accessOf(ar),
		store.GatewayLimitKey{Kind: s.kind, ConsumerID: s.consumerID, TenantID: s.tenantID, Tier: tier, Op: ops[0]}, auditStamp(r, ar))
	if err != nil {
		h.writeGLStoreError(w, s.kind, err)
		return
	}
	if deleted {
		h.reloadGL(r)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": deleted})
}
