package oai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// The recording of a recording consumer on the routes (issue #571, record.go):
// the request is held — the store's Open returned — before the first
// backend attempt, or it is not served; a non-streamed answer is held before
// it is written, or the client gets 503 recording_unavailable instead; a
// stream's relayed lines are held when it ends; every request the gateway
// refuses after the door leaves its row with what was read; and a consumer
// without the switch never reaches the store.

// recSecret is a credentials pattern in a prompt: the sensitivity scan raises
// a request carrying it to credentials, where the body policy stores nothing.
const recSecret = "password=hunter2-recorded-571"

// recordingConsumer is the rig's consumer with the switch set to on or off.
func recordingConsumer(on bool) *auth.ConsumerAuth {
	ca := urgentApp()
	ca.RecordBodies = on
	return ca
}

// answerIdentity drops what differs between two requests by construction:
// the answer's id and created stamp.
var answerIdentity = regexp.MustCompile(`"(id|created)":("[^"]*"|\d+)`)

func sameAnswer(a, b []byte) bool {
	return answerIdentity.ReplaceAllString(string(a), "") == answerIdentity.ReplaceAllString(string(b), "")
}

// recRow is one row of the fake store.
type recRow struct {
	req        llmlog.RecordedRequest
	body       []byte // a copy of req.Body at the write
	state      string
	out        llmlog.RecordedOutcome
	response   []byte // a copy of out.Response
	hitsAtOpen int64
	opened     bool
}

// recStore is a RecordingStore in memory: it notes every call, the upstream
// hits at the open, and can refuse or hang any of the three statements.
type recStore struct {
	mu    sync.Mutex
	rows  map[string]*recRow
	order []string
	calls atomic.Int64
	hits  func() int64

	openErr, finishErr, insertErr error
	openHang                      bool
	// openGate, when set, holds every Open until it is closed; inOpen and
	// maxInOpen count the Opens running at once.
	openGate          chan struct{}
	inOpen, maxInOpen atomic.Int64
	// finishFailures refuses that many Finish calls before it takes one.
	finishFailures int
	// finishLost commits that many Finish calls and still returns an error
	// (a commit whose acknowledgement was lost on the way back).
	finishLost int
	// finishGate, when set, holds every Finish until it is closed; inFinish
	// counts the Finish calls waiting there.
	finishGate chan struct{}
	inFinish   atomic.Int64
}

func newRecStore(hits func() int64) *recStore {
	return &recStore{rows: map[string]*recRow{}, hits: hits}
}

func (s *recStore) Open(ctx context.Context, q llmlog.RecordedRequest) error {
	s.calls.Add(1)
	n := s.inOpen.Add(1)
	defer s.inOpen.Add(-1)
	for m := s.maxInOpen.Load(); n > m && !s.maxInOpen.CompareAndSwap(m, n); m = s.maxInOpen.Load() {
	}
	s.mu.Lock()
	hang, err, gate := s.openHang, s.openErr, s.openGate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var hits int64
	if s.hits != nil {
		hits = s.hits()
	}
	s.rows[q.ID] = &recRow{req: q, body: append([]byte(nil), q.Body...), state: llmlog.RecordingOpen, hitsAtOpen: hits, opened: true}
	s.order = append(s.order, "open")
	return nil
}

func (s *recStore) Finish(ctx context.Context, id string, o llmlog.RecordedOutcome) error {
	s.calls.Add(1)
	s.mu.Lock()
	gate := s.finishGate
	s.mu.Unlock()
	if gate != nil {
		s.inFinish.Add(1)
		select {
		case <-gate:
		case <-ctx.Done():
			s.inFinish.Add(-1)
			return ctx.Err()
		}
		s.inFinish.Add(-1)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finishErr != nil {
		return s.finishErr
	}
	if s.finishFailures > 0 {
		s.finishFailures--
		return errors.New("finish refused once")
	}
	r := s.rows[id]
	if r == nil || (r.state != llmlog.RecordingOpen && !o.Supersedes) {
		return errors.New("no open row")
	}
	r.state, r.out, r.response = llmlog.RecordingDone, o, append([]byte(nil), o.Response...)
	s.order = append(s.order, "finish")
	if s.finishLost > 0 {
		s.finishLost--
		return errors.New("finish committed, its acknowledgement lost")
	}
	return nil
}

func (s *recStore) Insert(_ context.Context, q llmlog.RecordedRequest, o llmlog.RecordedOutcome) error {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return s.insertErr
	}
	s.rows[q.ID] = &recRow{req: q, body: append([]byte(nil), q.Body...), state: llmlog.RecordingDone, out: o, response: append([]byte(nil), o.Response...)}
	s.order = append(s.order, "insert")
	return nil
}

func (s *recStore) set(f func(s *recStore)) {
	s.mu.Lock()
	f(s)
	s.mu.Unlock()
}

// only is the one row of the store.
func (s *recStore) only(t *testing.T) *recRow {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) != 1 {
		t.Fatalf("%d recording rows, want 1 (calls %v)", len(s.rows), s.order)
	}
	for _, r := range s.rows {
		return r
	}
	return nil
}

// withRecordings mounts the recording stage over store (its wait for room is
// the rig's MaxWait).
func withRecordings(store oai.RecordingStore) rigOption {
	return func(r *chatRig) {
		r.recStage = oai.NewRecordingStage(store, r.deps.Consumer, func() oai.Settings { return *r.settings.Load() },
			func() time.Duration { return r.deps.MaxWait() })
	}
}

// statsOf is the consumer's counts in this process.
func statsOf(id string) oai.RecordingConsumer {
	for _, c := range oai.RecordingStats().Consumers {
		if c.ConsumerID == id {
			return c
		}
	}
	return oai.RecordingConsumer{ConsumerID: id}
}

func checkRecordingRefusal(t *testing.T, status int, header http.Header, body []byte) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	if status != http.StatusServiceUnavailable || env.Error.Code != oai.CodeRecordingUnavailable ||
		header.Get("Retry-After") != "1" || header.Get("X-Should-Retry") != "true" {
		t.Errorf("answer %d %q (Retry-After %q, x-should-retry %q), want 503 recording_unavailable, Retry-After 1, x-should-retry true: %.200s",
			status, env.Error.Code, header.Get("Retry-After"), header.Get("X-Should-Retry"), body)
	}
}

const recChatAnswer = `{"id":"gen-1","object":"chat.completion","created":1700000000,"model":"M",` +
	`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer"}}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`

