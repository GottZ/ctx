package oai

import (
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llmlog"
)

// ForcedSettler is the shutdown half of a reservation (design/06 §4.2
// "Usage-Quellen beim Settle", "Shutdown-Reihenfolge" step 2; wave W06-13):
// the meter's ForceSettleAll ends every reservation still open after the HTTP
// drain with an estimate and calls the writer the route registered. A
// LimitReservation that implements it (the production meter's) gets the runes
// a stream relays and the route's row writer; one that does not (test fakes)
// is never force-settled.
type ForcedSettler interface {
	// AddStreamed counts runes a stream handed on (the completion term of
	// the estimate).
	AddStreamed(runes int)
	// OnForcedSettle registers the writer of the request's one row for the
	// forced settle; it runs at most once, only when the forced settle ended
	// the reservation.
	OnForcedSettle(fn func(dispatch.Usage))
	// Forced reports that the forced settle ended the reservation: its
	// writer wrote the row, the route writes none.
	Forced() bool
}

// addStreamed tells l the runes a stream handed on, when it counts them.
func addStreamed(l LimitReservation, runes int) {
	if fs, ok := l.(ForcedSettler); ok {
		fs.AddStreamed(runes)
	}
}

// registerForcedRow registers the row build returns as l's forced-settle
// writer (build runs only for a reservation that has the forced half): the row of a
// request the shutdown ended before its route did — finish aborted, the
// meter's estimate in the token columns (B-06-8: the rollup and the next
// boot seed read the columns) with metadata.gw.usage "estimated", no body.
// row is built at the admission from values the request never changes
// afterwards, so the writer, which runs on the shutdown's goroutine, reads
// nothing the handler still writes.
func registerForcedRow(l LimitReservation, record func(llmlog.GatewayRow), build func() llmlog.GatewayRow, required string) {
	fs, ok := l.(ForcedSettler)
	if !ok || record == nil {
		return
	}
	row := build()
	row.Gateway.Usage = llmlog.UsageEstimated
	row.Gateway.Finish = llmlog.FinishAborted
	row.Err = &llmlog.RedactedErr{Class: "shutdown", Code: CodeServiceUnavailable}
	row.BodyLogging = llmlog.BodyLoggingNone
	fs.OnForcedSettle(func(u dispatch.Usage) {
		r := row
		r.Telemetry = llmlog.Entry{RequiredSensitivity: required, Metadata: map[string]any{},
			PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens}
		record(r)
	})
}

// forcedEnded reports whether the forced settle ended l: the route then
// writes no row of its own.
func forcedEnded(l LimitReservation) bool {
	fs, ok := l.(ForcedSettler)
	return ok && fs.Forced()
}
