package oai

import "time"

// Settings is one snapshot of the gateway.* keys (design/02 §3.3, renamed
// per masterplan K3). cmd/ctxd builds the getter (config.GatewaySettings
// over the live config store); the gateway reads it once per request, so
// every key is hot. Field meanings and defaults are documented on
// config.GatewayConfig — this package only consumes the values, except Auth,
// which the model-key door (handler.ModelAuth) reads from the same snapshot.
type Settings struct {
	Enabled bool

	MaxRequestBytes     int64
	InflightBudget      int64
	MaxResponseBytes    int64
	MaxStreamFrameBytes int64
	EmbeddingMaxFloats  int64

	StreamCommitGrace time.Duration
	KeepaliveInterval time.Duration
	ClientWriteWindow time.Duration
	RequestTimeout    time.Duration
	RequestDeadline   time.Duration

	// The stream relay (A03-W10, design/03 §4.10 no. 1): the bounded buffer
	// between the upstream reader and the client writer of one stream, and
	// the slowest drain its client may keep up while frames wait for it
	// (bytes per second over the window).
	StreamRelayBufferBytes int64
	StreamMinDrainRate     int64
	StreamMinDrainWindow   time.Duration

	DefaultMaxTokens       int
	MaxOutputTokensCeiling int
	TenantAliasCeiling     AliasCeiling
	// ExportableRoles (gateway.exportable_roles) are the roles whose operator
	// rows (_global or unscoped backends) a tenant alias's chain may reach;
	// empty = none (the export filter, design/02 §4.4.3 no. 3). Each snapshot
	// carries its own copy.
	ExportableRoles []string

	RelayUpstream4xxMessages bool
	RetryAfterJitter         float64

	// BodyLogging is the body policy of the /v1 rows (gateway.body_logging,
	// none, errors or full — llmlog.BodyLoggingValid); the handler passes it to
	// llmlog.GatewayEntry. AccessLog turns the per-request success line of
	// the model groups' access log on (gateway.access_log; W06-9).
	BodyLogging string
	AccessLog   bool

	// Meter are the limits of the per-consumer meter (gateway.<op>_rpm_default,
	// gateway.<op>_tpm_default, gateway.tpm_reserve_default,
	// gateway.xu_reserve_ratio; W06-10; gateway.consumer_concurrency_default;
	// W06-11), read by the limiter cmd/ctxd wires into Deps.Limiter.
	Meter MeterSettings

	Auth AuthSettings
}

// MeterSettings are the meter's limits per op (design/06 §4.11): until the
// limits rows of W06-15 they hold for every consumer.
type MeterSettings struct {
	Chat, Embed, Rerank OpLimits
	// TPMReserve is the completion share of a chat reservation without a
	// client maximum.
	TPMReserve int64
	// XUReserveRatio is the share r of each bucket and of the concurrency
	// slots the urgent lane keeps from the bulk lane of the same consumer,
	// [0, 1).
	XUReserveRatio float64
	// Concurrency is C, L1: the requests per consumer and op that may be
	// open at once, waiting in the queue or running
	// (gateway.consumer_concurrency_default; W06-11), at least 1.
	Concurrency int64
	// MonthlyTokens and DailyRequests are the consumer's tokens per UTC
	// month and requests per UTC day per op (W06-13); 0 = unbounded.
	MonthlyTokens, DailyRequests int64
	// TenantEnvelopeFactor and InstanceEnvelopeFactor scale the consumer's
	// budgets of an op to the envelope of its tenant and of the instance
	// (W06-13), at least 1.
	TenantEnvelopeFactor, InstanceEnvelopeFactor float64
	// AllowExternal is the consumer's permission for cost-reporting billable
	// rows and MonthlyCostUSD its monthly cost budget per op (W06-14,
	// design/06 §4.3; > 0).
	AllowExternal  bool
	MonthlyCostUSD float64
}

// OpLimits are one op's requests and tokens per minute and its tokens per
// model consumer and UTC day (W06-13, at least 1).
type OpLimits struct {
	RPM, TPM    int64
	DailyTokens int64
}

// AuthSettings are the limits of the model-key door (gateway.auth_*,
// design/01 §4.2 stages 5-7; wave W01-6): the failure lock per (address
// prefix, key-hash prefix), the global probe bucket for hashes without a
// success, and the database semaphore of ctx_model_auth.
type AuthSettings struct {
	FailureLimit  int
	FailureWindow time.Duration
	ProbeRate     int
	DBConcurrency int
	// InflightSweepInterval is the period of the in-flight registry's
	// periodic sweep (gateway.inflight_sweep_interval; wave W01-8): one
	// ctx_model_auth_live query over the key IDs of every running /v1
	// request of this process, whatever their number — the second line
	// behind the ctx_model_access_write notifications and the backlog sweep
	// after a listener reconnect. At least 1 s (V49).
	InflightSweepInterval time.Duration
}

// AliasCeiling is the server admin's ceiling on tenant-alias policies
// (gateway.tenant_alias_ceiling.*): tenant aliases are clamped to it on write
// (422) and on reload (clamp + WARN), W02-03/W02-04.
type AliasCeiling struct {
	MaxOutputTokens    int
	MaxN               int
	Timeout            time.Duration
	EmbeddingMaxInputs int
	RerankMaxDocuments int
	ToolsMaxCount      int
}