// TestRecordingHoldsTheRequestBeforeAnyBackend: for every route, the
// recording consumer's request is in the store — the exact bytes sent, a
// credentials pattern past 64 KiB included — before the backend saw it; the
// row ends done with the answer the client got (embeddings: none); the
// answer, the upstream body and the required class are those of the same
// request without the switch; the llmlog row names the recording and
// carries no bodies (TestRecordedLLMLogRowCarriesNoBodies).
func TestRecordingHoldsTheRequestBeforeAnyBackend(t *testing.T) {
	long := strings.Repeat("abcdefgh", 12<<10) // 96 KiB, past the 64 KiB cap
	t.Run("chat", func(t *testing.T) {
		u := newUpstream(t, recChatAnswer)
		store := newRecStore(func() int64 { return u.hits.Load() })
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
		rig.setSettings(func(s *oai.Settings) { s.BodyLogging = llmlog.BodyLoggingFull })
		body := `{"model":"pub","messages":[{"role":"user","content":"` + recSecret + " " + long + `"}]}`
		rig.ca.Store(recordingConsumer(false))
		off := rig.post(t, body)
		if store.calls.Load() != 0 {
			t.Fatalf("a consumer without the switch reached the store (%d calls)", store.calls.Load())
		}
		rig.ca.Store(recordingConsumer(true))
		on := rig.post(t, body)
		if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
			t.Fatalf("status off %d on %d, answers equal %v", off.status, on.status, sameAnswer(off.body, on.body))
		}
		if len(u.raws) != 2 || string(u.raws[0]) != string(u.raws[1]) {
			t.Errorf("the upstream bodies differ with the switch (%d calls)", len(u.raws))
		}
		r := store.only(t)
		if !r.opened || r.hitsAtOpen != 1 { // the first hit was the unrecorded request
			t.Errorf("open %v with %d upstream hits, want the open before the recorded request's backend call (1: the earlier unrecorded one)", r.opened, r.hitsAtOpen)
		}
		if string(r.body) != body || !r.req.Complete || r.req.Op != llmlog.GatewayOpChat || r.req.Model != "pub" || r.req.Stream == nil || *r.req.Stream {
			t.Errorf("held request: %d bytes (want %d, exact), complete %v, op %q, model %q, stream %v", len(r.body), len(body), r.req.Complete, r.req.Op, r.req.Model, r.req.Stream)
		}
		if r.state != llmlog.RecordingDone || r.out.Status != 200 || string(r.response) != string(on.body) {
			t.Errorf("row: state %q status %d answer %d bytes, want done 200 with the client's %d bytes", r.state, r.out.Status, len(r.response), len(on.body))
		}
		if !strings.Contains(string(on.body), `"id":"chatcmpl-`+r.req.ID+`"`) {
			t.Errorf("the answer's id does not name the recording %s: %.120s", r.req.ID, on.body)
		}
		rows := rig.rows.all()
		if len(rows) != 2 || rows[0].Recorded || !rows[1].Recorded || rows[1].RecordingID != r.req.ID {
			t.Fatalf("llmlog rows %d: recorded %v/%v, recording id %q (want %q)", len(rows), rows[0].Recorded, rows[1].Recorded, rows[1].RecordingID, r.req.ID)
		}
		eo, en := llmlog.GatewayEntry(rows[0]), llmlog.GatewayEntry(rows[1])
		if en.RequestUser != "" || en.ResponseContent != "" || en.RequiredSensitivity != eo.RequiredSensitivity {
			t.Errorf("the recorded llmlog row: %d/%d body bytes (want none), class %q vs %q without the switch",
				len(en.RequestUser), len(en.ResponseContent), en.RequiredSensitivity, eo.RequiredSensitivity)
		}
	})
	t.Run("stream", func(t *testing.T) {
		sh := streamShapes[0]
		u := newUpstream(t, "")
		u.setRespond(sseRespond(sh.steps(), nil, nil))
		store := newRecStore(func() int64 { return u.hits.Load() })
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
		srv := serveHTTP(t, rig.router)
		rig.ca.Store(recordingConsumer(false))
		off := postStream(context.Background(), t, srv, streamBody("chat"))
		rig.ca.Store(recordingConsumer(true))
		on := postStream(context.Background(), t, srv, streamBody("chat"))
		checkRelay(t, on)
		if !sameAnswer([]byte(off.raw), []byte(on.raw)) {
			t.Errorf("the stream differs with the switch:\n off %q\n on  %q", off.raw, on.raw)
		}
		r := store.only(t)
		if r.hitsAtOpen != 1 || r.req.Stream == nil || !*r.req.Stream || string(r.body) != streamBody("chat") {
			t.Errorf("open with %d upstream hits (want 1), stream %v, request %q", r.hitsAtOpen, r.req.Stream, r.body)
		}
		if r.state != llmlog.RecordingDone || r.out.Status != 200 || string(r.response)+"data: [DONE]\n\n" != on.raw {
			t.Errorf("row: state %q status %d answer %q, want done 200 with the client's lines before [DONE]: %q", r.state, r.out.Status, r.response, on.raw)
		}
	})
	t.Run("embeddings", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		store := newRecStore(func() int64 { return e.hits.Load() })
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withRecordings(store))
		body := embedBody("emb1", []string{recSecret + " " + long, "second"})
		rig.ca.Store(recordingConsumer(false))
		off := rig.postEmbed(t, body)
		offHits := e.hits.Load()
		rig.ca.Store(recordingConsumer(true))
		on := rig.postEmbed(t, body)
		if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
			t.Fatalf("status off %d on %d, answers equal %v: %.200s", off.status, on.status, sameAnswer(off.body, on.body), on.body)
		}
		r := store.only(t)
		if r.hitsAtOpen != offHits || e.hits.Load() != 2*offHits || string(r.body) != body || r.req.Op != llmlog.GatewayOpEmbed {
			t.Errorf("open with %d engine hits (want %d, the unrecorded request's), request %d bytes (want %d), op %q", r.hitsAtOpen, offHits, len(r.body), len(body), r.req.Op)
		}
		if r.state != llmlog.RecordingDone || r.out.Status != 200 || r.response != nil {
			t.Errorf("row: state %q status %d answer %d bytes, want done 200 without the vectors", r.state, r.out.Status, len(r.response))
		}
	})
	t.Run("rerank", func(t *testing.T) {
		e := newRerankEngine(t)
		store := newRecStore(func() int64 { return e.hits.Load() })
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(store))
		body := rerankBody("rrpub", "which document?", []string{recSecret + " " + long, "other"})
		rig.ca.Store(recordingConsumer(false))
		off := rig.postRerank(t, "/v1/rerank", body)
		rig.ca.Store(recordingConsumer(true))
		on := rig.postRerank(t, "/v1/rerank", body)
		if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
			t.Fatalf("status off %d on %d, answers equal %v: %.200s", off.status, on.status, sameAnswer(off.body, on.body), on.body)
		}
		r := store.only(t)
		if r.hitsAtOpen != 1 || string(r.body) != body || r.req.Op != llmlog.GatewayOpRerank {
			t.Errorf("open with %d engine hits (want 1), request %d bytes (want %d), op %q", r.hitsAtOpen, len(r.body), len(body), r.req.Op)
		}
		if r.state != llmlog.RecordingDone || r.out.Status != 200 || string(r.response) != string(on.body) {
			t.Errorf("row: state %q status %d answer %q, want done 200 with the client's answer %q", r.state, r.out.Status, r.response, on.body)
		}
	})
}

