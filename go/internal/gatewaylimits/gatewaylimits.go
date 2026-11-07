// Package gatewaylimits is the policy data of the model API's meter (wave
// W06-15, design/06 §3.5, §4.3, migration 168): the rows of
// context_gateway_limits — per consumer, per tenant (the default of its
// consumers), per instance (the default of every consumer, above the
// gateway.* settings, which stay the floor of the chain) and the tenant
// envelope — and the one rule that resolves them into the limits of a
// (consumer, op): Snapshot.Resolve. cmd/ctxd adapts the result to
// keyquota.Limits and backends.EgressPolicy; the manage actions
// gateway-limits-get|set|delete (internal/handler) check a tenant-admin write
// against Snapshot.Ceiling (narrowing, never widening — B-06-5, B-06-20).
//
// Resolution of a numeric dimension d of consumer c (tenant t) and op o:
//
//	base(d)        = instance[o].d ?? settings(o, d)            (settings 0 = ∞ only for monthly_tokens, daily_requests)
//	server(d)      = consumer[c, server, o].d ?? tenant[t, server, o].d ?? base(d)
//	tenantCap(d)   = min(consumer[c, tenant, o].d ?? ∞, tenant[t, tenant, o].d ?? ∞)
//	effective(d)   = min(server(d), tenantCap(d))               (rows are > 0 by CHECK; no 0 sentinel)
//	tenant_env(d)  = tenant_envelope[t, o].d ?? factor_t × base(d)
//	instance_env(d)= factor_i × base(d)
//
// max_concurrency is never unbounded (settings >= 1 by V48, rows > 0) and
// never above dispatch.external_queue_per_consumer while that cap is set
// (V48 per row). The egress booleans are a permission apart from the budget:
//
//	allow_external  = (consumer[server] ?? tenant[server] ?? instance ?? setting) ∧ (consumer[tenant] ?? tenant[tenant] ?? true)
//	allow_unmetered = allow_external ∧ (consumer[server] ?? tenant[server] ?? instance ?? false)
//
// A tenant-tier row can only store false (allow_external) or nothing
// (allow_unmetered_external): CHECK ck_gl_tenant_narrow.
//
// A leaf by contract (depguard gatewaylimits-leaf): it imports keyquota (the
// budget type and its envelope scaling) and nothing that reaches a database,
// config or handler; the rows arrive through a Loader.
package gatewaylimits

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

// ErrNotImplemented is the answer of every RC-D S1 stub of this package.
var ErrNotImplemented = errors.New("gatewaylimits: not implemented (RC-D S1 stub)")

// SubjectKind is whose limits a row holds.
type SubjectKind string

const (
	// SubjectConsumer rows hold one consumer's limits (consumer_id set).
	SubjectConsumer SubjectKind = "consumer"
	// SubjectTenant rows hold the default of every consumer of a tenant
	// (tenant_id set) — a consumer created later inherits it (B-06-19).
	SubjectTenant SubjectKind = "tenant"
	// SubjectInstance rows hold the default of every consumer of the
	// instance (neither id set), above the gateway.* settings.
	SubjectInstance SubjectKind = "instance"
	// SubjectTenantEnvelope rows hold the aggregate budget of a tenant over
	// all its consumers (tenant_id set; day/month tokens, day requests,
	// month cost only), replacing tenant_envelope_factor × base.
	SubjectTenantEnvelope SubjectKind = "tenant_envelope"
)

// ParseSubjectKind reports whether s names a subject kind.
func ParseSubjectKind(s string) (SubjectKind, bool) {
	switch k := SubjectKind(s); k {
	case SubjectConsumer, SubjectTenant, SubjectInstance, SubjectTenantEnvelope:
		return k, true
	}
	return "", false
}

// Tier is who wrote a row: the server-admin (free) or a tenant-admin
// (narrowing only). The tier of a write comes from the caller, never from
// the request body.
type Tier string

const (
	TierServer Tier = "server"
	TierTenant Tier = "tenant"
)

