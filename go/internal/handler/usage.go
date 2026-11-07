package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// W06-16 (design/06 §4.7, §4.8; masterplan K18; design/05 V-06-a/b/c/f):
// GET /api/usage and GET /api/usage/consumers, mounted next to /api/llmlog
// behind RequireAdminOrTenantAdmin (cmd/ctxd/server.go, S2).
//
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

	// gens are the consumers list's rollup generations, one per view (""
	// = the server-admin's view of every consumer, else the tenant id),
	// at most usageGenerationTTL old (V-06-f); genMu serializes builds.
	genMu sync.Mutex
	gens  map[string]*usageGeneration
}

// usageGeneration is one rollup batch of the consumers list.
type usageGeneration struct {
	at       time.Time
	ids      map[string]bool
	days     []llmusage.ConsumerDay
	refusals []llmusage.ConsumerRefusals
}

// NewUsageHandler returns the handler; meter and limits may be nil (no live
// part, no data tiers).
func NewUsageHandler(pool *pgxpool.Pool, meter UsageMeter, limits UsageLimitTiers) *UsageHandler {
	return &UsageHandler{pool: pool, meter: meter, limits: limits, now: time.Now, gens: map[string]*usageGeneration{}}
}

// usageUUID is the textual form of a consumer id the routes accept (format
// check only; existence and membership are the queries' business).
var usageUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// usageDefaultSpan is the window of GET /api/usage without from (the
// status rollup's 24 hours).
const usageDefaultSpan = 24 * time.Hour

func writeUsageError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": msg})
}

// usageCaller is the role of a request: a server-admin, or the tenant of a
// tenant-admin ("" for none — fail-closed, nothing visible).
func usageCaller(r *http.Request) (admin bool, tenant string) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil {
		return false, ""
	}
	if ar.IsServerAdmin() {
		return true, ""
	}
	return false, ar.TenantID
}