// TestRecordingUnheldIsNotServed: a request that cannot be held — the store
// refuses, hangs past recordWriteTimeout, or the account has no room within
// the wait — gets 503 recording_unavailable and never reaches a backend; its
// row is written at its end if the store takes it then.
func TestRecordingUnheldIsNotServed(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(s *recStore) (undo func())
	}{
		{"store_refuses", func(s *recStore) func() {
			s.set(func(s *recStore) { s.openErr = errors.New("database down") })
			return func() {}
		}},
		{"store_hangs", func(s *recStore) func() {
			s.set(func(s *recStore) { s.openHang = true })
			return func() {}
		}},
		{"account_full", func(*recStore) func() {
			return oai.FillRecordingAccount(oai.RecordingAccountLimit(chatSettings()))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "store_hangs" && testing.Short() {
				t.Skip("waits recordWriteTimeout (5 s)")
			}
			u := newUpstream(t, recChatAnswer)
			store := newRecStore(func() int64 { return u.hits.Load() })
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
				withDeps(func(d *oai.Deps) { d.MaxWait = func() time.Duration { return 50 * time.Millisecond } }), withRecordings(store))
			rig.ca.Store(recordingConsumer(true))
			before := statsOf(urgentApp().ConsumerID)
			undo := tc.prep(store)
			a := rig.post(t, chatBody("pub"))
			undo()
			checkRecordingRefusal(t, a.status, a.header, a.body)
			if n := u.hits.Load(); n != 0 {
				t.Errorf("the backend was called %d times for a request that was not held", n)
			}
			if rig.limiter.aborts.Load() != 1 {
				t.Errorf("the meter's reservation: %d aborts, want 1 (no wire contact)", rig.limiter.aborts.Load())
			}
			got := statsOf(urgentApp().ConsumerID)
			if tc.name == "account_full" {
				// The account stayed full through the request's end: no row
				// could be written — counted, the request was refused.
				if store.calls.Load() != 0 || got.Unrecordable-before.Unrecordable != 1 || got.Recorded != before.Recorded {
					t.Errorf("full account: %d store calls, unrecordable +%d, recorded +%d — want 0, +1, +0",
						store.calls.Load(), got.Unrecordable-before.Unrecordable, got.Recorded-before.Recorded)
				}
				return
			}
			// The store takes the row at the request's end: refused, with its body.
			r := store.only(t)
			if r.opened || r.out.Status != 503 || r.out.Code != oai.CodeRecordingUnavailable || string(r.body) != chatBody("pub") {
				t.Errorf("row at the end: opened %v status %d code %q body %q", r.opened, r.out.Status, r.out.Code, r.body)
			}
			if got.Recorded-before.Recorded != 1 || got.Unrecordable != before.Unrecordable {
				t.Errorf("recorded +%d unrecordable +%d, want +1 +0", got.Recorded-before.Recorded, got.Unrecordable-before.Unrecordable)
			}
		})
	}
}

// TestRecordedAnswerHeldBeforeItIsWritten: a non-streamed answer the store
// cannot take is not written — the client gets 503 recording_unavailable,
// never the answer — on chat, embeddings and rerank; the row stays open and
// counts as unfinished. A stream cannot take its lines back: it ends as it
// would, its row stays open and counts as a stream answer not recorded.
func TestRecordedAnswerHeldBeforeItIsWritten(t *testing.T) {
	failFinish := func(s *recStore) { s.finishErr = errors.New("database down") }
	check := func(t *testing.T, store *recStore, status int, header http.Header, body []byte, before oai.RecordingConsumer) {
		t.Helper()
		checkRecordingRefusal(t, status, header, body)
		if strings.Contains(string(body), "the answer") || strings.Contains(string(body), "relevance_score") || strings.Contains(string(body), `"embedding"`) {
			t.Errorf("the client got the answer that was not held: %.200s", body)
		}
		r := store.only(t)
		if r.state != llmlog.RecordingOpen {
			t.Errorf("row state %q, want open (its outcome could not be held)", r.state)
		}
		got := statsOf(urgentApp().ConsumerID)
		if got.Unfinished-before.Unfinished != 1 || got.Recorded-before.Recorded != 1 {
			t.Errorf("unfinished +%d recorded +%d, want +1 +1", got.Unfinished-before.Unfinished, got.Recorded-before.Recorded)
		}
	}
	t.Run("chat", func(t *testing.T) {
		u := newUpstream(t, recChatAnswer)
		store := newRecStore(nil)
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
		rig.ca.Store(recordingConsumer(true))
		store.set(failFinish)
		before := statsOf(urgentApp().ConsumerID)
		a := rig.post(t, chatBody("pub"))
		check(t, store, a.status, a.header, a.body, before)
	})
	t.Run("embeddings", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		store := newRecStore(nil)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withRecordings(store))
		rig.ca.Store(recordingConsumer(true))
		store.set(failFinish)
		before := statsOf(urgentApp().ConsumerID)
		a := rig.postEmbed(t, embedBody("emb1", []string{"a"}))
		check(t, store, a.status, a.header, a.body, before)
	})
	t.Run("rerank", func(t *testing.T) {
		e := newRerankEngine(t)
		store := newRecStore(nil)
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(store))
		rig.ca.Store(recordingConsumer(true))
		store.set(failFinish)
		before := statsOf(urgentApp().ConsumerID)
		a := rig.postRerank(t, "/v1/rerank", rerankBody("rrpub", "q", []string{"d1", "d2"}))
		check(t, store, a.status, a.header, a.body, before)
	})
	t.Run("stream", func(t *testing.T) {
		sh := streamShapes[0]
		u := newUpstream(t, "")
		u.setRespond(sseRespond(sh.steps(), nil, nil))
		store := newRecStore(nil)
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
		srv := serveHTTP(t, rig.router)
		rig.ca.Store(recordingConsumer(true))
		store.set(failFinish)
		before := statsOf(urgentApp().ConsumerID)
		on := postStream(context.Background(), t, srv, streamBody("chat"))
		checkRelay(t, on)
		r := store.only(t)
		got := statsOf(urgentApp().ConsumerID)
		if r.state != llmlog.RecordingOpen || got.StreamAnswersUnrecorded-before.StreamAnswersUnrecorded != 1 || got.Unfinished-before.Unfinished != 1 {
			t.Errorf("row %q, stream answers unrecorded +%d, unfinished +%d — want open, +1, +1",
				r.state, got.StreamAnswersUnrecorded-before.StreamAnswersUnrecorded, got.Unfinished-before.Unfinished)
		}
	})
}

// TestRecordingStreamWithoutRoomEnds: a stream whose next line the account
// cannot take ends before that line with recording_unavailable — every line
// the client got is a line the recording kept.
func TestRecordingStreamWithoutRoomEnds(t *testing.T) {
	sh := streamShapes[0]
	u := newUpstream(t, "")
	u.setRespond(sseRespond(sh.steps(), nil, nil))
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
	srv := serveHTTP(t, rig.router)
	rig.ca.Store(recordingConsumer(true))
	// The open's copies fit; the stream's first line does not.
	limit := oai.RecordingAccountLimit(chatSettings())
	release := oai.FillRecordingAccount(limit - int64(oai.RecordCopyFactor*len(streamBody("chat"))))
	on := postStream(context.Background(), t, srv, streamBody("chat"))
	release()
	r := store.only(t)
	if !strings.Contains(on.raw, oai.CodeRecordingUnavailable) {
		t.Errorf("the stream did not end with recording_unavailable: %d %q", on.status, on.raw)
	}
	if strings.Contains(on.raw, `"delta"`) || len(r.response) != 0 {
		t.Errorf("a line went out that the recording did not keep: client %q, kept %q", on.raw, r.response)
	}
}

