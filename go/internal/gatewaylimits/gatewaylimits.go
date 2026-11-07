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
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

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

// ErrNoCeiling: the subject kind has no tenant tier (instance, tenant
// envelope), so there is no ceiling a tenant-admin write could stay under.
var ErrNoCeiling = errors.New("gatewaylimits: subject kind is server-tier only")

// Column bounds of migration 168 a value must fit (INTEGER, SMALLINT,
// NUMERIC(14,8)); a value beyond them is refused as invalid at the write
// instead of reaching the database as SQLSTATE 22003.
const (
	maxInt4         = math.MaxInt32
	maxInt2         = math.MaxInt16
	maxCostUSD      = 999999.99999999
	maxSafeBigint   = math.MaxInt64
	costSourceField = "monthly_cost_usd"
)

// rowKey addresses one row: the subject (kind and the id its kind names,
// "" for the instance), the tier and the op.
type rowKey struct {
	kind SubjectKind
	id   string
	tier Tier
	op   keyquota.Op
}

func keyOf(r Row) rowKey {
	id := r.TenantID
	if r.Kind == SubjectConsumer {
		id = r.ConsumerID
	}
	return rowKey{kind: r.Kind, id: id, tier: r.Tier, op: r.Op}
}

// Snapshot is an immutable set of rows.
type Snapshot struct {
	rows  []Row
	index map[rowKey]*Values
}

// NewSnapshot builds a snapshot of rows. A later row of the same key wins
// (the database holds one per key, uq_gateway_limits).
func NewSnapshot(rows []Row) *Snapshot {
	s := &Snapshot{rows: append([]Row(nil), rows...), index: make(map[rowKey]*Values, len(rows))}
	for i := range s.rows {
		s.index[keyOf(s.rows[i])] = &s.rows[i].Values
	}
	return s
}

// Rows are the snapshot's rows (a copy).
func (s *Snapshot) Rows() []Row {
	if s == nil {
		return nil
	}
	return append([]Row(nil), s.rows...)
}

// row is the values of the row at key, nil without a row (or without an id:
// a consumer or tenant row is never looked up under "").
func (s *Snapshot) row(kind SubjectKind, id string, tier Tier, op keyquota.Op) *Values {
	if s == nil || (kind != SubjectInstance && id == "") {
		return nil
	}
	return s.index[rowKey{kind: kind, id: id, tier: tier, op: op}]
}

// levels are the rows that bear on (consumer c, tenant t, op): the server
// and tenant tier of the consumer and of its tenant, the instance row and
// the tenant envelope. A nil entry has no row.
type levels struct {
	cs, ct, ts, tt, in, env *Values
}

func (s *Snapshot) levelsOf(c, t string, op keyquota.Op) levels {
	return levels{
		cs:  s.row(SubjectConsumer, c, TierServer, op),
		ct:  s.row(SubjectConsumer, c, TierTenant, op),
		ts:  s.row(SubjectTenant, t, TierServer, op),
		tt:  s.row(SubjectTenant, t, TierTenant, op),
		in:  s.row(SubjectInstance, "", TierServer, op),
		env: s.row(SubjectTenantEnvelope, t, TierServer, op),
	}
}

// intDim is one integer dimension: its JSON name, its field in Values and
// its settings floor.
type intDim struct {
	name  string
	field func(*Values) **int64
	floor func(Defaults) int64
}

var intDims = []intDim{
	{"rpm", func(v *Values) **int64 { return &v.RPM }, func(d Defaults) int64 { return d.RPM }},
	{"tpm", func(v *Values) **int64 { return &v.TPM }, func(d Defaults) int64 { return d.TPM }},
	{"max_concurrency", func(v *Values) **int64 { return &v.MaxConcurrency }, func(d Defaults) int64 { return d.Concurrency }},
	{"daily_tokens", func(v *Values) **int64 { return &v.DailyTokens }, func(d Defaults) int64 { return d.DailyTokens }},
	{"monthly_tokens", func(v *Values) **int64 { return &v.MonthlyTokens }, func(d Defaults) int64 { return d.MonthlyTokens }},
	{"daily_requests", func(v *Values) **int64 { return &v.DailyRequests }, func(d Defaults) int64 { return d.DailyRequests }},
}

