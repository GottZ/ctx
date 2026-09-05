package llmlog

// WireAttempt is one tried backend of a chained wire call — the full
// provenance that lands in the row's metadata.chain (the row's backend_name
// alone only names the winner). Since MW10 it also carries the per-attempt
// lease wait (WaitMs) and — for dispatcher-aborted attempts — the abort kind
// ("preempted"/"reaped") as Class instead of the generic "canceled"
// (design/05 §3.2): the Σ view over ALL attempts (incl. waits on failover
// predecessors) lives here as single-case forensics; the row columns carry
// only the row-defining attempt.
//
// It lives in llmlog because metadata.chain is ONE persisted JSON vocabulary
// across pipeline families — the chat chain walk and the embed sequence
// write the same keys into the same column, and a per-package copy of this
// struct is exactly how those keys drift apart.
type WireAttempt struct {
	Backend string `json:"backend"`
	Class   string `json:"err_class"`
	Ms      int64  `json:"ms"`
	WaitMs  int64  `json:"wait_ms"`
	// PromptTokens is the embed backend's reported prompt-token usage of a
	// SUCCESSFUL attempt (0 otherwise). It is the D1(a) embed-token metric
	// substrate: LogEmbedWire copies the serving attempt's count onto the
	// llmlog row (prompt_tokens), so the status page can aggregate embed
	// tokens per target/window from llmlog — the SAME column the chat path
	// already fills. Distinct from the dispatcher usage window (C1/MW22),
	// which the lease.ReportUsage call feeds independently. The chat walk
	// never sets it; omitempty keeps it out of metadata.chain either way.
	PromptTokens int `json:"prompt_tokens,omitempty"`
}
