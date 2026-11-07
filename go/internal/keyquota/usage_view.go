package keyquota

import "time"

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

// Snapshot returns the meter's view of s under l at the meter's clock. ok is
// false for an invalid subject or an unseeded meter. S1 stub: always false.
func (m *Meter) Snapshot(s Subject, l Limits) (Snapshot, bool) {
	_, _, _ = m, s, l
	return Snapshot{}, false
}

// WarningHeader renders ws as the value of QuotaWarningHeader ("" for no
// warning). S1 stub: always "".
func WarningHeader(ws []Warning) string {
	_ = ws
	return ""
}
