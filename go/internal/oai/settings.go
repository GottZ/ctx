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

	DefaultMaxTokens       int
	MaxOutputTokensCeiling int
	TenantAliasCeiling     AliasCeiling

	RelayUpstream4xxMessages bool
	RetryAfterJitter         float64

	// BodyLogging is the body policy of the /v1 rows (gateway.body_logging,
	// none, errors or full — llmlog.BodyLoggingValid); the handler passes it to
	// llmlog.GatewayEntry. AccessLog turns the per-request success line of
	// the model groups' access log on (gateway.access_log; W06-9).
	BodyLogging string
	AccessLog   bool

	Auth AuthSettings
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
