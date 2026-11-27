package oai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/llmlog"
)

// Recording (issue #571, migration 197): a model consumer with the switch
// record_bodies on has every request it sends through this gateway held
// durably — nothing it sent is served without it, and nothing it sent is
// lost on the way to the log.
//
//   - The stage (RecordingStage, right behind the model-key door) opens a
//     recording for every request of such a consumer: an id, the door's
//     identity, the declared length, and the status the client gets
//     (recordingWriter). Every other consumer passes it untouched — no
//     wrapper, no allocation, no database call.
//   - The route hands it the body as soon as it is read (whole, or the part a
//     failed read got) and, before its walk — before the first backend
//     attempt and before any queue wait —, commits it (open: an INSERT of the
//     row in state open). A request whose row cannot be committed — the
//     database refuses, does not answer within recordWriteTimeout, or the
//     recording account has no room within the wait the gateway grants a
//     request anyway — is not served: 503 recording_unavailable,
//     x-should-retry: true, nothing goes to a backend. Every attempt of the
//     walk checks the commit again (ready).
//   - A non-streamed answer (chat, rerank; embeddings without the vectors)
//     is committed with the outcome before its first byte is written; a
//     failure there answers the request 503 recording_unavailable instead. A
//     stream keeps every line it relays (captured before the relay takes it)
//     and commits them when it ends; what a stream already sent cannot be
//     taken back, so a stream whose end cannot be committed leaves its row
//     open — the request held, the answer not — and is counted
//     (stream_answers_unrecorded).
//   - When the request has ended — the stage, after the route returned or
//     panicked — its outcome is committed: the open row is ended, or a
//     request the gateway refused before it opened one gets its row there,
//     with what was read of the body. A request for which no row could be
//     written at all is counted (unrecordable); it was refused, never served.
//
// The writes are synchronous and never queued; at most
// maxRecordingStatements of them run at once (a slot), so a hanging
// database binds that many connections of the process pool, never all of
// them. What they copy — pgx encodes a parameter into its own buffer and that
// into the connection's write buffer — and what a stream or a rerank answer
// keeps is held against ONE process-wide account (recordingAccount), not
// against the request's in-flight reservation: the switch changes no booking
// of the request, so a recorded request meets the same refusals as any other
// plus the one of its own. A write waits for a slot and for room in the
// account (backpressure); it never drops what it was given. The final
// statement of a captured answer takes its copies even past the account's
// bound (at most maxRecordingStatements at once): it already holds its
// capture, and waiting for room the other captures hold could wait on itself.

// recordingMessage is its text.
const recordingMessage = "The request could not be recorded; retry shortly."

// recordWriteTimeout bounds one recording statement (open, finish, insert):
// the database's share of the request frame. A named constant like llmlog's
// insertTimeout: it bounds the gateway's own bookkeeping, it is no policy of
// anybody's.
const recordWriteTimeout = 5 * time.Second

// recordAccountFloor is the recording account's size at the default caps:
// with gateway.inflight_budget's default of 512 MiB inside the container's
// 1536 MiB (docs/operations.md "Recording a consumer"). Larger caps raise it
// (recordAccountLimit) so that the largest request always fits.
const recordAccountFloor = 128 << 20

// maxRecordingStatements bounds the recording statements that run at once,
// process-wide — fixed in the code like llmlog's maxConcurrentInserts: it
// bounds the process's own use of its pool, it is no policy of anybody's.
const maxRecordingStatements = 4

// recordSlots are the statement slots.
var recordSlots = make(chan struct{}, maxRecordingStatements)

// recordCopyFactor is what one recording statement allocates per byte it
// carries: pgx encodes the parameter into its argument buffer, the Bind
// message copies it into the connection's write buffer, which grows by
// append once more on the next field (llmlog's TestRecordingStatementCost
// measured 2.0–2.3 × for bodies of 64 KiB to 8 MiB). Garbage after the
// statement, held against the account until it returns.
const recordCopyFactor = 3

// recordAccountLimit is the account's size under s: the floor, or what the
// row of a refused request at the caps holds while it is written — its body
// and the statement's copies —, whichever is larger.
func recordAccountLimit(s Settings) int64 {
	return max(int64(recordAccountFloor), (1+recordCopyFactor)*int64(max(s.MaxRequestBytes, s.MaxResponseBytes)))
}

