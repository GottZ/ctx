package oai

import "time"

// Settings is one snapshot of the gateway.* keys (design/02 §3.3, renamed
// per masterplan K3). cmd/ctxd builds the getter (config.GatewaySettings
// over the live config store); the gateway reads it once per request, so
// every key is hot. Field meanings and defaults are documented on
// config.GatewayConfig — this package only consumes the values.
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