// TestRecordingRefusedRequestsLeaveTheirRow: a request the gateway refuses
// after the door — a body that does not parse, a body cut short, an unknown
// model, the meter's refusal — leaves one row, never opened, with what was
// read of the body and the status, code and body of the refusal the client
// got.
func TestRecordingRefusedRequestsLeaveTheirRow(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		declared   int64
		limiter    bool
		status     int
		code       string
		complete   bool
	}{
		{"unparsable", `{"model":"pub","messages":[{"role":"user","content":"` + recSecret + `"`, -1, false, 400, "invalid_json", true},
		{"cut_short", `{"model":"pub","messages":`, 400, false, 400, "invalid_json", false},
		{"unknown_model", `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`, -1, false, 404, "model_not_found", true},
		{"limiter", chatBody("pub"), -1, true, 429, "rate_limit_exceeded", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, recChatAnswer)
			store := newRecStore(nil)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
			rig.ca.Store(recordingConsumer(true))
			if tc.limiter {
				rig.limiter.refuse = &oai.LimitRefusal{Code: oai.CodeRateLimitExceeded, Message: "slow down", ShouldRetry: true, RetryAfter: time.Second}
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			if tc.declared >= 0 {
				req.ContentLength = tc.declared
			}
			w := httptest.NewRecorder()
			rig.router.ServeHTTP(w, req)
			r := store.only(t)
			if w.Code != tc.status || r.opened || r.out.Status != tc.status || r.out.Code != tc.code {
				t.Errorf("client %d, row opened %v status %d code %q — want %d %q, never opened", w.Code, r.opened, r.out.Status, r.out.Code, tc.status, tc.code)
			}
			if string(r.body) != tc.body || r.req.Complete != tc.complete {
				t.Errorf("row body %q complete %v, want %q complete %v", r.body, r.req.Complete, tc.body, tc.complete)
			}
			if string(r.response) != w.Body.String() {
				t.Errorf("row answer %q — want the refusal the client got %q", r.response, w.Body.String())
			}
			if u.hits.Load() != 0 {
				t.Errorf("a refused request reached the backend")
			}
		})
	}
}

// TestRecordingStagePassesOtherConsumers: for a consumer without the switch
// the stage hands the route the very writer and request it got — no
// wrapper, no context value, no store call; for a recording consumer it does
// not.
func TestRecordingStagePassesOtherConsumers(t *testing.T) {
	store := newRecStore(nil)
	var ca atomic.Pointer[auth.ConsumerAuth]
	stage := oai.NewRecordingStage(store, func(context.Context) *auth.ConsumerAuth { return ca.Load() },
		func() oai.Settings { return chatSettings() }, func() time.Duration { return time.Second })
	var sawW http.ResponseWriter
	var sawR *http.Request
	h := stage.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sawW, sawR = w, r }))
	for _, on := range []bool{false, true} {
		ca.Store(recordingConsumer(on))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
		h.ServeHTTP(w, req)
		same := sawW == http.ResponseWriter(w) && sawR == req
		if same == on {
			t.Errorf("switch %v: the route got the stage's own writer and request %v, want %v", on, same, !on)
		}
	}
	if n := store.calls.Load(); n != 1 {
		t.Errorf("%d store calls, want 1 (the recording consumer's row, none for the other)", n)
	}
}

// TestRecordingPanicLeavesTheRow: a request that panics behind the stage
// leaves its row with the 503 internal_error oai.Recover answers.
func TestRecordingPanicLeavesTheRow(t *testing.T) {
	store := newRecStore(nil)
	stage := oai.NewRecordingStage(store, func(context.Context) *auth.ConsumerAuth { return recordingConsumer(true) },
		func() oai.Settings { return chatSettings() }, func() time.Duration { return time.Second })
	rt := chi.NewRouter()
	rt.Use(oai.Recover, stage.Handler)
	rt.Post("/v1/chat/completions", func(http.ResponseWriter, *http.Request) { panic("boom") })
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	r := store.only(t)
	if w.Code != 503 || r.out.Status != 503 || r.out.Code != oai.CodeInternalError {
		t.Errorf("client %d, row status %d code %q — want 503 internal_error both", w.Code, r.out.Status, r.out.Code)
	}
}

// TestRecordingCountsBalance: whatever happened to the requests, a recording
// consumer's counts hold admitted = in_flight + recorded + unrecordable, and
// recorded is the number of rows the store holds for it.
func TestRecordingCountsBalance(t *testing.T) {
	u := newUpstream(t, recChatAnswer)
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
	ca := recordingConsumer(true)
	ca.ConsumerID = uuid.NewString() // its own counts: the stats are process-wide (-count=N runs again)
	rig.ca.Store(ca)
	held := oai.RecordingHeld()
	rig.post(t, chatBody("pub"))  // served
	rig.post(t, `{"model":"pub"`) // refused before the walk
	store.set(func(s *recStore) { s.openErr = errors.New("down"); s.insertErr = errors.New("down") })
	rig.post(t, chatBody("pub")) // not held, not served, no row at all
	store.set(func(s *recStore) { s.openErr, s.insertErr, s.finishErr = nil, nil, errors.New("down") })
	rig.post(t, chatBody("pub")) // held, its answer not: refused, the row stays open
	got := statsOf(ca.ConsumerID)
	store.mu.Lock()
	rows := int64(len(store.rows))
	store.mu.Unlock()
	if got.Admitted != 4 || got.InFlight != 0 || got.Recorded != 3 || got.Unrecordable != 1 || got.Unfinished != 1 ||
		got.Admitted != got.InFlight+got.Recorded+got.Unrecordable || got.Recorded != rows {
		t.Errorf("counts %+v with %d rows — want admitted 4 = in flight 0 + recorded 3 + unrecordable 1, unfinished 1, recorded = rows", got, rows)
	}
	if after := oai.RecordingHeld(); after != held {
		t.Errorf("the recording account holds %d bytes after the requests ended, %d before — a request kept what it took", after, held)
	}
}

// TestRecordingAccountGivesBack: a stream's and a rerank answer's captures
// go back to the account when their requests end — served or not.
func TestRecordingAccountGivesBack(t *testing.T) {
	held := oai.RecordingHeld()
	sh := streamShapes[0]
	u := newUpstream(t, "")
	u.setRespond(sseRespond(sh.steps(), nil, nil))
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
	srv := serveHTTP(t, rig.router)
	rig.ca.Store(recordingConsumer(true))
	_ = postStream(context.Background(), t, srv, streamBody("chat"))
	store.set(func(s *recStore) { s.finishErr = errors.New("down") })
	_ = postStream(context.Background(), t, srv, streamBody("chat"))
	e := newRerankEngine(t)
	rstore := newRecStore(nil)
	rr := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(rstore))
	rr.ca.Store(recordingConsumer(true))
	_ = rr.postRerank(t, "/v1/rerank", rerankBody("rrpub", "q", []string{"d1", "d2"}))
	rstore.set(func(s *recStore) { s.finishErr = errors.New("down") })
	_ = rr.postRerank(t, "/v1/rerank", rerankBody("rrpub", "q", []string{"d1", "d2"}))
	if after := oai.RecordingHeld(); after != held {
		t.Errorf("the recording account holds %d bytes after the requests ended, %d before", after, held)
	}
}

