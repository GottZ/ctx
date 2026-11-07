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
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
)

// errGatewayLimitsNotImplemented is the 503 body of the RC-D S1 stubs.
const errGatewayLimitsNotImplemented = "gateway limits not implemented"

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

// handleGatewayLimitsGet answers {"success":true,"limits":GatewayLimitsView}.
//
// RC-D S1 stub: 503.
func (h *ManageHandler) handleGatewayLimitsGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	_, _, _ = r, ar, req
	writeConsumerError(w, http.StatusServiceUnavailable, errGatewayLimitsNotImplemented)
}

// handleGatewayLimitsSet answers {"success":true,"limits":<the stored row>}.
//
// RC-D S1 stub: 503.
func (h *ManageHandler) handleGatewayLimitsSet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	_, _, _ = r, ar, req
	writeConsumerError(w, http.StatusServiceUnavailable, errGatewayLimitsNotImplemented)
}

// handleGatewayLimitsDelete answers {"success":true,"deleted":bool}.
//
// RC-D S1 stub: 503.
func (h *ManageHandler) handleGatewayLimitsDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	_, _, _ = r, ar, req
	writeConsumerError(w, http.StatusServiceUnavailable, errGatewayLimitsNotImplemented)
}
