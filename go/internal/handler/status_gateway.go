package handler

import (
	"time"

	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/usageevents"
)

// W06-16 status sections of the model API (design/06 §4.8 rows
// "/api/status server-admin" and "tenant-admin"; design/05 V-06-d).
//
// Contract (RC-D S1 — the fields exist on statusResponse, nothing fills
// them yet; SetGatewayFeed is a no-op, the ring is empty):
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
//     usage_split_24h come from ONE generation with a 30-second TTL
//     (gatewayGenTTL) built from the rollup (llmusage.ReadSplit24h — rollup
//     plus at most one raw edge hour) and context_model_consumers/_keys —
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
	Enabled           bool                    `json:"enabled"`
	Consumers         int64                   `json:"consumers"`
	ReservationsOpen  int64                   `json:"reservations_open"`
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
	LoopRejects          int64  `json:"loop_rejects"`
	DepthRejects         int64  `json:"depth_rejects"`
	HeaderInvalid        int64  `json:"header_invalid"`
	HeaderIgnored        uint64 `json:"header_ignored"`
	PeerWithoutPath      uint64 `json:"peer_without_path"`
	PeerMismatch         int64  `json:"peer_mismatch"`
	EmbedSpaceRejects    int64  `json:"embed_space_rejects"`
	Busy                 int64  `json:"busy"`
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

// SetGatewayFeed wires the gateway sections. S1 stub: no-op.
func (c *StatusCollector) SetGatewayFeed(f *GatewayFeed) { _, _ = c, f }

// ConsumerRing is the in-memory top-consumers ring (V-06-d): 60 one-minute
// buckets of requests and tokens per consumer; a bucket older than 60
// minutes is dropped on the next Add or Top. Cardinality = the consumers.
type ConsumerRing struct {
	now func() time.Time
}

// NewConsumerRing returns an empty ring; now nil = time.Now.
func NewConsumerRing(now func() time.Time) *ConsumerRing {
	if now == nil {
		now = time.Now
	}
	return &ConsumerRing{now: now}
}

// Add counts one admitted request of consumerID (tenantID kept for the
// tenant view) with its tokens. S1 stub: drops.
func (r *ConsumerRing) Add(consumerID, tenantID string, tokens int64) {
	_, _, _, _ = r, consumerID, tenantID, tokens
}

// Top returns at most n consumers of the last 60 minutes by requests
// (ties: tokens, then id); tenantID "" = every tenant. S1 stub: nil.
func (r *ConsumerRing) Top(n int, tenantID string) []TopConsumer {
	_, _, _ = r, n, tenantID
	return nil
}
