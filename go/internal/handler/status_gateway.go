package handler

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/usageevents"
)

// W06-16 status sections of the model API (design/06 §4.8 rows
// "/api/status server-admin" and "tenant-admin"; design/05 V-06-d).
//
//
//   - gateway: PRESENT on both paths while gateway.enabled is on and a feed
//     is wired; ABSENT (JSON null for the SPA: tile off) otherwise. The
//     server-admin path carries every field (set in assemble from the
//     generation). The tenant path is per caller: HandleStatus stamps it
//     from the same generation keyed by AuthResult.TenantID (SnapshotForTenant
//     knows only the home scope; pattern GuardReviewByScope) and carries
//     only the tenant's own consumers' figures —
//     consumers, consumers_used_1h, consumers_expiring, top_consumers_1h,
//     reservations_open and rejections_5m of its consumers — and never
//     instance_envelope, auth or events (pointers left nil).
//   - usage_split_24h, federation: server-admin only (foreign-load
//     aggregates; dispatcher.go exposure rule), never on the tenant path.
//   - Hub load (V-06-d, K-05-19): top_consumers_1h comes from an in-memory
//     ring (60 one-minute buckets, no statement); consumers,
//     consumers_used_1h, consumers_expiring, the per-tenant counts and
//     usage_split_24h and the ledger counts of federation come from ONE
//     generation with a 30-second TTL (gatewayGenTTL) built from the rollup
//     (llmusage.ReadSplit24h, the last 24 hourly buckets), the refusal
//     ledger (the same 24 buckets) and context_model_consumers/_keys —
//     never in the 5-second tick, never a 24-hour raw scan.

// gatewayGenTTL is the age of the gateway status generation (V-06-d; the
// tenantRollupTTL pattern).
const gatewayGenTTL = 30 * time.Second

// ringBuckets is the length of the top-consumers ring: 60 one-minute
// buckets (V-06-d, "letzte Stunde" — hardcoded-legitim: the field's name is
// its window).
const ringBuckets = 60

// gatewayStatus is the gateway section of /api/status.
type gatewayStatus struct {
	Enabled   bool  `json:"enabled"`
	Consumers int64 `json:"consumers"`
	// ReservationsOpen and Rejections5m are null until the meter and the
	// in-memory ledger feed them (null ≠ 0).
	ReservationsOpen  *int64                  `json:"reservations_open"`
	Rejections5m      map[string]int64        `json:"rejections_5m"`
	TopConsumers1h    []TopConsumer           `json:"top_consumers_1h"`
	ConsumersUsed1h   int64                   `json:"consumers_used_1h"`
	ConsumersExpiring int64                   `json:"consumers_expiring"`
	InstanceEnvelope  *map[string]envelopeUse `json:"instance_envelope,omitempty"`
	Auth              *ModelAuthStats         `json:"auth,omitempty"`
	Events            *usageevents.Stats      `json:"events,omitempty"`
	// GeneratedAt is when the 30-second generation behind the counts was
	// built (the counts are that old at most).
	GeneratedAt *time.Time `json:"generated_at,omitempty"`
}

// envelopeUse is one op's instance envelope: used and limit tokens of the
// UTC day.
type envelopeUse struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

// federationStatus is the federation section of /api/status (server-admin
// only): the in-memory counters of fedhop and federation plus the refusal
// ledger's federation details of the last hour (loop, depth,
// header_invalid; reason 'federation').
type federationStatus struct {
	// The ledger-derived counts are the refusals of the last 24 hourly
	// buckets of context_llm_rejections_hourly (flushed every 10 s): loop,
	// depth, header_invalid and peer_mismatch (reason federation), busy
	// (capacity/server_busy). embed_space_rejects has no ledger detail yet
	// and stays null.
	LoopRejects          *int64 `json:"loop_rejects"`
	DepthRejects         *int64 `json:"depth_rejects"`
	HeaderInvalid        *int64 `json:"header_invalid"`
	HeaderIgnored        uint64 `json:"header_ignored"`
	PeerWithoutPath      uint64 `json:"peer_without_path"`
	PeerMismatch         *int64 `json:"peer_mismatch"`
	EmbedSpaceRejects    *int64 `json:"embed_space_rejects"`
	Busy                 *int64 `json:"busy"`
	StampWithoutIdentity uint64 `json:"stamp_without_identity"`
	PeerBound            int64  `json:"peer_bound"`
}

// TopConsumer is one row of top_consumers_1h.
type TopConsumer struct {
	ConsumerID string `json:"consumer_id"`
	Requests   int64  `json:"requests"`
	Tokens     int64  `json:"tokens"`
}

// GatewayFeed is what the status collector reads for the gateway sections;
// cmd/ctxd builds it next to the model gateway (S2). A nil func reads as
// "no source": its field stays absent.
type GatewayFeed struct {
	Enabled    func() bool
	DoorStats  func() ModelAuthStats
	Ring       *ConsumerRing
	Events     func() usageevents.Stats
	FedHop     func() fedhop.Counters
	Federation func() federation.Counters
	// Split reads usage_split_24h (llmusage.ReadSplit24h in production).
	Split func(now time.Time) ([]llmusage.SplitRow, error)
}

