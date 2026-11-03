package backends

// ChatMaxTokens is the row's output cap for chat-shaped calls,
// limits.chat_max_tokens (the CPU sidecar's 512, say); 0 = no cap. Every
// reader of the key goes through here — the web-chat token clamp
// (chat.Engine.tokenClamp) and the gateway overlay (llm.BuildUpstreamBody,
// design/02 §4.5.2 stage 3) — so the JSONB number forms are coerced in one
// place: float64 from the pool's JSON decode, int/int64 from fixtures.
func (b *Backend) ChatMaxTokens() int {
	if b.Limits == nil {
		return 0
	}
	switch v := b.Limits["chat_max_tokens"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}
