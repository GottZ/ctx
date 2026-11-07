package keyquota

import (
	"strconv"
	"strings"
	"time"
)

// RC-D cluster C2 (W06-16, W06-18) — the meter's read view and its soft
// threshold. Contract (RC-D S1: fail-closed stubs):
//
//   - Snapshot (W06-16, GET /api/usage/consumers): the day and month
//     counters of a subject's three budget levels, the level that binds, the
//     open reservations and the remaining bucket units as the subject's lane
//     sees them — read under the meter's lock, never a statement.
//   - Soft threshold (W06-18, gateway.soft_ratio): after an admission, every
//     day/month dimension of every level whose used + reserved share of its
//     lane limit reaches Limits.SoftRatio is listed in Decision.Warnings.
//     SoftRatio 0 = off (never a warning). An unbounded dimension (0) never
//     warns. A refusal carries no warning (the refusal is the hard event).
//   - WarningHeader renders the warnings as the value of the response header
//     QuotaWarningHeader: an RFC 8941 list of tokens in the fixed order
//     monthly_tokens, daily_tokens, daily_requests (consumer level, each with
//     the parameter pct = the floor of the used percentage), then
//     tenant_envelope and instance_envelope (no parameter: an envelope's
//     value is not the caller's own limit, B6 doctrine). Example:
//     "daily_tokens;pct=83, tenant_envelope". No warning ⇒ "" ⇒ no header.
//     The header rides in Reservation.Headers() next to the six
//     x-ratelimit-* headers, so it goes exactly where they go: on success
//     answers only (2xx, the stream's 200 at the commit included), never on
//     an error envelope or a refusal (oai/deps.go Limiter contract — the
//     routes copy the reservation's headers on success only). No route and
//     no stream/relay file changes for it.

// QuotaWarningHeader is the response header of the soft threshold (W06-18).
const QuotaWarningHeader = "x-ctx-quota-warning"

// BudgetLevel names the level of a budget: the consumer's own, its tenant's
// envelope, the instance's envelope.
type BudgetLevel string

// The levels as the usage API and the warnings name them.
const (
	LevelConsumer BudgetLevel = "consumer"
	LevelTenant   BudgetLevel = "tenant"
	LevelInstance BudgetLevel = "instance"
)

// Warning is one budget dimension at or over the soft threshold after an
// admission.
type Warning struct {
	Level BudgetLevel
	// Dimension is ReasonDailyTokens, ReasonMonthlyTokens or
	// ReasonDailyRequests.
	Dimension Reason
	// Used counts used + reserved tokens (requests for daily_requests) of
	// the window including this request; Limit is the lane's limit.
	Used, Limit int64
	// WindowStart is the UTC start of the dimension's window (00:00 UTC of
	// the day, the 1st of the month 00:00 UTC) — the dedupe key of the
	// soft_limit event (one per consumer × op × dimension × window).
	WindowStart time.Time
}

// LevelState is one level's counters in a Snapshot.
type LevelState struct {
	Level                               BudgetLevel
	DayTokens, MonthTokens, DayRequests int64
	Reserved, Open                      int64
	// MonthCostUSD is the provider-reported cost of the UTC month.
	MonthCostUSD float64
	// Budget is the level's limit as Limits carried it (0 = unbounded).
	Budget Budget
}

// Snapshot is the meter's view of one subject (consumer × op).
type Snapshot struct {
	Consumer, Tenant, Instance LevelState
	// Binding is the level with the least day-token headroom (ties: the
	// consumer, then the tenant); "" when no level is bounded.
	Binding BudgetLevel
	// Inflight is the subject's open requests (L1); RPMRemaining and
	// TPMRemaining the units its buckets admit now, as its lane sees them.
	Inflight, RPMRemaining, TPMRemaining int64
	// DayReset and MonthReset are the next window starts (UTC).
	DayReset, MonthReset time.Time
}

// levelName maps the meter's internal level to its exported name.
func levelName(l budgetLevel) BudgetLevel {
	switch l {
	case levelTenant:
		return LevelTenant
	case levelInstance:
		return LevelInstance
	}
	return LevelConsumer
}

// windowStarts are the UTC starts of the day and the month of t.
func windowStarts(t time.Time) (day, month time.Time) {
	u := t.UTC()
	y, mo, d := u.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC), time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC)
}