// errRecordingUnavailable is the cause of a run whose recording failed (a
// stream's capture without room): verdict answers it.
var errRecordingUnavailable = errors.New("oai: the request could not be recorded")

// recordingAccount is the process-wide account of recorded bytes held
// (statement copies, captured answers). acquire waits for room; release
// wakes every waiter.
type recordingAccount struct {
	mu   sync.Mutex
	held int64
	wake chan struct{}
}

var recAccount = &recordingAccount{wake: make(chan struct{})}

// RecordingHeld is the bytes the recording account holds now.
func RecordingHeld() int64 {
	recAccount.mu.Lock()
	defer recAccount.mu.Unlock()
	return recAccount.held
}

// tryAcquire takes n bytes at once or not at all.
func (a *recordingAccount) tryAcquire(n, limit int64) bool {
	if n <= 0 {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held+n > limit {
		return false
	}
	a.held += n
	return true
}

// acquire takes n bytes, waiting for room until ctx ends; n beyond the
// whole account never fits.
func (a *recordingAccount) acquire(ctx context.Context, n, limit int64) error {
	if n > limit {
		return errors.New("oai: a recording larger than the recording account")
	}
	for {
		a.mu.Lock()
		if n <= 0 || a.held+n <= limit {
			a.held += max(n, 0)
			a.mu.Unlock()
			return nil
		}
		wake := a.wake
		a.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// take adds n bytes whatever the bound (a captured answer's final copies).
func (a *recordingAccount) take(n int64) {
	if n <= 0 {
		return
	}
	a.mu.Lock()
	a.held += n
	a.mu.Unlock()
}

// release gives n bytes back and wakes the waiters.
func (a *recordingAccount) release(n int64) {
	if n <= 0 {
		return
	}
	a.mu.Lock()
	a.held -= n
	close(a.wake)
	a.wake = make(chan struct{})
	a.mu.Unlock()
}

// RecordingStore is where the requests of recording consumers are held
// (llmlog.Recordings over the process pool, cmd/ctxd). Each call returns nil
// only once its statement is committed.
type RecordingStore interface {
	Open(ctx context.Context, q llmlog.RecordedRequest) error
	Finish(ctx context.Context, id string, o llmlog.RecordedOutcome) error
	Insert(ctx context.Context, q llmlog.RecordedRequest, o llmlog.RecordedOutcome) error
}

// RecordingStage is the stage right behind the model-key door that opens a
// recording for every request of a recording consumer (the package comment
// above). One per process, shared by /v1 and /v2 like the door.
type RecordingStage struct {
	store    RecordingStore
	consumer func(context.Context) *auth.ConsumerAuth
	settings func() Settings
	maxWait  func() time.Duration
}

// NewRecordingStage builds the stage over store, the door's consumer
// (handler.ConsumerAuthFromContext), the gateway settings and the
// dispatcher's external wait budget (the longest a request waits for room
// in the recording account). A nil store refuses every request of a
// recording consumer (recording_unavailable) — fail-closed.
func NewRecordingStage(store RecordingStore, consumer func(context.Context) *auth.ConsumerAuth,
	settings func() Settings, maxWait func() time.Duration) *RecordingStage {
	return &RecordingStage{store: store, consumer: consumer, settings: settings, maxWait: maxWait}
}

// Handler is the stage.
func (s *RecordingStage) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ca := s.consumer(r.Context())
		if ca == nil || !ca.RecordBodies {
			next.ServeHTTP(w, r)
			return
		}
		rr := s.begin(ca, r)
		defer func() {
			p := recover()
			rr.endStage(p, w)
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(recordingWriter{ResponseWriter: w, rr: rr}, r.WithContext(withRecording(r.Context(), rr)))
	})
}

// begin opens the request's recording and counts its admission.
func (s *RecordingStage) begin(ca *auth.ConsumerAuth, r *http.Request) *requestRecording {
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	rr := &requestRecording{stage: s, acct: recAccount, access: AccessRecordFrom(r.Context()), req: llmlog.RecordedRequest{
		ID: id.String(), ConsumerID: ca.ConsumerID, ModelKeyID: ca.ModelKeyID, Path: r.URL.Path,
		ClientRequestID: clientRequestID(r), Declared: r.ContentLength,
	}}
	if r.ContentLength < 0 {
		rr.req.Declared = -1
	}
	rr.stats = recStats.admit(ca.ConsumerID)
	return rr
}

// requestRecording is one request's recording. Its fields are the request
// goroutine's (the route and the stage run on it); status alone is written
// by the response writer, which a stream's grace timer can call too.
type requestRecording struct {
	stage  *RecordingStage
	acct   *recordingAccount
	access *AccessRecord // the access log's record: the refusal's code
	req    llmlog.RecordedRequest
	stats  *recordingConsumerStats

	statusMu sync.Mutex
	status   int

	// row: a row of the request is committed (open, or inserted at the
	// end); opened: it was opened before the walk; ended: its outcome is
	// committed (or known to be there).
	row, opened, ended bool
	// unfinished: the outcome of an opened row could not be committed.
	unfinished bool

	// capture is a stream's relayed lines or a rerank answer; held is what
	// it takes of the account; streamed: a stream relayed at least one line.
	capture  []byte
	held     int64
	streamed bool
}

type recordingKey struct{}

func withRecording(ctx context.Context, rr *requestRecording) context.Context {
	return context.WithValue(ctx, recordingKey{}, rr)
}

// recordingFrom is the request's recording; nil for a consumer that does not
// record.
func recordingFrom(ctx context.Context) *requestRecording {
	rr, _ := ctx.Value(recordingKey{}).(*requestRecording)
	return rr
}

// routeRecording is the recording a route works with: the stage's, or — for
// a recording consumer whose request reached the route without the stage (a
// wiring defect) — false: the route refuses it, it does not serve it
// unrecorded.
func routeRecording(r *http.Request, ca *auth.ConsumerAuth, op string) (*requestRecording, bool) {
	rr := recordingFrom(r.Context())
	if rr == nil {
		if ca != nil && ca.RecordBodies {
			slog.Error("gateway: a recording consumer's request reached the route without the recording stage (wiring defect) — refused",
				"path", r.URL.Path)
			return nil, false
		}
		return nil, true
	}
	rr.req.Op = op
	return rr, true
}

// setBody hands the recording what the route read of the body (whole when
// complete). Nil-safe: a request that does not record does nothing.
func (rr *requestRecording) setBody(body []byte, complete bool) {
	if rr == nil {
		return
	}
	rr.req.Body, rr.req.Complete = body, complete
}

// parsed notes the model name and the stream flag the route's parse read.
func (rr *requestRecording) parsed(model string, stream bool) {
	if rr == nil {
		return
	}
	rr.req.Model, rr.req.Stream = model, &stream
}

// ready reports whether the request may reach a backend: it does not record,
// or its row is open.
func (rr *requestRecording) ready() bool { return rr == nil || rr.opened }

// open commits the request's row before the walk; false when it cannot —
// the caller refuses the request (refuseRecording) and serves nothing. The
// wait for room and a slot is the dispatcher's external wait budget, the
// statement recordWriteTimeout.
func (rr *requestRecording) open(ctx context.Context) bool {
	if rr == nil || rr.opened {
		return true
	}
	if rr.stage.store == nil {
		slog.Error("gateway: a recording consumer's request without a recording store (wiring defect) — refused")
		return false
	}
	err := rr.statement(ctx, rr.stage.maxWait(), recordCopyFactor*int64(len(rr.req.Body)), false,
		func(sctx context.Context) error { return rr.stage.store.Open(sctx, rr.req) })
	if err != nil {
		slog.Warn("gateway: a request could not be recorded — refused", "consumer_id", rr.req.ConsumerID, "bytes", len(rr.req.Body), "error", err)
		return false
	}
	rr.row, rr.opened = true, true
	return true
}

// statement runs one recording statement: n bytes of the account first
// (waiting for room until wait ends; force: taken past the bound), then a
// slot (waiting until wait ends), then fn under recordWriteTimeout. In this
// order nobody waits while holding a slot — a slot holder only waits for the
// database.
func (rr *requestRecording) statement(ctx context.Context, wait time.Duration, n int64, force bool, fn func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if force {
		rr.acct.take(n)
	} else if err := rr.acct.acquire(wctx, n, recordAccountLimit(rr.stage.settings())); err != nil {
		return fmt.Errorf("oai: no room to record: %w", err)
	}
	defer rr.acct.release(n)
	select {
	case recordSlots <- struct{}{}:
	case <-wctx.Done():
		return fmt.Errorf("oai: no recording slot: %w", wctx.Err())
	}
	defer func() { <-recordSlots }()
	sctx, scancel := context.WithTimeout(ctx, recordWriteTimeout)
	defer scancel()
	return fn(sctx)
}

// answer commits the outcome of a non-streamed answer — status 200 and body
// as the client will get it (nil: an answer not kept, the embedding
// vectors) — before it is written; false when it cannot: the caller answers
// refuseRecording instead. The answer is ready: the request's own deadline
// does not cut its commit short (only recordWriteTimeout does).
func (rr *requestRecording) answer(ctx context.Context, body []byte) bool {
	if rr == nil {
		return true
	}
	if !rr.opened || rr.ended {
		return false // the walk ran on an open row (ready); anything else is a defect
	}
	if err := rr.finish(context.WithoutCancel(ctx), llmlog.RecordedOutcome{Status: http.StatusOK, Response: body}); err != nil {
		slog.Warn("gateway: an answer could not be recorded — refused", "consumer_id", rr.req.ConsumerID, "error", err)
		return false
	}
	return true
}

// finish commits o on the open row. The statement's copies of o.Response
// wait for room for an answer of the request's own (a chat answer); for a
// captured one (a stream's lines, a rerank answer) they are taken past the
// bound — the capture is held already.
func (rr *requestRecording) finish(ctx context.Context, o llmlog.RecordedOutcome) error {
	captured := len(o.Response) > 0 && len(rr.capture) > 0 && &o.Response[0] == &rr.capture[0]
	err := rr.statement(ctx, recordWriteTimeout, recordCopyFactor*int64(len(o.Response)), captured,
		func(sctx context.Context) error { return rr.stage.store.Finish(sctx, rr.req.ID, o) })
	if err != nil {
		return err
	}
	rr.ended = true
	return nil
}

// capture appends one piece of the answer — a stream's line before the
// relay takes it, a part of a rerank answer — against the account (no
// wait: the answer is being produced); false when there is no room: the
// caller ends the request (errRecordingUnavailable) instead of sending a
// piece it did not keep. Nil-safe.
func (rr *requestRecording) addCapture(p []byte) bool {
	if rr == nil || len(p) == 0 {
		return true
	}
	if need := len(rr.capture) + len(p); need > cap(rr.capture) {
		grown := max(2*cap(rr.capture), need)
		booked := int64(grown) // the buffer; its final statement's copies come at finish
		if !rr.acct.tryAcquire(booked, recordAccountLimit(rr.stage.settings())) {
			return false
		}
		next := make([]byte, len(rr.capture), grown)
		copy(next, rr.capture)
		rr.acct.release(rr.held) // the old buffer: garbage from here on
		rr.capture, rr.held = next, booked
	}
	rr.capture = append(rr.capture, p...)
	return true
}

// captureWriter writes into the request's capture (a rerank answer, kept
// whole before it goes out); a write it has no room for fails.
type captureWriter struct{ rr *requestRecording }

func (w captureWriter) Write(p []byte) (int, error) {
	if !w.rr.addCapture(p) {
		return 0, errRecordingUnavailable
	}
	return len(p), nil
}

// dropCaptureTail takes back the last n captured bytes (a line the relay
// refused after the capture took it).
func (rr *requestRecording) dropCaptureTail(n int) {
	if rr == nil || n <= 0 || n > len(rr.capture) {
		return
	}
	rr.capture = rr.capture[:len(rr.capture)-n]
}

// id is the request's recording id ("" without a recording).
func (rr *requestRecording) id() string {
	if rr == nil {
		return ""
	}
	return rr.req.ID
}

// mark puts the recording on the request's llmlog row: metadata.gw.recorded
// and recording_id — and the row carries no bodies (llmlog.GatewayEntry):
// the recording is their one place. Nil-safe.
func (rr *requestRecording) mark(row *llmlog.GatewayRow) {
	if rr == nil {
		return
	}
	row.Recorded, row.RecordingID = true, rr.req.ID
}

// noteStreamed marks that a stream relayed a line.
func (rr *requestRecording) noteStreamed() {
	if rr != nil {
		rr.streamed = true
	}
}

// writeStatus is what recordingWriter saw first (0: nothing written).
func (rr *requestRecording) writeStatus() int {
	rr.statusMu.Lock()
	defer rr.statusMu.Unlock()
	return rr.status
}

func (rr *requestRecording) noteStatus(code int) {
	rr.statusMu.Lock()
	if rr.status == 0 {
		rr.status = code
	}
	rr.statusMu.Unlock()
}

// end commits the request's outcome once it has ended (endStage): the open
// row ends with the status the client got, code and a stream's or a rerank
// answer's captured bytes; a request refused before its walk gets its row
// with what was read of the body — the body itself held against the account
// with its copies, since the request's in-flight reservation has ended. A
// failure is counted, never retried later.
func (rr *requestRecording) end(code string) {
	if rr.ended {
		rr.dropCapture()
		return
	}
	defer rr.dropCapture()
	ctx := context.Background()
	o := llmlog.RecordedOutcome{Status: rr.writeStatus(), Code: code}
	if len(rr.capture) > 0 {
		o.Response = rr.capture
	}
	if rr.opened {
		rr.req.Body = nil // in the row since the open
		if err := rr.finish(ctx, o); err != nil {
			rr.unfinished = true
			slog.Error("gateway: the outcome of a recorded request could not be recorded — its row stays open",
				"consumer_id", rr.req.ConsumerID, "recording_id", rr.req.ID, "stream_answer_lost", rr.streamed, "error", err)
		}
		return
	}
	if rr.stage.store == nil {
		slog.Error("gateway: a recording consumer's request without a recording store (wiring defect) — not recorded",
			"consumer_id", rr.req.ConsumerID)
		return
	}
	n := (1+recordCopyFactor)*int64(len(rr.req.Body)) + recordCopyFactor*int64(len(o.Response))
	if err := rr.statement(ctx, recordWriteTimeout, n, false,
		func(sctx context.Context) error { return rr.stage.store.Insert(sctx, rr.req, o) }); err != nil {
		slog.Error("gateway: a refused request of a recording consumer could not be recorded", "consumer_id", rr.req.ConsumerID, "error", err)
		return
	}
	rr.row, rr.ended = true, true
}

// dropCapture gives the captured answer back to the account.
func (rr *requestRecording) dropCapture() {
	rr.acct.release(rr.held)
	rr.capture, rr.held = nil, 0
}

// endStage is the stage's end of the request, after the route returned or
// panicked: the outcome with the access record's code — a recovered panic as
// the 503 internal_error oai.Recover answers, unless the response had begun
// —, then the request's count.
func (rr *requestRecording) endStage(p any, w http.ResponseWriter) {
	code := ""
	if rr.access != nil {
		code = rr.access.Reason
	}
	if p != nil && rr.writeStatus() == 0 {
		rr.noteStatus(http.StatusServiceUnavailable)
		code = CodeInternalError
	}
	if p == nil && code == CodeRecordingUnavailable {
		// The database is in trouble: the client gets its 503 now, not after
		// the row's own attempt below (up to recordWriteTimeout more).
		_ = http.NewResponseController(w).Flush()
	}
	rr.end(code)
	recStats.done(rr.stats, rr.row, rr.unfinished, rr.unfinished && rr.streamed)
}

// refuseRecording answers a request whose recording failed: 503
// recording_unavailable, Retry-After: 1, x-should-retry: true — a fault of
// the gateway's database, not of the request, and no rule's refusal (no
// ledger note).
func refuseRecording(rec *AccessRecord, w http.ResponseWriter) {
	if rec != nil {
		rec.Reason = CodeRecordingUnavailable
	}
	w.Header().Set("Retry-After", overloadedRetryAfter)
	WriteCode(w, CodeRecordingUnavailable, recordingMessage, true)
}

// recordingWriter notes the status the client gets on the request's
// recording. Unwrap lets http.ResponseController reach the connection
// (flush, write deadlines).
type recordingWriter struct {
	http.ResponseWriter
	rr *requestRecording
}

func (w recordingWriter) WriteHeader(code int) {
	if code >= http.StatusOK {
		w.rr.noteStatus(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w recordingWriter) Write(p []byte) (int, error) {
	w.rr.noteStatus(http.StatusOK)
	return w.ResponseWriter.Write(p)
}

func (w recordingWriter) FlushError() error {
	w.rr.noteStatus(http.StatusOK)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w recordingWriter) Flush() { _ = w.FlushError() }

func (w recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recordingConsumerStats are one recording consumer's counts in this
// process (GET /api/status, docs/operations.md "Recording a consumer"):
// admitted = in_flight + recorded + unrecordable at every moment.
type recordingConsumerStats struct {
	admitted, inFlight, recorded, unrecordable int64
	unfinished, streamAnswersUnrecorded        int64
}

// recordingStats are the counts of every recording consumer since the
// process started.
type recordingStats struct {
	mu   sync.Mutex
	by   map[string]*recordingConsumerStats
	from time.Time
}

var recStats = &recordingStats{by: map[string]*recordingConsumerStats{}, from: time.Now().UTC()}

func (s *recordingStats) admit(consumer string) *recordingConsumerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.by[consumer]
	if c == nil {
		c = &recordingConsumerStats{}
		s.by[consumer] = c
	}
	c.admitted++
	c.inFlight++
	return c
}

func (s *recordingStats) done(c *recordingConsumerStats, row, unfinished, streamLost bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.inFlight--
	if row {
		c.recorded++
	} else {
		c.unrecordable++
	}
	if unfinished {
		c.unfinished++
	}
	if streamLost {
		c.streamAnswersUnrecorded++
	}
}

// RecordingConsumer is one recording consumer's counts in GET /api/status
// (gateway.recording.consumers).
type RecordingConsumer struct {
	ConsumerID string `json:"consumer_id"`
	// Admitted requests passed the door as this consumer; each is in
	// flight, recorded (its row is in context_llm_recordings) or
	// unrecordable (no row could be written: an inference request was
	// refused, not served; a model list may have been answered).
	Admitted     int64 `json:"admitted"`
	InFlight     int64 `json:"in_flight"`
	Recorded     int64 `json:"recorded"`
	Unrecordable int64 `json:"unrecordable"`
	// Unfinished rows were opened and their outcome could not be recorded
	// (they stay open); StreamAnswersUnrecorded are the streams among them
	// that had sent lines.
	Unfinished              int64 `json:"unfinished"`
	StreamAnswersUnrecorded int64 `json:"stream_answers_unrecorded"`
}

// RecordingSnapshot is gateway.recording of GET /api/status.
type RecordingSnapshot struct {
	// ProcessID is context_llm_recordings.process_id of this process's
	// rows, Since when its counts began.
	ProcessID string    `json:"process_id"`
	Since     time.Time `json:"since"`
	// HeldBytes is what the recording account holds now.
	HeldBytes int64               `json:"held_bytes"`
	Consumers []RecordingConsumer `json:"consumers"`
}

// RecordingStats is the snapshot of this process's recording counts, sorted
// by consumer.
func RecordingStats() RecordingSnapshot {
	snap := RecordingSnapshot{ProcessID: llmlog.ProcessID, HeldBytes: RecordingHeld(), Consumers: []RecordingConsumer{}}
	recStats.mu.Lock()
	snap.Since = recStats.from
	for id, c := range recStats.by {
		snap.Consumers = append(snap.Consumers, RecordingConsumer{ConsumerID: id,
			Admitted: c.admitted, InFlight: c.inFlight, Recorded: c.recorded, Unrecordable: c.unrecordable,
			Unfinished: c.unfinished, StreamAnswersUnrecorded: c.streamAnswersUnrecorded})
	}
	recStats.mu.Unlock()
	sort.Slice(snap.Consumers, func(i, j int) bool { return snap.Consumers[i].ConsumerID < snap.Consumers[j].ConsumerID })
	return snap
}
