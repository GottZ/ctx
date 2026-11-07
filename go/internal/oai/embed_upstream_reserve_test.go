package oai

// The booking pin of the upstream bodies over the forms of the encoder
// measurement (FOLGE-E hardening 1). The route booked embedUpstreamFactor =
// 8 × each body: the json.Encoder of internal/embed allocated up to 7.105 ×
// (255 texts, 24 MiB). internal/embed now writes a body in one allocation of
// its exact length (wire_body.go; measured there per form, size and count:
// TestWireBodyIsOneAllocation — one allocation, at most 8 178 B past the
// body against embed.WireBodySlack), so the route books each body once with
// that slack.

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embed"
)

// TestEmbedUpstreamReserveIsTheMeasuredOne: for plain text, U+FFFD,
// newlines, control characters, quotes, U+2028 and a mix at 64 KiB and
// 1 MiB, one and 255 texts, in one block and in 255 one-text blocks where
// there are 255, the attempt books the bodies once (factor 1) with
// embed.WireBodySlack and the wire call per block, and the route's slack
// is the one measured in internal/embed.
func TestEmbedUpstreamReserveIsTheMeasuredOne(t *testing.T) {
	if EmbedUpstreamSlack != embed.WireBodySlack {
		t.Fatalf("EmbedUpstreamSlack %d, embed.WireBodySlack %d", EmbedUpstreamSlack, embed.WireBodySlack)
	}
	units := map[string]string{"ascii": "a", "fffd": "�", "nl": "\n", "ctl": "\x01", "quote": `"`, "u2028": " ", "mixctl": "abcde\x01"}
	ones := make([]int, 255)
	for i := range ones {
		ones[i] = 1
	}
	b := backends.Backend{Protocol: backends.ProtocolOpenAI}
	for _, size := range []int{64 << 10, 1 << 20} {
		for _, unit := range units {
			for _, k := range []int{1, 255} {
				in := make([]string, k)
				for i := range in {
					in[i] = strings.Repeat(unit, max(size/k/len(unit), 1))
				}
				checkUpstreamBooking(t, b, "m", in, []int{k})
				if k == 255 {
					checkUpstreamBooking(t, b, "m", in, ones)
				}
			}
		}
	}
}