// softWarnings are the dimensions of the three levels at or over the soft
// share after an admission (W06-18): used + reserved — the admitted request's
// reservation and request included — against the lane's limit, in the order
// level (consumer, tenant, instance) × month, day, requests. A share of 0
// (off), not a number or ≥ 1 warns nothing; an unbounded dimension never
// warns. Called under m.mu with the levels the admission just booked.
func softWarnings(levels [3]budgetLevelOf, lane Lane, l Limits, wall time.Time) []Warning {
	ratio := l.SoftRatio
	if !(ratio > 0) || ratio >= 1 {
		return nil
	}
	day, month := windowStarts(wall)
	var out []Warning
	for _, lv := range levels {
		for _, d := range [...]struct {
			dim   Reason
			limit int64
			used  int64
			start time.Time
		}{
			{ReasonMonthlyTokens, lv.b.MonthlyTokens, satAdd(lv.u.monthTokens, lv.u.reserved), month},
			{ReasonDailyTokens, lv.b.DailyTokens, satAdd(lv.u.dayTokens, lv.u.reserved), day},
			{ReasonDailyRequests, lv.b.DailyRequests, lv.u.dayRequests, day},
		} {
			limit := laneLimit(d.limit, lane, l.XUReserve)
			if limit <= 0 || float64(d.used) < ratio*float64(limit) {
				continue
			}
			out = append(out, Warning{Level: levelName(lv.level), Dimension: d.dim, Used: d.used, Limit: limit, WindowStart: d.start})
		}
	}
	return out
}

// Snapshot returns the meter's view of s under l at the meter's clock: the
// three levels' counters rolled into the current windows (a key the meter
// has not seen reads as zero and is not stored), the binding level, the
// subject's open requests and the units its minute buckets admit now, as
// its lane sees them. ok is false for an invalid subject or limits and for
// an unseeded meter. No statement — the meter's lock only.
func (m *Meter) Snapshot(s Subject, l Limits) (Snapshot, bool) {
	if s.ConsumerID == "" || s.TenantID == "" || l.RPM < 1 || l.TPM < 1 || s.Lane > LaneBulk || !m.Seeded() {
		return Snapshot{}, false
	}
	if _, ok := ParseOp(string(s.Op)); !ok {
		return Snapshot{}, false
	}
	req := newDim(l.RPM, s.Lane, l.XUReserve)
	tok := newDim(l.TPM, s.Lane, l.XUReserve)
	m.mu.Lock()
	defer m.mu.Unlock()
	wall := m.now()
	now := int64(wall.Sub(m.base))
	out := Snapshot{RPMRemaining: req.cap, TPMRemaining: tok.cap}
	if st := m.state[stateKey{s.ConsumerID, s.Op}]; st != nil {
		out.Inflight = int64(len(st.open))
		out.RPMRemaining, out.TPMRemaining = req.remaining(st.req, now), tok.remaining(st.tok, now)
	}
	var headroom int64
	for _, lv := range m.levels(s, l, wall) {
		st := LevelState{Level: levelName(lv.level), DayTokens: lv.u.dayTokens, MonthTokens: lv.u.monthTokens,
			DayRequests: lv.u.dayRequests, Reserved: lv.u.reserved, Open: lv.u.open, MonthCostUSD: lv.u.monthCost, Budget: lv.b}
		switch lv.level {
		case levelConsumer:
			out.Consumer = st
		case levelTenant:
			out.Tenant = st
		default:
			out.Instance = st
		}
		if limit := laneLimit(lv.b.DailyTokens, s.Lane, l.XUReserve); limit > 0 {
			if h := limit - satAdd(lv.u.dayTokens, lv.u.reserved); out.Binding == "" || h < headroom {
				out.Binding, headroom = st.Level, h
			}
		}
	}
	day, month := windowStarts(wall)
	out.DayReset, out.MonthReset = day.AddDate(0, 0, 1), month.AddDate(0, 1, 0)
	return out, true
}

// warningOrder is the header's fixed order of the consumer's own dimensions.
var warningOrder = [...]Reason{ReasonMonthlyTokens, ReasonDailyTokens, ReasonDailyRequests}

// WarningHeader renders ws as the value of QuotaWarningHeader ("" for no
// warning): the consumer level's dimensions in the order monthly_tokens,
// daily_tokens, daily_requests, each with pct = ⌊used·100 / limit⌋ (at most
// 100), then tenant_envelope and instance_envelope once each and without a
// value (B6: an envelope is not the caller's own limit).
func WarningHeader(ws []Warning) string {
	if len(ws) == 0 {
		return ""
	}
	var parts []string
	for _, dim := range warningOrder {
		for _, w := range ws {
			if w.Level != LevelConsumer || w.Dimension != dim || w.Limit <= 0 {
				continue
			}
			pct := min(mulDiv(max(w.Used, 0), 100, w.Limit), 100)
			parts = append(parts, string(dim)+";pct="+strconv.FormatInt(pct, 10))
			break
		}
	}
	for _, lv := range [...]struct {
		level BudgetLevel
		token string
	}{{LevelTenant, string(ReasonTenantEnvelope)}, {LevelInstance, string(ReasonInstanceEnvelope)}} {
		for _, w := range ws {
			if w.Level == lv.level {
				parts = append(parts, lv.token)
				break
			}
		}
	}
	return strings.Join(parts, ", ")
}
