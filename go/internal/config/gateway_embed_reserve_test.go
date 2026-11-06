package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestEmbedReserveConstantsAreTheRoutes (W02-09, Fremd-Review 1 F-4): the
// boot rule "inflight_budget holds a request at every ceiling" sizes the
// embeddings answer with the same float width and encoder buffer the route
// reserves (oai.EmbedFloatBytes, oai.EmbedEncodeBuffer) — two copies of one
// number, pinned equal.
func TestEmbedReserveConstantsAreTheRoutes(t *testing.T) {
	if gatewayFloatBytes != oai.EmbedFloatBytes || gatewayEmbedEncoderReserve != oai.EmbedEncodeBuffer {
		t.Errorf("config sizes the embeddings answer with %d B per float + %d B buffer, the route reserves %d + %d",
			gatewayFloatBytes, gatewayEmbedEncoderReserve, oai.EmbedFloatBytes, oai.EmbedEncodeBuffer)
	}
}