// SetGatewayFeed wires the gateway sections (nil = none).
func (c *StatusCollector) SetGatewayFeed(f *GatewayFeed) { c.gateway.feed.Store(f) }

// gatewayStatusState is the collector's gateway part: the feed and the
// 30-second generation of the database counts (V-06-d).
type gatewayStatusState struct {
	feed atomic.Pointer[GatewayFeed]
	mu   sync.Mutex // serializes generation builds (single-flight)
	gen  atomic.Pointer[gatewayGeneration]
}

// gatewayGeneration is one build of the database-backed gateway counts.
type gatewayGeneration struct {
	at       time.Time
	byTenant map[string]gatewayCounts
	split    []llmusage.SplitRow
	// refusals are the ledger's (reason, detail) sums of the last 24 hourly
	// buckets; nil when the read failed or no feed reads them.
	refusals map[[2]string]int64
}

// gatewayRefusalsSQL reads the ledger's refusals per reason and detail
// over the last 24 hourly buckets (idx_llm_rejections_bucket).
const gatewayRefusalsSQL = `
SELECT reason, detail, sum(n)::bigint
FROM context_llm_rejections_hourly
WHERE bucket_start >= $1
GROUP BY 1, 2`

// readGatewayRefusals fills g.refusals.
func (c *StatusCollector) readGatewayRefusals(ctx context.Context, g *gatewayGeneration, now time.Time) error {
	rows, err := c.pool.Query(ctx, gatewayRefusalsSQL, now.UTC().Truncate(time.Hour).Add(-23*time.Hour))
	if err != nil {
		return err
	}
	defer rows.Close()
	g.refusals = map[[2]string]int64{}
	for rows.Next() {
		var reason, detail string
		var n int64
		if err := rows.Scan(&reason, &detail, &n); err != nil {
			return err
		}
		g.refusals[[2]string{reason, detail}] = n
	}
	return rows.Err()
}

// refusalCount is a ledger count of the generation, nil without a ledger
// read (null ≠ 0).
func (g *gatewayGeneration) refusalCount(reason, detail string) *int64 {
	if g.refusals == nil {
		return nil
	}
	n := g.refusals[[2]string{reason, detail}]
	return &n
}

// gatewayCounts are the consumers of one tenant: active ones, those whose
// keys were used in the last hour, those with a live key expiring within
// gateway.key_expiry_warn.
type gatewayCounts struct{ consumers, used1h, expiring int64 }

// gatewayCountsSQL is the generation's one statement on the consumers and
// their keys (idx_model_consumers_tenant_active; the key probes are per
// consumer).
const gatewayCountsSQL = `
SELECT c.tenant_id::text, count(*)::bigint,
       count(*) FILTER (WHERE EXISTS (SELECT 1 FROM context_model_keys k
                                       WHERE k.consumer_id = c.id AND k.last_used_at > $1))::bigint,
       count(*) FILTER (WHERE EXISTS (SELECT 1 FROM context_model_keys k
                                       WHERE k.consumer_id = c.id AND k.revoked_at IS NULL
                                         AND k.expires_at > $2 AND k.expires_at <= $3))::bigint
FROM context_model_consumers c
WHERE c.active
GROUP BY 1`