// get is the dimension's value in v (nil: v is nil or says nothing).
func (d intDim) get(v *Values) *int64 {
	if v == nil {
		return nil
	}
	return *d.field(v)
}

// limit is a resolved value with the level that set it; 0 = unbounded.
type limit struct {
	n   int64
	src Source
}

// tighter is the smaller of a and b, 0 counting as unbounded; on a tie a.
func tighter(a, b limit) limit {
	switch {
	case a.n == 0:
		return b
	case b.n == 0:
		return a
	case b.n < a.n:
		return b
	}
	return a
}

// pick is the first set value of the chain with its source, else fallback.
func pick(fallback limit, chain ...struct {
	v   *int64
	src Source
}) limit {
	for _, c := range chain {
		if c.v != nil {
			return limit{n: *c.v, src: c.src}
		}
	}
	return fallback
}

type at = struct {
	v   *int64
	src Source
}

// base is the instance row over the settings floor.
func (l levels) base(dim intDim, d Defaults) limit {
	return pick(limit{n: dim.floor(d), src: SourceSettings}, at{dim.get(l.in), SourceInstance})
}

// server is the server bound of the consumer level: the consumer's server
// row, its tenant's server row, the base.
func (l levels) server(dim intDim, d Defaults) limit {
	return pick(l.base(dim, d), at{dim.get(l.cs), SourceConsumerServer}, at{dim.get(l.ts), SourceTenantServer})
}

// effective is the server bound narrowed by the tenant-tier rows; withOwn
// false leaves the consumer's own tenant-tier row out (its ceiling).
func (l levels) effective(dim intDim, d Defaults, withOwn bool) limit {
	eff := l.server(dim, d)
	if v := dim.get(l.tt); v != nil {
		eff = tighter(eff, limit{n: *v, src: SourceTenantTenant})
	}
	if v := dim.get(l.ct); withOwn && v != nil {
		eff = tighter(eff, limit{n: *v, src: SourceConsumerTenant})
	}
	return eff
}

// cost is the float dimension monthly_cost_usd through the same chain.
type costLimit struct {
	usd float64
	src Source
}

func costOf(v *Values) *float64 {
	if v == nil {
		return nil
	}
	return v.MonthlyCostUSD
}

func (l levels) costBase(d Defaults) costLimit {
	if v := costOf(l.in); v != nil {
		return costLimit{*v, SourceInstance}
	}
	return costLimit{d.MonthlyCostUSD, SourceSettings}
}

func (l levels) costEffective(d Defaults, withOwn bool) costLimit {
	eff := l.costBase(d)
	switch {
	case costOf(l.cs) != nil:
		eff = costLimit{*l.cs.MonthlyCostUSD, SourceConsumerServer}
	case costOf(l.ts) != nil:
		eff = costLimit{*l.ts.MonthlyCostUSD, SourceTenantServer}
	}
	narrow := func(v *float64, src Source) {
		if v != nil && (eff.usd <= 0 || *v < eff.usd) {
			eff = costLimit{*v, src}
		}
	}
	narrow(costOf(l.tt), SourceTenantTenant)
	if withOwn {
		narrow(costOf(l.ct), SourceConsumerTenant)
	}
	return eff
}

// boolOf reads a permission of v (nil: no row or no value).
func boolOf(v *Values, unmetered bool) *bool {
	if v == nil {
		return nil
	}
	if unmetered {
		return v.AllowUnmeteredExternal
	}
	return v.AllowExternal
}

