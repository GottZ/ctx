package oai_test

// The pin of the embeddings route's exported terms (FOLGE-E hardening 2,
// L-2): no code reads them before the budget dimensioning (V39) does, and
// the deadcode ratchets do not see constants — so each is held here
// against its derivation, not against its own value.

import (
	"math"
	"reflect"
	"strconv"
	"testing"

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// TestEmbedExportsAreTheirDerivations: every oai.Embed* term the budget
// dimensioning (V39) is to read is the quantity it names — a per-input
// slot is a string header and a rune count; a per-block term is the
// measured or derived constant its own pin holds (EmbedWireCallBytes,
// EmbedWireRetryBytes, embed.WireBodySlack, embed.WireCopyBuffer); a
// ceiling is its per-block term at a block per input
// (modelalias.CeilingEmbeddingMaxInputs); the raw maxima are base64 with
// every character a \u escape and the longest int in decimal.
func TestEmbedExportsAreTheirDerivations(t *testing.T) {
	blocks := int64(modelalias.CeilingEmbeddingMaxInputs)
	for _, tc := range []struct {
		name      string
		got, want int64
	}{
		{"EmbedInputSlotReserve", int64(oai.EmbedInputSlotReserve), int64(reflect.TypeFor[string]().Size() + reflect.TypeFor[int]().Size())},
		{"EmbedWireCallReserve", int64(oai.EmbedWireCallReserve), oai.EmbedWireCallBytes},
		{"EmbedWireCallCeiling", int64(oai.EmbedWireCallCeiling), oai.EmbedWireCallBytes * blocks},
		{"EmbedUpstreamSlack", int64(oai.EmbedUpstreamSlack), embed.WireBodySlack},
		{"EmbedUpstreamSlackCeiling", int64(oai.EmbedUpstreamSlackCeiling), embed.WireBodySlack * blocks},
		{"EmbedWireCopyReserve", int64(oai.EmbedWireCopyReserve), embed.WireCopyBuffer},
		{"EmbedWireCopyCeiling", int64(oai.EmbedWireCopyCeiling), embed.WireCopyBuffer * blocks},
		{"EmbedWireRetryReserve", int64(oai.EmbedWireRetryReserve), oai.EmbedWireRetryBytes},
		{"EmbedWireRetryCeiling", int64(oai.EmbedWireRetryCeiling), (embed.WireCopyBuffer + oai.EmbedWireRetryBytes) * blocks},
		{"EmbedEncodingRawMax", int64(oai.EmbedEncodingRawMax), int64(max(len("float"), len("base64")) * len(`\u0062`))},
		{"EmbedDimensionsRawMax", int64(oai.EmbedDimensionsRawMax), int64(len(strconv.FormatInt(math.MinInt64, 10)))},
	} {
		if tc.got != tc.want {
			t.Errorf("oai.%s = %d, its derivation is %d", tc.name, tc.got, tc.want)
		}
	}
}