// Values are the dimensions of one row. nil = the row says nothing about the
// dimension (it inherits, design/06 §3.5). Every numeric value is > 0 —
// there is no 0 sentinel (B-06-20, CHECK 23514).
type Values struct {
	RPM                    *int64   `json:"rpm"`
	TPM                    *int64   `json:"tpm"`
	MaxConcurrency         *int64   `json:"max_concurrency"`
	DailyTokens            *int64   `json:"daily_tokens"`
	MonthlyTokens          *int64   `json:"monthly_tokens"`
	DailyRequests          *int64   `json:"daily_requests"`
	MonthlyCostUSD         *float64 `json:"monthly_cost_usd"`
	AllowExternal          *bool    `json:"allow_external"`
	AllowUnmeteredExternal *bool    `json:"allow_unmetered_external"`
}

// Row is one row of context_gateway_limits as loaded.
type Row struct {
	ID         string
	Kind       SubjectKind
	ConsumerID string // set iff Kind == SubjectConsumer
	TenantID   string // set iff Kind is SubjectTenant or SubjectTenantEnvelope
	Tier       Tier
	Op         keyquota.Op
	Values
	UpdatedAt time.Time
}

// Defaults are the settings floor of an op (the gateway.* keys of W06-10 …
// W06-14) plus the per-row concurrency cap of V48.
type Defaults struct {
	RPM, TPM, Concurrency  int64
	DailyTokens            int64
	MonthlyTokens          int64 // 0 = unbounded
	DailyRequests          int64 // 0 = unbounded
	MonthlyCostUSD         float64
	AllowExternal          bool
	TenantEnvelopeFactor   float64
	InstanceEnvelopeFactor float64
	ConcurrencyCap         int64 // dispatch.external_queue_per_consumer; 0 = no cap
}

// Source names the level that set an effective dimension.
type Source string

const (
	SourceConsumerServer Source = "consumer_server"
	SourceConsumerTenant Source = "consumer_tenant"
	SourceTenantServer   Source = "tenant_server"
	SourceTenantTenant   Source = "tenant_tenant"
	SourceTenantEnvelope Source = "tenant_envelope"
	SourceInstance       Source = "instance"
	SourceSettings       Source = "settings"
)

// Effective are the resolved limits of a (consumer, op).
type Effective struct {
	RPM, TPM, Concurrency int64
	// Consumer, Tenant and Instance are the budgets of the three meter
	// levels (keyquota.Limits); a zero dimension is unbounded.
	Consumer, Tenant, Instance    keyquota.Budget
	AllowExternal, AllowUnmetered bool
	// Sources maps the JSON name of each dimension of the consumer level to
	// the level that set it (gateway-limits-get "effective", the SPA panel).
	Sources map[string]Source
}

