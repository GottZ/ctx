package llmlog

import (
	"fmt"
	"slices"
	"sync/atomic"
)

// Recording (issue #571, migration 197). A model consumer with the switch
// record_bodies on has every gateway row of its own written with the request
// and the answer in full — whatever gateway.body_logging, the alias, the
// consumer's floor, the body scan or Ctx-Sensitivity say, and without
// gatewayBodyCap. Such a row is marked metadata.gw.recorded = true and can
// carry credentials in clear text: its bodies are for a server-admin only
// (SQLBodiesServerAdminOnly).
//
// The texts a recorded row keeps are held against ONE process-wide account,
// not against the request's in-flight budget: from the moment a route
// captures a text (ReserveRecorded) until the writer has inserted the row or
// lost it (queue full, failed insert). The request path books nothing for
// them, so its chain, its refusals and its answer are the same with and
// without the switch — the one booking that differs is the body policy's own
// log text (at most 64 KiB, booked at the request's end under
// gateway.body_logging errors/full), which a recorded row does not take; and
// the rows of recorded calls the writer has not
// inserted yet — a slow database, a burst of large requests — hold at most
// maxRecordedHeld bytes of text. A text the account cannot take is not
// captured: the row is still written, without it, and says so
// (metadata.gw.record_lost, record_lost_cause "memory").

// maxRecordedHeld bounds the bytes of recorded texts the process holds at
// once — captured by running requests, queued and in insertion. Hardcoded on
// purpose, like maxQueuedInserts: it bounds the process's own bookkeeping,
// not anybody's policy. 128 MiB beside the default gateway.inflight_budget
// of 512 MiB stay inside the container's 1536 MiB (docs/operations.md
// "Recording a consumer"); the largest non-streamed row — a request text of
// at most 2 × gateway.max_request_bytes (an invalid byte becomes U+FFFD) and
// the answer the client received — fits it at the defaults.
const maxRecordedHeld = 128 << 20

// metadata.gw.record_lost — the bodies a recorded row should carry and does
// not.
const (
	RecordLostRequest  = "request"
	RecordLostResponse = "response"
)

// metadata.gw.record_lost_cause — why.
const (
	// RecordLostMemory: maxRecordedHeld could not take the text.
	RecordLostMemory = "memory"
	// RecordLostShutdown: the shutdown's forced settle wrote the row of a
	// request still open (W06-13); that row carries no body.
	RecordLostShutdown = "shutdown"
)

var (
	recordedHeld atomic.Int64 // bytes of recorded texts held (ReserveRecorded − ReleaseRecorded)
	recordedLost atomic.Int64 // recorded rows written without a body they should carry
)

// ReserveRecorded takes n bytes of a recorded text from the account; false
// when they do not fit (nothing is taken then). n <= 0 always fits.
func ReserveRecorded(n int64) bool {
	if n <= 0 {
		return true
	}
	for {
		cur := recordedHeld.Load()
		if cur+n > maxRecordedHeld {
			return false
		}
		if recordedHeld.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// ReleaseRecorded gives n bytes back (n <= 0: nothing).
func ReleaseRecorded(n int64) {
	if n > 0 {
		recordedHeld.Add(-n)
	}
}

// RecordedHeld is the bytes of recorded texts the process holds now.
func RecordedHeld() int64 { return recordedHeld.Load() }

// RecordedBodiesLost counts, since the process started, the recorded rows
// the writer was handed without a body they should carry (record_lost). A
// recorded row the queue drops or the database refuses is in InsertDropped
// or InsertFailures, like every row.
func RecordedBodiesLost() int64 { return recordedLost.Load() }

// recordLostKinds and recordLostCauses are the vocabularies of the two keys.
var (
	recordLostKinds  = []string{RecordLostRequest, RecordLostResponse}
	recordLostCauses = map[string]bool{RecordLostMemory: true, RecordLostShutdown: true}
)

// cleanRecordLost keeps the known bodies of lost, each once, in vocabulary
// order; nil when none is left.
func cleanRecordLost(lost []string) []string {
	var out []string
	for _, k := range recordLostKinds {
		if slices.Contains(lost, k) {
			out = append(out, k)
		}
	}
	return out
}

// SQLBodiesServerAdminOnly is the SQL condition, over the context_llm_log
// alias t, of a row whose bodies only a server-admin may read: a gateway row
// (consumer_id set, or a gateway pipeline) unless its metadata.gw is an
// object that does not mark it recorded — recorded = true, and fail-closed a
// gateway row whose marker is missing or unreadable (metadata.gw absent or
// not an object, recorded anything but absent or false). Every other row
// (knowledge-key and background pipelines) is untouched. Never NULL.
func SQLBodiesServerAdminOnly(t string) string {
	return fmt.Sprintf(`((%[1]s.consumer_id IS NOT NULL OR COALESCE(%[1]s.pipeline LIKE 'gateway-%%', false))
	   AND NOT (COALESCE(jsonb_typeof(%[1]s.metadata->'gw') = 'object', false)
	            AND COALESCE(%[1]s.metadata->'gw'->'recorded', 'false'::jsonb) = 'false'::jsonb))`, t)
}
