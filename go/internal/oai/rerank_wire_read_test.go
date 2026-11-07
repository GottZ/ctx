package oai

// The route's wire term against the wire's own read bound (RC-D S2
// FOLGE-R): rerankWireCall books exactly rerank.AnswerReadMax for the
// answer's read beside the body's build, the copies, the decode, the call
// and the retry — and the exports the wire pins rebuild it from
// (internal/oai/rerankwire) sum to the same term with the body's length
// once more as the answer's room for an echo.

import (
	"testing"

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/rerank"
)

func TestRerankWireCallBooksTheAnswerRead(t *testing.T) {
	for _, body := range []int{1, 169, 32 << 10, 32<<10 + 1, 8 << 20, 48 << 20} {
		for _, docs := range []int{1, 128, 1000} {
			b := int64(body)
			c := min(b, embed.WireCopyBuffer)
			call := rerankWireCall(body, docs)
			read := call - (b + rerankWireBodySlack + c + int64(docs)*rerankDecodeDocBytes + rerankWireFixedBytes + c + rerankWireRetryBytes)
			if want := rerank.AnswerReadMax(docs, body); read != want {
				t.Errorf("body %d docs %d: the call books %d B for the answer's read, rerank.AnswerReadMax is %d B", body, docs, read, want)
			}
			exports := (RerankWireBodyNum*b+RerankWireBodyDen-1)/RerankWireBodyDen + RerankWireBodySlack + c + b +
				int64(docs)*RerankWireDocReserve + RerankWireCallReserve + c + RerankWireRetryReserve
			if exports != call {
				t.Errorf("body %d docs %d: the exports sum to %d B, rerankWireCall books %d B", body, docs, exports, call)
			}
		}
	}
}