// TestRecordingWithoutStageIsRefused: a recording consumer's request that
// reaches a route without the recording stage (a wiring defect) is refused
// with recording_unavailable — never served unrecorded.
func TestRecordingWithoutStageIsRefused(t *testing.T) {
	u := newUpstream(t, recChatAnswer)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	rig.ca.Store(recordingConsumer(true))
	a := rig.post(t, chatBody("pub"))
	checkRecordingRefusal(t, a.status, a.header, a.body)
	if n := u.hits.Load(); n != 0 {
		t.Errorf("the backend was called %d times for a recording consumer without the stage", n)
	}
}

// TestRecordingStreamBooksItsBuffer: a stream's capture holds its buffer of
// the account, not a multiple of it — a stream whose lines fit twice into
// the room left is served whole and kept.
func TestRecordingStreamBooksItsBuffer(t *testing.T) {
	sh := streamShapes[0]
	u := newUpstream(t, "")
	u.setRespond(sseRespond(sh.steps(), nil, nil))
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
	srv := serveHTTP(t, rig.router)
	rig.ca.Store(recordingConsumer(true))
	first := postStream(context.Background(), t, srv, streamBody("chat"))
	lines := len(store.only(t).response)
	if lines == 0 {
		t.Fatalf("the first stream kept nothing: %q", first.raw)
	}
	store.set(func(s *recStore) { s.rows = map[string]*recRow{} })
	limit := oai.RecordingAccountLimit(chatSettings())
	release := oai.FillRecordingAccount(limit - int64(oai.RecordCopyFactor*len(streamBody("chat"))) - int64(2*lines) - 64)
	on := postStream(context.Background(), t, srv, streamBody("chat"))
	release()
	checkRelay(t, on)
	if r := store.only(t); string(r.response)+"data: [DONE]\n\n" != on.raw || strings.Contains(on.raw, oai.CodeRecordingUnavailable) {
		t.Errorf("a stream of %d bytes with room for twice its lines: kept %d bytes, client %q", lines, len(r.response), on.raw)
	}
}

// TestRecordingRefusedRerankKeepsNoAnswer: a rerank answer whose commit
// fails is refused, and the row it ends with carries the refusal, not the
// answer the client never got.
func TestRecordingRefusedRerankKeepsNoAnswer(t *testing.T) {
	e := newRerankEngine(t)
	store := newRecStore(nil)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(store))
	rig.ca.Store(recordingConsumer(true))
	store.set(func(s *recStore) { s.finishFailures = 1 })
	a := rig.postRerank(t, "/v1/rerank", rerankBody("rrpub", "q", []string{"d1", "d2"}))
	checkRecordingRefusal(t, a.status, a.header, a.body)
	r := store.only(t)
	if r.state != llmlog.RecordingDone || r.out.Status != 503 || r.out.Code != oai.CodeRecordingUnavailable || len(r.response) != 0 {
		t.Errorf("row %q status %d code %q answer %d bytes — want done 503 recording_unavailable without an answer", r.state, r.out.Status, r.out.Code, len(r.response))
	}
}

// TestRecordingStatementSlots: however many recording consumers' requests
// arrive while the database holds their statements, at most four statements
// run at once — the rest wait for a slot (and the pool keeps its other
// connections).
func TestRecordingStatementSlots(t *testing.T) {
	u := newUpstream(t, recChatAnswer)
	store := newRecStore(nil)
	gate := make(chan struct{})
	store.set(func(s *recStore) { s.openGate = gate })
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 16, 1, withoutCensus, withRecordings(store))
	rig.ca.Store(recordingConsumer(true))
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { rig.post(t, chatBody("pub")) })
	}
	deadline := time.Now().Add(3 * time.Second)
	for store.inOpen.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // room for a fifth, should one get in
	running := store.inOpen.Load()
	close(gate)
	wg.Wait()
	if running != 4 || store.maxInOpen.Load() != 4 {
		t.Errorf("%d Opens ran at once while the database held them (at most %d over the run) — want exactly 4", running, store.maxInOpen.Load())
	}
}

// TestRecordedLLMLogRowCarriesNoBodies: under gateway.body_logging=full, the
// context_llm_log row of a recording consumer's request carries no bodies —
// its request and answer are in context_llm_recordings alone — while the row
// of the same request without the switch carries both; the recorded row is
// marked with its recording id. Chat, a stream, embeddings and rerank.
func TestRecordedLLMLogRowCarriesNoBodies(t *testing.T) {
	check := func(t *testing.T, rig *chatRig, store *recStore) {
		t.Helper()
		rows := rig.rows.all()
		if len(rows) != 2 {
			t.Fatalf("%d llmlog rows, want 2 (off, on)", len(rows))
		}
		off, on := llmlog.GatewayEntry(rows[0]), llmlog.GatewayEntry(rows[1])
		if off.RequestUser == "" {
			t.Fatalf("the unrecorded row carries no request under body_logging=full (handed %d bytes, sensitivity %q) — the test does not reach the body policy",
				len(rows[0].Request), off.RequiredSensitivity)
		}
		if on.RequestSystem != "" || on.RequestUser != "" || on.ResponseContent != "" {
			t.Errorf("the recorded llmlog row carries bodies (request %d, response %d bytes) — want none, the recording holds them",
				len(on.RequestUser), len(on.ResponseContent))
		}
		if r := store.only(t); !rows[1].Recorded || rows[1].RecordingID != r.req.ID {
			t.Errorf("recorded row: recorded %v recording_id %q, want true %q", rows[1].Recorded, rows[1].RecordingID, r.req.ID)
		}
	}
	full := func(s *oai.Settings) { s.BodyLogging = llmlog.BodyLoggingFull }
	t.Run("chat", func(t *testing.T) {
		u := newUpstream(t, recChatAnswer)
		store := newRecStore(nil)
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
		rig.setSettings(full)
		for _, on := range []bool{false, true} {
			rig.ca.Store(recordingConsumer(on))
			if a := rig.post(t, chatBody("pub")); a.status != 200 {
				t.Fatalf("switch %v: %d %s", on, a.status, a.body)
			}
		}
		check(t, rig, store)
	})
	t.Run("stream", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		store := newRecStore(nil)
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
		rig.setSettings(full)
		srv := serveHTTP(t, rig.router)
		for _, on := range []bool{false, true} {
			rig.ca.Store(recordingConsumer(on))
			if a := postStream(context.Background(), t, srv, streamBody("pub")); a.status != 200 || !strings.Contains(a.raw, "data: [DONE]") {
				t.Fatalf("switch %v: stream %d %q", on, a.status, a.raw)
			}
		}
		check(t, rig, store)
	})
	t.Run("embeddings", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		store := newRecStore(nil)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withRecordings(store))
		rig.setSettings(full)
		// A public embedding alias: the body policy stores bodies only below
		// credentials.
		rig.reg.reload(t, append(embedAliases(), chatAlias{"00000000-0000-4000-8000-0000000e0571", backends.GlobalScope, "embpub",
			"embedding", "embed", backends.SensPublic, `{"embedding.native_dims":8,"embedding.max_inputs":8}`, true}))
		for _, on := range []bool{false, true} {
			rig.ca.Store(recordingConsumer(on))
			if a := rig.postEmbed(t, embedBody("embpub", []string{"first", "second"})); a.status != 200 {
				t.Fatalf("switch %v: %d %s", on, a.status, a.body)
			}
		}
		check(t, rig, store)
	})
	t.Run("rerank", func(t *testing.T) {
		e := newRerankEngine(t)
		store := newRecStore(nil)
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(store))
		rig.setSettings(full)
		for _, on := range []bool{false, true} {
			rig.ca.Store(recordingConsumer(on))
			if a := rig.postRerank(t, "/v1/rerank", rerankBody("rrpub", "which?", []string{"d1", "d2"})); a.status != 200 {
				t.Fatalf("switch %v: %d %s", on, a.status, a.body)
			}
		}
		check(t, rig, store)
	})
}