// FieldError is a refused write: Field is the JSON name of the dimension
// (or "subject_kind", "op", …), Code one of the Code* constants.
type FieldError struct {
	Field, Code, Msg string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

const (
	// CodeWidens: a tenant-tier value above the ceiling of the levels
	// above (400).
	CodeWidens = "limit_widens"
	// CodeInvalid: a value the row cannot hold (<= 0, a dimension the
	// subject kind does not carry, a tenant-tier permission).
	CodeInvalid = "invalid_value"
)

// Snapshot is an immutable set of rows.
type Snapshot struct {
	rows []Row
}

// NewSnapshot builds a snapshot of rows.
func NewSnapshot(rows []Row) *Snapshot {
	return &Snapshot{rows: append([]Row(nil), rows...)}
}

// Rows are the snapshot's rows (a copy).
func (s *Snapshot) Rows() []Row {
	if s == nil {
		return nil
	}
	return append([]Row(nil), s.rows...)
}

// Resolve is the effective limits of consumer consumerID of tenant tenantID
// for op under the settings floor d (package comment).
//
// RC-D S1 stub: the rows are not consulted — every consumer gets the
// settings floor, the behaviour before W06-15.
func (s *Snapshot) Resolve(consumerID, tenantID string, op keyquota.Op, d Defaults) Effective {
	_, _, _ = consumerID, tenantID, op
	own := keyquota.Budget{DailyTokens: d.DailyTokens, MonthlyTokens: d.MonthlyTokens, DailyRequests: d.DailyRequests,
		MonthlyCostUSD: d.MonthlyCostUSD}
	return Effective{RPM: d.RPM, TPM: d.TPM, Concurrency: d.Concurrency,
		Consumer: own, Tenant: own.Scale(d.TenantEnvelopeFactor), Instance: own.Scale(d.InstanceEnvelopeFactor),
		AllowExternal: d.AllowExternal}
}

// TierPair are a consumer's stored rows of one op: the server-tier and the
// tenant-tier row (nil = no row).
type TierPair struct {
	Server, Tenant *Values
}

// ConsumerTiers are the stored rows of each given consumer per op — the
// source of C2's handler.UsageLimitTiers (/api/usage/consumers, W06-16). A
// consumer without rows is absent from the map; tenant, instance and
// envelope rows never appear.
//
// RC-D S1 stub: an empty map.
func (s *Snapshot) ConsumerTiers(consumerIDs []string) map[string]map[keyquota.Op]TierPair {
	_ = consumerIDs
	return map[string]map[keyquota.Op]TierPair{}
}

// Ceiling is the bound a tenant-tier write of (kind, consumerID|tenantID,
// op) must stay at or under: for a consumer the effective value without
// that consumer's own tenant-tier row, for a tenant the server-tier tenant
// row over the instance row over the settings. nil in the result =
// unbounded (only monthly_tokens and daily_requests with settings 0 and no
// row). A missing server row means the settings value, never "unbounded"
// (05 V-06-e).
//
// RC-D S1 stub: answers no ceiling at all — callers refuse the write.
func (s *Snapshot) Ceiling(kind SubjectKind, consumerID, tenantID string, op keyquota.Op, d Defaults) (Values, error) {
	_, _, _, _, _ = kind, consumerID, tenantID, op, d
	return Values{}, ErrNotImplemented
}

// CheckNarrowing refuses a tenant-tier value above its ceiling (CodeWidens,
// Field = the dimension) and a tenant-tier permission (allow_external true,
// any allow_unmetered_external: CodeInvalid). nil = the write narrows.
//
// RC-D S1 stub: refuses every write.
func CheckNarrowing(v, ceiling Values) *FieldError {
	_, _ = v, ceiling
	return &FieldError{Field: "", Code: CodeInvalid, Msg: ErrNotImplemented.Error()}
}

// ValidateValues checks a row's shape for (kind, tier) independent of other
// rows: every numeric value > 0 (no 0 sentinel), the envelope carries no
// rate, concurrency or unmetered permission, an instance or envelope row is
// server tier, max_concurrency at most d.ConcurrencyCap while that is set.
//
// RC-D S1 stub: refuses every row.
func ValidateValues(kind SubjectKind, tier Tier, v Values, d Defaults) *FieldError {
	_, _, _, _ = kind, tier, v, d
	return &FieldError{Field: "", Code: CodeInvalid, Msg: ErrNotImplemented.Error()}
}

// Loader reads every row of context_gateway_limits.
type Loader func(ctx context.Context) ([]Row, error)

// Registry keeps the rows as one atomic snapshot, loaded at boot, after each
// manage write (synchronously, before the response) and on every
// ctx_settings_write NOTIFY with entity context_gateway_limits (psql edits).
// A failed load keeps the previous snapshot.
type Registry struct {
	load Loader
	snap atomic.Pointer[Snapshot]
}

// NewRegistry builds a registry over load with an empty snapshot.
func NewRegistry(load Loader) *Registry {
	r := &Registry{load: load}
	r.snap.Store(NewSnapshot(nil))
	return r
}

// Snapshot is the current snapshot, never nil.
func (r *Registry) Snapshot() *Snapshot {
	if r == nil {
		return NewSnapshot(nil)
	}
	return r.snap.Load()
}

// Load reads all rows and publishes them as the new snapshot.
//
// RC-D S1 stub: loads nothing.
func (r *Registry) Load(ctx context.Context) error {
	_ = ctx
	return ErrNotImplemented
}

// Trigger schedules a Load without blocking the caller (the NOTIFY loop);
// a burst of triggers is coalesced into at most two loads.
//
// RC-D S1 stub: does nothing.
func (r *Registry) Trigger(ctx context.Context) {
	_ = ctx
}