// ownConsumer reports whether consumer belongs to tenant: the single-row
// membership check of design/06 §4.8 (B-06-3).
func (h *UsageHandler) ownConsumer(ctx context.Context, tenant, consumer string) (bool, error) {
	if tenant == "" {
		return false, nil
	}
	var ok bool
	err := h.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM context_model_consumers WHERE id = $1::uuid AND tenant_id = $2::uuid)`,
		consumer, tenant).Scan(&ok)
	return ok, err
}

// visibleConsumers is the consumer list of a request: nil for a
// server-admin without ?consumer= (every row), the single consumer when it
// is visible, an empty non-nil list when nothing is (foreign, unknown, no
// tenant) — the same answer for all three, no oracle.
func (h *UsageHandler) visibleConsumers(ctx context.Context, admin bool, tenant, consumer string) ([]string, error) {
	if admin {
		if consumer == "" {
			return nil, nil
		}
		return []string{consumer}, nil
	}
	if tenant == "" {
		return []string{}, nil
	}
	if consumer != "" {
		ok, err := h.ownConsumer(ctx, tenant, consumer)
		if err != nil || !ok {
			return []string{}, err
		}
		return []string{consumer}, nil
	}
	ids, err := store.TenantConsumerIDs(ctx, h.pool, tenant)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// SetMeter wires the meter's view after construction — the routes are
// mounted before the model gateway exists (cmd/ctxd/server.go). Call it
// before serving; nil keeps the live part null.
func (h *UsageHandler) SetMeter(m UsageMeter) { h.meter = m }

// HandleUsage serves GET /api/usage (W06-16).
func (h *UsageHandler) HandleUsage(w http.ResponseWriter, r *http.Request) {
	admin, tenant := usageCaller(r)
	qv := r.URL.Query()
	now := h.now().UTC()
	sq := llmusage.SeriesQuery{From: now.Add(-usageDefaultSpan), To: now, Bucket: llmusage.BucketHour,
		Group: llmusage.GroupConsumer, Source: qv.Get("source"), ServerAdmin: admin, Now: now}
	for _, b := range []struct {
		name string
		dst  *time.Time
	}{{"from", &sq.From}, {"to", &sq.To}} {
		if v := qv.Get(b.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeUsageError(w, http.StatusBadRequest, "invalid "+b.name+": want RFC 3339")
				return
			}
			*b.dst = t
		}
	}
	if v := qv.Get("bucket"); v != "" {
		sq.Bucket = llmusage.Bucket(v)
	}
	if v := qv.Get("group"); v != "" {
		sq.Group = llmusage.GroupBy(v)
	}
	consumer := qv.Get("consumer")
	if consumer != "" && !usageUUID.MatchString(consumer) {
		writeUsageError(w, http.StatusBadRequest, "invalid consumer")
		return
	}
	sq, err := llmusage.Align(sq)
	switch {
	case errors.Is(err, llmusage.ErrForbiddenGroup):
		writeUsageError(w, http.StatusForbidden, "group=instance is server-admin only")
		return
	case errors.Is(err, llmusage.ErrSpan):
		writeUsageError(w, http.StatusBadRequest, "to - from exceeds 92 days")
		return
	case err != nil:
		writeUsageError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	ids, err := h.visibleConsumers(ctx, admin, tenant, consumer)
	if err == nil {
		sq.ConsumerIDs = ids
		var cover llmusage.Cover
		if cover, err = llmusage.ReadCover(ctx, h.pool); err == nil {
			var res llmusage.Result
			if res, err = llmusage.ReadSeries(ctx, h.pool, sq, cover); err == nil {
				writeJSON(w, http.StatusOK, struct {
					Success bool `json:"success"`
					llmusage.Result
				}{true, res})
				return
			}
		}
	}
	slog.Error("usage: read failed", "error", err, "request_id", RequestIDFromContext(ctx))
	writeUsageError(w, http.StatusInternalServerError, "internal error")
}

// usageOps are the ops of the consumers list, in their order.
var usageOps = [...]keyquota.Op{keyquota.OpChat, keyquota.OpEmbed, keyquota.OpRerank}

// usageWindow is the day or month part of a consumers entry: used and limit
// are null when the meter is not wired (null ≠ 0); limit is null for an
// unbounded dimension too.
type usageWindow struct {
	Used    *int64     `json:"used"`
	Limit   *int64     `json:"limit"`
	ResetAt *time.Time `json:"reset_at"`
}

type usageCost struct {
	UsedUSD  *float64 `json:"used_usd"`
	LimitUSD *float64 `json:"limit_usd"`
}

type usageLive struct {
	Reservations int64 `json:"reservations"`
	Inflight     int64 `json:"inflight"`
	RPMRemaining int64 `json:"rpm_remaining"`
	TPMRemaining int64 `json:"tpm_remaining"`
}

type usage24h struct {
	Calls  int64 `json:"calls"`
	Tokens int64 `json:"tokens"`
	Errors int64 `json:"errors"`
}

type usageLimits struct {
	Server    *UsageLimitTier `json:"server"`
	Tenant    *UsageLimitTier `json:"tenant"`
	Defaults  *UsageLimitTier `json:"defaults"`
	Effective *UsageLimitTier `json:"effective"`
}

// usageConsumerEntry is one consumer × op of GET /api/usage/consumers.
type usageConsumerEntry struct {
	ConsumerID    string      `json:"consumer_id"`
	Label         string      `json:"label"`
	TenantID      string      `json:"tenant_id"`
	Op            keyquota.Op `json:"op"`
	Limits        usageLimits `json:"limits"`
	Day           usageWindow `json:"day"`
	Month         usageWindow `json:"month"`
	CostMonth     usageCost   `json:"cost_month"`
	Binding       *string     `json:"binding"`
	Live          *usageLive  `json:"live"`
	LastBucket    *time.Time  `json:"last_bucket"`
	Rejections24h int64       `json:"rejections_24h"`
	Usage24h      usage24h    `json:"usage_24h"`
}

// generation returns the rollup batch of key ("" = server-admin), built at
// most once per usageGenerationTTL for the same consumer set (V-06-f).
func (h *UsageHandler) generation(ctx context.Context, key string, ids []string) (*usageGeneration, error) {
	h.genMu.Lock()
	defer h.genMu.Unlock()
	now := h.now()
	if g := h.gens[key]; g != nil && now.Sub(g.at) < usageGenerationTTL && len(g.ids) == len(ids) {
		same := true
		for _, id := range ids {
			same = same && g.ids[id]
		}
		if same {
			return g, nil
		}
	}
	days, refusals, err := llmusage.ReadConsumerDays(ctx, h.pool, ids, now)
	if err != nil {
		return nil, err
	}
	g := &usageGeneration{at: now, ids: make(map[string]bool, len(ids)), days: days, refusals: refusals}
	for _, id := range ids {
		g.ids[id] = true
	}
	h.gens[key] = g
	return g, nil
}

// HandleUsageConsumers serves GET /api/usage/consumers (W06-16).
func (h *UsageHandler) HandleUsageConsumers(w http.ResponseWriter, r *http.Request) {
	admin, tenant := usageCaller(r)
	ctx := r.Context()
	consumer := r.URL.Query().Get("consumer")
	if consumer != "" && !usageUUID.MatchString(consumer) {
		writeUsageError(w, http.StatusBadRequest, "invalid consumer")
		return
	}
	entries, err := h.consumerEntries(ctx, admin, tenant, consumer)
	if err != nil {
		slog.Error("usage: consumers failed", "error", err, "request_id", RequestIDFromContext(ctx))
		writeUsageError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "consumers": entries})
}

type usageConsumerRow struct{ id, label, tenant string }

func (h *UsageHandler) consumerEntries(ctx context.Context, admin bool, tenant, consumer string) ([]usageConsumerEntry, error) {
	entries := []usageConsumerEntry{}
	if !admin && tenant == "" {
		return entries, nil
	}
	// The whole view is listed and batched; ?consumer= filters afterwards,
	// so the generations stay one per view (a key per requested id would
	// let a caller grow the cache with random ids).
	sql := `SELECT id::text, label, tenant_id::text FROM context_model_consumers`
	var args []any
	if !admin {
		sql += ` WHERE tenant_id = $1::uuid`
		args = append(args, tenant)
	}
	rows, err := h.pool.Query(ctx, sql+` ORDER BY label, id`, args...)
	if err != nil {
		return nil, err
	}
	var list []usageConsumerRow
	for rows.Next() {
		var c usageConsumerRow
		if err := rows.Scan(&c.id, &c.label, &c.tenant); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, len(list))
	for i, c := range list {
		ids[i] = c.id
	}
	genKey := tenant
	if admin {
		genKey = ""
	}
	gen, err := h.generation(ctx, genKey, ids)
	if err != nil {
		return nil, err
	}
	if consumer != "" {
		kept := list[:0]
		for _, c := range list {
			if strings.EqualFold(c.id, consumer) {
				kept = append(kept, c)
			}
		}
		list = kept
		ids = ids[:0]
		for _, c := range list {
			ids = append(ids, c.id)
		}
	}
	var tiers map[string]map[keyquota.Op]UsageTierPair
	if h.limits != nil && len(ids) > 0 {
		if tiers, err = h.limits.ConsumerTiers(ctx, ids); err != nil {
			return nil, err
		}
	}
	for _, c := range list {
		for _, op := range usageOps {
			entries = append(entries, h.consumerEntry(c, op, gen, tiers[c.id][op]))
		}
	}
	return entries, nil
}

// consumerEntry builds one consumer × op entry from the generation, the
// data tiers and the meter's view.
func (h *UsageHandler) consumerEntry(c usageConsumerRow, op keyquota.Op, gen *usageGeneration, pair UsageTierPair) usageConsumerEntry {
	e := usageConsumerEntry{ConsumerID: c.id, Label: c.label, TenantID: c.tenant, Op: op,
		Limits: usageLimits{Server: pair.Server, Tenant: pair.Tenant}}
	for _, d := range gen.days {
		if d.ConsumerID != c.id || keyquota.Op(llmlog.GatewayOp(d.Pipeline)) != op {
			continue
		}
		e.Usage24h.Calls += d.Calls24h
		e.Usage24h.Tokens += d.Tokens24h
		e.Usage24h.Errors += d.Errors24h
		if d.LastBucket != nil && (e.LastBucket == nil || d.LastBucket.After(*e.LastBucket)) {
			e.LastBucket = d.LastBucket
		}
	}
	for _, x := range gen.refusals {
		if x.ConsumerID == c.id && keyquota.Op(x.Op) == op {
			e.Rejections24h += x.Refused24h
		}
	}
	if h.meter == nil {
		return e
	}
	snap, lim, ok := h.meter.View(c.id, c.tenant, op)
	if !ok {
		return e
	}
	e.Limits.Defaults = tierOf(lim)
	e.Limits.Effective = effectiveTier(e.Limits.Defaults, pair)
	own := snap.Consumer
	e.Day = usageWindow{Used: ptr64(own.DayTokens), Limit: bounded(own.Budget.DailyTokens), ResetAt: &snap.DayReset}
	e.Month = usageWindow{Used: ptr64(own.MonthTokens), Limit: bounded(own.Budget.MonthlyTokens), ResetAt: &snap.MonthReset}
	cost := own.MonthCostUSD
	e.CostMonth = usageCost{UsedUSD: &cost}
	if own.Budget.MonthlyCostUSD > 0 {
		lc := own.Budget.MonthlyCostUSD
		e.CostMonth.LimitUSD = &lc
	}
	if snap.Binding != "" {
		b := string(snap.Binding)
		e.Binding = &b
	}
	e.Live = &usageLive{Reservations: own.Open, Inflight: snap.Inflight, RPMRemaining: snap.RPMRemaining, TPMRemaining: snap.TPMRemaining}
	// The instance envelope never carries values in this entry: binding
	// names it, for every role (design/06 §4.8).
	return e
}

func ptr64(v int64) *int64 { return &v }

// bounded is a limit for the wire: nil for an unbounded (0) dimension.
func bounded(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

// tierOf is the registry-default tier the meter applied.
func tierOf(l keyquota.Limits) *UsageLimitTier {
	t := &UsageLimitTier{RPM: ptr64(l.RPM), TPM: ptr64(l.TPM), MaxConcurrency: ptr64(l.Concurrency),
		DailyTokens: bounded(l.Consumer.DailyTokens), MonthlyTokens: bounded(l.Consumer.MonthlyTokens),
		DailyRequests: bounded(l.Consumer.DailyRequests)}
	if l.Consumer.MonthlyCostUSD > 0 {
		c := l.Consumer.MonthlyCostUSD
		t.MonthlyCostUSD = &c
	}
	return t
}

// effectiveTier is min over the tiers: the server tier replaces the
// default, the tenant tier can only lower it (design/06 §4.3); a nil field
// stays the level below.
func effectiveTier(def *UsageLimitTier, p UsageTierPair) *UsageLimitTier {
	out := *def
	for _, t := range []*UsageLimitTier{p.Server, p.Tenant} {
		if t == nil {
			continue
		}
		lower := t == p.Tenant
		pick := func(cur, v *int64) *int64 {
			if v == nil {
				return cur
			}
			if lower && cur != nil && *cur < *v {
				return cur
			}
			return v
		}
		out.RPM, out.TPM, out.DailyTokens = pick(out.RPM, t.RPM), pick(out.TPM, t.TPM), pick(out.DailyTokens, t.DailyTokens)
		out.MonthlyTokens, out.DailyRequests = pick(out.MonthlyTokens, t.MonthlyTokens), pick(out.DailyRequests, t.DailyRequests)
		out.MaxConcurrency = pick(out.MaxConcurrency, t.MaxConcurrency)
		if t.MonthlyCostUSD != nil && (!lower || out.MonthlyCostUSD == nil || *t.MonthlyCostUSD < *out.MonthlyCostUSD) {
			out.MonthlyCostUSD = t.MonthlyCostUSD
		}
		if t.AllowExternal != nil && (!lower || !*t.AllowExternal) {
			out.AllowExternal = t.AllowExternal
		}
	}
	return &out
}