// gatewayGenNow returns the generation, building it when it is older than
// gatewayGenTTL (one build at a time; a failed build keeps the old one).
func (c *StatusCollector) gatewayGenNow(ctx context.Context, f *GatewayFeed) *gatewayGeneration {
	now := time.Now()
	if g := c.gateway.gen.Load(); g != nil && now.Sub(g.at) < gatewayGenTTL {
		return g
	}
	c.gateway.mu.Lock()
	defer c.gateway.mu.Unlock()
	if g := c.gateway.gen.Load(); g != nil && now.Sub(g.at) < gatewayGenTTL {
		return g
	}
	warn := c.cfg.Snapshot().Gateway.Consumers.KeyExpiryWarn //nolint:forbidigo // MT 06 BLIND: the key-expiry warning window is a server-global gateway setting, not tenant-scoped.
	g := &gatewayGeneration{at: now, byTenant: map[string]gatewayCounts{}}
	rows, err := c.pool.Query(ctx, gatewayCountsSQL, now.Add(-time.Hour), now, now.Add(warn))
	if err == nil {
		for rows.Next() {
			var tenant string
			var n gatewayCounts
			if err = rows.Scan(&tenant, &n.consumers, &n.used1h, &n.expiring); err != nil {
				break
			}
			g.byTenant[tenant] = n
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
	}
	if err == nil && f.Split != nil {
		g.split, err = f.Split(now)
	}
	if err == nil {
		err = c.readGatewayRefusals(ctx, g, now)
	}
	if err != nil {
		slog.Warn("status: gateway generation failed", "error", err)
		if old := c.gateway.gen.Load(); old != nil {
			return old
		}
		return nil
	}
	c.gateway.gen.Store(g)
	return g
}

// topConsumersN is the length of top_consumers_1h.
const topConsumersN = 10

// stampGateway sets the gateway sections of one caller's answer: every
// field for a server-admin, the tenant's own consumers for a tenant-admin
// (nothing without a tenant). No section while the feed is missing, the
// gateway is off or no generation could be built.
func (c *StatusCollector) stampGateway(ctx context.Context, resp *statusResponse, admin bool, tenant string) {
	f := c.gateway.feed.Load()
	if f == nil || f.Enabled == nil || !f.Enabled() || (!admin && tenant == "") {
		return
	}
	g := c.gatewayGenNow(ctx, f)
	if g == nil {
		return
	}
	gw := &gatewayStatus{Enabled: true, GeneratedAt: &g.at}
	if admin {
		for _, n := range g.byTenant {
			gw.Consumers += n.consumers
			gw.ConsumersUsed1h += n.used1h
			gw.ConsumersExpiring += n.expiring
		}
	} else {
		n := g.byTenant[tenant]
		gw.Consumers, gw.ConsumersUsed1h, gw.ConsumersExpiring = n.consumers, n.used1h, n.expiring
	}
	gw.TopConsumers1h = []TopConsumer{}
	if f.Ring != nil {
		gw.TopConsumers1h = f.Ring.Top(topConsumersN, tenant)
	}
	if admin {
		if f.DoorStats != nil {
			st := f.DoorStats()
			gw.Auth = &st
		}
		if f.Events != nil {
			ev := f.Events()
			gw.Events = &ev
		}
		split := g.split
		if split == nil {
			split = []llmusage.SplitRow{}
		}
		resp.UsageSplit24h = &split
		fs := &federationStatus{}
		if f.FedHop != nil {
			h := f.FedHop()
			fs.HeaderIgnored, fs.StampWithoutIdentity = h.HeaderIgnored, h.StampWithoutIdentity
		}
		if f.Federation != nil {
			fs.PeerWithoutPath = f.Federation().PeerWithoutPath
		}
		if gw.Events != nil {
			fs.PeerBound = gw.Events.PeerBound
		}
		fs.LoopRejects, fs.DepthRejects = g.refusalCount("federation", "loop"), g.refusalCount("federation", "depth")
		fs.HeaderInvalid = g.refusalCount("federation", "header_invalid")
		fs.PeerMismatch = g.refusalCount("federation", "peer_mismatch")
		fs.Busy = g.refusalCount("capacity", "server_busy")
		resp.Federation = fs
	}
	resp.Gateway = gw
}

// ConsumerRing is the in-memory top-consumers ring (V-06-d): ringBuckets
// one-minute buckets of requests and tokens per consumer; buckets older
// than ringBuckets minutes are dropped on the next Add or Top.
// Cardinality = the consumers active within the hour.
type ConsumerRing struct {
	now func() time.Time
	mu  sync.Mutex
	// minutes are the per-minute counts, keyed by the minute (unix
	// minutes); each holds the consumers seen in it.
	minutes map[int64]map[string]*ringCount
}

type ringCount struct {
	tenant           string
	requests, tokens int64
}

// NewConsumerRing returns an empty ring; now nil = time.Now.
func NewConsumerRing(now func() time.Time) *ConsumerRing {
	if now == nil {
		now = time.Now
	}
	return &ConsumerRing{now: now, minutes: map[int64]map[string]*ringCount{}}
}

// rotate drops the minutes outside the window ending at minute m. Under r.mu.
func (r *ConsumerRing) rotate(m int64) {
	for k := range r.minutes {
		if k <= m-ringBuckets {
			delete(r.minutes, k)
		}
	}
}

// Add counts one admitted request of consumerID (tenantID kept for the
// tenant view) with its tokens.
func (r *ConsumerRing) Add(consumerID, tenantID string, tokens int64) {
	if consumerID == "" {
		return
	}
	m := r.now().Unix() / 60
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotate(m)
	slot := r.minutes[m]
	if slot == nil {
		slot = map[string]*ringCount{}
		r.minutes[m] = slot
	}
	c := slot[consumerID]
	if c == nil {
		c = &ringCount{tenant: tenantID}
		slot[consumerID] = c
	}
	c.requests++
	c.tokens += max(tokens, 0)
}

// Top returns at most n consumers of the last 60 minutes by requests
// (ties: tokens, then id); tenantID "" = every tenant.
func (r *ConsumerRing) Top(n int, tenantID string) []TopConsumer {
	m := r.now().Unix() / 60
	r.mu.Lock()
	sum := map[string]*TopConsumer{}
	r.rotate(m)
	for _, slot := range r.minutes {
		for id, c := range slot {
			if tenantID != "" && c.tenant != tenantID {
				continue
			}
			t := sum[id]
			if t == nil {
				t = &TopConsumer{ConsumerID: id}
				sum[id] = t
			}
			t.Requests += c.requests
			t.Tokens += c.tokens
		}
	}
	r.mu.Unlock()
	out := make([]TopConsumer, 0, len(sum))
	for _, t := range sum {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].ConsumerID < out[j].ConsumerID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