// recRejection is a backend's own refusal text: with
// gateway.relay_upstream_4xx_messages on, the client's 400 carries it.
const recRejection = "the engine refuses this prompt: marker-571-rejected"

// TestRecordingErrorAnswerHeldBeforeItIsWritten: an error answer the client
// gets after the walk — here a backend's 400 whose text the gateway relays —
// is in the recording's row with its status and code, byte for byte the body
// the client got, and it is held before it is written: when its row cannot
// take it the client gets 503 recording_unavailable, never the backend's
// text. Chat (non-streamed), embeddings and rerank.
func TestRecordingErrorAnswerHeldBeforeItIsWritten(t *testing.T) {
	reject := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"` + recRejection + `","type":"invalid_request_error"}}`))
	}
	relay := func(s *oai.Settings) { s.RelayUpstream4xxMessages = true }
	routes := []struct {
		name string
		rig  func(t *testing.T, store *recStore) (post func() chatAnswer)
	}{
		{"chat", func(t *testing.T, store *recStore) func() chatAnswer {
			u := newUpstream(t, recChatAnswer)
			u.setRespond(func(w http.ResponseWriter, _ *http.Request) { reject(w) })
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
			rig.setSettings(relay)
			rig.ca.Store(recordingConsumer(true))
			return func() chatAnswer { return rig.post(t, chatBody("pub")) }
		}},
		{"embeddings", func(t *testing.T, store *recStore) func() chatAnswer {
			e := newEmbedEngine(t, 8)
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool { reject(w); return true })
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withRecordings(store))
			rig.setSettings(relay)
			rig.ca.Store(recordingConsumer(true))
			return func() chatAnswer { return rig.postEmbed(t, embedBody("emb1", []string{"a"})) }
		}},
		{"rerank", func(t *testing.T, store *recStore) func() chatAnswer {
			e := newRerankEngine(t)
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ rerankCall) bool { reject(w); return true })
			rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(store))
			rig.setSettings(relay)
			rig.ca.Store(recordingConsumer(true))
			return func() chatAnswer { return rig.postRerank(t, "/v1/rerank", rerankBody("rrpub", "q", []string{"d1", "d2"})) }
		}},
	}
	for _, rt := range routes {
		t.Run(rt.name+"/held", func(t *testing.T) {
			store := newRecStore(nil)
			a := rt.rig(t, store)()
			if a.status != http.StatusBadRequest || !strings.Contains(string(a.body), "marker-571-rejected") {
				t.Fatalf("client: %d %s — want the backend's 400 relayed (the test does not reach the relayed text)", a.status, a.body)
			}
			r := store.only(t)
			if r.state != llmlog.RecordingDone || r.out.Status != http.StatusBadRequest || r.out.Code != oai.CodeUpstreamRejected {
				t.Errorf("row: state %q status %d code %q — want done 400 upstream_rejected", r.state, r.out.Status, r.out.Code)
			}
			if string(r.response) != string(a.body) {
				t.Errorf("row answer %q — want the body the client got %q", r.response, a.body)
			}
		})
		t.Run(rt.name+"/not_held", func(t *testing.T) {
			store := newRecStore(nil)
			post := rt.rig(t, store)
			store.set(func(s *recStore) { s.finishErr = errors.New("database down") })
			a := post()
			checkRecordingRefusal(t, a.status, a.header, a.body)
			if strings.Contains(string(a.body), "marker-571-rejected") {
				t.Errorf("the client got the backend's text that was not held: %s", a.body)
			}
			if r := store.only(t); r.state != llmlog.RecordingOpen {
				t.Errorf("row state %q, want open (its outcome could not be held)", r.state)
			}
		})
	}
}

// postVia posts body to path over srv (a real connection: the revoker's
// request contexts) and reads the whole answer.
func postVia(t *testing.T, srv *httptest.Server, path, body string) chatAnswer {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		return chatAnswer{status: -1, body: []byte(err.Error())}
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return chatAnswer{status: resp.StatusCode, header: resp.Header, body: b}
}

// recRoutes are the three non-streamed routes of a recording consumer, each
// over its own engine and rig: rig builds them over store, path and body
// are the request; answered tells the route's answer from a refusal.
var recRoutes = []struct {
	name, path string
	rig        func(t *testing.T, store *recStore, opts ...rigOption) *chatRig
	body       string
	answered   func(b []byte) bool
}{
	{"chat", "/v1/chat/completions", func(t *testing.T, store *recStore, opts ...rigOption) *chatRig {
		u := newUpstream(t, recChatAnswer)
		return newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, append(opts, withRecordings(store))...)
	}, chatBody("pub"), func(b []byte) bool { return strings.Contains(string(b), "the answer") }},
	{"embeddings", "/v1/embeddings", func(t *testing.T, store *recStore, opts ...rigOption) *chatRig {
		e := newEmbedEngine(t, 8)
		return newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, append(opts, withRecordings(store))...)
	}, embedBody("emb1", []string{"a"}), func(b []byte) bool { return strings.Contains(string(b), `"embedding"`) }},
	{"rerank", "/v1/rerank", func(t *testing.T, store *recStore, opts ...rigOption) *chatRig {
		e := newRerankEngine(t)
		return newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, append(opts, withRecordings(store))...)
	}, rerankBody("rrpub", "q", []string{"d1", "d2"}), func(b []byte) bool { return strings.Contains(string(b), "relevance_score") }},
}

// TestRecordingAnswerWithheldWhenRevokedAfterItsHold: a request whose key
// stops authenticating while its answer is being held — between the commit
// of the answer and its write — gets the 401 the revocation answers, never
// the answer; its row ends with that 401 and its body, not with the answer
// it held. Chat, embeddings and rerank.
func TestRecordingAnswerWithheldWhenRevokedAfterItsHold(t *testing.T) {
	for _, rt := range recRoutes {
		t.Run(rt.name, func(t *testing.T) {
			store := newRecStore(nil)
			gate := make(chan struct{})
			var gateOnce sync.Once
			defer gateOnce.Do(func() { close(gate) })
			store.set(func(s *recStore) { s.finishGate = gate })
			rig := rt.rig(t, store)
			rig.ca.Store(recordingConsumer(true))
			rv := newRevoker()
			srv := serveHTTP(t, rv.wrap(rig.router))
			done := make(chan chatAnswer, 1)
			go func() { done <- postVia(t, srv, rt.path, rt.body) }()
			if !waitFor(5*time.Second, func() bool { return store.inFinish.Load() == 1 }) {
				t.Fatal("the answer's commit did not start")
			}
			if n := rv.revoke(); n != 1 {
				t.Fatalf("revoked %d running requests, want 1", n)
			}
			gateOnce.Do(func() { close(gate) })
			var a chatAnswer
			select {
			case a = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("no answer after the revocation")
			}
			if a.status != http.StatusUnauthorized || !strings.Contains(string(a.body), `"code":"invalid_api_key"`) || rt.answered(a.body) {
				t.Fatalf("answer %d %s — want 401 invalid_api_key, without the answer", a.status, a.body)
			}
			r := store.only(t)
			if r.state != llmlog.RecordingDone || r.out.Status != http.StatusUnauthorized || r.out.Code != oai.CodeInvalidAPIKey ||
				string(r.response) != string(a.body) {
				t.Errorf("row: state %q status %d code %q answer %q — want done 401 invalid_api_key with the client's %q",
					r.state, r.out.Status, r.out.Code, r.response, a.body)
			}
		})
	}
}

// TestRecordingLostAcknowledgementLeavesNoFalseAnswer: an answer whose
// commit went through but whose acknowledgement was lost is refused (the
// client gets 503 recording_unavailable and retries), and the row then says
// so — status 503, the code, no answer — instead of the 200 and the answer
// the client never got. Chat, embeddings and rerank.
func TestRecordingLostAcknowledgementLeavesNoFalseAnswer(t *testing.T) {
	for _, rt := range recRoutes {
		t.Run(rt.name, func(t *testing.T) {
			store := newRecStore(nil)
			store.set(func(s *recStore) { s.finishLost = 1 })
			rig := rt.rig(t, store)
			ca := recordingConsumer(true)
			ca.ConsumerID = uuid.NewString()
			rig.ca.Store(ca)
			srv := serveHTTP(t, rig.router)
			a := postVia(t, srv, rt.path, rt.body)
			checkRecordingRefusal(t, a.status, a.header, a.body)
			if rt.answered(a.body) {
				t.Errorf("the client got the answer: %s", a.body)
			}
			r := store.only(t)
			if r.state != llmlog.RecordingDone || r.out.Status != http.StatusServiceUnavailable ||
				r.out.Code != oai.CodeRecordingUnavailable || len(r.response) != 0 {
				t.Errorf("row: state %q status %d code %q answer %d bytes — want done 503 recording_unavailable without an answer",
					r.state, r.out.Status, r.out.Code, len(r.response))
			}
			if got := statsOf(ca.ConsumerID); got.Unfinished != 0 || got.Recorded != 1 {
				t.Errorf("counts %+v — want recorded 1, unfinished 0 (the row holds the outcome)", got)
			}
		})
	}
}

// TestRecordingRefusalGoesOutWhenItsRowFails: a refusal before the walk —
// nothing reached a backend — is held only until its row is attempted; when
// the row cannot be written the client still gets that refusal as it is
// (not 503 recording_unavailable), and the request counts as unrecordable.
func TestRecordingRefusalGoesOutWhenItsRowFails(t *testing.T) {
	u := newUpstream(t, recChatAnswer)
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
	ca := recordingConsumer(true)
	ca.ConsumerID = uuid.NewString()
	rig.ca.Store(ca)
	store.set(func(s *recStore) { s.insertErr = errors.New("database down") })
	a := rig.post(t, `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`)
	if code, _, _ := a.envelope(t); a.status != http.StatusNotFound || code != oai.CodeModelNotFound {
		t.Errorf("client %d %q — want the refusal as it is, 404 model_not_found: %s", a.status, code, a.body)
	}
	if got := statsOf(ca.ConsumerID); got.Unrecordable != 1 || got.Recorded != 0 {
		t.Errorf("counts %+v — want unrecordable 1, recorded 0", got)
	}
}

// TestRecordingHeldAnswerChecksPastTheDeadline: the request deadline may
// pass while an answer's commit waits (the commit is not cut by it); a
// request that stopped being served during that time — the model API
// switched off, the key revoked after the deadline or before it, the
// shutdown — still gets its refusal, never the answer and never an empty
// 200, and its row says what the client got. Chat, embeddings and rerank.
func TestRecordingHeldAnswerChecksPastTheDeadline(t *testing.T) {
	want := map[string]struct {
		status int
		code   string
	}{
		"gateway_off":             {http.StatusNotFound, oai.CodeNotFound},
		"revoked":                 {http.StatusUnauthorized, oai.CodeInvalidAPIKey},
		"revoked_before_deadline": {http.StatusUnauthorized, oai.CodeInvalidAPIKey},
		"shutdown":                {http.StatusServiceUnavailable, oai.CodeInternalError},
	}
	for _, rt := range recRoutes {
		for _, mode := range []string{"gateway_off", "revoked", "revoked_before_deadline", "shutdown"} {
			t.Run(rt.name+"/"+mode, func(t *testing.T) {
				store := newRecStore(nil)
				gate := make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(gate) })
				store.set(func(s *recStore) { s.finishGate = gate })
				life, shutdown := context.WithCancel(context.Background())
				defer shutdown()
				rig := rt.rig(t, store, func(r *chatRig) { r.deps.Lifecycle = life })
				rig.ca.Store(recordingConsumer(true))
				const deadline = 800 * time.Millisecond
				rig.setSettings(func(s *oai.Settings) { s.RequestDeadline = deadline })
				rv := newRevoker()
				srv := serveHTTP(t, rv.wrap(rig.router))
				done := make(chan chatAnswer, 1)
				start := time.Now()
				go func() { done <- postVia(t, srv, rt.path, rt.body) }()
				if !waitFor(5*time.Second, func() bool { return store.inFinish.Load() == 1 }) {
					t.Fatal("the answer's commit did not start")
				}
				if mode == "revoked_before_deadline" {
					rv.revoke()
				}
				time.Sleep(time.Until(start.Add(deadline + 400*time.Millisecond)))
				switch mode {
				case "gateway_off":
					rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
				case "revoked":
					if n := rv.revoke(); n != 1 {
						t.Fatalf("revoked %d running requests, want 1", n)
					}
				case "shutdown":
					shutdown()
				}
				once.Do(func() { close(gate) })
				var a chatAnswer
				select {
				case a = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("no answer")
				}
				w := want[mode]
				if code, _, _ := a.envelope(t); a.status != w.status || code != w.code || rt.answered(a.body) {
					t.Errorf("client %d %q (%d bytes) — want %d %q without the answer", a.status, code, len(a.body), w.status, w.code)
				}
				r := store.only(t)
				if r.state != llmlog.RecordingDone || r.out.Status != a.status || r.out.Code != w.code || string(r.response) != string(a.body) {
					t.Errorf("row: state %q status %d code %q answer %d bytes — want done %d %q with the client's %d bytes",
						r.state, r.out.Status, r.out.Code, len(r.response), a.status, w.code, len(a.body))
				}
			})
		}
	}
}

// TestRecordingHeldErrorLostAcknowledgement: an error answer held for its
// row whose commit went through but reported an error goes out as 503
// recording_unavailable — and the row then says that, not the error answer
// the client never got.
func TestRecordingHeldErrorLostAcknowledgement(t *testing.T) {
	u := newUpstream(t, recChatAnswer)
	u.setRespond(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"` + recRejection + `","type":"invalid_request_error"}}`))
	})
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
	rig.setSettings(func(s *oai.Settings) { s.RelayUpstream4xxMessages = true })
	ca := recordingConsumer(true)
	ca.ConsumerID = uuid.NewString()
	rig.ca.Store(ca)
	store.set(func(s *recStore) { s.finishLost = 1 })
	a := rig.post(t, chatBody("pub"))
	checkRecordingRefusal(t, a.status, a.header, a.body)
	r := store.only(t)
	if r.state != llmlog.RecordingDone || r.out.Status != http.StatusServiceUnavailable || r.out.Code != oai.CodeRecordingUnavailable ||
		strings.Contains(string(r.response), "marker-571-rejected") {
		t.Errorf("row: state %q status %d code %q answer %q — want done 503 recording_unavailable, not the error the client did not get",
			r.state, r.out.Status, r.out.Code, r.response)
	}
	if got := statsOf(ca.ConsumerID); got.Unfinished != 0 || got.Recorded != 1 {
		t.Errorf("counts %+v — want recorded 1, unfinished 0", got)
	}
}

