package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/jackc/pgx/v5/pgxpool"
)

// W06-16 (design/06 §4.7, §4.8; masterplan K18; design/05 V-06-a/b/c/f):
// GET /api/usage and GET /api/usage/consumers, mounted next to /api/llmlog
// behind RequireAdminOrTenantAdmin (cmd/ctxd/server.go, S2).
//
// Contract (RC-D S1 — both handlers answer 503 service_unavailable):
//
// GET /api/usage?from&to&bucket=hour|day&group=consumer|alias|pipeline|source|backend|class|instance&consumer&source
//   - from/to RFC 3339 (default: the last 24 h), cut to whole UTC buckets
//     (llmusage.Align); to − from > 92 d ⇒ 400 invalid_request (llmusage.ErrSpan);
//     unknown bucket/group ⇒ 400; group=instance for a non-server-admin ⇒ 403.
//   - tenant-admin: the rows of the tenant's model consumers only
//     (store.TenantConsumerIDs; ?consumer= is checked by a single-row
//     membership EXISTS (… id = $1 AND tenant_id = $2) — a foreign or unknown
//     id answers 200 with an empty series, never 403/404: no oracle);
//     internal (knowledge-key and background) rows never; refusals without a
//     consumer (auth, before the door) never; target/dispatch_class of
//     refusals never (fields absent, not "").
//   - server-admin: every row; ?consumer= narrows; internal rows included
//     unless ?source=gateway.
//   - Body: {"success":true, …llmusage.Result} — null ≠ 0 per point
//     (llmusage.Point), "complete" false when any point is incomplete,
//     "hist" true when the percentiles come from histograms.
//
// GET /api/usage/consumers?consumer
//   - One entry per visible consumer × op (chat, embed, rerank):
//     {consumer_id, label, tenant_id, op, limits{server, tenant, defaults,
//     effective} (each a usageLimitTier, server/tenant null until the limits
//     rows of W06-15 exist), day{used, limit, reset_at}, month{used, limit,
//     reset_at}, cost_month{used_usd, limit_usd}, binding
//     ("consumer"|"tenant"|"instance"|null), live{reservations, inflight,
//     rpm_remaining, tpm_remaining} (null when the meter is not wired or not
//     seeded), last_bucket, rejections_24h, usage_24h{calls, tokens, errors}}.
//   - The rollup part (last_bucket, usage_24h, rejections_24h) comes from one
//     llmusage.ReadConsumerDays batch per tenant in a 30-second generation
//     (V-06-f: two polls within 30 s ⇒ one statement); the live part from
//     the meter's lock (keyquota.Meter.Snapshot), never a statement.
//   - tenant-admin: own consumers only; the instance envelope only as
//     binding "instance" — its limit and used values are null.

// UsageMeter is the meter's view the consumers list reads (cmd/ctxd wraps
// meterLimits + keyquota.Meter.Snapshot, S2). ok = false: no live part.
type UsageMeter interface {
	View(consumerID, tenantID string, op keyquota.Op) (keyquota.Snapshot, keyquota.Limits, bool)
}

// UsageLimitTiers is the per-consumer limits rows of W06-15 (cluster C1,
// context_gateway_limits): the server and tenant tiers of each consumer × op
// the caller may see. nil source ⇒ both tiers null.
type UsageLimitTiers interface {
	ConsumerTiers(ctx context.Context, consumerIDs []string) (map[string]map[keyquota.Op]UsageTierPair, error)
}

// UsageTierPair is one consumer × op's two data tiers.
type UsageTierPair struct {
	Server, Tenant *UsageLimitTier
}

// UsageLimitTier is one tier of limits; nil field = not set at this tier.
type UsageLimitTier struct {
	RPM            *int64   `json:"rpm"`
	TPM            *int64   `json:"tpm"`
	DailyTokens    *int64   `json:"daily_tokens"`
	MonthlyTokens  *int64   `json:"monthly_tokens"`
	DailyRequests  *int64   `json:"daily_requests"`
	MaxConcurrency *int64   `json:"max_concurrency"`
	MonthlyCostUSD *float64 `json:"monthly_cost_usd"`
	AllowExternal  *bool    `json:"allow_external"`
}

// usageGenerationTTL is the age of the consumers list's rollup generation
// (V-06-f; the tenantRollupTTL pattern, status_tenant.go).
const usageGenerationTTL = 30 * time.Second

// UsageHandler serves GET /api/usage and GET /api/usage/consumers.
type UsageHandler struct {
	pool   *pgxpool.Pool
	meter  UsageMeter
	limits UsageLimitTiers
	now    func() time.Time
}

// NewUsageHandler returns the handler; meter and limits may be nil (no live
// part, no data tiers).
func NewUsageHandler(pool *pgxpool.Pool, meter UsageMeter, limits UsageLimitTiers) *UsageHandler {
	return &UsageHandler{pool: pool, meter: meter, limits: limits, now: time.Now}
}

// writeUsageNotImplemented is the RC-D S1 answer of both routes: fail-closed.
func writeUsageNotImplemented(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"success": false, "error": "not implemented", "code": "service_unavailable",
	})
}

// HandleUsage serves GET /api/usage. S1 stub: 503.
func (h *UsageHandler) HandleUsage(w http.ResponseWriter, r *http.Request) {
	_ = r
	writeUsageNotImplemented(w)
}

// HandleUsageConsumers serves GET /api/usage/consumers. S1 stub: 503.
func (h *UsageHandler) HandleUsageConsumers(w http.ResponseWriter, r *http.Request) {
	_ = r
	writeUsageNotImplemented(w)
}
