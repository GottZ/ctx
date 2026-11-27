package oai

import (
	"math"
	"strings"

	"github.com/GottZ/ctx/internal/llmlog"
)

// recording is the capture of one request of a recording consumer (issue
// #571, auth.ConsumerAuth.RecordBodies): the texts its row keeps whole — the
// request as the body policy's text but without its cut, the answer the
// client received (a non-streamed chat answer as written, a stream's data
// lines as relayed). Every text is reserved against llmlog's recording
// account (llmlog.ReserveRecorded) before it is made — never against the
// request's in-flight budget, so the switch changes no booking, no refusal
// and no answer of the request. A text the account cannot take is not made:
// the row goes without it and names it (record_lost, cause "memory"). The
// zero value records nothing; on is set once, from the door's ConsumerAuth.
type recording struct {
	on bool
	// held is what this request reserved and has not handed to its row.
	held int64
	// lost names the texts the account refused.
	lost []string
	// stream is a stream's relayed data lines; streamLost once the account
	// refused one of them (the lines after it are not kept either: a stream
	// with a hole is not the answer the client received).
	stream     strings.Builder
	streamHeld int64 // the part of held the stream's buffer takes: its capacity
	streamLost bool
}

// noLogLimit is the text limit of a recorded row: none (bodyLogWalkN).
const noLogLimit = math.MaxInt

// reserve takes n bytes from the account for this request.
func (r *recording) reserve(n int64) bool {
	if !llmlog.ReserveRecorded(n) {
		return false
	}
	r.held += n
	return true
}

// loseBody records that body (llmlog.RecordLostRequest/Response) was not
// kept.
func (r *recording) loseBody(body string) {
	r.lost = append(r.lost, body)
}

// text is the whole log text of raw (bodyLogWalkN without a limit), reserved
// first; "" and the body named lost when the account refuses it.
func (r *recording) text(raw []byte, body string) string {
	n := bodyLogSizeN(raw, noLogLimit)
	if !r.reserve(int64(n)) {
		r.loseBody(body)
		return ""
	}
	return bodyLogTextN(raw, n, noLogLimit)
}

// bytes is a copy of b, reserved first; "" and the body named lost when the
// account refuses it.
func (r *recording) bytes(b []byte, body string) string {
	if !r.reserve(int64(len(b))) {
		r.loseBody(body)
		return ""
	}
	return string(b)
}

// addStream appends one relayed data line of a stream. The buffer grows as
// strings.Builder.Grow grows it (to 2 × its capacity + the line, then the
// allocator's size class); the new capacity is reserved before the growth
// and the allocator's rounding right after it — the account holds the
// current buffer, the replaced one is garbage from then on. A refusal drops
// what the stream kept so far and every later line.
func (r *recording) addStream(line []byte) {
	if !r.on || r.streamLost || len(line) == 0 {
		return
	}
	if r.stream.Cap()-r.stream.Len() < len(line) {
		grown := int64(2*r.stream.Cap() + len(line))
		if !r.reserve(grown - r.streamHeld) {
			r.dropStream()
			return
		}
		r.streamHeld = grown
		r.stream.Grow(len(line))
		if c := int64(r.stream.Cap()); c > r.streamHeld {
			if !r.reserve(c - r.streamHeld) {
				r.dropStream()
				return
			}
			r.streamHeld = c
		}
	}
	r.stream.Write(line)
}

// dropStream gives the stream's buffer back and names the answer lost.
func (r *recording) dropStream() {
	r.held -= r.streamHeld
	llmlog.ReleaseRecorded(r.streamHeld)
	r.streamHeld = 0
	r.stream = strings.Builder{}
	r.streamLost = true
	r.loseBody(llmlog.RecordLostResponse)
}

// streamText is the stream's kept answer ("" after a refusal).
func (r *recording) streamText() string {
	if r.streamLost {
		return ""
	}
	return r.stream.String()
}

// mark puts the recording on the row: Recorded, the lost bodies and what the
// texts hold of the account, which from now on is the writer's to give back.
// Not recording: the row stays as it is.
func (r *recording) mark(row *llmlog.GatewayRow) {
	if !r.on {
		return
	}
	row.Recorded = true
	if len(r.lost) > 0 {
		row.RecordLost, row.RecordLostCause = r.lost, llmlog.RecordLostMemory
	}
	row.RecordedHeld, r.held = r.held, 0
}

// release gives back what no row took (a request that wrote no row of its
// own: no wire attempt, the shutdown's forced settle). Runs at the request's
// end (close).
func (r *recording) release() {
	llmlog.ReleaseRecorded(r.held)
	r.held = 0
}

// markForced puts the recording on the row the shutdown's forced settle
// writes: recorded, without a body (it is built at the admission, before
// there is an answer) — the request's text is named lost, and for chat the
// answer too.
func markForced(row *llmlog.GatewayRow, on bool) {
	if !on {
		return
	}
	row.Recorded = true
	row.RecordLost = []string{llmlog.RecordLostRequest}
	if row.Pipeline == chatPipeline {
		row.RecordLost = append(row.RecordLost, llmlog.RecordLostResponse)
	}
	row.RecordLostCause = llmlog.RecordLostShutdown
}