// TestRecordingPanicAfterAHeldAnswer: a route that wrote an error answer —
// held for its row — and then panics: the client gets oai.Recover's 503,
// and the row says 503 internal_error, not the held answer.
func TestRecordingPanicAfterAHeldAnswer(t *testing.T) {
	store := newRecStore(nil)
	stage := oai.NewRecordingStage(store, func(context.Context) *auth.ConsumerAuth { return recordingConsumer(true) },
		func() oai.Settings { return chatSettings() }, func() time.Duration { return time.Second })
	rt := chi.NewRouter()
	rt.Use(oai.Recover, stage.Handler)
	rt.Post("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		oai.WriteCode(w, oai.CodeUpstreamRejected, "a held answer", false)
		panic("boom")
	})
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	r := store.only(t)
	if w.Code != 503 || strings.Contains(w.Body.String(), "a held answer") || r.out.Status != 503 || r.out.Code != oai.CodeInternalError ||
		strings.Contains(string(r.response), "a held answer") {
		t.Errorf("client %d %q, row status %d code %q answer %q — want 503 internal_error both, without the held answer",
			w.Code, w.Body.String(), r.out.Status, r.out.Code, r.response)
	}
}

// refusedBody is an unparsable chat request of n bytes: the route reads it
// whole and refuses it (invalid_json) before its walk.
func refusedBody(n int) string {
	head := `{"model":"pub","messages":"`
	return head + strings.Repeat("x", n-len(head))
}