// egress resolves the two permissions (package comment).
func (l levels) egress(d Defaults) (external, unmetered bool, extSrc, unmSrc Source) {
	external, extSrc = d.AllowExternal, SourceSettings
	for _, c := range []struct {
		v   *Values
		src Source
	}{{l.cs, SourceConsumerServer}, {l.ts, SourceTenantServer}, {l.in, SourceInstance}} {
		if b := boolOf(c.v, false); b != nil {
			external, extSrc = *b, c.src
			break
		}
	}
	for _, c := range []struct {
		v   *Values
		src Source
	}{{l.ct, SourceConsumerTenant}, {l.tt, SourceTenantTenant}} {
		if b := boolOf(c.v, false); b != nil && !*b && external {
			external, extSrc = false, c.src
		}
	}
	unmSrc = SourceSettings
	for _, c := range []struct {
		v   *Values
		src Source
	}{{l.cs, SourceConsumerServer}, {l.ts, SourceTenantServer}, {l.in, SourceInstance}} {
		if b := boolOf(c.v, true); b != nil {
			unmetered, unmSrc = *b, c.src
			break
		}
	}
	return external, external && unmetered, extSrc, unmSrc
}

// Resolve is the effective limits of consumer consumerID of tenant tenantID
// for op under the settings floor d (package comment). Without rows it is
// the settings floor — the behaviour before W06-15.
func (s *Snapshot) Resolve(consumerID, tenantID string, op keyquota.Op, d Defaults) Effective {
	l := s.levelsOf(consumerID, tenantID, op)
	e := Effective{Sources: make(map[string]Source, len(intDims)+3)}
	vals := make(map[string]int64, len(intDims))
	for _, dim := range intDims {
		eff := l.effective(dim, d, true)
		if dim.name == "max_concurrency" && d.ConcurrencyCap > 0 && (eff.n == 0 || eff.n > d.ConcurrencyCap) {
			eff.n = d.ConcurrencyCap
		}
		vals[dim.name], e.Sources[dim.name] = eff.n, eff.src
	}
	cost := l.costEffective(d, true)
	e.Sources[costSourceField] = cost.src
	e.RPM, e.TPM, e.Concurrency = vals["rpm"], vals["tpm"], vals["max_concurrency"]
	e.Consumer = keyquota.Budget{DailyTokens: vals["daily_tokens"], MonthlyTokens: vals["monthly_tokens"],
		DailyRequests: vals["daily_requests"], MonthlyCostUSD: cost.usd}
	base := keyquota.Budget{DailyTokens: l.base(intDims[3], d).n, MonthlyTokens: l.base(intDims[4], d).n,
		DailyRequests: l.base(intDims[5], d).n, MonthlyCostUSD: l.costBase(d).usd}
	e.Tenant = base.Scale(d.TenantEnvelopeFactor)
	if l.env != nil {
		if v := l.env.DailyTokens; v != nil {
			e.Tenant.DailyTokens = *v
		}
		if v := l.env.MonthlyTokens; v != nil {
			e.Tenant.MonthlyTokens = *v
		}
		if v := l.env.DailyRequests; v != nil {
			e.Tenant.DailyRequests = *v
		}
		if v := l.env.MonthlyCostUSD; v != nil {
			e.Tenant.MonthlyCostUSD = *v
		}
	}
	e.Instance = base.Scale(d.InstanceEnvelopeFactor)
	var extSrc, unmSrc Source
	e.AllowExternal, e.AllowUnmetered, extSrc, unmSrc = l.egress(d)
	e.Sources["allow_external"], e.Sources["allow_unmetered_external"] = extSrc, unmSrc
	return e
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
func (s *Snapshot) ConsumerTiers(consumerIDs []string) map[string]map[keyquota.Op]TierPair {
	out := map[string]map[keyquota.Op]TierPair{}
	if s == nil {
		return out
	}
	for _, c := range consumerIDs {
		for _, op := range []keyquota.Op{keyquota.OpChat, keyquota.OpEmbed, keyquota.OpRerank} {
			sv, tv := s.row(SubjectConsumer, c, TierServer, op), s.row(SubjectConsumer, c, TierTenant, op)
			if sv == nil && tv == nil {
				continue
			}
			if out[c] == nil {
				out[c] = map[keyquota.Op]TierPair{}
			}
			out[c][op] = TierPair{Server: copyValues(sv), Tenant: copyValues(tv)}
		}
	}
	return out
}

// copyValues is a deep copy (nil stays nil): a snapshot's rows never leak
// a pointer a caller could write through.
func copyValues(v *Values) *Values {
	if v == nil {
		return nil
	}
	out := Values{}
	for _, dim := range intDims {
		if p := dim.get(v); p != nil {
			n := *p
			*dim.field(&out) = &n
		}
	}
	if v.MonthlyCostUSD != nil {
		c := *v.MonthlyCostUSD
		out.MonthlyCostUSD = &c
	}
	if v.AllowExternal != nil {
		b := *v.AllowExternal
		out.AllowExternal = &b
	}
	if v.AllowUnmeteredExternal != nil {
		b := *v.AllowUnmeteredExternal
		out.AllowUnmeteredExternal = &b
	}
	return &out
}

// Ceiling is the bound a tenant-tier write of (kind, consumerID|tenantID,
// op) must stay at or under: for a consumer the effective value without
// that consumer's own tenant-tier row, for a tenant the server-tier tenant
// row over the instance row over the settings. nil in the result =
// unbounded (only monthly_tokens and daily_requests with settings 0 and no
// row). A missing server row means the settings value, never "unbounded"
// (05 V-06-e). The permissions carry no ceiling (CheckNarrowing refuses any
// tenant-tier grant). ErrNoCeiling for the server-tier-only kinds.
func (s *Snapshot) Ceiling(kind SubjectKind, consumerID, tenantID string, op keyquota.Op, d Defaults) (Values, error) {
	var l levels
	switch kind {
	case SubjectConsumer:
		l = s.levelsOf(consumerID, tenantID, op)
	case SubjectTenant:
		// The tenant level: its server row over the base. Its own
		// tenant-tier row is the one being written.
		l = s.levelsOf("", tenantID, op)
		l.tt = nil
	default:
		return Values{}, ErrNoCeiling
	}
	var out Values
	for _, dim := range intDims {
		eff := l.effective(dim, d, false)
		if dim.name == "max_concurrency" && d.ConcurrencyCap > 0 && (eff.n == 0 || eff.n > d.ConcurrencyCap) {
			eff.n = d.ConcurrencyCap
		}
		if eff.n > 0 {
			n := eff.n
			*dim.field(&out) = &n
		}
	}
	if c := l.costEffective(d, false); c.usd > 0 {
		usd := c.usd
		out.MonthlyCostUSD = &usd
	}
	return out, nil
}

// CheckNarrowing refuses a tenant-tier value above its ceiling (CodeWidens,
// Field = the dimension) and a tenant-tier permission (allow_external true,
// any allow_unmetered_external: CodeWidens). nil = the write narrows. The
// dimensions are checked in the order of the row, the first one refuses.
func CheckNarrowing(v, ceiling Values) *FieldError {
	for _, dim := range intDims {
		got, ceil := dim.get(&v), dim.get(&ceiling)
		if got != nil && ceil != nil && *got > *ceil {
			return &FieldError{Field: dim.name, Code: CodeWidens,
				Msg: fmt.Sprintf("%d is above %d, the limit of the levels above", *got, *ceil)}
		}
	}
	if v.MonthlyCostUSD != nil && ceiling.MonthlyCostUSD != nil && *v.MonthlyCostUSD > *ceiling.MonthlyCostUSD {
		return &FieldError{Field: costSourceField, Code: CodeWidens,
			Msg: fmt.Sprintf("%g is above %g, the limit of the levels above", *v.MonthlyCostUSD, *ceiling.MonthlyCostUSD)}
	}
	if v.AllowExternal != nil && *v.AllowExternal {
		return &FieldError{Field: "allow_external", Code: CodeWidens, Msg: "a tenant-tier row can only withdraw the permission (false)"}
	}
	if v.AllowUnmeteredExternal != nil {
		return &FieldError{Field: "allow_unmetered_external", Code: CodeWidens, Msg: "only a server-admin sets the unmetered permission"}
	}
	return nil
}

// intMax is the column bound of an integer dimension.
func intMax(name string) int64 {
	switch name {
	case "max_concurrency":
		return maxInt2
	case "daily_tokens", "monthly_tokens":
		return maxSafeBigint
	}
	return maxInt4
}

// ValidateValues checks a row's shape for (kind, tier) independent of other
// rows: every numeric value > 0 and within its column (no 0 sentinel), the
// envelope carries no rate, concurrency or unmetered permission, an
// instance or envelope row is server tier, max_concurrency at most
// d.ConcurrencyCap while that is set. nil = valid.
func ValidateValues(kind SubjectKind, tier Tier, v Values, d Defaults) *FieldError {
	if tier != TierServer && (kind == SubjectInstance || kind == SubjectTenantEnvelope) {
		return &FieldError{Field: "subject_kind", Code: CodeInvalid, Msg: string(kind) + " rows are server-admin only"}
	}
	for _, dim := range intDims {
		p := dim.get(&v)
		if p == nil {
			continue
		}
		if *p <= 0 {
			return &FieldError{Field: dim.name, Code: CodeInvalid, Msg: "must be > 0 or null (null inherits; there is no 0 sentinel)"}
		}
		if *p > intMax(dim.name) {
			return &FieldError{Field: dim.name, Code: CodeInvalid, Msg: fmt.Sprintf("must be at most %d", intMax(dim.name))}
		}
		if kind == SubjectTenantEnvelope && (dim.name == "rpm" || dim.name == "tpm" || dim.name == "max_concurrency") {
			return &FieldError{Field: dim.name, Code: CodeInvalid, Msg: "a tenant envelope carries day/month budgets only"}
		}
		if dim.name == "max_concurrency" && d.ConcurrencyCap > 0 && *p > d.ConcurrencyCap {
			return &FieldError{Field: dim.name, Code: CodeInvalid,
				Msg: fmt.Sprintf("must be at most %d (dispatch.external_queue_per_consumer)", d.ConcurrencyCap)}
		}
	}
	if c := v.MonthlyCostUSD; c != nil {
		if math.IsNaN(*c) || math.IsInf(*c, 0) || *c <= 0 || *c > maxCostUSD {
			return &FieldError{Field: costSourceField, Code: CodeInvalid, Msg: "must be > 0 and below 1000000, or null (there is no 0 sentinel)"}
		}
	}
	if kind == SubjectTenantEnvelope && v.AllowUnmeteredExternal != nil {
		return &FieldError{Field: "allow_unmetered_external", Code: CodeInvalid, Msg: "a tenant envelope carries no unmetered permission"}
	}
	return nil
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
	// loadMu serialises Load: each load reads the table after the previous
	// one published, so a slower older read never overwrites a newer
	// snapshot.
	loadMu sync.Mutex
	// flightMu guards running/pending of Trigger's single flight.
	flightMu         sync.Mutex
	running, pending bool
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

// Load reads all rows and publishes them as the new snapshot. A failed read
// keeps the previous snapshot and returns the error.
func (r *Registry) Load(ctx context.Context) error {
	if r == nil || r.load == nil {
		return errors.New("gatewaylimits: registry without a loader")
	}
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	rows, err := r.load(ctx)
	if err != nil {
		return err
	}
	r.snap.Store(NewSnapshot(rows))
	return nil
}

// Trigger schedules a Load without blocking the caller (the NOTIFY loop):
// single flight with a pending bit like modelalias.Registry.Trigger — a
// burst of triggers costs the running load plus one. A failed load is
// logged and keeps the previous snapshot; ctx bounds the goroutine.
func (r *Registry) Trigger(ctx context.Context) {
	if r == nil {
		return
	}
	r.flightMu.Lock()
	defer r.flightMu.Unlock()
	if r.running {
		r.pending = true
		return
	}
	r.running = true
	go r.drain(ctx)
}

func (r *Registry) drain(ctx context.Context) {
	for {
		if err := r.Load(ctx); err != nil {
			slog.Warn("gatewaylimits: registry reload failed — previous snapshot stays active", "error", err)
		}
		r.flightMu.Lock()
		if !r.pending || ctx.Err() != nil {
			r.running, r.pending = false, false
			r.flightMu.Unlock()
			return
		}
		r.pending = false
		r.flightMu.Unlock()
	}
}