// TestRecordingRefusedBodyStaysBooked: the body of a request refused before
// its walk stays booked while its row waits for room in the recording
// account — the request's in-flight reservation is held until the row is
// written, so the wait is backpressure on the in-flight budget, never a body
// booked nowhere — and the row gets the body once there is room.
func TestRecordingRefusedBodyStaysBooked(t *testing.T) {
	u := newUpstream(t, recChatAnswer)
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
	rig.ca.Store(recordingConsumer(true))
	body := refusedBody(1 << 20)
	release := oai.FillRecordingAccount(oai.RecordingAccountLimit(chatSettings()))
	var once sync.Once
	defer once.Do(release)
	done := make(chan chatAnswer, 1)
	go func() { done <- rig.post(t, body) }()
	// The route has read and refused the body; its row waits for room.
	time.Sleep(500 * time.Millisecond)
	booked := rig.budget.InUse()
	once.Do(release)
	a := <-done
	if booked < int64(len(body)) {
		t.Errorf("while the refused request's row waited for room, the in-flight budget booked %d bytes — want its reservation (≥ %d, the body)", booked, len(body))
	}
	if code, _, _ := a.envelope(t); a.status != http.StatusBadRequest || code != "invalid_json" {
		t.Errorf("client %d %q, want 400 invalid_json", a.status, code)
	}
	if r := store.only(t); string(r.body) != body || r.opened {
		t.Errorf("row: %d bytes opened %v — want the whole refused body, never opened", len(r.body), r.opened)
	}
	if n := rig.budget.InUse(); n != 0 {
		t.Errorf("the in-flight budget holds %d bytes after the request ended", n)
	}
}

// TestRecordingRefusedFloodProbe (opt-in, CTX_RECORD_PROBE=1) measures what a
// flood of refused 1.5 MiB requests of a recording consumer holds while their
// rows wait for room in the recording account: the live heap at the wait
// (after a collection), the in-flight budget's booking then, and how the
// requests were answered.
func TestRecordingRefusedFloodProbe(t *testing.T) {
	if os.Getenv("CTX_RECORD_PROBE") != "1" {
		t.Skip("opt-in probe (CTX_RECORD_PROBE=1)")
	}
	const n, size, budget = 48, 3 << 19, 64 << 20 // 48 requests of 1.5 MiB, a 64 MiB in-flight budget
	u := newUpstream(t, recChatAnswer)
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = budget })
	rig.ca.Store(recordingConsumer(true))
	body := refusedBody(size)
	var base runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)
	release := oai.FillRecordingAccount(oai.RecordingAccountLimit(chatSettings()))
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for range n {
		// One request after the other, as a client's steady stream: each
		// finds the in-flight budget as the ones before it left it.
		wg.Go(func() {
			a := rig.post(t, body)
			mu.Lock()
			codes[a.status]++
			mu.Unlock()
		})
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // the last one has been read and refused or turned away; the rows wait
	var at runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&at)
	booked := rig.budget.InUse()
	release()
	wg.Wait()
	live := int64(at.HeapAlloc) - int64(base.HeapAlloc)
	t.Logf("PROBE flood: %d requests of %d B, in-flight budget %d MiB: live heap at the wait %.1f MiB over the baseline, in-flight booked %.1f MiB, answers %v, rows %d",
		n, size, budget>>20, float64(live)/(1<<20), float64(booked)/(1<<20), codes, len(store.rows))
}
